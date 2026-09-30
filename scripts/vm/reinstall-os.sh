#!/usr/bin/env bash
# `make vm-reinstall-os` (doc 06 §4, doc 01 §6, D20): replaces only this
# lab's own VM's OS disk with a fresh copy-on-write image over the cached
# Debian base image, boots it, and installs Hoserva on it from the same
# .deb a first install uses — a bare-metal reinstall, with every array
# disk (and the domain definition that carries their identities) left
# exactly as they were.
#
# Only $VM_IMG_DIR/os.qcow2 is written or replaced. No array disk image is
# named, opened or modified here, and the script checks the domain still
# lists the same array-disk serials after the swap. Same own-domain guard
# as every other vm-*.sh script.
#
# Failure after the domain is stopped never leaves a half-swapped domain
# quietly: the new image is built beside the old one and only renamed into
# place once the domain is shut off, so a failure before the rename leaves
# the old OS disk in place, and one after it exits non-zero with the stage
# it reached on stderr.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/vm/lib.sh
source "$script_dir/lib.sh"

vm_require_id
vm_assert_own_domain "$VM_DOMAIN"

vm_domain_exists "$VM_DOMAIN" || die "domain '$VM_DOMAIN' does not exist — run 'make vm-up' first"

OS_DISK="$VM_IMG_DIR/os.qcow2"
NEW_OS_DISK="$VM_IMG_DIR/os.qcow2.new"
OS_DISK_SIZE="${HOSERVA_VM_OS_DISK_SIZE:-12G}"
SHUTDOWN_TIMEOUT_S=120

[[ -f "$OS_DISK" ]] || die "no OS disk at $OS_DISK — nothing to replace"

array_serials() {
  virsh -c "$VM_CONNECT" dumpxml "$VM_DOMAIN" | grep -o '<serial>[^<]*</serial>' | sort
}

domain_xml="$(virsh -c "$VM_CONNECT" dumpxml "$VM_DOMAIN")"
grep -Fq -- "$(vm_xml_attr_escape "$OS_DISK")" <<<"$domain_xml" \
  || die "domain '$VM_DOMAIN' does not boot from $OS_DISK — refusing to replace a disk the domain does not use"

serials_before="$(array_serials)"
[[ -n "$serials_before" ]] || die "domain '$VM_DOMAIN' lists no array-disk serials — refusing to reinstall a VM that is not this harness's array VM"

stage="preparing the new OS disk"
on_exit() {
  local rc=$?
  rm -f -- "$NEW_OS_DISK"
  if [[ $rc -ne 0 ]]; then
    printf 'vm-reinstall-os[%s]: FAILED while %s — array disks were not touched; check the domain with: virsh -c %s domstate %s\n' \
      "$HOSERVA_LAB_ID" "$stage" "$VM_CONNECT" "$VM_DOMAIN" >&2
  fi
}
trap on_exit EXIT

echo "vm-reinstall-os[$HOSERVA_LAB_ID]: fetching base image"
BASE_IMAGE="$("$script_dir/fetch-base-image.sh")"

echo "vm-reinstall-os[$HOSERVA_LAB_ID]: building a fresh OS disk beside the old one (copy-on-write over $BASE_IMAGE)"
rm -f -- "$NEW_OS_DISK"
qemu-img create -q -f qcow2 -F qcow2 -b "$BASE_IMAGE" "$NEW_OS_DISK" "$OS_DISK_SIZE"

stage="shutting the domain down"
if vm_domain_running "$VM_DOMAIN"; then
  echo "vm-reinstall-os[$HOSERVA_LAB_ID]: shutting '$VM_DOMAIN' down so its array filesystems are cleanly unmounted"
  virsh -c "$VM_CONNECT" shutdown "$VM_DOMAIN" >/dev/null
  waited=0
  while vm_domain_running "$VM_DOMAIN"; do
    waited=$((waited + 1))
    if [[ $waited -ge $SHUTDOWN_TIMEOUT_S ]]; then
      echo "vm-reinstall-os[$HOSERVA_LAB_ID]: guest did not shut down within ${SHUTDOWN_TIMEOUT_S}s — destroying the domain" >&2
      virsh -c "$VM_CONNECT" destroy "$VM_DOMAIN" >/dev/null
      break
    fi
    sleep 1
  done
fi
vm_domain_running "$VM_DOMAIN" && die "domain '$VM_DOMAIN' is still running — not replacing its OS disk"

stage="replacing the OS disk (the domain is shut off; the old OS disk is gone once this completes)"
echo "vm-reinstall-os[$HOSERVA_LAB_ID]: replacing $OS_DISK"
mv -f -- "$NEW_OS_DISK" "$OS_DISK"

stage="starting the domain on the new OS disk (the OS disk is already replaced)"
virsh -c "$VM_CONNECT" start "$VM_DOMAIN" >/dev/null

echo "vm-reinstall-os[$HOSERVA_LAB_ID]: waiting for the guest's forwarded SSH port ($VM_SSH_PORT)"
vm_wait_tcp "$VM_SSH_PORT" 180 || die "guest did not open its forwarded SSH port within 180s"
vm_ssh_wait_ready 180 || die "could not SSH into the guest within 180s of its port opening"

stage="checking the array disks are unchanged"
serials_after="$(array_serials)"
[[ "$serials_after" == "$serials_before" ]] \
  || die "the domain's array-disk serials changed across the reinstall (before: $(tr '\n' ' ' <<<"$serials_before")after: $(tr '\n' ' ' <<<"$serials_after"))"

stage="installing Hoserva on the new OS disk"
if [[ -z "${DEB:-}" ]]; then
  built_deb="$(find "$VM_STATE_DIR/deb-build" -maxdepth 1 -name '*.deb' -print -quit 2>/dev/null || true)"
  [[ -z "$built_deb" ]] || DEB="$built_deb"
fi
echo "vm-reinstall-os[$HOSERVA_LAB_ID]: installing Hoserva on the fresh OS"
DEB="${DEB:-}" TAG="${TAG:-}" "$script_dir/deploy.sh"

stage="checking the fresh install has no admin"
setup_status="$(vm_ssh 'curl -sk https://127.0.0.1:8008/api/v1/setup/status')" \
  || die "could not read /setup/status from the freshly installed hoservad"
[[ "$setup_status" == *'"adminExists":false'* ]] \
  || die "the freshly installed hoservad does not report adminExists=false: $setup_status"

stage="done"
echo "vm-reinstall-os[$HOSERVA_LAB_ID]: done — fresh OS with Hoserva installed and no admin; array disks untouched (${serials_after//$'\n'/ })"
