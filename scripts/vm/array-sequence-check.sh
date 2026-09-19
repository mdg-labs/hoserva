#!/usr/bin/env bash
# `make vm-suite` step 11 (issue #146, doc 02 §1, §4, Q69, Q70): the two
# L3 scenarios issue #109's own L2 test
# (internal/job/array_lab_test.go) explicitly left blocked — real
# physical disk mount/unmount through a real systemd .mount unit (the
# loop-device lab has no init system, doc 08 §6) and a real process
# standing in for a container holding a file open on the pool.
#
# internal/job/array_l3_test.go (build tag l3) is built here with
# `go test -tags l3 -c` on the host — compiling touches no device — and
# the resulting binary is copied into this lab's own VM and run there
# with sudo, the same build-elsewhere/run-inside pattern
# internal/pool/mount_lab_test.go's own header established for the lab
# (see spindown-check.sh's own precedent for this same pattern applied to
# a VM instead of the lab container).
#
# There is no array/disk/pool operation in api/openapi.yaml yet (the same
# gap run-l3-suite.sh's own "array setup" step names), so this never goes
# through a running hoservad: it constructs a real
# internal/disk.StorageGate and a real internal/job.ArraySequence
# directly against the guest's own virtio-blk devices, exactly the way
# array_lab_test.go already does at L2 against the lab's loop devices.
#
# Runs last in run-l3-suite.sh's own step order: scenario 1 below
# permanently detaches one array disk from this domain's own persistent
# config for the rest of this VM's life, so nothing earlier in the suite
# that assumes the full array-disk topology (e.g. spindown-check.sh's own
# device discovery) needs to run after it.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/vm/lib.sh
source "$script_dir/lib.sh"

vm_require_id
vm_assert_own_domain "$VM_DOMAIN"

vm_domain_exists "$VM_DOMAIN" || die "domain '$VM_DOMAIN' does not exist — run 'make vm-up' first"
vm_domain_running "$VM_DOMAIN" || die "domain '$VM_DOMAIN' is not running — run 'make vm-up' first"

# No hyphen anywhere in this path (or in any path built under it, see
# the Go test's own "diskN"/"svcdiskN" naming): a physical disk's mount
# unit filename is derived from its Where= path by disk.UnitFileName,
# which (correctly, per its own doc comment — production's own
# /mnt/diskN, /mnt/parityN, /mnt/cache paths never contain one) does not
# escape a literal '-' the way real systemd-escape would, so a mount
# path containing one produces a unit systemd itself refuses to start
# ("Where= setting doesn't match unit name") — confirmed empirically
# against this exact harness while first building this script.
MOUNT_ROOT="/mnt/hoserval3array"
TEST_BIN_LOCAL="$VM_STATE_DIR/hoserva-array-l3-test"
TEST_BIN_REMOTE="/tmp/hoserva-array-l3-test"

echo "array-sequence-check[$HOSERVA_LAB_ID]: building the L3 test binary (compiling touches no device)"
(cd "$VM_REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -tags l3 -c -o "$TEST_BIN_LOCAL" ./internal/job/)

# Copied again after the reboot below, not just here: this lab's base
# image mounts /tmp as tmpfs, so anything placed under it does not
# survive the clean shutdown/reboot scenario 1 performs (confirmed
# empirically against this exact harness — the first version of this
# script copied the binary only here and then found it gone after
# rebooting, issue #146). This first copy still runs the guest through
# the package installs below before the reboot, so the reboot itself
# never spends time on `apt-get`.
vm_scp "$TEST_BIN_LOCAL" "hoserva@127.0.0.1:$TEST_BIN_REMOTE"
vm_ssh "chmod +x $TEST_BIN_REMOTE"

echo "array-sequence-check[$HOSERVA_LAB_ID]: ensuring mergerfs and e2fsprogs are present on the guest"
vm_ssh 'command -v mergerfs >/dev/null 2>&1 || (sudo apt-get update -qq && sudo apt-get install -y -qq mergerfs)'
vm_ssh 'command -v mkfs.ext4 >/dev/null 2>&1 || (sudo apt-get update -qq && sudo apt-get install -y -qq e2fsprogs)'

echo "array-sequence-check[$HOSERVA_LAB_ID]: === scenario 1/2: boot with a data disk detached ==="

echo "array-sequence-check[$HOSERVA_LAB_ID]: discovering array disks from the live domain XML"
DOMXML="$(virsh -c "$VM_CONNECT" dumpxml "$VM_DOMAIN")" || die "could not read domain XML for '$VM_DOMAIN'"

# Each entry is "dev:serial:slot" — dev is libvirt's own persistent
# <target dev='vdX'/> label (only ever used to pass back to
# `detach-disk`, which takes that same label); slot is the disk's own PCI
# slot (e.g. "0a" from <address .../ slot='0x0a'/>), which is what the L3
# test actually uses to identify a disk once it is running inside the
# guest — see the comment on PCI_SERIAL_MAP below for why.
ARRAY_DEVS=()
dev=""
serial=""
slot=""
while IFS= read -r line; do
  case "$line" in
    *'<disk '*) dev=""; serial=""; slot="" ;;
  esac
  if [[ "$line" == *'<target '* && "$line" =~ dev=\'([^\']*)\' ]]; then
    dev="${BASH_REMATCH[1]}"
  fi
  if [[ "$line" =~ \<serial\>([^\<]*)\</serial\> ]]; then
    serial="${BASH_REMATCH[1]}"
  fi
  if [[ "$line" == *'<address '* && "$line" =~ slot=\'0x([0-9a-fA-F]+)\' ]]; then
    slot="${BASH_REMATCH[1]}"
  fi
  if [[ "$line" == *'</disk>'* ]]; then
    if [[ -n "$dev" && -n "$serial" && -n "$slot" && "$serial" == "hoserva-$HOSERVA_LAB_ID-"* ]]; then
      ARRAY_DEVS+=("$dev:$serial:$slot")
    fi
    dev=""
    serial=""
    slot=""
  fi
done <<<"$DOMXML"

[[ "${#ARRAY_DEVS[@]}" -ge 2 ]] || die "found ${#ARRAY_DEVS[@]} array disk(s) in the domain XML, want at least 2 (one to detach, one to keep present) — is this domain fresh from 'make vm-up'?"
echo "array-sequence-check[$HOSERVA_LAB_ID]: array disks: ${ARRAY_DEVS[*]}"

# Expected serials come straight from the domain XML this script already
# parsed above (ARRAY_DEVS), never from the guest's own copy: a virtio-blk
# device's exported serial is truncated by the guest kernel to
# VIRTIO_BLK_ID_BYTES (20 bytes, include/uapi/linux/virtio_blk.h) before
# it ever reaches /sys/class/block/<dev>/serial, and that truncation point
# depends on $HOSERVA_LAB_ID's own length — this project's own nightly
# lab id ("nightly-<run_id>-<attempt>") is long enough that every array
# disk's serial collides on the guest's truncated copy, which would make
# every disk look identical to this script and the L3 test it drives.
# This is exactly the hazard scripts/vm/spindown-check.sh's own device
# discovery already avoids the same way: read the untruncated <serial>
# from `virsh dumpxml`, never touch the guest's own (potentially-
# truncated) copy at all.
EXPECTED_SERIALS=""
for entry in "${ARRAY_DEVS[@]}"; do
  rest="${entry#*:}"       # "serial:slot"
  EXPECTED_SERIALS+="${rest%:*} "
done
echo "array-sequence-check[$HOSERVA_LAB_ID]: expected serials (untruncated, from the domain XML): $EXPECTED_SERIALS"

# The first genuinely data-role array disk (create-vm.sh's own "diskN"
# serial suffix, e.g. vdc for "disk1") — parity1 is declared first in
# ARRAY_DEVS (create-vm.sh's own disk declaration order) and is
# deliberately not the one detached here, since detaching it would not
# exercise "a real data disk is missing", which is what this scenario and
# doc 06's own wording describe. Detached by its own target device, which
# detach-disk takes directly.
DETACH_TARGET=""
DETACH_SERIAL=""
DETACH_SLOT=""
for entry in "${ARRAY_DEVS[@]}"; do
  rest="${entry#*:}"                # "serial:slot"
  candidate_serial="${rest%:*}"
  case "$candidate_serial" in
    "hoserva-$HOSERVA_LAB_ID-disk"*)
      DETACH_TARGET="${entry%%:*}"
      DETACH_SERIAL="$candidate_serial"
      DETACH_SLOT="${rest##*:}"
      break
      ;;
  esac
done
[[ -n "$DETACH_TARGET" ]] || die "no data-role (hoserva-$HOSERVA_LAB_ID-disk*) array disk found in the domain XML to detach — is this domain fresh from 'make vm-up'?"
echo "array-sequence-check[$HOSERVA_LAB_ID]: will detach $DETACH_TARGET (serial $DETACH_SERIAL, PCI slot 0x$DETACH_SLOT) before the next boot"

# The slot:serial mapping the L3 test uses to identify each disk still
# present after the reboot below, keyed by each disk's own PCI slot
# rather than its libvirt target-dev label: a detached disk's *label*
# (e.g. "vdc") is simply removed from the persistent config and every
# other disk keeps its own label, but the *guest kernel's* own /dev/vdX
# naming is assigned by virtio-blk probe order, not by that label — with
# one disk gone, every disk probed after it shifts down by one letter
# (confirmed empirically against this exact harness: with "disk1"
# detached, "disk2" — libvirt's own "vdd" — came back up as the guest's
# /dev/vdc). A disk's own PCI slot (e.g. "0x0c") is assigned explicitly
# by create-vm.sh and never renumbered by any of this, and is exactly
# what the guest's own /sys/class/block/<dev>/device resolves back to, so
# it is what the L3 test actually keys this map on.
PCI_SERIAL_MAP=""
for entry in "${ARRAY_DEVS[@]}"; do
  rest="${entry#*:}"      # "serial:slot"
  slot="${rest##*:}"
  [[ "$slot" == "$DETACH_SLOT" ]] && continue
  PCI_SERIAL_MAP+="$slot:${rest%:*} "
done

echo "array-sequence-check[$HOSERVA_LAB_ID]: shutting the guest down cleanly"
virsh -c "$VM_CONNECT" shutdown "$VM_DOMAIN" >/dev/null
shutdown_deadline=$((SECONDS + 120))
while vm_domain_running "$VM_DOMAIN"; do
  if (( SECONDS >= shutdown_deadline )); then
    echo "array-sequence-check[$HOSERVA_LAB_ID]: guest did not shut down within 120s, forcing it off" >&2
    virsh -c "$VM_CONNECT" destroy "$VM_DOMAIN" >/dev/null
    break
  fi
  sleep 2
done

echo "array-sequence-check[$HOSERVA_LAB_ID]: detaching $DETACH_TARGET from the domain's persistent config"
virsh -c "$VM_CONNECT" detach-disk "$VM_DOMAIN" "$DETACH_TARGET" --config >/dev/null

echo "array-sequence-check[$HOSERVA_LAB_ID]: starting the guest back up — this boot genuinely has one fewer array disk"
virsh -c "$VM_CONNECT" start "$VM_DOMAIN" >/dev/null
vm_wait_tcp "$VM_SSH_PORT" 180 || die "guest did not open its forwarded SSH port within 180s of the missing-disk boot"
vm_ssh_wait_ready 180 || die "could not SSH into the guest within 180s of the missing-disk boot"

# Re-copied: this lab's base image mounts /tmp as tmpfs, so the copy
# taken before the shutdown/reboot above did not survive it (see the
# comment on the first copy).
vm_scp "$TEST_BIN_LOCAL" "hoserva@127.0.0.1:$TEST_BIN_REMOTE"
vm_ssh "chmod +x $TEST_BIN_REMOTE"

SCENARIO1_STATUS=0
vm_ssh "sudo env HOSERVA_LAB_ID='$HOSERVA_LAB_ID' HOSERVA_L3_MOUNT_ROOT='$MOUNT_ROOT' HOSERVA_L3_EXPECTED_SERIALS='$EXPECTED_SERIALS' HOSERVA_L3_PCI_SERIAL_MAP='$PCI_SERIAL_MAP' $TEST_BIN_REMOTE -test.v -test.run '^TestL3ArraySequence_MissingDiskAtBoot$' -test.timeout 5m" \
  || SCENARIO1_STATUS=$?

echo "array-sequence-check[$HOSERVA_LAB_ID]: === scenario 2/2: a service holding a file open blocks the unmount, and ArraySequence.Stop avoids it ==="
SCENARIO2_STATUS=0
vm_ssh "sudo env HOSERVA_LAB_ID='$HOSERVA_LAB_ID' HOSERVA_L3_MOUNT_ROOT='$MOUNT_ROOT' $TEST_BIN_REMOTE -test.v -test.run '^TestL3ArraySequence_ServiceStopsBeforeUnmount$' -test.timeout 5m" \
  || SCENARIO2_STATUS=$?

echo ""
echo "array-sequence-check[$HOSERVA_LAB_ID]: ===== summary ====="
if (( SCENARIO1_STATUS == 0 )); then
  echo "array-sequence-check[$HOSERVA_LAB_ID]: scenario 1 (missing disk at boot) — PASS"
else
  echo "array-sequence-check[$HOSERVA_LAB_ID]: scenario 1 (missing disk at boot) — FAIL" >&2
fi
if (( SCENARIO2_STATUS == 0 )); then
  echo "array-sequence-check[$HOSERVA_LAB_ID]: scenario 2 (service stops before unmount) — PASS"
else
  echo "array-sequence-check[$HOSERVA_LAB_ID]: scenario 2 (service stops before unmount) — FAIL" >&2
fi

[[ "$SCENARIO1_STATUS" -eq 0 && "$SCENARIO2_STATUS" -eq 0 ]]
