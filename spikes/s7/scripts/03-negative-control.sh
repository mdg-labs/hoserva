#!/usr/bin/env bash
# Methodological control (dispatch requirement): 02's comparison passing
# proves the counts happened to match. It does not by itself prove the
# comparison can *notice* a real gap. This script deliberately creates one
# — a change made while no journal is running — and asserts the
# journal/diff comparison correctly reports a mismatch. If it does not,
# the comparator itself is broken and 02's PASS would not be trustworthy.
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

s7_log "== negative control: one change made with the journal stopped must not be counted =="

s7_start_journal nc-d1 "$LAB/mnt/disk1" "$S7_RUN/nc-d1.log"

# Two changes while the journal is watching.
head -c 1024 /dev/urandom > "$LAB/mnt/disk1/media/nc-seen-1.bin"
head -c 1024 /dev/urandom > "$LAB/mnt/disk1/media/nc-seen-2.bin"
sleep 1

s7_stop_journal nc-d1

# One more change with no journal running at all — this is the gap.
head -c 1024 /dev/urandom > "$LAB/mnt/disk1/media/nc-missed.bin"

s7_log "-- disk1 journal raw log (expect exactly 2 distinct paths) --"
cat "$S7_RUN/nc-d1.log"

set +e
s7_snapraid diff > "$S7_RUN/nc-diff.log" 2>&1
diff_rc=$?
set -e
cat "$S7_RUN/nc-diff.log"
[[ "$diff_rc" -eq 2 ]] || die "expected snapraid diff to report pending changes (exit 2), got exit $diff_rc"

journal_count=$(s7_distinct_count "$S7_RUN/nc-d1.log")
diff_count=$(s7_diff_changed_count "$S7_RUN/nc-diff.log")

s7_log ""
s7_log "journal distinct paths (disk1 only): $journal_count"
s7_log "snapraid diff changed (whole array, includes nc-missed.bin): $diff_count"

[[ "$journal_count" -eq 2 ]] || die "expected exactly 2 distinct paths from the journal (the two seen changes), got $journal_count"
[[ "$diff_count" -gt "$journal_count" ]] || die "the control failed to create a real gap: diff ($diff_count) is not greater than the journal count ($journal_count) — this proves nothing"

s7_log "PASS: the comparison correctly flags a mismatch ($journal_count != $diff_count) when a real change is missed while the journal is down — this is exactly why the journal is documented as approximate (Q13) and the UI must say 'unknown until the next sync' rather than a number, once a gap like this is known to exist"

s7_snapraid sync
