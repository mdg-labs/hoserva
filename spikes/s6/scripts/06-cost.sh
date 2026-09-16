#!/usr/bin/env bash
# Step 6 (cost) — RSS and process count with the twelve-share topology up,
# and relative sequential-read throughput against a single mergerfs mount
# (doc 07 R12's stated risk, Q11/Q12's own "if S6 fails" cost accounting).
#
# Method, stated plainly because this lab cannot drop the page cache: no
# procps, and `echo 1 > /proc/sys/vm/drop_caches` fails with "Read-only
# file system" in this container (`/proc/sys` is itself bind-mounted `ro`
# — confirmed directly, `mount | grep proc/sys` shows
# `proc on /proc/sys type proc (ro,...)`; the file's own permission bits,
# `--w-------`, would otherwise allow it). `dd ... iflag=direct` is the one
# cache-avoidance mechanism available without a root-only sysctl write, and
# it worked on this build — but a first pass of this script (rejected,
# commit cbd9a52) showed that `iflag=direct` alone does not remove every
# warm-up effect: whichever scenario ran *first* showed a depressed first
# repetition that climbed toward the other scenario's level by the third,
# and reversing which scenario ran first moved the depressed rep to the
# other scenario — an order effect, not a mount-count effect, that a
# scenario-B-always-second design can't tell apart from a genuine cost.
#
# This version controls for that two ways rather than one:
#   1. A discarded warm-up read before any measured repetition, in
#      whichever state the script happens to be in on entry (the twelve
#      shares, per the RSS scan above) — absorbs the very-first-read-ever
#      cold state (loop-file page cache on the *backing* store, CPU
#      frequency scaling, etc.) that iflag=direct on the FUSE mount does
#      not reliably remove.
#   2. The measured repetitions alternate scenario in an ABBA-style
#      counterbalance (pair 1: A,B; pair 2: B,A; pair 3: A,B; ...) rather
#      than running every A before every B — this makes each scenario's
#      average position in the run sequence nearly identical (for N=5
#      pairs: A's mean position 5.4, B's mean position 5.6), so a linear
#      drift across the whole run (warm-up, thermal, scheduler noise on
#      this shared host) lands on both scenarios almost equally instead of
#      entirely on whichever one happened to run first.
# "Single mount" means the catch-all alone; "twelve share mounts" means
# the same catch-all with the full topology from 02-topology-up.sh nested
# inside it — since the test file is a top-level path with no per-share
# mount of its own, the read in both scenarios crosses exactly one FUSE
# layer (the catch-all); what this measures is whether a dozen *other*,
# idle mergerfs processes measurably affect an unrelated read's throughput
# on a shared host, which is the read half of R12's actual cost question —
# not a nested-mount read-path overhead, which the kernel's own mountpoint
# crossing (not the filesystem) resolves without an extra FUSE hop.
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

OUT=${S6_OUT:-$LAB/s6-results}
mkdir -p -- "$OUT"

mountpoint -q -- "$LAB/mnt/user/ctm1" || die "the twelve-share topology is not up — run 02-topology-up.sh first"

s6_log "== RSS and process count, twelve share mounts up =="
{
  echo "share  pid  VmRSS_kB"
  total=0
  for share in "${S6_SHARES_CTM[@]}" "${S6_SHARES_CO[@]}" "${S6_SHARES_AO[@]}"; do
    pid=$(cat "$S6_PIDS/$share.pid")
    rss=$(s6_rss_kb "$pid")
    echo "$share  $pid  $rss"
    total=$((total + rss))
  done
  echo "TOTAL_TWELVE_SHARES_KB $total"
  catchall_pid=$(cat "$S6_PIDS/catchall.pid")
  mover_pid=$(cat "$S6_PIDS/mover-ctm1.pid")
  echo "catchall  $catchall_pid  $(s6_rss_kb "$catchall_pid")"
  echo "mover-ctm1  $mover_pid  $(s6_rss_kb "$mover_pid")"
  echo "TOTAL_ALL_14_PROCESSES_KB $((total + $(s6_rss_kb "$catchall_pid") + $(s6_rss_kb "$mover_pid")))"
} | tee "$OUT/cost-rss.log"

n_procs=$(s6_process_count)
echo "mergerfs process count (/proc scan, comm == mergerfs): $n_procs" | tee -a "$OUT/cost-rss.log"
[[ "$n_procs" -eq 14 ]] || die "expected 14 live mergerfs processes (catch-all + 12 shares + mover target), found $n_procs"

s6_log "== confirming this container cannot drop the page cache (checked, not assumed) =="
{
  echo "mount | grep 'proc/sys':"
  mount | grep 'proc/sys' || echo "(no separate /proc/sys mount entry)"
  echo
  echo "ls -l /proc/sys/vm/drop_caches:"
  ls -l /proc/sys/vm/drop_caches
  echo
  echo "echo 1 > /proc/sys/vm/drop_caches:"
  # Run inside its own bash -c so a failure setting up the ">" redirection
  # itself (which is what actually happens here — a read-only bind mount
  # rejects the open, not the write) is reported on *this* subshell's own
  # stderr, which the outer 2>&1 below can actually capture; asking the
  # outer shell to redirect a failing redirection's own error message
  # doesn't work, because the error is emitted before that redirection is
  # established.
  if drop_caches_out=$(bash -c 'echo 1 > /proc/sys/vm/drop_caches' 2>&1); then
    echo "unexpectedly succeeded — page cache was dropped for real"
  else
    echo "failed: $drop_caches_out"
  fi
} | tee "$OUT/cost-drop-caches-check.log"

s6_log "== preparing the throughput test file (200 MiB, written directly to disk1, identical for both scenarios) =="
dd if=/dev/urandom of="$LAB/mnt/disk1/throughput-src.bin" bs=1M count=200 status=none
sha_src=$(sha256sum "$LAB/mnt/disk1/throughput-src.bin" | awk '{print $1}')
s6_log "throughput-src.bin sha256=$sha_src"

# Tries `iflag=direct` first (bypasses the page cache without needing the
# root-only /proc/sys/vm/drop_caches this container's read-only /proc/sys
# refuses anyway); if FUSE refuses O_DIRECT on this build (some do, some
# don't), falls back to a plain read and says so once rather than failing
# the whole step — recorded plainly, not silently swapped. Prints one
# "label: bytes, time, throughput" style line per read, and separately
# echoes the exact elapsed seconds and a throughput figure computed by
# this script directly from bytes/seconds (more precise than dd's own
# 2-significant-figure rounding, which is what the median/range below is
# computed from).
S6_DIRECT_IO_WORKS=${S6_DIRECT_IO_WORKS:-unknown}
S6_BYTES=209715200
# Runs one dd read and prints exactly one line, "label direct_io=X seconds=S
# gbps=G raw=<dd's own stderr, semicolons for newlines>", to stdout — every
# field name-tagged and grep/cut-able, so callers never re-parse a
# human-sentence for the number that decides a scenario's own figures.
# gbps is computed by this script from bytes/seconds, not from dd's own
# rounded (1-2 significant figure) "3.2 GB/s" text.
s6_read_one() {
  local label=$1
  local dd_out dd_rc iflag_opt=(iflag=direct)
  if [[ "$S6_DIRECT_IO_WORKS" == "no" ]]; then iflag_opt=(); fi
  set +e
  dd_out=$(dd if="$LAB/mnt/user/throughput-src.bin" of=/dev/null bs=1M "${iflag_opt[@]}" 2>&1 1>/dev/null); dd_rc=$?
  set -e
  if [[ "$dd_rc" -ne 0 && "$S6_DIRECT_IO_WORKS" == "unknown" ]]; then
    s6_log "iflag=direct failed on this build ($dd_out) — falling back to a plain (page-cache-eligible) read for the rest of this step; recorded as a method limitation, not hidden"
    S6_DIRECT_IO_WORKS=no
    iflag_opt=()
    set +e
    dd_out=$(dd if="$LAB/mnt/user/throughput-src.bin" of=/dev/null bs=1M 2>&1 1>/dev/null); dd_rc=$?
    set -e
  fi
  [[ "$dd_rc" -eq 0 ]] || die "$label: dd failed (exit $dd_rc): $dd_out"
  [[ "$S6_DIRECT_IO_WORKS" == "unknown" ]] && S6_DIRECT_IO_WORKS=yes
  local seconds gbps raw
  seconds=$(printf '%s' "$dd_out" | sed -n 's/.* \([0-9.]*\) s,.*/\1/p')
  [[ -n "$seconds" ]] || die "$label: could not parse elapsed seconds out of dd's own output: $dd_out"
  gbps=$(awk -v b="$S6_BYTES" -v s="$seconds" 'BEGIN{printf "%.4f", b/s/1e9}')
  raw=$(printf '%s' "$dd_out" | tr '\n' ';')
  echo "label=[$label] direct_io=$S6_DIRECT_IO_WORKS seconds=$seconds gbps=$gbps raw=[$raw]"
}

# Bring-up/tear-down helpers that only act when the topology isn't already
# in the wanted state — so consecutive same-scenario reps in the ABBA
# sequence don't remount anything unnecessarily, but every transition is
# still verified against findmnt's own count, never assumed.
S6_SHARES_UP=1
s6_want_shares() {
  local want=$1
  if [[ "$want" == "1" && "$S6_SHARES_UP" == "0" ]]; then
    s6_log "-- bringing the twelve-share topology back up (scenario B) --"
    s6_bring_up_shares
    local n; n=$(findmnt -R -n "$LAB/mnt/user" | wc -l)
    [[ "$n" -eq 13 ]] || die "expected 13 mounts under $LAB/mnt/user after bring-up, found $n"
    S6_SHARES_UP=1
  elif [[ "$want" == "0" && "$S6_SHARES_UP" == "1" ]]; then
    s6_log "-- tearing the twelve-share topology down (scenario A, single mount) --"
    s6_tear_down_shares
    local n; n=$(findmnt -R -n "$LAB/mnt/user" | wc -l)
    [[ "$n" -eq 1 ]] || die "expected only the catch-all mounted after tear-down, found $n mounts"
    S6_SHARES_UP=0
  fi
}

s6_log "== discarded warm-up reads, several per state, not counted in any scenario's figures =="
# One warm-up in whichever state the script is in on entry is not enough
# by itself: a first pass of this fix showed the *first measured* read
# after the *first* tear-down to single-mount was still depressed (4.66
# vs ~5.3 GB/s for every other rep in that run) even with one warm-up
# already taken in the up state — the cold state decays over several
# reads after a mount/unmount transition, not in one. A second pass
# (one warm-up per state) still showed a multi-repetition climb (2.7 to
# 5.3 GB/s over the first ~4-5 reads of the whole run, in *both*
# scenarios' own earliest reps alike) — evidence this is a whole-run
# settling effect (host scheduling/FUSE churn following the RSS scan and
# repeated mount/unmount cycling earlier in this same script), not a
# mount-count effect, but one repetition per state still wasn't enough to
# let it decay before counting starts. This does several (not counted)
# reads per state instead — a fixed count, not an adaptive
# convergence check, so it is not tuned after the fact against any run's
# own numbers.
S6_WARMUP_REPS=${S6_WARMUP_REPS:-4}
s6_warm_up_state() {
  local label_prefix=$1
  local i line
  for ((i = 1; i <= S6_WARMUP_REPS; i++)); do
    line=$(s6_read_one "$label_prefix-warmup$i")
    echo "$line" | tee -a "$OUT/cost-throughput-warmup.log"
  done
}
: > "$OUT/cost-throughput-warmup.log"
s6_warm_up_state "shares-up"
s6_want_shares 0
s6_warm_up_state "single-mount"
s6_log "$S6_WARMUP_REPS discarded warm-up reads taken in each state (see results/cost-throughput-warmup.log) — none counted in either scenario below. Left in the single-mount (torn-down) state afterwards, since the measured sequence below starts with scenario A (single-mount) and an extra bring-up/tear-down cycle right before the first counted rep would just relocate the transition noise this warm-up exists to absorb, not remove it"

sha_after_warmup=$(sha256sum "$LAB/mnt/user/throughput-src.bin" | awk '{print $1}')
[[ "$sha_after_warmup" == "$sha_src" ]] || die "throughput-src.bin changed after the warm-up reads"

# ABBA counterbalance: pair i is A,B when i is odd and B,A when i is even,
# so scenario A and scenario B each spend roughly half their reps in the
# "ran right after a tear-down/bring-up" slot and half in the "ran second"
# slot, and their mean position in the overall sequence is nearly equal —
# a linear drift across the run lands on both scenarios almost evenly
# instead of entirely on whichever one this script happened to run first.
N_PAIRS=${S6_COST_PAIRS:-5}
SEQUENCE=()
for ((i = 1; i <= N_PAIRS; i++)); do
  if (( i % 2 == 1 )); then
    SEQUENCE+=(A B)
  else
    SEQUENCE+=(B A)
  fi
done

s6_log "== ${#SEQUENCE[@]} measured reads, scenario order: ${SEQUENCE[*]} (ABBA-counterbalanced, ${N_PAIRS} reps per scenario) =="

A_VALUES=()
B_VALUES=()
ALL_POS=()
ALL_VALUES=()
INTERLEAVED_LOG="$OUT/cost-throughput-interleaved.log"
: > "$INTERLEAVED_LOG"
seq_pos=0
for item in "${SEQUENCE[@]}"; do
  seq_pos=$((seq_pos + 1))
  if [[ "$item" == "A" ]]; then
    s6_want_shares 0
    rep=$(( ${#A_VALUES[@]} + 1 ))
    label="single-mount rep$rep (seq-pos $seq_pos)"
  else
    s6_want_shares 1
    rep=$(( ${#B_VALUES[@]} + 1 ))
    label="twelve-shares rep$rep (seq-pos $seq_pos)"
  fi
  line=$(s6_read_one "$label")
  echo "$line" | tee -a "$INTERLEAVED_LOG"
  gbps=$(printf '%s' "$line" | sed -n 's/.*gbps=\([0-9.]*\).*/\1/p')
  [[ -n "$gbps" ]] || die "could not parse gbps out of: $line"
  ALL_POS+=("$seq_pos")
  ALL_VALUES+=("$gbps")
  if [[ "$item" == "A" ]]; then
    A_VALUES+=("$gbps")
  else
    B_VALUES+=("$gbps")
  fi
done

sha_after_all=$(sha256sum "$LAB/mnt/user/throughput-src.bin" | awk '{print $1}')
[[ "$sha_after_all" == "$sha_src" ]] || die "throughput-src.bin changed across the measured sequence"
s6_log "confirmed: throughput-src.bin's sha256 ($sha_src) is unchanged across the warm-up and every measured rep — every read served the same bytes"

# Median/min/max per scenario, plus a plain check for a residual order
# effect: correlate each scenario's own values against their own position
# in SEQUENCE (not against the other scenario) — a flat or non-monotonic
# trend means the ABBA counterbalance did its job; a still-visible
# monotonic climb/fall within a scenario is named as a residual caveat,
# not explained away.
s6_stats() {
  local -n arr=$1
  local sorted; sorted=$(printf '%s\n' "${arr[@]}" | sort -n)
  local n=${#arr[@]}
  local median
  median=$(printf '%s\n' "$sorted" | awk -v n="$n" '{a[NR]=$1} END{if(n%2==1) print a[(n+1)/2]; else print (a[n/2]+a[n/2+1])/2}')
  local min max
  min=$(printf '%s\n' "$sorted" | head -1)
  max=$(printf '%s\n' "$sorted" | tail -1)
  echo "n=$n values=[${arr[*]}] median=$median min=$min max=$max"
}

# Pearson correlation coefficient between two equal-length numeric arrays
# (position, value) — used below to check for a residual linear drift
# across the run's own sequence position, the exact confound the ABBA
# counterbalance is meant to cancel. |r| near 0: no visible drift left.
# A large |r| (either sign) is reported plainly, not explained away.
s6_pearson_r() {
  local -n xs=$1 ys=$2
  local n=${#xs[@]}
  paste <(printf '%s\n' "${xs[@]}") <(printf '%s\n' "${ys[@]}") | awk -v n="$n" '
    {sx+=$1; sy+=$2; sxx+=$1*$1; syy+=$2*$2; sxy+=$1*$2}
    END{
      mx=sx/n; my=sy/n;
      cov=sxy/n-mx*my; vx=sxx/n-mx*mx; vy=syy/n-my*my;
      if (vx<=0 || vy<=0) { print "undefined (no variance in one axis)"; exit }
      printf "%.3f", cov/sqrt(vx*vy)
    }'
}

{
  echo "scenario A (single-mount), in run order: ${A_VALUES[*]} GB/s"
  echo "  $(s6_stats A_VALUES)"
  echo "scenario B (twelve-shares), in run order: ${B_VALUES[*]} GB/s"
  echo "  $(s6_stats B_VALUES)"
  a_median=$(s6_stats A_VALUES | sed -n 's/.*median=\([0-9.]*\).*/\1/p')
  b_median=$(s6_stats B_VALUES | sed -n 's/.*median=\([0-9.]*\).*/\1/p')
  rel_loss=$(awk -v a="$a_median" -v b="$b_median" 'BEGIN{printf "%.2f", (a-b)/a*100}')
  echo "relative loss (twelve-shares vs single-mount, by median): ${rel_loss}%"
  echo
  echo "order-effect check: Pearson r between each read's own sequence position (1..${#ALL_VALUES[@]}) and its measured GB/s, all reps pooled regardless of scenario:"
  echo "  r = $(s6_pearson_r ALL_POS ALL_VALUES)"
  echo "  (|r| close to 0: no visible linear drift left across the run after ABBA counterbalancing. A large |r| means a drift remains and is a measurement caveat, not something this script's counterbalance removed.)"
} | tee "$OUT/cost-throughput-summary.log"

# Backward-compatible per-scenario logs (README/doc 08 reference these
# filenames), now carrying the full interleaved sequence position and a
# note that the two files are extracted from one interleaved run, not two
# separate back-to-back ones.
{
  echo "# extracted from the ABBA-interleaved run — see cost-throughput-interleaved.log for the real run order and cost-throughput-summary.log for the order-effect check"
  grep 'label=\[single-mount ' "$INTERLEAVED_LOG"
} | tee "$OUT/cost-throughput-single-mount.log" >/dev/null
{
  echo "# extracted from the ABBA-interleaved run — see cost-throughput-interleaved.log for the real run order and cost-throughput-summary.log for the order-effect check"
  grep 'label=\[twelve-shares ' "$INTERLEAVED_LOG"
} | tee "$OUT/cost-throughput-twelve-shares.log" >/dev/null

s6_log "== rebuilding the twelve-share topology for 07-failure-kill.sh (if not already up) =="
s6_want_shares 1
n_mounted=$(findmnt -R -n "$LAB/mnt/user" | wc -l)
[[ "$n_mounted" -eq 13 ]] || die "rebuild failed: expected 13 mounts under $LAB/mnt/user, found $n_mounted"
s6_log "confirmed: twelve-share topology is up (13 mounts under $LAB/mnt/user)"
