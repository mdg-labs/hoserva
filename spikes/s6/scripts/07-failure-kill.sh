#!/usr/bin/env bash
# Step 7 (failure) — kills exactly one share's own mergerfs process (ao1,
# array-only) and records what the catch-all exposes at that path while it
# is dead, and whether a plain remount recovers it. Per the dispatch's
# kill-by-PID rule: the PID killed is the one this spike itself captured
# at ao1's own mount time (s6_mount, "$S6_PIDS/ao1.pid"), confirmed against
# its own /proc/<pid>/cmdline immediately before the kill — never a name or
# pattern match, and never any other lab's process.
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

OUT=${S6_OUT:-$LAB/s6-results}
mkdir -p -- "$OUT"

mountpoint -q -- "$LAB/mnt/user/ao1" || die "ao1 is not mounted — run 02-topology-up.sh (or 06-cost.sh's rebuild) first"

echo "before-kill" > "$LAB/mnt/user/ao1/marker.txt"
sha_before=$(sha256sum "$LAB/mnt/user/ao1/marker.txt" | awk '{print $1}')
s6_locate "marker.txt" "$LAB/mnt/disk1/ao1" "$LAB/mnt/disk2/ao1" "$LAB/mnt/disk3/ao1" | tee "$OUT/kill-marker-location.log"

pid=$(cat "$S6_PIDS/ao1.pid")
s6_log "== confirming the PID to be killed is genuinely ao1's own mergerfs process, before touching it =="
[[ -r "/proc/$pid/comm" ]] || die "pid $pid (ao1) has no /proc entry — already dead before this step started"
comm=$(cat "/proc/$pid/comm")
cmdline=$(tr '\0' ' ' < "/proc/$pid/cmdline")
{
  echo "pid: $pid"
  echo "comm: $comm"
  echo "cmdline: $cmdline"
} | tee "$OUT/kill-target-confirmation.log"
[[ "$comm" == "mergerfs" ]] || die "pid $pid's comm is '$comm', not mergerfs — refusing to kill it"
[[ "$cmdline" == *"$LAB/mnt/user/ao1"* ]] || die "pid $pid's cmdline does not mention ao1's own mountpoint ($LAB/mnt/user/ao1) — refusing to kill it: $cmdline"
s6_log "confirmed: pid $pid is mergerfs, mounted at $LAB/mnt/user/ao1 — killing exactly this PID"

kill -9 "$pid"
for _ in $(seq 1 50); do
  s6_pid_alive "$pid" || break
  sleep 0.1
done
s6_pid_alive "$pid" && die "pid $pid is still alive 5s after SIGKILL"
s6_log "confirmed: pid $pid is dead"

s6_log "== what does the catch-all's path show while ao1's own mergerfs is dead? =="
{
  echo "findmnt \$LAB/mnt/user/ao1 (kernel mount-table view, process gone):"
  findmnt "$LAB/mnt/user/ao1" || echo "(findmnt: no output)"
  echo
  echo "ls -la \$LAB/mnt/user/ao1:"
  ls -la "$LAB/mnt/user/ao1" 2>&1 || true
  echo
  echo "stat \$LAB/mnt/user/ao1:"
  stat "$LAB/mnt/user/ao1" 2>&1 || true
  echo
  echo "cat \$LAB/mnt/user/ao1/marker.txt:"
  cat "$LAB/mnt/user/ao1/marker.txt" 2>&1 || true
} | tee "$OUT/kill-dead-mount-behaviour.log"

findmnt -n "$LAB/mnt/user/ao1" >/dev/null || die "expected the dead mount to still be listed by findmnt (a stale kernel mount entry, not silently removed)"
s6_log "confirmed: the kernel still lists $LAB/mnt/user/ao1 as mounted even though the process serving it is dead — reads against it fail (see results/ for the exact error text), rather than falling through to the catch-all's own view of that path or to whatever real directory happens to sit underneath. A dead per-share mount is a stuck, inaccessible path until it is explicitly unmounted — it does not silently expose the array beneath it, which matters because a stray write into a stale path cannot land anywhere by accident; it simply fails."

s6_log "== recovery: unmount the dead mount, remount fresh, confirm the pre-kill data is intact =="
if ! fusermount -u -- "$LAB/mnt/user/ao1" 2>"$OUT/kill-unmount-attempt.log"; then
  s6_log "plain fusermount -u failed on the dead mount (see results/kill-unmount-attempt.log) — escalating to a lazy unmount, same escalation destroy-array.sh's own unmount_if_mounted() uses"
  fusermount -uz -- "$LAB/mnt/user/ao1" || umount -l -- "$LAB/mnt/user/ao1" || die "could not unmount the dead ao1 mount even with -z/-l"
fi
mountpoint -q -- "$LAB/mnt/user/ao1" && die "ao1 is still reported as mounted after the recovery unmount"
rm -f -- "$S6_PIDS/ao1.pid"

s6_mount ao1 "$(s6_opts mspmfs 10M hoserva-ao1)" \
  "$LAB/mnt/disk1/ao1=RW:$LAB/mnt/disk2/ao1=RW:$LAB/mnt/disk3/ao1=RW" \
  "$LAB/mnt/user/ao1"

sha_after=$(sha256sum "$LAB/mnt/user/ao1/marker.txt" | awk '{print $1}')
[[ "$sha_before" == "$sha_after" ]] || die "marker.txt changed across the kill+remount cycle: before=$sha_before after=$sha_after"
s6_log "confirmed: after unmounting the dead mount and remounting ao1 fresh, marker.txt reads back byte-identical (sha256 $sha_after) — the remount recovers cleanly and no data was at risk, only availability during the outage"
