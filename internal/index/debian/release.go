package debian

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ashleyconnor/linux-repo-indexer/internal/debcontrol"
)

// ReleaseConfig describes one codename's Release file.
type ReleaseConfig struct {
	Origin      string
	Label       string
	Suite       string
	Codename    string
	Description string

	Components    []string
	Architectures []string

	// AcquireByHash tells apt it may fetch index files from their by-hash
	// paths. The live repository sets this, and it is what lets a publish
	// replace an index without breaking a client mid-refresh.
	AcquireByHash bool

	// Date is the publication timestamp. Callers pass it explicitly rather
	// than letting the generator read the clock, so output stays reproducible
	// and testable.
	Date time.Time

	// ValidUntil, when non-zero, tells apt to reject the release after this
	// instant. Leave it zero unless something guarantees regular republishing;
	// an expired Release breaks every client.
	ValidUntil time.Time
}

// releaseChecksumSections are emitted in this order, matching apt-ftparchive.
var releaseChecksumSections = []struct {
	field string
	pick  func(IndexFile) string
}{
	{"MD5Sum", func(f IndexFile) string { return f.Digests.MD5 }},
	{"SHA1", func(f IndexFile) string { return f.Digests.SHA1 }},
	{"SHA256", func(f IndexFile) string { return f.Digests.SHA256 }},
}

// BuildRelease renders the Release file for one codename.
//
// files lists every index file published under this codename, with paths
// relative to the codename directory, such as "main/binary-amd64/Packages.gz".
// The publisher supplies them from digests recorded when the Packages files
// were written, so building a Release never re-reads S3.
func BuildRelease(cfg ReleaseConfig, files []IndexFile) ([]byte, error) {
	if cfg.Codename == "" {
		return nil, fmt.Errorf("debian: release has no codename")
	}
	if len(cfg.Components) == 0 {
		return nil, fmt.Errorf("debian: release %s has no components", cfg.Codename)
	}
	if len(cfg.Architectures) == 0 {
		return nil, fmt.Errorf("debian: release %s has no architectures", cfg.Codename)
	}

	suite := cfg.Suite
	if suite == "" {
		suite = cfg.Codename
	}

	p := &debcontrol.Paragraph{}
	setIfNotEmpty(p, "Origin", cfg.Origin)
	setIfNotEmpty(p, "Label", cfg.Label)
	p.Set("Suite", suite)
	p.Set("Codename", cfg.Codename)
	p.Set("Date", cfg.Date.UTC().Format(releaseTimeFormat))
	if !cfg.ValidUntil.IsZero() {
		p.Set("Valid-Until", cfg.ValidUntil.UTC().Format(releaseTimeFormat))
	}
	if cfg.AcquireByHash {
		p.Set("Acquire-By-Hash", "yes")
	}
	p.Set("Components", strings.Join(cfg.Components, " "))
	p.Set("Architectures", strings.Join(cfg.Architectures, " "))
	setIfNotEmpty(p, "Description", cfg.Description)

	var buf bytes.Buffer
	// The header is written without debcontrol's paragraph terminator, because
	// the checksum sections belong to the same paragraph.
	for _, f := range p.Fields {
		fmt.Fprintf(&buf, "%s: %s\n", f.Name, f.Value)
	}

	sorted := slices.Clone(files)
	slices.SortFunc(sorted, func(a, b IndexFile) int { return strings.Compare(a.Path, b.Path) })

	for _, section := range releaseChecksumSections {
		fmt.Fprintf(&buf, "%s:\n", section.field)
		for _, f := range sorted {
			digest := section.pick(f)
			if digest == "" {
				return nil, fmt.Errorf("debian: %s has no %s digest", f.Path, section.field)
			}
			// apt-ftparchive right-aligns the size in a 16-character column.
			fmt.Fprintf(&buf, " %s %16d %s\n", digest, f.Digests.Size, f.Path)
		}
	}

	return buf.Bytes(), nil
}

// releaseTimeFormat is RFC 1123 with an explicit UTC label, which is what apt
// parses and what the live Release files carry.
const releaseTimeFormat = "Mon, 02 Jan 2006 15:04:05 UTC"

func setIfNotEmpty(p *debcontrol.Paragraph, name, value string) {
	if value != "" {
		p.Set(name, value)
	}
}
