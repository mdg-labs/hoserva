#!/usr/bin/env bash
# Host-side orchestrator for the S9 AppArmor-necessity check (issue #10),
# run from the `s9-hosted-probe.yml` `lab` job. Replaces the inline shell
# the "AppArmor necessity check" step used to carry directly, after that
# step produced three void-but-confident verdicts in a row (see
# apparmor-classify.sh's own header for the run-by-run history) — none of
# them from AppArmor.
#
# Runs TWO independent probes and never lets one borrow the other's
# result (requirement F):
#   - the faithful probe (apparmor-loop-probe.sh): mknod, losetup, XFS,
#     mount — the actual mechanics the real lab recipe uses.
#   - the loop-free mediation probe (apparmor-tmpfs-probe.sh): a bare
#     `mount -t tmpfs`, isolating the one syscall in question
#     (`mount(2)`) from every loop-device/XFS-sizing/host-global-device
#     concern that has corrupted this experiment three times running.
#
# Each probe classifies its own control+variant pair with
# apparmor-classify.sh's classify_verdict — the same function
# test-apparmor-classify.sh already exercises against the exact strings
# that produced runs 35049304081, 35056076616 and 35057453620's wrong
# verdicts, so this script's classification behaviour is not new logic
# introduced here unverified; it's the tested library, called several
# times.
#
# issue #10 fix round: run 35068617498 printed "Probe 1/2", ran both loop
# branches, printed a correct VOID reason for the loop probe — then exited
# immediately. "Probe 2/2" (the loop-free tmpfs probe below, the one
# designed to answer the AppArmor question without any of the loop/XFS
# machinery that produced every void so far) never ran, because
# classify_verdict signalled VOID through its OWN return status as well as
# through its echoed string, classify_branch_pair forwarded that status,
# and this script captured it with a bare `verdict=$(classify_branch_pair
# ...)` under `set -euo pipefail` two lines below — so a VOID at probe 1
# killed the script on the spot. classify_verdict/classify_branch_pair now
# always return 0 (apparmor-classify.sh); the verdict is communicated by
# the echoed string alone, which is all this script — and
# test-apparmor-classify.sh's `|| true` capture idiom — ever read anyway.
# See apparmor-classify.sh's own comments on classify_verdict and
# decide_final_verdict for the full reasoning, and
# test-apparmor-classify.sh's "set -e survival" cases for the reproduction.
#
# All progress and diagnostics go to stderr; the only things this script
# ever writes to stdout are its own final verdict word (NOT-REQUIRED /
# REQUIRED / VOID, exactly once, as the very last line) and nothing else —
# every helper below is deliberately disciplined about this, because a
# stray unredirected `echo` inside a function whose output is meant to be
# captured is exactly the kind of bug this rewrite exists to stop
# repeating. Exit status: 0 for a concluded NOT-REQUIRED or REQUIRED
# verdict (both probes agree, and the tmpfs probe agrees with itself run
# both orders); 1 for VOID, including a probe disagreement — reported,
# never resolved by picking one side (requirement F).
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -- "$HERE/../../.." && pwd)

# shellcheck source=./apparmor-classify.sh
source "$HERE/apparmor-classify.sh"

void() {
  echo "$1" >&2
  echo "VOID"
  exit 1
}

[[ -r "$REPO_ROOT/scripts/devenv/lib.sh" ]] || \
  void "cannot find scripts/devenv/lib.sh at $REPO_ROOT/scripts/devenv/lib.sh — nothing to reuse for device attribution (requirement C)"

# run_branch OUTVAR_PREFIX SCRIPT NEEDS_LOOP SECURITY_OPT...
# Runs one branch of one probe in a fresh container against a fresh,
# per-branch host directory (never shared with any other branch or with
# the real lab's .lab/<id> tree — each branch gets its own mktemp -d, so a
# leftover file from one branch can never be read by another). Sets
# ${OUTVAR_PREFIX}_LAB / ${OUTVAR_PREFIX}_DOCKER_EXIT so the caller can
# read the result files apparmor-loop-probe.sh / apparmor-tmpfs-probe.sh
# wrote. All of this function's own progress output goes to stderr —
# nothing here is meant to be captured by a caller.
run_branch() {
  local outvar="$1" script="$2" needs_loop="$3"
  shift 3
  local security_opts=("$@")
  local branch_lab
  branch_lab=$(mktemp -d)

  local loop_args=()
  if [[ "$needs_loop" == "loop" ]]; then
    loop_args=(--device /dev/loop-control --device-cgroup-rule 'b 7:* rmw')
  fi

  echo "## running $script (${security_opts[*]:-default profile}) in $branch_lab" >&2
  set +e
  docker run --rm \
    --cap-add SYS_ADMIN \
    "${loop_args[@]}" \
    "${security_opts[@]}" \
    -v "$branch_lab:/probe" \
    -v "$REPO_ROOT/scripts/devenv/lib.sh:/opt/hoserva-lib/lib.sh:ro" \
    -v "$REPO_ROOT/spikes/s9/scripts:/opt/s9-scripts:ro" \
    debian:trixie-slim bash "/opt/s9-scripts/$script" /probe >&2
  local docker_exit=$?
  set -e
  echo "## $script container exit: $docker_exit" >&2

  printf -v "${outvar}_LAB" '%s' "$branch_lab"
  printf -v "${outvar}_DOCKER_EXIT" '%s' "$docker_exit"
}

# read_result and classify_branch_pair now live in apparmor-classify.sh
# (sourced above), alongside classify_verdict/classify_mount_failure —
# moved there in the issue #10 fix round so test-apparmor-classify.sh can
# exercise the file-reading contract directly, with constructed lab
# fixtures, the same way it already exercises the pure classification
# logic. See that file's own comments for what changed and why.

# print_branch_diagnostics LABEL LAB DOCKER_EXIT -> always writes to
# stderr (every call site redirects it there), never to stdout:
# requirement E's "diagnosable from its own log without guessing" is about
# the step's log, not about anything this script needs to parse back out
# of itself.
#
# DOCKER_EXIT is the same value classify_branch_pair is called with for
# this branch, and the "exit" line below falls back to it for exactly the
# same reason classify_branch_pair does (apparmor-classify.sh): a stale
# hardcoded "0" default here would print an "exit: 0" line for a branch
# that died before ever reaching the mount step — misleading even after
# the verdict itself is correctly VOID, and precisely the kind of
# diagnostic falsely implying success this fix round exists to remove.
print_branch_diagnostics() {
  local label="$1" lab="$2" docker_exit="$3"
  {
    echo "### $label diagnostics ($lab)"
    echo "device: $(read_result "$lab" device '(none — branch never attached one)')"
    echo "step that failed: $(read_result "$lab" step none)"
    echo "exit: $(read_result "$lab" exit "$docker_exit")"
    echo "attribution: $(read_result "$lab" attribution 'n/a (tmpfs probe)')"
    echo "detach barrier: $(read_result "$lab" detach 'n/a (tmpfs probe)')"
    echo "stderr:"
    read_result "$lab" stderr '(empty)'
    echo "losetup -a after attach:"
    read_result "$lab" losetup-a-after-attach.log '(n/a)'
    echo "losetup -a at end:"
    read_result "$lab" losetup-a-final.log '(n/a)'
    echo "apparmor profiles listing:"
    read_result "$lab" apparmor-profiles-listing.log '(n/a)'
    echo "kernel/audit apparmor lines (dmesg + journalctl -k, best-effort):"
    read_result "$lab" audit-apparmor-lines.log '(none captured — not available on this runner, or genuinely none)'
    echo
  } >&2
}

echo "############################################" >&2
echo "## Probe 1/2: faithful loop/XFS recipe" >&2
echo "############################################" >&2
echo "## === loop probe: control (apparmor=unconfined) ===" >&2
run_branch LOOP_CONTROL apparmor-loop-probe.sh loop --security-opt apparmor=unconfined
print_branch_diagnostics "loop probe control" "$LOOP_CONTROL_LAB" "$LOOP_CONTROL_DOCKER_EXIT"
echo "## === loop probe: variant (default AppArmor profile) ===" >&2
run_branch LOOP_VARIANT apparmor-loop-probe.sh loop
print_branch_diagnostics "loop probe variant" "$LOOP_VARIANT_LAB" "$LOOP_VARIANT_DOCKER_EXIT"
loop_verdict=$(classify_branch_pair "$LOOP_CONTROL_LAB" "$LOOP_CONTROL_DOCKER_EXIT" "$LOOP_VARIANT_LAB" "$LOOP_VARIANT_DOCKER_EXIT")
echo "## loop probe verdict: $loop_verdict" >&2

echo >&2
echo "############################################" >&2
echo "## Probe 2/2: loop-free tmpfs mediation probe (forward order: control, then variant)" >&2
echo "############################################" >&2
echo "## === tmpfs probe: control (apparmor=unconfined) ===" >&2
run_branch TMPFS_CONTROL apparmor-tmpfs-probe.sh no-loop --security-opt apparmor=unconfined
print_branch_diagnostics "tmpfs probe control" "$TMPFS_CONTROL_LAB" "$TMPFS_CONTROL_DOCKER_EXIT"
echo "## === tmpfs probe: variant (default AppArmor profile) ===" >&2
run_branch TMPFS_VARIANT apparmor-tmpfs-probe.sh no-loop
print_branch_diagnostics "tmpfs probe variant" "$TMPFS_VARIANT_LAB" "$TMPFS_VARIANT_DOCKER_EXIT"
tmpfs_verdict=$(classify_branch_pair "$TMPFS_CONTROL_LAB" "$TMPFS_CONTROL_DOCKER_EXIT" "$TMPFS_VARIANT_LAB" "$TMPFS_VARIANT_DOCKER_EXIT")
echo "## tmpfs probe verdict (forward order): $tmpfs_verdict" >&2

# Order-sensitivity (requirement G): a second, reversed-order run of the
# cheap tmpfs probe (no apt-get install, no mkfs.xfs, no loop device —
# seconds, not the ~30-60s the loop probe's package install alone costs
# per branch). If reversing which branch runs first changes the verdict,
# that is exactly the kind of confound that produced run 35057453620's
# wrong answer (a still-releasing resource from the branch that ran
# first), and it is reported as a finding, not silently averaged away.
#
# The loop probe is NOT doubled this way: its own detach barrier
# (apparmor-loop-probe.sh, requirement D) already targets the specific
# race 35057453620 hit directly, at the resource level, rather than by
# inference from run order, and a second full loop-probe pass would
# roughly double this step's runtime (an apt-get install and a mkfs.xfs
# cycle per extra branch) for a check the barrier already covers more
# directly. If a future run's diagnostics show the barrier itself is
# insufficient, doubling the loop probe too is the next thing to add here
# — not attempted this round (see the execution report for this
# reasoning stated plainly, per requirement G).
echo >&2
echo "############################################" >&2
echo "## Order check: tmpfs probe run reversed (variant, then control)" >&2
echo "############################################" >&2
echo "## === tmpfs probe (reversed order): variant (default AppArmor profile) ===" >&2
run_branch TMPFS_R_VARIANT apparmor-tmpfs-probe.sh no-loop
print_branch_diagnostics "tmpfs probe reversed variant" "$TMPFS_R_VARIANT_LAB" "$TMPFS_R_VARIANT_DOCKER_EXIT"
echo "## === tmpfs probe (reversed order): control (apparmor=unconfined) ===" >&2
run_branch TMPFS_R_CONTROL apparmor-tmpfs-probe.sh no-loop --security-opt apparmor=unconfined
print_branch_diagnostics "tmpfs probe reversed control" "$TMPFS_R_CONTROL_LAB" "$TMPFS_R_CONTROL_DOCKER_EXIT"
tmpfs_reversed_verdict=$(classify_branch_pair "$TMPFS_R_CONTROL_LAB" "$TMPFS_R_CONTROL_DOCKER_EXIT" "$TMPFS_R_VARIANT_LAB" "$TMPFS_R_VARIANT_DOCKER_EXIT")
echo "## tmpfs probe verdict (reversed order): $tmpfs_reversed_verdict" >&2

echo >&2
echo "############################################" >&2
echo "## Combined verdict" >&2
echo "############################################" >&2

# decide_final_verdict (apparmor-classify.sh) carries the same requirement
# F/G logic this block used to hold inline; moved so it can be exercised
# directly, without Docker, by test-apparmor-classify.sh's check_decide()
# cases. Called directly inside this `if` — never captured with a bare
# `x=$(decide_final_verdict ...)` — so its own diagnostics stream straight
# to stderr and its exit status (1 for a genuine VOID here, the very last
# decision this script makes) is read explicitly rather than risking the
# same `set -e`-swallows-a-VOID trap this fix round exists to close for
# classify_verdict/classify_branch_pair (see apparmor-classify.sh's
# comment on decide_final_verdict for why this one case is meant to be
# terminal).
if decide_final_verdict "$loop_verdict" "$tmpfs_verdict" "$tmpfs_reversed_verdict"; then
  exit 0
else
  exit 1
fi
