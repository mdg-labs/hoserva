#!/usr/bin/env bash
# Downloads every GitHub Release's release-index-entry.json asset into
# <out-dir>, for assemble-release-index.sh to turn into
# https://hoserva.dev/releases/index.json (Q66).
#
# Called from pages.yml after a Release is published (so the tag that
# woke the workflow already has its asset attached) or after a docs
# push. Releases that predate this asset, or whose tag is not a
# recognised channel tag, are skipped; any other gh failure stops the
# fetch so we never publish a silently truncated index.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/release/lib.sh
source "$script_dir/lib.sh"

usage() {
  echo "usage: $0 <out-dir>" >&2
  exit 1
}

[ $# -eq 1 ] || usage
out_dir="$1"
repo="${GITHUB_REPOSITORY:-mdg-labs/hoserva}"

mkdir -p "$out_dir"

mapfile -t tags < <(
  gh release list \
    --repo "$repo" \
    --limit 10000 \
    --exclude-drafts \
    --json tagName \
    --jq '.[].tagName'
)

if [ "${#tags[@]}" -eq 0 ]; then
  exit 0
fi

i=0
for tag in "${tags[@]}"; do
  [ -n "$tag" ] || continue
  if ! hoserva_channel_from_tag "$tag" >/dev/null 2>&1; then
    continue
  fi

  assets="$(
    gh release view "$tag" \
      --repo "$repo" \
      --json assets \
      --jq '.assets[].name'
  )"
  if ! printf '%s\n' "$assets" | grep -Fxq 'release-index-entry.json'; then
    continue
  fi

  tmp="$(mktemp -d "$out_dir/tmp.XXXXXX")"
  gh release download "$tag" \
    --repo "$repo" \
    --pattern 'release-index-entry.json' \
    --dir "$tmp"
  i=$((i + 1))
  mv "$tmp/release-index-entry.json" "$out_dir/entry-$(printf '%04d' "$i").json"
  rm -rf "$tmp"
done
