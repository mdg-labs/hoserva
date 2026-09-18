#!/usr/bin/env bash
# `make vm-destroy` (doc 06 §4, D20): tears down this lab's own VM only.
#
# vm_assert_own_domain refuses anything but this exact lab's domain name
# before any destroy/undefine call, so a stale variable or a typo can
# never reach a domain this harness did not create — including the
# invoking user's own qemu:///session domains. Idempotent: safe to run
# when the domain was never created, is already stopped, or teardown
# was already run once.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/vm/lib.sh
source "$script_dir/lib.sh"

vm_require_id
vm_assert_own_domain "$VM_DOMAIN"

if vm_domain_exists "$VM_DOMAIN"; then
  if vm_domain_running "$VM_DOMAIN"; then
    echo "vm-destroy[$HOSERVA_LAB_ID]: destroying running domain '$VM_DOMAIN'"
    virsh -c "$VM_CONNECT" destroy "$VM_DOMAIN" >/dev/null
  fi
  echo "vm-destroy[$HOSERVA_LAB_ID]: undefining '$VM_DOMAIN' and its snapshot metadata"
  virsh -c "$VM_CONNECT" undefine "$VM_DOMAIN" --snapshots-metadata --nvram >/dev/null 2>&1 \
    || virsh -c "$VM_CONNECT" undefine "$VM_DOMAIN" --snapshots-metadata >/dev/null
else
  echo "vm-destroy[$HOSERVA_LAB_ID]: domain '$VM_DOMAIN' does not exist, nothing to tear down"
fi

if [[ -d "$VM_ROOT" ]]; then
  echo "vm-destroy[$HOSERVA_LAB_ID]: removing this lab's own disk images, seeds and state ($VM_ROOT)"
  rm -rf -- "$VM_ROOT"
fi

echo "vm-destroy[$HOSERVA_LAB_ID]: done"
