#!/usr/bin/env bash
# Overflow detection. /proc/sys/fs/fanotify/max_queued_events cannot be
# lowered in this lab (/proc/sys is bind-mounted read-only) — so this
# reads the real value and generates a burst comfortably larger than it,
# with the listener started but deliberately not yet reading (-delay), so
# the burst queues up undrained. A second run at the same burst size with
# FAN_UNLIMITED_QUEUE is the control: same size, no overflow, every event
# still counted — proving the check can tell a genuinely overflowed run
# from a genuinely clean one, not just report "0 overflow lines" whenever
# something goes wrong.
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

MAXQ=$(cat /proc/sys/fs/fanotify/max_queued_events)
[[ "$MAXQ" =~ ^[0-9]+$ ]] || die "could not read /proc/sys/fs/fanotify/max_queued_events"
s7_log "fs.fanotify.max_queued_events = $MAXQ (read-only in this lab — not lowered)"

MARGIN=$((MAXQ / 4))
[[ "$MARGIN" -lt 5000 ]] && MARGIN=5000
N=$((MAXQ + MARGIN))
s7_log "burst size N = $N (max_queued_events + margin of $MARGIN); each touch generates 2 raw fanotify events (CREATE, CLOSE_WRITE), so this run queues roughly $((N * 2)) raw events against a $MAXQ-event queue"

DELAY=15
DURATION=30

flood() {
  local dir=$1
  mkdir -p -- "$dir"
  s7_log "+ creating $N files under $dir (xargs -n 1000 touch)"
  local start end
  start=$(date +%s.%N)
  seq 1 "$N" | awk -v d="$dir" '{print d "/f" $1}' | xargs -n 1000 touch
  end=$(date +%s.%N)
  s7_log "flood done in $(awk -v s="$start" -v e="$end" 'BEGIN{printf "%.1f", e-s}')s (delay budget was ${DELAY}s)"
}

s7_log ""
s7_log "== run A: default (bounded) queue — expect overflow =="
s7_start_journal ovf-a "$LAB/mnt/disk1" "$S7_RUN/ovf-a.log" -delay "${DELAY}s" -duration "${DURATION}s"
flood "$LAB/mnt/disk1/flood-a"
a_pid=$(cat "$S7_RUN/ovf-a.pid")
s7_log "waiting for pid $a_pid (journal) to exit on its own -duration timeout..."
wait "$a_pid" 2>/dev/null || true
s7_log "-- run A log tail (last 5 lines) --"
tail -n 5 "$S7_RUN/ovf-a.log"
a_lines=$(wc -l < "$S7_RUN/ovf-a.log" | tr -d ' ')
a_distinct=$(s7_distinct_count "$S7_RUN/ovf-a.log")
a_overflow=$(s7_had_overflow "$S7_RUN/ovf-a.log")
s7_log "run A: $a_lines raw log lines, $a_distinct distinct paths captured (of $N created), overflow marker present: $a_overflow"
[[ "$a_overflow" -eq 1 ]] || die "expected FAN_Q_OVERFLOW in run A (bounded queue, burst=$N > max_queued=$MAXQ) but none was recorded"
[[ "$a_distinct" -lt "$N" ]] || die "expected run A to have DROPPED at least some events (distinct $a_distinct should be < N=$N) — if this holds, the burst wasn't actually large enough to overflow the queue, and the overflow line above is unexplained"
s7_log "CONFIRMED: overflow detected and the run under-counted ($a_distinct of $N) — a production journal must mark itself incomplete rather than present $a_distinct as a real count, exactly as Q13's UI default (approximate; 'unknown until the next sync' on overflow) requires."

s7_log ""
s7_log "== run B: FAN_UNLIMITED_QUEUE, same burst size — control, expect no overflow =="
s7_start_journal ovf-b "$LAB/mnt/disk1" "$S7_RUN/ovf-b.log" -unlimited -delay "${DELAY}s" -duration "${DURATION}s"
flood "$LAB/mnt/disk1/flood-b"
b_pid=$(cat "$S7_RUN/ovf-b.pid")
s7_log "waiting for pid $b_pid (journal) to exit on its own -duration timeout..."
wait "$b_pid" 2>/dev/null || true
s7_log "-- run B log tail (last 5 lines) --"
tail -n 5 "$S7_RUN/ovf-b.log"
b_lines=$(wc -l < "$S7_RUN/ovf-b.log" | tr -d ' ')
b_distinct=$(s7_distinct_count "$S7_RUN/ovf-b.log")
b_overflow=$(s7_had_overflow "$S7_RUN/ovf-b.log")
s7_log "run B: $b_lines raw log lines, $b_distinct distinct paths captured (of $N created), overflow marker present: $b_overflow"
[[ "$b_overflow" -eq 0 ]] || die "run B used FAN_UNLIMITED_QUEUE at the same burst size as run A and still overflowed — the control itself is broken, investigate before trusting run A's result"
[[ "$b_distinct" -eq "$N" ]] || die "run B (FAN_UNLIMITED_QUEUE) should have captured every one of the $N files, got $b_distinct"
s7_log "CONFIRMED (control): the same burst size with FAN_UNLIMITED_QUEUE drops nothing ($b_distinct == N == $N, no overflow) — proving run A's overflow and undercount are a real effect of the bounded queue, not an artifact of the burst, the listener, or the counting method."

rm -rf -- "$LAB/mnt/disk1/flood-a" "$LAB/mnt/disk1/flood-b"
