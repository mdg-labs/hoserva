#!/usr/bin/env bash
# Builds one architecture's .deb from a release tag (Q66, Q7, D8, D9).
# Runs in CI (release.yml, ci.yml) and on the dev host for L3, where
# scripts/vm/deploy.sh builds the .deb under test (doc 06 §4, D20). Needs
# dpkg-dev (dpkg-buildpackage), debhelper and fakeroot wherever it runs.
# It never touches a real disk. dpkg-buildpackage would write the .deb,
# .buildinfo and .changes to this repository's parent directory, which
# sibling clones (orchestrate's scratch clones) share under identical
# file names, so all three go to a private mktemp directory instead and
# the script moves the .deb to the output directory given below. It
# writes and deletes nothing in the parent directory, and leaves this
# tree as it found it, on success and on failure (the EXIT trap below).
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
#
# The build also rewrites the tracked packaging/debian/changelog and
# writes hoservad, hoserva and debhelper's own state into the tree; the
# trap restores the changelog and removes exactly those named paths, so a
# local build (a dev-host L3 run, an orchestrate scratch clone) never
# leaves anything for `git status` or a later commit to sweep up.
#
# build_out holds the build's outputs outside the tree and the saved
# changelog; the trap removes it as one directory, never a glob in the
# parent.
changelog=packaging/debian/changelog
build_out="$(mktemp -d)"
saved_changelog="$build_out/changelog"
cp -- "$changelog" "$saved_changelog"
cleanup() {
  rm -f debian
  cp -- "$saved_changelog" "$changelog"
  rm -rf -- "$build_out"
  rm -rf -- hoserva hoservad \
    packaging/debian/.debhelper \
    packaging/debian/debhelper-build-stamp \
    packaging/debian/files \
    packaging/debian/hoserva.debhelper.log \
    packaging/debian/hoserva.postrm.debhelper \
    packaging/debian/hoserva.substvars \
    packaging/debian/hoserva
}
trap cleanup EXIT
ln -sfn packaging/debian debian

# dpkg parses only debian/changelog's topmost entry for the version it
# builds — the git tag is this project's one source of truth for that
# version (doc 12 §6), so it is written here immediately before the
# build, never hand-maintained.
cat >"$changelog" <<CHANGELOG
hoserva (${version}) unstable; urgency=medium

  * ${channel} release ${tag}: https://github.com/mdg-labs/hoserva/releases/tag/${tag}

 -- Hoserva release automation <releases@hoserva.dev>  $(date -R)
CHANGELOG

# --changes-file and --buildinfo-file place those two files; the .deb goes
# where packaging/debian/rules' dh_builddeb --destdir sends it, from the
# exported HOSERVA_DEB_DESTDIR. dpkg-genchanges and dpkg-genbuildinfo find
# the .deb through their -u option, so both are pointed at the same place.
base="hoserva_${version}_${arch}"
HOSERVA_DEB_DESTDIR="$build_out" dpkg-buildpackage -a"$arch" -us -uc -b \
  --changes-file="$build_out/$base.changes" \
  --buildinfo-file="$build_out/$base.buildinfo" \
  --changes-option=-u"$build_out" \
  --buildinfo-option=-u"$build_out"

mkdir -p "$out_dir"
mv "$build_out/$base.deb" "$out_dir/"
echo "build-deb.sh: built $out_dir/hoserva_${version}_${arch}.deb"
