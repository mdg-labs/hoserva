#!/usr/bin/env bash
# Refuses a stable release tag whose commit has no docs version snapshot
# (doc 13 Q90). Run only from release.yml, before any build or signing
# step, against the tag's own checkout.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/release/lib.sh
source "$script_dir/lib.sh"

usage() {
  echo "usage: $0 <git-tag> [repo-root]" >&2
  exit 1
}

if [ $# -lt 1 ] || [ $# -gt 2 ]; then usage; fi
tag="$1"
root="${2:-.}"

hoserva_verify_docs_snapshot "$tag" "$root"
