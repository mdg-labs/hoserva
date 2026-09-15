#!/usr/bin/env bash
# The migration-scan proxy (doc 05 §3): for each of build-fixture.sh's
# cleanly-unmounted disks, run `xfs_repair -n` before touching the
# filesystem at all, resolve its UUID (no /dev/disk/by-uuid in this
# container — resolved via `blkid`), mount it read-only by that UUID, then
# union the three read-only mounts with mergerfs and verify every file
# against the fixture's manifest.sha256 by content, not just presence or
# count (doc 06 §5 step 5).
#
# Every claim is a measured before/after comparison, never an assumption:
#
#   - Per disk, this compares a `-o ro` mount (no norecovery) against a
#     `-o ro,norecovery` mount, each bracketed by a whole-partition
#     snapshot (s2_snapshot_bytes) taken before and after. Where the two
#     snapshots differ, a real `cmp -l` (s2_byte_diff_ranges) counts and
#     locates every differing byte, and each contiguous range is classified
#     against the disk's own superblock/log geometry (s2_xfs_log_geometry,
#     s2_classify_ranges) — never asserted free-hand. `ro,norecovery`
#     changed nothing at all on every disk this run. That finding is why
#     every mount this script performs from here on uses `norecovery`.
#   - A write attempt against a read-only mount is asserted to fail, not
#     just narrated.
#   - Every per-file sha256 is checked against build-fixture.sh's manifest,
#     not just file presence or a count.
#
# A separate, container-specific pitfall found while building this: this
# lab has no udevd, so nothing invalidates the blkid cache
# (/run/blkid/blkid.tab) when a recycled /dev/loopN's backing image
# changes — a UUID that resolved correctly in one round can silently
# resolve to a *stale* value in a later round against the same disk. See
# lib.sh's `s2_fresh_blkid_cache`, called after every fresh attach.
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

MANIFEST="$S2/manifest.sha256"
[[ -f "$MANIFEST" ]] || die "no manifest at $MANIFEST — run build-fixture.sh first"

partition_size_bytes() {
  local img=$1 name=$2 size_sectors
  size_sectors=$(sfdisk -d "$img" | sed -n "s/.*${name}\.img1 : start=\s*64, size=\s*\([0-9]\+\).*/\1/p")
  [[ "$size_sectors" =~ ^[0-9]+$ ]] || die "could not read partition size for $name from sfdisk -d"
  echo $((size_sectors * 512))
}

attach() {
  local img=$1 size_bytes=$2 dev
  dev=$(losetup --find --show --offset 32768 --sizelimit "$size_bytes" "$img")
  s2_assert_own_loop "$dev" "$img"
  # See lib.sh: a recycled loop number can carry a stale blkid cache entry
  # from a previous attach of a *different* image; every fresh attach must
  # invalidate it before anything resolves or mounts this device by UUID.
  s2_fresh_blkid_cache
  echo "$dev"
}

run_xfs_repair_n() {
  local dev=$1 label=$2
  local out rc
  set +e
  out=$(xfs_repair -n "$dev" 2>&1)
  rc=$?
  set -e
  echo "-- xfs_repair -n ($label) --"
  echo "$out"
  echo "xfs_repair -n ($label) exit status: $rc"
  [[ "$rc" -eq 0 ]] || die "$dev: xfs_repair -n ($label) reported errors — refusing to adopt"
}

adopt_disk() {
  local name=$1
  local img="$S2_IMG/$name.img"
  local adopt="$S2_ADOPT/$name"
  [[ -f "$img" ]] || die "no image for $name: $img"
  local size_bytes
  size_bytes=$(partition_size_bytes "$img" "$name")

  echo "== $name =="

  local dev
  dev=$(attach "$img" "$size_bytes")
  run_xfs_repair_n "$dev" "before any mount"
  local uuid
  uuid=$(blkid -s UUID -o value "$dev")
  [[ -n "$uuid" ]] || die "$name: blkid could not resolve a UUID for $dev"
  echo "resolved UUID (blkid, no /dev/disk/by-uuid in this container): $uuid"
  # Read once, read-only, while the disk is provably untouched by this
  # script — the fixed points every byte-diff range below is classified
  # against.
  local log_start_byte log_length_bytes sectsize
  read -r log_start_byte log_length_bytes sectsize <<<"$(s2_xfs_log_geometry "$dev")"
  echo "xfs_db (read-only): internal log at byte offset $log_start_byte, length $log_length_bytes bytes; sector size $sectsize"
  losetup -d "$dev"

  mkdir -p -- "$adopt"

  # --- Round 1: plain `-o ro`, no norecovery. Control for the claim that a
  # read-only mount by itself does not guarantee byte-for-byte silence.
  local before_copy="$S2/$name-before-ro.raw" after_copy="$S2/$name-after-ro.raw"
  rm -f "$before_copy" "$after_copy"
  dev=$(attach "$img" "$size_bytes")
  local before_ro after_ro
  s2_snapshot_bytes "$dev" "$before_copy"
  before_ro=$(sha256sum "$before_copy" | awk '{print $1}')
  mount -t xfs -o ro "UUID=$uuid" "$adopt"
  echo "mounted: mount -t xfs -o ro UUID=$uuid $adopt"
  mount | grep -F " $adopt "
  local write_rc=0
  touch "$adopt/should-not-be-created" 2>/dev/null || write_rc=$?
  [[ "$write_rc" -ne 0 && ! -e "$adopt/should-not-be-created" ]] \
    || die "$name: a write to the read-only ($adopt) mount did not fail as expected"
  echo "write attempt refused, as expected (exit $write_rc)"
  # File *content* must be intact regardless of what this round does to the
  # device's own bytes elsewhere (superblock/log) — checked against the
  # manifest below, not assumed here.
  umount "$adopt"
  s2_snapshot_bytes "$dev" "$after_copy"
  after_ro=$(sha256sum "$after_copy" | awk '{print $1}')
  losetup -d "$dev"
  if [[ "$before_ro" == "$after_ro" ]]; then
    echo "plain 'ro' mount: partition bytes unchanged ($before_ro)"
  else
    echo "plain 'ro' mount: partition bytes CHANGED ($before_ro -> $after_ro) — recorded, not treated as data loss by itself (file-content check follows), but this is why adoption uses norecovery below"
    local ranges_file="$S2/$name-ro-diff-ranges.txt" byte_count
    byte_count=$(s2_byte_diff_ranges "$before_copy" "$after_copy" "$ranges_file")
    echo "plain 'ro' mount byte diff: cmp -l found $byte_count differing byte(s), in these ranges (0-indexed, half-open):"
    s2_classify_ranges "$sectsize" "$log_start_byte" "$log_length_bytes" < "$ranges_file"
    rm -f "$ranges_file"
  fi
  rm -f "$before_copy" "$after_copy"

  # --- Round 2: `-o ro,norecovery` — the mount this spike recommends for
  # adoption. xfs_repair -n runs again first, on the device as Round 1 left
  # it, so a real corruption from Round 1 (not expected, but not assumed
  # away) would still be caught before Round 2 mounts it.
  dev=$(attach "$img" "$size_bytes")
  run_xfs_repair_n "$dev" "after round 1, before round 2"
  local before_nr after_nr
  before_nr=$(sha256sum "$dev" | awk '{print $1}')
  mount -t xfs -o ro,norecovery "UUID=$uuid" "$adopt"
  echo "mounted: mount -t xfs -o ro,norecovery UUID=$uuid $adopt"
  mount | grep -F " $adopt "
  write_rc=0
  touch "$adopt/should-not-be-created" 2>/dev/null || write_rc=$?
  [[ "$write_rc" -ne 0 && ! -e "$adopt/should-not-be-created" ]] \
    || die "$name: a write to the read-only,norecovery ($adopt) mount did not fail as expected"
  echo "write attempt refused, as expected (exit $write_rc)"
  find "$adopt" -type f -exec sha256sum {} \; > /dev/null
  umount "$adopt"
  after_nr=$(sha256sum "$dev" | awk '{print $1}')
  [[ "$before_nr" == "$after_nr" ]] \
    || die "$name: partition bytes changed across a ro,norecovery mount+read ($before_nr -> $after_nr) — the read-only claim does not hold even with norecovery"
  echo "ro,norecovery mount: partition bytes unchanged across mount+read: $before_nr"

  run_xfs_repair_n "$dev" "after round 2"
  rmdir "$adopt"
  losetup -d "$dev"
}

for name in disk1 disk2 disk3; do
  adopt_disk "$name"
done

echo
echo "== union and verify (ro,norecovery, the recommended adoption mount) =="
declare -a mounted_devs=()
for name in disk1 disk2 disk3; do
  img="$S2_IMG/$name.img"
  size_bytes=$(partition_size_bytes "$img" "$name")
  dev=$(attach "$img" "$size_bytes")
  mounted_devs+=("$dev")
  mkdir -p -- "$S2_ADOPT/$name"
  mount -t xfs -o ro,norecovery "$dev" "$S2_ADOPT/$name"
done

branches=$(IFS=:; echo "$S2_ADOPT/disk1:$S2_ADOPT/disk2:$S2_ADOPT/disk3")
mergerfs -o ro,category.create=mspmfs "$branches" "$S2_POOL"
echo "mergerfs pool (read-only) mounted at $S2_POOL"

FOUND="$S2/found.sha256"
: > "$FOUND"
while IFS= read -r -d '' f; do
  rel=${f#"$S2_POOL"/}
  printf '%s  %s\n' "$(sha256sum "$f" | awk '{print $1}')" "$rel" >> "$FOUND"
done < <(find "$S2_POOL" -type f -print0)
sort -k2 -o "$FOUND" "$FOUND"

echo "expected files: $(wc -l < "$MANIFEST"); found in union: $(wc -l < "$FOUND")"
if diff -u "$MANIFEST" "$FOUND"; then
  echo "PASS: every file in the mergerfs union matches the fixture's manifest by sha256"
else
  die "manifest and union contents diverge — see diff above"
fi

fusermount -u "$S2_POOL" 2>/dev/null || umount "$S2_POOL"
for name in disk1 disk2 disk3; do
  umount "$S2_ADOPT/$name"
  rmdir "$S2_ADOPT/$name"
done
for dev in "${mounted_devs[@]}"; do
  losetup -d "$dev"
done

echo "check-fixture.sh: PASS"
