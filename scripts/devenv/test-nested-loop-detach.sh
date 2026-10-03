#!/usr/bin/env bash
# Self-check of lab_detach_nested_loops (scripts/devenv/lib.sh), the step
# destroy-array.sh runs before it unmounts anything (issue #576). Run inside
# the lab, against the standing array, by `make test-lab`. Runs only inside
# the hoserva-lab container, only under this lab's own $LAB.
#
# A loop device backed by a file on a lab disk mount, with no builder left to
# clean it up, must be detached while that mount is still mounted. This test
# never runs destroy-array.sh and never unmounts a mount that has one of its
# devices on it: it calls the detach step directly, and if the step leaves a
# device attached, its EXIT trap detaches that exact device itself, while the
# filesystem is still mounted and the path still resolves, before it removes
# anything.
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"
lab_require_id

disk="$LAB/mnt/disk1"
mountpoint -q "$disk" || die "$disk is not mounted: run against the standing array (make lab-up)"

fail=0
ok() { echo "ok: $*"; }
bad() {
  echo "FAIL: $*"
  fail=1
}

nested_img="$disk/nested-loop-check.img"
mounted_img="$disk/nested-loop-mounted.img"
mounted_mnt="$LAB/mnt/nested-loop-mounted"
freed_mnt="$LAB/mnt/nested-loop-freed"
outside_img="$LAB/nested-loop-outside.img"
# Only ever the devices this test attached itself, each with the one file it
# attached it to.
declare -A attached=()

attach() {  # image size
  local img=$1 dev
  truncate -s "$2" "$img"
  dev=$(losetup --find --show "$img")
  [[ "$dev" =~ ^/dev/loop[0-9]+$ ]] || die "losetup returned '$dev'"
  [[ "$(losetup -j "$img" --output NAME --noheadings | tr -d '[:space:]')" == "$dev" ]] \
    || die "$dev is not the only device backed by $img"
  attached[$img]=$dev
  ATTACHED=$dev
}

is_attached() { [[ -e "/sys/block/${1#/dev/}/loop/backing_file" ]]; }

# shellcheck disable=SC2317 # called from the EXIT trap
cleanup() {
  local img dev
  set +e
  # Unmount first, while the device is still there: a mounted device only
  # detaches lazily.
  mountpoint -q "$mounted_mnt" 2>/dev/null && umount "$mounted_mnt"
  mountpoint -q "$freed_mnt" 2>/dev/null && umount "$freed_mnt"
  for img in "${!attached[@]}"; do
    dev=${attached[$img]}
    if is_attached "$dev" && [[ "$(cat "/sys/block/${dev#/dev/}/loop/backing_file" 2>/dev/null)" == "$img" ]]; then
      echo "cleanup: detaching $dev (backed by $img) that the test itself attached"
      losetup -d "$dev" || echo "cleanup: FAILED to detach $dev (backed by $img) — the lab container must stay up; clear it from inside" >&2
    fi
  done
  # Only after every detach: a file removed under an attached device leaves it
  # reading "(deleted)".
  for img in "${!attached[@]}"; do
    is_attached "${attached[$img]}" || rm -f -- "$img"
  done
  rmdir "$mounted_mnt" "$freed_mnt" 2>/dev/null
}
trap cleanup EXIT

# 1. A plain nested device: attached to a file on a lab disk mount.
attach "$nested_img" 16M
nested_dev=$ATTACHED
is_attached "$nested_dev" || die "positive control: $nested_dev is not attached before the step"

# 2. A nested device whose own filesystem is mounted under $LAB, so detaching
#    it alone would only mark it autoclear.
attach "$mounted_img" 320M
mounted_dev=$ATTACHED
mkfs.xfs -q "$mounted_dev"
mkdir -p "$mounted_mnt"
mount "$mounted_dev" "$mounted_mnt"
mountpoint -q "$mounted_mnt" || die "positive control: $mounted_mnt is not mounted before the step"

# 3. A device backed by a file directly on $LAB (not on a lab mount, as the
#    disk images are): not this step's, destroy-array.sh's later pass owns it.
attach "$outside_img" 16M
outside_dev=$ATTACHED

lab_detach_nested_loops

if is_attached "$nested_dev"; then
  bad "$nested_dev, backed by a file on $disk, is still attached after lab_detach_nested_loops"
else
  ok "a loop device backed by a file on a lab disk mount is detached while the mount is still mounted"
fi

if is_attached "$mounted_dev"; then
  bad "$mounted_dev, mounted at $mounted_mnt, is still attached after lab_detach_nested_loops"
elif mountpoint -q "$mounted_mnt"; then
  bad "$mounted_mnt is still mounted after lab_detach_nested_loops"
else
  ok "a nested device with its own mount is unmounted, then detached"
fi

if is_attached "$outside_dev"; then
  ok "a device backed by a file on \$LAB itself, not on a lab mount, is left for the later pass"
else
  bad "$outside_dev, backed by $outside_img, was detached though that file is not on a lab mount"
fi

if mountpoint -q "$disk"; then
  ok "$disk is still mounted: the step detaches, it does not unmount the disk"
else
  bad "$disk is no longer mounted"
fi

# 4. _lab_free_nested_loop against a device that is gone, or that another file
#    now backs. Every device here is one this test attached itself; the
#    re-use is staged by re-attaching the number this test just freed.
backing_of() { cat "/sys/block/${1#/dev/}/loop/backing_file" 2>/dev/null || true; }
free_nested() {  # device path -> output in $FREE_OUT, status in $FREE_RC
  FREE_RC=0
  FREE_OUT=$( (_lab_free_nested_loop "$1" "$2") 2>&1 ) || FREE_RC=$?
}

# 4a. A device already marked autoclear while mounted: the unmount frees it,
#     so there is nothing left to detach.
freed_img="$disk/nested-loop-freed.img"
attach "$freed_img" 320M
freed_dev=$ATTACHED
mkfs.xfs -q "$freed_dev"
mkdir -p "$freed_mnt"
mount "$freed_dev" "$freed_mnt"
losetup -d "$freed_dev"
is_attached "$freed_dev" || die "positive control: $freed_dev was freed by losetup -d while mounted"
free_nested "$freed_dev" "$freed_img"
rmdir "$freed_mnt" 2>/dev/null || true
if ((FREE_RC == 0)) && ! is_attached "$freed_dev"; then
  ok "a device the unmount already freed (autoclear) is treated as freed, not detached"
else
  bad "freeing the autoclear device $freed_dev: status $FREE_RC, attached=$(is_attached "$freed_dev" && echo yes || echo no): $FREE_OUT"
fi

# 4b. The number is now backed by a different file: it is not detached, and the
#     step does not die over it.
other_img="$disk/nested-loop-other.img"
claimed_img="$disk/nested-loop-claimed.img"
attach "$other_img" 16M
other_dev=$ATTACHED
free_nested "$other_dev" "$claimed_img"
if ((FREE_RC == 0)) && [[ "$(backing_of "$other_dev")" == "$other_img" ]] \
   && [[ "$FREE_OUT" == *"not this lab's device"* ]]; then
  ok "a device backed by a different file than the one expected is neither detached nor fatal, and the step says so"
else
  bad "freeing $other_dev with a different expected file: status $FREE_RC, backing now '$(backing_of "$other_dev")': $FREE_OUT"
fi

# 4c. The device really is detached by losetup -d, and the number is taken by
#     another file in the same instant: the still-attached check must not
#     mistake that for this device still being attached.
reused_img="$disk/nested-loop-reused.img"
swapped_img="$disk/nested-loop-swapped.img"
attach "$reused_img" 16M
reused_dev=$ATTACHED
truncate -s 16M "$swapped_img"
attached[$swapped_img]=$reused_dev
FREE_RC=0
FREE_OUT=$(
  # shellcheck disable=SC2317,SC2329 # called by _lab_free_nested_loop
  losetup() {
    command losetup "$@" || return
    [[ "$1" == "-d" ]] && command losetup "$2" "$swapped_img"
  }
  _lab_free_nested_loop "$reused_dev" "$reused_img"
) 2>&1 || FREE_RC=$?
if ((FREE_RC == 0)) && [[ "$(backing_of "$reused_dev")" == "$swapped_img" ]]; then
  ok "a number re-used by another file right after the detach is not reported as still attached"
else
  bad "freeing $reused_dev whose number was re-used: status $FREE_RC, backing now '$(backing_of "$reused_dev")': $FREE_OUT"
fi

# destroy-array.sh must run the step before its first unmount: the detach has
# to happen while the mounts are attached, so the test above would pass
# against a destroy-array.sh that only called it afterwards.
destroy="$HERE/destroy-array.sh"
call=$(grep -n '^lab_detach_nested_loops$' "$destroy" | head -n 1 | cut -d: -f1 || true)
first_unmount=$(grep -n '^  unmount_if_mounted ' "$destroy" | head -n 1 | cut -d: -f1 || true)
if [[ -n "$call" && -n "$first_unmount" ]] && ((call < first_unmount)); then
  ok "destroy-array.sh calls lab_detach_nested_loops (line $call) before its first unmount (line $first_unmount)"
else
  bad "destroy-array.sh does not call lab_detach_nested_loops before its first unmount (call: ${call:-none}, unmount: ${first_unmount:-none})"
fi

exit "$fail"
