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
  # classify_verdict's exit status is ALWAYS 0, including VOID (issue #10
  # fix round — see that function's own comment for why the old `return 1`
  # on VOID was the bug that let a VOID at probe 1 kill run-apparmor-
  # check.sh before probe 2 ever ran). The `|| true` here is therefore
  # redundant now, kept only so this line keeps working unchanged if that
  # contract ever regresses.
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
echo "== set -e survival: a VOID verdict must not abort the caller (issue #10 fix round) =="
echo

# check_survives_set_e NAME SCRIPT_BODY EXPECT_EXIT EXPECT_STDOUT
# Runs SCRIPT_BODY in its own `bash -c`, under `set -euo pipefail`,
# exactly like run-apparmor-check.sh, and compares its exit status and
# full stdout against EXPECT_EXIT/EXPECT_STDOUT verbatim. This is the only
# way to actually prove the bug: sourcing apparmor-classify.sh into this
# test script's own already-running shell (as every case above does) never
# exercises `set -e` killing a *caller* on a captured nonzero return,
# because none of the checks above hand classify_verdict's/
# classify_branch_pair's raw exit status to `set -e` unguarded — they all
# use `|| true` or (for decide_final_verdict, below) the `&&`/`||` idiom.
# run-apparmor-check.sh's own bug was exactly that: a bare
# `verdict=$(classify_branch_pair ...)` with no guard, under `set -e`. This
# reproduces that literally, in a subshell, so the reproduction cannot
# affect this test script's own state.
check_survives_set_e() {
  local name="$1" body="$2" expect_exit="$3" expect_stdout="$4"
  local actual_stdout actual_exit
  total=$((total + 1))
  actual_stdout=$(HERE="$HERE" bash -c "$body" 2>/dev/null) && actual_exit=0 || actual_exit=$?
  if [[ "$actual_exit" -eq "$expect_exit" && "$actual_stdout" == "$expect_stdout" ]]; then
    pass=$((pass + 1))
    printf 'PASS  %-70s -> exit=%s\n' "$name" "$actual_exit"
  else
    FAIL=1
    printf 'FAIL  %-70s -> exit=%s (expected %s)\n' "$name" "$actual_exit" "$expect_exit"
    printf '      stdout: %q\n' "$actual_stdout"
    printf '      expected stdout: %q\n' "$expect_stdout"
  fi
}

# Reproduces run 35068617498 exactly: probe 1 (here, one classify_verdict
# call standing in for the loop probe) comes back VOID; the script must
# still reach probe 2 (a second classify_verdict call standing in for the
# tmpfs probe) and its own final echo, and must exit 0 overall, because
# neither call's own return status is meant to end the script — only the
# combined decision (decide_final_verdict, tested below) is.
#
# This is the case that must fail against the pre-fix idiom and pass
# against the fixed one; see spikes/s9/results/ for the committed
# before/after runs proving it does.
# shellcheck disable=SC2016
# The single-quoted block below is a script for a SEPARATE bash -c
# invocation (inside check_survives_set_e, above); its $HERE/$probe1/
# $probe2 are meant to expand there, not here.
check_survives_set_e \
  "a VOID at probe 1 does not stop probe 2 from running, under set -euo pipefail" \
  '
set -euo pipefail
# shellcheck source=./apparmor-classify.sh
source "$HERE/apparmor-classify.sh"
probe1=$(classify_verdict 0 "" 32 "mount: /mnt/probe: /dev/loop0 already mounted or mount point busy." 2>/dev/null)
echo "PROBE1_RAN:$probe1"
probe2=$(classify_verdict 0 "" 0 "" 2>/dev/null)
echo "PROBE2_RAN:$probe2"
' \
  0 \
  "PROBE1_RAN:VOID
PROBE2_RAN:NOT-REQUIRED"

# Same reproduction one level up, through classify_branch_pair — the
# function run-apparmor-check.sh's own three call sites (loop probe,
# tmpfs probe forward, tmpfs probe reversed) actually call — using
# constructed lab fixtures exactly like check_branch_pair above, so the
# same trap is proven closed at the layer the real script uses, not only
# at classify_verdict's.
# shellcheck disable=SC2016
# Same as above: a script for a SEPARATE bash -c invocation, not this one.
check_survives_set_e \
  "a VOID classify_branch_pair result does not stop the next probe from running, under set -euo pipefail" \
  '
set -euo pipefail
# shellcheck source=./apparmor-classify.sh
source "$HERE/apparmor-classify.sh"
control_lab=$(mktemp -d); mkdir -p "$control_lab/result"
variant_lab=$(mktemp -d); mkdir -p "$variant_lab/result"
printf 32 > "$variant_lab/result/exit"
printf "mount: /mnt/probe: /dev/loop0 already mounted or mount point busy." > "$variant_lab/result/stderr"
probe1=$(classify_branch_pair "$control_lab" 0 "$variant_lab" 0 2>/dev/null)
echo "PROBE1_RAN:$probe1"
control_lab2=$(mktemp -d); mkdir -p "$control_lab2/result"
variant_lab2=$(mktemp -d); mkdir -p "$variant_lab2/result"
probe2=$(classify_branch_pair "$control_lab2" 0 "$variant_lab2" 0 2>/dev/null)
echo "PROBE2_RAN:$probe2"
' \
  0 \
  "PROBE1_RAN:VOID
PROBE2_RAN:NOT-REQUIRED"

echo
echo "== decide_final_verdict: the orchestrator's combined-decision step (issue #10 fix round) =="
echo

# check_decide NAME LOOP_VERDICT TMPFS_VERDICT TMPFS_REVERSED_VERDICT EXPECTED_VERDICT EXPECTED_EXIT
# Unlike classify_verdict/classify_branch_pair, decide_final_verdict's
# exit status IS meaningful (1 for VOID) by design — it is the terminal
# decision run-apparmor-check.sh makes, called directly inside an `if`,
# never captured — so this harness captures it deliberately with the
# `x=$(...) && rc=0 || rc=$?` idiom (exempt from set -e, since the
# assignment is checked by the following `&&`/`||`) rather than the `||
# true` used for classify_verdict/classify_branch_pair above, to prove
# both the echoed verdict AND the real exit status.
check_decide() {
  local name="$1" loop="$2" tmpfs="$3" tmpfs_rev="$4" expect_verdict="$5" expect_exit="$6"
  local actual rc reason reason_file
  total=$((total + 1))
  reason_file=$(mktemp)
  actual=$(decide_final_verdict "$loop" "$tmpfs" "$tmpfs_rev" 2>"$reason_file") && rc=0 || rc=$?
  reason=$(cat "$reason_file" 2>/dev/null || true)
  rm -f "$reason_file"
  if [[ "$actual" == "$expect_verdict" && "$rc" -eq "$expect_exit" ]]; then
    pass=$((pass + 1))
    printf 'PASS  %-70s -> %-14s exit=%s (expected %s exit=%s)\n' "$name" "$actual" "$rc" "$expect_verdict" "$expect_exit"
  else
    FAIL=1
    printf 'FAIL  %-70s -> %-14s exit=%s (expected %s exit=%s)\n' "$name" "$actual" "$rc" "$expect_verdict" "$expect_exit"
  fi
  printf '      %s\n' "$reason"
}

check_decide "both probes agree NOT-REQUIRED, tmpfs order-consistent" \
  "NOT-REQUIRED" "NOT-REQUIRED" "NOT-REQUIRED" \
  "NOT-REQUIRED" 0

check_decide "both probes agree REQUIRED, tmpfs order-consistent" \
  "REQUIRED" "REQUIRED" "REQUIRED" \
  "REQUIRED" 0

# This is the case requirement checks that a VOID combined verdict still
# yields a non-zero exit from the orchestrator's own decision path, not
# just an echoed "VOID" string.
check_decide "loop probe individually VOID -> combined VOID, non-zero exit" \
  "VOID" "NOT-REQUIRED" "NOT-REQUIRED" \
  "VOID" 1

check_decide "tmpfs probe order-sensitive (forward != reversed) -> combined VOID, non-zero exit" \
  "NOT-REQUIRED" "NOT-REQUIRED" "REQUIRED" \
  "VOID" 1

check_decide "probes disagree with each other -> combined VOID, non-zero exit" \
  "REQUIRED" "NOT-REQUIRED" "NOT-REQUIRED" \
  "VOID" 1

echo
echo "== summary: $pass/$total passed =="

exit "$FAIL"
