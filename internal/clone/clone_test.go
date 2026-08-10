package clone

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ashleyconnor/linux-repo-indexer/internal/repoconfig"
)

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
      architectures: [x86_64, aarch64]
      channels: [stable, test]
    - distro: RHEL
      version: "10"
      architectures: [x86_64, aarch64]
      channels: [stable, test]
    - distro: RHEL
      version: "11"
      architectures: [x86_64]
      channels: [stable]
`

func testConfig(t *testing.T) *repoconfig.Config {
	t.Helper()
	c, err := repoconfig.Parse([]byte(configYAML))
	if err != nil {
		t.Fatalf("parsing config: %v", err)
	}
	return c
}

func mustParseVersion(t *testing.T, s string) Version {
	t.Helper()
	v, err := ParseVersion(s)
	if err != nil {
		t.Fatalf("ParseVersion(%q): %v", s, err)
	}
	return v
}

// --- ParseVersion ----------------------------------------------------------

func TestParseVersion(t *testing.T) {
	v, err := ParseVersion("RHEL/9")
	if err != nil {
		t.Fatalf("ParseVersion: %v", err)
	}
	if v.Distro != "RHEL" || v.Version != "9" {
		t.Errorf("got %+v, want {RHEL 9}", v)
	}
}

func TestParseVersionRejectsMalformed(t *testing.T) {
	for _, in := range []string{"RHEL", "RHEL/9/x86_64", "RHEL/9/x86_64/stable", "", "/", "RHEL/"} {
		if _, err := ParseVersion(in); err == nil {
			t.Errorf("ParseVersion(%q) should fail, want <distro>/<version>", in)
		}
	}
}

// A codename or deb scope is the likeliest wrong input, because the shared pool
// makes copying unnecessary there. The error has to say so rather than just
// rejecting the shape.
func TestParseVersionExplainsDebScopes(t *testing.T) {
	for _, in := range []string{"deb/main/amd64", "release/noble"} {
		_, err := ParseVersion(in)
		if err == nil {
			t.Fatalf("ParseVersion(%q) should fail", in)
		}
		if !strings.Contains(err.Error(), "pool") {
			t.Errorf("ParseVersion(%q) = %v, want the error to explain the shared pool", in, err)
		}
	}
}

// --- Trees -----------------------------------------------------------------

func TestTreesExpandsEveryArchAndChannelOfTheTarget(t *testing.T) {
	pairs, err := Trees(testConfig(t), mustParseVersion(t, "RHEL/9"), mustParseVersion(t, "RHEL/10"))
	if err != nil {
		t.Fatalf("Trees: %v", err)
	}

	var got []string
	for _, p := range pairs {
		got = append(got, p.FromDir+" -> "+p.ToDir)
	}
	slices.Sort(got)

	want := []string{
		"RHEL/9/aarch64/stable -> RHEL/10/aarch64/stable",
		"RHEL/9/aarch64/test -> RHEL/10/aarch64/test",
		"RHEL/9/x86_64/stable -> RHEL/10/x86_64/stable",
		"RHEL/9/x86_64/test -> RHEL/10/x86_64/test",
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// The target drives the matrix, so a version that declares less than its
// source copies less. RHEL 11 has one arch and one channel.
func TestTreesFollowsTheTargetNotTheSource(t *testing.T) {
	pairs, err := Trees(testConfig(t), mustParseVersion(t, "RHEL/9"), mustParseVersion(t, "RHEL/11"))
	if err != nil {
		t.Fatalf("Trees: %v", err)
	}
	if len(pairs) != 1 {
		t.Fatalf("got %d pairs, want 1 for a target with one arch and one channel", len(pairs))
	}
	if got, want := pairs[0].FromDir, "RHEL/9/x86_64/stable"; got != want {
		t.Errorf("FromDir = %q, want %q", got, want)
	}
	if got, want := pairs[0].ToDir, "RHEL/11/x86_64/stable"; got != want {
		t.Errorf("ToDir = %q, want %q", got, want)
	}
}

// The source does not have to be configured: a version removed from repos.yaml
// still has its objects, and cloning from it is legitimate.
func TestTreesAllowsAnUnconfiguredSource(t *testing.T) {
	pairs, err := Trees(testConfig(t), mustParseVersion(t, "RHEL/8"), mustParseVersion(t, "RHEL/11"))
	if err != nil {
		t.Fatalf("Trees should not require the source in repos.yaml: %v", err)
	}
	if got, want := pairs[0].FromDir, "RHEL/8/x86_64/stable"; got != want {
		t.Errorf("FromDir = %q, want %q", got, want)
	}
}

func TestTreesRejectsAnUnconfiguredTarget(t *testing.T) {
	_, err := Trees(testConfig(t), mustParseVersion(t, "RHEL/9"), mustParseVersion(t, "RHEL/12"))
	if err == nil {
		t.Fatal("Trees should refuse a target that is not in repos.yaml")
	}
	if !strings.Contains(err.Error(), "repos.yaml") {
		t.Errorf("error = %v, want it to name repos.yaml", err)
	}
}

func TestTreesRejectsCloningAVersionOntoItself(t *testing.T) {
	v := mustParseVersion(t, "RHEL/9")
	if _, err := Trees(testConfig(t), v, v); err == nil {
		t.Fatal("Trees should refuse to clone a version onto itself")
	}
}

// --- Plan ------------------------------------------------------------------

func testPair(t *testing.T) TreePair {
	t.Helper()
	pairs, err := Trees(testConfig(t), mustParseVersion(t, "RHEL/9"), mustParseVersion(t, "RHEL/11"))
	if err != nil {
		t.Fatalf("Trees: %v", err)
	}
	return pairs[0]
}

func TestPlanMapsEachPackageToTheTargetPrefix(t *testing.T) {
	got := Plan(testPair(t), []string{
		"RHEL/9/x86_64/stable/a-1.0-1.x86_64.rpm",
		"RHEL/9/x86_64/stable/b-2.0-1.x86_64.rpm",
	}, nil)

	want := []Copy{
		{From: "RHEL/9/x86_64/stable/a-1.0-1.x86_64.rpm", To: "RHEL/11/x86_64/stable/a-1.0-1.x86_64.rpm"},
		{From: "RHEL/9/x86_64/stable/b-2.0-1.x86_64.rpm", To: "RHEL/11/x86_64/stable/b-2.0-1.x86_64.rpm"},
	}
	if !slices.Equal(got.Copies, want) {
		t.Errorf("got %v, want %v", got.Copies, want)
	}
}

// Filtering on the suffix is what replaces --exclude "repodata/*", and it is
// stricter: nothing that is not a package can be carried across.
func TestPlanCopiesOnlyPackages(t *testing.T) {
	got := Plan(testPair(t), []string{
		"RHEL/9/x86_64/stable/a-1.0-1.x86_64.rpm",
		"RHEL/9/x86_64/stable/repodata/repomd.xml",
		"RHEL/9/x86_64/stable/repodata/repomd.xml.asc",
		"RHEL/9/x86_64/stable/repodata/abc-primary.xml.gz",
		"RHEL/9/x86_64/stable/README",
	}, nil)

	if len(got.Copies) != 1 || !strings.HasSuffix(got.Copies[0].From, "a-1.0-1.x86_64.rpm") {
		t.Errorf("got %v, want only the .rpm", got.Copies)
	}
}

// Re-running after an interruption must not re-copy, because every copy is
// re-parsed by ingest.
func TestPlanSkipsPackagesAlreadyInTheTarget(t *testing.T) {
	got := Plan(testPair(t), []string{
		"RHEL/9/x86_64/stable/a-1.0-1.x86_64.rpm",
		"RHEL/9/x86_64/stable/b-2.0-1.x86_64.rpm",
	}, []string{
		"RHEL/11/x86_64/stable/a-1.0-1.x86_64.rpm",
	})

	if len(got.Copies) != 1 || !strings.HasSuffix(got.Copies[0].From, "b-2.0-1.x86_64.rpm") {
		t.Errorf("got %v, want only the package missing from the target", got.Copies)
	}
}

// "Nothing to do" has two causes and the report must tell them apart: an
// operator who expected packages needs to know which one happened.
func TestPlanDistinguishesAnEmptySourceFromAFullTarget(t *testing.T) {
	empty := Plan(testPair(t), nil, nil)
	if len(empty.Copies) != 0 {
		t.Fatalf("got %v, want nothing", empty.Copies)
	}
	if !strings.Contains(empty.Note, "source") {
		t.Errorf("Note = %q, want it to say the source is empty", empty.Note)
	}

	full := Plan(testPair(t),
		[]string{"RHEL/9/x86_64/stable/a-1.0-1.x86_64.rpm"},
		[]string{"RHEL/11/x86_64/stable/a-1.0-1.x86_64.rpm"})
	if len(full.Copies) != 0 {
		t.Fatalf("got %v, want nothing", full.Copies)
	}
	if !strings.Contains(full.Note, "already") {
		t.Errorf("Note = %q, want it to say the packages are already present", full.Note)
	}
}

// --- Execute ---------------------------------------------------------------

type fakeCopier struct {
	listed map[string][]string
	copied []Copy
	err    error
}

func (f *fakeCopier) List(_ context.Context, prefix string) ([]string, error) {
	return f.listed[prefix], nil
}

func (f *fakeCopier) Copy(_ context.Context, src, dst string) error {
	if f.err != nil {
		return f.err
	}
	f.copied = append(f.copied, Copy{From: src, To: dst})
	return nil
}

func TestExecuteCopiesEveryPlannedKey(t *testing.T) {
	c := &fakeCopier{}
	plans := []TreeCopy{{
		TreePair: testPair(t),
		Copies: []Copy{
			{From: "RHEL/9/x86_64/stable/a.rpm", To: "RHEL/11/x86_64/stable/a.rpm"},
			{From: "RHEL/9/x86_64/stable/b.rpm", To: "RHEL/11/x86_64/stable/b.rpm"},
		},
	}}

	n, err := Execute(context.Background(), c, plans)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if n != 2 {
		t.Errorf("copied %d, want 2", n)
	}
	if !slices.Equal(c.copied, plans[0].Copies) {
		t.Errorf("copied %v, want %v", c.copied, plans[0].Copies)
	}
}

// A partial clone that reports success would be published as though complete.
func TestExecuteStopsOnFailure(t *testing.T) {
	c := &fakeCopier{err: errors.New("access denied")}
	plans := []TreeCopy{{
		TreePair: testPair(t),
		Copies:   []Copy{{From: "RHEL/9/x86_64/stable/a.rpm", To: "RHEL/11/x86_64/stable/a.rpm"}},
	}}

	if _, err := Execute(context.Background(), c, plans); err == nil {
		t.Fatal("Execute should fail when a copy fails")
	}
}

func TestPlanAllListsBothSides(t *testing.T) {
	c := &fakeCopier{listed: map[string][]string{
		"RHEL/9/x86_64/stable/":  {"RHEL/9/x86_64/stable/a.rpm", "RHEL/9/x86_64/stable/b.rpm"},
		"RHEL/11/x86_64/stable/": {"RHEL/11/x86_64/stable/a.rpm"},
	}}

	plans, err := PlanAll(context.Background(), c, []TreePair{testPair(t)})
	if err != nil {
		t.Fatalf("PlanAll: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("got %d plans, want 1", len(plans))
	}
	if len(plans[0].Copies) != 1 || !strings.HasSuffix(plans[0].Copies[0].From, "b.rpm") {
		t.Errorf("got %v, want only b.rpm", plans[0].Copies)
	}
}
