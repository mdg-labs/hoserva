#!/usr/bin/env bash
# Downloads and caches the Debian 13 (Q4) generic cloud qcow2 image the L3
# harness boots every VM from. Cached once under the workspace's own
# .vm/cache/ (never a shared or system location, D20) and verified against
# Debian's own published SHA512SUMS before anything trusts it — a
# corrupted or truncated download must be refused, not booted.
#
# This script only ever writes under $VM_CACHE_DIR. It never touches a
# real disk, mount or system location — the image it fetches is booted
# only as a qcow2 file passed to qemu/libvirt under qemu:///session
# (create-vm.sh), never written to a block device directly.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/vm/lib.sh
source "$script_dir/lib.sh"

vm_require_id

IMAGE_URL="${HOSERVA_VM_IMAGE_URL:-https://cloud.debian.org/images/cloud/trixie/latest/debian-13-generic-amd64.qcow2}"
SUMS_URL="${HOSERVA_VM_IMAGE_SUMS_URL:-https://cloud.debian.org/images/cloud/trixie/latest/SHA512SUMS}"
IMAGE_NAME="debian-13-generic-amd64.qcow2"
CACHED="$VM_CACHE_DIR/$IMAGE_NAME"

if [[ -s "$CACHED" && -s "$CACHED.sha512" ]]; then
  echo "fetch-base-image: using cached $CACHED" >&2
else
  sums_tmp="$(mktemp)"
  tmp_img="$(mktemp "$VM_CACHE_DIR/.download.XXXXXX")"
  trap 'rm -f -- "$sums_tmp" "$tmp_img"' EXIT

  echo "fetch-base-image: fetching $SUMS_URL" >&2
  curl -fsSL --retry 3 -o "$sums_tmp" "$SUMS_URL"
  expected="$(grep -F "  $IMAGE_NAME" "$sums_tmp" | awk '{print $1}')"
  [[ -n "$expected" ]] || die "fetch-base-image: '$IMAGE_NAME' not listed in $SUMS_URL"

  echo "fetch-base-image: downloading $IMAGE_URL (this can take a few minutes)" >&2
  curl -fL --retry 3 -o "$tmp_img" "$IMAGE_URL"

  actual="$(sha512sum "$tmp_img" | awk '{print $1}')"
  [[ "$actual" == "$expected" ]] || die "fetch-base-image: checksum mismatch for $IMAGE_NAME (expected $expected, got $actual) — refusing to use a corrupted or tampered base image"

  mv -- "$tmp_img" "$CACHED"
  printf '%s\n' "$expected" > "$CACHED.sha512"
  echo "fetch-base-image: verified and cached at $CACHED" >&2
fi

echo "$CACHED"
