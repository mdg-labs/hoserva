#!/usr/bin/env bash
# L3 immutable-mountpoint check (issue #463, doc 02 §1, Q69, doc 06 §6):
# proves on the guest's real kernel what internal/disk's fake-Runner tests
# can only model — that an empty array-slot mountpoint made immutable by the
# production guard (disk.GuardedMounter) refuses a write with EPERM while its
# disk is not mounted, that a real filesystem still mounts onto it with a
# writable root, and that unmounting exposes the immutable directory again.
# The loop-device lab cannot host it: its container has no CAP_LINUX_IMMUTABLE
# and Q45 keeps its capability set at loop devices and FUSE.
#
# internal/disk/mountpoint_guard_l3_test.go (build tag l3) is built here with
# `go test -tags l3 -c` on the host — compiling touches no device — and the
# resulting binary is copied into this lab's own VM and run there with sudo,
# the same build-elsewhere/run-inside pattern array-sequence-check.sh uses.
# The test creates its own ext4 image file, loop device and mountpoint under
# MOUNT_ROOT inside the guest and removes them again; it never touches the
# array disks or their /mnt/diskN mountpoints.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/vm/lib.sh
source "$script_dir/lib.sh"

vm_require_id
vm_assert_own_domain "$VM_DOMAIN"

vm_domain_exists "$VM_DOMAIN" || die "domain '$VM_DOMAIN' does not exist — run 'make vm-up' first"
vm_domain_running "$VM_DOMAIN" || die "domain '$VM_DOMAIN' is not running — run 'make vm-up' first"

MOUNT_ROOT="/mnt/hoserval3immut"
TEST_BIN_LOCAL="$VM_STATE_DIR/hoserva-immutable-mountpoint-l3-test"
TEST_BIN_REMOTE="/tmp/hoserva-immutable-mountpoint-l3-test"

echo "immutable-mountpoint-check[$HOSERVA_LAB_ID]: building the L3 test binary (compiling touches no device)"
(cd "$VM_REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -tags l3 -c -o "$TEST_BIN_LOCAL" ./internal/disk/)

echo "immutable-mountpoint-check[$HOSERVA_LAB_ID]: ensuring e2fsprogs (mkfs.ext4, chattr, lsattr) is present on the guest"
vm_ssh 'command -v mkfs.ext4 >/dev/null 2>&1 && command -v chattr >/dev/null 2>&1 || (sudo apt-get update -qq && sudo apt-get install -y -qq e2fsprogs)'

vm_scp "$TEST_BIN_LOCAL" "hoserva@127.0.0.1:$TEST_BIN_REMOTE"
vm_ssh "chmod +x $TEST_BIN_REMOTE"

STATUS=0
OUTPUT="$(vm_ssh "sudo env HOSERVA_LAB_ID='$HOSERVA_LAB_ID' HOSERVA_L3_MOUNT_ROOT='$MOUNT_ROOT' $TEST_BIN_REMOTE -test.v -test.run '^TestL3ImmutableMountpoint_GuardsUnmountedSlot\$' -test.timeout 5m" 2>&1)" \
  || STATUS=$?
printf '%s\n' "$OUTPUT"

vm_ssh "rm -f -- $TEST_BIN_REMOTE" >/dev/null 2>&1 || true

# A skipped test exits 0 too; the step only passes if the test really ran.
if (( STATUS == 0 )) && ! grep -q '^--- PASS: TestL3ImmutableMountpoint_GuardsUnmountedSlot ' <<<"$OUTPUT"; then
  echo "immutable-mountpoint-check[$HOSERVA_LAB_ID]: the test exited 0 without reporting PASS (skipped?)" >&2
  STATUS=1
fi

if (( STATUS == 0 )); then
  echo "immutable-mountpoint-check[$HOSERVA_LAB_ID]: PASS — an unmounted immutable slot refuses writes with EPERM, a disk mounts onto it, and unmounting exposes it again"
else
  echo "immutable-mountpoint-check[$HOSERVA_LAB_ID]: FAIL" >&2
fi
exit "$STATUS"
