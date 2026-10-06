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
#
# VARIANT=<unraid fixture variant> sizes the array disks from that variant's
# spec (see below) instead of the HOSERVA_VM_*_SIZE values.
#
# HOSERVA_VM_TOPOLOGY selects the layout (default "separate", the one
# above). "shared-nvme" is doc 01 §6's partitioned-NVMe layout: the OS disk
# carries a second, unused partition after root (HOSERVA_VM_SPARE_PARTITION_SIZE,
# default 8G), there is no separate cache disk, and only parity1, disk1 and
# disk2 are attached as array disks. An array created in it puts the cache
# on that partition (Hoserva never creates it, doc 02 §4). The partition is
# added once the guest is up: the base image grows root to fill the disk on
# its first boot, so the harness grows the disk live (virsh blockresize) and
# appends the partition into the new space with the guest's own sfdisk.
#
# With VARIANT, a disk the variant's spec targets and the shared-nvme topology
# lacks (disk3, disk4, disk5, cache) is attached as well, because the Unraid
# fixture builder needs every disk of its spec. The migration suite
# (run-migration-suite.sh) then detaches the fixture's own cache disk, which this
# layout replaces with the spare partition.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/vm/lib.sh
source "$script_dir/lib.sh"

vm_require_id

MEMORY_MIB="${HOSERVA_VM_MEMORY_MIB:-4096}"
VCPU_COUNT="${HOSERVA_VM_VCPU:-4}"
TOPOLOGY="${HOSERVA_VM_TOPOLOGY:-separate}"
OS_DISK_SIZE="${HOSERVA_VM_OS_DISK_SIZE:-12G}"
case "$TOPOLOGY" in
  separate | shared-nvme) ;;
  *) die "unknown HOSERVA_VM_TOPOLOGY '$TOPOLOGY' — valid values: separate, shared-nvme" ;;
esac
SPARE_PARTITION_SIZE="${HOSERVA_VM_SPARE_PARTITION_SIZE:-8G}"

# name:size pairs — the same six-disk-plus-cache topology
# scripts/devenv/create-array.sh uses for the L2 lab (doc 06 §3), so the
# two layers describe the same array shape at different fidelity.
# HOSERVA_VM_{PARITY,DATA,CACHE}_SIZE override the defaults for runs that
# must actually fill a disk (the L3 soak): a 4T sparse qcow2 still
# allocates host blocks as soon as the guest writes, so a "full disk"
# injection cannot use the lab-scale sizes.
PARITY_SIZE="${HOSERVA_VM_PARITY_SIZE:-8T}"
DATA_SIZE="${HOSERVA_VM_DATA_SIZE:-4T}"
CACHE_SIZE="${HOSERVA_VM_CACHE_SIZE:-1T}"
if [[ "$TOPOLOGY" == "shared-nvme" ]]; then
  ARRAY_DISK_SPECS=(
    "parity1:$PARITY_SIZE"
    "disk1:$DATA_SIZE"
    "disk2:$DATA_SIZE"
  )
else
  ARRAY_DISK_SPECS=(
    "parity1:$PARITY_SIZE"
    "disk1:$DATA_SIZE"
    "disk2:$DATA_SIZE"
    "disk3:$DATA_SIZE"
    "disk4:$DATA_SIZE"
    "disk5:$DATA_SIZE"
    "cache:$CACHE_SIZE"
  )
fi

# VARIANT (issue #570) sizes each array disk its spec names, by target, from
# that Unraid fixture variant's spec (doc 06 §5) at its l3size=, else size=, so
# the L3 build lays out the same partition scheme as the L2 one. A disk no spec
# line targets keeps the size
# above. Everything is resolved here, before the first image is fetched or
# created: a missing variant, a spec line without a size or target, or a target
# this topology has no disk for refuses the whole run. The shared-nvme topology
# attaches the spec's disks it lacks, as the header says.
VARIANT="${VARIANT:-}"
if [[ -n "$VARIANT" ]]; then
  # shellcheck source=scripts/vm/unraid-lib.sh
  source "$script_dir/unraid-lib.sh"
  unraid_require_variant
  spec_sizes="$(unraid_spec_sizes)" || exit 1
  [[ -n "$spec_sizes" ]] || die "variant '$VARIANT' has no disk lines in its spec"
  while read -r spec_target spec_bytes; do
    matched=0
    for i in "${!ARRAY_DISK_SPECS[@]}"; do
      if [[ "${ARRAY_DISK_SPECS[$i]%%:*}" == "$spec_target" ]]; then
        ARRAY_DISK_SPECS[i]="$spec_target:$spec_bytes"
        matched=1
      fi
    done
    if ((!matched)) && [[ "$TOPOLOGY" == shared-nvme && " parity1 disk1 disk2 disk3 disk4 disk5 cache " == *" $spec_target "* ]]; then
      ARRAY_DISK_SPECS+=("$spec_target:$spec_bytes")
      matched=1
    fi
    ((matched)) || die "variant '$VARIANT' targets disk '$spec_target', which topology '$TOPOLOGY' does not create"
  done <<<"$spec_sizes"
fi

# HOSERVA_VM_PLAN_DISKS=1 prints the resolved name:size of every array disk and
# stops, before any image is fetched or created (unraid-sizing-check.sh).
if [[ -n "${HOSERVA_VM_PLAN_DISKS:-}" ]]; then
  printf '%s\n' "${ARRAY_DISK_SPECS[@]}"
  exit 0
fi

if vm_domain_exists "$VM_DOMAIN"; then
  die "domain '$VM_DOMAIN' already exists — run 'make vm-destroy' first if you want a fresh VM"
fi

echo "vm-up[$HOSERVA_LAB_ID]: fetching base image"
BASE_IMAGE="$("$script_dir/fetch-base-image.sh")"

echo "vm-up[$HOSERVA_LAB_ID]: creating OS disk (copy-on-write over $BASE_IMAGE)"
OS_DISK="$VM_IMG_DIR/os.qcow2"
qemu-img create -q -f qcow2 -F qcow2 -b "$BASE_IMAGE" "$OS_DISK" "$OS_DISK_SIZE"

echo "vm-up[$HOSERVA_LAB_ID]: creating array disks"
ARRAY_DISKS_XML="$(mktemp)"
OS_DISK_SERIAL_XML_FILE="$(mktemp)"
trap 'rm -f -- "$ARRAY_DISKS_XML" "$OS_DISK_SERIAL_XML_FILE"' EXIT
# The shared-nvme OS disk is the boot and cache disk at once, so it gets a
# serial of its own: the guest then names it /dev/disk/by-id/virtio-<serial>
# and its partitions ...-partN, the identity Hoserva binds a cache
# partition to. boot-hoserva-<id> keeps its first 20 bytes (the guest's
# truncation, see the array disks' serials below) distinct from every
# array disk's.
if [[ "$TOPOLOGY" == "shared-nvme" ]]; then
  echo "      <serial>boot-hoserva-$HOSERVA_LAB_ID</serial>" > "$OS_DISK_SERIAL_XML_FILE"
fi

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
    echo "      <source file='$(vm_xml_attr_escape "$disk_path")'/>"
    echo "      <target dev='vd${DEV_LETTERS[$i]}' bus='virtio'/>"
    # disk_name leads the serial, HOSERVA_LAB_ID trails it (issue #162):
    # the guest kernel truncates a virtio-blk device's exported serial to
    # VIRTIO_BLK_ID_BYTES (20 bytes) from the front before udev ever
    # builds /dev/disk/by-id/virtio-<serial>, so whatever comes after
    # byte 20 is silently dropped. ARRAY_DISK_SPECS' own names
    # (parity1/disk1-5/cache) are already distinct within their first 5
    # bytes, and 20 bytes is never less than that, so leading with
    # disk_name keeps every array disk's by-id link distinct regardless
    # of $HOSERVA_LAB_ID's length — unlike a leading lab id, which a
    # long id (this project's own nightly shape) can push the
    # disk-identifying suffix past the truncation point entirely,
    # colliding every array disk's by-id link on the same guest-side
    # name. libvirt itself does not support a virtio-blk disk's <wwn>
    # ("Only ide and scsi disk support wwn", confirmed against this
    # host's libvirt), so this ordering — not a <wwn> — is what makes
    # this array disk resolve to a distinct identity in the guest.
    echo "      <serial>$disk_name-hoserva-$HOSERVA_LAB_ID</serial>"
    echo "      <address type='pci' domain='0x0000' bus='0x00' slot='${PCI_SLOTS[$i]}' function='0x0'/>"
    echo "    </disk>"
  } >> "$ARRAY_DISKS_XML"
done

echo "vm-up[$HOSERVA_LAB_ID]: generating this lab's own SSH keypair"
if [[ ! -f "$VM_KEY_DIR/id_ed25519" ]]; then
  ssh-keygen -q -t ed25519 -N '' -C "hoserva-lab-$HOSERVA_LAB_ID" -f "$VM_KEY_DIR/id_ed25519"
fi
PUBKEY="$(cat "$VM_KEY_DIR/id_ed25519.pub")"

echo "$TOPOLOGY" > "$VM_STATE_DIR/topology"

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
# OS_DISK, SEED_ISO and the serial log path all derive from
# HOSERVA_VM_REPO_ROOT (an arbitrary filesystem path) — XML-escaped for
# the attribute they land in, then sed-escaped so a literal '&', '\' or
# '|' in the path can't corrupt this substitution itself. The other
# placeholders below are this harness's own lab id, numeric ports and
# generated MAC (vm_require_id/vm_id_valid's character whitelist), never
# template- or user-supplied text.
OS_DISK_XML="$(vm_sed_replacement_escape "$(vm_xml_attr_escape "$OS_DISK")")"
SEED_ISO_XML="$(vm_sed_replacement_escape "$(vm_xml_attr_escape "$SEED_ISO")")"
SERIAL_LOG_XML="$(vm_sed_replacement_escape "$(vm_xml_attr_escape "$VM_STATE_DIR/serial.log")")"
sed -e "/__ARRAY_DISKS__/r $ARRAY_DISKS_XML" -e "/__ARRAY_DISKS__/d" \
    -e "/__OS_DISK_SERIAL__/r $OS_DISK_SERIAL_XML_FILE" -e "/__OS_DISK_SERIAL__/d" "$script_dir/domain.xml.tmpl" \
  | sed \
    -e "s|__DOMAIN_NAME__|$VM_DOMAIN|g" \
    -e "s|__MEMORY_MIB__|$MEMORY_MIB|g" \
    -e "s|__VCPU_COUNT__|$VCPU_COUNT|g" \
    -e "s|__OS_DISK_PATH__|$OS_DISK_XML|g" \
    -e "s|__SEED_ISO_PATH__|$SEED_ISO_XML|g" \
    -e "s|__MAC_ADDRESS__|$MAC_ADDRESS|g" \
    -e "s|__SSH_PORT__|$VM_SSH_PORT|g" \
    -e "s|__HTTPS_PORT__|$VM_HTTPS_PORT|g" \
    -e "s|__SERIAL_LOG_PATH__|$SERIAL_LOG_XML|g" \
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

if [[ "$TOPOLOGY" == "shared-nvme" ]]; then
  echo "vm-up[$HOSERVA_LAB_ID]: adding the spare partition to the OS disk ($SPARE_PARTITION_SIZE after root, left unformatted and unmounted)"
  os_bytes="$(numfmt --from=iec "$OS_DISK_SIZE")" || die "invalid HOSERVA_VM_OS_DISK_SIZE '$OS_DISK_SIZE'"
  spare_bytes="$(numfmt --from=iec "$SPARE_PARTITION_SIZE")" || die "invalid HOSERVA_VM_SPARE_PARTITION_SIZE '$SPARE_PARTITION_SIZE'"
  vm_assert_own_domain "$VM_DOMAIN"
  # cloud-init must be done first: nothing may still be growing root when
  # the new space appears.
  vm_ssh 'sudo cloud-init status --wait >/dev/null 2>&1 || true'
  virsh -c "$VM_CONNECT" blockresize "$VM_DOMAIN" vda "$(((os_bytes + spare_bytes) / 1024))KiB" >/dev/null
  # --no-reread: root is mounted, so the kernel cannot re-read the table;
  # partx then adds only the new partition (2 — root, BIOS boot and EFI
  # already hold 1, 14 and 15).
  # No size: the partition takes all the space the disk grew by, less the
  # backup GPT sfdisk moves to the new end of the disk.
  vm_ssh "echo 'type=0FC63DAF-8483-4772-8E79-3D69D8477DE4' | sudo sfdisk --append --no-reread /dev/vda >/dev/null && sudo partx -a --nr 2 /dev/vda && sudo udevadm settle" \
    || die "could not add the spare partition to the guest's OS disk"
fi
