package debian

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ashleyconnor/linux-repo-indexer/internal/debcontrol"
	"github.com/ashleyconnor/linux-repo-indexer/internal/pkgmeta"
)

const goldenDir = "../../../testdata/golden/apt"

// fieldsWeDoNotEmit are the differences from apt-ftparchive that are
// deliberate rather than defects.
//
// SHA512 is omitted because records seeded from the existing index cannot have
// one: the live Packages file carries only SHA1 and SHA256, and computing a
// SHA512 would mean downloading all 3400 packages, which is exactly the cost
// this design exists to avoid. Emitting it for freshly ingested packages but
// not for seeded ones would be worse than omitting it consistently.
var fieldsWeDoNotEmit = []string{"SHA512"}

func readGoldenPackages(t *testing.T, arch string) map[string]*debcontrol.Paragraph {
	t.Helper()

	b, err := os.ReadFile(filepath.Join(goldenDir, "Packages."+arch))
	if err != nil {
		t.Fatalf("reading golden: %v (run ./testdata/generate-golden.sh)", err)
	}
	paras, err := debcontrol.ReadAll(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("parsing golden: %v", err)
	}

	out := make(map[string]*debcontrol.Paragraph, len(paras))
	for _, p := range paras {
		out[p.Get("Package")+"_"+p.Get("Version")+"_"+p.Get("Architecture")] = p
	}
	return out
}

// TestPackagesMatchesAptFtparchive compares our Packages entries with
// apt-ftparchive's, field by field.
//
// The comparison is deliberately not byte-for-byte. apt-ftparchive rewrites
// each paragraph into its own canonical field order and interleaves the
// location fields in the middle; the live Artifactory-generated index instead
// preserves each package's own control order and appends the location fields
// at the end. We match the live index, because reproducing it is what makes
// the cutover diff clean, and apt itself is indifferent to field order.
func TestPackagesMatchesAptFtparchive(t *testing.T) {
	tests := []struct {
		arch  string
		files []string
	}{
		{"amd64", []string{
			"indexer-fixture_1.0.0-1_amd64.deb",
			"indexer-fixture_1.1.0-1_amd64.deb",
			"indexer-other_2.5.0-1_amd64.deb",
		}},
		{"arm64", []string{"indexer-fixture_1.0.0-1_arm64.deb"}},
	}

	for _, tt := range tests {
		t.Run(tt.arch, func(t *testing.T) {
			golden := readGoldenPackages(t, tt.arch)

			body, err := RenderPackages(loadFixtures(t, tt.files...))
			if err != nil {
				t.Fatalf("RenderPackages: %v", err)
			}
			ours, err := debcontrol.ReadAll(bytes.NewReader(body))
			if err != nil {
				t.Fatalf("parsing our output: %v", err)
			}

			if len(ours) != len(golden) {
				t.Fatalf("got %d paragraphs, apt-ftparchive produced %d", len(ours), len(golden))
			}

			for _, got := range ours {
				key := got.Get("Package") + "_" + got.Get("Version") + "_" + got.Get("Architecture")
				want, ok := golden[key]
				if !ok {
					t.Errorf("%s is not in apt-ftparchive's output", key)
					continue
				}
				compareParagraphs(t, key, got, want)
			}
		})
	}
}

func compareParagraphs(t *testing.T, key string, got, want *debcontrol.Paragraph) {
	t.Helper()

	// Every field apt-ftparchive emits must be present with the same value,
	// except the ones we deliberately omit.
	for _, f := range want.Fields {
		if slices.Contains(fieldsWeDoNotEmit, f.Name) {
			continue
		}
		if got.Index(f.Name) < 0 {
			t.Errorf("%s: missing field %s that apt-ftparchive emits", key, f.Name)
			continue
		}
		if g := got.Get(f.Name); g != f.Value {
			t.Errorf("%s: %s =\n %q\nwant\n %q", key, f.Name, g, f.Value)
		}
	}

	// And we must not invent fields apt-ftparchive does not know about.
	for _, f := range got.Fields {
		if want.Index(f.Name) < 0 {
			t.Errorf("%s: we emit %s, which apt-ftparchive does not", key, f.Name)
		}
	}
}

// TestReleaseChecksumLineMatchesAptFtparchive checks our Release entry
// formatting against apt-ftparchive's, which apt's parser is tolerant of but
// which is worth matching so a diff against the live index stays readable.
func TestReleaseChecksumLineMatchesAptFtparchive(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(goldenDir, "Release"))
	if err != nil {
		t.Fatalf("reading golden Release: %v (run ./testdata/generate-golden.sh)", err)
	}
	golden, err := debcontrol.Parse(string(b))
	if err != nil {
		t.Fatalf("parsing golden Release: %v", err)
	}

	// Rebuild a Release from the golden's own SHA256 section, so the only
	// thing under test is how we format it.
	var files []IndexFile
	for _, line := range strings.Split(golden.Get("SHA256"), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		size, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			t.Fatalf("parsing golden size %q: %v", fields[1], err)
		}
		var f IndexFile
		f.Path = fields[2]
		f.Digests.Size = size
		f.Digests.SHA256 = fields[0]
		f.Digests.MD5, f.Digests.SHA1 = "unused", "unused"
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatal("golden Release has no SHA256 entries")
	}

	cfg := testReleaseConfig()
	cfg.Components = []string{"main"}
	body, err := BuildRelease(cfg, files)
	if err != nil {
		t.Fatalf("BuildRelease: %v", err)
	}

	ours, err := debcontrol.Parse(string(body))
	if err != nil {
		t.Fatalf("parsing our Release: %v", err)
	}
	if got, want := ours.Get("SHA256"), golden.Get("SHA256"); got != want {
		t.Errorf("SHA256 section differs:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestGoldenFixturesCoverBothFormats guards against a fixture set that has
// silently stopped exercising what these tests claim to cover.
func TestGoldenFixturesCoverBothFormats(t *testing.T) {
	pkgs := loadFixtures(t, "indexer-fixture_1.0.0-1_amd64.deb")
	if pkgs[0].Format != pkgmeta.FormatDeb {
		t.Fatal("fixture is not a deb package")
	}
	if !strings.Contains(pkgs[0].Description, "\n.\n") {
		t.Error("fixture no longer exercises blank-line continuation in Description")
	}
}
