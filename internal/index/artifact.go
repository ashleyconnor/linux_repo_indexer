// Package index defines what the repository generators produce.
//
// Generators are pure functions from package records to Artifacts. They know
// nothing about S3, DynamoDB or Lambda, which is what makes them testable
// against the output of apt-ftparchive and createrepo_c.
package index

import (
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"

	"compress/gzip"

	"github.com/ulikunitz/xz"
)

// An Artifact is one file to publish, at a path relative to the repository
// root.
type Artifact struct {
	Path        string
	ContentType string
	Body        []byte
}

// Digests describes a published file well enough for a Release file or a
// repomd entry to reference it.
type Digests struct {
	Size   int64  `json:"size"`
	MD5    string `json:"md5"`
	SHA1   string `json:"sha1"`
	SHA256 string `json:"sha256"`
}

// DigestsOf computes all three checksums in a single pass.
func DigestsOf(b []byte) Digests {
	m, s1, s256 := md5.New(), sha1.New(), sha256.New()
	w := io.MultiWriter(m, s1, s256)
	w.Write(b)
	return Digests{
		Size:   int64(len(b)),
		MD5:    hex.EncodeToString(m.Sum(nil)),
		SHA1:   hex.EncodeToString(s1.Sum(nil)),
		SHA256: hex.EncodeToString(s256.Sum(nil)),
	}
}

// Digests computes the artifact's checksums.
func (a Artifact) Digests() Digests { return DigestsOf(a.Body) }

// HashByName returns the digest a checksum algorithm names it, using the
// spellings apt uses for its by-hash directories.
func (d Digests) HashByName(algorithm string) (string, error) {
	switch algorithm {
	case "MD5Sum":
		return d.MD5, nil
	case "SHA1":
		return d.SHA1, nil
	case "SHA256":
		return d.SHA256, nil
	default:
		return "", fmt.Errorf("index: unknown checksum algorithm %q", algorithm)
	}
}

// Compression identifies one of the encodings an index file is published in.
type Compression string

const (
	CompressionNone Compression = ""
	CompressionGzip Compression = "gz"
	CompressionXZ   Compression = "xz"
)

// Suffix is the file extension for this encoding, including the dot.
func (c Compression) Suffix() string {
	if c == CompressionNone {
		return ""
	}
	return "." + string(c)
}

// ContentType is the MIME type to store the encoded file under.
func (c Compression) ContentType() string {
	switch c {
	case CompressionGzip:
		return "application/gzip"
	case CompressionXZ:
		return "application/x-xz"
	default:
		return "text/plain; charset=utf-8"
	}
}

// Compress encodes b. The output is deterministic: publishing runs on
// unchanged input must produce byte-identical files, both so that repodata
// filenames (which embed their own checksum) stay stable and so that S3 is not
// rewritten on every no-op publish.
func Compress(b []byte, c Compression) ([]byte, error) {
	switch c {
	case CompressionNone:
		return b, nil

	case CompressionGzip:
		var buf bytes.Buffer
		zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if err != nil {
			return nil, fmt.Errorf("index: gzip: %w", err)
		}
		// The default header carries the current time, which would make every
		// publish produce a different file for identical content.
		zw.Header = gzip.Header{OS: 255} // 255 = unknown, per RFC 1952
		if _, err := zw.Write(b); err != nil {
			return nil, fmt.Errorf("index: gzip: %w", err)
		}
		if err := zw.Close(); err != nil {
			return nil, fmt.Errorf("index: gzip: %w", err)
		}
		return buf.Bytes(), nil

	case CompressionXZ:
		var buf bytes.Buffer
		xw, err := xz.NewWriter(&buf)
		if err != nil {
			return nil, fmt.Errorf("index: xz: %w", err)
		}
		if _, err := xw.Write(b); err != nil {
			return nil, fmt.Errorf("index: xz: %w", err)
		}
		if err := xw.Close(); err != nil {
			return nil, fmt.Errorf("index: xz: %w", err)
		}
		return buf.Bytes(), nil

	default:
		return nil, fmt.Errorf("index: unsupported compression %q", c)
	}
}

// hashOf is a small helper for generators that need one named digest.
func hashOf(h hash.Hash, b []byte) string {
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// SHA256Hex returns the hex-encoded SHA256 of b.
func SHA256Hex(b []byte) string { return hashOf(sha256.New(), b) }
