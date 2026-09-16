#!/usr/bin/env bash
# Boots a minimal guest with KVM acceleration and confirms the acceleration
# is real, not a silent TCG fallback (S9, issue #10).
#
# Fix history, so the next reader doesn't have to reconstruct it from the
# commit log: attempt 1 (landed) tried the *installed* kernel from
# `apt-get install linux-image-generic`; on a real hosted run
# (35049304081) it never got that far — `/boot/vmlinuz-6.17.0-1022-azure`,
# the runner's own currently-running kernel, was already there but 0600
# root:root, and this script's own readability guard correctly refused it.
# A rejected attempt 2 tried installing a *fresh* kernel package instead —
# reviewed and rejected, because `linux-image-generic`'s own postinst
# leaves its `/boot/vmlinuz-*` at the same 0600 root:root (verified
# independently against the identical package on `ubuntu:24.04`), so it
# would have failed the exact same way, just later.
#
# This version copies the *running* kernel out with `sudo` and `chown`s
# the copy to the calling (unprivileged) user — the runner's own
# passwordless, job-scoped `sudo`, already proven to work earlier in this
# same job (the udev-rule step). This needs no extra package, no version
# mismatch between the copy and what's actually running, and the read
# itself happens under the exact non-root boundary the guest-boot step
# uses. Validated under that boundary, not assumed:
# `spikes/s9/results/kernel-acquisition-nonroot.log`.
#
# Acceleration evidence no longer rests solely on the guest's own
# `/proc/cpuinfo` self-report (a check a payload without a guest kernel
# could not perform at all, and which a KVM host that still silently fell
# back to TCG could in principle spoof). It now also comes from the
# QEMU host process itself, queried over QMP: `-accel kvm` alone (no
# `,tcg` fallback list) makes QEMU refuse to start at all if KVM is
# unavailable, and `query-kvm` is asked directly whether it is using it.
# Both checks run; either one failing fails the script loudly.
#
# HOSERVA_S9_ACCEL overrides the accelerator for local plumbing validation
# only (e.g. `tcg`, to exercise this script's boot/QMP mechanics inside a
# disposable container with no /dev/kvm). The real workflow never sets
# this, so its default (kvm) and its full assertion set are unchanged.
# A non-kvm run never claims KVM acceleration and says so on every line
# that could otherwise be misread.
set -euo pipefail

ACCEL="${HOSERVA_S9_ACCEL:-kvm}"
CPU="host"
if [[ "$ACCEL" != "kvm" ]]; then
  CPU="max"
  echo "## HOSERVA_S9_ACCEL=$ACCEL — this is a LOCAL PLUMBING VALIDATION ONLY." >&2
  echo "## It exercises the boot/QMP mechanics under software emulation and PROVES NOTHING about KVM acceleration." >&2
fi

WORK=$(mktemp -d)
trap 'rm -rf -- "$WORK"' EXIT

if [[ "$ACCEL" == "kvm" ]]; then
  [[ -e /dev/kvm ]] || { echo "no /dev/kvm — cannot request kvm acceleration" >&2; exit 1; }
  [[ -r /dev/kvm && -w /dev/kvm ]] || { echo "/dev/kvm exists but is not read/write for $(id -un) — the udev rule did not take effect" >&2; exit 1; }
fi

echo "## Installing qemu-system-x86, busybox-static, jq (and python3 if missing)"
sudo apt-get update -qq
sudo apt-get install -y -qq --no-install-recommends qemu-system-x86 busybox-static jq
command -v python3 >/dev/null || sudo apt-get install -y -qq --no-install-recommends python3

echo "## Acquiring the running kernel readably (sudo cp + chown, as the unprivileged user)"
RUNNING_KERNEL="/boot/vmlinuz-$(uname -r)"
[[ -e "$RUNNING_KERNEL" ]] || { echo "no kernel image at $RUNNING_KERNEL for the running kernel — cannot proceed" >&2; exit 1; }

KERNEL="$WORK/vmlinuz"
sudo cp -- "$RUNNING_KERNEL" "$KERNEL"
sudo chown "$(id -u):$(id -g)" "$KERNEL"
chmod 0644 "$KERNEL"
[[ -r "$KERNEL" ]] || { echo "still unreadable after sudo cp+chown — the acquisition mechanism failed, not just a permission assumption" >&2; exit 1; }
echo "## Kernel acquired: $RUNNING_KERNEL -> $KERNEL ($(stat -c '%U:%G %a' "$KERNEL"))"

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

# The QMP client: connects, negotiates capabilities, asks query-kvm, writes
# the raw reply, then resumes the guest (started paused via -S so nothing
# runs before this query happens — no race between guest boot speed and
# the QMP client connecting).
cat > "$WORK/qmp_client.py" <<'PY'
import json
import socket
import sys
import time

sock_path, out_path = sys.argv[1], sys.argv[2]

sock = None
deadline = time.time() + 30
while time.time() < deadline:
    try:
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        sock.settimeout(5)
        sock.connect(sock_path)
        break
    except (FileNotFoundError, ConnectionRefusedError):
        sock = None
        time.sleep(0.2)
if sock is None:
    print("QMP socket never became connectable", file=sys.stderr)
    sys.exit(1)

f = sock.makefile("rwb")


def send(obj):
    f.write((json.dumps(obj) + "\n").encode())
    f.flush()


greeting = f.readline()
if not greeting:
    print("QMP: no greeting from QEMU", file=sys.stderr)
    sys.exit(1)

send({"execute": "qmp_capabilities"})
cap_ack = f.readline()
if not cap_ack:
    print("QMP: no reply to qmp_capabilities", file=sys.stderr)
    sys.exit(1)

send({"execute": "query-kvm"})
reply = f.readline()
if not reply:
    print("QMP: no reply to query-kvm", file=sys.stderr)
    sys.exit(1)

with open(out_path, "w", encoding="utf-8") as out:
    out.write(reply.decode())

send({"execute": "cont"})
f.readline()
sock.close()
PY

QMP_SOCK="$WORK/qmp.sock"
SERIAL_LOG="$WORK/serial.log"
QEMU_ERR="$WORK/qemu.stderr.log"
QMP_RESULT="$WORK/qmp-query-kvm.json"

echo "## Booting (kernel: $KERNEL, accel: $ACCEL, timeout 90s), started paused pending the QMP query-kvm check"
qemu-system-x86_64 \
  -M pc \
  -accel "$ACCEL" \
  -cpu "$CPU" -m 256 -smp 1 \
  -kernel "$KERNEL" \
  -initrd "$WORK/initramfs.cpio.gz" \
  -append "console=ttyS0 panic=-1 quiet" \
  -display none -serial "file:$SERIAL_LOG" -no-reboot -S \
  -qmp "unix:$QMP_SOCK,server=on,wait=off" \
  2> "$QEMU_ERR" &
QEMU_PID=$!

if ! python3 "$WORK/qmp_client.py" "$QMP_SOCK" "$QMP_RESULT"; then
  echo "FAIL: could not complete the QMP query-kvm handshake — see stderr above" >&2
  kill "$QEMU_PID" 2>/dev/null || true
  wait "$QEMU_PID" 2>/dev/null || true
  cat "$QEMU_ERR" || true
  exit 1
fi

echo "## QMP query-kvm reply: $(cat "$QMP_RESULT")"

SECONDS=0
DEADLINE=90
while kill -0 "$QEMU_PID" 2>/dev/null; do
  if (( SECONDS > DEADLINE )); then
    echo "FAIL: qemu (pid $QEMU_PID) did not exit within ${DEADLINE}s — killing it" >&2
    kill "$QEMU_PID" 2>/dev/null || true
    wait "$QEMU_PID" 2>/dev/null || true
    cat "$QEMU_ERR" || true
    exit 1
  fi
  sleep 1
done
wait "$QEMU_PID"
QEMU_RC=$?
ELAPSED=$SECONDS

echo "## qemu-system-x86_64 exit $QEMU_RC, wall time ${ELAPSED}s"
echo "## qemu stderr:"
cat "$QEMU_ERR" || true
echo "## guest serial console:"
cat "$SERIAL_LOG" || true

FAIL=0

if [[ "$QEMU_RC" -ne 0 ]]; then
  echo "FAIL: qemu-system-x86_64 exited $QEMU_RC" >&2
  FAIL=1
fi

if grep -qi 'failed to initialize KVM\|could not access KVM kernel module' "$QEMU_ERR"; then
  echo "FAIL: QEMU reported it could not use KVM — see stderr above" >&2
  FAIL=1
fi

grep -q 'HOSERVA-S9-KVM-GUEST-BOOTED' "$SERIAL_LOG" || {
  echo "FAIL: guest never printed its boot marker — see serial console above" >&2
  FAIL=1
}

# NOT `.return.enabled // "missing"`: jq's `//` treats a legitimate `false`
# value the same as null/absent, so a genuine "not accelerated" answer would
# print as "missing" instead of "false" — caught by this script's own local
# TCG plumbing validation, spikes/s9/results/tcg-plumbing-check.log.
QMP_ENABLED=$(jq -r 'if (.return.enabled|type) == "boolean" then (.return.enabled|tostring) else "missing" end' "$QMP_RESULT" 2>/dev/null || echo "unparseable")
QMP_PRESENT=$(jq -r 'if (.return.present|type) == "boolean" then (.return.present|tostring) else "missing" end' "$QMP_RESULT" 2>/dev/null || echo "unparseable")

if [[ "$ACCEL" == "kvm" ]]; then
  if [[ "$QMP_ENABLED" != "true" || "$QMP_PRESENT" != "true" ]]; then
    echo "FAIL: QMP query-kvm did not report {enabled: true, present: true} — got enabled=$QMP_ENABLED present=$QMP_PRESENT" >&2
    FAIL=1
  fi
  grep -q 'HOSERVA-S9-HYPERVISOR-FLAG-PRESENT' "$SERIAL_LOG" || {
    echo "FAIL: guest booted but /proc/cpuinfo never showed a hypervisor flag — the QMP check above is the primary evidence, but this corroborating guest-side signal is expected too and did not appear" >&2
    FAIL=1
  }
else
  echo "## (accel=$ACCEL, not kvm) QMP reported enabled=$QMP_ENABLED present=$QMP_PRESENT — expected, not evidence of anything about a real KVM host"
fi

if [[ "$FAIL" -ne 0 ]]; then
  exit 1
fi

if [[ "$ACCEL" == "kvm" ]]; then
  echo "PASS: guest booted to its own init in ${ELAPSED}s; QMP query-kvm reported {enabled: true, present: true}; /proc/cpuinfo also showed a hypervisor flag — KVM acceleration confirmed by the host (QMP), not only inferred from the guest"
else
  echo "TCG-PLUMBING-OK: boot and QMP mechanics work under software emulation (accel=$ACCEL, ${ELAPSED}s). This is NOT evidence of KVM acceleration on any real host."
fi
