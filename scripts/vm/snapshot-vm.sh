#!/usr/bin/env bash
# `make vm-snapshot NAME=<name>` (doc 06 §4): an internal qcow2 snapshot
# of every disk this lab's domain has, named so a later `make vm-restore
# NAME=<name>` reaches it in seconds instead of a reinstall. Requires the
# domain to exist (running or shut off) and refuses anything but this
# lab's own domain, the same guard every other vm-*.sh script uses.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/vm/lib.sh
source "$script_dir/lib.sh"

vm_require_id
vm_assert_own_domain "$VM_DOMAIN"

NAME="${NAME:-}"
[[ -n "$NAME" ]] || die "set NAME (e.g. make vm-snapshot NAME=clean)"
[[ "$NAME" =~ ^[a-zA-Z0-9][a-zA-Z0-9_-]*$ ]] || die "invalid NAME '$NAME': letters, digits, '_', '-' only, starting with a letter or digit"

vm_domain_exists "$VM_DOMAIN" || die "domain '$VM_DOMAIN' does not exist — run 'make vm-up' first"

echo "vm-snapshot[$HOSERVA_LAB_ID]: creating snapshot '$NAME' of '$VM_DOMAIN'"
virsh -c "$VM_CONNECT" snapshot-create-as --domain "$VM_DOMAIN" --name "$NAME" \
  --description "hoserva L3 lab $HOSERVA_LAB_ID" --atomic >/dev/null
echo "vm-snapshot[$HOSERVA_LAB_ID]: done"
