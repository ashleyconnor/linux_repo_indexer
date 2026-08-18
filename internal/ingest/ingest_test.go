package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ashleyconnor/linux-repo-indexer/internal/pkgmeta"
	"github.com/ashleyconnor/linux-repo-indexer/internal/repoconfig"
)

var ingestTime = time.Date(2026, time.August, 5, 12, 0, 0, 0, time.UTC)

const configYAML = `
version: 1
apt:
  components: [main]
  architectures: [amd64]
  codenames: [noble]
rpm:
  trees:
    - distro: RHEL
      version: "9"
      architectures: [x86_64]
      channels: [stable]
`

type fakeObjects struct {
	objects map[string][]byte
	err     error
	modTime time.Time
}

func (f *fakeObjects) Get(_ context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	if f.err != nil {
		return nil, ObjectInfo{}, f.err
	}
	body, ok := f.objects[key]
	if !ok {
		return nil, ObjectInfo{}, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(body)), ObjectInfo{
		Size:         int64(len(body)),
		LastModified: f.modTime,
	}, nil
}

type fakePackages struct {
	put     []*pkgmeta.Package
	scopes  []string
	deleted []string
	err     error
}

func (f *fakePackages) Put(_ context.Context, scope repoconfig.Scope, pkg *pkgmeta.Package) error {
	if f.err != nil {
		return f.err
	}
	f.put = append(f.put, pkg)
	f.scopes = append(f.scopes, scope.String())
	return nil
}

func (f *fakePackages) Delete(_ context.Context, scope repoconfig.Scope, filename string) error {
	if f.err != nil {
		return f.err
	}
	f.deleted = append(f.deleted, scope.String()+" "+filename)
	return nil
}

type fakeState struct{ dirtied []string }

func (f *fakeState) MarkDirty(_ context.Context, scope repoconfig.Scope) (int64, error) {
	f.dirtied = append(f.dirtied, scope.String())
	return int64(len(f.dirtied)), nil
}

type fakeQueue struct {
	scopes []string
	delays []time.Duration
}

func (q *fakeQueue) EnqueuePublish(_ context.Context, scope repoconfig.Scope, delay time.Duration) error {
	q.scopes = append(q.scopes, scope.String())
	q.delays = append(q.delays, delay)
	return nil
}

type harness struct {
	ing   *Ingester
	objs  *fakeObjects
	pkgs  *fakePackages
	state *fakeState
	queue *fakeQueue
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	cfg, err := repoconfig.Parse([]byte(configYAML))
	if err != nil {
		t.Fatalf("parsing config: %v", err)
	}

	h := &harness{
		objs:  &fakeObjects{objects: map[string][]byte{}, modTime: ingestTime},
		pkgs:  &fakePackages{},
		state: &fakeState{},
		queue: &fakeQueue{},
	}
	h.ing = &Ingester{
		Config:       cfg,
		ConfigKey:    "repos.yaml",
		Objects:      h.objs,
		Packages:     h.pkgs,
		State:        h.state,
		Queue:        h.queue,
		PublishDelay: 30 * time.Second,
		Now:          func() time.Time { return ingestTime },
	}
	return h
}

func (h *harness) upload(t *testing.T, key, dir, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("../../testdata/packages", dir, name))
	if err != nil {
		t.Fatalf("reading fixture: %v (run ./testdata/generate.sh)", err)
	}
	h.objs.objects[key] = raw
	return raw
}

func TestCreatedDeb(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	const key = "pool/amd64/main/indexer-fixture_1.0.0-1_amd64.deb"
	raw := h.upload(t, key, "deb", "indexer-fixture_1.0.0-1_amd64.deb")

	if err := h.ing.Created(ctx, key); err != nil {
		t.Fatalf("Created: %v", err)
	}
	if len(h.pkgs.put) != 1 {
		t.Fatalf("stored %d packages, want 1", len(h.pkgs.put))
	}

	pkg := h.pkgs.put[0]
	if got, want := h.pkgs.scopes[0], "deb/main/amd64"; got != want {
		t.Errorf("scope = %q, want %q", got, want)
	}
	if got, want := pkg.Name, "indexer-fixture"; got != want {
		t.Errorf("Name = %q, want %q", got, want)
	}
	if got, want := pkg.S3Key, key; got != want {
		t.Errorf("S3Key = %q, want %q", got, want)
	}
	// apt resolves Filename against the repository root.
	if got, want := pkg.Filename, key; got != want {
		t.Errorf("Filename = %q, want %q", got, want)
	}
	if got, want := pkg.Size, int64(len(raw)); got != want {
		t.Errorf("Size = %d, want %d", got, want)
	}
	if !pkg.UpdatedAt.Equal(ingestTime) {
		t.Errorf("UpdatedAt = %v, want %v", pkg.UpdatedAt, ingestTime)
	}
}

func TestCreatedHashesTheWholeObject(t *testing.T) {
	// The parser stops at the end of the control data, so the checksums are
	// only correct if the remainder is drained. Getting this wrong would put
	// a hash of the first few kilobytes into the index, and every client
	// would reject every download.
	ctx := context.Background()
	h := newHarness(t)

	const key = "pool/amd64/main/indexer-fixture_1.0.0-1_amd64.deb"
	raw := h.upload(t, key, "deb", "indexer-fixture_1.0.0-1_amd64.deb")

	if err := h.ing.Created(ctx, key); err != nil {
		t.Fatalf("Created: %v", err)
	}

	want := sha256.Sum256(raw)
	if got := h.pkgs.put[0].SHA256; got != hex.EncodeToString(want[:]) {
		t.Errorf("SHA256 = %s, want %s (the whole object)", got, hex.EncodeToString(want[:]))
	}
	if h.pkgs.put[0].MD5 == "" || h.pkgs.put[0].SHA1 == "" {
		t.Error("MD5 and SHA1 should also be computed in the same pass")
	}
}

func TestCreatedRPM(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	const key = "RHEL/9/x86_64/stable/indexer-fixture-1.0.0-1.x86_64.rpm"
	raw := h.upload(t, key, "rpm", "indexer-fixture-1.0.0-1.x86_64.rpm")

	if err := h.ing.Created(ctx, key); err != nil {
		t.Fatalf("Created: %v", err)
	}

	pkg := h.pkgs.put[0]
	if got, want := h.pkgs.scopes[0], "rpm/RHEL/9/x86_64/stable"; got != want {
		t.Errorf("scope = %q, want %q", got, want)
	}
	// dnf resolves location href against the tree root, so it is the basename.
	if got, want := pkg.Filename, "indexer-fixture-1.0.0-1.x86_64.rpm"; got != want {
		t.Errorf("Filename = %q, want %q", got, want)
	}
	// createrepo records the file's mtime; in S3 that is last-modified.
	if got, want := pkg.RPM.FileTime, ingestTime.Unix(); got != want {
		t.Errorf("FileTime = %d, want %d", got, want)
	}
	want := sha256.Sum256(raw)
	if got := pkg.SHA256; got != hex.EncodeToString(want[:]) {
		t.Errorf("SHA256 = %s, want the whole object's", got)
	}
}

func TestCreatedSchedulesCoalescedPublish(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	const key = "pool/amd64/main/indexer-fixture_1.0.0-1_amd64.deb"
	h.upload(t, key, "deb", "indexer-fixture_1.0.0-1_amd64.deb")

	if err := h.ing.Created(ctx, key); err != nil {
		t.Fatalf("Created: %v", err)
	}

	if !slices.Equal(h.state.dirtied, []string{"deb/main/amd64"}) {
		t.Errorf("marked %v dirty, want the pool scope", h.state.dirtied)
	}
	if !slices.Equal(h.queue.scopes, []string{"deb/main/amd64"}) {
		t.Errorf("enqueued %v, want the pool scope", h.queue.scopes)
	}
	// The delay is what collapses a bulk upload into one index build.
	if h.queue.delays[0] != 30*time.Second {
		t.Errorf("delay = %v, want the coalescing window", h.queue.delays[0])
	}
}

func TestCreatedRepublishesEveryScopeWhenTheConfigChanges(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	// A codename added to repos.yaml has no upload to trigger it, and its
	// scopes read back clean because nothing has ever bumped their
	// generation. Marking them dirty here is what gives the publisher
	// something to do.
	if err := h.ing.Created(ctx, "repos.yaml"); err != nil {
		t.Fatalf("Created: %v", err)
	}

	want := []string{"deb/main/amd64", "release/noble", "rpm/RHEL/9/x86_64/stable"}

	dirtied := slices.Clone(h.state.dirtied)
	slices.Sort(dirtied)
	if !slices.Equal(dirtied, want) {
		t.Errorf("marked %v dirty, want every configured scope %v", dirtied, want)
	}

	enqueued := slices.Clone(h.queue.scopes)
	slices.Sort(enqueued)
	if !slices.Equal(enqueued, want) {
		t.Errorf("enqueued %v, want every configured scope %v", enqueued, want)
	}
}

func TestCreatedDoesNotReadTheConfigAsAPackage(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	// The object is never fetched: the handler has already loaded the new
	// config for this invocation, so parsing it here would be a second read
	// of a file that is not a package.
	if err := h.ing.Created(ctx, "repos.yaml"); err != nil {
		t.Fatalf("Created: %v", err)
	}
	if len(h.pkgs.put) != 0 {
		t.Errorf("stored %d packages for the config object, want none", len(h.pkgs.put))
	}
}

func TestCreatedSkipsUnindexedKeys(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	// Notably including the index files the publisher itself writes: without
	// this, publishing would trigger ingest, which would trigger publishing.
	keys := []string{
		"dists/noble/main/binary-amd64/Packages.gz",
		"dists/noble/InRelease",
		"RHEL/9/x86_64/stable/repodata/repomd.xml",
		"pool/riscv64/main/thing_1.0_riscv64.deb",
	}

	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			if err := h.ing.Created(ctx, key); err != nil {
				t.Fatalf("Created(%q) = %v, want it skipped", key, err)
			}
		})
	}

	if len(h.pkgs.put) != 0 {
		t.Errorf("stored %d packages, want none", len(h.pkgs.put))
	}
	if len(h.state.dirtied) != 0 {
		t.Errorf("marked %v dirty, want nothing", h.state.dirtied)
	}
}

func TestRemoved(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	const key = "pool/amd64/main/indexer-fixture_1.0.0-1_amd64.deb"
	if err := h.ing.Removed(ctx, key); err != nil {
		t.Fatalf("Removed: %v", err)
	}

	// The object is gone, so it cannot be re-parsed. Records are keyed by
	// object precisely so the key alone is enough.
	want := "deb/main/amd64 " + key
	if !slices.Equal(h.pkgs.deleted, []string{want}) {
		t.Errorf("deleted %v, want %q", h.pkgs.deleted, want)
	}
	if !slices.Equal(h.state.dirtied, []string{"deb/main/amd64"}) {
		t.Errorf("marked %v dirty, want the pool scope", h.state.dirtied)
	}
}

func TestRemovedRPMUsesBasename(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	const key = "RHEL/9/x86_64/stable/indexer-other-2.5.0-1.x86_64.rpm"
	if err := h.ing.Removed(ctx, key); err != nil {
		t.Fatalf("Removed: %v", err)
	}

	// This package has epoch 2, which never appears in the filename. Keying
	// records by object is what makes the deletion possible at all.
	want := "rpm/RHEL/9/x86_64/stable indexer-other-2.5.0-1.x86_64.rpm"
	if !slices.Equal(h.pkgs.deleted, []string{want}) {
		t.Errorf("deleted %v, want %q", h.pkgs.deleted, want)
	}
}

func TestRemovedSkipsUnindexedKeys(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	if err := h.ing.Removed(ctx, "dists/noble/main/binary-amd64/Packages.gz"); err != nil {
		t.Fatalf("Removed: %v", err)
	}
	if len(h.pkgs.deleted) != 0 {
		t.Errorf("deleted %v, want nothing", h.pkgs.deleted)
	}
}

func TestCreatedRejectsCorruptPackage(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	const key = "pool/amd64/main/broken_1.0_amd64.deb"
	h.objs.objects[key] = []byte("this is not a deb")

	// A corrupt package must fail so the message goes to the DLQ, rather than
	// being silently dropped and leaving the repository missing a package.
	if err := h.ing.Created(ctx, key); err == nil {
		t.Fatal("expected an error for an unparseable package")
	}
	if len(h.state.dirtied) != 0 {
		t.Error("a failed ingest must not mark the scope dirty")
	}
}

func TestCreatedPropagatesFetchFailure(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.objs.err = errors.New("s3 unavailable")

	if err := h.ing.Created(ctx, "pool/amd64/main/thing_1.0_amd64.deb"); err == nil {
		t.Fatal("expected the fetch failure to surface so the message is retried")
	}
}

func TestCreatedDoesNotEnqueueWhenStoreFails(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	const key = "pool/amd64/main/indexer-fixture_1.0.0-1_amd64.deb"
	h.upload(t, key, "deb", "indexer-fixture_1.0.0-1_amd64.deb")
	h.pkgs.err = errors.New("dynamodb unavailable")

	if err := h.ing.Created(ctx, key); err == nil {
		t.Fatal("expected the store failure to surface")
	}
	if len(h.queue.scopes) != 0 {
		t.Error("must not schedule a publish for a package that was not stored")
	}
}
