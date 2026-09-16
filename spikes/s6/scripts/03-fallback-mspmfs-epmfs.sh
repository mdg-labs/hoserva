#!/usr/bin/env bash
# Step 3 (fallback) — a dedicated three-disk array, separate from the
# standing disk1..3 array (so filling one branch to below minfreespace
# never touches the twelve-share topology), mounted twice through the same
# three branches with two pools that differ in exactly one option:
# category.create. Everything else — minfreespace, moveonenospc, the
# branch list, the fill state at the moment each write happens — is
# identical, so any difference in outcome is attributable to the create
# policy alone (dispatch's own instruction for this step).
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

OUT=${S6_OUT:-$LAB/s6-results}
mkdir -p -- "$OUT"

s6_create_disk mfp1 300M
s6_create_disk mfp2 300M
s6_create_disk mfp3 300M

# "media" exists only on mfp1 to start — the path-preserving policies'
# existing-path filter (man/mergerfs.1: "A path preserving policy will
# only consider branches where the relative path exists") then has exactly
# one candidate branch, which is the one this script fills.
mkdir -p -- "$LAB/mnt/mfp1/media"

BRANCHES="$LAB/mnt/mfp1:$LAB/mnt/mfp2:$LAB/mnt/mfp3"
MINFREE=50M
msp_opts=$(s6_opts mspmfs "$MINFREE" hoserva-fallback-mspmfs)
ep_opts=$(s6_opts epmfs "$MINFREE" hoserva-fallback-epmfs)
s6_mount fallback-mspmfs "$msp_opts" "$BRANCHES" "$LAB/mnt/pool-mspmfs"
s6_mount fallback-epmfs "$ep_opts" "$BRANCHES" "$LAB/mnt/pool-epmfs"

s6_log "== baseline: with mfp1 not yet full, a write to media/ prefers the branch that already has the path (positive control) =="
dd if=/dev/urandom of="$LAB/mnt/pool-mspmfs/media/baseline.bin" bs=1M count=2 status=none
s6_locate "media/baseline.bin" "$LAB/mnt/mfp1" "$LAB/mnt/mfp2" "$LAB/mnt/mfp3" | tee "$OUT/fallback-baseline-location.log"
[[ -e "$LAB/mnt/mfp1/media/baseline.bin" ]] || die "baseline write did not land on mfp1 (the only branch with an existing 'media' path) — the existing-path mechanism this spike is about to stress-test isn't behaving as assumed"
s6_log "confirmed: baseline write landed on mfp1, the only branch where 'media' already existed"

s6_log "== filling mfp1 below minfreespace ($MINFREE), via fallocate directly on the branch, matching doc 06 §3's own filler-file technique =="
df -B1 "$LAB/mnt/mfp1" > "$OUT/fallback-df-mfp1-before-fill.log"
avail=$(df --output=avail -B1 "$LAB/mnt/mfp1" | tail -1 | tr -d ' ')
target_free=$((20*1024*1024))   # leave ~20 MiB, comfortably under the 50M minfreespace filter
fill=$((avail - target_free))
[[ "$fill" -gt 0 ]] || die "mfp1 already has less than ${target_free} bytes free before filling — geometry assumption wrong"
fallocate -l "$fill" "$LAB/mnt/mfp1/filler.bin"
df -B1 "$LAB/mnt/mfp1" > "$OUT/fallback-df-mfp1-after-fill.log"
avail_after=$(df --output=avail -B1 "$LAB/mnt/mfp1" | tail -1 | tr -d ' ')
[[ "$avail_after" -lt $((50*1024*1024)) ]] || die "mfp1 still has ${avail_after} bytes free after filling — expected under 50 MiB"
s6_log "confirmed: mfp1 now has $((avail_after/1024/1024)) MiB free, under the ${MINFREE} minfreespace filter (raw before/after df in results/)"

s6_log "== epmfs first, while 'media' still exists only on the now-full mfp1 =="
# Order matters here and is deliberate: epmfs must be tested while "media"
# still exists on exactly one branch (mfp1, filled above). Testing mspmfs
# first would have mspmfs's own fallback create "media" on mfp2 or mfp3 as
# a side effect (both pools share the same three physical branches) —
# which would then hand epmfs a second, unfilled branch with the path
# already on it, contaminating the very case this step exists to observe.
# (Found empirically: an earlier version of this script ran mspmfs first
# and epmfs's write then unexpectedly succeeded on mfp3, purely because
# mspmfs's own prior write had already created "media" there.)
set +e
ep_out=$(dd if=/dev/urandom of="$LAB/mnt/pool-epmfs/media/after-fill-epmfs.bin" bs=1M count=5 status=none 2>&1); ep_rc=$?
set -e
echo "$ep_out" > "$OUT/fallback-epmfs-write.log"; echo "[exit $ep_rc]" >> "$OUT/fallback-epmfs-write.log"
if [[ "$ep_rc" -eq 0 ]]; then
  s6_locate "media/after-fill-epmfs.bin" "$LAB/mnt/mfp1" "$LAB/mnt/mfp2" "$LAB/mnt/mfp3" | tee "$OUT/fallback-epmfs-location.log"
  die "epmfs write unexpectedly succeeded (exit 0) — expected ENOSPC since epmfs does not retry the parent path (man/mergerfs.1: epmfs is path-preserving with no parent-directory fallback, unlike mspmfs)"
fi
echo "$ep_out" | grep -qi 'no space left on device' || die "epmfs write failed, but not with ENOSPC's usual message — got: $ep_out"
[[ -e "$LAB/mnt/mfp1/media/after-fill-epmfs.bin" || -e "$LAB/mnt/mfp2/media/after-fill-epmfs.bin" || -e "$LAB/mnt/mfp3/media/after-fill-epmfs.bin" ]] && die "epmfs reported failure but a file was created somewhere anyway"
s6_log "confirmed: epmfs returned ENOSPC (exit $ep_rc, \"No space left on device\") while 'media' still existed only on the filled mfp1 branch — man/mergerfs.1's epmfs entry (\"Of all the branches on which the relative path exists choose the branch with the most free space\") has no parent-directory retry, unlike mspmfs"

s6_log "== mspmfs second, same fill state, same branches, only the create policy differs =="
set +e
msp_out=$(dd if=/dev/urandom of="$LAB/mnt/pool-mspmfs/media/after-fill-mspmfs.bin" bs=1M count=5 status=none 2>&1); msp_rc=$?
set -e
echo "$msp_out" > "$OUT/fallback-mspmfs-write.log"; echo "[exit $msp_rc]" >> "$OUT/fallback-mspmfs-write.log"
[[ "$msp_rc" -eq 0 ]] || die "mspmfs write failed (exit $msp_rc): $msp_out — expected it to fall back to the parent path and succeed"
s6_locate "media/after-fill-mspmfs.bin" "$LAB/mnt/mfp1" "$LAB/mnt/mfp2" "$LAB/mnt/mfp3" | tee "$OUT/fallback-mspmfs-location.log"
[[ -e "$LAB/mnt/mfp1/media/after-fill-mspmfs.bin" ]] && die "mspmfs's fallback write landed on mfp1, the branch that is supposed to be filtered out by minfreespace"
[[ -e "$LAB/mnt/mfp2/media/after-fill-mspmfs.bin" || -e "$LAB/mnt/mfp3/media/after-fill-mspmfs.bin" ]] || die "mspmfs's fallback write is not on mfp2 or mfp3 either — cannot locate it anywhere"
s6_log "confirmed: mspmfs wrote the file successfully, on a branch other than the full mfp1 (and only after epmfs, on the exact same fill state, had already failed above) — man/mergerfs.1's 'mspmfs (most shared path, most free space)': \"Like epmfs but if it fails to find a branch it will try again with the parent directory. Continues this pattern till finding one.\" holds on this build"

s6_unmount fallback-mspmfs "$LAB/mnt/pool-mspmfs"
s6_unmount fallback-epmfs "$LAB/mnt/pool-epmfs"
