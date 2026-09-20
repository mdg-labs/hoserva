#!/usr/bin/env bash
# Compare two release-signing public-key sources (PEM or pubkey.go) and
# print only "match" or "mismatch". Used by ci.yml and release.yml (issue
# #205) — never logs PEM, hex key bytes, or openssl pkey -text output.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/release/lib.sh
source "$script_dir/lib.sh"

usage() {
  echo "usage: $0 <source-a> <source-b>" >&2
  echo "  Each source is a PEM file (public or private) or a pubkey.go file." >&2
  exit 2
}

[ $# -eq 2 ] || usage

if hoserva_compare_release_pubkeys "$1" "$2"; then
  exit 0
fi
exit 1
