#!/usr/bin/env bash
# Refuses a release tag that isn't on the branch its channel publishes
# from (Q66, doc 12 §6). Run only from release.yml, before any build or
# signing step, against a checkout with origin/main and origin/beta
# fetched.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/release/lib.sh
source "$script_dir/lib.sh"

usage() {
  echo "usage: $0 <git-tag>" >&2
  exit 1
}

[ $# -eq 1 ] || usage
tag="$1"

git fetch --quiet origin main beta
hoserva_verify_tag_ancestry "$tag"
