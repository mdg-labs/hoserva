#!/usr/bin/env bash
# `make vm-unraid-capture VARIANT=<variant>` (doc 06 §5, doc 05 §4 step 0, Q89,
# issue #74): regenerates the committed capture of one fixture variant. In this
# lab's own disposable L3 guest it builds the variant without a capture, runs
# the variant's containers under the guest's Docker, runs
# tools/unraid/prepare-migration.sh against the fixture's flash and disks, and
# copies what the script wrote under /boot/config/hoserva/ (all but disks.ini,
# which the builder renders from the target disks) into
# testdata/unraid-fixtures/<variant>/flash/config/hoserva/. The capture is real
# `docker inspect` output of real containers, never hand-typed JSON. Container
# ids and timestamps in it come from the guest, never from a real server.
#
# It then builds and verifies the variant again with the new capture, so the
# files that are about to be committed are known to be accepted.
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
unraid_guest_build "$VARIANT" no-capture

echo "vm-unraid-capture[$HOSERVA_LAB_ID]: running the containers and the prepare script in the guest"
vm_ssh "sudo env HOSERVA_LAB_ID='$HOSERVA_LAB_ID' bash '$UNRAID_GUEST_DIR/scripts/vm/unraid-capture-guest.sh' '$VARIANT'"

variant_dir="$VM_REPO_ROOT/testdata/unraid-fixtures/$VARIANT"
dest="$variant_dir/flash/config/hoserva"
echo "vm-unraid-capture[$HOSERVA_LAB_ID]: copying the capture to ${dest#"$VM_REPO_ROOT"/}"
# beside flash/, not inside it: the builder copies all of flash/ into a fixture
tmp_dest="$(mktemp -d -- "$variant_dir/.capture.XXXXXX")"
trap 'rm -rf -- "$tmp_dest"' EXIT
vm_ssh "sudo tar -C '$UNRAID_GUEST_OUT/$VARIANT/capture' -cf - ." | tar -C "$tmp_dest" --no-same-owner --no-same-permissions -xf -
# the FAT32 flash has no permissions: keep them out of the committed files
find "$tmp_dest" -type d -exec chmod 0755 {} +
find "$tmp_dest" -type f -exec chmod 0644 {} +
mkdir -p -- "$(dirname -- "$dest")"
rm -rf -- "$dest"
mv -- "$tmp_dest" "$dest"

unraid_guest_push
unraid_guest_build "$VARIANT"
unraid_guest_verify "$VARIANT"
echo "vm-unraid-capture[$HOSERVA_LAB_ID]: done — review and commit testdata/unraid-fixtures/$VARIANT/flash/config/hoserva/"
