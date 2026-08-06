// Package pkgmeta defines the normalized package record that the whole system
// is built around.
//
// A .deb or .rpm is parsed exactly once, on upload, into a Package. Everything
// downstream — the DynamoDB rows, the Packages file, primary.xml — is derived
// from Package records, never from the package files themselves. That is what
// removes the "rescan every package" step that makes conventional repository
// tooling scale badly.
package pkgmeta

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/ashleyconnor/linux-repo-indexer/internal/debcontrol"
)

// Format identifies which packaging system a record came from.
type Format string

const (
	FormatDeb Format = "deb"
	FormatRPM Format = "rpm"
)

// A Package is one package file's metadata, normalized across formats.
//
// The common fields carry what both formats share and what the storage layer
// indexes on. Format-specific detail hangs off Deb or RPM, exactly one of
// which is non-nil, because the two formats' index files need genuinely
// different information and flattening them into one struct would mean a pile
// of fields that are meaningless half the time.
type Package struct {
	Format Format `json:"format"`

	// Identity. Epoch and Release are RPM-only; a Debian version string
	// carries its own epoch inline, as in "1:1.2.3-1".
	Name         string `json:"name"`
	Epoch        int    `json:"epoch,omitempty"`
	Version      string `json:"version"`
	Release      string `json:"release,omitempty"`
	Architecture string `json:"architecture"`

	// Location and integrity.
	S3Key    string `json:"s3Key"`    // full object key in the packages bucket
	Filename string `json:"filename"` // path recorded in the index, relative to the repo root
	Size     int64  `json:"size"`
	MD5      string `json:"md5,omitempty"`
	SHA1     string `json:"sha1,omitempty"`
	SHA256   string `json:"sha256,omitempty"`

	Summary     string `json:"summary,omitempty"`
	Description string `json:"description,omitempty"`

	Deb *DebDetail `json:"deb,omitempty"`
	RPM *RPMDetail `json:"rpm,omitempty"`

	UpdatedAt time.Time `json:"updatedAt"`
}

// DebDetail holds the package's control paragraph verbatim.
//
// The Packages index is, by design, the control paragraph plus a handful of
// location fields. Keeping the original rather than a parsed-out struct means
// fields we never modelled — and field ordering, which varies per package —
// survive into the generated index untouched.
type DebDetail struct {
	Control *debcontrol.Paragraph `json:"control"`
}

// RPMDetail holds what primary.xml, filelists.xml and other.xml need beyond
// the common fields.
type RPMDetail struct {
	License   string `json:"license,omitempty"`
	Vendor    string `json:"vendor,omitempty"`
	Packager  string `json:"packager,omitempty"`
	Group     string `json:"group,omitempty"`
	URL       string `json:"url,omitempty"`
	SourceRPM string `json:"sourceRpm,omitempty"`
	BuildHost string `json:"buildHost,omitempty"`

	BuildTime int64 `json:"buildTime,omitempty"` // unix seconds
	FileTime  int64 `json:"fileTime,omitempty"`  // unix seconds, mtime of the .rpm

	InstalledSize int64 `json:"installedSize,omitempty"`
	ArchiveSize   int64 `json:"archiveSize,omitempty"`

	// Byte range of the header blob within the .rpm, which primary.xml
	// reports so clients can fetch metadata with a ranged request.
	HeaderStart int `json:"headerStart,omitempty"`
	HeaderEnd   int `json:"headerEnd,omitempty"`

	Provides   []Dependency `json:"provides,omitempty"`
	Requires   []Dependency `json:"requires,omitempty"`
	Conflicts  []Dependency `json:"conflicts,omitempty"`
	Obsoletes  []Dependency `json:"obsoletes,omitempty"`
	Recommends []Dependency `json:"recommends,omitempty"`
	Suggests   []Dependency `json:"suggests,omitempty"`

	// Files is the complete file list, which feeds filelists.xml. It is the
	// bulk of a record — a few KB for a typical package, but megabytes for
	// something like a kernel — so the store may offload it to S3.
	Files []File `json:"files,omitempty"`

	Changelog []ChangelogEntry `json:"changelog,omitempty"`
}

// FileType distinguishes the three kinds of entry that repository metadata
// records. Plain files carry no type attribute at all.
type FileType string

const (
	FileTypeFile  FileType = ""
	FileTypeDir   FileType = "dir"
	FileTypeGhost FileType = "ghost"
)

// A File is one entry in a package's file list.
type File struct {
	Path string   `json:"path"`
	Type FileType `json:"type,omitempty"`
}

// A ChangelogEntry is one changelog record, as reported in other.xml.
type ChangelogEntry struct {
	Author string `json:"author"`
	Date   int64  `json:"date"` // unix seconds
	Text   string `json:"text"`
}

// RPM dependency sense flags, as stored in the RPMTAG_*FLAGS arrays.
const (
	SenseAny          = 0
	SenseLess         = 1 << 1
	SenseGreater      = 1 << 2
	SenseEqual        = 1 << 3
	SensePreReq       = 1 << 6
	SenseScriptPre    = 1 << 9
	SenseScriptPost   = 1 << 10
	SenseScriptPreUn  = 1 << 11
	SenseScriptPostUn = 1 << 12
	// RPMSENSE_RPMLIB marks dependencies on rpm's own features, which
	// createrepo excludes from primary.xml because no package provides them.
	SenseRPMLib = 1 << 24
	SenseConfig = 1 << 28
)

// A Dependency is one entry in a requires/provides/conflicts/obsoletes list.
type Dependency struct {
	Name    string `json:"name"`
	Flags   int    `json:"flags,omitempty"`
	Epoch   int    `json:"epoch,omitempty"`
	Version string `json:"version,omitempty"`
	Release string `json:"release,omitempty"`
}

// Comparison renders the sense flags as primary.xml's flags attribute, or ""
// when the dependency is unversioned.
func (d Dependency) Comparison() string {
	switch d.Flags & (SenseLess | SenseGreater | SenseEqual) {
	case SenseLess:
		return "LT"
	case SenseLess | SenseEqual:
		return "LE"
	case SenseGreater:
		return "GT"
	case SenseGreater | SenseEqual:
		return "GE"
	case SenseEqual:
		return "EQ"
	default:
		return ""
	}
}

// IsPre reports whether the dependency must be satisfied before installation
// or scriptlet execution, which primary.xml marks with pre="1".
func (d Dependency) IsPre() bool {
	const preMask = SensePreReq | SenseScriptPre | SenseScriptPost |
		SenseScriptPreUn | SenseScriptPostUn
	return d.Flags&preMask != 0
}

// IsRPMLib reports whether this is an rpmlib(...) feature dependency, which is
// excluded from primary.xml.
func (d Dependency) IsRPMLib() bool {
	return d.Flags&SenseRPMLib != 0 || strings.HasPrefix(d.Name, "rpmlib(")
}

// EVR renders the version in the form used for ordering and for the store's
// sort key. RPM records always include the epoch so that "1.0" and "1:1.0"
// cannot collide; Debian version strings already carry any epoch inline.
func (p *Package) EVR() string {
	if p.Format == FormatRPM {
		return fmt.Sprintf("%d:%s-%s", p.Epoch, p.Version, p.Release)
	}
	return p.Version
}

// Key is the record's identity within a scope, used as the DynamoDB sort key.
func (p *Package) Key() string {
	return p.Name + "#" + p.EVR() + "#" + p.Architecture
}

// PkgID returns the checksum that yum metadata identifies the package by,
// together with the algorithm name repodata spells it with.
//
// SHA256 is preferred. Records seeded from the existing index fall back to
// SHA1, because the live primary.xml carries only a SHA1 pkgid and recomputing
// a SHA256 would mean downloading every package — the exact cost the design
// exists to avoid. Mixing algorithms across packages is legal: each <checksum>
// element declares its own type, and dnf verifies whichever it is told.
func (p *Package) PkgID() (digest, algorithm string) {
	switch {
	case p.SHA256 != "":
		return p.SHA256, "sha256"
	case p.SHA1 != "":
		return p.SHA1, "sha1"
	default:
		return "", ""
	}
}

// NEVRA is the conventional human-readable name for an RPM.
func (p *Package) NEVRA() string {
	if p.Epoch != 0 {
		return fmt.Sprintf("%s-%d:%s-%s.%s", p.Name, p.Epoch, p.Version, p.Release, p.Architecture)
	}
	return fmt.Sprintf("%s-%s-%s.%s", p.Name, p.Version, p.Release, p.Architecture)
}

// String names the package the way its own ecosystem does, for logs and
// operator-facing output. Rendering a .deb as a NEVRA produces nonsense like
// "waypoint-0.9.1-.amd64", because Debian packages have no release field.
func (p *Package) String() string {
	if p.Format == FormatRPM {
		return p.NEVRA()
	}
	return fmt.Sprintf("%s_%s_%s", p.Name, p.Version, p.Architecture)
}

// primaryFilePattern mirrors createrepo_c's PRIMARY_FILES: primary.xml carries
// only the entries a client might resolve a file dependency against, and
// filelists.xml carries everything.
var primaryFilePattern = regexp.MustCompile(`^/etc/.*|^/usr/lib/sendmail$|.*bin/.*`)

// IsPrimaryFile reports whether path belongs in primary.xml as well as
// filelists.xml.
func IsPrimaryFile(path string) bool {
	return primaryFilePattern.MatchString(path)
}

// PrimaryFiles returns the subset of the package's files that primary.xml
// records.
func (d *RPMDetail) PrimaryFiles() []File {
	var out []File
	for _, f := range d.Files {
		if IsPrimaryFile(f.Path) {
			out = append(out, f)
		}
	}
	return out
}
