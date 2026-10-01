#!/usr/bin/env bash
# Fetches the curated catalog archive named by scripts/devenv/catalog.pin
# from the immutable GitHub Release `serial-<serial>` of
# mdg-labs/hoserva-catalog and places it, with its detached signature, where
# internal/template embeds it (Q65, doc 04 §7). Run by `make catalog-snapshot`,
# which `make build`, `make test-go` and the .deb build run first.
#
# The archive is accepted only if its SHA-256 equals the pin and its
# signature verifies against the compiled-in catalog key (catalog-verify);
# anything else fails the script and leaves nothing in the embed directory.
# A cached copy that already matches the pin is kept and not downloaded again.
# The installations' refresh URL (catalog.hoserva.dev) is never used here.
#
# CATALOG_PIN_FILE and CATALOG_SNAPSHOT_DIR exist for test-catalog-snapshot.sh.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "$script_dir/../.." && pwd)"
pin="${CATALOG_PIN_FILE:-$script_dir/catalog.pin}"
dest="${CATALOG_SNAPSHOT_DIR:-$repo_root/internal/template/snapshot}"
archive="$dest/catalog.tar.zst"
sigfile="$dest/catalog.tar.zst.sig"

die() {
  echo "catalog-snapshot: $*" >&2
  exit 1
}

pin_value() {
  awk -F= -v key="$1" '$1 == key { n++; v = $2 } END { if (n != 1) exit 1; print v }' "$pin"
}

serial="$(pin_value serial)" || die "$pin must name exactly one serial="
sha256="$(pin_value sha256)" || die "$pin must name exactly one sha256="
[[ "$serial" =~ ^[0-9]+$ ]] || die "the pinned serial '$serial' is not a number"
[[ "$sha256" =~ ^[0-9a-f]{64}$ ]] || die "the pinned sha256 is not 64 lowercase hex digits"

sha256_of() {
  sha256sum -- "$1" | awk '{ print $1 }'
}

verify() {
  (cd "$repo_root" && "${GO:-go}" run ./scripts/devenv/catalog-verify "$1" "$2" "$serial")
}

tmp="$(mktemp -d)"
ok=0
# Unless the script succeeded the embed directory holds neither file — not
# even a cached copy that no longer matches the pin, which the build would
# embed.
cleanup() {
  rm -rf -- "$tmp"
  if [ "$ok" != 1 ]; then
    rm -f -- "$archive" "$sigfile"
  fi
}
trap cleanup EXIT

if [ -f "$archive" ] && [ -f "$sigfile" ] && [ "$(sha256_of "$archive")" = "$sha256" ] && verify "$archive" "$sigfile"; then
  echo "catalog-snapshot: serial $serial is already in place"
  ok=1
  exit 0
fi

mkdir -p -- "$dest"

base="https://github.com/mdg-labs/hoserva-catalog/releases/download/serial-$serial"
fetch() {
  curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
    --max-time 120 --retry 2 --output "$2" "$1"
}
echo "catalog-snapshot: fetching serial $serial from $base"
fetch "$base/catalog.tar.zst" "$tmp/catalog.tar.zst" || die "downloading catalog.tar.zst failed"
fetch "$base/catalog.tar.zst.sig" "$tmp/catalog.tar.zst.sig" || die "downloading catalog.tar.zst.sig failed"

got="$(sha256_of "$tmp/catalog.tar.zst")"
[ "$got" = "$sha256" ] || die "the downloaded archive's SHA-256 is $got, the pin names $sha256"
verify "$tmp/catalog.tar.zst" "$tmp/catalog.tar.zst.sig" || die "the downloaded archive does not verify against the catalog key"

mv -- "$tmp/catalog.tar.zst.sig" "$sigfile"
mv -- "$tmp/catalog.tar.zst" "$archive"
ok=1
echo "catalog-snapshot: serial $serial verified and placed in $dest"
