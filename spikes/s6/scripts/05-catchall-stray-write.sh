#!/usr/bin/env bash
# Step 5 (catch-all) — a write to a brand-new top-level directory that has
# no per-share mount of its own must land on a data disk through the
# catch-all's own default policy, never on the boot device and never
# outside the pool. Uses a name ("stray") that is deliberately not one of
# the twelve shares 02-topology-up.sh mounted.
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

OUT=${S6_OUT:-$LAB/s6-results}
mkdir -p -- "$OUT"

mountpoint -q -- "$LAB/mnt/user" || die "catch-all is not mounted — run 01-ordering-shadow-mount.sh first"
mountpoint -q -- "$LAB/mnt/user/stray" && die "'stray' is unexpectedly already a mounted share — pick a different name"

mkdir -p -- "$LAB/mnt/user/stray"
dd if=/dev/urandom of="$LAB/mnt/user/stray/file.bin" bs=1M count=2 status=none

s6_log "== where did the stray top-level write actually land? =="
s6_locate "stray/file.bin" "$LAB/mnt/disk1" "$LAB/mnt/disk2" "$LAB/mnt/disk3" "$LAB/mnt/cache" | tee "$OUT/catchall-stray-location.log"

on_a_data_disk=0
for d in disk1 disk2 disk3; do
  [[ -e "$LAB/mnt/$d/stray/file.bin" ]] && on_a_data_disk=1
done
[[ "$on_a_data_disk" -eq 1 ]] || die "stray/file.bin is not on any data disk"
[[ -e "$LAB/mnt/cache/stray/file.bin" ]] && die "stray/file.bin landed on cache — the catch-all's own branch list is disk1..3 only, cache is not one of them"

sha_via_catchall=$(sha256sum "$LAB/mnt/user/stray/file.bin" | awk '{print $1}')
for d in disk1 disk2 disk3; do
  [[ -e "$LAB/mnt/$d/stray/file.bin" ]] || continue
  sha_direct=$(sha256sum "$LAB/mnt/$d/stray/file.bin" | awk '{print $1}')
  [[ "$sha_direct" == "$sha_via_catchall" ]] || die "stray/file.bin on $d has a different sha256 than the view through the catch-all"
done
s6_log "confirmed: stray/file.bin, written to a top-level path with no per-share mount, landed on a data disk (not cache) through the catch-all's own default policy, byte-identical (sha256 $sha_via_catchall)"
