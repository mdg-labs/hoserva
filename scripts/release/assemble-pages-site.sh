#!/usr/bin/env bash
# Assembles the GitHub Pages artifact for hoserva.dev (Q66, Q3, Q65):
# the site build (the root page, the app list at /apps, with the docs under
# /docs/; a placeholder until site/ exists) and the release index under
# /releases/. /apps is built from the signed catalog archive, so it is a
# different path from the /catalog/ wiped below, and CATALOG_LIVE_URL in the
# environment reaches `make site-build`, which exports the live archive when it
# verifies and the pinned snapshot otherwise. The catalog is published from its own
# repository at catalog.hoserva.dev (Q66, Q65), so this site has no
# /catalog/ path and strips one if a docs tree tries to add it. Apt is
# not part of this origin and is stripped too.
#
# Permanent URL — compiled into hoservad, never changes:
#   https://hoserva.dev/releases/index.json
#
# usage: assemble-pages-site.sh <out-dir> <entries-dir> [docs-dir]
#
# If docs-dir is omitted and the repository has a site/ project, `make
# site-build` builds it and site/dist is used; the build output is never
# taken from an earlier run. A failed build, or a build with no
# site/dist/index.html, stops the assembly rather than publishing a
# placeholder over the site. Without a site/ project the placeholder page
# that points at the GitHub repository is used.
#
# HOSERVA_PAGES_MAX_BYTES (default 900 MiB) is the fail-before-deploy
# canary: 90% of GitHub Pages' 1 GB published-site cap, measured on this
# artifact only.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"

usage() {
  echo "usage: $0 <out-dir> <entries-dir> [docs-dir]" >&2
  exit 1
}

[ $# -ge 2 ] && [ $# -le 3 ] || usage
out="$1"
entries_dir="$2"
docs_dir="${3:-}"

mkdir -p "$out"
out="$(cd "$out" && pwd)"
if [ "$out" = / ]; then
  echo "assemble-pages-site.sh: refusing to write to /" >&2
  exit 1
fi

if [ -z "$docs_dir" ] && [ -f "$repo_root/site/package.json" ]; then
  if ! make -C "$repo_root" site-build >&2; then
    echo "assemble-pages-site.sh: 'make site-build' failed — refusing to publish without the docs" >&2
    exit 1
  fi
  if [ ! -f "$repo_root/site/dist/index.html" ]; then
    echo "assemble-pages-site.sh: 'make site-build' left no site/dist/index.html — refusing to publish without the docs" >&2
    exit 1
  fi
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

# /releases/ is generated here, and /catalog/ lives at catalog.hoserva.dev.
# Wipe anything the docs root (or a docs build) may have
# emitted under those names, and under /apt/ (#118).
rm -rf "$out/catalog" "$out/releases" "$out/apt"

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
