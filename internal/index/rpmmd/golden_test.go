package rpmmd

import (
	"bytes"
	"compress/gzip"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ashleyconnor/linux-repo-indexer/internal/index"
	"github.com/ashleyconnor/linux-repo-indexer/internal/pkgmeta"
	rpmparse "github.com/ashleyconnor/linux-repo-indexer/internal/pkgmeta/rpm"
)

const (
	packageDir = "../../../testdata/packages/rpm"
	goldenDir  = "../../../testdata/golden/rpm"
)

// loadTree parses every fixture for one architecture and fills in the location
// fields that ingest would supply, laid out as the live yum trees are: RPMs
// flat at the tree root with repodata alongside.
func loadTree(t *testing.T, arch string) []pkgmeta.Package {
	t.Helper()

	entries, err := os.ReadDir(packageDir)
	if err != nil {
		t.Fatalf("reading fixtures: %v (run ./testdata/generate.sh)", err)
	}

	var out []pkgmeta.Package
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, "."+arch+".rpm") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(packageDir, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		p, err := rpmparse.ParseBytes(raw)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		d := index.DigestsOf(raw)
		p.S3Key = "RHEL/9/" + arch + "/stable/" + name
		p.Filename = name // <location href> is relative to the tree root
		p.Size = d.Size
		p.MD5, p.SHA1, p.SHA256 = d.MD5, d.SHA1, d.SHA256
		out = append(out, *p)
	}

	if len(out) == 0 {
		t.Fatalf("no %s fixtures found in %s", arch, packageDir)
	}
	return out
}

// readGolden returns the decompressed golden file whose name ends in suffix.
func readGolden(t *testing.T, arch, suffix string) []byte {
	t.Helper()
	dir := filepath.Join(goldenDir, arch, "repodata")
	matches, err := filepath.Glob(filepath.Join(dir, "*"+suffix))
	if err != nil || len(matches) == 0 {
		t.Fatalf("no golden matching *%s in %s (run ./testdata/generate-golden.sh)", suffix, dir)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("reading golden: %v", err)
	}
	return b
}

// fileTimeRE extracts the mtime createrepo_c recorded for each package. That
// value is the .rpm's timestamp on the machine that ran createrepo_c, which we
// cannot know, so the test replays it into our records rather than normalising
// it out of both sides.
var fileTimeRE = regexp.MustCompile(`<time file="(\d+)" build="\d+"/>`)

// withGoldenFileTimes copies the <time file=...> values out of the golden
// primary.xml, matching packages by position in our sorted order.
func withGoldenFileTimes(t *testing.T, pkgs []pkgmeta.Package, arch string) []pkgmeta.Package {
	t.Helper()

	golden := string(readGolden(t, arch, "-primary.xml"))
	matches := fileTimeRE.FindAllStringSubmatch(golden, -1)

	sorted := sortPackages(pkgs)
	if len(matches) != len(sorted) {
		t.Fatalf("golden has %d packages, fixtures have %d", len(matches), len(sorted))
	}
	for i := range sorted {
		v, err := strconv.ParseInt(matches[i][1], 10, 64)
		if err != nil {
			t.Fatalf("parsing golden file time: %v", err)
		}
		sorted[i].RPM.FileTime = v
	}
	return sorted
}

func TestPrimaryMatchesCreaterepoC(t *testing.T) {
	for _, arch := range []string{"x86_64", "aarch64"} {
		t.Run(arch, func(t *testing.T) {
			pkgs := withGoldenFileTimes(t, loadTree(t, arch), arch)

			got, err := BuildPrimary(pkgs)
			if err != nil {
				t.Fatalf("BuildPrimary: %v", err)
			}
			assertEqualXML(t, got, readGolden(t, arch, "-primary.xml"))
		})
	}
}

func TestFilelistsMatchesCreaterepoC(t *testing.T) {
	for _, arch := range []string{"x86_64", "aarch64"} {
		t.Run(arch, func(t *testing.T) {
			got, err := BuildFilelists(loadTree(t, arch))
			if err != nil {
				t.Fatalf("BuildFilelists: %v", err)
			}
			assertEqualXML(t, got, readGolden(t, arch, "-filelists.xml"))
		})
	}
}

func TestOtherMatchesCreaterepoC(t *testing.T) {
	for _, arch := range []string{"x86_64", "aarch64"} {
		t.Run(arch, func(t *testing.T) {
			got, err := BuildOther(loadTree(t, arch))
			if err != nil {
				t.Fatalf("BuildOther: %v", err)
			}
			assertEqualXML(t, got, readGolden(t, arch, "-other.xml"))
		})
	}
}

// repomd mirrors repomd.xml for comparison.
type repomd struct {
	Revision string        `xml:"revision"`
	Data     []repomdEntry `xml:"data"`
}

type repomdEntry struct {
	Type         string `xml:"type,attr"`
	Checksum     string `xml:"checksum"`
	OpenChecksum string `xml:"open-checksum"`
	Location     struct {
		Href string `xml:"href,attr"`
	} `xml:"location"`
	Timestamp string `xml:"timestamp"`
	Size      int    `xml:"size"`
	OpenSize  int    `xml:"open-size"`
}

func parseRepomd(t *testing.T, b []byte) repomd {
	t.Helper()
	var r repomd
	if err := xml.Unmarshal(b, &r); err != nil {
		t.Fatalf("parsing repomd.xml: %v\n%s", err, b)
	}
	return r
}

// TestRepodataMatchesCreaterepoC compares repomd.xml against createrepo_c's.
//
// A byte comparison is not possible here and would not mean anything if it
// were: the compressed checksum, the size and the filename derived from the
// checksum all depend on the gzip encoder, and Go's differs from zlib's even
// though both produce valid gzip. What must match is the uncompressed content,
// which the open-checksum and open-size assert exactly. The compressed side is
// then checked for self-consistency, which is what dnf actually verifies.
func TestRepodataMatchesCreaterepoC(t *testing.T) {
	const arch = "x86_64"
	pkgs := withGoldenFileTimes(t, loadTree(t, arch), arch)

	goldenBytes, err := os.ReadFile(filepath.Join(goldenDir, arch, "repodata", "repomd.xml"))
	if err != nil {
		t.Fatalf("reading golden repomd: %v", err)
	}
	golden := parseRepomd(t, goldenBytes)

	// Replay createrepo_c's revision so the timestamps line up.
	stamp, err := strconv.ParseInt(golden.Revision, 10, 64)
	if err != nil {
		t.Fatalf("parsing golden revision %q: %v", golden.Revision, err)
	}

	repodata, err := BuildRepodata(pkgs, time.Unix(stamp, 0))
	if err != nil {
		t.Fatalf("BuildRepodata: %v", err)
	}
	got := parseRepomd(t, repodata.RepomdXML)

	if got.Revision != golden.Revision {
		t.Errorf("revision = %q, want %q", got.Revision, golden.Revision)
	}
	if len(got.Data) != len(golden.Data) {
		t.Fatalf("got %d data entries, want %d", len(got.Data), len(golden.Data))
	}

	for i, want := range golden.Data {
		g := got.Data[i]
		if g.Type != want.Type {
			t.Fatalf("data[%d] type = %q, want %q (entry order must match)", i, g.Type, want.Type)
		}
		// The decisive assertion: identical uncompressed metadata.
		if g.OpenChecksum != want.OpenChecksum {
			t.Errorf("%s open-checksum = %s, want %s", g.Type, g.OpenChecksum, want.OpenChecksum)
		}
		if g.OpenSize != want.OpenSize {
			t.Errorf("%s open-size = %d, want %d", g.Type, g.OpenSize, want.OpenSize)
		}
		if g.Timestamp != want.Timestamp {
			t.Errorf("%s timestamp = %q, want %q", g.Type, g.Timestamp, want.Timestamp)
		}
		if g.Location.Href != "repodata/"+g.Checksum+"-"+g.Type+".xml.gz" {
			t.Errorf("%s location %q does not embed its own checksum", g.Type, g.Location.Href)
		}
	}
}

func TestRepodataIsSelfConsistent(t *testing.T) {
	const arch = "x86_64"
	pkgs := withGoldenFileTimes(t, loadTree(t, arch), arch)

	repodata, err := BuildRepodata(pkgs, time.Unix(1785988475, 0))
	if err != nil {
		t.Fatalf("BuildRepodata: %v", err)
	}

	// Three metadata files plus repomd.xml, which must be last so a publisher
	// writing in order never advertises metadata it has not yet uploaded.
	if got, want := len(repodata.Artifacts), 4; got != want {
		t.Fatalf("got %d artifacts, want %d", got, want)
	}
	last := repodata.Artifacts[len(repodata.Artifacts)-1]
	if got, want := last.Path, "repodata/repomd.xml"; got != want {
		t.Errorf("last artifact = %q, want %q", got, want)
	}

	byPath := make(map[string]index.Artifact, len(repodata.Artifacts))
	for _, a := range repodata.Artifacts {
		byPath[a.Path] = a
	}

	for _, entry := range parseRepomd(t, repodata.RepomdXML).Data {
		a, ok := byPath[entry.Location.Href]
		if !ok {
			t.Errorf("%s: repomd references %q, which is not published", entry.Type, entry.Location.Href)
			continue
		}
		if got := index.SHA256Hex(a.Body); got != entry.Checksum {
			t.Errorf("%s: published checksum %s does not match repomd's %s", entry.Type, got, entry.Checksum)
		}
		if len(a.Body) != entry.Size {
			t.Errorf("%s: published size %d does not match repomd's %d", entry.Type, len(a.Body), entry.Size)
		}

		zr, err := gzip.NewReader(bytes.NewReader(a.Body))
		if err != nil {
			t.Fatalf("%s is not gzip: %v", a.Path, err)
		}
		plain, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("%s: %v", a.Path, err)
		}
		if got := index.SHA256Hex(plain); got != entry.OpenChecksum {
			t.Errorf("%s: decompressed checksum %s does not match repomd's open-checksum %s", entry.Type, got, entry.OpenChecksum)
		}
		if len(plain) != entry.OpenSize {
			t.Errorf("%s: decompressed size %d does not match repomd's open-size %d", entry.Type, len(plain), entry.OpenSize)
		}
	}
}

// assertEqualXML compares byte for byte and reports the first differing line,
// which is far more useful than a diff of two multi-kilobyte documents.
func assertEqualXML(t *testing.T, got, want []byte) {
	t.Helper()
	if string(got) == string(want) {
		return
	}

	gotLines := strings.Split(string(got), "\n")
	wantLines := strings.Split(string(want), "\n")

	for i := 0; i < len(gotLines) && i < len(wantLines); i++ {
		if gotLines[i] != wantLines[i] {
			t.Fatalf("output differs from createrepo_c at line %d:\n got: %s\nwant: %s", i+1, gotLines[i], wantLines[i])
		}
	}
	t.Fatalf("output differs from createrepo_c in length: got %d lines, want %d lines\ngot tail:  %q\nwant tail: %q",
		len(gotLines), len(wantLines),
		lastLines(gotLines, 3), lastLines(wantLines, 3))
}

func lastLines(lines []string, n int) string {
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
