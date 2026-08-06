package rpmmd

import (
	"slices"

	"github.com/ashleyconnor/linux-repo-indexer/internal/pkgmeta"
)

// BuildPrimary renders primary.xml, the file dnf reads to resolve a package
// name or a versioned dependency to a downloadable object.
func BuildPrimary(pkgs []pkgmeta.Package) ([]byte, error) {
	sorted := sortPackages(pkgs)

	w := &writer{}
	w.decl()
	w.open(0, "metadata",
		at("xmlns", nsCommon),
		at("xmlns:rpm", nsRPM),
		atNum("packages", len(sorted)),
	)

	for i := range sorted {
		p := &sorted[i]
		if err := validate(p); err != nil {
			return nil, err
		}
		writePrimaryPackage(w, p)
	}

	w.close(0, "metadata")
	return w.document(), nil
}

func writePrimaryPackage(w *writer, p *pkgmeta.Package) {
	d := p.RPM
	digest, algorithm := p.PkgID()

	w.open(0, "package", at("type", "rpm"))
	w.text(1, "name", p.Name)
	w.text(1, "arch", p.Architecture)
	w.empty(1, "version", versionAttrs(p)...)
	w.text(1, "checksum", digest, at("type", algorithm), at("pkgid", "YES"))
	w.text(1, "summary", p.Summary)
	w.text(1, "description", p.Description)
	// createrepo_c emits these even when the package sets neither.
	w.text(1, "packager", d.Packager)
	w.text(1, "url", d.URL)
	w.empty(1, "time", atNum("file", d.FileTime), atNum("build", d.BuildTime))
	w.empty(1, "size",
		atNum("package", p.Size),
		atNum("installed", d.InstalledSize),
		atNum("archive", d.ArchiveSize),
	)
	w.empty(1, "location", at("href", p.Filename))

	w.open(1, "format")
	w.text(2, "rpm:license", d.License)
	w.text(2, "rpm:vendor", d.Vendor)
	w.text(2, "rpm:group", d.Group)
	w.text(2, "rpm:buildhost", d.BuildHost)
	w.text(2, "rpm:sourcerpm", d.SourceRPM)
	w.empty(2, "rpm:header-range", atNum("start", d.HeaderStart), atNum("end", d.HeaderEnd))

	writeDependencySection(w, "rpm:provides", d.Provides)
	writeDependencySection(w, "rpm:requires", dedupeRequires(d.Requires))
	writeDependencySection(w, "rpm:conflicts", d.Conflicts)
	writeDependencySection(w, "rpm:obsoletes", d.Obsoletes)
	writeDependencySection(w, "rpm:recommends", d.Recommends)
	writeDependencySection(w, "rpm:suggests", d.Suggests)

	// Only the files a client might resolve a file dependency against; the
	// complete list lives in filelists.xml.
	for _, f := range sortedFiles(d.PrimaryFiles()) {
		writeFileElement(w, 2, f)
	}

	w.close(1, "format")
	w.close(0, "package")
}

// writeDependencySection emits one relationship block, or nothing at all when
// there is nothing to say. createrepo_c omits empty sections rather than
// writing an empty element.
func writeDependencySection(w *writer, name string, deps []pkgmeta.Dependency) {
	if len(deps) == 0 {
		return
	}
	w.open(2, name)
	for _, dep := range deps {
		w.empty(3, "rpm:entry", entryAttrs(dep)...)
	}
	w.close(2, name)
}

// entryAttrs renders one <rpm:entry>. Version attributes appear only on a
// versioned dependency, and rel only when the dependency names a release.
func entryAttrs(d pkgmeta.Dependency) []attr {
	as := []attr{at("name", d.Name)}

	comparison := d.Comparison()
	if comparison == "" {
		if d.IsPre() {
			as = append(as, at("pre", "1"))
		}
		return as
	}

	as = append(as,
		at("flags", comparison),
		atNum("epoch", d.Epoch),
		at("ver", d.Version),
		atOpt("rel", d.Release),
	)
	if d.IsPre() {
		as = append(as, at("pre", "1"))
	}
	return as
}

// dedupeRequires drops the entries createrepo_c leaves out of primary.xml:
// rpmlib() feature requirements, which no package provides, and duplicates,
// which rpm records once per scriptlet that needs them.
func dedupeRequires(deps []pkgmeta.Dependency) []pkgmeta.Dependency {
	if len(deps) == 0 {
		return nil
	}
	out := make([]pkgmeta.Dependency, 0, len(deps))
	seen := make(map[pkgmeta.Dependency]bool, len(deps))
	for _, d := range deps {
		if d.IsRPMLib() || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return slices.Clip(out)
}
