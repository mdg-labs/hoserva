#!/usr/bin/env bash
# Exercises spikes/s9/scripts/apparmor-classify.sh directly against real
# strings, with no loop device, no AppArmor, no Docker — this host has
# AppArmor disabled (/sys/module/apparmor/parameters/enabled = N) and
# loop-device probes are out of bounds here entirely (CLAUDE.md), but the
# classification logic that has produced three void-but-confident verdicts
# in a row is pure text processing and needs no privilege at all to test.
#
# Also exercises read_result/classify_branch_pair — the file-reading
# contract between a probe script's result/ directory and the verdict
# (issue #10 fix round): a branch that dies before it ever reaches the
# mount step must never be read as SUCCESS, whether its result file is
# absent (the normal case, after apparmor-loop-probe.sh's own fix) or
# still present with stale, pre-outcome content (the defense-in-depth case
# classify_branch_pair itself now rejects, independent of the probe
# script). Constructs lab fixtures with plain mktemp -d + files, exactly
# the shape apparmor-loop-probe.sh/apparmor-tmpfs-probe.sh would leave
# behind — no Docker, no loop device, no AppArmor needed for this either.
#
# Run and its verbatim output committed at
# spikes/s9/results/apparmor-classify-test.log — this is the artifact that
# proves the rule now rejects the exact strings that fooled it before, and
# that a branch which never reached the mount step can no longer read as
# SUCCESS.
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./apparmor-classify.sh
source "$HERE/apparmor-classify.sh"

FAIL=0
total=0
pass=0

# check NAME EXIT STDERR EXPECTED
check() {
  local name="$1" exit_code="$2" stderr_text="$3" expected="$4"
  local actual
  actual=$(classify_mount_failure "$exit_code" "$stderr_text")
  total=$((total + 1))
  if [[ "$actual" == "$expected" ]]; then
    pass=$((pass + 1))
    printf 'PASS  %-45s exit=%-4s -> %-14s (expected %s)\n' "$name" "$exit_code" "$actual" "$expected"
  else
    FAIL=1
    printf 'FAIL  %-45s exit=%-4s -> %-14s (expected %s)\n' "$name" "$exit_code" "$actual" "$expected"
  fi
}

# check_verdict NAME CONTROL_EXIT CONTROL_STDERR VARIANT_EXIT VARIANT_STDERR EXPECTED
check_verdict() {
  local name="$1" c_exit="$2" c_stderr="$3" v_exit="$4" v_stderr="$5" expected="$6"
  local actual reason reason_file
  total=$((total + 1))
  reason_file=$(mktemp)
  # classify_verdict's exit status is 1 for VOID by design (see the
  # function itself) — capture the echoed verdict regardless of that exit
  # status, since it is not an error in this test harness's own sense.
  actual=$(classify_verdict "$c_exit" "$c_stderr" "$v_exit" "$v_stderr" 2>"$reason_file" || true)
  reason=$(cat "$reason_file" 2>/dev/null || true)
  rm -f "$reason_file"
  if [[ "$actual" == "$expected" ]]; then
    pass=$((pass + 1))
    printf 'PASS  %-45s -> %-14s (expected %s)\n' "$name" "$actual" "$expected"
  else
    FAIL=1
    printf 'FAIL  %-45s -> %-14s (expected %s)\n' "$name" "$actual" "$expected"
  fi
  printf '      %s\n' "$reason"
}

echo "== classify_mount_failure: single-branch cases =="
echo

# The exact EBUSY string from run 35057453620's own log
# (spikes/s9/README.md, "Bug" not yet written up there — this is the fix).
# libmount's busy message, not a permission message: must NOT classify as
# a denial.
check "run-35057453620 verbatim EBUSY" 32 \
  "mount: /mnt/probe: /dev/loop0 already mounted or mount point busy." \
  "OTHER-FAILURE"

# A representative real denial: exact AppArmor/libmount wording for a
# mount(2) that returned EACCES/EPERM under a confining profile.
check "representative AppArmor denial" 32 \
  "mount: /mnt/probe: permission denied." \
  "DENIED"

# A second representative denial phrasing (EPERM path).
check "representative EPERM denial" 1 \
  "mount: operation not permitted." \
  "DENIED"

# A clean success.
check "clean success" 0 "" "SUCCESS"

# Unrelated failures from the probe's own earlier bugs — run 35049304081's
# missing loop node, and run 35056076616's undersized-image mkfs.xfs
# refusal. Neither is a permission failure; both must VOID.
check "run-35049304081 lost loop node" 1 \
  "losetup: /probe.img: failed to set up loop device: No such file or directory
losetup: device node /dev/loop0 (7:0) is lost. You may use mknod(1) to recover it." \
  "OTHER-FAILURE"

check "run-35056076616 undersized mkfs.xfs" 1 \
  "mkfs.xfs: Filesystem must be larger than 300MB." \
  "OTHER-FAILURE"

echo
echo "== classify_verdict: end-to-end control+variant combinations =="
echo

# The exact scenario that produced run 35057453620's wrong "REQUIRED"
# verdict: a succeeding control, an EBUSY variant. Correct answer is VOID.
check_verdict "control OK, variant EBUSY (run 35057453620's own case)" \
  0 "" 32 "mount: /mnt/probe: /dev/loop0 already mounted or mount point busy." \
  "VOID"

# A genuine denial on the variant, control clean.
check_verdict "control OK, variant denied" \
  0 "" 32 "mount: /mnt/probe: permission denied." \
  "REQUIRED"

# Both succeed.
check_verdict "control OK, variant OK" \
  0 "" 0 "" \
  "NOT-REQUIRED"

# Control itself fails (run 35056076616's own case, before the size fix) —
# must VOID regardless of what the variant did or would have done.
check_verdict "control fails on undersized mkfs.xfs (run 35056076616's own case)" \
  1 "mkfs.xfs: Filesystem must be larger than 300MB." 0 "" \
  "VOID"

# Both branches fail identically on a probe bug (run 35049304081's own
# case) — must VOID, not assert a verdict either way.
check_verdict "both branches fail on lost loop node (run 35049304081's own case)" \
  1 "losetup: device node /dev/loop0 (7:0) is lost. You may use mknod(1) to recover it." \
  1 "losetup: device node /dev/loop0 (7:0) is lost. You may use mknod(1) to recover it." \
  "VOID"

echo
echo "== classify_branch_pair / read_result: stale-file-vs-docker_exit contract (issue #10 fix round) =="
echo

LAB_CLEANUP=()
trap 'rm -rf -- "${LAB_CLEANUP[@]}"' EXIT

# make_lab [FIELD=VALUE ...] -> creates a fresh temp lab dir (with a
# result/ subdir) containing exactly the given result files, and echoes
# its path. Called with no arguments, it makes a lab dir whose result/
# fields are all genuinely absent — the "died before writing anything"
# case.
make_lab() {
  local lab
  lab=$(mktemp -d)
  LAB_CLEANUP+=("$lab")
  mkdir -p -- "$lab/result"
  local kv
  for kv in "$@"; do
    printf '%s' "${kv#*=}" > "$lab/result/${kv%%=*}"
  done
  printf '%s' "$lab"
}

# check_branch_pair NAME CONTROL_LAB CONTROL_DOCKER_EXIT VARIANT_LAB VARIANT_DOCKER_EXIT EXPECTED
check_branch_pair() {
  local name="$1" c_lab="$2" c_dexit="$3" v_lab="$4" v_dexit="$5" expected="$6"
  local actual reason reason_file
  total=$((total + 1))
  reason_file=$(mktemp)
  actual=$(classify_branch_pair "$c_lab" "$c_dexit" "$v_lab" "$v_dexit" 2>"$reason_file" || true)
  reason=$(cat "$reason_file" 2>/dev/null || true)
  rm -f "$reason_file"
  if [[ "$actual" == "$expected" ]]; then
    pass=$((pass + 1))
    printf 'PASS  %-70s -> %-14s (expected %s)\n' "$name" "$actual" "$expected"
  else
    FAIL=1
    printf 'FAIL  %-70s -> %-14s (expected %s)\n' "$name" "$actual" "$expected"
  fi
  printf '      %s\n' "$reason"
}

# The exact bug this fix round closes: apparmor-loop-probe.sh used to
# write a success-shaped placeholder (result/exit=0) before apt-get,
# mknod, losetup or mkfs.xfs/mount ever ran. A routine failure earlier
# than the mount step (an apt mirror flake, here simulated as a plain
# nonzero, non-97 docker exit) then killed the script under `set -euo
# pipefail`, leaving that placeholder file behind unedited. The probe
# script itself no longer writes it (apparmor-loop-probe.sh) — this proves
# the reader also refuses to be fooled by it, independent of the writer,
# for any lab dir shaped this way now or in the future.
control_died_stale=$(make_lab exit=0 step=none stderr=)
variant_healthy_absent=$(make_lab)
check_branch_pair "control died before mount, stale result/exit=0 present (docker_exit=17) -> never SUCCESS" \
  "$control_died_stale" 17 "$variant_healthy_absent" 0 \
  "VOID"

# Same bug, on the other branch: a stale variant result/exit=0 must never
# read as "variant succeeded" (NOT-REQUIRED) nor be confused with a denial
# (REQUIRED) — a branch that never reached the mount step says nothing
# about AppArmor either way.
control_healthy_absent=$(make_lab)
variant_died_stale=$(make_lab exit=0 step=none stderr=)
check_branch_pair "variant died before mount, stale result/exit=0 present (docker_exit=9) -> never SUCCESS or REQUIRED" \
  "$control_healthy_absent" 0 "$variant_died_stale" 9 \
  "VOID"

# result file absent entirely, on both sides, with a genuine denial on the
# variant: the pre-existing docker_exit fallback (unchanged for the
# absent-file case) must still let a real denial reach REQUIRED.
control_absent=$(make_lab)
variant_denied=$(make_lab exit=32 "stderr=mount: /mnt/probe: permission denied.")
check_branch_pair "result files absent, variant genuinely denied (docker_exit=0 both) -> REQUIRED unaffected" \
  "$control_absent" 0 "$variant_denied" 0 \
  "REQUIRED"

# result files absent entirely on both sides, both branches genuinely
# succeeded (docker_exit=0, nothing to read): the original absent-file
# fallback behaviour, unchanged.
control_absent2=$(make_lab)
variant_absent2=$(make_lab)
check_branch_pair "result files absent, both branches succeeded (docker_exit=0 both) -> NOT-REQUIRED unaffected" \
  "$control_absent2" 0 "$variant_absent2" 0 \
  "NOT-REQUIRED"

# Defense in depth: the sentinel-97 short-circuit must still fire before
# any file is ever read, even if that lab dir also happens to carry a
# stale success-shaped result file.
control_void_stale=$(make_lab exit=0)
variant_absent3=$(make_lab)
check_branch_pair "control hit sentinel 97 despite a stale result/exit=0 present -> VOID via the 97 path, file never trusted" \
  "$control_void_stale" 97 "$variant_absent3" 0 \
  "VOID"

echo
echo "== summary: $pass/$total passed =="

exit "$FAIL"
