#!/usr/bin/env bash
# `make vm-restore NAME=<name>` (doc 06 §4): reverts this lab's domain to
# a previously taken snapshot (snapshot-vm.sh) — seconds, not a reinstall.
# Same own-domain guard as every other vm-*.sh script.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/vm/lib.sh
source "$script_dir/lib.sh"

vm_require_id
vm_assert_own_domain "$VM_DOMAIN"

NAME="${NAME:-}"
[[ -n "$NAME" ]] || die "set NAME (e.g. make vm-restore NAME=clean)"
[[ "$NAME" =~ ^[a-zA-Z0-9][a-zA-Z0-9_-]*$ ]] || die "invalid NAME '$NAME': letters, digits, '_', '-' only, starting with a letter or digit"

vm_domain_exists "$VM_DOMAIN" || die "domain '$VM_DOMAIN' does not exist — run 'make vm-up' first"

echo "vm-restore[$HOSERVA_LAB_ID]: reverting '$VM_DOMAIN' to snapshot '$NAME'"
virsh -c "$VM_CONNECT" snapshot-revert --domain "$VM_DOMAIN" --snapshotname "$NAME" --running >/dev/null \
  || virsh -c "$VM_CONNECT" snapshot-revert --domain "$VM_DOMAIN" --snapshotname "$NAME" >/dev/null

if vm_domain_running "$VM_DOMAIN"; then
  echo "vm-restore[$HOSERVA_LAB_ID]: waiting for the guest's forwarded SSH port ($VM_SSH_PORT)"
  vm_wait_tcp "$VM_SSH_PORT" 120 || die "guest did not open its forwarded SSH port within 120s of the restore"
  vm_ssh_wait_ready 120 || die "could not SSH into the guest within 120s of the restore"
fi

echo "vm-restore[$HOSERVA_LAB_ID]: done"
