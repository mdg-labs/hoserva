#!/usr/bin/env bash
# `make vm-unraid-fixture VARIANT=<variant>` (doc 06 §5, issue #74): builds the
# synthetic Unraid source of one variant on this lab's own L3 guest's array
# disks, before any .deb is deployed, verifies it, copies its expected result
# (manifest, layout and the Flash Backup zips) into
# .vm/<HOSERVA_LAB_ID>/unraid/<variant>/, and takes the snapshot named like the
# variant, so `make vm-restore NAME=<variant>` returns to it in seconds.
#
# The guest must come from `make vm-up VARIANT=<variant>`, which creates each
# target disk at its spec size=; the builder refuses a disk of any other size
# before it writes one. So the layout is the L2 build's: up to 2000G is Unraid's
# MBR layout, 2T (2 TiB) and larger the GPT layout.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/vm/lib.sh
source "$script_dir/lib.sh"
# shellcheck source=scripts/vm/unraid-lib.sh
source "$script_dir/unraid-lib.sh"

vm_require_id
unraid_require_variant
unraid_require_guest

unraid_guest_packages
unraid_guest_push
unraid_guest_build "$VARIANT"
unraid_guest_verify "$VARIANT"

dest="$VM_ROOT/unraid/$VARIANT"
echo "vm-unraid-fixture[$HOSERVA_LAB_ID]: copying the expected result to $dest"
rm -rf -- "$dest"
mkdir -p -- "$dest"
vm_ssh "sudo tar -C '$UNRAID_GUEST_OUT/$VARIANT' -cf - expected" | tar -C "$dest" -xf -

if virsh -c "$VM_CONNECT" snapshot-list "$VM_DOMAIN" --name | grep -qxF -- "$VARIANT"; then
  echo "vm-unraid-fixture[$HOSERVA_LAB_ID]: replacing the earlier snapshot '$VARIANT'"
  vm_assert_own_domain "$VM_DOMAIN"
  virsh -c "$VM_CONNECT" snapshot-delete "$VM_DOMAIN" --snapshotname "$VARIANT" >/dev/null
fi
NAME="$VARIANT" "$script_dir/snapshot-vm.sh"
echo "vm-unraid-fixture[$HOSERVA_LAB_ID]: done — make vm-restore NAME=$VARIANT returns to it"
