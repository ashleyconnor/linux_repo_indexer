package rpm

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ashleyconnor/linux-repo-indexer/internal/pkgmeta"
)

const fixtureDir = "../../../testdata/packages/rpm"

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatalf("reading fixture: %v (run ./testdata/generate.sh)", err)
	}
	return b
}

func parseFixture(t *testing.T, name string) *pkgmeta.Package {
	t.Helper()
	p, err := ParseBytes(readFixture(t, name))
	if err != nil {
		t.Fatalf("ParseBytes(%s): %v", name, err)
	}
	return p
}

func TestParseFixture(t *testing.T) {
	pkg := parseFixture(t, "indexer-fixture-1.0.0-1.x86_64.rpm")

	if got, want := pkg.Format, pkgmeta.FormatRPM; got != want {
		t.Errorf("Format = %q, want %q", got, want)
	}
	if got, want := pkg.Name, "indexer-fixture"; got != want {
		t.Errorf("Name = %q, want %q", got, want)
	}
	if got, want := pkg.Version, "1.0.0"; got != want {
		t.Errorf("Version = %q, want %q", got, want)
	}
	if got, want := pkg.Release, "1"; got != want {
		t.Errorf("Release = %q, want %q", got, want)
	}
	if got, want := pkg.Epoch, 0; got != want {
		t.Errorf("Epoch = %d, want %d", got, want)
	}
	if got, want := pkg.Architecture, "x86_64"; got != want {
		t.Errorf("Architecture = %q, want %q", got, want)
	}
	if got, want := pkg.Summary, "indexer-fixture test fixture"; got != want {
		t.Errorf("Summary = %q, want %q", got, want)
	}
	if pkg.RPM == nil {
		t.Fatal("RPM detail is nil")
	}
	if got, want := pkg.RPM.License, "MPL-2.0"; got != want {
		t.Errorf("License = %q, want %q", got, want)
	}
	if got, want := pkg.RPM.Vendor, "Fixture"; got != want {
		t.Errorf("Vendor = %q, want %q", got, want)
	}
	if got, want := pkg.RPM.URL, "https://example.com/indexer-fixture"; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
	if got, want := pkg.RPM.SourceRPM, "indexer-fixture-1.0.0-1.src.rpm"; got != want {
		t.Errorf("SourceRPM = %q, want %q", got, want)
	}
	if pkg.RPM.BuildTime == 0 {
		t.Error("BuildTime = 0, want the package's build timestamp")
	}
	if pkg.RPM.InstalledSize == 0 {
		t.Error("InstalledSize = 0, want the installed byte count")
	}

	if pkg.S3Key != "" || pkg.Filename != "" || pkg.Size != 0 || pkg.SHA256 != "" {
		t.Errorf("parser must not populate location fields, got %+v", pkg)
	}
}

func TestParseEpoch(t *testing.T) {
	pkg := parseFixture(t, "indexer-other-2.5.0-1.x86_64.rpm")
	if got, want := pkg.Epoch, 2; got != want {
		t.Errorf("Epoch = %d, want %d", got, want)
	}
	if got, want := pkg.EVR(), "2:2.5.0-1"; got != want {
		t.Errorf("EVR() = %q, want %q", got, want)
	}
	if got, want := pkg.NEVRA(), "indexer-other-2:2.5.0-1.x86_64"; got != want {
		t.Errorf("NEVRA() = %q, want %q", got, want)
	}
}

func TestParseHeaderRange(t *testing.T) {
	raw := readFixture(t, "indexer-fixture-1.0.0-1.x86_64.rpm")
	pkg := parseFixture(t, "indexer-fixture-1.0.0-1.x86_64.rpm")

	start, end := pkg.RPM.HeaderStart, pkg.RPM.HeaderEnd
	if start <= 96 {
		t.Errorf("HeaderStart = %d, want it past the 96-byte lead", start)
	}
	if end <= start {
		t.Errorf("HeaderEnd = %d, want it greater than HeaderStart %d", end, start)
	}
	if end > len(raw) {
		t.Fatalf("HeaderEnd = %d exceeds the file size %d", end, len(raw))
	}
	// The main header begins with rpm's header magic, 8e ad e8 01.
	if got, want := raw[start:start+4], []byte{0x8e, 0xad, 0xe8, 0x01}; !bytes.Equal(got, want) {
		t.Errorf("bytes at HeaderStart = % x, want % x (header magic)", got, want)
	}
}

func TestParseDependencies(t *testing.T) {
	pkg := parseFixture(t, "indexer-fixture-1.1.0-1.x86_64.rpm")

	idx := slices.IndexFunc(pkg.RPM.Requires, func(d pkgmeta.Dependency) bool {
		return d.Name == "openssl"
	})
	if idx < 0 {
		t.Fatalf("openssl not found in Requires: %+v", pkg.RPM.Requires)
	}
	dep := pkg.RPM.Requires[idx]
	if got, want := dep.Comparison(), "GE"; got != want {
		t.Errorf("openssl comparison = %q, want %q", got, want)
	}
	if got, want := dep.Version, "3.0.0"; got != want {
		t.Errorf("openssl version = %q, want %q", got, want)
	}

	// The spec declares "Provides: indexer-fixture-virtual = %{version}".
	if !slices.ContainsFunc(pkg.RPM.Provides, func(d pkgmeta.Dependency) bool {
		return d.Name == "indexer-fixture-virtual" && d.Version == "1.1.0" && d.Comparison() == "EQ"
	}) {
		t.Errorf("virtual provide not found: %+v", pkg.RPM.Provides)
	}
}

func TestParseFiles(t *testing.T) {
	pkg := parseFixture(t, "indexer-fixture-1.0.0-1.x86_64.rpm")

	want := map[string]pkgmeta.FileType{
		"/usr/bin/indexer-fixture":                  pkgmeta.FileTypeFile,
		"/etc/indexer-fixture":                      pkgmeta.FileTypeDir,
		"/etc/indexer-fixture/indexer-fixture.conf": pkgmeta.FileTypeFile,
		"/usr/share/doc/indexer-fixture/README":     pkgmeta.FileTypeFile,
		"/var/log/indexer-fixture.log":              pkgmeta.FileTypeGhost,
	}

	got := make(map[string]pkgmeta.FileType, len(pkg.RPM.Files))
	for _, f := range pkg.RPM.Files {
		got[f.Path] = f.Type
	}

	for path, wantType := range want {
		gotType, ok := got[path]
		if !ok {
			t.Errorf("%s missing from file list %+v", path, pkg.RPM.Files)
			continue
		}
		if gotType != wantType {
			t.Errorf("%s type = %q, want %q", path, gotType, wantType)
		}
	}
}

func TestPrimaryFilesSubset(t *testing.T) {
	pkg := parseFixture(t, "indexer-fixture-1.0.0-1.x86_64.rpm")

	var paths []string
	for _, f := range pkg.RPM.PrimaryFiles() {
		paths = append(paths, f.Path)
	}
	slices.Sort(paths)

	want := []string{
		"/etc/indexer-fixture",
		"/etc/indexer-fixture/indexer-fixture.conf",
		"/usr/bin/indexer-fixture",
	}
	if !slices.Equal(paths, want) {
		t.Errorf("PrimaryFiles() = %v, want %v", paths, want)
	}
}

func TestParseChangelog(t *testing.T) {
	pkg := parseFixture(t, "indexer-fixture-1.0.0-1.x86_64.rpm")

	if len(pkg.RPM.Changelog) != 2 {
		t.Fatalf("got %d changelog entries, want 2: %+v", len(pkg.RPM.Changelog), pkg.RPM.Changelog)
	}
	first := pkg.RPM.Changelog[0]
	if got, want := first.Author, "Fixture Maintainer <fixtures@example.com> - 1.0.0-1"; got != want {
		t.Errorf("changelog author = %q, want %q", got, want)
	}
	if got, want := first.Text, "- Second changelog entry, so other.xml has more than one."; got != want {
		t.Errorf("changelog text = %q, want %q", got, want)
	}
	if first.Date == 0 {
		t.Error("changelog date = 0, want a timestamp")
	}
	// rpmbuild orders entries newest first, and other.xml preserves that.
	if pkg.RPM.Changelog[0].Date < pkg.RPM.Changelog[1].Date {
		t.Error("changelog entries are not newest-first")
	}
}

func TestParseArchVariants(t *testing.T) {
	for _, tt := range []struct{ file, arch string }{
		{"indexer-fixture-1.0.0-1.x86_64.rpm", "x86_64"},
		{"indexer-fixture-1.0.0-1.aarch64.rpm", "aarch64"},
		{"indexer-other-2.5.0-1.aarch64.rpm", "aarch64"},
	} {
		t.Run(tt.file, func(t *testing.T) {
			if got := parseFixture(t, tt.file).Architecture; got != tt.arch {
				t.Errorf("Architecture = %q, want %q", got, tt.arch)
			}
		})
	}
}

func TestParseStopsBeforeConsumingPayload(t *testing.T) {
	raw := readFixture(t, "indexer-fixture-1.0.0-1.x86_64.rpm")

	h := sha256.New()
	tee := io.TeeReader(bytes.NewReader(raw), h)

	if _, err := Parse(tee); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := io.Copy(io.Discard, tee); err != nil {
		t.Fatalf("draining remainder: %v", err)
	}

	want := sha256.Sum256(raw)
	if got := hex.EncodeToString(h.Sum(nil)); got != hex.EncodeToString(want[:]) {
		t.Errorf("teed hash = %s, want %s", got, hex.EncodeToString(want[:]))
	}
}

func TestParseRejectsNonRPM(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   []byte
	}{
		{"empty", nil},
		{"garbage", []byte("this is not an rpm package at all, not even close")},
		{"truncated lead", bytes.Repeat([]byte{0xed}, 16)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseBytes(tt.in); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}
