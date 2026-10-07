#!/usr/bin/env bash
# Gathers the two built .deb files from <dist-dir> into an empty <out-dir>
# together with hoserva-debs.sha256, their SHA-256 sums. release.yml's build
# job uploads <out-dir> as the one artifact the signing job reads back
# through scripts/release/stage-release-artifacts.sh.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/release/lib.sh
source "$script_dir/lib.sh"

usage() {
  echo "usage: $0 <git-tag> <dist-dir> <out-dir>" >&2
  exit 1
}

[ $# -eq 3 ] || usage
tag="$1"
dist_dir="$2"
out_dir="$3"

hoserva_channel_from_tag "$tag" >/dev/null
version="$(hoserva_debian_version_from_tag "$tag")"
debs=("hoserva_${version}_amd64.deb" "hoserva_${version}_arm64.deb")

for deb in "${debs[@]}"; do
  if [ ! -f "$dist_dir/$deb" ] || [ -L "$dist_dir/$deb" ]; then
    echo "collect-build-output.sh: missing built package $dist_dir/$deb" >&2
    exit 1
  fi
done

mkdir -p "$out_dir"
if [ -n "$(ls -A "$out_dir")" ]; then
  echo "collect-build-output.sh: $out_dir is not empty" >&2
  exit 1
fi

for deb in "${debs[@]}"; do
  cp -- "$dist_dir/$deb" "$out_dir/$deb"
done
(cd "$out_dir" && sha256sum -- "${debs[@]}" >hoserva-debs.sha256)
