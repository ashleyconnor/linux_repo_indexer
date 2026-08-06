#!/usr/bin/env bash
# Regenerates the committed test fixtures.
#
# Fixtures are built by the real packaging tools inside containers, not
# hand-assembled in Go, so the parsers are tested against packages that dpkg
# and rpm actually accept. The outputs are committed; run this only when the
# fixture definitions below change.
#
# Usage: ./testdata/generate.sh
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
out="$here/packages"

rm -rf "$out"
mkdir -p "$out/deb" "$out/rpm"

echo "==> building .deb fixtures (debian:bookworm)"
docker run --rm -v "$out/deb:/out" debian:bookworm bash -euo pipefail -c '
apt-get update -qq
apt-get install -y -qq dpkg-dev >/dev/null

build_deb() {
  local name="$1" version="$2" arch="$3" depends="$4"
  local root="/tmp/$name-$version-$arch"
  rm -rf "$root"
  mkdir -p "$root/DEBIAN" "$root/usr/bin" "$root/etc/$name" "$root/usr/share/doc/$name"

  cat > "$root/usr/bin/$name" <<EOS
#!/bin/sh
echo "$name $version"
EOS
  chmod 755 "$root/usr/bin/$name"

  echo "# $name configuration" > "$root/etc/$name/$name.conf"
  echo "$name $version fixture" > "$root/usr/share/doc/$name/README"

  # A deliberately awkward control file: a multi-line description with a
  # blank-line marker and an indented block, plus fields in a non-alphabetical
  # order, because that is what the live index actually contains.
  cat > "$root/DEBIAN/control" <<EOS
Package: $name
Version: $version
License: MPL-2.0
Vendor: Fixture
Architecture: $arch
Maintainer: Fixture Maintainer <fixtures@example.com>
Installed-Size: 32
Depends: $depends
Section: default
Priority: extra
Homepage: https://example.com/$name
Description: $name test fixture
 This package exists only to exercise the repository indexer.
 .
 It carries a multi-line description so that continuation-line
 handling is covered:
   an indented line
EOS

  cat > "$root/DEBIAN/conffiles" <<EOS
/etc/$name/$name.conf
EOS

  dpkg-deb --build --root-owner-group -Zgzip "$root" "/out/${name}_${version}_${arch}.deb" >/dev/null
  echo "    built ${name}_${version}_${arch}.deb"
}

build_deb indexer-fixture 1.0.0-1 amd64 "openssl"
build_deb indexer-fixture 1.1.0-1 amd64 "openssl (>= 3.0.0)"
build_deb indexer-fixture 1.0.0-1 arm64 "openssl"
build_deb indexer-other   2.5.0-1 amd64 "indexer-fixture (>= 1.0.0), passwd"

# The same package compressed with xz and zstd, so the parser is exercised
# against every control.tar.* variant it can meet in the wild.
root=/tmp/indexer-fixture-1.0.0-1-amd64
dpkg-deb --build --root-owner-group -Zxz   "$root" /out/indexer-fixture_1.0.0-1_amd64.xz.deb  >/dev/null
dpkg-deb --build --root-owner-group -Zzstd "$root" /out/indexer-fixture_1.0.0-1_amd64.zst.deb >/dev/null
echo "    built xz and zstd control variants"
'

# rpmbuild refuses to build for an architecture the running kernel cannot
# execute, so each target arch is built inside a container of that platform
# rather than cross-targeted from one.
build_rpms_for_arch() {
  local platform="$1" arch="$2"
  echo "==> building .rpm fixtures for $arch (rockylinux:9, $platform)"
  docker run --rm --platform "$platform" -v "$out/rpm:/out" -e "TARGET_ARCH=$arch" \
    rockylinux:9 bash -euo pipefail -c '
dnf install -y -q rpm-build >/dev/null 2>&1

mkdir -p /root/rpmbuild/{SPECS,BUILD,RPMS,SOURCES,SRPMS}

build_rpm() {
  local name="$1" version="$2" release="$3" arch="$TARGET_ARCH" requires="$4" epoch="$5"

  cat > /root/rpmbuild/SPECS/$name.spec <<EOS
Name:           $name
Version:        $version
Release:        $release
Epoch:          $epoch
Summary:        $name test fixture
License:        MPL-2.0
URL:            https://example.com/$name
Vendor:         Fixture
Packager:       Fixture Maintainer <fixtures@example.com>
Group:          Applications/System
Requires:       $requires
Provides:       $name-virtual = %{version}
AutoReqProv:    no

%description
This package exists only to exercise the repository indexer.

It carries a multi-line description so that description handling
is covered.

%install
mkdir -p %{buildroot}/usr/bin %{buildroot}/etc/$name %{buildroot}/usr/share/doc/$name
cat > %{buildroot}/usr/bin/$name <<SCRIPT
#!/bin/sh
echo "$name $version"
SCRIPT
chmod 755 %{buildroot}/usr/bin/$name
echo "# $name configuration" > %{buildroot}/etc/$name/$name.conf
echo "$name $version fixture" > %{buildroot}/usr/share/doc/$name/README

%files
/usr/bin/$name
%config(noreplace) /etc/$name/$name.conf
%dir /etc/$name
/usr/share/doc/$name/README
%ghost /var/log/$name.log

%changelog
* Mon Aug 04 2026 Fixture Maintainer <fixtures@example.com> - $version-$release
- Second changelog entry, so other.xml has more than one.

* Sun Aug 03 2026 Fixture Maintainer <fixtures@example.com> - 0.9.0-1
- Initial fixture package.
EOS

  rpmbuild -bb --quiet --define "_topdir /root/rpmbuild" --target "$arch" \
    /root/rpmbuild/SPECS/$name.spec >/dev/null
  cp /root/rpmbuild/RPMS/$arch/*.rpm /out/
  rm -f /root/rpmbuild/RPMS/$arch/*.rpm
  echo "    built $name-$version-$release.$arch.rpm"
}

build_rpm indexer-fixture 1.0.0 1 "openssl"                  0
build_rpm indexer-fixture 1.1.0 1 "openssl >= 3.0.0"         0
build_rpm indexer-other   2.5.0 1 "indexer-fixture >= 1.0.0" 2
'
}

build_rpms_for_arch linux/amd64 x86_64
build_rpms_for_arch linux/arm64 aarch64

echo
echo "==> fixtures written to $out"
ls -la "$out/deb" "$out/rpm"
