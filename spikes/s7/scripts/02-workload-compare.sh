#!/usr/bin/env bash
# The acceptance test itself: one FAN_MARK_FILESYSTEM journal per data
# disk, a scripted workload covering every operation the issue names
# (create, overwrite, append, delete, rename within and across
# directories, touch — each both directly on a disk and through the
# mergerfs pool), then a `snapraid diff` — and the two counts, side by
# side.
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

s7_log "== starting one journal per data disk =="
s7_start_journal wl-d1 "$LAB/mnt/disk1" "$S7_RUN/wl-d1.log"
s7_start_journal wl-d2 "$LAB/mnt/disk2" "$S7_RUN/wl-d2.log"
s7_start_journal wl-d3 "$LAB/mnt/disk3" "$S7_RUN/wl-d3.log"

# Extra pool baseline files this workload needs, seeded and synced now so
# the workload's own pool operations below are genuine changes against a
# known last-sync state, not new-file noise from the seeding itself.
mkdir -p -- "$LAB/mnt/disk3/tmp"
head -c 2048 /dev/urandom > "$LAB/mnt/disk3/media/rename-src.bin"
head -c 2048 /dev/urandom > "$LAB/mnt/disk2/media/touchme.bin"
head -c 2048 /dev/urandom > "$LAB/mnt/user/pool-baseline/delete-me.bin"
head -c 2048 /dev/urandom > "$LAB/mnt/user/pool-baseline/rename-me.bin"
s7_snapraid sync

s7_log ""
s7_log "== direct-disk workload =="
# create
head -c 1024 /dev/urandom > "$LAB/mnt/disk1/media/new-direct.bin"
# overwrite
head -c 1024 /dev/urandom > "$LAB/mnt/disk2/media/b.bin"
# append
head -c 512 /dev/urandom >> "$LAB/mnt/disk3/media/c.bin"
# delete
rm -f -- "$LAB/mnt/disk1/docs/report.txt"
# rename, same directory
mv -- "$LAB/mnt/disk2/docs/notes.txt" "$LAB/mnt/disk2/docs/notes-renamed.txt"
# rename, across directories, same disk
mv -- "$LAB/mnt/disk3/media/rename-src.bin" "$LAB/mnt/disk3/tmp/rename-dst.bin"
# copy, same basename elsewhere, timestamp preserved (snapraid.txt §5.5's
# "copied" classification, per spike S5's own worked example)
cp -p -- "$LAB/mnt/disk1/media/a.bin" "$LAB/mnt/disk1/archive/a.bin"
# touch, existing file, content unchanged
touch -- "$LAB/mnt/disk2/media/touchme.bin"
# touch, brand-new file
touch -- "$LAB/mnt/disk1/media/touched-new.bin"

s7_log ""
s7_log "== through-the-pool workload =="
s7_log "pool-baseline files before this workload:"
s7_locate_disk pool-baseline/seed.bin
s7_locate_disk pool-baseline/delete-me.bin
s7_locate_disk pool-baseline/rename-me.bin
# create
head -c 1024 /dev/urandom > "$LAB/mnt/user/pool-baseline/new-via-pool.bin"
# overwrite
head -c 1024 /dev/urandom > "$LAB/mnt/user/pool-baseline/seed.bin"
# delete
rm -f -- "$LAB/mnt/user/pool-baseline/delete-me.bin"
# rename
mv -- "$LAB/mnt/user/pool-baseline/rename-me.bin" "$LAB/mnt/user/pool-baseline/renamed-via-pool.bin"

# Give the kernel a moment to actually deliver the last events before
# stopping the listeners — fanotify delivery is not synchronous with the
# syscall that caused it.
sleep 2

s7_log ""
s7_log "== stopping journals =="
s7_stop_journal wl-d1
s7_stop_journal wl-d2
s7_stop_journal wl-d3

for d in 1 2 3; do
  s7_log "-- disk$d journal raw log ($S7_RUN/wl-d$d.log) --"
  cat "$S7_RUN/wl-d$d.log"
done

s7_log ""
s7_log "== snapraid diff =="
set +e
s7_snapraid diff > "$S7_RUN/diff.log" 2>&1
diff_rc=$?
set -e
cat "$S7_RUN/diff.log"
[[ "$diff_rc" -eq 2 ]] || die "expected snapraid diff to report pending changes (exit 2), got exit $diff_rc"

j1=$(s7_distinct_count "$S7_RUN/wl-d1.log")
j2=$(s7_distinct_count "$S7_RUN/wl-d2.log")
j3=$(s7_distinct_count "$S7_RUN/wl-d3.log")
journal_total=$((j1 + j2 + j3))
diff_total=$(s7_diff_changed_count "$S7_RUN/diff.log")

o1=$(s7_had_overflow "$S7_RUN/wl-d1.log")
o2=$(s7_had_overflow "$S7_RUN/wl-d2.log")
o3=$(s7_had_overflow "$S7_RUN/wl-d3.log")

s7_log ""
s7_log "== result =="
s7_log "journal distinct paths: disk1=$j1 disk2=$j2 disk3=$j3 total=$journal_total (overflow: d1=$o1 d2=$o2 d3=$o3)"
s7_log "snapraid diff changed (added+removed+updated+copied+restored): $diff_total"

[[ "$o1" -eq 0 && "$o2" -eq 0 && "$o3" -eq 0 ]] || die "unexpected overflow during the normal workload — counts below would be meaningless"
[[ "$journal_total" -eq "$diff_total" ]] || die "MISMATCH: journal total ($journal_total) != snapraid diff total ($diff_total) — see $S7_RUN/diff.log and $S7_RUN/wl-d*.log above"

s7_log "PASS: journal distinct-path total ($journal_total) matches snapraid diff's changed total ($diff_total)"

# Re-sync so later steps in this spike start from a clean baseline again.
s7_snapraid sync
