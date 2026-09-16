#!/usr/bin/env bash
# S8 step 2: confirm -Z/--force-zero and -E/--force-empty on SnapRAID
# 12.4 (Debian 13) — the two doc 02 §2 features S5's own fixtures never
# exercised (S5 never emptied a disk or zeroed a tracked file). The
# untouched-filler-file mitigation for the all-files-missing guard is S7's
# own fix (doc 08 §7), added after S7's re-run of its own workload tripped
# it by accident — not part of S5's fixture design. This script avoids the
# same accidental trip differently: disk1 and disk2 each keep a "keep" file
# no step here ever touches, so count_equal stays >= 1 on both throughout;
# only disk3, whose files are both deliberately removed to exercise -E
# itself, ever reaches zero. sync/diff/scrub/fix/touch/2-parity are S5's
# job, cited here, not repeated.
set -euo pipefail
LAB=/lab/9-a1
S8=$LAB/s8
CONF=$S8/snapraid.conf
OUT=$S8/snapraid-guards.log
mkdir -p -- "$S8"

echo "== snapraid pkg version (metadata, never --version) ==" | tee "$OUT"
dpkg-query -W -f '${Package} ${Version}\n' snapraid | tee -a "$OUT"

cat > "$CONF" <<EOF
parity $LAB/mnt/parity1/snapraid.parity
content $LAB/mnt/parity1/snapraid.content
content $LAB/mnt/cache/snapraid.content
data d1 $LAB/mnt/disk1/
data d2 $LAB/mnt/disk2/
data d3 $LAB/mnt/disk3/
EOF
echo "== wrote $CONF ==" | tee -a "$OUT"
cat "$CONF" | tee -a "$OUT"

run() {  # logfile-suffix, args...
  local suffix=$1; shift
  local logfile="$S8/$suffix.log"
  echo "+ snapraid -c $CONF $*" | tee -a "$OUT"
  local rc=0
  snapraid -c "$CONF" "$@" > "$logfile" 2>&1 || rc=$?
  echo "[exit $rc]" | tee -a "$OUT"
  cat "$logfile" | tee -a "$OUT"
  return "$rc"
}

echo "== seed: 2 files per disk, one will be force-zero'd, one disk will be force-empty'd ==" | tee -a "$OUT"
seed() {  # disk, relpath, size
  local dest="$LAB/mnt/$1/$2"
  mkdir -p -- "$(dirname -- "$dest")"
  head -c "$3" /dev/urandom > "$dest"
}
seed disk1 "a/keep.bin" 200000
seed disk1 "a/zero-me.bin" 150000
seed disk2 "b/keep.bin" 220000
seed disk2 "b/also-keep.bin" 180000
seed disk3 "c/keep.bin" 190000
seed disk3 "c/empty-me.bin" 170000
find "$LAB/mnt/disk1" "$LAB/mnt/disk2" "$LAB/mnt/disk3" -type f | sort | tee -a "$OUT"

echo "== initial sync (baseline, all files present) ==" | tee -a "$OUT"
run initial-sync sync

echo "=========================================================="
echo "== force-zero: truncate a previously-non-zero tracked file to 0 bytes =="
echo "=========================================================="
: > "$LAB/mnt/disk1/a/zero-me.bin"
stat -c '%n %s bytes' "$LAB/mnt/disk1/a/zero-me.bin" | tee -a "$OUT"

echo "-- plain sync, expect refusal (snapraid.txt -Z/--force-zero) --" | tee -a "$OUT"
if run zero-plain-sync sync; then
  echo "UNEXPECTED: plain sync succeeded with a zeroed file present" >&2
  exit 1
else
  echo "confirmed: plain sync refused a zeroed previously-non-zero file" | tee -a "$OUT"
fi
grep -qi "zero" "$S8/zero-plain-sync.log" && echo "confirmed: refusal message names the zero-size condition" | tee -a "$OUT"

echo "-- sync -Z/--force-zero, expect success --" | tee -a "$OUT"
run zero-forced-sync sync -Z
echo "confirmed: --force-zero overrides the guard" | tee -a "$OUT"

echo "=========================================================="
echo "== force-empty: remove every originally-present file on one whole disk =="
echo "=========================================================="
rm -f "$LAB/mnt/disk3/c/keep.bin" "$LAB/mnt/disk3/c/empty-me.bin"
find "$LAB/mnt/disk3" -type f | tee -a "$OUT"
echo "(disk3 now has zero tracked files, mountpoint itself untouched — simulating the doc 02 §2 removing-disk case, doc 09 §4)" | tee -a "$OUT"

echo "-- plain sync, expect refusal (snapraid.txt -E/--force-empty) --" | tee -a "$OUT"
if run empty-plain-sync sync; then
  echo "UNEXPECTED: plain sync succeeded with disk3 fully emptied" >&2
  exit 1
else
  echo "confirmed: plain sync refused disk3 with all originally-present files missing" | tee -a "$OUT"
fi
grep -qi "empty\|missing\|rewritten" "$S8/empty-plain-sync.log" && echo "confirmed: refusal message names the all-files-missing condition" | tee -a "$OUT"

echo "-- sync -E/--force-empty, expect success --" | tee -a "$OUT"
run empty-forced-sync sync -E
echo "confirmed: --force-empty overrides the guard" | tee -a "$OUT"

echo "-- final status, everything settled --" | tee -a "$OUT"
run final-status status

echo "OK" | tee -a "$OUT"
