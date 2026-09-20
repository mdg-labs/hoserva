#!/usr/bin/env bash
# L3 NFS export check (issue #47, doc 03 §4.2): inside this lab's own
# guest, export a directory using the same client-option form
# config.RenderNFSExports writes, loopback-mount it, and write a file.
# Mount and unmount run only in the guest — never on the development host.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/vm/lib.sh
source "$script_dir/lib.sh"

vm_require_id
vm_assert_own_domain "$VM_DOMAIN"

vm_domain_exists "$VM_DOMAIN" || die "domain '$VM_DOMAIN' does not exist — run 'make vm-up' first"
vm_domain_running "$VM_DOMAIN" || die "domain '$VM_DOMAIN' is not running — run 'make vm-up' first"

# The export line matches RenderNFSExports: path, then
# host(rw,sync,no_subtree_check,<squash>). no_root_squash lets the
# guest's root write through the loopback client. /etc/exports.d keeps
# this from replacing a Q76 unmanaged /etc/exports.
vm_ssh 'sudo bash -s' <<'EOF'
set -euo pipefail
export_dir=/export/hoserva-nfs-l3
mnt=/tmp/hoserva-nfs-l3-mnt
exports_d=/etc/exports.d/hoserva-nfs-l3.exports
marker=hoserva-nfs-l3-wrote

cleanup() {
  if findmnt -n "$mnt" >/dev/null 2>&1; then
    umount "$mnt" || umount -l "$mnt" || true
  fi
  rm -rf -- "$mnt"
  rm -f -- "$exports_d"
  if command -v exportfs >/dev/null 2>&1; then
    exportfs -ra >/dev/null 2>&1 || true
  fi
  rm -rf -- "$export_dir"
}
trap cleanup EXIT

command -v exportfs >/dev/null 2>&1 || {
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq nfs-kernel-server
}

mkdir -p -- "$export_dir" "$mnt" /etc/exports.d
printf '%s 127.0.0.1(rw,sync,no_subtree_check,no_root_squash)\n' "$export_dir" > "$exports_d"
systemctl start rpcbind nfs-server
exportfs -ra

mount -t nfs -o vers=3,nolock 127.0.0.1:"$export_dir" "$mnt"
printf '%s\n' "$marker" > "$mnt/from-client.txt"
grep -qx "$marker" "$export_dir/from-client.txt"
grep -qx "$marker" "$mnt/from-client.txt"
umount "$mnt"
trap - EXIT
cleanup
EOF

echo "vm-nfs-export[$HOSERVA_LAB_ID]: loopback NFS mount wrote a file"
