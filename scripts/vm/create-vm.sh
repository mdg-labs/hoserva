#!/usr/bin/env bash
# `make vm-up` (doc 06 §4, Q42, Q79, D20): boots one L3 test VM under
# qemu:///session, disk images and domain name namespaced by
# HOSERVA_LAB_ID, never touching a host disk or a domain this harness did
# not create itself.
#
# Layout matches doc 06 §3's own loop-device array topology, applied to
# real virtual block devices instead of loop devices: one parity disk,
# five data disks, one cache disk — sparse qcow2, so declaring realistic
# sizes costs almost nothing on disk until the guest actually writes to
# them.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/vm/lib.sh
source "$script_dir/lib.sh"

vm_require_id

if vm_domain_exists "$VM_DOMAIN"; then
  die "domain '$VM_DOMAIN' already exists — run 'make vm-destroy' first if you want a fresh VM"
fi

MEMORY_MIB="${HOSERVA_VM_MEMORY_MIB:-4096}"
VCPU_COUNT="${HOSERVA_VM_VCPU:-4}"
OS_DISK_SIZE="${HOSERVA_VM_OS_DISK_SIZE:-12G}"

echo "vm-up[$HOSERVA_LAB_ID]: fetching base image"
BASE_IMAGE="$("$script_dir/fetch-base-image.sh")"

echo "vm-up[$HOSERVA_LAB_ID]: creating OS disk (copy-on-write over $BASE_IMAGE)"
OS_DISK="$VM_IMG_DIR/os.qcow2"
qemu-img create -q -f qcow2 -F qcow2 -b "$BASE_IMAGE" "$OS_DISK" "$OS_DISK_SIZE"

echo "vm-up[$HOSERVA_LAB_ID]: creating array disks"
# name:size pairs — the same six-disk-plus-cache topology
# scripts/devenv/create-array.sh uses for the L2 lab (doc 06 §3), so the
# two layers describe the same array shape at different fidelity.
ARRAY_DISK_SPECS=(
  "parity1:8T"
  "disk1:4T"
  "disk2:4T"
  "disk3:4T"
  "disk4:4T"
  "disk5:4T"
  "cache:1T"
)
ARRAY_DISKS_XML="$(mktemp)"
trap 'rm -f -- "$ARRAY_DISKS_XML"' EXIT

# One virtio target letter, and one explicit PCI slot, per array disk, in
# declaration order — vda/slot 0x09 is the OS disk above, so this starts
# at vdb/0x0a. Exactly 7 of each because ARRAY_DISK_SPECS above declares
# exactly 7 disks; a mismatch here is a bug in this script, not a runtime
# condition to handle. Every slot is explicit and at or above 0x09 —
# never left to libvirt's own default assignment — for the reason
# domain.xml.tmpl's own header comment records: libvirt's default first
# choice (0x03) resets this specific host partway through GRUB loading
# the kernel, confirmed by bisecting a hand-built qemu invocation.
DEV_LETTERS=(b c d e f g h)
PCI_SLOTS=(0x0a 0x0b 0x0c 0x0d 0x0e 0x0f 0x10)
for i in "${!ARRAY_DISK_SPECS[@]}"; do
  spec="${ARRAY_DISK_SPECS[$i]}"
  disk_name="${spec%%:*}"
  disk_size="${spec##*:}"
  disk_path="$VM_IMG_DIR/$disk_name.qcow2"
  qemu-img create -q -f qcow2 "$disk_path" "$disk_size"
  {
    echo "    <disk type='file' device='disk'>"
    echo "      <driver name='qemu' type='qcow2'/>"
    echo "      <source file='$disk_path'/>"
    echo "      <target dev='vd${DEV_LETTERS[$i]}' bus='virtio'/>"
    echo "      <serial>hoserva-$HOSERVA_LAB_ID-$disk_name</serial>"
    echo "      <address type='pci' domain='0x0000' bus='0x00' slot='${PCI_SLOTS[$i]}' function='0x0'/>"
    echo "    </disk>"
  } >> "$ARRAY_DISKS_XML"
done

echo "vm-up[$HOSERVA_LAB_ID]: generating this lab's own SSH keypair"
if [[ ! -f "$VM_KEY_DIR/id_ed25519" ]]; then
  ssh-keygen -q -t ed25519 -N '' -C "hoserva-lab-$HOSERVA_LAB_ID" -f "$VM_KEY_DIR/id_ed25519"
fi
PUBKEY="$(cat "$VM_KEY_DIR/id_ed25519.pub")"

echo "vm-up[$HOSERVA_LAB_ID]: building cloud-init seed ISO"
CIDATA_DIR="$VM_SEED_DIR/cidata"
rm -rf -- "$CIDATA_DIR"
mkdir -p -- "$CIDATA_DIR"
cat > "$CIDATA_DIR/meta-data" <<EOF
instance-id: hoserva-$HOSERVA_LAB_ID
local-hostname: hoserva-vm
EOF
cat > "$CIDATA_DIR/user-data" <<EOF
#cloud-config
ssh_pwauth: false
package_update: false
package_upgrade: false
users:
  - name: hoserva
    lock_passwd: true
    shell: /bin/bash
    sudo: ["ALL=(ALL) NOPASSWD:ALL"]
    ssh_authorized_keys:
      - $PUBKEY
EOF
SEED_ISO="$VM_SEED_DIR/seed.iso"
xorriso -as genisoimage -output "$SEED_ISO" -volid cidata -joliet -rock \
  "$CIDATA_DIR/user-data" "$CIDATA_DIR/meta-data" >/dev/null

MAC_HASH="$(printf '%s' "$HOSERVA_LAB_ID" | cksum | cut -d' ' -f1)"
MAC_ADDRESS="$(printf '52:54:00:%02x:%02x:%02x' \
  "$(((MAC_HASH >> 16) & 0xff))" "$(((MAC_HASH >> 8) & 0xff))" "$((MAC_HASH & 0xff))")"

echo "vm-up[$HOSERVA_LAB_ID]: rendering domain XML (ssh->$VM_SSH_PORT, hoservad TLS->$VM_HTTPS_PORT)"
DOMAIN_XML="$VM_STATE_DIR/domain.xml"
sed -e "/__ARRAY_DISKS__/r $ARRAY_DISKS_XML" -e "/__ARRAY_DISKS__/d" "$script_dir/domain.xml.tmpl" \
  | sed \
    -e "s|__DOMAIN_NAME__|$VM_DOMAIN|g" \
    -e "s|__MEMORY_MIB__|$MEMORY_MIB|g" \
    -e "s|__VCPU_COUNT__|$VCPU_COUNT|g" \
    -e "s|__OS_DISK_PATH__|$OS_DISK|g" \
    -e "s|__SEED_ISO_PATH__|$SEED_ISO|g" \
    -e "s|__MAC_ADDRESS__|$MAC_ADDRESS|g" \
    -e "s|__SSH_PORT__|$VM_SSH_PORT|g" \
    -e "s|__HTTPS_PORT__|$VM_HTTPS_PORT|g" \
    -e "s|__SERIAL_LOG_PATH__|$VM_STATE_DIR/serial.log|g" \
  > "$DOMAIN_XML"

echo "vm-up[$HOSERVA_LAB_ID]: defining and starting domain '$VM_DOMAIN'"
vm_assert_own_domain "$VM_DOMAIN"
virsh -c "$VM_CONNECT" define "$DOMAIN_XML" >/dev/null
virsh -c "$VM_CONNECT" start "$VM_DOMAIN" >/dev/null

echo "vm-up[$HOSERVA_LAB_ID]: waiting for the guest's forwarded SSH port ($VM_SSH_PORT)"
vm_wait_tcp "$VM_SSH_PORT" 180 || die "guest did not open its forwarded SSH port within 180s"

echo "vm-up[$HOSERVA_LAB_ID]: waiting for cloud-init/sshd to accept our key"
vm_ssh_wait_ready 180 || die "could not SSH into the guest within 180s of its port opening"

echo "vm-up[$HOSERVA_LAB_ID]: ready — domain '$VM_DOMAIN', ssh: ssh -p $VM_SSH_PORT hoserva@127.0.0.1 -i $VM_KEY_DIR/id_ed25519"
