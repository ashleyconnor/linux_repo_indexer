package debian

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ashleyconnor/linux-repo-indexer/internal/debcontrol"
	"github.com/ashleyconnor/linux-repo-indexer/internal/index"
	"github.com/ashleyconnor/linux-repo-indexer/internal/pkgmeta"
	"github.com/ashleyconnor/linux-repo-indexer/internal/pkgmeta/deb"
)

const fixtureDir = "../../../testdata/packages/deb"

// loadFixtures parses the committed .deb fixtures and fills in the location
// fields that ingest would normally supply.
func loadFixtures(t *testing.T, names ...string) []pkgmeta.Package {
	t.Helper()
	var out []pkgmeta.Package
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(fixtureDir, name))
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
		p.Size = d.Size
		p.MD5, p.SHA1, p.SHA256 = d.MD5, d.SHA1, d.SHA256
		out = append(out, *p)
	}
	return out
}

func TestRenderPackagesParagraphPerPackage(t *testing.T) {
	pkgs := loadFixtures(t,
		"indexer-fixture_1.0.0-1_amd64.deb",
		"indexer-fixture_1.1.0-1_amd64.deb",
		"indexer-other_2.5.0-1_amd64.deb",
	)

	body, err := RenderPackages(pkgs)
	if err != nil {
		t.Fatalf("RenderPackages: %v", err)
	}

	paras, err := debcontrol.ReadAll(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("re-parsing generated Packages: %v", err)
	}
	if len(paras) != 3 {
		t.Fatalf("got %d paragraphs, want 3", len(paras))
	}

	// Sorted by name then version, so the output is byte-stable.
	want := []string{"indexer-fixture", "indexer-fixture", "indexer-other"}
	wantVer := []string{"1.0.0-1", "1.1.0-1", "2.5.0-1"}
	for i, p := range paras {
		if got := p.Get("Package"); got != want[i] {
			t.Errorf("paragraph %d Package = %q, want %q", i, got, want[i])
		}
		if got := p.Get("Version"); got != wantVer[i] {
			t.Errorf("paragraph %d Version = %q, want %q", i, got, wantVer[i])
		}
	}
}

func TestRenderPackagesAppendsLocationFields(t *testing.T) {
	pkgs := loadFixtures(t, "indexer-fixture_1.0.0-1_amd64.deb")

	body, err := RenderPackages(pkgs)
	if err != nil {
		t.Fatalf("RenderPackages: %v", err)
	}
	para, err := debcontrol.Parse(string(body))
	if err != nil {
		t.Fatalf("re-parsing: %v", err)
	}

	p := pkgs[0]
	for _, tt := range []struct{ field, want string }{
		{"Filename", p.Filename},
		{"SHA256", p.SHA256},
		{"SHA1", p.SHA1},
		{"MD5sum", p.MD5},
	} {
		if got := para.Get(tt.field); got != tt.want {
			t.Errorf("%s = %q, want %q", tt.field, got, tt.want)
		}
	}
	if got, want := para.Get("Size"), "976"; got != want {
		t.Errorf("Size = %q, want %q (the object size, not Installed-Size)", got, want)
	}

	// Location fields go last, after everything the control file declared.
	names := make([]string, len(para.Fields))
	for i, f := range para.Fields {
		names[i] = f.Name
	}
	tail := names[len(names)-5:]
	if got, want := strings.Join(tail, ","), "Filename,Size,MD5sum,SHA1,SHA256"; got != want {
		t.Errorf("trailing fields = %q, want %q", got, want)
	}
}

func TestRenderPackagesPreservesDescriptionBody(t *testing.T) {
	pkgs := loadFixtures(t, "indexer-fixture_1.0.0-1_amd64.deb")
	body, err := RenderPackages(pkgs)
	if err != nil {
		t.Fatalf("RenderPackages: %v", err)
	}

	// The continuation lines must survive rendering with their single leading
	// space, including the "." that marks a blank line and the extra indent.
	for _, want := range []string{
		"Description: indexer-fixture test fixture\n",
		"\n This package exists only to exercise the repository indexer.\n",
		"\n .\n",
		"\n   an indented line\n",
	} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("generated Packages is missing %q\n---\n%s", want, body)
		}
	}
}

func TestRenderPackagesEndsWithBlankLine(t *testing.T) {
	// The live index ends with "\n\n"; apt tolerates either, but matching it
	// keeps a byte-level diff against the existing index clean.
	body, err := RenderPackages(loadFixtures(t, "indexer-fixture_1.0.0-1_amd64.deb"))
	if err != nil {
		t.Fatalf("RenderPackages: %v", err)
	}
	if !bytes.HasSuffix(body, []byte("\n\n")) {
		t.Errorf("Packages does not end with a blank line: %q", body[len(body)-10:])
	}
}

func TestRenderPackagesEmpty(t *testing.T) {
	body, err := RenderPackages(nil)
	if err != nil {
		t.Fatalf("RenderPackages(nil): %v", err)
	}
	if len(body) != 0 {
		t.Errorf("empty scope should render an empty Packages file, got %q", body)
	}
}

func TestRenderPackagesRejectsIncompleteRecords(t *testing.T) {
	valid := loadFixtures(t, "indexer-fixture_1.0.0-1_amd64.deb")[0]

	t.Run("no filename", func(t *testing.T) {
		p := valid
		p.Filename = ""
		if _, err := RenderPackages([]pkgmeta.Package{p}); err == nil {
			t.Fatal("expected an error for a record with no Filename")
		}
	})

	t.Run("rpm record", func(t *testing.T) {
		p := valid
		p.Format = pkgmeta.FormatRPM
		if _, err := RenderPackages([]pkgmeta.Package{p}); err == nil {
			t.Fatal("expected an error for an rpm record")
		}
	})
}

func TestBuildPackagesIndexVariants(t *testing.T) {
	pkgs := loadFixtures(t, "indexer-fixture_1.0.0-1_amd64.deb", "indexer-other_2.5.0-1_amd64.deb")

	idx, err := BuildPackagesIndex(pkgs, []index.Compression{index.CompressionGzip, index.CompressionXZ})
	if err != nil {
		t.Fatalf("BuildPackagesIndex: %v", err)
	}
	if len(idx.Variants) != 3 {
		t.Fatalf("got %d variants, want 3 (plain, gz, xz)", len(idx.Variants))
	}

	plain := idx.Variants[0]
	if plain.Compression != index.CompressionNone {
		t.Errorf("first variant = %q, want the uncompressed one", plain.Compression)
	}

	// Every compressed variant must decompress back to the plain body.
	for _, v := range idx.Variants[1:] {
		if v.Digests.Size != int64(len(v.Body)) {
			t.Errorf("%s: digest size %d != body length %d", v.Compression, v.Digests.Size, len(v.Body))
		}
		if v.Compression != index.CompressionGzip {
			continue
		}
		zr, err := gzip.NewReader(bytes.NewReader(v.Body))
		if err != nil {
			t.Fatalf("gzip variant is not readable: %v", err)
		}
		got, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("reading gzip variant: %v", err)
		}
		if !bytes.Equal(got, plain.Body) {
			t.Error("gzip variant does not round-trip to the plain body")
		}
	}
}

func TestBuildPackagesIndexIsDeterministic(t *testing.T) {
	// Repodata filenames embed their own checksum and S3 writes are skipped
	// when content is unchanged, so identical input must compress identically.
	pkgs := loadFixtures(t, "indexer-fixture_1.0.0-1_amd64.deb")

	first, err := BuildPackagesIndex(pkgs, []index.Compression{index.CompressionGzip, index.CompressionXZ})
	if err != nil {
		t.Fatalf("BuildPackagesIndex: %v", err)
	}
	second, err := BuildPackagesIndex(pkgs, []index.Compression{index.CompressionGzip, index.CompressionXZ})
	if err != nil {
		t.Fatalf("BuildPackagesIndex: %v", err)
	}

	for i := range first.Variants {
		if first.Variants[i].Digests.SHA256 != second.Variants[i].Digests.SHA256 {
			t.Errorf("%s variant is not reproducible", first.Variants[i].Compression)
		}
	}
}

func TestArtifactPathsAndByHash(t *testing.T) {
	pkgs := loadFixtures(t, "indexer-fixture_1.0.0-1_amd64.deb")
	idx, err := BuildPackagesIndex(pkgs, []index.Compression{index.CompressionGzip})
	if err != nil {
		t.Fatalf("BuildPackagesIndex: %v", err)
	}

	const dir = "dists/noble/main/binary-amd64"
	arts := idx.Artifacts(dir, true)

	paths := make([]string, len(arts))
	for i, a := range arts {
		paths[i] = a.Path
	}

	for _, want := range []string{
		dir + "/Packages",
		dir + "/Packages.gz",
		dir + "/by-hash/SHA256/" + idx.Variants[0].Digests.SHA256,
		dir + "/by-hash/SHA1/" + idx.Variants[0].Digests.SHA1,
		dir + "/by-hash/MD5Sum/" + idx.Variants[0].Digests.MD5,
		dir + "/by-hash/SHA256/" + idx.Variants[1].Digests.SHA256,
	} {
		if !slices.Contains(paths, want) {
			t.Errorf("missing artifact %q\ngot: %v", want, paths)
		}
	}

	// Two variants, each written once canonically and once per algorithm.
	if got, want := len(arts), 2*(1+3); got != want {
		t.Errorf("got %d artifacts, want %d", got, want)
	}
}

func TestArtifactsWithoutByHash(t *testing.T) {
	idx, err := BuildPackagesIndex(loadFixtures(t, "indexer-fixture_1.0.0-1_amd64.deb"), nil)
	if err != nil {
		t.Fatalf("BuildPackagesIndex: %v", err)
	}
	arts := idx.Artifacts("dists/noble/main/binary-amd64", false)
	if len(arts) != 1 {
		t.Fatalf("got %d artifacts, want 1", len(arts))
	}
	if strings.Contains(arts[0].Path, "by-hash") {
		t.Errorf("by-hash artifact emitted when disabled: %q", arts[0].Path)
	}
}

func TestFilesForRelease(t *testing.T) {
	idx, err := BuildPackagesIndex(
		loadFixtures(t, "indexer-fixture_1.0.0-1_amd64.deb"),
		[]index.Compression{index.CompressionGzip},
	)
	if err != nil {
		t.Fatalf("BuildPackagesIndex: %v", err)
	}

	files := idx.Files("main/binary-amd64")
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2", len(files))
	}
	// Paths are relative to the codename directory, not the repository root,
	// because that is how a Release file references them.
	if got, want := files[0].Path, "main/binary-amd64/Packages"; got != want {
		t.Errorf("files[0].Path = %q, want %q", got, want)
	}
	if got, want := files[1].Path, "main/binary-amd64/Packages.gz"; got != want {
		t.Errorf("files[1].Path = %q, want %q", got, want)
	}
	if files[0].Digests.SHA256 != idx.Variants[0].Digests.SHA256 {
		t.Error("Release digest does not match the published variant")
	}
}

func TestPlacingOneIndexIntoManyCodenames(t *testing.T) {
	// The pool is codename-independent: the live repository serves the same
	// package set to jammy, noble, bookworm and trixie. Rendering once and
	// placing many times must produce identical bodies at different paths.
	idx, err := BuildPackagesIndex(loadFixtures(t, "indexer-fixture_1.0.0-1_amd64.deb"), nil)
	if err != nil {
		t.Fatalf("BuildPackagesIndex: %v", err)
	}

	noble := idx.Artifacts("dists/noble/main/binary-amd64", false)
	jammy := idx.Artifacts("dists/jammy/main/binary-amd64", false)

	if noble[0].Path == jammy[0].Path {
		t.Fatal("codenames must produce distinct paths")
	}
	if !bytes.Equal(noble[0].Body, jammy[0].Body) {
		t.Error("the same index placed in two codenames must have identical bodies")
	}
}
