package repoconfig

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ashleyconnor/linux-repo-indexer/internal/index"
)

const testYAML = `
version: 1
signing:
  secret_id: linux-repo-indexer/gpg-signing-key
apt:
  origin: HashiCorp
  label: HashiCorp
  pool_prefix: pool
  dists_prefix: dists
  acquire_by_hash: true
  compressions: [gz, xz]
  components: [main, test]
  architectures: [amd64, arm64]
  codenames: [noble]
rpm:
  trees:
    - distro: RHEL
      version: "9"
      architectures: [x86_64, aarch64]
      channels: [stable, test]
`

func mustParse(t *testing.T) *Config {
	t.Helper()
	c, err := Parse([]byte(testYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return c
}

func TestParse(t *testing.T) {
	c := mustParse(t)

	if got, want := c.Signing.SecretID, "linux-repo-indexer/gpg-signing-key"; got != want {
		t.Errorf("SecretID = %q, want %q", got, want)
	}
	if !c.APT.AcquireByHash {
		t.Error("AcquireByHash should be true")
	}
	if got, want := c.APT.Compressions, []index.Compression{index.CompressionGzip, index.CompressionXZ}; !slices.Equal(got, want) {
		t.Errorf("Compressions = %v, want %v", got, want)
	}
	if got, want := len(c.RPM.Trees), 1; got != want {
		t.Fatalf("got %d trees, want %d", got, want)
	}
	// Quoted so YAML keeps "9" a string; an unquoted 9 would decode as an int
	// and fail, which is the point of KnownFields-style strictness.
	if got, want := c.RPM.Trees[0].Version, "9"; got != want {
		t.Errorf("tree version = %q, want %q", got, want)
	}
}

func TestParseDefaultsPrefixes(t *testing.T) {
	c, err := Parse([]byte(`
version: 1
apt:
  components: [main]
  architectures: [amd64]
  codenames: [noble]
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := c.APT.PoolPrefix, "pool"; got != want {
		t.Errorf("PoolPrefix = %q, want the default %q", got, want)
	}
	if got, want := c.APT.DistsPrefix, "dists"; got != want {
		t.Errorf("DistsPrefix = %q, want the default %q", got, want)
	}
}

func TestParseRejectsUnknownField(t *testing.T) {
	// A typo in repos.yaml must fail loudly rather than silently take a
	// default, because the default might publish a different repository.
	_, err := Parse([]byte(`
version: 1
apt:
  componets: [main]
  architectures: [amd64]
  codenames: [noble]
`))
	if err == nil {
		t.Fatal("expected an error for a misspelled key")
	}
}

func TestValidateRejectsBadConfig(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{"wrong version", "version: 2\napt:\n  components: [main]\n  architectures: [amd64]\n  codenames: [noble]\n"},
		{"nothing to publish", "version: 1\n"},
		{"no components", "version: 1\napt:\n  architectures: [amd64]\n  codenames: [noble]\n"},
		{"no architectures", "version: 1\napt:\n  components: [main]\n  codenames: [noble]\n"},
		{"no codenames", "version: 1\napt:\n  components: [main]\n  architectures: [amd64]\n"},
		{"empty component", "version: 1\napt:\n  components: [main, \"\"]\n  architectures: [amd64]\n  codenames: [noble]\n"},
		{"unsupported compression", "version: 1\napt:\n  compressions: [bz2]\n  components: [main]\n  architectures: [amd64]\n  codenames: [noble]\n"},
		{"prefixes collide", "version: 1\napt:\n  pool_prefix: x\n  dists_prefix: x\n  components: [main]\n  architectures: [amd64]\n  codenames: [noble]\n"},
		{"nested prefix", "version: 1\napt:\n  pool_prefix: a/b\n  components: [main]\n  architectures: [amd64]\n  codenames: [noble]\n"},
		{"rpm tree with no arch", "version: 1\nrpm:\n  trees:\n    - distro: RHEL\n      version: \"9\"\n      channels: [stable]\n"},
		{"duplicate rpm tree", "version: 1\nrpm:\n  trees:\n    - distro: RHEL\n      version: \"9\"\n      architectures: [x86_64]\n      channels: [stable]\n    - distro: RHEL\n      version: \"9\"\n      architectures: [aarch64]\n      channels: [stable]\n"},
		{"rpm distro collides with pool", "version: 1\napt:\n  components: [main]\n  architectures: [amd64]\n  codenames: [noble]\nrpm:\n  trees:\n    - distro: pool\n      version: \"9\"\n      architectures: [x86_64]\n      channels: [stable]\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse([]byte(tt.yaml)); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}

func TestClassifyDeb(t *testing.T) {
	c := mustParse(t)

	scope, filename, err := c.Classify("pool/amd64/main/indexer-fixture_1.0.0-1_amd64.deb")
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if got, want := scope.String(), "deb/main/amd64"; got != want {
		t.Errorf("scope = %q, want %q", got, want)
	}
	// apt resolves Filename against the repository root, so it is the key.
	if got, want := filename, "pool/amd64/main/indexer-fixture_1.0.0-1_amd64.deb"; got != want {
		t.Errorf("filename = %q, want %q", got, want)
	}
}

func TestClassifyRPM(t *testing.T) {
	c := mustParse(t)

	scope, filename, err := c.Classify("RHEL/9/x86_64/stable/indexer-fixture-1.0.0-1.x86_64.rpm")
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if got, want := scope.String(), "rpm/RHEL/9/x86_64/stable"; got != want {
		t.Errorf("scope = %q, want %q", got, want)
	}
	// dnf resolves location href against the tree root, so it is the basename.
	if got, want := filename, "indexer-fixture-1.0.0-1.x86_64.rpm"; got != want {
		t.Errorf("filename = %q, want %q", got, want)
	}
}

func TestClassifySkipsUnindexedKeys(t *testing.T) {
	c := mustParse(t)

	// Every one of these must be skipped rather than fail the batch: an index
	// file, an unconfigured architecture, a stray object, a package at the
	// wrong depth.
	keys := []string{
		"dists/noble/main/binary-amd64/Packages.gz",
		"dists/noble/Release",
		"RHEL/9/x86_64/stable/repodata/repomd.xml",
		"pool/riscv64/main/thing_1.0_riscv64.deb",
		"pool/amd64/unstable/thing_1.0_amd64.deb",
		"pool/amd64/main/nested/thing_1.0_amd64.deb",
		"pool/amd64/thing_1.0_amd64.deb",
		"RHEL/8/x86_64/stable/thing-1.0-1.x86_64.rpm",
		"RHEL/9/x86_64/nightly/thing-1.0-1.x86_64.rpm",
		"Fedora/42/x86_64/stable/thing-1.0-1.x86_64.rpm",
		"repos.yaml",
		"",
	}

	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			_, _, err := c.Classify(key)
			if !errors.Is(err, ErrNotIndexed) {
				t.Fatalf("Classify(%q) err = %v, want ErrNotIndexed", key, err)
			}
		})
	}
}

func TestClassifyToleratesLeadingSlash(t *testing.T) {
	c := mustParse(t)
	scope, _, err := c.Classify("/pool/arm64/test/thing_1.0_arm64.deb")
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if got, want := scope.String(), "deb/test/arm64"; got != want {
		t.Errorf("scope = %q, want %q", got, want)
	}
}

func TestScopeRoundTrip(t *testing.T) {
	// Publish messages carry scopes as strings, so the round trip has to hold.
	scopes := []Scope{
		DebPoolScope("main", "amd64"),
		DebReleaseScope("noble"),
		RPMTreeScope("RHEL", "9", "x86_64", "stable"),
	}
	for _, s := range scopes {
		t.Run(s.String(), func(t *testing.T) {
			got, err := ParseScope(s.String())
			if err != nil {
				t.Fatalf("ParseScope(%q): %v", s.String(), err)
			}
			if got != s {
				t.Errorf("round trip = %+v, want %+v", got, s)
			}
		})
	}
}

func TestParseScopeRejectsMalformed(t *testing.T) {
	for _, s := range []string{
		"", "deb", "deb/main", "deb/main/amd64/extra",
		"release", "release/noble/extra",
		"rpm/RHEL/9/x86_64", "rpm/RHEL/9/x86_64/stable/extra",
		"nonsense/a/b",
	} {
		t.Run(s, func(t *testing.T) {
			if _, err := ParseScope(s); err == nil {
				t.Fatalf("ParseScope(%q) should fail", s)
			}
		})
	}
}

func TestPublishScopes(t *testing.T) {
	c := mustParse(t)

	var got []string
	for _, s := range c.PublishScopes() {
		got = append(got, s.String())
	}
	slices.Sort(got)

	want := []string{
		"deb/main/amd64", "deb/main/arm64",
		"deb/test/amd64", "deb/test/arm64",
		"release/noble",
		"rpm/RHEL/9/aarch64/stable", "rpm/RHEL/9/aarch64/test",
		"rpm/RHEL/9/x86_64/stable", "rpm/RHEL/9/x86_64/test",
	}
	if !slices.Equal(got, want) {
		t.Errorf("PublishScopes() =\n %v\nwant\n %v", got, want)
	}
}

func TestKnows(t *testing.T) {
	c := mustParse(t)

	known := []Scope{
		DebPoolScope("main", "amd64"),
		DebReleaseScope("noble"),
		RPMTreeScope("RHEL", "9", "aarch64", "test"),
	}
	for _, s := range known {
		if !c.Knows(s) {
			t.Errorf("Knows(%s) = false, want true", s)
		}
	}

	// A scope removed from the config stops being published. It must not be
	// reported as known, and nothing deletes its published tree implicitly.
	unknown := []Scope{
		DebPoolScope("main", "riscv64"),
		DebReleaseScope("jammy"),
		RPMTreeScope("RHEL", "8", "x86_64", "stable"),
		RPMTreeScope("RHEL", "9", "x86_64", "nightly"),
		{},
		{Kind: KindDebPool},
	}
	for _, s := range unknown {
		if c.Knows(s) {
			t.Errorf("Knows(%s) = true, want false", s)
		}
	}
}

func TestReleaseFanOut(t *testing.T) {
	c, err := Parse([]byte(strings.Replace(testYAML, "codenames: [noble]", "codenames: [jammy, noble, bookworm]", 1)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// One pool scope feeds every codename's Release, which is what makes a
	// single uploaded .deb appear in all of them.
	got := c.ReleaseScopesFor(DebPoolScope("main", "amd64"))
	if len(got) != 3 {
		t.Fatalf("got %d release scopes, want 3: %v", len(got), got)
	}
	for i, codename := range []string{"jammy", "noble", "bookworm"} {
		if got[i].Codename != codename {
			t.Errorf("release[%d] = %q, want %q", i, got[i].Codename, codename)
		}
	}

	// And a Release covers every component and architecture.
	pools := c.PoolScopesFor(DebReleaseScope("noble"))
	if len(pools) != 4 {
		t.Errorf("got %d pool scopes, want 4 (2 components x 2 arches): %v", len(pools), pools)
	}

	if got := c.ReleaseScopesFor(DebPoolScope("main", "riscv64")); got != nil {
		t.Errorf("an unconfigured pool scope should fan out to nothing, got %v", got)
	}
	if got := c.PoolScopesFor(DebReleaseScope("trixie")); got != nil {
		t.Errorf("an unconfigured codename should map to no pools, got %v", got)
	}
}

func TestPathHelpers(t *testing.T) {
	c := mustParse(t)
	pool := DebPoolScope("main", "amd64")

	if got, want := c.IndexDir("noble", pool), "dists/noble/main/binary-amd64"; got != want {
		t.Errorf("IndexDir = %q, want %q", got, want)
	}
	if got, want := c.IndexDirRelativeToCodename(pool), "main/binary-amd64"; got != want {
		t.Errorf("IndexDirRelativeToCodename = %q, want %q", got, want)
	}
	if got, want := c.CodenameDir("noble"), "dists/noble"; got != want {
		t.Errorf("CodenameDir = %q, want %q", got, want)
	}
	if got, want := c.TreeDir(RPMTreeScope("RHEL", "9", "x86_64", "stable")), "RHEL/9/x86_64/stable"; got != want {
		t.Errorf("TreeDir = %q, want %q", got, want)
	}
}
