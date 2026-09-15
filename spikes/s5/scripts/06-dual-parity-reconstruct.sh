#!/usr/bin/env bash
# Step 6: add a second parity disk (snapraid.txt §7.2, "2-parity ... enables
# RAID6 dual parity"), sync it in, then fail *two* data disks at once
# (d1 and d3) and reconstruct both — the real test of dual parity, since a
# single-disk failure recovers identically under 1-parity or 2-parity.
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

[[ -f "$S5/manifest-after-d2-reconstruction.sha256" ]] || die "run 01 through 05 first"

echo "### create the second parity disk (same size as parity1 — snapraid.txt §7.1: parity space must be >= the biggest data disk; matching parity1 keeps this simple) ###"
s5_create_disk parity2 3G

cat >> "$CONF" <<EOF
2-parity $LAB/mnt/parity2/snapraid.parity
content $LAB/mnt/parity2/snapraid.content
EOF
echo "updated $CONF:"
cat "$CONF"
# parity disks used (2) + 1 = 3 content copies required (snapraid.txt §7.4);
# we now have parity1, cache and parity2 — three distinct physical devices,
# none of them a data disk.

echo "### sync to build the second parity level over the existing data ###"
# A first attempt used a plain 'sync' here and it refused: "WARNING! The
# 2-Parity parity has only 0 blocks instead of 23. DANGER! ... If instead
# you are adding a new parity level, you can 'sync' using
# 'snapraid --force-full sync' to force a full rebuild of the parity." —
# exactly the case this step is in (data already existed before 2-parity
# was added), so --force-full is the documented, expected answer, not a
# workaround.
s5_snapraid --force-full sync

echo "### post-sync diff (expect: everything equal) ###"
s5_snapraid diff

pre_manifest="$S5/manifest-before-dual-failure.sha256"
s5_array_manifest disk1 disk2 disk3 > "$pre_manifest"
echo "pre-failure manifest: $pre_manifest ($(wc -l < "$pre_manifest") files)"

echo "### detach BOTH d1 and d3 — the case single parity cannot recover from ###"
s5_detach_disk disk1
s5_detach_disk disk3

replace_empty() {  # name
  local name=$1
  local img="$LAB/img/$name.img"
  rm -f "$img"
  truncate -s 2G "$img"
  local dev
  dev=$(losetup --find --show "$img")
  lab_assert_own_loop "$dev" "$img"
  mkfs.xfs -q -L "$name" "$dev"
  mount "$dev" "$LAB/mnt/$name"
  echo "replacement $name attached: $dev, mounted at $LAB/mnt/$name, empty ($(find "$LAB/mnt/$name" -type f | wc -l) files)"
}
replace_empty disk1
replace_empty disk3

echo "### snapraid.txt §4.4 step 2: fix -d d1 -d d3 (multiple -d options for multiple failed disks) ###"
set +e
s5_snapraid -d d1 -d d3 -l "$S5/fix-dual.log" fix
fix_rc=$?
set -e
echo "[fix -d d1 -d d3 exit $fix_rc]"
[[ "$fix_rc" -eq 0 ]] || die "expected 'fix -d d1 -d d3' to exit 0, got $fix_rc — see $S5/fix-dual.log"
unrecoverable=$(sed -n 's/^summary:error_unrecoverable:\([0-9]*\)$/\1/p' "$S5/fix-dual.log")
[[ "$unrecoverable" =~ ^[0-9]+$ ]] || die "could not find a summary:error_unrecoverable: line in $S5/fix-dual.log"
[[ "$unrecoverable" -eq 0 ]] || die "fix -d d1 -d d3 reports $unrecoverable unrecoverable block(s) — dual-parity reconstruction was not clean"

echo "### snapraid.txt §4.4 step 3: check -d d1 -d d3 -a ###"
s5_snapraid -d d1 -d d3 -a check

echo "### snapraid.txt §4.4 step 4: sync ###"
s5_snapraid sync

echo "### verify: every file's sha256 matches the pre-failure manifest ###"
post_manifest="$S5/manifest-after-dual-reconstruction.sha256"
s5_array_manifest disk1 disk2 disk3 > "$post_manifest"
if ! diff -u "$pre_manifest" "$post_manifest" > "$S5/dual-reconstruction.diff"; then
  cat "$S5/dual-reconstruction.diff"
  die "post-reconstruction manifest does not match the pre-failure manifest — see $S5/dual-reconstruction.diff"
fi
echo "DUAL-PARITY RECONSTRUCTION OK: $post_manifest byte-for-byte (sha256) identical to $pre_manifest ($(wc -l < "$post_manifest") files), two disks (d1, d3) lost and rebuilt at once"
