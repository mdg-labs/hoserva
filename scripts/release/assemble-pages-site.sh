#!/usr/bin/env bash
# Assembles the GitHub Pages artifact for hoserva.dev (Q66, Q3, Q65):
# docs (or a placeholder until site/ exists) at the root, the catalog
# under /catalog/, and the release index under /releases/. Apt is not
# part of this origin and is stripped if a docs tree tries to add it.
#
# Permanent URLs — compiled into hoservad, never change:
#   https://hoserva.dev/releases/index.json
#   https://hoserva.dev/catalog/
#
# usage: assemble-pages-site.sh <out-dir> <entries-dir> [catalog-dir] [docs-dir]
#
# If catalog-dir is omitted or empty, a stub is written so /catalog/
# exists as its own tree and cannot be overwritten by the docs root.
# If docs-dir is omitted, site/dist is used when present, otherwise the
# placeholder page that points at the GitHub repository.
#
# HOSERVA_PAGES_MAX_BYTES (default 900 MiB) is the fail-before-deploy
# canary: 90% of GitHub Pages' 1 GB published-site cap, measured on this
# artifact only.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"

usage() {
  echo "usage: $0 <out-dir> <entries-dir> [catalog-dir] [docs-dir]" >&2
  exit 1
}

[ $# -ge 2 ] && [ $# -le 4 ] || usage
out="$1"
entries_dir="$2"
catalog_dir="${3:-}"
docs_dir="${4:-}"

mkdir -p "$out"
out="$(cd "$out" && pwd)"
if [ "$out" = / ]; then
  echo "assemble-pages-site.sh: refusing to write to /" >&2
  exit 1
fi

if [ -z "$docs_dir" ] && [ -d "$repo_root/site/dist" ]; then
  docs_dir="$repo_root/site/dist"
fi

if [ -n "$docs_dir" ]; then
  if [ ! -d "$docs_dir" ]; then
    echo "assemble-pages-site.sh: docs dir is not a directory: $docs_dir" >&2
    exit 1
  fi
  cp -a "$docs_dir"/. "$out"/
else
  cp "$script_dir/pages-placeholder.html" "$out/index.html"
fi

# Each of /catalog/ and /releases/ is its own tree. Wipe anything the
# docs root (or a future Starlight build) may have emitted under those
# names so they cannot clobber, or be clobbered by, the other two. Apt
# does not publish here (#118).
rm -rf "$out/catalog" "$out/releases" "$out/apt"

mkdir -p "$out/catalog"
if [ -n "$catalog_dir" ] && [ -d "$catalog_dir" ] && [ -n "$(find "$catalog_dir" -mindepth 1 -maxdepth 1 -print -quit)" ]; then
  cp -a "$catalog_dir"/. "$out/catalog"/
else
  cp "$script_dir/pages-catalog-stub.html" "$out/catalog/index.html"
fi

mkdir -p "$out/releases"
"$script_dir/assemble-release-index.sh" "$entries_dir" "$out/releases/index.json"

printf 'hoserva.dev\n' >"$out/CNAME"
: >"$out/.nojekyll"

if [ -e "$out/apt" ]; then
  echo "assemble-pages-site.sh: refusing to publish /apt/ on the Pages artifact" >&2
  exit 1
fi

max="${HOSERVA_PAGES_MAX_BYTES:-$((900 * 1024 * 1024))}"
size="$(du -sb "$out" | cut -f1)"
if [ "$size" -gt "$max" ]; then
  echo "assemble-pages-site.sh: Pages artifact is $size bytes; limit is $max (900 MiB canary under GitHub Pages' 1 GB cap) — refusing to deploy" >&2
  exit 1
fi
