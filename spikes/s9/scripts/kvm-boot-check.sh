#!/usr/bin/env bash
# Boots a minimal guest with -enable-kvm on the runner's own kernel and a
# throwaway busybox initramfs (no network fetch, no external kernel image —
# both come from packages the runner's own apt sources already provide),
# to a printed marker line and a clean poweroff, then confirms QEMU used
# KVM acceleration rather than falling back to TCG software emulation.
#
# This can only run where /dev/kvm exists and is accessible — the calling
# workflow applies the udev rule and settles first (S9, issue #10).
#
# NEVER run against qemu:///system, and never anywhere but an ephemeral
# hosted runner or this issue's own probe — CLAUDE.md's VM rules are about
# Hoserva's own L3 test VM and the maintainer's libvirt session, neither of
# which this touches.
set -euo pipefail

WORK=$(mktemp -d)
trap 'rm -rf -- "$WORK"' EXIT

echo "## Installing qemu-system-x86 and busybox-static"
sudo apt-get update -qq
sudo apt-get install -y -qq --no-install-recommends qemu-system-x86 busybox-static

KERNEL="/boot/vmlinuz-$(uname -r)"
[[ -r "$KERNEL" ]] || { echo "no readable kernel at $KERNEL — cannot boot a guest" >&2; exit 1; }

echo "## Building a one-file initramfs (busybox init, prints a marker, powers off)"
INITRAMFS_DIR="$WORK/initramfs"
mkdir -p -- "$INITRAMFS_DIR/bin"
cp "$(command -v busybox)" "$INITRAMFS_DIR/bin/busybox"
cat > "$INITRAMFS_DIR/init" <<'EOF'
#!/bin/busybox sh
/bin/busybox mkdir -p /proc
/bin/busybox mount -t proc proc /proc
/bin/busybox echo "HOSERVA-S9-KVM-GUEST-BOOTED"
/bin/busybox cat /proc/cpuinfo | /bin/busybox grep -m1 hypervisor && /bin/busybox echo "HOSERVA-S9-HYPERVISOR-FLAG-PRESENT"
/bin/busybox poweroff -f
EOF
chmod +x "$INITRAMFS_DIR/init"
( cd "$INITRAMFS_DIR" && find . | cpio -o -H newc 2>/dev/null | gzip -1 ) > "$WORK/initramfs.cpio.gz"

SERIAL_LOG="$WORK/serial.log"
QEMU_ERR="$WORK/qemu.stderr.log"

echo "## Booting (kernel: $KERNEL, timeout 90s)"
SECONDS=0
timeout 90 qemu-system-x86_64 \
  -enable-kvm -cpu host -m 256 -smp 1 \
  -kernel "$KERNEL" \
  -initrd "$WORK/initramfs.cpio.gz" \
  -append "console=ttyS0 panic=-1 quiet" \
  -display none -serial "file:$SERIAL_LOG" -no-reboot \
  2> "$QEMU_ERR"
rc=$?
elapsed=$SECONDS

echo "## qemu-system-x86_64 exit $rc, wall time ${elapsed}s"
echo "## qemu stderr:"
cat "$QEMU_ERR" || true
echo "## guest serial console:"
cat "$SERIAL_LOG" || true

if grep -qi 'failed to initialize KVM\|could not access KVM kernel module' "$QEMU_ERR"; then
  echo "FAIL: QEMU reported it could not use KVM — see stderr above" >&2
  exit 1
fi

grep -q 'HOSERVA-S9-KVM-GUEST-BOOTED' "$SERIAL_LOG" || {
  echo "FAIL: guest never printed its boot marker — see serial console above" >&2
  exit 1
}

grep -q 'HOSERVA-S9-HYPERVISOR-FLAG-PRESENT' "$SERIAL_LOG" || {
  echo "FAIL: guest booted but /proc/cpuinfo never showed a hypervisor flag — cannot confirm KVM was actually accelerating this boot, only that QEMU didn't refuse -enable-kvm outright" >&2
  exit 1
}

echo "PASS: guest booted to its own init, /proc/cpuinfo confirmed a hypervisor, in ${elapsed}s wall time — consistent with KVM acceleration, not TCG (a software-emulated boot of a real kernel this way is minutes, not seconds)"
