//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// These are the tests that decide whether the whole thing works. Everything
// else asserts that our output matches what we believe correct output to be;
// these hand the generated repository to the real package managers, with
// signature verification enabled, and see whether they will install from it.

// hostArches returns the Debian and RPM architecture names matching the
// machine running the test, since the containers run natively.
func hostArches() (debArch, rpmArch string) {
	if runtime.GOARCH == "arm64" {
		return "arm64", "aarch64"
	}
	return "amd64", "x86_64"
}

// publishFixtures uploads the fixture packages for the host architecture and
// waits for the repository to be published.
func publishFixtures(t *testing.T, stack *Stack) (debArch, rpmArch string) {
	t.Helper()
	debArch, rpmArch = hostArches()

	debName := fmt.Sprintf("indexer-fixture_1.0.0-1_%s.deb", debArch)
	rpmName := fmt.Sprintf("indexer-fixture-1.0.0-1.%s.rpm", rpmArch)

	stack.PutFixture(t, fmt.Sprintf("pool/%s/main/%s", debArch, debName), "deb", debName)
	stack.PutFixture(t, fmt.Sprintf("RHEL/9/%s/stable/%s", rpmArch, rpmName), "rpm", rpmName)

	// Clients fetch the signing key from the repository, the way HashiCorp's
	// own instructions tell them to.
	stack.PutObject(t, "gpg", stack.SigningKey, "application/pgp-keys")

	stack.WaitForObject(t, "dists/noble/InRelease", settle)
	stack.WaitForObject(t, fmt.Sprintf("dists/noble/main/binary-%s/Packages", debArch), settle)
	stack.WaitForObject(t, fmt.Sprintf("RHEL/9/%s/stable/repodata/repomd.xml.asc", rpmArch), settle)

	return debArch, rpmArch
}

// runInContainer starts an image on the stack's network and runs a script,
// returning its combined output.
func runInContainer(t *testing.T, stack *Stack, image, script string) (string, error) {
	t.Helper()
	ctx := context.Background()

	req := testcontainers.ContainerRequest{
		Image:      image,
		Networks:   []string{stack.Network},
		Entrypoint: []string{"/bin/bash", "-c"},
		Cmd:        []string{script},
		WaitingFor: wait.ForExit().WithExitTimeout(10 * time.Minute),
	}

	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return "", fmt.Errorf("starting %s: %w", image, err)
	}
	defer func() {
		if err := testcontainers.TerminateContainer(c); err != nil {
			t.Logf("terminating %s: %v", image, err)
		}
	}()

	logs, err := c.Logs(ctx)
	if err != nil {
		return "", fmt.Errorf("reading logs from %s: %w", image, err)
	}
	defer logs.Close()

	out := new(strings.Builder)
	if _, err := io.Copy(out, logs); err != nil {
		return "", err
	}

	state, err := c.State(ctx)
	if err != nil {
		return out.String(), fmt.Errorf("inspecting %s: %w", image, err)
	}
	if state.ExitCode != 0 {
		return out.String(), fmt.Errorf("%s exited %d", image, state.ExitCode)
	}
	return out.String(), nil
}

// TestUbuntuInstallsFromGeneratedRepository proves the whole apt chain: the
// clearsigned InRelease verifies against the imported key, the Packages file
// it names has the checksum apt expects, and the .deb that file points at has
// the checksum the Packages entry claims. Any break anywhere in the metadata
// we generate shows up here as a failed install.
func TestUbuntuInstallsFromGeneratedRepository(t *testing.T) {
	stack := StartStack(t)
	stack.PutConfig(t, reposYAML)
	debArch, _ := publishFixtures(t, stack)

	baseURL := fmt.Sprintf("%s/%s", stack.Internal, stack.Bucket)

	script := fmt.Sprintf(`set -euxo pipefail

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq -o Acquire::Retries=3
apt-get install -y -qq --no-install-recommends ca-certificates curl >/dev/null

install -d -m 0755 /etc/apt/keyrings
curl -fsSL %[1]s/gpg -o /etc/apt/keyrings/indexer.asc

# signed-by pins this repository to our key, so the install below only
# succeeds if InRelease actually verifies against it.
echo "deb [arch=%[2]s signed-by=/etc/apt/keyrings/indexer.asc] %[1]s noble main" \
  > /etc/apt/sources.list.d/indexer.list

apt-get update -o Acquire::Retries=3
apt-get install -y indexer-fixture

indexer-fixture | tee /tmp/out
grep -q "indexer-fixture 1.0.0-1" /tmp/out
echo "INSTALL_OK"
`, baseURL, debArch)

	out, err := runInContainer(t, stack, "ubuntu:24.04", script)
	if err != nil {
		t.Fatalf("apt install failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "INSTALL_OK") {
		t.Fatalf("install did not complete:\n%s", out)
	}

	// apt reports an unverifiable repository as a warning and carries on for
	// some operations, so check it never said so.
	for _, bad := range []string{"NO_PUBKEY", "not signed", "is not signed", "InRelease is not valid"} {
		if strings.Contains(out, bad) {
			t.Errorf("apt reported a signature problem (%q):\n%s", bad, out)
		}
	}
}

// TestUBI9InstallsFromGeneratedRepository proves the dnf chain: repomd.xml.asc
// verifies against the imported key, and primary.xml resolves the package to a
// downloadable object with a matching checksum.
//
// repo_gpgcheck is on, which is the part this service is responsible for.
// gpgcheck is off because the fixture RPM is not package-signed; signing
// packages is the build system's job, not the indexer's.
func TestUBI9InstallsFromGeneratedRepository(t *testing.T) {
	stack := StartStack(t)
	stack.PutConfig(t, reposYAML)
	_, rpmArch := publishFixtures(t, stack)

	baseURL := fmt.Sprintf("%s/%s", stack.Internal, stack.Bucket)

	script := fmt.Sprintf(`set -euxo pipefail

cat > /etc/yum.repos.d/indexer.repo <<EOF
[indexer]
name=Indexer Test
baseurl=%[1]s/RHEL/9/%[2]s/stable
enabled=1
gpgcheck=0
repo_gpgcheck=1
gpgkey=%[1]s/gpg
EOF

rpm --import %[1]s/gpg

# Verify the pair by hand first. If this fails, the signature genuinely does
# not match what is being served; if it succeeds but dnf still refuses, the
# problem is in how dnf fetches or caches them.
curl -fsS %[1]s/RHEL/9/%[2]s/stable/repodata/repomd.xml -o /tmp/repomd.xml
curl -fsS %[1]s/RHEL/9/%[2]s/stable/repodata/repomd.xml.asc -o /tmp/repomd.xml.asc
curl -fsS %[1]s/gpg -o /tmp/key.asc
gpg --import /tmp/key.asc
gpg --verify /tmp/repomd.xml.asc /tmp/repomd.xml

# -y on makecache matters: without it dnf will not accept the repo key from
# gpgkey=, librepo then sees a signature from an untrusted key, and reports it
# as "Bad GPG signature" rather than as a missing key.
dnf -y --disablerepo='*' --enablerepo=indexer makecache
dnf -y --disablerepo='*' --enablerepo=indexer install indexer-fixture

indexer-fixture | tee /tmp/out
grep -q "indexer-fixture 1.0.0" /tmp/out
echo "INSTALL_OK"
`, baseURL, rpmArch)

	out, err := runInContainer(t, stack, "registry.access.redhat.com/ubi9/ubi", script)
	if err != nil {
		t.Fatalf("dnf install failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "INSTALL_OK") {
		t.Fatalf("install did not complete:\n%s", out)
	}
}

// TestUbuntuRejectsTamperedIndex is the negative control. Without it, the test
// above would still pass if verification were silently disabled, because a
// repository that is never checked installs just as happily as one that is.
func TestUbuntuRejectsTamperedIndex(t *testing.T) {
	stack := StartStack(t)
	stack.PutConfig(t, reposYAML)
	debArch, _ := publishFixtures(t, stack)

	// Replace the signed InRelease with an unsigned copy of Release. apt must
	// refuse it, because the sources entry pins the repository to our key.
	release := stack.WaitForObject(t, "dists/noble/Release", settle)
	stack.PutObject(t, "dists/noble/InRelease", release, "text/plain")
	stack.DeleteObject(t, "dists/noble/Release.gpg")

	baseURL := fmt.Sprintf("%s/%s", stack.Internal, stack.Bucket)

	script := fmt.Sprintf(`set -eux

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq -o Acquire::Retries=3
apt-get install -y -qq --no-install-recommends ca-certificates curl >/dev/null

install -d -m 0755 /etc/apt/keyrings
curl -fsSL %[1]s/gpg -o /etc/apt/keyrings/indexer.asc

echo "deb [arch=%[2]s signed-by=/etc/apt/keyrings/indexer.asc] %[1]s noble main" \
  > /etc/apt/sources.list.d/indexer.list

if apt-get update -o Acquire::Retries=1; then
  echo "UPDATE_SUCCEEDED"
else
  echo "UPDATE_REJECTED"
fi
`, baseURL, debArch)

	out, _ := runInContainer(t, stack, "ubuntu:24.04", script)
	if strings.Contains(out, "UPDATE_SUCCEEDED") {
		t.Fatalf("apt accepted an unsigned index, so the passing install test proves nothing:\n%s", out)
	}
	if !strings.Contains(out, "UPDATE_REJECTED") {
		t.Fatalf("could not tell whether apt rejected the index:\n%s", out)
	}
}
