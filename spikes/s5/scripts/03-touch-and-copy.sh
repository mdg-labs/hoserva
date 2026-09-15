#!/usr/bin/env bash
# Step 3: exercise "touch" (snapraid.txt §5.14) and the sub-second-timestamp
# rule for "copied" detection (§5.5): with a zero sub-second time-stamp, a
# copy is recognized only if the *full path* matches; with a non-zero one,
# matching name+size+timestamp is enough, regardless of directory. Also a
# negative control: a same-content, same-timestamp file under a different
# *name* must never be reported as copied, proving the match needs the name,
# not just the bytes.
#
# Every "copy" case here needs its source file already committed by a prior
# "sync" before the copy is made in a later diff — a first version of this
# script copied a file that was brand new in the *same* diff pass as its
# source, on the (wrong) assumption that "add" pairs within one diff can
# match each other; they cannot (found by running it: two simultaneously new,
# identical-metadata files on two disks were both reported "add", never
# "copy" — restructured below to always sync the source first).
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

[[ -f "$CONF" ]] || die "run 01-seed-and-sync.sh and 02-diff-and-sync.sh first"

zero_ts="202401010000.00"  # touch -t: 2024-01-01 00:00:00, integer seconds -> zero sub-second component

mkdir -p "$LAB/mnt/disk1/touchtest/srcA" "$LAB/mnt/disk1/touchtest/srcB" "$LAB/mnt/disk1/touchtest/srcC"

echo "### create three zero-sub-second source files on disk1, sync them in ###"
head -c 111111 /dev/urandom > "$LAB/mnt/disk1/touchtest/srcA/f.bin"
head -c 222222 /dev/urandom > "$LAB/mnt/disk1/touchtest/srcB/f.bin"
head -c 333333 /dev/urandom > "$LAB/mnt/disk1/touchtest/srcC/f.bin"
touch -t "$zero_ts" "$LAB/mnt/disk1/touchtest/srcA/f.bin" "$LAB/mnt/disk1/touchtest/srcB/f.bin" "$LAB/mnt/disk1/touchtest/srcC/f.bin"
stat -c '%n mtime=%y' "$LAB/mnt/disk1/touchtest/srcA/f.bin" "$LAB/mnt/disk1/touchtest/srcB/f.bin" "$LAB/mnt/disk1/touchtest/srcC/f.bin"
sync
s5_snapraid sync

echo "### case A: copy srcA to disk2 at a DIFFERENT relative path, subsec=0 -> expect NOT copied ###"
mkdir -p "$LAB/mnt/disk2/touchtest/dstA-elsewhere"
cp -p "$LAB/mnt/disk1/touchtest/srcA/f.bin" "$LAB/mnt/disk2/touchtest/dstA-elsewhere/f.bin"

echo "### case B: copy srcB to disk2 at the IDENTICAL relative path, subsec=0 -> expect copied (full path matches) ###"
mkdir -p "$LAB/mnt/disk2/touchtest/srcB"
cp -p "$LAB/mnt/disk1/touchtest/srcB/f.bin" "$LAB/mnt/disk2/touchtest/srcB/f.bin"

echo "### negative control: copy srcA to a DIFFERENT NAME, same disk, same dir, timestamp+size preserved -> must never be 'copied' ###"
cp -p "$LAB/mnt/disk1/touchtest/srcA/f.bin" "$LAB/mnt/disk1/touchtest/srcA/f-renamed.bin"

sync
s5_snapraid_log "$S5/diff-touch-before.log" diff
echo "[diff exit $S5_LAST_RC]"

grep -qF "add touchtest/dstA-elsewhere/f.bin" "$S5/diff-touch-before.log" || die "case A: expected 'add touchtest/dstA-elsewhere/f.bin' (not recognized as a copy) — see diff-touch-before.log"
grep -qF "copy touchtest/srcB/f.bin -> touchtest/srcB/f.bin" "$S5/diff-touch-before.log" || die "case B: expected a 'copy' line for touchtest/srcB/f.bin — full-path match with subsec=0 did not behave as snapraid.txt §5.5 documents"
grep -qF "add touchtest/srcA/f-renamed.bin" "$S5/diff-touch-before.log" || die "rename control: expected 'add touchtest/srcA/f-renamed.bin' (never a copy across different names)"
! grep -qF -- "-> touchtest/srcA/f-renamed.bin" "$S5/diff-touch-before.log" || die "rename control FAILED: a same-content, different-name file was reported as 'copied' — the mechanism is not actually checking the name"

echo "case A/B/rename-control all classified as documented"

s5_snapraid sync

echo "### snapraid touch: set sub-second time-stamps on every zero-subsecond file still in the array ###"
stat -c '%n %.9Y' "$LAB/mnt/disk1/touchtest/srcC/f.bin" > "$S5/touch-mtimes-before.log"
s5_snapraid touch
stat -c '%n %.9Y' "$LAB/mnt/disk1/touchtest/srcC/f.bin" > "$S5/touch-mtimes-after.log"
echo "srcC/f.bin before: $(cat "$S5/touch-mtimes-before.log")"
echo "srcC/f.bin after:  $(cat "$S5/touch-mtimes-after.log")"
if diff -q "$S5/touch-mtimes-before.log" "$S5/touch-mtimes-after.log" >/dev/null; then
  die "'snapraid touch' left srcC/f.bin's zero sub-second timestamp unchanged — expected it to set it to something non-zero"
fi

s5_snapraid sync

echo "### case C: copy the now-non-zero-subsecond, already-synced srcC to a DIFFERENT directory, SAME name, expect copied ###"
mkdir -p "$LAB/mnt/disk3/touchtest/dstC-elsewhere"
cp -p "$LAB/mnt/disk1/touchtest/srcC/f.bin" "$LAB/mnt/disk3/touchtest/dstC-elsewhere/f.bin"
sync
s5_snapraid_log "$S5/diff-touch-after.log" diff
echo "[diff exit $S5_LAST_RC]"
grep -qF "copy touchtest/srcC/f.bin -> touchtest/dstC-elsewhere/f.bin" "$S5/diff-touch-after.log" || die "case C: expected a 'copy' line once the source file's sub-second stamp was non-zero, even though the destination directory differs"
echo "case C classified as documented"

s5_snapraid sync
s5_array_manifest disk1 disk2 disk3 > "$S5/manifest-after-touch.sha256"
echo "manifest: $S5/manifest-after-touch.sha256 ($(wc -l < "$S5/manifest-after-touch.sha256") files)"
