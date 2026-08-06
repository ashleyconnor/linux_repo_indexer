package seed

import (
	"encoding/xml"
	"fmt"
	"io"
	"time"

	"github.com/ashleyconnor/linux-repo-indexer/internal/pkgmeta"
)

// The subset of the repodata schema a record needs. Parsing with encoding/xml
// is fine here even though generating with it is not: reading does not care
// about self-closing tags or attribute order.

type primaryMetadata struct {
	Packages []primaryPackage `xml:"package"`
}

type primaryPackage struct {
	Name     string      `xml:"name"`
	Arch     string      `xml:"arch"`
	Version  xmlEVR      `xml:"version"`
	Checksum xmlChecksum `xml:"checksum"`

	Summary     string `xml:"summary"`
	Description string `xml:"description"`
	Packager    string `xml:"packager"`
	URL         string `xml:"url"`

	Time     xmlTime     `xml:"time"`
	Size     xmlSize     `xml:"size"`
	Location xmlLocation `xml:"location"`

	Format primaryFormat `xml:"format"`
}

type primaryFormat struct {
	License     string     `xml:"license"`
	Vendor      string     `xml:"vendor"`
	Group       string     `xml:"group"`
	BuildHost   string     `xml:"buildhost"`
	SourceRPM   string     `xml:"sourcerpm"`
	HeaderRange xmlHeader  `xml:"header-range"`
	Provides    []xmlEntry `xml:"provides>entry"`
	Requires    []xmlEntry `xml:"requires>entry"`
	Conflicts   []xmlEntry `xml:"conflicts>entry"`
	Obsoletes   []xmlEntry `xml:"obsoletes>entry"`
	Recommends  []xmlEntry `xml:"recommends>entry"`
	Suggests    []xmlEntry `xml:"suggests>entry"`
}

type xmlEVR struct {
	Epoch int    `xml:"epoch,attr"`
	Ver   string `xml:"ver,attr"`
	Rel   string `xml:"rel,attr"`
}

type xmlChecksum struct {
	Type  string `xml:"type,attr"`
	Value string `xml:",chardata"`
}

type xmlTime struct {
	File  int64 `xml:"file,attr"`
	Build int64 `xml:"build,attr"`
}

type xmlSize struct {
	Package   int64 `xml:"package,attr"`
	Installed int64 `xml:"installed,attr"`
	Archive   int64 `xml:"archive,attr"`
}

type xmlLocation struct {
	Href string `xml:"href,attr"`
}

type xmlHeader struct {
	Start int `xml:"start,attr"`
	End   int `xml:"end,attr"`
}

type xmlEntry struct {
	Name  string `xml:"name,attr"`
	Flags string `xml:"flags,attr"`
	Epoch int    `xml:"epoch,attr"`
	Ver   string `xml:"ver,attr"`
	Rel   string `xml:"rel,attr"`
	Pre   string `xml:"pre,attr"`
}

type filelistsMetadata struct {
	Packages []filelistsPackage `xml:"package"`
}

type filelistsPackage struct {
	PkgID string    `xml:"pkgid,attr"`
	Files []xmlFile `xml:"file"`
}

type xmlFile struct {
	Type string `xml:"type,attr"`
	Path string `xml:",chardata"`
}

type otherMetadata struct {
	Packages []otherPackage `xml:"package"`
}

type otherPackage struct {
	PkgID     string         `xml:"pkgid,attr"`
	Changelog []xmlChangelog `xml:"changelog"`
}

type xmlChangelog struct {
	Author string `xml:"author,attr"`
	Date   int64  `xml:"date,attr"`
	Text   string `xml:",chardata"`
}

// ParseRepodata reconstructs records from a yum tree's metadata.
//
// The three files are joined on pkgid, which is what dnf itself does; primary
// alone lacks the complete file list and the changelog. filelists and other
// may be nil when a repository does not publish them.
//
// treePrefix is the tree's location in the bucket, used to turn each package's
// tree-relative href into an object key.
func ParseRepodata(primary, filelists, other io.Reader, treePrefix string, now time.Time) ([]pkgmeta.Package, error) {
	var meta primaryMetadata
	if err := xml.NewDecoder(primary).Decode(&meta); err != nil {
		return nil, fmt.Errorf("seed: reading primary.xml: %w", err)
	}

	files, err := decodeFilelists(filelists)
	if err != nil {
		return nil, err
	}
	changelogs, err := decodeOther(other)
	if err != nil {
		return nil, err
	}

	out := make([]pkgmeta.Package, 0, len(meta.Packages))
	for i := range meta.Packages {
		p := &meta.Packages[i]
		if p.Location.Href == "" {
			return nil, fmt.Errorf("seed: %s has no location", p.Name)
		}

		pkg := pkgmeta.Package{
			Format:       pkgmeta.FormatRPM,
			Name:         p.Name,
			Epoch:        p.Version.Epoch,
			Version:      p.Version.Ver,
			Release:      p.Version.Rel,
			Architecture: p.Arch,
			S3Key:        joinKey(treePrefix, p.Location.Href),
			Filename:     p.Location.Href,
			Size:         p.Size.Package,
			Summary:      p.Summary,
			Description:  p.Description,
			UpdatedAt:    now,
			RPM: &pkgmeta.RPMDetail{
				License:       p.Format.License,
				Vendor:        p.Format.Vendor,
				Packager:      p.Packager,
				Group:         p.Format.Group,
				URL:           p.URL,
				SourceRPM:     p.Format.SourceRPM,
				BuildHost:     p.Format.BuildHost,
				BuildTime:     p.Time.Build,
				FileTime:      p.Time.File,
				InstalledSize: p.Size.Installed,
				ArchiveSize:   p.Size.Archive,
				HeaderStart:   p.Format.HeaderRange.Start,
				HeaderEnd:     p.Format.HeaderRange.End,
				Provides:      convertEntries(p.Format.Provides),
				Requires:      convertEntries(p.Format.Requires),
				Conflicts:     convertEntries(p.Format.Conflicts),
				Obsoletes:     convertEntries(p.Format.Obsoletes),
				Recommends:    convertEntries(p.Format.Recommends),
				Suggests:      convertEntries(p.Format.Suggests),
				Files:         files[p.Checksum.Value],
				Changelog:     changelogs[p.Checksum.Value],
			},
		}

		// The existing index identifies packages by a SHA1 pkgid. Recomputing
		// a SHA256 would mean downloading every package, so the SHA1 is kept
		// and republished as-is; each <checksum> declares its own type, and
		// dnf verifies whichever it is given.
		switch p.Checksum.Type {
		case "sha256":
			pkg.SHA256 = p.Checksum.Value
		case "sha", "sha1":
			pkg.SHA1 = p.Checksum.Value
		default:
			return nil, fmt.Errorf("seed: %s has an unsupported checksum type %q", p.Name, p.Checksum.Type)
		}

		out = append(out, pkg)
	}
	return out, nil
}

func decodeFilelists(r io.Reader) (map[string][]pkgmeta.File, error) {
	out := map[string][]pkgmeta.File{}
	if r == nil {
		return out, nil
	}

	var meta filelistsMetadata
	if err := xml.NewDecoder(r).Decode(&meta); err != nil {
		return nil, fmt.Errorf("seed: reading filelists.xml: %w", err)
	}
	for _, p := range meta.Packages {
		files := make([]pkgmeta.File, 0, len(p.Files))
		for _, f := range p.Files {
			files = append(files, pkgmeta.File{Path: f.Path, Type: pkgmeta.FileType(f.Type)})
		}
		out[p.PkgID] = files
	}
	return out, nil
}

func decodeOther(r io.Reader) (map[string][]pkgmeta.ChangelogEntry, error) {
	out := map[string][]pkgmeta.ChangelogEntry{}
	if r == nil {
		return out, nil
	}

	var meta otherMetadata
	if err := xml.NewDecoder(r).Decode(&meta); err != nil {
		return nil, fmt.Errorf("seed: reading other.xml: %w", err)
	}
	for _, p := range meta.Packages {
		entries := make([]pkgmeta.ChangelogEntry, 0, len(p.Changelog))
		for _, c := range p.Changelog {
			entries = append(entries, pkgmeta.ChangelogEntry{
				Author: c.Author,
				Date:   c.Date,
				Text:   c.Text,
			})
		}
		out[p.PkgID] = entries
	}
	return out, nil
}

// convertEntries turns primary.xml dependency entries back into the model's
// sense flags, which is the inverse of what the generator does.
func convertEntries(entries []xmlEntry) []pkgmeta.Dependency {
	if len(entries) == 0 {
		return nil
	}
	out := make([]pkgmeta.Dependency, 0, len(entries))
	for _, e := range entries {
		d := pkgmeta.Dependency{
			Name:    e.Name,
			Epoch:   e.Epoch,
			Version: e.Ver,
			Release: e.Rel,
			Flags:   flagsFor(e.Flags),
		}
		if e.Pre == "1" {
			d.Flags |= pkgmeta.SensePreReq
		}
		out = append(out, d)
	}
	return out
}

func flagsFor(comparison string) int {
	switch comparison {
	case "LT":
		return pkgmeta.SenseLess
	case "LE":
		return pkgmeta.SenseLess | pkgmeta.SenseEqual
	case "GT":
		return pkgmeta.SenseGreater
	case "GE":
		return pkgmeta.SenseGreater | pkgmeta.SenseEqual
	case "EQ":
		return pkgmeta.SenseEqual
	default:
		return pkgmeta.SenseAny
	}
}

func joinKey(prefix, href string) string {
	if prefix == "" {
		return href
	}
	return prefix + "/" + href
}
