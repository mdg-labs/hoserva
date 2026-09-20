#!/usr/bin/env bash
# Seeds a running L3 guest with a Samba share, an NFS export and an fstab
# bind mount *before* the .deb is installed (issue #113, Q76). Domain
# names are namespaced by HOSERVA_LAB_ID; this never touches a domain it
# did not create.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/vm/lib.sh
source "$script_dir/lib.sh"

vm_require_id
vm_assert_own_domain "$VM_DOMAIN"

vm_domain_exists "$VM_DOMAIN" || die "domain '$VM_DOMAIN' does not exist — run create-vm.sh first"
vm_domain_running "$VM_DOMAIN" || die "domain '$VM_DOMAIN' is not running"

echo "vm-seed-host[$HOSERVA_LAB_ID]: installing samba and nfs-kernel-server in the guest"
vm_ssh 'sudo DEBIAN_FRONTEND=noninteractive apt-get update -qq'
vm_ssh 'sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq samba nfs-kernel-server'

echo "vm-seed-host[$HOSERVA_LAB_ID]: creating Samba share, NFS export and fstab bind mount"
vm_ssh 'sudo mkdir -p /srv/hoserva-existing /export/hoserva-existing /srv/hoserva-bind /mnt/hoserva-existing'
vm_ssh 'echo hello-samba | sudo tee /srv/hoserva-existing/hello.txt >/dev/null'
vm_ssh 'echo hello-nfs | sudo tee /export/hoserva-existing/hello.txt >/dev/null'
vm_ssh 'echo hello-fstab | sudo tee /srv/hoserva-bind/hello.txt >/dev/null'

# Append a share without replacing Debian's packaged smb.conf.
vm_ssh 'grep -q "^\[hoserva-existing\]" /etc/samba/smb.conf || printf "%s\n" "" "[hoserva-existing]" "   path = /srv/hoserva-existing" "   browseable = yes" "   read only = yes" "   guest ok = yes" | sudo tee -a /etc/samba/smb.conf >/dev/null'

vm_ssh 'grep -q "^/export/hoserva-existing" /etc/exports || echo "/export/hoserva-existing *(ro,sync,no_subtree_check,no_root_squash)" | sudo tee -a /etc/exports >/dev/null'
vm_ssh 'grep -q "/mnt/hoserva-existing" /etc/fstab || echo "/srv/hoserva-bind /mnt/hoserva-existing none bind 0 0" | sudo tee -a /etc/fstab >/dev/null'

vm_ssh 'sudo mount /mnt/hoserva-existing'
vm_ssh 'sudo exportfs -ra'
vm_ssh 'sudo systemctl enable --now smbd nfs-server'

echo "vm-seed-host[$HOSERVA_LAB_ID]: verifying seeded services before install"
vm_ssh 'testparm -s --section-name=hoserva-existing >/dev/null'
vm_ssh 'exportfs | grep -q /export/hoserva-existing'
vm_ssh 'findmnt /mnt/hoserva-existing >/dev/null'
vm_ssh 'grep -qx hello-samba /srv/hoserva-existing/hello.txt'
vm_ssh 'grep -qx hello-nfs /export/hoserva-existing/hello.txt'
vm_ssh 'grep -qx hello-fstab /mnt/hoserva-existing/hello.txt'

echo "vm-seed-host[$HOSERVA_LAB_ID]: seeded"
