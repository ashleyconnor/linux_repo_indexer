// Package rpm extracts metadata from a .rpm file.
//
// An RPM's metadata lives entirely in its header, ahead of the compressed
// payload, so the payload is never decompressed — the parser stops as soon as
// the header ends.
package rpm

import (
	"bytes"
	"fmt"
	"io"

	crpm "github.com/cavaliergopher/rpm"

	"github.com/ashleyconnor/linux-repo-indexer/internal/pkgmeta"
)

// RPM header tags that the upstream library does not expose as methods.
//
// Package.ChangeLog reads tag 1017, a legacy field that modern rpmbuild leaves
// empty; the changelog actually lives in the three parallel arrays below.
const (
	tagGroup         = 1016
	tagChangelogTime = 1080
	tagChangelogName = 1081
	tagChangelogText = 1082
)

// Parse reads a .rpm from r and returns its metadata.
//
// Like the Debian parser, Parse leaves the payload unread so that callers can
// tee r through their hashers and drain the remainder, reading the object from
// S3 exactly once. Location and integrity fields are the caller's to fill in,
// with one exception noted on RPMDetail.FileTime.
func Parse(r io.Reader) (*pkgmeta.Package, error) {
	p, err := crpm.Read(r)
	if err != nil {
		return nil, fmt.Errorf("rpm: reading headers: %w", err)
	}

	name := p.Name()
	if name == "" {
		return nil, fmt.Errorf("rpm: package has no name")
	}
	version := p.Version()
	if version == "" {
		return nil, fmt.Errorf("rpm: %s has no version", name)
	}

	// Source RPMs have no architecture of their own and do not belong in a
	// binary repository's index.
	arch := p.Architecture()
	if arch == "" {
		return nil, fmt.Errorf("rpm: %s has no architecture", name)
	}

	start, end := p.HeaderRange()

	return &pkgmeta.Package{
		Format:       pkgmeta.FormatRPM,
		Name:         name,
		Epoch:        p.Epoch(),
		Version:      version,
		Release:      p.Release(),
		Architecture: arch,
		Summary:      p.Summary(),
		Description:  p.Description(),
		RPM: &pkgmeta.RPMDetail{
			License:   p.License(),
			Vendor:    p.Vendor(),
			Packager:  p.Packager(),
			Group:     p.Header.GetTag(tagGroup).String(),
			URL:       p.URL(),
			SourceRPM: p.SourceRPM(),
			BuildHost: p.BuildHost(),

			BuildTime: p.BuildTime().Unix(),

			InstalledSize: int64(p.Size()),
			ArchiveSize:   int64(p.ArchiveSize()),

			HeaderStart: start,
			HeaderEnd:   end,

			Provides:   convertDeps(p.Provides()),
			Requires:   convertDeps(p.Requires()),
			Conflicts:  convertDeps(p.Conflicts()),
			Obsoletes:  convertDeps(p.Obsoletes()),
			Recommends: convertDeps(p.Recommends()),
			Suggests:   convertDeps(p.Suggests()),

			Files:     convertFiles(p.Files()),
			Changelog: changelog(p),
		},
	}, nil
}

// ParseBytes is a convenience wrapper for callers that already hold the whole
// package in memory, such as tests and the seeder's verification mode.
func ParseBytes(b []byte) (*pkgmeta.Package, error) {
	return Parse(bytes.NewReader(b))
}

func convertDeps(deps []crpm.Dependency) []pkgmeta.Dependency {
	if len(deps) == 0 {
		return nil
	}
	out := make([]pkgmeta.Dependency, 0, len(deps))
	for _, d := range deps {
		out = append(out, pkgmeta.Dependency{
			Name:    d.Name(),
			Flags:   d.Flags(),
			Epoch:   d.Epoch(),
			Version: d.Version(),
			Release: d.Release(),
		})
	}
	return out
}

func convertFiles(files []crpm.FileInfo) []pkgmeta.File {
	if len(files) == 0 {
		return nil
	}
	out := make([]pkgmeta.File, 0, len(files))
	for i := range files {
		f := &files[i]
		out = append(out, pkgmeta.File{
			Path: f.Name(),
			Type: fileType(f),
		})
	}
	return out
}

// fileType mirrors createrepo_c's classification: directories are reported as
// such even when they are also marked %ghost, and everything else is either a
// ghost or a plain file.
func fileType(f *crpm.FileInfo) pkgmeta.FileType {
	switch {
	case f.IsDir():
		return pkgmeta.FileTypeDir
	case f.Flags()&crpm.FileFlagGhost != 0:
		return pkgmeta.FileTypeGhost
	default:
		return pkgmeta.FileTypeFile
	}
}

// changelog reads the three parallel changelog arrays. rpmbuild writes them in
// newest-first order, which is the order other.xml expects.
func changelog(p *crpm.Package) []pkgmeta.ChangelogEntry {
	times := p.Header.GetTag(tagChangelogTime).Int64Slice()
	names := p.Header.GetTag(tagChangelogName).StringSlice()
	texts := p.Header.GetTag(tagChangelogText).StringSlice()

	// A malformed header could leave the arrays out of step; take the shortest
	// rather than panicking on a package we do not control.
	n := min(len(times), min(len(names), len(texts)))
	if n == 0 {
		return nil
	}

	out := make([]pkgmeta.ChangelogEntry, 0, n)
	for i := range n {
		out = append(out, pkgmeta.ChangelogEntry{
			Author: names[i],
			Date:   times[i],
			Text:   texts[i],
		})
	}
	return out
}
