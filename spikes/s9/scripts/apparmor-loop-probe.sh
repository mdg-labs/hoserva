#!/usr/bin/env bash
# Runs *inside* one branch's disposable probe container (invoked by
# run-apparmor-check.sh via `docker run`, with or without
# `--security-opt apparmor=unconfined`). Faithfully reproduces the real
# lab recipe's loop/XFS mechanics (scripts/devenv/entrypoint.sh's mknod
# loop, scripts/devenv/create-array.sh's attach/format/mount) so the
# AppArmor question is answered against the actual thing the lab does, not
# a simplified stand-in — see apparmor-tmpfs-probe.sh for that simplified,
# loop-free check, run alongside this one, never instead of it (S9,
# requirement F).
#
# Never invents attribution logic: sources scripts/devenv/lib.sh (bind-
# mounted read-only from the real repo path, not copied) and calls its
# lab_assert_own_loop() exactly as scripts/devenv/create-array.sh does,
# because loop devices are host-global and nothing else in this container
# proves the device `losetup --find` handed back is bound to *this
# branch's own image* rather than a device left behind by another branch
# or another job (CLAUDE.md: orchestrate, never reimplement; requirement C).
#
# Writes every result as a plain file under $LAB/result/ instead of
# encoding pass/fail in this script's own exit status: the caller
# (run-apparmor-check.sh) needs the *classification inputs* — which step
# failed, its exit code, its stderr, whether attribution and the detach
# barrier held — not a single collapsed boolean, or the misclassification
# this file exists to prevent just moves one level up.
#
# $1: LAB   — this branch's own directory, bind-mounted from a fresh host
#             mktemp -d (never shared with another branch or the real
#             lab's .lab/<id> tree)
set -euo pipefail

LAB="${1:?usage: apparmor-loop-probe.sh LAB}"
LIB="/opt/hoserva-lib/lib.sh"

[[ -r "$LIB" ]] || { echo "INTERNAL_VOID: $LIB not mounted — cannot reuse lab_assert_own_loop" >&2; exit 97; }
# shellcheck source=/dev/null
source "$LIB"

mkdir -p -- "$LAB/img" "$LAB/result"

# No exit/step/stderr placeholder is written here. A previous version of
# this script wrote a success-shaped placeholder (exit=0, step=none, empty
# stderr) at this point, before apt-get, mknod, losetup or mkfs.xfs/mount
# ever ran — so a routine failure earlier than the mount step (an apt
# mirror flake, a missing tool) killed the script under `set -euo
# pipefail` with a nonzero, non-97 exit code while leaving that
# placeholder file behind unedited, and a caller reading only "is the file
# present" read it as SUCCESS. The fix is to never write these three files
# until the mount step's real outcome is known: on every path below (the
# two `else` branches, and the fall-through success case at the bottom of
# the mkfs.xfs/mount block) they are the truth or they don't exist yet —
# never a guess written before the fact. run-apparmor-check.sh's
# read_result/classify_branch_pair (spikes/s9/scripts/apparmor-classify.sh)
# already fall back to the container's own real docker exit status when a
# result file is absent, so "doesn't exist yet" is exactly as safe as
# "never got there" ought to be, with no new mechanism required — and that
# reader is now hardened a second way too, so it no longer needs this
# script to get the write-order right to stay correct (see
# apparmor-classify.sh's classify_branch_pair for that half of the fix).
#
# attribution/detach keep their own "unknown" placeholder: neither field
# is ever read by classify_branch_pair (only exit/stderr/audit feed a
# verdict), so a stale "unknown" here cannot manufacture a false verdict —
# it only ever affects this branch's own diagnostics line, where "unknown"
# is already the truthful answer for a branch that hasn't reached that
# point yet.
echo "unknown" > "$LAB/result/attribution"
echo "unknown" > "$LAB/result/detach"

for i in $(seq 0 63); do
  [[ -e "/dev/loop$i" ]] || mknod -m 660 "/dev/loop$i" b 7 "$i"
done

apt-get update -qq
apt-get install -y -qq --no-install-recommends util-linux xfsprogs >/dev/null

img="$LAB/img/probe.img"
truncate -s 512M "$img"

dev=$(losetup --find --show -- "$img")
echo "$dev" > "$LAB/result/device"
losetup -a > "$LAB/result/losetup-a-after-attach.log" 2>&1 || true

# lab_assert_own_loop calls lib.sh's own die() on failure, which is a bare
# `exit 1` — run it in a subshell so that exit only ends the subshell, and
# this script can tell an attribution failure apart from every other
# failure by its own distinct exit code (97) and INTERNAL_VOID marker,
# never conflating it with a mount/mkfs result classify_mount_failure
# would otherwise have to guess about.
if ! ( lab_assert_own_loop "$dev" "$img" ) 2>"$LAB/result/attribution.err"; then
  echo "FAILED" > "$LAB/result/attribution"
  cat "$LAB/result/attribution.err" >&2
  echo "INTERNAL_VOID: device $dev did not resolve back to $img via losetup -j — refusing to trust it (requirement C)" >&2
  losetup -d "$dev" 2>/dev/null || true
  exit 97
fi
echo "OK" > "$LAB/result/attribution"

mkdir -p "$LAB/mnt"
# Neither command runs under `set -e` here — both sit in an `if`
# condition, bash's one standard exception to errexit — so `$?` in each
# `else` branch is that command's own real exit status, not the `if`
# construct's (a bare `if ! cmd; then mount_exit=$?` would have collapsed
# every failure to 0, since `!` resets `$?` for the negated test itself).
if mkfs.xfs -q "$dev" 2>"$LAB/result/stderr"; then
  if mount -- "$dev" "$LAB/mnt" 2>"$LAB/result/stderr"; then
    echo ok > "$LAB/mnt/marker"
    umount -- "$LAB/mnt" || echo "warning: umount $LAB/mnt failed after a successful mount" >&2
  else
    mount_exit=$?
    echo "mount" > "$LAB/result/step"
    echo "$mount_exit" > "$LAB/result/exit"
  fi
else
  mount_exit=$?
  echo "mkfs.xfs" > "$LAB/result/step"
  echo "$mount_exit" > "$LAB/result/exit"
fi

# Best-effort kernel/audit corroboration (requirement E). Neither source is
# guaranteed available to an unprivileged container on every runner image —
# both are captured if present, left empty if not, and the classifier
# (apparmor-classify.sh) never requires either to reach a DENIED verdict,
# only uses them to corroborate one.
dmesg 2>/dev/null | tail -n 200 > "$LAB/result/dmesg-tail.log" || : > "$LAB/result/dmesg-tail.log"
journalctl -k -n 200 --no-pager 2>/dev/null > "$LAB/result/journalctl-k-tail.log" || : > "$LAB/result/journalctl-k-tail.log"
{ cat "$LAB/result/dmesg-tail.log" "$LAB/result/journalctl-k-tail.log" 2>/dev/null | grep -i 'apparmor' || true; } > "$LAB/result/audit-apparmor-lines.log"
ls /sys/kernel/security/apparmor/profiles > "$LAB/result/apparmor-profiles-listing.log" 2>&1 || \
  echo "not available in this container" > "$LAB/result/apparmor-profiles-listing.log"

# Detach barrier (requirement D): poll, bounded, until the kernel no longer
# attributes any loop device to *this image* before this branch's
# container exits — the next branch's `losetup --find` must never be able
# to race a still-releasing device from this one. Run 35057453620's own
# EBUSY happened here: the variant's mount hit a device the previous
# branch's detach had not actually finished releasing yet.
losetup -d "$dev" 2>/dev/null || true
detach_ok=0
for _ in $(seq 1 20); do
  remaining=$(losetup -j -- "$img" --output NAME --noheadings 2>/dev/null | tr -d '[:space:]')
  [[ -z "$remaining" ]] && { detach_ok=1; break; }
  sleep 0.5
done
if [[ "$detach_ok" -eq 1 ]]; then
  echo "OK" > "$LAB/result/detach"
else
  echo "TIMEOUT" > "$LAB/result/detach"
  echo "INTERNAL_VOID: $img still attached to a loop device after a 10s poll — detach never confirmed (requirement D)" >&2
fi
losetup -a > "$LAB/result/losetup-a-final.log" 2>&1 || true

chmod -R a+rwX "$LAB" 2>/dev/null || true

if [[ "$detach_ok" -ne 1 ]]; then
  exit 97
fi
exit 0
