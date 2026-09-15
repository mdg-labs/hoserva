#!/usr/bin/env bash
# Safety-critical acceptance test (doc 06 §3, Q45, S9 doc 08): from inside
# the lab container, opening a host block device must fail, and any device
# path or backing file outside this lab's own $LAB/img/ must be refused by
# our own guard, not just by luck. Run via `make lab-verify-refusal` against
# a running lab container. Never run on the host.
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"
lab_require_id

fail=0
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# The device cgroup rule (doc06 §3, Q45: `b 7:* rmw`) allows only loop
# devices (major 7). Major 259 (nvme) and major 8 (sd) are real host disk
# classes and must not be openable even if mknod'd inside the container —
# S9 observed exactly this split: mknod succeeds, open fails.
check_device_refused() {  # description, path, major, minor
  local desc=$1 path=$2 major=$3 minor=$4
  rm -f "$path"
  if ! mknod -m 660 "$path" b "$major" "$minor" 2>/dev/null; then
    echo "ok: $desc — mknod itself refused"
    return 0
  fi
  local err
  if err=$(dd if="$path" of=/dev/null bs=512 count=1 status=none 2>&1); then
    echo "FAIL: $desc — read succeeded, a host device class is reachable from the lab"
    fail=1
  elif [[ "$err" == *'Operation not permitted'* ]]; then
    # EPERM is what the device cgroup rule itself produces on a denied
    # open — this is the only failure that actually proves the rule ran.
    echo "ok: $desc — open refused with EPERM (device cgroup)"
  else
    # Any other failure (e.g. ENXIO, "No such device or address", from a
    # mknod'd node with no real backing device) proves nothing about the
    # cgroup rule — that would be a false "ok" with no positive control
    # behind it, so it must fail the test instead.
    echo "FAIL: $desc — dd failed but not with EPERM, so this is not a confirmed cgroup refusal: $err"
    fail=1
  fi
  rm -f "$path"
}

check_device_refused "host NVMe class (major 259)" "$work/nvme0n1" 259 0
check_device_refused "host SCSI/SATA class (major 8)" "$work/sda" 8 0

# Our own guard must refuse a device/image pair outside $LAB/img/, not rely
# on the device cgroup alone — this is what every create-array.sh /
# destroy-array.sh call is protected by.
outside_img="$work/outside.img"
: > "$outside_img"
# lab_assert_own_loop calls die() on refusal, which exits — run it in a
# subshell so that expected exit only ends the subshell, not this test.
if ( lab_assert_own_loop "/dev/loop0" "$outside_img" ) 2>/dev/null; then
  echo "FAIL: lab_assert_own_loop accepted an image path outside \$LAB/img/"
  fail=1
else
  echo "ok: lab_assert_own_loop refuses an image path outside \$LAB/img/"
fi

# It must also refuse a non-loop device path even for one of our own images.
own_img="$LAB/img/refusal-check.img"
mkdir -p "$LAB/img"
: > "$own_img"
if ( lab_assert_own_loop "/dev/sda" "$own_img" ) 2>/dev/null; then
  echo "FAIL: lab_assert_own_loop accepted a non-loop device path"
  fail=1
else
  echo "ok: lab_assert_own_loop refuses a non-loop device path"
fi
rm -f "$own_img"

exit "$fail"
