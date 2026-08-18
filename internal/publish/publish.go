// Package publish turns stored metadata into published repository indexes.
//
// It is the half of the system that never touches a package file: everything
// it writes is derived from DynamoDB records, which is what removes the
// "rescan every package" step from a repository update.
package publish

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"time"

	"github.com/ashleyconnor/linux-repo-indexer/internal/index"
	"github.com/ashleyconnor/linux-repo-indexer/internal/index/debian"
	"github.com/ashleyconnor/linux-repo-indexer/internal/index/rpmmd"
	"github.com/ashleyconnor/linux-repo-indexer/internal/pkgmeta"
	"github.com/ashleyconnor/linux-repo-indexer/internal/repoconfig"
	"github.com/ashleyconnor/linux-repo-indexer/internal/sign"
	"github.com/ashleyconnor/linux-repo-indexer/internal/store"
)

// A Syncer writes artifacts to object storage.
type Syncer interface {
	// Put writes artifacts, skipping any whose content is already present so
	// a no-op publish does not rewrite the repository.
	Put(ctx context.Context, artifacts []index.Artifact) error

	// Prune deletes objects under dir that are neither in keep nor newer than
	// cutoff. The age check is what stops a client mid-refresh from getting a
	// 404 for metadata it was told about moments earlier.
	Prune(ctx context.Context, dir string, keep map[string]bool, cutoff time.Time) error
}

// A Queue enqueues follow-up publish work.
type Queue interface {
	EnqueuePublish(ctx context.Context, scope repoconfig.Scope, delay time.Duration) error
}

// PackageLister reads a scope's package records.
type PackageLister interface {
	List(ctx context.Context, scope repoconfig.Scope) ([]pkgmeta.Package, error)
}

// StateAPI is the publish-state surface the publisher needs.
type StateAPI interface {
	Get(ctx context.Context, scope repoconfig.Scope) (store.State, error)
	MarkDirty(ctx context.Context, scope repoconfig.Scope) (int64, error)
	AcquireLease(ctx context.Context, scope repoconfig.Scope, owner string, ttl time.Duration, now time.Time) (store.State, error)
	ReleaseLease(ctx context.Context, scope repoconfig.Scope, owner string) error
	RecordPublished(ctx context.Context, scope repoconfig.Scope, owner string, generation int64, artifacts []store.PublishedArtifact, now time.Time) error
}

// staleGrace is how long superseded index files are left in place before
// pruning. apt and dnf read repomd.xml or Release first and then fetch the
// files it names; deleting those immediately would break a client that is
// partway through.
const staleGrace = 7 * 24 * time.Hour

// contentionDelay is how long a publisher that loses the lease waits before
// its message is delivered again.
//
// The lease can be held for as long as the publish timeout, so a contended
// message may be re-delivered many times before it wins. That is cheap: each
// attempt is one no-op invocation, and the requeue is one-for-one with the
// message it replaces, so a burst cannot amplify itself.
const contentionDelay = 30 * time.Second

// Publisher rebuilds and uploads the index for one scope at a time.
type Publisher struct {
	Config   *repoconfig.Config
	Packages PackageLister
	State    StateAPI
	Sync     Syncer
	Signer   sign.Signer
	Queue    Queue

	// Owner identifies this publisher in the lease. On Lambda it is the
	// request ID, so a log line names whoever holds a contested scope.
	Owner    string
	LeaseTTL time.Duration

	// Now is injectable so tests control timestamps and the generated indexes
	// stay reproducible.
	Now func() time.Time

	Log *slog.Logger
}

func (p *Publisher) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now().UTC()
}

func (p *Publisher) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

// Publish rebuilds one scope's index if it has unpublished changes.
//
// It is safe to call for a clean scope, for a scope another publisher is
// working on, and for a scope that no longer exists in the config — all three
// return nil without writing anything, because publish messages are coalesced
// and replayed and must be harmless when they arrive late.
func (p *Publisher) Publish(ctx context.Context, scope repoconfig.Scope) error {
	if !p.Config.Knows(scope) {
		// The scope was removed from repos.yaml. Publishing stops here; the
		// existing tree is left alone until someone retires it explicitly.
		p.log().Info("skipping publish for a scope that is no longer configured", "scope", scope.String())
		return nil
	}

	state, err := p.State.AcquireLease(ctx, scope, p.Owner, p.LeaseTTL, p.now())
	if err != nil {
		if errors.Is(err, store.ErrLeaseHeld) {
			// The holder captured its generation before this message was sent,
			// so it may not cover the change that prompted us. Hand the
			// message back to the queue rather than letting it be deleted:
			// dropping it here is what used to leave a scope dirty with no
			// pending publish, recoverable only by the sweep.
			p.log().Info("scope is being published by another worker, requeuing",
				"scope", scope.String())
			if p.Queue == nil {
				return nil
			}
			return p.Queue.EnqueuePublish(ctx, scope, contentionDelay)
		}
		return err
	}
	defer func() {
		if err := p.State.ReleaseLease(ctx, scope, p.Owner); err != nil {
			p.log().Error("releasing lease", "scope", scope.String(), "error", err)
		}
	}()

	if !state.Dirty() {
		p.log().Info("scope is already published", "scope", scope.String(), "generation", state.Generation)
		return nil
	}

	// Captured under the lease, so nothing can be published between reading
	// the packages and recording the generation they came from.
	generation := state.Generation

	var artifacts []store.PublishedArtifact
	switch scope.Kind {
	case repoconfig.KindDebPool:
		artifacts, err = p.publishDebPool(ctx, scope)
	case repoconfig.KindDebRelease:
		artifacts, err = p.publishDebRelease(ctx, scope)
	case repoconfig.KindRPMTree:
		artifacts, err = p.publishRPMTree(ctx, scope)
	default:
		err = fmt.Errorf("publish: unknown scope kind %q", scope.Kind)
	}
	if err != nil {
		return err
	}

	if err := p.State.RecordPublished(ctx, scope, p.Owner, generation, artifacts, p.now()); err != nil {
		// Leaving the scope dirty is the safe failure: it is republished
		// rather than being recorded as current while stale.
		return err
	}

	p.log().Info("published", "scope", scope.String(), "generation", generation, "artifacts", len(artifacts))
	return nil
}

// publishDebPool writes the Packages files for one component and architecture
// into every codename that claims them, then marks each codename's Release
// dirty so it is rebuilt with the new digests.
func (p *Publisher) publishDebPool(ctx context.Context, scope repoconfig.Scope) ([]store.PublishedArtifact, error) {
	pkgs, err := p.Packages.List(ctx, scope)
	if err != nil {
		return nil, err
	}

	idx, err := debian.BuildPackagesIndex(pkgs, p.Config.APT.Compressions)
	if err != nil {
		return nil, err
	}

	var (
		all      []index.Artifact
		recorded []store.PublishedArtifact
	)
	for _, codename := range p.Config.APT.Codenames {
		dir := p.Config.IndexDir(codename, scope)
		all = append(all, idx.Artifacts(dir, p.Config.APT.AcquireByHash)...)
	}

	// Recorded once, relative to the codename directory, because every
	// codename publishes byte-identical index files and the Release builder
	// only needs the digests.
	for _, f := range idx.Files(p.Config.IndexDirRelativeToCodename(scope)) {
		recorded = append(recorded, store.PublishedArtifact{Path: f.Path, Digests: f.Digests})
	}

	if err := p.Sync.Put(ctx, all); err != nil {
		return nil, err
	}

	if p.Config.APT.AcquireByHash {
		if err := p.pruneByHash(ctx, scope, idx); err != nil {
			return nil, err
		}
	}

	// The Release files cannot be rebuilt here: one spans every component and
	// architecture, so it is a separate scope with its own lease.
	for _, release := range p.Config.ReleaseScopesFor(scope) {
		if _, err := p.State.MarkDirty(ctx, release); err != nil {
			return nil, err
		}
		if p.Queue != nil {
			if err := p.Queue.EnqueuePublish(ctx, release, 0); err != nil {
				return nil, err
			}
		}
	}

	p.log().Info("published packages index",
		"scope", scope.String(),
		"packages", len(pkgs),
		"codenames", len(p.Config.APT.Codenames))

	return recorded, nil
}

// pruneByHash removes superseded by-hash entries once they are past the grace
// period.
func (p *Publisher) pruneByHash(ctx context.Context, scope repoconfig.Scope, idx *debian.PackagesIndex) error {
	keep := make(map[string]bool)
	cutoff := p.now().Add(-staleGrace)

	for _, codename := range p.Config.APT.Codenames {
		dir := path.Join(p.Config.IndexDir(codename, scope), "by-hash")
		for _, a := range idx.Artifacts(p.Config.IndexDir(codename, scope), true) {
			keep[a.Path] = true
		}
		if err := p.Sync.Prune(ctx, dir, keep, cutoff); err != nil {
			return err
		}
	}
	return nil
}

// publishDebRelease assembles one codename's Release from the digests recorded
// when its component and architecture indexes were published, signs it, and
// writes InRelease and Release.gpg alongside.
func (p *Publisher) publishDebRelease(ctx context.Context, scope repoconfig.Scope) ([]store.PublishedArtifact, error) {
	var files []debian.IndexFile

	for _, pool := range p.Config.PoolScopesFor(scope) {
		state, err := p.State.Get(ctx, pool)
		if err != nil {
			return nil, err
		}
		for _, a := range state.Artifacts {
			files = append(files, debian.IndexFile{Path: a.Path, Digests: a.Digests})
		}
	}

	if len(files) == 0 {
		// Nothing has been published for any component or architecture yet.
		// A Release listing no files would tell apt the repository is empty,
		// so it is better to write nothing and be rebuilt once a pool
		// publishes.
		p.log().Info("no index files recorded yet, skipping Release", "scope", scope.String())
		return nil, nil
	}

	cfg := debian.ReleaseConfig{
		Origin:        p.Config.APT.Origin,
		Label:         p.Config.APT.Label,
		Suite:         scope.Codename,
		Codename:      scope.Codename,
		Description:   p.Config.APT.Description,
		Components:    p.Config.APT.Components,
		Architectures: p.Config.APT.Architectures,
		AcquireByHash: p.Config.APT.AcquireByHash,
		Date:          p.now(),
	}

	release, err := debian.BuildRelease(cfg, files)
	if err != nil {
		return nil, err
	}

	dir := p.Config.CodenameDir(scope.Codename)
	artifacts := []index.Artifact{{
		Path:        path.Join(dir, "Release"),
		ContentType: "text/plain; charset=utf-8",
		Body:        release,
	}}

	if p.Signer != nil {
		inRelease, err := p.Signer.ClearSign(ctx, release)
		if err != nil {
			return nil, err
		}
		detached, err := p.Signer.DetachSign(ctx, release)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts,
			index.Artifact{
				Path:        path.Join(dir, "InRelease"),
				ContentType: "text/plain; charset=utf-8",
				Body:        inRelease,
			},
			index.Artifact{
				Path:        path.Join(dir, "Release.gpg"),
				ContentType: "application/pgp-signature",
				Body:        detached,
			},
		)
	}

	if err := p.Sync.Put(ctx, artifacts); err != nil {
		return nil, err
	}

	recorded := make([]store.PublishedArtifact, 0, len(artifacts))
	for _, a := range artifacts {
		recorded = append(recorded, store.PublishedArtifact{Path: a.Path, Digests: a.Digests()})
	}
	return recorded, nil
}

// publishRPMTree writes a complete repodata directory. A yum tree is
// self-contained, so unlike the Debian side there is no second phase.
func (p *Publisher) publishRPMTree(ctx context.Context, scope repoconfig.Scope) ([]store.PublishedArtifact, error) {
	pkgs, err := p.Packages.List(ctx, scope)
	if err != nil {
		return nil, err
	}

	repodata, err := rpmmd.BuildRepodata(pkgs, repodataRevision(pkgs))
	if err != nil {
		return nil, err
	}

	treeDir := p.Config.TreeDir(scope)
	artifacts := make([]index.Artifact, 0, len(repodata.Artifacts)+1)
	for _, a := range repodata.Artifacts {
		a.Path = path.Join(treeDir, a.Path)
		artifacts = append(artifacts, a)
	}

	if p.Signer != nil {
		signature, err := p.Signer.DetachSign(ctx, repodata.RepomdXML)
		if err != nil {
			return nil, err
		}
		sigArtifact := rpmmd.SignatureArtifact(signature)
		sigArtifact.Path = path.Join(treeDir, sigArtifact.Path)
		artifacts = append(artifacts, sigArtifact)
	}

	if err := p.Sync.Put(ctx, artifacts); err != nil {
		return nil, err
	}

	// Old metadata files are named after their own checksum, so a new publish
	// lands beside the old rather than replacing it. Pruning past the grace
	// period is what stops the directory growing without bound.
	keep := make(map[string]bool, len(artifacts))
	for _, a := range artifacts {
		keep[a.Path] = true
	}
	if err := p.Sync.Prune(ctx, path.Join(treeDir, "repodata"), keep, p.now().Add(-staleGrace)); err != nil {
		return nil, err
	}

	p.log().Info("published repodata", "scope", scope.String(), "packages", len(pkgs))

	recorded := make([]store.PublishedArtifact, 0, len(artifacts))
	for _, a := range artifacts {
		recorded = append(recorded, store.PublishedArtifact{Path: a.Path, Digests: a.Digests()})
	}
	return recorded, nil
}

// repodataRevision derives repomd.xml's revision from the packages themselves
// rather than from the clock.
//
// This is what makes a republish of unchanged content a genuine no-op. With a
// wall-clock revision every publish produced a new repomd.xml, and since
// repomd.xml and its detached signature are two separate objects, each publish
// opened a window in which a client could fetch the new repomd.xml against the
// old repomd.xml.asc and reject the repository as corrupt. dnf hits that window
// readily, because it fetches the pair back to back.
//
// Deriving it from the newest package means identical input yields byte-
// identical output, the sync skips the write entirely, and the window only
// exists when something has actually changed — the same exposure createrepo
// has.
func repodataRevision(pkgs []pkgmeta.Package) time.Time {
	var newest int64
	for i := range pkgs {
		if ts := pkgs[i].UpdatedAt.Unix(); ts > newest {
			newest = ts
		}
	}
	return time.Unix(newest, 0).UTC()
}

// Sweep publishes every configured scope that has unpublished changes.
//
// The scheduled run calls it as a backstop for work that exhausted its retries
// and reached the dead-letter queue: the scope is left dirty with no message
// pending, and without this nothing would republish it short of an operator
// redriving the queue by hand.
//
// It only sees scopes whose generation has moved. A scope that has never been
// written to reads back clean, so this cannot discover a codename added to
// repos.yaml — the write to the config object is what triggers that.
func (p *Publisher) Sweep(ctx context.Context) error {
	var errs []error
	for _, scope := range p.Config.PublishScopes() {
		state, err := p.State.Get(ctx, scope)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !state.Dirty() {
			continue
		}
		if err := p.Publish(ctx, scope); err != nil {
			errs = append(errs, fmt.Errorf("publish: sweeping %s: %w", scope, err))
		}
	}
	return errors.Join(errs...)
}
