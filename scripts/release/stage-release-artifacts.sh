#!/usr/bin/env bash
# Checks the artifact release.yml's signing job downloaded and copies the
# two .deb files into an empty <staging-dir> for publish-release.sh.
# <download-dir> must hold exactly hoserva_<version>_amd64.deb,
# hoserva_<version>_arm64.deb and hoserva-debs.sha256 (as written by
# scripts/release/collect-build-output.sh), all regular files, and the
# sums must list exactly those two packages and match them. Anything
# missing, extra or different is refused: an artifact download that
# yielded nothing must not become an empty release.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/release/lib.sh
source "$script_dir/lib.sh"

usage() {
  echo "usage: $0 <git-tag> <download-dir> <staging-dir>" >&2
  exit 1
}

die() {
  echo "stage-release-artifacts.sh: $*" >&2
  exit 1
}

[ $# -eq 3 ] || usage
tag="$1"
download_dir="$2"
staging_dir="$3"

hoserva_channel_from_tag "$tag" >/dev/null
version="$(hoserva_debian_version_from_tag "$tag")"
debs=("hoserva_${version}_amd64.deb" "hoserva_${version}_arm64.deb")
sums=hoserva-debs.sha256

[ -d "$download_dir" ] || die "$download_dir is not a directory"

expected="$(printf '%s\n' "${debs[@]}" "$sums" | LC_ALL=C sort)"
found="$(cd "$download_dir" && find . -mindepth 1 -printf '%P\n' | LC_ALL=C sort)"
[ "$found" = "$expected" ] || die "$download_dir does not hold exactly the expected files (found: ${found:-nothing})"

for f in "${debs[@]}" "$sums"; do
  if [ ! -f "$download_dir/$f" ] || [ -L "$download_dir/$f" ]; then
    die "$download_dir/$f is not a regular file"
  fi
done
for f in "${debs[@]}"; do
  [ -s "$download_dir/$f" ] || die "$download_dir/$f is empty"
done

listed="$(awk '{print $2}' "$download_dir/$sums" | LC_ALL=C sort)"
want_listed="$(printf '%s\n' "${debs[@]}" | LC_ALL=C sort)"
[ "$listed" = "$want_listed" ] || die "$sums does not list exactly the two packages"
(cd "$download_dir" && sha256sum --check --strict --quiet "$sums") || die "a package does not match $sums"

mkdir -p "$staging_dir"
if [ -n "$(ls -A "$staging_dir")" ]; then
  die "$staging_dir is not empty"
fi
for f in "${debs[@]}"; do
  cp -- "$download_dir/$f" "$staging_dir/$f"
done
