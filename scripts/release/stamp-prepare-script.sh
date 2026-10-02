#!/usr/bin/env bash
# Writes the release's copy of the Unraid prepare script
# (tools/unraid/prepare-migration.sh, doc 05 §4 Phase A step 0) and its
# checksum into <out-dir>: the script with its version set to the release
# tag, as prepare-migration.sh, and prepare-migration.sh.sha256 in the
# format `sha256sum -c` reads. publish-release.sh attaches both to the
# GitHub release.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/release/lib.sh
source "$script_dir/lib.sh"

usage() {
  echo "usage: $0 <git-tag> <out-dir>" >&2
  exit 1
}

[ $# -eq 2 ] || usage
tag="$1"
out_dir="$2"

hoserva_channel_from_tag "$tag" >/dev/null

src="${HOSERVA_PREPARE_SCRIPT:-$script_dir/../../tools/unraid/prepare-migration.sh}"
[ -f "$src" ] || {
  echo "stamp-prepare-script.sh: $src does not exist" >&2
  exit 1
}

placeholder='SCRIPT_VERSION="unreleased"'
count="$(grep -c -x -F "$placeholder" "$src" || true)"
if [ "$count" != 1 ]; then
  echo "stamp-prepare-script.sh: expected exactly one '$placeholder' line in $src, found $count" >&2
  exit 1
fi

mkdir -p "$out_dir"
out="$out_dir/prepare-migration.sh"
sed "s/^SCRIPT_VERSION=\"unreleased\"\$/SCRIPT_VERSION=\"$tag\"/" "$src" >"$out"
chmod 755 "$out"

grep -q -x -F "SCRIPT_VERSION=\"$tag\"" "$out" || {
  echo "stamp-prepare-script.sh: the version was not set in $out" >&2
  exit 1
}
bash -n "$out"

(cd "$out_dir" && sha256sum prepare-migration.sh >prepare-migration.sh.sha256)
