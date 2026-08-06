package publish

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ashleyconnor/linux-repo-indexer/internal/index"
	"github.com/ashleyconnor/linux-repo-indexer/internal/pkgmeta"
	"github.com/ashleyconnor/linux-repo-indexer/internal/pkgmeta/deb"
	rpmparse "github.com/ashleyconnor/linux-repo-indexer/internal/pkgmeta/rpm"
	"github.com/ashleyconnor/linux-repo-indexer/internal/repoconfig"
	"github.com/ashleyconnor/linux-repo-indexer/internal/sign"
	"github.com/ashleyconnor/linux-repo-indexer/internal/store"
)

var publishTime = time.Date(2026, time.August, 5, 12, 0, 0, 0, time.UTC)

const configYAML = `
version: 1
apt:
  origin: HashiCorp
  label: HashiCorp
  acquire_by_hash: true
  compressions: [gz]
  components: [main]
  architectures: [amd64]
  codenames: [jammy, noble]
rpm:
  trees:
    - distro: RHEL
      version: "9"
      architectures: [x86_64]
      channels: [stable]
`

// --- fakes -----------------------------------------------------------------

type fakePackages struct {
	byScope map[string][]pkgmeta.Package
	err     error
}

func (f *fakePackages) List(_ context.Context, scope repoconfig.Scope) ([]pkgmeta.Package, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byScope[scope.String()], nil
}

type fakeState struct {
	states map[string]store.State

	leaseHeldOn map[string]bool
	recordErr   error
	dirtied     []string
	recorded    map[string][]store.PublishedArtifact
	released    []string
	acquired    []string
}

func newFakeState() *fakeState {
	return &fakeState{
		states:      map[string]store.State{},
		leaseHeldOn: map[string]bool{},
		recorded:    map[string][]store.PublishedArtifact{},
	}
}

func (f *fakeState) Get(_ context.Context, scope repoconfig.Scope) (store.State, error) {
	st, ok := f.states[scope.String()]
	if !ok {
		return store.State{Scope: scope.String()}, nil
	}
	return st, nil
}

func (f *fakeState) MarkDirty(_ context.Context, scope repoconfig.Scope) (int64, error) {
	f.dirtied = append(f.dirtied, scope.String())
	st := f.states[scope.String()]
	st.Scope = scope.String()
	st.Generation++
	f.states[scope.String()] = st
	return st.Generation, nil
}

func (f *fakeState) AcquireLease(_ context.Context, scope repoconfig.Scope, owner string, _ time.Duration, _ time.Time) (store.State, error) {
	if f.leaseHeldOn[scope.String()] {
		return store.State{}, fmt.Errorf("%w: %s", store.ErrLeaseHeld, scope)
	}
	f.acquired = append(f.acquired, scope.String())
	st, ok := f.states[scope.String()]
	if !ok {
		st = store.State{Scope: scope.String()}
	}
	st.LeaseOwner = owner
	f.states[scope.String()] = st
	return st, nil
}

func (f *fakeState) ReleaseLease(_ context.Context, scope repoconfig.Scope, _ string) error {
	f.released = append(f.released, scope.String())
	return nil
}

func (f *fakeState) RecordPublished(_ context.Context, scope repoconfig.Scope, _ string, generation int64, artifacts []store.PublishedArtifact, _ time.Time) error {
	if f.recordErr != nil {
		return f.recordErr
	}
	f.recorded[scope.String()] = artifacts
	st := f.states[scope.String()]
	st.PublishedGeneration = generation
	st.Artifacts = artifacts
	f.states[scope.String()] = st
	return nil
}

type fakeSync struct {
	written map[string][]byte
	pruned  []string
	err     error
}

func newFakeSync() *fakeSync { return &fakeSync{written: map[string][]byte{}} }

func (f *fakeSync) Put(_ context.Context, artifacts []index.Artifact) error {
	if f.err != nil {
		return f.err
	}
	for _, a := range artifacts {
		f.written[a.Path] = a.Body
	}
	return nil
}

func (f *fakeSync) Prune(_ context.Context, dir string, _ map[string]bool, _ time.Time) error {
	f.pruned = append(f.pruned, dir)
	return nil
}

func (f *fakeSync) paths() []string {
	out := make([]string, 0, len(f.written))
	for p := range f.written {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

type fakeQueue struct{ enqueued []string }

func (q *fakeQueue) EnqueuePublish(_ context.Context, scope repoconfig.Scope, _ time.Duration) error {
	q.enqueued = append(q.enqueued, scope.String())
	return nil
}

// --- helpers ---------------------------------------------------------------

func testConfig(t *testing.T) *repoconfig.Config {
	t.Helper()
	c, err := repoconfig.Parse([]byte(configYAML))
	if err != nil {
		t.Fatalf("parsing config: %v", err)
	}
	return c
}

func loadDeb(t *testing.T, name string) pkgmeta.Package {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("../../testdata/packages/deb", name))
	if err != nil {
		t.Fatalf("reading fixture: %v (run ./testdata/generate.sh)", err)
	}
	p, err := deb.ParseBytes(raw)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	d := index.DigestsOf(raw)
	p.S3Key = "pool/amd64/main/" + name
	p.Filename = p.S3Key
	p.Size, p.MD5, p.SHA1, p.SHA256 = d.Size, d.MD5, d.SHA1, d.SHA256
	return *p
}

func loadRPM(t *testing.T, name string) pkgmeta.Package {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("../../testdata/packages/rpm", name))
	if err != nil {
		t.Fatalf("reading fixture: %v (run ./testdata/generate.sh)", err)
	}
	p, err := rpmparse.ParseBytes(raw)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	d := index.DigestsOf(raw)
	p.S3Key = "RHEL/9/x86_64/stable/" + name
	p.Filename = name
	p.Size, p.MD5, p.SHA1, p.SHA256 = d.Size, d.MD5, d.SHA1, d.SHA256
	return *p
}

type harness struct {
	pub   *Publisher
	state *fakeState
	sync  *fakeSync
	queue *fakeQueue
	pkgs  *fakePackages
	key   []byte // armored public key
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	source, public, err := sign.GenerateTestKey("Fixture", "fixtures@example.com")
	if err != nil {
		t.Fatalf("GenerateTestKey: %v", err)
	}

	h := &harness{
		state: newFakeState(),
		sync:  newFakeSync(),
		queue: &fakeQueue{},
		pkgs:  &fakePackages{byScope: map[string][]pkgmeta.Package{}},
		key:   public,
	}
	h.pub = &Publisher{
		Config:   testConfig(t),
		Packages: h.pkgs,
		State:    h.state,
		Sync:     h.sync,
		Signer:   sign.NewPGPSigner(source),
		Queue:    h.queue,
		Owner:    "test-owner",
		LeaseTTL: time.Minute,
		Now:      func() time.Time { return publishTime },
	}
	return h
}

// markDirty makes a scope have unpublished changes.
func (h *harness) markDirty(scope repoconfig.Scope) {
	h.state.states[scope.String()] = store.State{Scope: scope.String(), Generation: 1}
}

// --- tests -----------------------------------------------------------------

func TestPublishDebPoolWritesEveryCodename(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	scope := repoconfig.DebPoolScope("main", "amd64")

	h.pkgs.byScope[scope.String()] = []pkgmeta.Package{
		loadDeb(t, "indexer-fixture_1.0.0-1_amd64.deb"),
		loadDeb(t, "indexer-other_2.5.0-1_amd64.deb"),
	}
	h.markDirty(scope)

	if err := h.pub.Publish(ctx, scope); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// The pool is codename-independent, so one build lands in both codenames.
	for _, codename := range []string{"jammy", "noble"} {
		for _, name := range []string{"Packages", "Packages.gz"} {
			p := "dists/" + codename + "/main/binary-amd64/" + name
			if _, ok := h.sync.written[p]; !ok {
				t.Errorf("missing %s\ngot: %v", p, h.sync.paths())
			}
		}
	}

	jammy := h.sync.written["dists/jammy/main/binary-amd64/Packages"]
	noble := h.sync.written["dists/noble/main/binary-amd64/Packages"]
	if string(jammy) != string(noble) {
		t.Error("codenames must receive byte-identical Packages files")
	}
	if !strings.Contains(string(noble), "Package: indexer-fixture\n") {
		t.Errorf("Packages does not list the fixture:\n%s", noble)
	}
}

func TestPublishDebPoolWritesByHash(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	scope := repoconfig.DebPoolScope("main", "amd64")

	h.pkgs.byScope[scope.String()] = []pkgmeta.Package{loadDeb(t, "indexer-fixture_1.0.0-1_amd64.deb")}
	h.markDirty(scope)

	if err := h.pub.Publish(ctx, scope); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	var byHash int
	for _, p := range h.sync.paths() {
		if strings.Contains(p, "/by-hash/") {
			byHash++
		}
	}
	// Two variants x three algorithms x two codenames.
	if want := 2 * 3 * 2; byHash != want {
		t.Errorf("wrote %d by-hash objects, want %d:\n%v", byHash, want, h.sync.paths())
	}

	if len(h.sync.pruned) != 2 {
		t.Errorf("pruned %d by-hash directories, want one per codename: %v", len(h.sync.pruned), h.sync.pruned)
	}
}

func TestPublishDebPoolTriggersReleaseRebuild(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	scope := repoconfig.DebPoolScope("main", "amd64")

	h.pkgs.byScope[scope.String()] = []pkgmeta.Package{loadDeb(t, "indexer-fixture_1.0.0-1_amd64.deb")}
	h.markDirty(scope)

	if err := h.pub.Publish(ctx, scope); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// A Release spans every component and architecture, so it cannot be built
	// from this scope; each codename is marked dirty and queued instead.
	want := []string{"release/jammy", "release/noble"}
	if !slices.Equal(h.state.dirtied, want) {
		t.Errorf("marked %v dirty, want %v", h.state.dirtied, want)
	}
	if !slices.Equal(h.queue.enqueued, want) {
		t.Errorf("enqueued %v, want %v", h.queue.enqueued, want)
	}
}

func TestPublishDebPoolRecordsDigestsForRelease(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	scope := repoconfig.DebPoolScope("main", "amd64")

	h.pkgs.byScope[scope.String()] = []pkgmeta.Package{loadDeb(t, "indexer-fixture_1.0.0-1_amd64.deb")}
	h.markDirty(scope)

	if err := h.pub.Publish(ctx, scope); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	recorded := h.state.recorded[scope.String()]
	if len(recorded) != 2 {
		t.Fatalf("recorded %d artifacts, want 2 (Packages and Packages.gz)", len(recorded))
	}
	// Paths are relative to the codename directory, which is how a Release
	// file cites them, and are what lets the Release be built without S3.
	for _, a := range recorded {
		if !strings.HasPrefix(a.Path, "main/binary-amd64/") {
			t.Errorf("recorded path %q should be relative to the codename directory", a.Path)
		}
		if a.Digests.SHA256 == "" || a.Digests.Size == 0 {
			t.Errorf("recorded %q without usable digests: %+v", a.Path, a.Digests)
		}
	}
}

func TestPublishReleaseSignsFromRecordedDigests(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	pool := repoconfig.DebPoolScope("main", "amd64")
	h.pkgs.byScope[pool.String()] = []pkgmeta.Package{loadDeb(t, "indexer-fixture_1.0.0-1_amd64.deb")}
	h.markDirty(pool)
	if err := h.pub.Publish(ctx, pool); err != nil {
		t.Fatalf("publishing pool: %v", err)
	}

	release := repoconfig.DebReleaseScope("noble")
	if err := h.pub.Publish(ctx, release); err != nil {
		t.Fatalf("publishing release: %v", err)
	}

	// The publisher never re-read S3 to build this: the digests came from the
	// pool scope's recorded state.
	body, ok := h.sync.written["dists/noble/Release"]
	if !ok {
		t.Fatalf("no Release written:\n%v", h.sync.paths())
	}
	for _, want := range []string{"Codename: noble", "Acquire-By-Hash: yes", "main/binary-amd64/Packages.gz"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("Release is missing %q:\n%s", want, body)
		}
	}

	inRelease, ok := h.sync.written["dists/noble/InRelease"]
	if !ok {
		t.Fatal("no InRelease written")
	}
	payload, err := sign.VerifyClearSigned(h.key, inRelease)
	if err != nil {
		t.Fatalf("InRelease does not verify: %v", err)
	}
	if strings.TrimSpace(string(payload)) != strings.TrimSpace(string(body)) {
		t.Error("InRelease payload differs from Release")
	}

	detached, ok := h.sync.written["dists/noble/Release.gpg"]
	if !ok {
		t.Fatal("no Release.gpg written")
	}
	if err := sign.Verify(h.key, body, detached); err != nil {
		t.Fatalf("Release.gpg does not verify: %v", err)
	}
}

func TestPublishReleaseSkipsWhenNothingPublishedYet(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	release := repoconfig.DebReleaseScope("noble")
	h.markDirty(release)

	if err := h.pub.Publish(ctx, release); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	// A Release listing no files would tell apt the repository is empty.
	if len(h.sync.written) != 0 {
		t.Errorf("should not write a Release before any index exists, wrote: %v", h.sync.paths())
	}
}

func TestPublishRPMTree(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	scope := repoconfig.RPMTreeScope("RHEL", "9", "x86_64", "stable")

	h.pkgs.byScope[scope.String()] = []pkgmeta.Package{
		loadRPM(t, "indexer-fixture-1.0.0-1.x86_64.rpm"),
		loadRPM(t, "indexer-fixture-1.1.0-1.x86_64.rpm"),
	}
	h.markDirty(scope)

	if err := h.pub.Publish(ctx, scope); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	const treeDir = "RHEL/9/x86_64/stable"
	repomd, ok := h.sync.written[treeDir+"/repodata/repomd.xml"]
	if !ok {
		t.Fatalf("no repomd.xml written:\n%v", h.sync.paths())
	}

	signature, ok := h.sync.written[treeDir+"/repodata/repomd.xml.asc"]
	if !ok {
		t.Fatal("no repomd.xml.asc written; dnf needs it when repo_gpgcheck is on")
	}
	if err := sign.Verify(h.key, repomd, signature); err != nil {
		t.Fatalf("repomd.xml.asc does not verify: %v", err)
	}

	// Every metadata file repomd names must have been written.
	var metadata int
	for _, p := range h.sync.paths() {
		if strings.HasPrefix(p, treeDir+"/repodata/") && strings.HasSuffix(p, ".xml.gz") {
			metadata++
			if !strings.Contains(string(repomd), strings.TrimPrefix(p, treeDir+"/")) {
				t.Errorf("%s was written but is not referenced by repomd.xml", p)
			}
		}
	}
	if metadata != 3 {
		t.Errorf("wrote %d metadata files, want 3 (primary, filelists, other)", metadata)
	}

	if len(h.sync.pruned) != 1 || h.sync.pruned[0] != treeDir+"/repodata" {
		t.Errorf("pruned %v, want the repodata directory", h.sync.pruned)
	}
}

func TestRepublishingUnchangedPackagesIsByteIdentical(t *testing.T) {
	// repomd.xml and repomd.xml.asc are two separate objects, so a publish
	// that rewrites them opens a window where a client can fetch the new
	// repomd.xml against the old signature and reject the repository. Keeping
	// unchanged input byte-identical is what closes that window: the sync
	// skips the write entirely.
	ctx := context.Background()
	scope := repoconfig.RPMTreeScope("RHEL", "9", "x86_64", "stable")
	pkgs := []pkgmeta.Package{loadRPM(t, "indexer-fixture-1.0.0-1.x86_64.rpm")}

	publishOnce := func(at time.Time) map[string][]byte {
		h := newHarness(t)
		h.pub.Now = func() time.Time { return at }
		h.pkgs.byScope[scope.String()] = pkgs
		h.markDirty(scope)
		if err := h.pub.Publish(ctx, scope); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		return h.sync.written
	}

	// Two publishes an hour apart, with the same packages.
	first := publishOnce(publishTime)
	second := publishOnce(publishTime.Add(time.Hour))

	const repomd = "RHEL/9/x86_64/stable/repodata/repomd.xml"
	if !bytes.Equal(first[repomd], second[repomd]) {
		t.Errorf("repomd.xml changed between publishes of identical packages:\n%s\n---\n%s",
			first[repomd], second[repomd])
	}

	for path, body := range first {
		if path == repomd+".asc" {
			continue // signatures embed their own creation time
		}
		if !bytes.Equal(body, second[path]) {
			t.Errorf("%s changed between publishes of identical packages", path)
		}
	}
}

func TestPublishSkipsCleanScope(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	scope := repoconfig.DebPoolScope("main", "amd64")

	// Published at its current generation: a coalesced duplicate message.
	h.state.states[scope.String()] = store.State{Scope: scope.String(), Generation: 4, PublishedGeneration: 4}

	if err := h.pub.Publish(ctx, scope); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(h.sync.written) != 0 {
		t.Errorf("a clean scope must not be rewritten, wrote: %v", h.sync.paths())
	}
	// The lease is still taken and released, so the check is race-free.
	if len(h.state.released) != 1 {
		t.Errorf("released the lease %d times, want 1", len(h.state.released))
	}
}

func TestPublishYieldsWhenLeaseIsHeld(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	scope := repoconfig.DebPoolScope("main", "amd64")

	h.markDirty(scope)
	h.state.leaseHeldOn[scope.String()] = true

	// Not an error: whoever holds the lease reads the current generation, so
	// our work is already covered.
	if err := h.pub.Publish(ctx, scope); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(h.sync.written) != 0 {
		t.Errorf("must not write while another publisher holds the lease: %v", h.sync.paths())
	}
}

func TestPublishSkipsUnconfiguredScope(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	// Removed from repos.yaml. Publishing stops; the tree is left alone.
	scope := repoconfig.DebReleaseScope("trixie")
	h.markDirty(scope)

	if err := h.pub.Publish(ctx, scope); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(h.sync.written) != 0 {
		t.Errorf("wrote %v for a scope no longer in the config", h.sync.paths())
	}
	if len(h.state.acquired) != 0 {
		t.Error("should not even take the lease for an unconfigured scope")
	}
}

func TestPublishLeavesScopeDirtyWhenRecordingFails(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	scope := repoconfig.DebPoolScope("main", "amd64")

	h.pkgs.byScope[scope.String()] = []pkgmeta.Package{loadDeb(t, "indexer-fixture_1.0.0-1_amd64.deb")}
	h.markDirty(scope)
	h.state.recordErr = errors.New("lost the lease")

	if err := h.pub.Publish(ctx, scope); err == nil {
		t.Fatal("expected an error when the generation cannot be recorded")
	}

	// The scope must stay dirty so it is republished, rather than being
	// recorded as current while potentially stale.
	st, _ := h.state.Get(ctx, scope)
	if !st.Dirty() {
		t.Error("scope should still be dirty after a failed record")
	}
}

func TestPublishPropagatesReadFailure(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	scope := repoconfig.DebPoolScope("main", "amd64")

	h.markDirty(scope)
	h.pkgs.err = errors.New("dynamodb unavailable")

	if err := h.pub.Publish(ctx, scope); err == nil {
		t.Fatal("expected the read failure to surface")
	}
	if len(h.state.released) != 1 {
		t.Error("the lease must be released even when publishing fails")
	}
}

func TestSweepPublishesOnlyDirtyScopes(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	pool := repoconfig.DebPoolScope("main", "amd64")
	h.pkgs.byScope[pool.String()] = []pkgmeta.Package{loadDeb(t, "indexer-fixture_1.0.0-1_amd64.deb")}
	h.markDirty(pool)

	// Everything else is clean and must be left alone.
	if err := h.pub.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if _, ok := h.state.recorded[pool.String()]; !ok {
		t.Error("the dirty scope should have been published")
	}
	for _, scope := range h.pub.Config.PublishScopes() {
		if scope == pool {
			continue
		}
		if scope.Kind == repoconfig.KindDebRelease {
			continue // legitimately dirtied by the pool publish
		}
		if _, ok := h.state.recorded[scope.String()]; ok {
			t.Errorf("clean scope %s should not have been published", scope)
		}
	}
}
