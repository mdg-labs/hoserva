#!/usr/bin/env bash
# `make vm-suite` boot-cache step (issue #557, doc 01 §6, doc 02 §4): an
# array whose cache is the spare partition of the OS disk — the
# partitioned-NVMe layout doc 01 §6 calls acceptable.
#
# It boots a second VM of its own in create-vm.sh's shared-nvme topology
# (HOSERVA_VM_TOPOLOGY: the OS disk carries an unused second partition,
# there is no cache disk, three array disks), because the suite's own VM has
# a separate cache disk. That VM's lab id is "<suite id>-bc", so its domain,
# images and ports are its own and destroy-vm.sh removes exactly it; it is
# destroyed again whatever the outcome.
#
# internal/job/boot_cache_l3_test.go (build tag l3) is built here with
# `go test -tags l3 -c` on the host — compiling touches no device — copied
# into that VM and run there with sudo. It creates the array through the real
# disk_format job and checks the root filesystem and the OS disk's partition
# table are exactly as they were.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/vm/lib.sh
source "$script_dir/lib.sh"

vm_require_id
SUITE_LAB_ID="$HOSERVA_LAB_ID"
export HOSERVA_LAB_ID="$SUITE_LAB_ID-bc"
vm_require_id
vm_assert_own_domain "$VM_DOMAIN"

# shellcheck disable=SC2317 # run by the EXIT trap below
cleanup() {
  HOSERVA_LAB_ID="$SUITE_LAB_ID-bc" "$script_dir/destroy-vm.sh" || echo "boot-cache-check[$HOSERVA_LAB_ID]: destroying this step's own VM failed — run 'make vm-destroy HOSERVA_LAB_ID=$SUITE_LAB_ID-bc'" >&2
}
trap cleanup EXIT

TEST_BIN_LOCAL="$VM_STATE_DIR/hoserva-boot-cache-l3-test"
TEST_BIN_REMOTE="/tmp/hoserva-boot-cache-l3-test"

echo "boot-cache-check[$HOSERVA_LAB_ID]: building the L3 test binary (compiling touches no device)"
(cd "$VM_REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -tags l3 -c -o "$TEST_BIN_LOCAL" ./internal/job/)

echo "boot-cache-check[$HOSERVA_LAB_ID]: creating the shared-nvme VM"
HOSERVA_VM_TOPOLOGY=shared-nvme HOSERVA_VM_MEMORY_MIB="${HOSERVA_VM_MEMORY_MIB:-2048}" "$script_dir/create-vm.sh"
[[ "$(cat "$VM_STATE_DIR/topology")" == "shared-nvme" ]] || die "VM '$VM_DOMAIN' is not in the shared-nvme topology"

echo "boot-cache-check[$HOSERVA_LAB_ID]: ensuring xfsprogs is present on the guest"
vm_ssh 'command -v mkfs.xfs >/dev/null 2>&1 || (sudo apt-get update -qq && sudo apt-get install -y -qq xfsprogs)'

echo "boot-cache-check[$HOSERVA_LAB_ID]: the guest's OS disk layout before the test"
vm_ssh 'lsblk -o NAME,SIZE,TYPE,FSTYPE,MOUNTPOINTS /dev/vda'

vm_scp "$TEST_BIN_LOCAL" "hoserva@127.0.0.1:$TEST_BIN_REMOTE"
vm_ssh "chmod +x $TEST_BIN_REMOTE"

STATUS=0
vm_ssh "sudo env HOSERVA_LAB_ID='$HOSERVA_LAB_ID' $TEST_BIN_REMOTE -test.v -test.run '^TestL3BootCache_ArrayWithTheCacheOnTheBootDisksSparePartition\$' -test.timeout 8m" \
  || STATUS=$?

if (( STATUS == 0 )); then
  echo "boot-cache-check[$HOSERVA_LAB_ID]: array with the cache on the boot disk's spare partition — PASS"
else
  echo "boot-cache-check[$HOSERVA_LAB_ID]: array with the cache on the boot disk's spare partition — FAIL" >&2
fi
exit "$STATUS"
