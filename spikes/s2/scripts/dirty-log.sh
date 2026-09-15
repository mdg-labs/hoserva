#!/usr/bin/env bash
# Characterizes the case build-fixture.sh's disks don't cover: a disk
# pulled from an array that was *not* cleanly stopped (crash, power loss,
# a forced pull) and so has a dirty XFS log. Never one of the three fixture
# disks check-fixture.sh verifies — its own throwaway disk, deliberately
# crashed with `xfs_io -x -c shutdown` (the standard, safe way to simulate
# an unclean shutdown of a still-mounted XFS filesystem for testing — the
# same mechanism xfstests itself uses — without touching a real device: it
# forces the kernel to abort the filesystem in place, leaving genuine
# on-disk log/metadata state, not injected/fabricated corruption).
#
# One crashed image is built once, then copied fresh before *each* mount
# variant is tried — a plain `-o ro` mount was found (below) to itself
# replay the dirty log via a write, which would otherwise silently "fix"
# the disk before the next variant ever saw the original dirty state.
# Comparing variants against a shared, already-mutated image would have
# been exactly the kind of silently-broken control S1 warns against.
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

pristine_img="$S2/dirty-pristine.img"
[[ -e "$pristine_img" ]] && die "refusing to recreate existing image: $pristine_img"

build_img="$S2_IMG/dirty-build.img"
truncate -s 320M "$build_img"
dev=$(s2_partition_disk "$build_img")
mkfs.xfs -q -f "$dev"

mnt="$S2_MNT/dirty"
mkdir -p -- "$mnt"
mount -t xfs -o rw "$dev" "$mnt"
echo "before-crash" > "$mnt/marker"
dd if=/dev/urandom of="$mnt/synced.bin" bs=4096 count=8 status=none
sync
# Left deliberately unsynced: something genuinely in flight at crash time,
# not just a filesystem that merely lacks a clean-unmount record.
dd if=/dev/urandom of="$mnt/unsynced.bin" bs=4096 count=8 status=none

echo "== forcing an XFS shutdown (xfs_io -x -c shutdown), simulating an unclean crash =="
# The array-stopped-cleanly case is what build-fixture.sh's disks already
# are; this is the other case Unraid itself distinguishes (docs.unraid.net:
# stopping the array unmounts every storage device) — a disk pulled, or a
# box that lost power, while the array was still started.
xfs_io -x -c "shutdown" "$mnt" || true
sleep 1
umount "$mnt" 2>/dev/null || umount -f "$mnt" 2>/dev/null || umount -l "$mnt"
rmdir "$mnt" 2>/dev/null || true
losetup -d "$dev"

part_size_bytes=$(($(sfdisk -d "$build_img" | sed -n 's/.*dirty-build\.img1 : start=\s*64, size=\s*\([0-9]\+\).*/\1/p') * 512))
cp "$build_img" "$pristine_img"
rm -f "$build_img"

fresh_copy() {
  local variant=$1
  local img="$S2_IMG/dirty-$variant.img"
  cp "$pristine_img" "$img"
  echo "$img"
}

echo
echo "== xfs_repair -n on the crashed disk, dry-run only, before any mount =="
img=$(fresh_copy "repaircheck")
dev=$(losetup --find --show --offset 32768 --sizelimit "$part_size_bytes" "$img")
set +e
repair_out=$(xfs_repair -n "$dev" 2>&1)
repair_rc=$?
set -e
echo "$repair_out"
echo "xfs_repair -n exit status on a dirty (crashed) log: $repair_rc"
losetup -d "$dev"
rm -f "$img"
[[ "$repair_rc" -ne 0 ]] || die "expected xfs_repair -n to flag the dirty log with a non-zero exit; got 0 — the crash may not have produced a dirty log this run"

try_mount() {
  local variant=$1 opts=$2
  echo
  echo "== mount -o $opts against a *fresh copy* of the dirty-log disk (variant: $variant) =="
  local img dev before after adopt=$S2_ADOPT/dirty
  local before_copy="$S2/dirty-$variant-before.raw" after_copy="$S2/dirty-$variant-after.raw"
  rm -f "$before_copy" "$after_copy"
  img=$(fresh_copy "$variant")
  dev=$(losetup --find --show --offset 32768 --sizelimit "$part_size_bytes" "$img")
  local log_start_byte log_length_bytes sectsize
  read -r log_start_byte log_length_bytes sectsize <<<"$(s2_xfs_log_geometry "$dev")"
  s2_snapshot_bytes "$dev" "$before_copy"
  before=$(sha256sum "$before_copy" | awk '{print $1}')
  mkdir -p -- "$adopt"
  local rc=0
  mount -t xfs -o "$opts" "$dev" "$adopt" 2>"$S2/dirty-$variant.err" || rc=$?
  echo "mount exit status: $rc"
  cat "$S2/dirty-$variant.err" 2>/dev/null || true
  if [[ "$rc" -eq 0 ]]; then
    echo "contents:"
    ls -la "$adopt"
    umount "$adopt"
  fi
  rmdir "$adopt" 2>/dev/null || true
  s2_snapshot_bytes "$dev" "$after_copy"
  after=$(sha256sum "$after_copy" | awk '{print $1}')
  if [[ "$before" == "$after" ]]; then
    echo "partition bytes unchanged by this mount ($before)"
  else
    echo "partition bytes CHANGED by this mount ($before -> $after) — this mount wrote to the device despite being requested read-only"
    local ranges_file="$S2/dirty-$variant-diff-ranges.txt" byte_count
    byte_count=$(s2_byte_diff_ranges "$before_copy" "$after_copy" "$ranges_file")
    echo "byte diff: cmp -l found $byte_count differing byte(s), in these ranges (0-indexed, half-open):"
    s2_classify_ranges "$sectsize" "$log_start_byte" "$log_length_bytes" < "$ranges_file"
    rm -f "$ranges_file"
  fi
  rm -f "$before_copy" "$after_copy"
  echo "-- xfs_repair -n immediately after this mount attempt --"
  local ro2 rc2
  set +e
  ro2=$(xfs_repair -n "$dev" 2>&1)
  rc2=$?
  set -e
  echo "$ro2"
  echo "xfs_repair -n exit status: $rc2"
  losetup -d "$dev"
  rm -f "$img"
}

try_mount "plain-ro" "ro"
try_mount "ro-norecovery" "ro,norecovery"

rm -f "$pristine_img"
echo
echo "dirty-log.sh: done — see doc 08 S2 for what this means for adoption's mount-option choice on a disk with a dirty log"
