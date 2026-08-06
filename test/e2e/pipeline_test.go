//go:build e2e

package e2e

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ashleyconnor/linux-repo-indexer/internal/debcontrol"
	"github.com/ashleyconnor/linux-repo-indexer/internal/index"
	"github.com/ashleyconnor/linux-repo-indexer/internal/sign"
)

// The matrix under test: one codename and one yum tree, both channels, so the
// component and channel dimensions are exercised without a container per row.
const reposYAML = `
version: 1
signing:
  secret_id: lri-test/gpg-signing-key
apt:
  origin: HashiCorp
  label: HashiCorp
  acquire_by_hash: true
  compressions: [gz, xz]
  components: [main, test]
  architectures: [amd64, arm64]
  codenames: [noble]
rpm:
  trees:
    - distro: RHEL
      version: "9"
      architectures: [x86_64, aarch64]
      channels: [stable, test]
`

const (
	debKey = "pool/amd64/main/indexer-fixture_1.0.0-1_amd64.deb"
	rpmKey = "RHEL/9/x86_64/stable/indexer-fixture-1.0.0-1.x86_64.rpm"

	packagesKey = "dists/noble/main/binary-amd64/Packages"
	releaseKey  = "dists/noble/Release"
	repomdKey   = "RHEL/9/x86_64/stable/repodata/repomd.xml"

	settle = 90 * time.Second
)

// TestPipelinePublishesBothFormats is the end-to-end path the whole design
// exists to serve: a package lands in S3, an event fires, a Lambda reads it
// once, and a signed index appears — with no full-repository rescan anywhere.
func TestPipelinePublishesBothFormats(t *testing.T) {
	stack := StartStack(t)
	stack.PutConfig(t, reposYAML)

	stack.PutFixture(t, debKey, "deb", "indexer-fixture_1.0.0-1_amd64.deb")
	stack.PutFixture(t, rpmKey, "rpm", "indexer-fixture-1.0.0-1.x86_64.rpm")

	t.Run("debian index", func(t *testing.T) {
		body := stack.WaitForObject(t, packagesKey, settle)

		paras, err := debcontrol.ReadAll(bytes.NewReader(body))
		if err != nil {
			t.Fatalf("generated Packages is not parseable: %v\n%s", err, body)
		}
		if len(paras) != 1 {
			t.Fatalf("got %d paragraphs, want 1:\n%s", len(paras), body)
		}
		if got, want := paras[0].Get("Package"), "indexer-fixture"; got != want {
			t.Errorf("Package = %q, want %q", got, want)
		}
		if got, want := paras[0].Get("Filename"), debKey; got != want {
			t.Errorf("Filename = %q, want %q", got, want)
		}
		if paras[0].Get("SHA256") == "" {
			t.Error("Packages entry has no SHA256, so apt could not verify the download")
		}
	})

	t.Run("compressed variants", func(t *testing.T) {
		gz := stack.WaitForObject(t, packagesKey+".gz", settle)
		zr, err := gzip.NewReader(bytes.NewReader(gz))
		if err != nil {
			t.Fatalf("Packages.gz is not gzip: %v", err)
		}
		plain, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("reading Packages.gz: %v", err)
		}
		if !bytes.Equal(plain, stack.WaitForObject(t, packagesKey, settle)) {
			t.Error("Packages.gz does not match Packages")
		}
		stack.WaitForObject(t, packagesKey+".xz", settle)
	})

	t.Run("acquire by hash", func(t *testing.T) {
		body := stack.WaitForObject(t, packagesKey, settle)
		release := stack.WaitForObject(t, releaseKey, settle)

		// Release declares Acquire-By-Hash, so apt will fetch the index from
		// its digest path; that object has to exist.
		p, err := debcontrol.Parse(string(release))
		if err != nil {
			t.Fatalf("parsing Release: %v", err)
		}
		if got := p.Get("Acquire-By-Hash"); got != "yes" {
			t.Fatalf("Acquire-By-Hash = %q, want yes", got)
		}

		digest := index.SHA256Hex(body)
		key := "dists/noble/main/binary-amd64/by-hash/SHA256/" + digest
		byHash := stack.WaitForObject(t, key, settle)
		if !bytes.Equal(byHash, body) {
			t.Error("the by-hash object does not match the index it names")
		}
	})

	t.Run("release is signed", func(t *testing.T) {
		release := stack.WaitForObject(t, releaseKey, settle)
		inRelease := stack.WaitForObject(t, "dists/noble/InRelease", settle)
		detached := stack.WaitForObject(t, "dists/noble/Release.gpg", settle)

		payload, err := sign.VerifyClearSigned(stack.SigningKey, inRelease)
		if err != nil {
			t.Fatalf("InRelease does not verify: %v", err)
		}
		if strings.TrimSpace(string(payload)) != strings.TrimSpace(string(release)) {
			t.Error("InRelease payload differs from Release")
		}
		if err := sign.Verify(stack.SigningKey, release, detached); err != nil {
			t.Fatalf("Release.gpg does not verify: %v", err)
		}
	})

	t.Run("release covers every component and architecture", func(t *testing.T) {
		p, err := debcontrol.Parse(string(stack.WaitForObject(t, releaseKey, settle)))
		if err != nil {
			t.Fatalf("parsing Release: %v", err)
		}
		if got, want := p.Get("Components"), "main test"; got != want {
			t.Errorf("Components = %q, want %q", got, want)
		}
		if got, want := p.Get("Architectures"), "amd64 arm64"; got != want {
			t.Errorf("Architectures = %q, want %q", got, want)
		}
	})

	t.Run("yum repodata", func(t *testing.T) {
		repomd := stack.WaitForObject(t, repomdKey, settle)

		for _, want := range []string{`type="primary"`, `type="filelists"`, `type="other"`} {
			if !bytes.Contains(repomd, []byte(want)) {
				t.Errorf("repomd.xml is missing %s:\n%s", want, repomd)
			}
		}

		signature := stack.WaitForObject(t, repomdKey+".asc", settle)
		if err := sign.Verify(stack.SigningKey, repomd, signature); err != nil {
			t.Fatalf("repomd.xml.asc does not verify: %v", err)
		}

		// Everything repomd names must actually be published.
		for _, key := range stack.List(t, "RHEL/9/x86_64/stable/repodata/") {
			rel := strings.TrimPrefix(key, "RHEL/9/x86_64/stable/")
			if strings.HasSuffix(rel, ".xml.gz") && !bytes.Contains(repomd, []byte(rel)) {
				t.Errorf("%s is published but not referenced by repomd.xml", key)
			}
		}
	})
}

// TestPipelineDeIndexesRemovedPackage covers the other half of the contract: a
// package removed from the bucket must leave the index, or every client that
// tries to install it gets a 404 from a repository that promised it was there.
func TestPipelineDeIndexesRemovedPackage(t *testing.T) {
	stack := StartStack(t)
	stack.PutConfig(t, reposYAML)

	const otherKey = "pool/amd64/main/indexer-other_2.5.0-1_amd64.deb"
	stack.PutFixture(t, debKey, "deb", "indexer-fixture_1.0.0-1_amd64.deb")
	stack.PutFixture(t, otherKey, "deb", "indexer-other_2.5.0-1_amd64.deb")

	stack.WaitUntil(t, "both packages to be indexed", settle, func() (bool, error) {
		body, err := stack.GetObject(context.Background(), packagesKey)
		if err != nil {
			return false, err
		}
		return bytes.Contains(body, []byte("Package: indexer-fixture\n")) &&
			bytes.Contains(body, []byte("Package: indexer-other\n")), nil
	})

	stack.DeleteObject(t, otherKey)

	stack.WaitUntil(t, "the removed package to leave the index", settle, func() (bool, error) {
		body, err := stack.GetObject(context.Background(), packagesKey)
		if err != nil {
			return false, err
		}
		return !bytes.Contains(body, []byte("Package: indexer-other\n")) &&
			bytes.Contains(body, []byte("Package: indexer-fixture\n")), nil
	})
}

// TestPipelineIgnoresItsOwnOutput guards the loop that would otherwise never
// settle: the publisher writes into the same bucket it is triggered by.
func TestPipelineIgnoresItsOwnOutput(t *testing.T) {
	stack := StartStack(t)
	stack.PutConfig(t, reposYAML)
	stack.PutFixture(t, debKey, "deb", "indexer-fixture_1.0.0-1_amd64.deb")

	stack.WaitForObject(t, releaseKey, settle)

	// Once published, the scope must settle: a stable generation means the
	// index writes did not feed back into ingest.
	stack.Sweep(t)
	first := stack.WaitForObject(t, packagesKey, settle)

	time.Sleep(10 * time.Second)
	stack.Sweep(t)

	second, err := stack.GetObject(context.Background(), packagesKey)
	if err != nil {
		t.Fatalf("re-reading Packages: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Error("a no-op publish rewrote the index, so the pipeline is not settling")
	}
}
