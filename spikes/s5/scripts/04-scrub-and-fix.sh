#!/usr/bin/env bash
# Step 4: scrub (snapraid.txt §5.7/§4.1), undeleting (§4.3) and silent-
# corruption detection+repair (§4.1's "fix -e" cycle), against the array
# 01-03 left synced.
#
# The corruption itself is injected with a physical-offset-targeted `dd`
# directly on the data disk's loop device, not the blind
# `bs=1M seek=100 count=10` example in doc 06 §3: on this run's 2 GiB data
# disks that blind recipe risks landing on XFS metadata (superblock, AG
# headers, inode btrees) rather than inside a real file's data, which is a
# filesystem-corruption test, not the silent-bit-rot test SnapRAID's scrub
# is actually for. `xfs_bmap -v` gives the target file's exact physical
# block range so the corruption always lands inside that one file's own
# data (see doc 08 for the measured finding and the doc 06 §3 update this
# motivates).
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

[[ -f "$CONF" ]] || die "run 01-seed-and-sync.sh, 02-diff-and-sync.sh and 03-touch-and-copy.sh first"

echo "### control: sha256 actually detects a single changed byte (not just asserted) ###"
probe=$(mktemp)
probe_flipped=$(mktemp)
head -c 65536 /dev/urandom > "$probe"
cp "$probe" "$probe_flipped"
printf '\xFF' | dd of="$probe_flipped" bs=1 seek=32768 count=1 conv=notrunc status=none
a=$(sha256sum "$probe" | awk '{print $1}')
b=$(sha256sum "$probe_flipped" | awk '{print $1}')
rm -f "$probe" "$probe_flipped"
[[ "$a" != "$b" ]] || die "control failed: flipping one byte did not change the sha256 — the comparison mechanism this spike relies on elsewhere would be worthless"
echo "control OK: single-byte flip changed sha256 ($a != $b)"

echo "### baseline: full scrub on a clean, synced array (-p 100 -o 0 forces literally everything, ignoring the 10-day recency default) ###"
s5_snapraid -p 100 -o 0 scrub
s5_snapraid_log "$S5/status-before-corruption.log" status
grep -qF "No error detected." "$S5/status-before-corruption.log" || die "expected a clean status before any corruption was injected — the negative control that scrub can report a genuinely uncorrupted array as clean"

echo "### undeleting (snapraid.txt §4.3): delete a tracked file, fix -f to restore it ###"
target="$LAB/mnt/disk2/documents/tax-2025.bin"
before_hash=$(sha256sum "$target" | awk '{print $1}')
rm -f "$target"
[[ ! -e "$target" ]] || die "delete did not remove $target"
s5_snapraid fix -f /documents/tax-2025.bin
[[ -e "$target" ]] || die "fix -f did not restore $target"
after_hash=$(sha256sum "$target" | awk '{print $1}')
[[ "$before_hash" == "$after_hash" ]] || die "restored file's sha256 ($after_hash) does not match the pre-delete hash ($before_hash)"
echo "undelete OK: $target restored with matching sha256 ($after_hash)"

echo "### corruption injection, targeted via xfs_bmap ###"
corrupt_file="$LAB/mnt/disk3/backup/weekly.bin"
orig_hash=$(sha256sum "$corrupt_file" | awk '{print $1}')
echo "target: $corrupt_file (sha256 $orig_hash)"
xfs_bmap -v "$corrupt_file" | tee "$S5/xfs_bmap-corrupt-target.log"
start_block=$(sed -n 's/^ *0: *\[[0-9]*\.\.[0-9]*\]: *\([0-9]*\)\.\..*/\1/p' "$S5/xfs_bmap-corrupt-target.log")
[[ "$start_block" =~ ^[0-9]+$ ]] || die "could not parse xfs_bmap's starting block (extent 0) from: $(cat "$S5/xfs_bmap-corrupt-target.log")"
# xfs_bmap's default BLOCK-RANGE unit is 512-byte basic blocks (this fs's
# sectsz, confirmed via xfs_info) — offset 500000 bytes into the file's own
# extent, well clear of both ends of the 2,203,648-byte allocated range for
# a 2,200,000-byte file, so only file data is touched, never metadata.
start_byte=$((start_block * 512))
corrupt_offset=$((start_byte + 500000))
echo "device byte offset: $corrupt_offset"

dev=$(findmnt -n -o SOURCE --target "$LAB/mnt/disk3")
img="$LAB/img/disk3.img"
lab_assert_own_loop "$dev" "$img"

sync
# Unmount before corrupting and remount after: writing under a live mount
# risks the kernel serving the old, still-cached page instead of the
# corrupted on-disk bytes on the very next read, which would make this
# whole step a false pass. Unmount forces the page cache for this
# filesystem to be dropped; the remount's first read is guaranteed to come
# from the (now corrupted) device.
umount "$LAB/mnt/disk3"
lab_assert_own_loop "$dev" "$img"
dd if=/dev/urandom of="$dev" bs=1 seek="$corrupt_offset" count=500000 conv=notrunc status=none
mount "$dev" "$LAB/mnt/disk3"

corrupt_hash=$(sha256sum "$corrupt_file" | awk '{print $1}')
[[ "$corrupt_hash" != "$orig_hash" ]] || die "corruption injection had no effect: sha256 unchanged after the dd — wrong offset, or the mount served a cached page"
echo "corrupted: sha256 now $corrupt_hash (was $orig_hash)"

echo "### scrub detects the corruption ###"
set +e
s5_snapraid -p 100 -o 0 scrub > "$S5/scrub-after-corruption.log" 2>&1
scrub_rc=$?
set -e
cat "$S5/scrub-after-corruption.log"
echo "[scrub exit $scrub_rc]"
[[ "$scrub_rc" -eq 1 ]] || die "expected scrub to exit 1 (data errors found), got $scrub_rc"
grep -qF "data errors" "$S5/scrub-after-corruption.log" || die "expected a 'N data errors' line in scrub's output"
grep -qF "backup/weekly.bin" "$S5/scrub-after-corruption.log" || die "expected scrub to name the corrupted file"

s5_snapraid_log "$S5/status-after-corruption.log" status
grep -qF "DANGER! In the array there are" "$S5/status-after-corruption.log" || die "expected status to report the bad blocks after scrub found them"

echo "### fix -e repairs the corrupted blocks ###"
s5_snapraid -e fix

repaired_hash=$(sha256sum "$corrupt_file" | awk '{print $1}')
[[ "$repaired_hash" == "$orig_hash" ]] || die "repaired file's sha256 ($repaired_hash) does not match the pre-corruption hash ($orig_hash)"
echo "repair OK: $corrupt_file back to sha256 $repaired_hash"

echo "### rescrub the previously-bad blocks to clear the bad mark (snapraid.txt §5.7: 'scrub -p bad') ###"
s5_snapraid -p bad scrub
s5_snapraid_log "$S5/status-after-fix.log" status
grep -qF "No error detected." "$S5/status-after-fix.log" || die "expected a clean status after fix + rescrub"

s5_array_manifest disk1 disk2 disk3 > "$S5/manifest-after-scrub-fix.sha256"
echo "manifest: $S5/manifest-after-scrub-fix.sha256 ($(wc -l < "$S5/manifest-after-scrub-fix.sha256") files)"
