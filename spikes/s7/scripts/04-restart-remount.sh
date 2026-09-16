#!/usr/bin/env bash
# Documents the two behaviours the issue asks for by name: a listener
# restart mid-workload, and a filesystem remount — both measured, neither
# assumed.
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

s7_log "== part A: listener restart mid-workload (disk2) =="

s7_start_journal restart-a "$LAB/mnt/disk2" "$S7_RUN/restart-a.log"
head -c 1024 /dev/urandom > "$LAB/mnt/disk2/media/restart-seen-1.bin"
sleep 1
s7_stop_journal restart-a

# The gap: a real change made while no listener is running for this disk.
head -c 1024 /dev/urandom > "$LAB/mnt/disk2/media/restart-gap.bin"

s7_start_journal restart-b "$LAB/mnt/disk2" "$S7_RUN/restart-b.log"
head -c 1024 /dev/urandom > "$LAB/mnt/disk2/media/restart-seen-2.bin"
sleep 1
s7_stop_journal restart-b

s7_log "-- pre-restart listener log --"; cat "$S7_RUN/restart-a.log"
s7_log "-- post-restart listener log --"; cat "$S7_RUN/restart-b.log"

set +e
s7_snapraid diff > "$S7_RUN/restart-diff.log" 2>&1
diff_rc=$?
set -e
cat "$S7_RUN/restart-diff.log"
[[ "$diff_rc" -eq 2 ]] || die "expected snapraid diff to report pending changes (exit 2), got exit $diff_rc"

ca=$(s7_distinct_count "$S7_RUN/restart-a.log")
cb=$(s7_distinct_count "$S7_RUN/restart-b.log")
combined=$((ca + cb))
diff_total=$(s7_diff_changed_count "$S7_RUN/restart-diff.log")

s7_log ""
s7_log "pre-restart distinct paths: $ca, post-restart distinct paths: $cb, combined: $combined"
s7_log "snapraid diff changed (whole array, includes the gap file): $diff_total"
[[ "$combined" -eq $((diff_total - 1)) ]] || die "expected the combined pre+post journal count to undercount the real diff total by exactly the 1 file changed during the gap; got combined=$combined diff_total=$diff_total"
s7_log "CONFIRMED: a listener restart loses exactly the changes made while it was down (here: 1 file, restart-gap.bin) — no error, no self-detected gap marker. A daemon restart must therefore treat the journal as unknown until the next real diff (Q13's UI default), not resume counting as if nothing happened."

s7_snapraid sync

s7_log ""
s7_log "== part B: filesystem remount, same listener process kept running (disk3) =="

before_dev=$(findmnt -n -o SOURCE -- "$LAB/mnt/disk3") || die "disk3 is not mounted"
s7_log "disk3 currently mounted from $before_dev"

s7_start_journal remount-c "$LAB/mnt/disk3" "$S7_RUN/remount-c.log"

head -c 1024 /dev/urandom > "$LAB/mnt/disk3/media/remount-before.bin"
sleep 1
pre_count=$(s7_distinct_count "$S7_RUN/remount-c.log")
s7_log "distinct paths captured before the remount: $pre_count"
[[ "$pre_count" -eq 1 ]] || die "expected the pre-remount write to be captured before remounting, got $pre_count"

s7_log "+ umount $LAB/mnt/disk3 && mount $before_dev $LAB/mnt/disk3"
umount -- "$LAB/mnt/disk3"
mount -- "$before_dev" "$LAB/mnt/disk3"
mountpoint -q -- "$LAB/mnt/disk3" || die "disk3 failed to remount"

head -c 1024 /dev/urandom > "$LAB/mnt/disk3/media/remount-after.bin"
sleep 2

post_count=$(s7_distinct_count "$S7_RUN/remount-c.log")
s7_stop_journal remount-c

s7_log "-- listener log across the remount (same process, same PID throughout) --"
cat "$S7_RUN/remount-c.log"

set +e
s7_snapraid diff > "$S7_RUN/remount-diff.log" 2>&1
diff_rc=$?
set -e
cat "$S7_RUN/remount-diff.log"
[[ "$diff_rc" -eq 2 ]] || die "expected snapraid diff to report pending changes (exit 2), got exit $diff_rc"
diff_total=$(s7_diff_changed_count "$S7_RUN/remount-diff.log")

s7_log ""
s7_log "distinct paths captured after the remount (same listener, same mark): $post_count (before remount: $pre_count)"
s7_log "snapraid diff changed (whole array, includes both remount-before.bin and remount-after.bin): $diff_total"

if [[ "$post_count" -eq "$pre_count" ]]; then
  s7_log "CONFIRMED: a filesystem remount silently ends event delivery to an already-running listener — no error, no exit, the process just stops receiving events for that disk. FAN_MARK_FILESYSTEM is scoped to the mounted filesystem instance (its superblock); unmounting destroys that instance and remounting creates a new one the old mark was never attached to. A production hoservad must re-arm each disk's mark after detecting its filesystem was remounted, not assume a long-lived mark survives it."
else
  s7_log "UNEXPECTED: the listener kept receiving events across the remount (post_count=$post_count > pre_count=$pre_count) — contradicts the fanotify(7)/kernel-source expectation that a filesystem mark is tied to the superblock instance. Recorded as measured, not assumed; see the raw log above."
fi

s7_snapraid sync
