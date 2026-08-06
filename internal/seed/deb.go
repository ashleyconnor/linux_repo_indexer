// Package seed reconstructs package records from an existing published index.
//
// The repository being replaced already holds thousands of packages, and
// re-reading them all to populate the database would cost exactly the
// full-repository scan this design exists to avoid. The published index
// already contains everything a record needs, so it is parsed back instead.
package seed

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/ashleyconnor/linux-repo-indexer/internal/debcontrol"
	"github.com/ashleyconnor/linux-repo-indexer/internal/pkgmeta"
)

// locationFields are added by the index generator, so a seeded record must not
// keep them in its control paragraph. Leaving them would put them in the
// middle of the regenerated paragraph rather than at the end, where a freshly
// ingested package puts them.
var locationFields = []string{"Filename", "Size", "MD5sum", "SHA1", "SHA256", "SHA512", "Description-md5"}

// ParsePackages reconstructs records from a Packages file.
//
// Everything needed is present: the control fields are the package's own, and
// the location fields carry the size and checksums that would otherwise have
// to be recomputed by downloading every package.
func ParsePackages(r io.Reader, now time.Time) ([]pkgmeta.Package, error) {
	paragraphs, err := debcontrol.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("seed: reading Packages: %w", err)
	}

	out := make([]pkgmeta.Package, 0, len(paragraphs))
	for i, p := range paragraphs {
		pkg, err := packageFromParagraph(p, now)
		if err != nil {
			return nil, fmt.Errorf("seed: paragraph %d: %w", i+1, err)
		}
		out = append(out, *pkg)
	}
	return out, nil
}

func packageFromParagraph(p *debcontrol.Paragraph, now time.Time) (*pkgmeta.Package, error) {
	name := p.Get("Package")
	if name == "" {
		return nil, fmt.Errorf("no Package field")
	}
	filename := p.Get("Filename")
	if filename == "" {
		return nil, fmt.Errorf("%s has no Filename", name)
	}

	size, err := strconv.ParseInt(p.Get("Size"), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%s has an unusable Size %q: %w", name, p.Get("Size"), err)
	}

	// The existing index carries SHA1 and SHA256 but no SHA512, and computing
	// one would mean downloading every package. A record seeded without it is
	// complete for our purposes, because the generator does not emit SHA512.
	pkg := &pkgmeta.Package{
		Format:       pkgmeta.FormatDeb,
		Name:         name,
		Version:      p.Get("Version"),
		Architecture: p.Get("Architecture"),
		S3Key:        filename,
		Filename:     filename,
		Size:         size,
		MD5:          p.Get("MD5sum"),
		SHA1:         p.Get("SHA1"),
		SHA256:       p.Get("SHA256"),
		UpdatedAt:    now,
	}
	pkg.Summary, pkg.Description = splitDescription(p.Get("Description"))

	control := p.Clone()
	for _, f := range locationFields {
		control.Delete(f)
	}
	pkg.Deb = &pkgmeta.DebDetail{Control: control}

	if pkg.Version == "" {
		return nil, fmt.Errorf("%s has no Version", name)
	}
	if pkg.Architecture == "" {
		return nil, fmt.Errorf("%s has no Architecture", name)
	}
	if pkg.SHA256 == "" && pkg.SHA1 == "" {
		return nil, fmt.Errorf("%s has no usable checksum", name)
	}
	return pkg, nil
}

func splitDescription(desc string) (summary, body string) {
	summary, body, _ = strings.Cut(desc, "\n")
	return summary, body
}
