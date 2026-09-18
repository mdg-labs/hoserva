#!/usr/bin/env bash
# Builds one architecture's .deb from a release tag (Q66, Q7, D8, D9).
# Needs dpkg-buildpackage, debhelper and fakeroot — CI installs them; the
# dev host and lab deliberately do not have them (CLAUDE.md: no
# packaging tool here ever touches a real disk, and this one doesn't
# need to — dpkg-buildpackage only ever writes to its own build tree and
# the output directory given below).
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"
# shellcheck source=scripts/release/lib.sh
source "$script_dir/lib.sh"

usage() {
  echo "usage: $0 <git-tag> <amd64|arm64> <output-dir>" >&2
  exit 1
}

[ $# -eq 3 ] || usage
tag="$1"
arch="$2"
out_dir="$3"

case "$arch" in
  amd64 | arm64) ;;
  *)
    echo "build-deb.sh: unsupported architecture '$arch' (want amd64 or arm64)" >&2
    exit 1
    ;;
esac

channel="$(hoserva_channel_from_tag "$tag")"
version="$(hoserva_debian_version_from_tag "$tag")"

cd "$repo_root"

# debian/rules and friends assume a top-level debian/ (dpkg-buildpackage
# convention); this repo keeps it under packaging/ instead (area:packaging,
# CLAUDE.md). The symlink exists only for the duration of this build.
ln -sfn packaging/debian debian
cleanup() { rm -f debian; }
trap cleanup EXIT

# dpkg parses only debian/changelog's topmost entry for the version it
# builds — the git tag is this project's one source of truth for that
# version (doc 12 §6), so it is written here immediately before the
# build, never hand-maintained.
cat >packaging/debian/changelog <<CHANGELOG
hoserva (${version}) unstable; urgency=medium

  * ${channel} release ${tag}: https://github.com/mdg-labs/hoserva/releases/tag/${tag}

 -- Hoserva release automation <releases@hoserva.dev>  $(date -R)
CHANGELOG

dpkg-buildpackage -a"$arch" -us -uc -b

mkdir -p "$out_dir"
mv "../hoserva_${version}_${arch}.deb" "$out_dir/"
echo "build-deb.sh: built $out_dir/hoserva_${version}_${arch}.deb"
