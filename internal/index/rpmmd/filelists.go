package rpmmd

import (
	"cmp"
	"slices"

	"github.com/ashleyconnor/linux-repo-indexer/internal/pkgmeta"
)

// BuildFilelists renders filelists.xml, the complete file list for every
// package.
//
// dnf needs it to resolve file dependencies such as "Requires: /usr/bin/env",
// which primary.xml deliberately cannot answer. It is also by far the largest
// metadata file — on the live repository it is 23 MB uncompressed for 3400
// packages — which is why the store may offload a package's file list to S3.
func BuildFilelists(pkgs []pkgmeta.Package) ([]byte, error) {
	sorted := sortPackages(pkgs)

	w := &writer{}
	w.decl()
	w.open(0, "filelists",
		at("xmlns", nsFilelists),
		atNum("packages", len(sorted)),
	)

	for i := range sorted {
		p := &sorted[i]
		if err := validate(p); err != nil {
			return nil, err
		}
		digest, _ := p.PkgID()

		w.open(0, "package",
			at("pkgid", digest),
			at("name", p.Name),
			at("arch", p.Architecture),
		)
		w.empty(1, "version", versionAttrs(p)...)
		for _, f := range sortedFiles(p.RPM.Files) {
			writeFileElement(w, 1, f)
		}
		w.close(0, "package")
	}

	w.close(0, "filelists")
	return w.document(), nil
}

// BuildOther renders other.xml, which carries changelogs.
//
// dnf works without it, but repomd.xml lists it and "dnf changelog" reads it,
// so it is published for parity with the existing repository.
func BuildOther(pkgs []pkgmeta.Package) ([]byte, error) {
	sorted := sortPackages(pkgs)

	w := &writer{}
	w.decl()
	w.open(0, "otherdata",
		at("xmlns", nsOther),
		atNum("packages", len(sorted)),
	)

	for i := range sorted {
		p := &sorted[i]
		if err := validate(p); err != nil {
			return nil, err
		}
		digest, _ := p.PkgID()

		w.open(0, "package",
			at("pkgid", digest),
			at("name", p.Name),
			at("arch", p.Architecture),
		)
		w.empty(1, "version", versionAttrs(p)...)
		for _, c := range sortedChangelog(p.RPM.Changelog) {
			w.text(1, "changelog", c.Text,
				at("author", c.Author),
				atNum("date", c.Date),
			)
		}
		w.close(0, "package")
	}

	w.close(0, "otherdata")
	return w.document(), nil
}

// sortedChangelog puts entries oldest first. rpm stores them newest first;
// createrepo_c reverses them, and matching that keeps our output comparable
// with the canonical tool.
func sortedChangelog(entries []pkgmeta.ChangelogEntry) []pkgmeta.ChangelogEntry {
	out := slices.Clone(entries)
	slices.SortStableFunc(out, func(a, b pkgmeta.ChangelogEntry) int {
		return cmp.Compare(a.Date, b.Date)
	})
	return out
}
