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
holder=

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
  # A device a holder still has open only detaches lazily.
  [[ -z "$holder" ]] || kill "$holder" 2>/dev/null
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

# 4c. The device really is detached, and the number is taken by another file
#     as soon as the kernel frees it: the still-attached check must not mistake
#     that for this device still being attached. A process of this test holds
#     the device open, so after the step's losetup -d it is only marked
#     autoclear and the post-detach check reads it still backed by the expected
#     file. The step's first sleep between polls is the moment the test acts:
#     it kills the holder (by PID), waits for the kernel to free the number and
#     takes it with another file. The next poll reads that other file and the
#     step must return through the backing-file comparison.
reused_img="$disk/nested-loop-reused.img"
swapped_img="$disk/nested-loop-swapped.img"
attach "$reused_img" 16M
reused_dev=$ATTACHED
truncate -s 16M "$swapped_img"
attached[$swapped_img]=$reused_dev
sleep 60 3<"$reused_dev" &
holder=$!
holds_dev() { [[ "$(readlink "/proc/$holder/fd/3" 2>/dev/null)" == "$1" ]]; }
for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
  holds_dev "$reused_dev" && break
  sleep 0.1
done
holds_dev "$reused_dev" || die "positive control: the holder process does not have $reused_dev open"
FREE_RC=0
FREE_OUT=$(
  swap_done=0
  # shellcheck disable=SC2317,SC2329 # called by _lab_free_nested_loop
  sleep() {
    if ((swap_done == 0)); then
      swap_done=1
      echo "probe: backing before the swap '$(backing_of "$reused_dev")'"
      kill "$holder"
      for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
        holds_dev "$reused_dev" || break
        command sleep 0.1
      done
      for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
        is_attached "$reused_dev" || break
        command sleep 0.1
      done
      if command losetup "$reused_dev" "$swapped_img" 2>/dev/null; then
        echo "probe: number re-used"
      else
        echo "probe: number not re-used"
      fi
    fi
    command sleep "$@"
  }
  _lab_free_nested_loop "$reused_dev" "$reused_img"
) 2>&1 || FREE_RC=$?
wait "$holder" 2>/dev/null || true
holder=
if ((FREE_RC == 0)) && [[ "$(backing_of "$reused_dev")" == "$swapped_img" ]] \
   && [[ "$FREE_OUT" == *"probe: backing before the swap '$reused_img'"* && "$FREE_OUT" == *"probe: number re-used"* ]]; then
  ok "a number re-used by another file right after the detach is not reported as still attached"
else
  bad "freeing $reused_dev whose number was re-used: status $FREE_RC, backing now '$(backing_of "$reused_dev")': $FREE_OUT"
fi

# 4d. A device already marked autoclear and still held by another opener (here
#     a process of this test): when that opener lets go, the kernel frees the
#     number, and another lab could take it. While _lab_free_nested_loop holds
#     the device open, the number must stay this device's. The probe runs at the
#     moment the other opener is gone, just before the detach, and tries to take
#     the number with another file this test owns.
held_img="$disk/nested-loop-held.img"
taken_img="$disk/nested-loop-taken.img"
attach "$held_img" 16M
held_dev=$ATTACHED
truncate -s 16M "$taken_img"
attached[$taken_img]=$held_dev
sleep 60 3<"$held_dev" &
holder=$!
for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
  holds_dev "$held_dev" && break
  sleep 0.1
done
holds_dev "$held_dev" || die "positive control: the holder process does not have $held_dev open"
losetup -d "$held_dev"
is_attached "$held_dev" || die "positive control: $held_dev was freed by losetup -d though a process holds it"
FREE_RC=0
FREE_OUT=$(
  # shellcheck disable=SC2317,SC2329 # called by _lab_free_nested_loop
  losetup() {
    if [[ "$1" == "-d" ]]; then
      kill "$holder"
      for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
        holds_dev "$held_dev" || break
        sleep 0.1
      done
      holds_dev "$held_dev" && echo "probe: the holder still has the device open"
      sleep 0.3
      echo "probe: backing '$(backing_of "$held_dev")'"
      if command losetup "$held_dev" "$taken_img" 2>/dev/null; then
        echo "probe: number re-used"
      else
        echo "probe: number not re-usable"
      fi
    fi
    command losetup "$@"
  }
  _lab_free_nested_loop "$held_dev" "$held_img"
) 2>&1 || FREE_RC=$?
wait "$holder" 2>/dev/null || true
holder=
if ((FREE_RC == 0)) && ! is_attached "$held_dev" \
   && [[ "$FREE_OUT" == *"probe: backing '$held_img'"* && "$FREE_OUT" == *"probe: number not re-usable"* ]]; then
  ok "a device held by another opener cannot be freed or re-used while the step holds it, and is freed once it lets go"
else
  bad "freeing the held device $held_dev: status $FREE_RC, attached=$(is_attached "$held_dev" && echo yes || echo no): $FREE_OUT"
fi

# 4e. A device that cannot be opened. A number that is not there at all counts
#     as freed. A device that is attached but cannot be opened (the path below
#     names a loop device of this test through a non-directory, so open fails
#     while /sys still resolves it) is not detached, and the step dies.
free_nested /dev/loop99999 "$LAB/nested-loop-none.img"
if ((FREE_RC == 0)); then
  ok "a device number that is gone when the step opens it counts as freed"
else
  bad "freeing a loop device that is not there: status $FREE_RC: $FREE_OUT"
fi
free_nested "$other_dev/../${other_dev#/dev/}" "$other_img"
if ((FREE_RC != 0)) && is_attached "$other_dev" && [[ "$(backing_of "$other_dev")" == "$other_img" ]] \
   && [[ "$FREE_OUT" == *"cannot open"* && "$FREE_OUT" != *"failed to detach"* ]]; then
  ok "an attached device that cannot be opened is not detached, and the step dies at the open"
else
  bad "freeing $other_dev through an unopenable path: status $FREE_RC, backing now '$(backing_of "$other_dev")': $FREE_OUT"
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
