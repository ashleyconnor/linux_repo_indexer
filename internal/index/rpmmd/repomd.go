package rpmmd

import (
	"fmt"
	"path"
	"time"

	"github.com/ashleyconnor/linux-repo-indexer/internal/index"
	"github.com/ashleyconnor/linux-repo-indexer/internal/pkgmeta"
)

// repodataDir is where a yum tree keeps its metadata, relative to the tree
// root that a client's baseurl points at.
const repodataDir = "repodata"

// metadataKinds are the three files repomd.xml indexes, in createrepo_c's
// order.
var metadataKinds = []struct {
	name  string
	build func([]pkgmeta.Package) ([]byte, error)
}{
	{"primary", BuildPrimary},
	{"filelists", BuildFilelists},
	{"other", BuildOther},
}

// A Repodata is a complete, self-contained yum metadata directory: the three
// compressed metadata files plus the repomd.xml that references them.
type Repodata struct {
	// Artifacts are the files to publish, at paths relative to the tree root.
	// repomd.xml is last, so a writer that publishes in order never advertises
	// metadata it has not yet uploaded.
	Artifacts []index.Artifact

	// RepomdXML is the unsigned repomd.xml, kept separately because it is the
	// document the detached signature covers.
	RepomdXML []byte
}

// BuildRepodata generates a yum tree's metadata.
//
// revision is the publication timestamp recorded in repomd.xml. It is passed
// in rather than read from the clock so that output stays reproducible.
func BuildRepodata(pkgs []pkgmeta.Package, revision time.Time) (*Repodata, error) {
	stamp := revision.Unix()

	w := &writer{}
	w.decl()
	w.open(0, "repomd", at("xmlns", nsRepo), at("xmlns:rpm", nsRPM))
	w.text(1, "revision", fmt.Sprint(stamp))

	out := &Repodata{}

	for _, kind := range metadataKinds {
		plain, err := kind.build(pkgs)
		if err != nil {
			return nil, err
		}
		compressed, err := index.Compress(plain, index.CompressionGzip)
		if err != nil {
			return nil, fmt.Errorf("rpmmd: compressing %s: %w", kind.name, err)
		}

		// The published name embeds the compressed file's own checksum, which
		// is what makes a metadata update atomic from a client's point of
		// view: new metadata lands at a new path and repomd.xml switches to it
		// in one write.
		digest := index.SHA256Hex(compressed)
		href := path.Join(repodataDir, fmt.Sprintf("%s-%s.xml.gz", digest, kind.name))

		out.Artifacts = append(out.Artifacts, index.Artifact{
			Path:        href,
			ContentType: index.CompressionGzip.ContentType(),
			Body:        compressed,
		})

		w.open(1, "data", at("type", kind.name))
		w.text(2, "checksum", digest, at("type", "sha256"))
		w.text(2, "open-checksum", index.SHA256Hex(plain), at("type", "sha256"))
		w.empty(2, "location", at("href", href))
		w.text(2, "timestamp", fmt.Sprint(stamp))
		w.text(2, "size", fmt.Sprint(len(compressed)))
		w.text(2, "open-size", fmt.Sprint(len(plain)))
		w.close(1, "data")
	}

	w.close(0, "repomd")

	out.RepomdXML = w.document()
	out.Artifacts = append(out.Artifacts, index.Artifact{
		Path:        path.Join(repodataDir, "repomd.xml"),
		ContentType: "application/xml",
		Body:        out.RepomdXML,
	})

	return out, nil
}

// SignatureArtifact wraps a detached signature over repomd.xml as the file dnf
// fetches when repo_gpgcheck is enabled.
func SignatureArtifact(signature []byte) index.Artifact {
	return index.Artifact{
		Path:        path.Join(repodataDir, "repomd.xml.asc"),
		ContentType: "application/pgp-signature",
		Body:        signature,
	}
}
