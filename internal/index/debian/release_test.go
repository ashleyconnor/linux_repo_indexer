package debian

import (
	"strings"
	"testing"
	"time"

	"github.com/ashleyconnor/linux-repo-indexer/internal/debcontrol"
	"github.com/ashleyconnor/linux-repo-indexer/internal/index"
)

func testReleaseConfig() ReleaseConfig {
	return ReleaseConfig{
		Origin:        "HashiCorp",
		Label:         "HashiCorp",
		Suite:         "noble",
		Codename:      "noble",
		Components:    []string{"main", "test"},
		Architectures: []string{"amd64", "arm64"},
		AcquireByHash: true,
		Date:          time.Date(2026, time.August, 5, 18, 18, 45, 0, time.UTC),
	}
}

func testFiles() []IndexFile {
	return []IndexFile{
		{Path: "main/binary-amd64/Packages.gz", Digests: index.Digests{Size: 304336, MD5: "827d5ffabbd4d3eff40bb2052049fae9", SHA1: "aaaa", SHA256: "bbbb"}},
		{Path: "main/binary-amd64/Packages", Digests: index.Digests{Size: 1912372, MD5: "e2694e352ad6c36a4abd142aae2d60ea", SHA1: "cccc", SHA256: "dddd"}},
	}
}

func TestBuildReleaseHeader(t *testing.T) {
	body, err := BuildRelease(testReleaseConfig(), testFiles())
	if err != nil {
		t.Fatalf("BuildRelease: %v", err)
	}

	// A Release file is a single control paragraph, so it must parse as one.
	p, err := debcontrol.Parse(string(body))
	if err != nil {
		t.Fatalf("generated Release is not a valid control paragraph: %v\n%s", err, body)
	}

	for _, tt := range []struct{ field, want string }{
		{"Origin", "HashiCorp"},
		{"Label", "HashiCorp"},
		{"Suite", "noble"},
		{"Codename", "noble"},
		{"Date", "Wed, 05 Aug 2026 18:18:45 UTC"},
		{"Acquire-By-Hash", "yes"},
		{"Components", "main test"},
		{"Architectures", "amd64 arm64"},
	} {
		if got := p.Get(tt.field); got != tt.want {
			t.Errorf("%s = %q, want %q", tt.field, got, tt.want)
		}
	}
}

func TestBuildReleaseChecksumSections(t *testing.T) {
	body, err := BuildRelease(testReleaseConfig(), testFiles())
	if err != nil {
		t.Fatalf("BuildRelease: %v", err)
	}
	got := string(body)

	// All three sections, in apt-ftparchive's order.
	md5At := strings.Index(got, "\nMD5Sum:\n")
	sha1At := strings.Index(got, "\nSHA1:\n")
	sha256At := strings.Index(got, "\nSHA256:\n")
	if md5At < 0 || sha1At < 0 || sha256At < 0 {
		t.Fatalf("missing a checksum section:\n%s", got)
	}
	if !(md5At < sha1At && sha1At < sha256At) {
		t.Errorf("checksum sections are out of order: MD5Sum@%d SHA1@%d SHA256@%d", md5At, sha1At, sha256At)
	}

	// Sizes are right-aligned in a 16-character column, as apt-ftparchive
	// writes them.
	const want = " e2694e352ad6c36a4abd142aae2d60ea          1912372 main/binary-amd64/Packages\n"
	if !strings.Contains(got, want) {
		t.Errorf("missing correctly formatted checksum line %q\n%s", want, got)
	}
}

func TestBuildReleaseSortsFiles(t *testing.T) {
	body, err := BuildRelease(testReleaseConfig(), testFiles())
	if err != nil {
		t.Fatalf("BuildRelease: %v", err)
	}
	got := string(body)

	plain := strings.Index(got, "main/binary-amd64/Packages\n")
	gz := strings.Index(got, "main/binary-amd64/Packages.gz\n")
	if plain < 0 || gz < 0 {
		t.Fatalf("both entries should be listed:\n%s", got)
	}
	if plain > gz {
		t.Error("entries are not sorted by path, so output is not reproducible")
	}
}

func TestBuildReleaseOptionalFields(t *testing.T) {
	cfg := testReleaseConfig()
	cfg.Origin, cfg.Label = "", ""
	cfg.AcquireByHash = false
	cfg.Suite = "" // must fall back to the codename

	body, err := BuildRelease(cfg, testFiles())
	if err != nil {
		t.Fatalf("BuildRelease: %v", err)
	}
	p, err := debcontrol.Parse(string(body))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}

	if p.Index("Origin") >= 0 || p.Index("Label") >= 0 {
		t.Error("empty Origin/Label should be omitted rather than emitted blank")
	}
	if p.Index("Acquire-By-Hash") >= 0 {
		t.Error("Acquire-By-Hash should be omitted when disabled")
	}
	if got, want := p.Get("Suite"), "noble"; got != want {
		t.Errorf("Suite = %q, want it to default to the codename %q", got, want)
	}
	if p.Index("Valid-Until") >= 0 {
		t.Error("Valid-Until should be omitted when zero; an expired Release breaks every client")
	}
}

func TestBuildReleaseValidUntil(t *testing.T) {
	cfg := testReleaseConfig()
	cfg.ValidUntil = cfg.Date.Add(7 * 24 * time.Hour)

	body, err := BuildRelease(cfg, testFiles())
	if err != nil {
		t.Fatalf("BuildRelease: %v", err)
	}
	p, _ := debcontrol.Parse(string(body))
	if got, want := p.Get("Valid-Until"), "Wed, 12 Aug 2026 18:18:45 UTC"; got != want {
		t.Errorf("Valid-Until = %q, want %q", got, want)
	}
}

func TestBuildReleaseRejectsIncompleteConfig(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ReleaseConfig)
	}{
		{"no codename", func(c *ReleaseConfig) { c.Codename = "" }},
		{"no components", func(c *ReleaseConfig) { c.Components = nil }},
		{"no architectures", func(c *ReleaseConfig) { c.Architectures = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testReleaseConfig()
			tt.mutate(&cfg)
			if _, err := BuildRelease(cfg, testFiles()); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestBuildReleaseRejectsMissingDigest(t *testing.T) {
	// A file recorded without a digest would produce a Release that apt reads
	// as corrupt, so it must fail loudly at generation time instead.
	files := testFiles()
	files[0].Digests.SHA256 = ""

	if _, err := BuildRelease(testReleaseConfig(), files); err == nil {
		t.Fatal("expected an error for a file with no SHA256")
	} else if !strings.Contains(err.Error(), "SHA256") {
		t.Errorf("error = %v, want it to name the missing algorithm", err)
	}
}

func TestBuildReleaseIsDeterministic(t *testing.T) {
	cfg := testReleaseConfig()
	first, err := BuildRelease(cfg, testFiles())
	if err != nil {
		t.Fatalf("BuildRelease: %v", err)
	}
	// Same inputs in a different order must still yield identical bytes.
	shuffled := []IndexFile{testFiles()[1], testFiles()[0]}
	second, err := BuildRelease(cfg, shuffled)
	if err != nil {
		t.Fatalf("BuildRelease: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("Release is not reproducible:\n%s\n---\n%s", first, second)
	}
}
