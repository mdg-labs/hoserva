#!/usr/bin/env bash
# Step 2 (ordering) — the "wrong order" half. Demonstrates what doc 02 §1's
# systemd `RequiresMountsFor=` requirement is actually structurally
# protecting against: a per-share mount that comes up *before* the
# catch-all does not fail to mount — it mounts fine, accepts writes, and
# then becomes silently unreachable (not destroyed, not corrupted, just
# unreachable via the intended path) the moment the catch-all mounts over
# its parent directory.
#
# Runs first, against a disposable share ("shadow-test") that no later
# script touches, and ends by establishing the catch-all mount every later
# script in this spike relies on — mounted through s6_mount (not
# create-array.sh's own daemonized instance) so this spike tracks its PID
# and can unmount it the same way as everything else.
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

OUT=${S6_OUT:-$LAB/s6-results}
mkdir -p -- "$OUT"

mountpoint -q -- "$LAB/mnt/user" || die "expected the standing catch-all at $LAB/mnt/user (make lab-up) to be mounted already"

s6_log "== shadow-mount experiment: mount a share before its catch-all =="

# The share's own branches: cache (RW, so any new file necessarily lands
# there — NC below excludes disk1 from create) and disk1 tagged NC, mirroring
# doc 02 §1's cache-then-move pattern. Created directly on the physical
# mounts, never through a pool.
mkdir -p -- "$LAB/mnt/cache/shadow-test" "$LAB/mnt/disk1/shadow-test"

# Take the catch-all down first — nothing is mounted inside it yet, so this
# must succeed cleanly.
fusermount -u -- "$LAB/mnt/user" || die "could not unmount the standing catch-all to start the experiment"
mountpoint -q -- "$LAB/mnt/user" && die "catch-all still mounted after fusermount -u"
s6_log "catch-all unmounted; $LAB/mnt/user is now the plain (pre-catch-all) directory"

# Mount the share's own pool directly at $LAB/mnt/user/shadow-test while the
# catch-all is down — this is the "child before parent" ordering mistake.
# The mountpoint directory is created directly (not through any pool),
# since with the catch-all down there is no pool to create it through.
mkdir -p -- "$LAB/mnt/user/shadow-test"
opts=$(s6_opts mspmfs 10M hoserva-shadow-test)
s6_mount shadow-test "$opts" "$LAB/mnt/cache/shadow-test=RW:$LAB/mnt/disk1/shadow-test=NC" "$LAB/mnt/user/shadow-test"

echo "marker-written-before-catchall" > "$LAB/mnt/user/shadow-test/marker.txt"
sha_before=$(sha256sum "$LAB/mnt/user/shadow-test/marker.txt" | awk '{print $1}')
s6_log "marker written through shadow-test, sha256=$sha_before"

# man/mergerfs.1 "NC: (no-create) - Will be excluded from create policies."
# — confirm the write actually landed on cache (RW), not on disk1 (NC),
# before drawing any conclusion from what the catch-all shows later.
s6_log "-- where did marker.txt actually land? --"
s6_locate "shadow-test/marker.txt" "$LAB/mnt/cache" "$LAB/mnt/disk1"
[[ -e "$LAB/mnt/cache/shadow-test/marker.txt" ]] || die "expected marker.txt on cache (RW branch)"
[[ -e "$LAB/mnt/disk1/shadow-test/marker.txt" ]] && die "marker.txt landed on disk1, an NC branch — NC did not exclude it from create"
s6_log "confirmed: marker.txt is on cache only, not on the NC-tagged disk1 branch"

pid_before=$(cat "$S6_PIDS/shadow-test.pid")
kill -0 "$pid_before" 2>/dev/null || die "shadow-test's own mergerfs (pid $pid_before) is not running before the catch-all comes up"

# Now bring the catch-all up over the *parent* directory ($LAB/mnt/user),
# using the same three data disks as the standing lab (no cache branch) —
# doc 02 §1's own catch-all definition ("mergerfs over /mnt/disk*=RW").
catchall_opts=$(s6_opts mspmfs 50M hoserva-pool)
s6_mount catchall "$catchall_opts" "$LAB/mnt/disk1:$LAB/mnt/disk2:$LAB/mnt/disk3" "$LAB/mnt/user"

s6_log "-- catch-all now mounted over the shadow-test mount's own parent directory --"
{
  echo "ls -la $LAB/mnt/user/shadow-test (through the catch-all, after it came up):"
  ls -la "$LAB/mnt/user/shadow-test" || echo "(ls failed)"
} | tee "$OUT/shadow-mount-after-catchall.log"

if [[ -e "$LAB/mnt/user/shadow-test/marker.txt" ]]; then
  die "unexpected: marker.txt is visible through the catch-all — the shadow-mount effect did not occur"
fi
s6_log "confirmed: marker.txt is NOT visible at $LAB/mnt/user/shadow-test/marker.txt any more — the catch-all's own view of that path (disk1 only, empty) is what's exposed now, not shadow-test's own cache+NC view"

# The orphaned mount is still alive and still tracked by the kernel, purely
# unreachable by path — check both, from raw evidence, not by eye.
kill -0 "$pid_before" 2>/dev/null || die "shadow-test's mergerfs (pid $pid_before) died merely from being shadowed — it should still be running"
grep -F " $LAB/mnt/user/shadow-test " /proc/self/mountinfo > "$OUT/shadow-mount-mountinfo-while-shadowed.log" \
  || die "expected $LAB/mnt/user/shadow-test to still appear in /proc/self/mountinfo while shadowed"
s6_log "confirmed: pid $pid_before is still running and the mount is still present in /proc/self/mountinfo — it is orphaned, not torn down"

# Recovery: unmount the catch-all (the top of the stack at this parent
# path) — this must re-expose the pre-existing directory tree underneath,
# including the shadow-test mountpoint, without anyone having touched
# shadow-test's own mount at all.
s6_unmount catchall "$LAB/mnt/user"
[[ -e "$LAB/mnt/user/shadow-test/marker.txt" ]] || die "recovery failed: marker.txt did not reappear once the catch-all was removed"
sha_after=$(sha256sum "$LAB/mnt/user/shadow-test/marker.txt" | awk '{print $1}')
[[ "$sha_before" == "$sha_after" ]] || die "recovery corrupted the file: sha256 before=$sha_before after=$sha_after"
s6_log "confirmed: once the catch-all is removed again, shadow-test's own mount is reachable and marker.txt is byte-identical (sha256 $sha_after) — the file was never at risk, only its path"

s6_unmount shadow-test "$LAB/mnt/user/shadow-test"
rmdir -- "$LAB/mnt/user/shadow-test" 2>/dev/null || true

# Also confirm the *correct* order (parent already up) is not itself the
# thing preventing an unmount attempt from working: mounting the catch-all
# again now, with nothing inside it, must unmount cleanly on the first try.
s6_mount catchall "$catchall_opts" "$LAB/mnt/disk1:$LAB/mnt/disk2:$LAB/mnt/disk3" "$LAB/mnt/user"
s6_log "catch-all remounted (tracked, pid $(cat "$S6_PIDS/catchall.pid")) — every later script in this spike builds on this instance"
