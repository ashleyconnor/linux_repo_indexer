// Package deb extracts metadata from a .deb file.
//
// A .deb is an ar archive of three members: debian-binary, control.tar.* and
// data.tar.*. Only the control member is of interest — the payload is never
// unpacked, because the index needs the control paragraph and nothing else.
package deb

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/blakesmith/ar"
	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"

	"github.com/ashleyconnor/linux-repo-indexer/internal/debcontrol"
	"github.com/ashleyconnor/linux-repo-indexer/internal/pkgmeta"
)

// arMagic is the global header every ar archive starts with.
const arMagic = "!<arch>\n"

// maxControlSize caps how much of control.tar.* we will decompress. A control
// file is a few kilobytes; anything approaching this bound is a decompression
// bomb rather than a package.
const maxControlSize = 8 << 20

// Parse reads a .deb from r and returns its metadata.
//
// Parse stops as soon as the control member has been read, so r is usually
// left partly unread. Callers that also need to hash the object should tee r
// into their hashers and drain the remainder afterwards; that way the package
// is read from S3 exactly once.
//
// The returned Package carries no location or integrity fields — S3Key,
// Filename, Size and the checksums describe the object, not its contents, and
// are the caller's to fill in.
func Parse(r io.Reader) (*pkgmeta.Package, error) {
	br := bufio.NewReader(r)

	magic, err := br.Peek(len(arMagic))
	if err != nil {
		return nil, fmt.Errorf("deb: reading ar magic: %w", err)
	}
	if string(magic) != arMagic {
		return nil, fmt.Errorf("deb: not an ar archive (magic %q)", magic)
	}

	ar := ar.NewReader(br)
	for {
		hdr, err := ar.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("deb: no control.tar member found")
		}
		if err != nil {
			return nil, fmt.Errorf("deb: reading ar member: %w", err)
		}

		// ar pads short names with spaces and some writers terminate them
		// with a slash.
		name := strings.TrimSuffix(strings.TrimSpace(hdr.Name), "/")
		if !strings.HasPrefix(name, "control.tar") {
			continue
		}

		cr, err := decompress(name, ar)
		if err != nil {
			return nil, err
		}
		control, err := readControlFile(cr)
		if err != nil {
			return nil, err
		}
		return buildPackage(control)
	}
}

// decompress wraps the member reader according to the control archive's
// extension. dpkg has shipped gzip, xz, bzip2 and zstd control tarballs over
// the years, and an uncompressed control.tar is legal too.
func decompress(name string, r io.Reader) (io.Reader, error) {
	limited := io.LimitReader(r, maxControlSize)

	switch path.Ext(name) {
	case ".tar":
		return limited, nil
	case ".gz":
		zr, err := gzip.NewReader(limited)
		if err != nil {
			return nil, fmt.Errorf("deb: %s: %w", name, err)
		}
		return zr, nil
	case ".xz":
		xr, err := xz.NewReader(limited)
		if err != nil {
			return nil, fmt.Errorf("deb: %s: %w", name, err)
		}
		return xr, nil
	case ".zst":
		zr, err := zstd.NewReader(limited)
		if err != nil {
			return nil, fmt.Errorf("deb: %s: %w", name, err)
		}
		return zr.IOReadCloser(), nil
	case ".bz2":
		return bzip2.NewReader(limited), nil
	default:
		return nil, fmt.Errorf("deb: unsupported control archive %q", name)
	}
}

// readControlFile pulls ./control out of the decompressed control tarball.
func readControlFile(r io.Reader) (*debcontrol.Paragraph, error) {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("deb: control file missing from control archive")
		}
		if err != nil {
			return nil, fmt.Errorf("deb: reading control archive: %w", err)
		}

		if path.Clean(hdr.Name) != "control" {
			continue
		}

		data, err := io.ReadAll(io.LimitReader(tr, maxControlSize))
		if err != nil {
			return nil, fmt.Errorf("deb: reading control file: %w", err)
		}
		p, err := debcontrol.Parse(string(data))
		if err != nil {
			return nil, fmt.Errorf("deb: parsing control file: %w", err)
		}
		return p, nil
	}
}

// fieldsNotInIndex are control fields that describe the package's installed
// state rather than the package itself. dpkg writes them into control.tar for
// its own bookkeeping and apt-ftparchive drops them from the Packages index.
var fieldsNotInIndex = []string{"Status", "Config-Version", "Conffiles", "Triggers-Awaited", "Triggers-Pending"}

func buildPackage(control *debcontrol.Paragraph) (*pkgmeta.Package, error) {
	name := control.Get("Package")
	if name == "" {
		return nil, fmt.Errorf("deb: control file has no Package field")
	}
	version := control.Get("Version")
	if version == "" {
		return nil, fmt.Errorf("deb: control file for %q has no Version field", name)
	}
	arch := control.Get("Architecture")
	if arch == "" {
		return nil, fmt.Errorf("deb: control file for %q has no Architecture field", name)
	}

	indexed := control.Clone()
	for _, f := range fieldsNotInIndex {
		indexed.Delete(f)
	}

	summary, body := splitDescription(control.Get("Description"))

	return &pkgmeta.Package{
		Format:       pkgmeta.FormatDeb,
		Name:         name,
		Version:      version,
		Architecture: arch,
		Summary:      summary,
		Description:  body,
		Deb:          &pkgmeta.DebDetail{Control: indexed},
	}, nil
}

// splitDescription separates a Debian description into its one-line synopsis
// and the extended body that follows it.
func splitDescription(desc string) (summary, body string) {
	summary, body, _ = strings.Cut(desc, "\n")
	return summary, body
}

// ParseBytes is a convenience wrapper for callers that already hold the whole
// package in memory, such as tests and the seeder's verification mode.
func ParseBytes(b []byte) (*pkgmeta.Package, error) {
	return Parse(bytes.NewReader(b))
}
