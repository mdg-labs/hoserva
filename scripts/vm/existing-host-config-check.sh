#!/usr/bin/env bash
# After .deb install and onboarding (issue #113, Q76): the Samba share,
# NFS export and fstab bind mount that seed-existing-host.sh created
# still work, and hoservad has not overwritten their files.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/vm/lib.sh
source "$script_dir/lib.sh"

vm_require_id
vm_assert_own_domain "$VM_DOMAIN"

vm_domain_exists "$VM_DOMAIN" || die "domain '$VM_DOMAIN' does not exist"
vm_domain_running "$VM_DOMAIN" || die "domain '$VM_DOMAIN' is not running"

vm_ssh 'testparm -s --section-name=hoserva-existing >/dev/null'
vm_ssh 'exportfs | grep -q /export/hoserva-existing'
vm_ssh 'findmnt /mnt/hoserva-existing >/dev/null'
vm_ssh 'grep -qx hello-samba /srv/hoserva-existing/hello.txt'
vm_ssh 'grep -qx hello-nfs /export/hoserva-existing/hello.txt'
vm_ssh 'grep -qx hello-fstab /mnt/hoserva-existing/hello.txt'
vm_ssh 'grep -q "^\[hoserva-existing\]" /etc/samba/smb.conf'
vm_ssh 'grep -q "^/export/hoserva-existing" /etc/exports'
vm_ssh 'grep -q "/mnt/hoserva-existing" /etc/fstab'

echo "vm-existing-host[$HOSERVA_LAB_ID]: samba share, NFS export and fstab mount still work"
