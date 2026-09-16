#!/usr/bin/env bash
# Runs inside one branch's disposable probe container (invoked by
# run-apparmor-check.sh via `docker run`, with or without
# `--security-opt apparmor=unconfined`). This is the loop-free mediation
# probe requirement F asks for alongside the faithful one
# (apparmor-loop-probe.sh): the actual question is whether Docker's
# default AppArmor profile denies the `mount(2)` the lab recipe needs, and
# a bare `mount -t tmpfs` isolates that question from every loop-device,
# XFS-minimum-size, and host-global-device-attribution concern that has
# corrupted this experiment three times running. No `lab_assert_own_loop`
# call here on purpose: a tmpfs mount creates no loop device and nothing
# host-global to attribute or race against — the entire reason this branch
# exists is to have nothing left to get wrong except the one syscall under
# test.
#
# $1: LAB — this branch's own directory, bind-mounted from a fresh host
#           mktemp -d (mirrors apparmor-loop-probe.sh's argument, even
#           though this probe writes far less into it, so
#           run-apparmor-check.sh can read both probes' results the same
#           way).
set -euo pipefail

LAB="${1:?usage: apparmor-tmpfs-probe.sh LAB}"
mkdir -p -- "$LAB/result" "$LAB/mnt"

echo "mount" > "$LAB/result/step"
if mount -t tmpfs none "$LAB/mnt" 2>"$LAB/result/stderr"; then
  echo "0" > "$LAB/result/exit"
  umount -- "$LAB/mnt" || echo "warning: umount $LAB/mnt failed after a successful tmpfs mount" >&2
else
  mount_exit=$?
  echo "$mount_exit" > "$LAB/result/exit"
fi

dmesg 2>/dev/null | tail -n 200 > "$LAB/result/dmesg-tail.log" || : > "$LAB/result/dmesg-tail.log"
journalctl -k -n 200 --no-pager 2>/dev/null > "$LAB/result/journalctl-k-tail.log" || : > "$LAB/result/journalctl-k-tail.log"
{ cat "$LAB/result/dmesg-tail.log" "$LAB/result/journalctl-k-tail.log" 2>/dev/null | grep -i 'apparmor' || true; } > "$LAB/result/audit-apparmor-lines.log"

chmod -R a+rwX "$LAB" 2>/dev/null || true
exit 0
