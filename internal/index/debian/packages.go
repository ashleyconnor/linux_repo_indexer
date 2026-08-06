// Package debian generates apt repository metadata: the Packages index and
// the Release file that signs it.
//
// Generation is split in two because the pool is shared across codenames. The
// live repository publishes the identical package set to jammy, noble,
// bookworm and trixie, so a Packages file is rendered and compressed once per
// component/architecture and then placed into every codename that claims it.
package debian

import (
	"bytes"
	"cmp"
	"fmt"
	"path"
	"slices"
	"strconv"

	"github.com/ashleyconnor/linux-repo-indexer/internal/index"
	"github.com/ashleyconnor/linux-repo-indexer/internal/pkgmeta"
)

// byHashAlgorithms are the checksum directories apt looks in when a Release
// declares Acquire-By-Hash, in the spelling apt expects.
var byHashAlgorithms = []string{"MD5Sum", "SHA1", "SHA256"}

// A Variant is one encoding of the Packages file.
type Variant struct {
	Compression index.Compression
	Body        []byte
	Digests     index.Digests
}

// A PackagesIndex is the rendered Packages file in every encoding, not yet
// bound to a location. Compressing once and placing many times is what keeps a
// fan-out across codenames cheap.
type PackagesIndex struct {
	Variants []Variant
}

// An IndexFile is one published file as the Release file must reference it:
// by its path relative to the codename's dists directory.
type IndexFile struct {
	Path    string        `json:"path"`
	Digests index.Digests `json:"digests"`
}

// BuildPackagesIndex renders the Packages file for one component and
// architecture and encodes it with each requested compression.
//
// The uncompressed variant is always produced; apt can read it directly and
// the Release file references all three.
func BuildPackagesIndex(pkgs []pkgmeta.Package, compressions []index.Compression) (*PackagesIndex, error) {
	body, err := RenderPackages(pkgs)
	if err != nil {
		return nil, err
	}

	wanted := append([]index.Compression{index.CompressionNone}, compressions...)

	out := &PackagesIndex{}
	seen := make(map[index.Compression]bool, len(wanted))
	for _, c := range wanted {
		if seen[c] {
			continue
		}
		seen[c] = true

		encoded, err := index.Compress(body, c)
		if err != nil {
			return nil, err
		}
		out.Variants = append(out.Variants, Variant{
			Compression: c,
			Body:        encoded,
			Digests:     index.DigestsOf(encoded),
		})
	}
	return out, nil
}

// Artifacts places the index into dir, which is relative to the repository
// root, for example "dists/noble/main/binary-amd64".
//
// When byHash is set each variant is additionally written under
// by-hash/<algorithm>/<digest>. Those copies are what apt actually fetches, and
// they are why an index update cannot break a client that is midway through a
// refresh: the old digests keep resolving until they are pruned.
func (p *PackagesIndex) Artifacts(dir string, byHash bool) []index.Artifact {
	var out []index.Artifact
	for _, v := range p.Variants {
		name := "Packages" + v.Compression.Suffix()
		out = append(out, index.Artifact{
			Path:        path.Join(dir, name),
			ContentType: v.Compression.ContentType(),
			Body:        v.Body,
		})

		if !byHash {
			continue
		}
		for _, algorithm := range byHashAlgorithms {
			digest, err := v.Digests.HashByName(algorithm)
			if err != nil {
				continue // unreachable: byHashAlgorithms is a fixed list
			}
			out = append(out, index.Artifact{
				Path:        path.Join(dir, "by-hash", algorithm, digest),
				ContentType: v.Compression.ContentType(),
				Body:        v.Body,
			})
		}
	}
	return out
}

// Files describes the index's canonical paths for the Release file. relDir is
// relative to the codename directory, for example "main/binary-amd64".
func (p *PackagesIndex) Files(relDir string) []IndexFile {
	out := make([]IndexFile, 0, len(p.Variants))
	for _, v := range p.Variants {
		out = append(out, IndexFile{
			Path:    path.Join(relDir, "Packages"+v.Compression.Suffix()),
			Digests: v.Digests,
		})
	}
	return out
}

// RenderPackages concatenates one control paragraph per package, which is all
// a Packages file is.
//
// Each paragraph is the package's own control data with the location fields
// appended: Filename, Size and the checksums describe the object rather than
// the package, so a stored record never carries them. Packages are sorted so
// that identical input always yields byte-identical output.
func RenderPackages(pkgs []pkgmeta.Package) ([]byte, error) {
	sorted := slices.Clone(pkgs)
	slices.SortFunc(sorted, func(a, b pkgmeta.Package) int {
		if c := cmp.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Version, b.Version); c != 0 {
			return c
		}
		return cmp.Compare(a.Architecture, b.Architecture)
	})

	var buf bytes.Buffer
	for i := range sorted {
		p := &sorted[i]
		if p.Format != pkgmeta.FormatDeb {
			return nil, fmt.Errorf("debian: %s is a %s package", p.Name, p.Format)
		}
		if p.Deb == nil || p.Deb.Control == nil {
			return nil, fmt.Errorf("debian: %s has no control paragraph", p.Name)
		}
		if p.Filename == "" {
			return nil, fmt.Errorf("debian: %s has no Filename", p.Name)
		}

		para := p.Deb.Control.Clone()
		para.Set("Filename", p.Filename)
		para.Set("Size", strconv.FormatInt(p.Size, 10))
		if p.MD5 != "" {
			para.Set("MD5sum", p.MD5)
		}
		if p.SHA1 != "" {
			para.Set("SHA1", p.SHA1)
		}
		if p.SHA256 != "" {
			para.Set("SHA256", p.SHA256)
		}

		if _, err := para.WriteTo(&buf); err != nil {
			return nil, fmt.Errorf("debian: rendering %s: %w", p.Name, err)
		}
	}

	return buf.Bytes(), nil
}
