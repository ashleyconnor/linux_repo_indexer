package seed

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ashleyconnor/linux-repo-indexer/internal/index"
	"github.com/ashleyconnor/linux-repo-indexer/internal/index/debian"
	"github.com/ashleyconnor/linux-repo-indexer/internal/index/rpmmd"
	"github.com/ashleyconnor/linux-repo-indexer/internal/pkgmeta"
	"github.com/ashleyconnor/linux-repo-indexer/internal/pkgmeta/deb"
	rpmparse "github.com/ashleyconnor/linux-repo-indexer/internal/pkgmeta/rpm"
)

var seedTime = time.Date(2026, time.August, 6, 0, 0, 0, 0, time.UTC)

func loadDebFixtures(t *testing.T, names ...string) []pkgmeta.Package {
	t.Helper()
	var out []pkgmeta.Package
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join("../../testdata/packages/deb", name))
		if err != nil {
			t.Fatalf("reading fixture: %v (run ./testdata/generate.sh)", err)
		}
		p, err := deb.ParseBytes(raw)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		d := index.DigestsOf(raw)
		p.S3Key = "pool/" + p.Architecture + "/main/" + name
		p.Filename = p.S3Key
		p.Size, p.MD5, p.SHA1, p.SHA256 = d.Size, d.MD5, d.SHA1, d.SHA256
		p.UpdatedAt = seedTime
		out = append(out, *p)
	}
	return out
}

func loadRPMFixtures(t *testing.T, names ...string) []pkgmeta.Package {
	t.Helper()
	var out []pkgmeta.Package
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join("../../testdata/packages/rpm", name))
		if err != nil {
			t.Fatalf("reading fixture: %v (run ./testdata/generate.sh)", err)
		}
		p, err := rpmparse.ParseBytes(raw)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		d := index.DigestsOf(raw)
		p.S3Key = "RHEL/9/x86_64/stable/" + name
		p.Filename = name
		p.Size, p.MD5, p.SHA1, p.SHA256 = d.Size, d.MD5, d.SHA1, d.SHA256
		p.UpdatedAt = seedTime
		p.RPM.FileTime = seedTime.Unix()
		out = append(out, *p)
	}
	return out
}

// TestDebianSeedRoundTrip is the property that makes seeding safe: an index
// parsed back into records and regenerated must be byte-identical. If it is,
// the cutover cannot change what clients see.
func TestDebianSeedRoundTrip(t *testing.T) {
	original := loadDebFixtures(t,
		"indexer-fixture_1.0.0-1_amd64.deb",
		"indexer-fixture_1.1.0-1_amd64.deb",
		"indexer-other_2.5.0-1_amd64.deb",
	)

	first, err := debian.RenderPackages(original)
	if err != nil {
		t.Fatalf("RenderPackages: %v", err)
	}

	seeded, err := ParsePackages(bytes.NewReader(first), seedTime)
	if err != nil {
		t.Fatalf("ParsePackages: %v", err)
	}
	if len(seeded) != len(original) {
		t.Fatalf("seeded %d packages, want %d", len(seeded), len(original))
	}

	second, err := debian.RenderPackages(seeded)
	if err != nil {
		t.Fatalf("regenerating: %v", err)
	}

	if !bytes.Equal(first, second) {
		t.Errorf("regenerated index differs from the one it was seeded from:\n%s\n---\n%s", first, second)
	}
}

func TestDebianSeedRecoversFields(t *testing.T) {
	original := loadDebFixtures(t, "indexer-fixture_1.0.0-1_amd64.deb")

	body, err := debian.RenderPackages(original)
	if err != nil {
		t.Fatalf("RenderPackages: %v", err)
	}
	seeded, err := ParsePackages(bytes.NewReader(body), seedTime)
	if err != nil {
		t.Fatalf("ParsePackages: %v", err)
	}

	got, want := seeded[0], original[0]
	for _, tt := range []struct{ field, got, want string }{
		{"Name", got.Name, want.Name},
		{"Version", got.Version, want.Version},
		{"Architecture", got.Architecture, want.Architecture},
		{"Filename", got.Filename, want.Filename},
		{"SHA256", got.SHA256, want.SHA256},
		{"SHA1", got.SHA1, want.SHA1},
		{"MD5", got.MD5, want.MD5},
		{"Summary", got.Summary, want.Summary},
		{"Description", got.Description, want.Description},
	} {
		if tt.got != tt.want {
			t.Errorf("%s = %q, want %q", tt.field, tt.got, tt.want)
		}
	}
	if got.Size != want.Size {
		t.Errorf("Size = %d, want %d", got.Size, want.Size)
	}

	// The location fields must not survive into the stored control paragraph,
	// or regeneration would leave them mid-paragraph instead of at the end.
	for _, f := range []string{"Filename", "Size", "SHA256"} {
		if got.Deb.Control.Index(f) >= 0 {
			t.Errorf("%s should be stripped from the stored control paragraph", f)
		}
	}
}

// TestDebianSeedFromAptFtparchive proves the seeder copes with an index
// written by other tooling, which is what the existing repository is.
func TestDebianSeedFromAptFtparchive(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/golden/apt/Packages.amd64")
	if err != nil {
		t.Fatalf("reading golden: %v (run ./testdata/generate-golden.sh)", err)
	}

	seeded, err := ParsePackages(bytes.NewReader(raw), seedTime)
	if err != nil {
		t.Fatalf("ParsePackages: %v", err)
	}
	if len(seeded) != 3 {
		t.Fatalf("seeded %d packages, want 3", len(seeded))
	}

	for _, p := range seeded {
		if p.SHA256 == "" || p.Size == 0 || p.Filename == "" {
			t.Errorf("%s is missing location data: %+v", p.Name, p)
		}
		// apt-ftparchive emits SHA512, which we do not; it must be stripped
		// rather than carried into the regenerated index.
		if p.Deb.Control.Index("SHA512") >= 0 {
			t.Errorf("%s kept SHA512, which the generator does not emit", p.Name)
		}
	}
}

func TestDebianSeedRejectsIncompleteEntry(t *testing.T) {
	for _, tt := range []struct{ name, body string }{
		{"no filename", "Package: foo\nVersion: 1\nArchitecture: amd64\nSize: 1\nSHA256: abc\n"},
		{"no size", "Package: foo\nVersion: 1\nArchitecture: amd64\nFilename: p/foo.deb\nSHA256: abc\n"},
		{"no version", "Package: foo\nArchitecture: amd64\nFilename: p/foo.deb\nSize: 1\nSHA256: abc\n"},
		{"no checksum", "Package: foo\nVersion: 1\nArchitecture: amd64\nFilename: p/foo.deb\nSize: 1\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParsePackages(strings.NewReader(tt.body), seedTime); err == nil {
				t.Fatal("expected an error rather than a half-populated record")
			}
		})
	}
}

// TestRPMSeedRoundTrip is the same property for yum metadata, and needs all
// three files: primary alone has neither the complete file list nor the
// changelog, so a seeder that read only primary would silently produce a
// repository missing file-dependency resolution.
func TestRPMSeedRoundTrip(t *testing.T) {
	original := loadRPMFixtures(t,
		"indexer-fixture-1.0.0-1.x86_64.rpm",
		"indexer-fixture-1.1.0-1.x86_64.rpm",
		"indexer-other-2.5.0-1.x86_64.rpm",
	)

	primary, err := rpmmd.BuildPrimary(original)
	if err != nil {
		t.Fatalf("BuildPrimary: %v", err)
	}
	filelists, err := rpmmd.BuildFilelists(original)
	if err != nil {
		t.Fatalf("BuildFilelists: %v", err)
	}
	other, err := rpmmd.BuildOther(original)
	if err != nil {
		t.Fatalf("BuildOther: %v", err)
	}

	seeded, err := ParseRepodata(
		bytes.NewReader(primary), bytes.NewReader(filelists), bytes.NewReader(other),
		"RHEL/9/x86_64/stable", seedTime)
	if err != nil {
		t.Fatalf("ParseRepodata: %v", err)
	}
	if len(seeded) != len(original) {
		t.Fatalf("seeded %d packages, want %d", len(seeded), len(original))
	}

	for _, tt := range []struct {
		name  string
		build func([]pkgmeta.Package) ([]byte, error)
		want  []byte
	}{
		{"primary.xml", rpmmd.BuildPrimary, primary},
		{"filelists.xml", rpmmd.BuildFilelists, filelists},
		{"other.xml", rpmmd.BuildOther, other},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.build(seeded)
			if err != nil {
				t.Fatalf("regenerating: %v", err)
			}
			if !bytes.Equal(got, tt.want) {
				t.Errorf("regenerated %s differs from the one it was seeded from:\n%s\n---\n%s",
					tt.name, got, tt.want)
			}
		})
	}
}

func TestRPMSeedRecoversDetail(t *testing.T) {
	original := loadRPMFixtures(t, "indexer-other-2.5.0-1.x86_64.rpm")

	primary, _ := rpmmd.BuildPrimary(original)
	filelists, _ := rpmmd.BuildFilelists(original)
	other, _ := rpmmd.BuildOther(original)

	seeded, err := ParseRepodata(
		bytes.NewReader(primary), bytes.NewReader(filelists), bytes.NewReader(other),
		"RHEL/9/x86_64/stable", seedTime)
	if err != nil {
		t.Fatalf("ParseRepodata: %v", err)
	}

	got, want := seeded[0], original[0]
	if got.Epoch != want.Epoch {
		t.Errorf("Epoch = %d, want %d (it appears only in the metadata, never the filename)", got.Epoch, want.Epoch)
	}
	if got.S3Key != want.S3Key {
		t.Errorf("S3Key = %q, want %q", got.S3Key, want.S3Key)
	}
	if got.RPM.HeaderStart != want.RPM.HeaderStart || got.RPM.HeaderEnd != want.RPM.HeaderEnd {
		t.Errorf("header range = %d-%d, want %d-%d",
			got.RPM.HeaderStart, got.RPM.HeaderEnd, want.RPM.HeaderStart, want.RPM.HeaderEnd)
	}
	if len(got.RPM.Files) != len(want.RPM.Files) {
		t.Errorf("got %d files, want %d", len(got.RPM.Files), len(want.RPM.Files))
	}
	if len(got.RPM.Changelog) != len(want.RPM.Changelog) {
		t.Errorf("got %d changelog entries, want %d", len(got.RPM.Changelog), len(want.RPM.Changelog))
	}
}

// TestRPMSeedKeepsSHA1PkgID covers the known gap: the live index identifies
// packages by a SHA1 pkgid, and recomputing a SHA256 would mean downloading
// every package. Keeping the SHA1 is legal, because each checksum element
// declares its own type.
func TestRPMSeedKeepsSHA1PkgID(t *testing.T) {
	const primary = `<?xml version="1.0" encoding="UTF-8"?>
<metadata xmlns="http://linux.duke.edu/metadata/common" xmlns:rpm="http://linux.duke.edu/metadata/rpm" packages="1">
<package type="rpm">
  <name>consul</name>
  <arch>x86_64</arch>
  <version epoch="0" ver="1.21.0" rel="1"/>
  <checksum type="sha" pkgid="YES">547b70bdf2f11eedea2eef02e2264dd3970667e5</checksum>
  <summary>consul</summary>
  <description>consul</description>
  <packager>HashiCorp</packager>
  <url>https://consul.io</url>
  <time file="1772269362" build="1772203419"/>
  <size package="67018194" installed="181092652" archive="181092652"/>
  <location href="consul-1.21.0-1.x86_64.rpm"/>
  <format>
    <rpm:license>MPL-2.0</rpm:license>
    <rpm:vendor>HashiCorp</rpm:vendor>
    <rpm:sourcerpm>consul-1.21.0-1.src.rpm</rpm:sourcerpm>
    <rpm:header-range start="848" end="2408"/>
    <rpm:requires>
      <rpm:entry name="openssl"/>
    </rpm:requires>
  </format>
</package>
</metadata>`

	seeded, err := ParseRepodata(strings.NewReader(primary), nil, nil, "RHEL/9/x86_64/stable", seedTime)
	if err != nil {
		t.Fatalf("ParseRepodata: %v", err)
	}
	if len(seeded) != 1 {
		t.Fatalf("seeded %d packages, want 1", len(seeded))
	}

	p := seeded[0]
	if p.SHA256 != "" {
		t.Errorf("SHA256 = %q, want empty; it cannot be known without downloading the package", p.SHA256)
	}
	digest, algorithm := p.PkgID()
	if algorithm != "sha1" || digest != "547b70bdf2f11eedea2eef02e2264dd3970667e5" {
		t.Errorf("PkgID() = %q/%q, want the SHA1 from the index", digest, algorithm)
	}

	// And it must still generate usable metadata.
	out, err := rpmmd.BuildPrimary(seeded)
	if err != nil {
		t.Fatalf("BuildPrimary: %v", err)
	}
	if !bytes.Contains(out, []byte(`<checksum type="sha1" pkgid="YES">547b70bdf2f11eedea2eef02e2264dd3970667e5</checksum>`)) {
		t.Errorf("regenerated primary.xml does not carry the seeded checksum:\n%s", out)
	}
}
