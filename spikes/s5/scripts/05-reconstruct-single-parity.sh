#!/usr/bin/env bash
# Step 5: whole-disk failure and reconstruction under single parity
# (snapraid.txt §4.4 "Recovering"): detach d2's loop device (the "lost a
# disk" case), attach a brand-new, empty, same-mount-point replacement (a
# real disk swap keeps the config's `data d2 <mountpoint>` line unchanged,
# so — unlike the manual's own worked example, which repoints the option at
# a spare location — this run needs no config edit), then
# `fix -d d2` / `check -d d2 -a` / `sync` exactly as the manual's four
# recovery steps describe, and verify every file's sha256 against the
# pre-failure manifest.
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

[[ -f "$S5/manifest-after-scrub-fix.sha256" ]] || die "run 01 through 04 first"

pre_manifest="$S5/manifest-before-d2-failure.sha256"
s5_array_manifest disk1 disk2 disk3 > "$pre_manifest"
echo "pre-failure manifest: $pre_manifest ($(wc -l < "$pre_manifest") files)"

echo "### detach d2's loop device (doc 06 §3 failure injection; confirmed backed by our own image immediately before) ###"
s5_detach_disk disk2

echo "### attach a brand-new, empty replacement disk at the same mount point ###"
img="$LAB/img/disk2.img"
rm -f "$img"
truncate -s 2G "$img"
dev=$(losetup --find --show "$img")
lab_assert_own_loop "$dev" "$img"
mkfs.xfs -q -L disk2 "$dev"
mount "$dev" "$LAB/mnt/disk2"
echo "replacement disk2 attached: $dev, mounted at $LAB/mnt/disk2, empty ($(find "$LAB/mnt/disk2" -type f | wc -l) files)"

echo "### snapraid.txt §4.4 step 2: fix -d d2 ###"
set +e
s5_snapraid -d d2 -l "$S5/fix-d2.log" fix
fix_rc=$?
set -e
echo "[fix -d d2 exit $fix_rc]"
[[ "$fix_rc" -eq 0 ]] || die "expected 'fix -d d2' to exit 0, got $fix_rc — see $S5/fix-d2.log"
# The -l log is SnapRAID's own structured, tagged format (one
# "summary:error_unrecoverable:N" trailer line) — this is the exact
# machine-checkable count the manual's "check all the lines starting with
# 'unrecoverable:'" advice refers to for the plain stdout, not a substring
# match against the whole log (a naive `grep unrecoverable:` also matches
# this trailer's own "summary:error_unrecoverable:0" line, which reports
# zero, not a failure — found and fixed while writing this script).
unrecoverable=$(sed -n 's/^summary:error_unrecoverable:\([0-9]*\)$/\1/p' "$S5/fix-d2.log")
[[ "$unrecoverable" =~ ^[0-9]+$ ]] || die "could not find a summary:error_unrecoverable: line in $S5/fix-d2.log"
[[ "$unrecoverable" -eq 0 ]] || die "fix -d d2 reports $unrecoverable unrecoverable block(s) — reconstruction was not clean"

echo "### snapraid.txt §4.4 step 3: check -d d2 -a (paranoid check) ###"
s5_snapraid -d d2 -a check

echo "### snapraid.txt §4.4 step 4: sync (should be immediate if everything recovered) ###"
s5_snapraid sync

echo "### verify: every file's sha256 matches the pre-failure manifest ###"
post_manifest="$S5/manifest-after-d2-reconstruction.sha256"
s5_array_manifest disk1 disk2 disk3 > "$post_manifest"
if ! diff -u "$pre_manifest" "$post_manifest" > "$S5/d2-reconstruction.diff"; then
  cat "$S5/d2-reconstruction.diff"
  die "post-reconstruction manifest does not match the pre-failure manifest — see $S5/d2-reconstruction.diff"
fi
echo "RECONSTRUCTION OK: $post_manifest byte-for-byte (sha256) identical to $pre_manifest ($(wc -l < "$post_manifest") files)"
