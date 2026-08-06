package deb

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ashleyconnor/linux-repo-indexer/internal/pkgmeta"
)

const fixtureDir = "../../../testdata/packages/deb"

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatalf("reading fixture: %v (run ./testdata/generate.sh)", err)
	}
	return b
}

func TestParseFixture(t *testing.T) {
	pkg, err := ParseBytes(readFixture(t, "indexer-fixture_1.0.0-1_amd64.deb"))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}

	if got, want := pkg.Format, pkgmeta.FormatDeb; got != want {
		t.Errorf("Format = %q, want %q", got, want)
	}
	if got, want := pkg.Name, "indexer-fixture"; got != want {
		t.Errorf("Name = %q, want %q", got, want)
	}
	if got, want := pkg.Version, "1.0.0-1"; got != want {
		t.Errorf("Version = %q, want %q", got, want)
	}
	if got, want := pkg.Architecture, "amd64"; got != want {
		t.Errorf("Architecture = %q, want %q", got, want)
	}
	if got, want := pkg.Summary, "indexer-fixture test fixture"; got != want {
		t.Errorf("Summary = %q, want %q", got, want)
	}
	if pkg.Deb == nil || pkg.Deb.Control == nil {
		t.Fatal("Deb.Control is nil")
	}
	if got, want := pkg.Deb.Control.Get("Maintainer"), "Fixture Maintainer <fixtures@example.com>"; got != want {
		t.Errorf("Maintainer = %q, want %q", got, want)
	}
	if got, want := pkg.Deb.Control.Get("Depends"), "openssl"; got != want {
		t.Errorf("Depends = %q, want %q", got, want)
	}

	// Location and integrity are the caller's responsibility, not the parser's.
	if pkg.S3Key != "" || pkg.Filename != "" || pkg.Size != 0 || pkg.SHA256 != "" {
		t.Errorf("parser must not populate location fields, got %+v", pkg)
	}
}

func TestParsePreservesControlFieldOrder(t *testing.T) {
	pkg, err := ParseBytes(readFixture(t, "indexer-fixture_1.0.0-1_amd64.deb"))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}

	// The fixture's control file deliberately lists License and Vendor between
	// Version and Architecture, which is not alphabetical and not the order
	// apt-ftparchive would choose.
	var names []string
	for _, f := range pkg.Deb.Control.Fields {
		names = append(names, f.Name)
	}
	got := strings.Join(names, ",")
	const want = "Package,Version,License,Vendor,Architecture,Maintainer,Installed-Size,Depends,Section,Priority,Homepage,Description"
	if got != want {
		t.Errorf("control field order =\n %s\nwant\n %s", got, want)
	}
}

func TestParseMultiLineDescription(t *testing.T) {
	pkg, err := ParseBytes(readFixture(t, "indexer-fixture_1.0.0-1_amd64.deb"))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}

	const wantBody = "This package exists only to exercise the repository indexer.\n" +
		".\n" +
		"It carries a multi-line description so that continuation-line\n" +
		"handling is covered:\n" +
		"  an indented line"
	if got := pkg.Description; got != wantBody {
		t.Errorf("Description body =\n%q\nwant\n%q", got, wantBody)
	}
}

func TestParseDropsInstalledStateFields(t *testing.T) {
	// Conffiles lives in control.tar but must not reach the Packages index.
	pkg, err := ParseBytes(readFixture(t, "indexer-fixture_1.0.0-1_amd64.deb"))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if got := pkg.Deb.Control.Get("Conffiles"); got != "" {
		t.Errorf("Conffiles = %q, want it dropped from the indexed paragraph", got)
	}
}

func TestParseCompressionVariants(t *testing.T) {
	// Every control.tar.* compression must yield identical metadata.
	base, err := ParseBytes(readFixture(t, "indexer-fixture_1.0.0-1_amd64.deb"))
	if err != nil {
		t.Fatalf("gzip fixture: %v", err)
	}

	for _, name := range []string{
		"indexer-fixture_1.0.0-1_amd64.xz.deb",
		"indexer-fixture_1.0.0-1_amd64.zst.deb",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ParseBytes(readFixture(t, name))
			if err != nil {
				t.Fatalf("ParseBytes: %v", err)
			}
			if got.Deb.Control.String() != base.Deb.Control.String() {
				t.Errorf("control differs from the gzip variant:\n%s", got.Deb.Control)
			}
		})
	}
}

func TestParseArchAndVersionVariants(t *testing.T) {
	tests := []struct{ file, version, arch, depends string }{
		{"indexer-fixture_1.0.0-1_amd64.deb", "1.0.0-1", "amd64", "openssl"},
		{"indexer-fixture_1.1.0-1_amd64.deb", "1.1.0-1", "amd64", "openssl (>= 3.0.0)"},
		{"indexer-fixture_1.0.0-1_arm64.deb", "1.0.0-1", "arm64", "openssl"},
		{"indexer-other_2.5.0-1_amd64.deb", "2.5.0-1", "amd64", "indexer-fixture (>= 1.0.0), passwd"},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			pkg, err := ParseBytes(readFixture(t, tt.file))
			if err != nil {
				t.Fatalf("ParseBytes: %v", err)
			}
			if pkg.Version != tt.version {
				t.Errorf("Version = %q, want %q", pkg.Version, tt.version)
			}
			if pkg.Architecture != tt.arch {
				t.Errorf("Architecture = %q, want %q", pkg.Architecture, tt.arch)
			}
			if got := pkg.Deb.Control.Get("Depends"); got != tt.depends {
				t.Errorf("Depends = %q, want %q", got, tt.depends)
			}
		})
	}
}

func TestParseStopsBeforeConsumingPayload(t *testing.T) {
	// Ingest tees the S3 body through hashers while the parser reads it, then
	// drains the rest. That only works if Parse leaves the payload unread and
	// the remainder can still be consumed to produce the whole object's hash.
	raw := readFixture(t, "indexer-fixture_1.0.0-1_amd64.deb")

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

func TestParseRejectsNonDeb(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
	}{
		{"empty", nil},
		{"too short for magic", []byte("!<arch")},
		{"wrong magic", []byte("not an ar archive at all")},
		{"gzip file", []byte{0x1f, 0x8b, 0x08, 0, 0, 0, 0, 0, 0, 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseBytes(tt.in); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestParseRejectsArchiveWithoutControl(t *testing.T) {
	// A valid ar archive whose only member is debian-binary.
	var buf bytes.Buffer
	buf.WriteString(arMagic)
	buf.WriteString("debian-binary   1700000000  0     0     100644  4         `\n")
	buf.WriteString("2.0\n")

	if _, err := ParseBytes(buf.Bytes()); err == nil {
		t.Fatal("expected an error for an archive with no control member")
	} else if !strings.Contains(err.Error(), "control.tar") {
		t.Errorf("error = %v, want it to mention the missing control member", err)
	}
}
