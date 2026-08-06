// Package rpmmd generates yum/dnf repository metadata: primary.xml,
// filelists.xml, other.xml and the repomd.xml that ties them together.
//
// Output is byte-compatible with createrepo_c, which is what the golden tests
// assert. Unlike the Debian side there is no fan-out: a yum tree is
// self-contained, so one storage scope maps to exactly one published tree.
package rpmmd

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/ashleyconnor/linux-repo-indexer/internal/pkgmeta"
)

// XML namespaces, as createrepo_c writes them.
const (
	nsCommon    = "http://linux.duke.edu/metadata/common"
	nsFilelists = "http://linux.duke.edu/metadata/filelists"
	nsOther     = "http://linux.duke.edu/metadata/other"
	nsRepo      = "http://linux.duke.edu/metadata/repo"
	nsRPM       = "http://linux.duke.edu/metadata/rpm"
)

// sortPackages orders records so that identical input always produces
// identical output. dnf joins the three metadata files by pkgid rather than by
// position, so the order is ours to choose; it only has to be stable.
func sortPackages(pkgs []pkgmeta.Package) []pkgmeta.Package {
	sorted := slices.Clone(pkgs)
	slices.SortFunc(sorted, func(a, b pkgmeta.Package) int {
		if c := cmp.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		if c := cmp.Compare(a.EVR(), b.EVR()); c != 0 {
			return c
		}
		return cmp.Compare(a.Architecture, b.Architecture)
	})
	return sorted
}

// validate rejects records that cannot produce usable metadata. A package with
// no checksum would be indexed but unverifiable, which dnf reports as a
// corrupt repository rather than a missing package.
func validate(p *pkgmeta.Package) error {
	if p.Format != pkgmeta.FormatRPM {
		return fmt.Errorf("rpmmd: %s is a %s package", p.Name, p.Format)
	}
	if p.RPM == nil {
		return fmt.Errorf("rpmmd: %s has no rpm detail", p.Name)
	}
	if p.Filename == "" {
		return fmt.Errorf("rpmmd: %s has no filename", p.Name)
	}
	if digest, _ := p.PkgID(); digest == "" {
		return fmt.Errorf("rpmmd: %s has no checksum", p.Name)
	}
	return nil
}

// versionAttrs renders the shared <version epoch= ver= rel=/> element.
func versionAttrs(p *pkgmeta.Package) []attr {
	return []attr{
		atNum("epoch", p.Epoch),
		at("ver", p.Version),
		at("rel", p.Release),
	}
}

// sortedFiles returns a package's files ordered by path.
func sortedFiles(files []pkgmeta.File) []pkgmeta.File {
	out := slices.Clone(files)
	slices.SortFunc(out, func(a, b pkgmeta.File) int { return strings.Compare(a.Path, b.Path) })
	return out
}

// writeFileElement emits <file>, <file type="dir"> or <file type="ghost">.
func writeFileElement(w *writer, depth int, f pkgmeta.File) {
	if f.Type == pkgmeta.FileTypeFile {
		w.text(depth, "file", f.Path)
		return
	}
	w.text(depth, "file", f.Path, at("type", string(f.Type)))
}
