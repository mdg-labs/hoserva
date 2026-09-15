#!/usr/bin/env bash
# Positive/negative controls on the checking mechanism itself (S1's
# measurement-integrity lesson, doc 08 S1: "a check that can silently pass
# when broken needs a positive/negative control"). Three checks, each
# asserted, not narrated:
#
#   1. The manifest-vs-union sha256 diff genuinely detects a single changed
#      byte, not just a missing/extra file.
#   2. `mount -o ro,norecovery UUID=<uuid-that-does-not-exist>` genuinely
#      fails, proving UUID resolution is doing real work rather than always
#      falling through to whatever device happened to be attached.
#   3. A throwaway scratch disk (never one of the three fixture disks
#      check-fixture.sh verifies) really is an ordinary read-write-capable
#      XFS filesystem — confirming that every "read-only" claim made about
#      the fixture disks above is a mount-option choice this spike is
#      making, not a property of the image itself.
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

MANIFEST="$S2/manifest.sha256"
[[ -f "$MANIFEST" ]] || die "no manifest at $MANIFEST — run build-fixture.sh first"

echo "== control 1: a single changed byte must fail the manifest check =="
TAMPERED="$S2/tampered.sha256"
cp "$MANIFEST" "$TAMPERED"
# Flip one hex character of the first line's hash — same shape as a real
# sha256, guaranteed not to match any real file's content.
first_hash=$(head -1 "$MANIFEST" | awk '{print $1}')
bad_char=${first_hash:0:1}
[[ "$bad_char" == "0" ]] && replacement=1 || replacement=0
bad_hash="${replacement}${first_hash:1}"
sed -i "0,/^$first_hash/s//$bad_hash/" "$TAMPERED"
if diff -u "$MANIFEST" "$TAMPERED" > /dev/null; then
  die "control 1 FAILED: a manifest with one deliberately wrong hash compared equal to the original — the diff check cannot be trusted"
fi
echo "control 1 PASS: diff detects a single deliberately wrong sha256 (as used by check-fixture.sh's final comparison)"
rm -f "$TAMPERED"

echo
echo "== control 2: mounting by a UUID that does not exist must fail =="
img="$S2_IMG/disk1.img"
[[ -f "$img" ]] || die "no image for disk1: $img — run build-fixture.sh first"
size_sectors=$(sfdisk -d "$img" | sed -n 's/.*disk1\.img1 : start=\s*64, size=\s*\([0-9]\+\).*/\1/p')
size_bytes=$((size_sectors * 512))
dev=$(losetup --find --show --offset 32768 --sizelimit "$size_bytes" "$img")
s2_assert_own_loop "$dev" "$img"
s2_fresh_blkid_cache
real_uuid=$(blkid -s UUID -o value "$dev")
fake_uuid="00000000-0000-0000-0000-000000000000"
[[ "$fake_uuid" != "$real_uuid" ]] || die "control 2 setup is broken: fake UUID collided with the real one"
mkdir -p -- "$S2_ADOPT/negctrl"
mount_rc=0
mount -t xfs -o ro,norecovery "UUID=$fake_uuid" "$S2_ADOPT/negctrl" 2>"$S2/negctrl-mount.err" || mount_rc=$?
if [[ "$mount_rc" -eq 0 ]]; then
  umount "$S2_ADOPT/negctrl" 2>/dev/null || true
  die "control 2 FAILED: mounting a nonexistent UUID succeeded (it must have silently fallen back to some other device)"
fi
echo "control 2 PASS: mount by a nonexistent UUID failed as expected (exit $mount_rc):"
cat "$S2/negctrl-mount.err"
rmdir "$S2_ADOPT/negctrl"
losetup -d "$dev"

echo
echo "== control 3: a fresh scratch disk (never one of the fixture disks) is ordinary and rw-capable =="
scratch_img="$S2_IMG/scratch.img"
[[ -e "$scratch_img" ]] && die "refusing to recreate existing image: $scratch_img"
truncate -s 320M "$scratch_img"
scratch_dev=$(s2_partition_disk "$scratch_img")
mkfs.xfs -q -f "$scratch_dev"
mkdir -p -- "$S2_ADOPT/negctrl"
mount -t xfs -o rw "$scratch_dev" "$S2_ADOPT/negctrl"
echo "control-3-marker" > "$S2_ADOPT/negctrl/marker"
sync
umount "$S2_ADOPT/negctrl"
rmdir "$S2_ADOPT/negctrl"
losetup -d "$scratch_dev"
rm -f "$scratch_img"
echo "control 3 PASS: a normal rw mount wrote a file with no restriction — confirming every 'read-only' claim made about disk1-disk3 above is this spike's own mount-option choice, not a property of the image"

echo
echo "negative-controls.sh: PASS"
