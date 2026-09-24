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
#
# Captured rather than left to stream straight through (issue #348): a
# guest that already exports NFS (the seeded existing-host share, and
# any real Hoserva share once array setup has run) can make this fail in
# under a second, and this check's own diagnostic below is what has to
# show the guest's own stderr in the suite log either way, not whatever
# ssh happened to forward live.
OUTPUT="$(vm_ssh 'sudo bash -s' 2>&1 <<'EOF'
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

# exportfs -ra re-processes every entry in /etc/exports and
# /etc/exports.d, including ones this check did not write (Q76: a
# pre-existing /etc/exports is imported, never replaced, and array setup
# leaves its own real shares there too). Before #350, every FUSE-backed
# share RenderNFSExports rendered (/mnt/user/<share> is a mergerfs
# mount) had no fsid=, which made exportfs warn and return non-zero for
# the whole invocation regardless of what this check's own plain-
# directory export did — so this used to tolerate that with `|| true`
# and fall back to confirming its own export via exportfs -v. Now that
# RenderNFSExports carries a name-derived fsid= on every line it emits
# (#350), and array setup's own NFS import re-renders the seeded
# existing-host export through it too (run-l3-suite.sh's array_setup
# step greps the regenerated /etc/exports for the imported share under
# /mnt/user/<name>), exportfs -ra failing here is a real regression
# again, not a known, tolerated defect — strict again.
if ! EXPORTFS_ERR="$(exportfs -ra 2>&1)"; then
  echo "hoserva-nfs-l3: exportfs -ra failed" >&2
  printf '%s\n' "$EXPORTFS_ERR" >&2
  exit 1
fi
EXPORTFS_OUT="$(exportfs -v)"
if ! printf '%s\n' "$EXPORTFS_OUT" | grep -qF "$export_dir"; then
  printf '%s\n' "$EXPORTFS_OUT" >&2
  echo "hoserva-nfs-l3: exportfs -ra did not install $export_dir — see exportfs -v output above" >&2
  exit 1
fi

mount -t nfs -o vers=3,nolock 127.0.0.1:"$export_dir" "$mnt"
printf '%s\n' "$marker" > "$mnt/from-client.txt"
grep -qx "$marker" "$export_dir/from-client.txt"
grep -qx "$marker" "$mnt/from-client.txt"
umount "$mnt"
trap - EXIT
cleanup
EOF
)" && STATUS=0 || STATUS=$?

printf '%s\n' "$OUTPUT"
if [[ "$STATUS" -ne 0 ]]; then
  echo "nfs-export-check[$HOSERVA_LAB_ID]: guest-side check failed (exit $STATUS) — see its own output above" >&2
  exit "$STATUS"
fi

echo "vm-nfs-export[$HOSERVA_LAB_ID]: loopback NFS mount wrote a file"
