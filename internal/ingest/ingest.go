// Package ingest turns an S3 event into a package record.
//
// This is the only place a package file is ever read. Everything downstream
// works from the stored record, which is what removes the repeated full-repo
// rescan that conventional tooling performs on every update.
package ingest

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"time"

	"github.com/ashleyconnor/linux-repo-indexer/internal/pkgmeta"
	"github.com/ashleyconnor/linux-repo-indexer/internal/pkgmeta/deb"
	rpmparse "github.com/ashleyconnor/linux-repo-indexer/internal/pkgmeta/rpm"
	"github.com/ashleyconnor/linux-repo-indexer/internal/repoconfig"
)

// ObjectInfo describes an object in the bucket.
type ObjectInfo struct {
	Size         int64
	LastModified time.Time
}

// An ObjectReader fetches objects from the bucket.
type ObjectReader interface {
	Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error)
}

// A PackageWriter stores and removes package records.
type PackageWriter interface {
	Put(ctx context.Context, scope repoconfig.Scope, pkg *pkgmeta.Package) error
	Delete(ctx context.Context, scope repoconfig.Scope, filename string) error
}

// A StateMarker records that a scope has unpublished changes.
type StateMarker interface {
	MarkDirty(ctx context.Context, scope repoconfig.Scope) (int64, error)
}

// A Queue schedules the publish that will pick the change up.
type Queue interface {
	EnqueuePublish(ctx context.Context, scope repoconfig.Scope, delay time.Duration) error
}

// Ingester handles one S3 object event at a time.
type Ingester struct {
	Config *repoconfig.Config

	// ConfigKey is the object holding repos.yaml. A write to it is the one
	// event that changes what every scope should contain, so it is handled
	// here rather than being classified and discarded.
	ConfigKey string

	Objects  ObjectReader
	Packages PackageWriter
	State    StateMarker
	Queue    Queue

	// PublishDelay is the coalescing window. Uploading a release's worth of
	// packages should cause one index build, not one per package, and a short
	// delay on the publish message is what collapses them: by the time the
	// first message is delivered, the rest have already been stored.
	PublishDelay time.Duration

	Now func() time.Time
	Log *slog.Logger
}

func (i *Ingester) now() time.Time {
	if i.Now != nil {
		return i.Now()
	}
	return time.Now().UTC()
}

func (i *Ingester) log() *slog.Logger {
	if i.Log != nil {
		return i.Log
	}
	return slog.Default()
}

// Created handles an object being added or overwritten.
//
// A key that is not an indexed package — an index file we wrote ourselves, an
// unconfigured architecture, a stray upload — is logged and skipped. Failing
// instead would let one unrelated object block the queue behind it.
func (i *Ingester) Created(ctx context.Context, key string) error {
	if i.ConfigKey != "" && key == i.ConfigKey {
		return i.configChanged(ctx)
	}

	scope, filename, err := i.Config.Classify(key)
	if err != nil {
		if errors.Is(err, repoconfig.ErrNotIndexed) {
			i.log().Debug("skipping key", "key", key, "reason", err)
			return nil
		}
		return err
	}

	pkg, err := i.parse(ctx, key, filename, scope)
	if err != nil {
		return err
	}

	if err := i.Packages.Put(ctx, scope, pkg); err != nil {
		return err
	}

	i.log().Info("ingested",
		"key", key,
		"scope", scope.String(),
		"package", pkg.NEVRA(),
		"size", pkg.Size)

	return i.schedulePublish(ctx, scope)
}

// Removed handles an object being deleted.
//
// Records are keyed by object, so the S3 key alone identifies the row — which
// matters because the object is gone and cannot be re-parsed to work out which
// package it held.
func (i *Ingester) Removed(ctx context.Context, key string) error {
	scope, filename, err := i.Config.Classify(key)
	if err != nil {
		if errors.Is(err, repoconfig.ErrNotIndexed) {
			i.log().Debug("skipping removal", "key", key, "reason", err)
			return nil
		}
		return err
	}

	if err := i.Packages.Delete(ctx, scope, filename); err != nil {
		return err
	}

	i.log().Info("removed", "key", key, "scope", scope.String())

	return i.schedulePublish(ctx, scope)
}

// configChanged republishes everything after repos.yaml is written.
//
// A config change can alter what any scope should contain — a new codename
// needs the pool indexes copied into it, and a changed origin or compression
// setting rewrites output that no upload would otherwise touch. None of that
// bumps a generation, so without marking the scopes dirty here they would read
// back clean and the publisher would skip them.
//
// Republishing everything is affordable because an unchanged index is a no-op:
// the sync skips content that is already present, so the cost is the rebuild,
// not the upload.
func (i *Ingester) configChanged(ctx context.Context) error {
	scopes := i.Config.PublishScopes()

	i.log().Info("repository config changed, republishing every scope",
		"scopes", len(scopes))

	for _, scope := range scopes {
		if err := i.schedulePublish(ctx, scope); err != nil {
			return err
		}
	}
	return nil
}

func (i *Ingester) schedulePublish(ctx context.Context, scope repoconfig.Scope) error {
	if _, err := i.State.MarkDirty(ctx, scope); err != nil {
		return err
	}
	if i.Queue == nil {
		return nil
	}
	return i.Queue.EnqueuePublish(ctx, scope, i.PublishDelay)
}

// parse reads the object once, hashing it as it goes.
//
// The parsers stop at the end of the metadata, so the remainder is drained
// afterwards to complete the checksums. That is the whole reason a package
// never needs to be downloaded twice.
func (i *Ingester) parse(ctx context.Context, key, filename string, scope repoconfig.Scope) (*pkgmeta.Package, error) {
	body, info, err := i.Objects.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("ingest: fetching %s: %w", key, err)
	}
	defer body.Close()

	hashes := newHashes()
	tee := io.TeeReader(body, hashes.writer)

	var pkg *pkgmeta.Package
	switch scope.Kind {
	case repoconfig.KindDebPool:
		pkg, err = deb.Parse(tee)
	case repoconfig.KindRPMTree:
		pkg, err = rpmparse.Parse(tee)
	default:
		return nil, fmt.Errorf("ingest: %s is not a package scope", scope)
	}
	if err != nil {
		return nil, fmt.Errorf("ingest: parsing %s: %w", key, err)
	}

	if _, err := io.Copy(io.Discard, tee); err != nil {
		return nil, fmt.Errorf("ingest: reading the rest of %s: %w", key, err)
	}

	pkg.S3Key = key
	pkg.Filename = filename
	pkg.Size = info.Size
	pkg.MD5, pkg.SHA1, pkg.SHA256 = hashes.sums()
	pkg.UpdatedAt = i.now()

	if pkg.RPM != nil {
		// createrepo records the package file's mtime; in S3 that is the
		// object's last-modified time.
		pkg.RPM.FileTime = info.LastModified.Unix()
	}

	return pkg, nil
}

type hashes struct {
	md5    hash.Hash
	sha1   hash.Hash
	sha256 hash.Hash
	writer io.Writer
}

func newHashes() *hashes {
	h := &hashes{md5: md5.New(), sha1: sha1.New(), sha256: sha256.New()}
	h.writer = io.MultiWriter(h.md5, h.sha1, h.sha256)
	return h
}

func (h *hashes) sums() (md5sum, sha1sum, sha256sum string) {
	return hex.EncodeToString(h.md5.Sum(nil)),
		hex.EncodeToString(h.sha1.Sum(nil)),
		hex.EncodeToString(h.sha256.Sum(nil))
}
