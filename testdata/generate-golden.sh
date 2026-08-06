#!/usr/bin/env bash
# Regenerates the golden index files, using the canonical tools.
#
# apt-ftparchive and createrepo_c define what correct output looks like. Our
# generators are checked against these files so that a divergence shows up as a
# unit-test failure rather than as a broken client months later.
#
# Usage: ./testdata/generate-golden.sh
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
packages="$here/packages"
golden="$here/golden"

if [ ! -d "$packages/deb" ] || [ ! -d "$packages/rpm" ]; then
  echo "fixtures missing; run ./testdata/generate.sh first" >&2
  exit 1
fi

rm -rf "$golden"
mkdir -p "$golden/apt" "$golden/rpm"

echo "==> apt-ftparchive (debian:bookworm)"
docker run --rm \
  -v "$packages/deb:/packages:ro" -v "$golden/apt:/out" \
  debian:bookworm bash -euo pipefail -c '
apt-get update -qq
apt-get install -y -qq apt-utils >/dev/null 2>&1

# apt-ftparchive records Filename relative to its working directory, so the
# pool is laid out exactly as the live repository has it and the tool is run
# from the repository root.
mkdir -p /work/pool/amd64/main /work/pool/arm64/main
cd /work

cp /packages/indexer-fixture_1.0.0-1_amd64.deb pool/amd64/main/
cp /packages/indexer-fixture_1.1.0-1_amd64.deb pool/amd64/main/
cp /packages/indexer-other_2.5.0-1_amd64.deb   pool/amd64/main/
cp /packages/indexer-fixture_1.0.0-1_arm64.deb pool/arm64/main/

apt-ftparchive packages pool/amd64/main > /out/Packages.amd64
apt-ftparchive packages pool/arm64/main > /out/Packages.arm64

# A Release file over the generated indexes, for comparison against ours.
mkdir -p dists/noble/main/binary-amd64 dists/noble/main/binary-arm64
cp /out/Packages.amd64 dists/noble/main/binary-amd64/Packages
cp /out/Packages.arm64 dists/noble/main/binary-arm64/Packages
apt-ftparchive \
  -o APT::FTPArchive::Release::Origin=HashiCorp \
  -o APT::FTPArchive::Release::Label=HashiCorp \
  -o APT::FTPArchive::Release::Suite=noble \
  -o APT::FTPArchive::Release::Codename=noble \
  -o APT::FTPArchive::Release::Components=main \
  -o APT::FTPArchive::Release::Architectures="amd64 arm64" \
  release dists/noble > /out/Release
'

echo "==> createrepo_c (rockylinux:9)"
docker run --rm \
  -v "$packages/rpm:/packages:ro" -v "$golden/rpm:/out" \
  rockylinux:9 bash -euo pipefail -c '
dnf install -y -q createrepo_c >/dev/null 2>&1

# One self-contained tree per arch, matching the live yum layout: RPMs flat at
# the root with repodata/ alongside.
for arch in x86_64 aarch64; do
  mkdir -p /work/$arch
  cp /packages/*.$arch.rpm /work/$arch/
  # --no-database matches the live repository, whose repomd.xml lists only
  # primary, filelists and other; modern dnf reads the XML and never the
  # sqlite mirrors.
  createrepo_c --quiet --checksum sha256 --unique-md-filenames --no-database /work/$arch

  mkdir -p /out/$arch
  cp -r /work/$arch/repodata /out/$arch/
done

# Decompress the metadata so the goldens are reviewable in a diff, keeping the
# compressed originals for the repomd checksum assertions.
for arch in x86_64 aarch64; do
  for f in /out/$arch/repodata/*.xml.gz; do
    gunzip -c "$f" > "${f%.gz}"
  done
done
'

echo
echo "==> goldens written to $golden"
find "$golden" -type f | sort
