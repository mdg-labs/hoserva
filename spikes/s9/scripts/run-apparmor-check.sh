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

# shellcheck source=./apparmor-classify.sh
source "$HERE/apparmor-classify.sh"

# issue #10 fix round: hosted-runner guard. Placed immediately after the
# only two things needed to reach it — `set -euo pipefail` above and the
# `source` immediately above it, since hosted_runner_guard itself lives in
# apparmor-classify.sh — and before EVERYTHING else this script does:
# REPO_ROOT is not resolved yet, void() is not defined yet, no lib.sh
# check has run, no mktemp, no docker, no sudo. This is the actual fix for
# the defect this round exists to close (issue #10 dispatch): the host-side
# `sudo dmesg` / `sudo journalctl -k` / `sudo cat
# /sys/kernel/security/apparmor/profiles` calls further down (see
# capture_host_audit / capture_host_apparmor_state) were previously
# guarded only by a comment on capture_host_audit ("this function must
# only ever run on that hosted runner — never on a maintainer's dev
# host") — a comment is not a check, and this script had no runtime guard
# at all before this round. A `sudo` invocation on the maintainer's own
# dev host raises a polkit/PAM prompt on their *desktop session*, and
# three failed authentications trip `pam_faillock`'s `deny=3` and lock
# them out of their own machine, including out of `sudo` itself
# (CLAUDE.md's absolute rule on this outranks finishing any task).
#
# The check is positive, not negative: hosted_runner_guard
# (apparmor-classify.sh) requires GITHUB_ACTIONS to be exactly "true" —
# the literal value GitHub Actions sets in every workflow job's
# environment
# (https://docs.github.com/actions/learn-github-actions/variables#default-environment-variables)
# — and refuses on anything else (unset, empty, "false", any other
# value). It deliberately does NOT try to detect "looks like a dev host":
# that shape of check fails open the instant a dev shell happens to
# export something it doesn't anticipate. No override variable is
# accepted (no `FORCE=1` or similar) — an escape hatch is exactly how a
# guard like this ends up bypassed on a dev host anyway; see this fix
# round's execution report for why one was rejected rather than added.
#
# hosted_runner_guard is a pure function — no sudo, no docker, no side
# effect of any kind — so its allow/refuse decision is unit-tested
# directly in test-apparmor-classify.sh without ever running this script,
# and that same test file's static ordering check greps this file by line
# number to assert no `sudo` or `docker run` call precedes the guard call
# immediately below, so the property this comment block claims is proven
# mechanically, not left to be eyeballed.
if ! hosted_runner_guard "${GITHUB_ACTIONS:-}" >/dev/null; then
  echo "run-apparmor-check.sh refuses to run: no GITHUB_ACTIONS=true signal found in this process's environment. This script performs host-side kernel-audit capture via passwordless sudo (dmesg, journalctl -k, /sys/kernel/security/apparmor/profiles) and is meant to run ONLY as the 'AppArmor necessity check' step of the s9-hosted-probe.yml workflow's lab job, on a GitHub-hosted runner — never on a maintainer's dev host (CLAUDE.md: never trigger a sudo/polkit/PAM prompt on a developer's desktop session)." >&2
  echo "VOID"
  exit 1
fi

REPO_ROOT=$(cd -- "$HERE/../../.." && pwd)

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

# capture_host_audit LAB
# issue #10 host-audit fix round. apparmor-loop-probe.sh and
# apparmor-tmpfs-probe.sh each already do a best-effort in-container
# `dmesg`/`journalctl -k` capture (kept, unchanged — harmless when it comes
# up empty, and free), but that capture runs inside an unprivileged,
# namespaced probe container, which was never going to see the HOST
# kernel's ring buffer or audit trail — every run to date shows this:
# "kernel/audit apparmor lines: (n/a)" on every single branch, including
# the control branches where mediation could not have fired at all. This
# script itself, unlike the probe scripts, runs directly on the runner
# HOST (see the "AppArmor necessity check" step in s9-hosted-probe.yml),
# so it is the one place in this whole check that *can* read the host's
# own dmesg/journalctl -k. GitHub's hosted runners grant the runner user
# passwordless sudo (the `kvm` job's own udev step already relies on
# this); `dmesg` in particular is commonly restricted by
# kernel.dmesg_restrict and needs it. This function must only ever run on
# that hosted runner — never on a maintainer's dev host (CLAUDE.md).
#
# Merges any AppArmor lines found into the SAME
# $LAB/result/audit-apparmor-lines.log apparmor-classify.sh's
# audit_corroborates_denial already reads (so no classifier change is
# needed for this to take effect), on top of whatever the in-container
# capture already wrote there, deduplicated, order preserved. Also writes
# $LAB/result/audit-capture-status via apparmor-classify.sh's
# describe_audit_capture, so a runner where dmesg AND journalctl -k are
# both unreadable is reported as "failed: ..." in diagnostics, never
# silently indistinguishable from "captured fine, found nothing" — the
# exact ambiguity the old `|| : > file` in-container fallback produced on
# every run so far.
capture_host_audit() {
  local lab="$1"
  local host_dmesg host_journal
  local dmesg_rc=0 journal_rc=0

  mkdir -p -- "$lab/result"
  host_dmesg=$(mktemp)
  host_journal=$(mktemp)

  # Neither command runs under `set -e` here — both sit directly in an
  # `if` condition, bash's one standard exception to errexit — and neither
  # is negated with `!`, on purpose: apparmor-loop-probe.sh's own header
  # already documents why `if ! cmd; then rc=$?; fi` is wrong here — `!`
  # inverts the exit status being tested, so `$?` inside that `then` would
  # read the negation's own 0/1, not the command's real rc, and every
  # capture failure would misreport as rc=0 ("succeeded"). Using the
  # positive form with the real rc captured in the `else` branch avoids
  # that trap the same way apparmor-loop-probe.sh's mkfs.xfs/mount block
  # already does.
  # shellcheck disable=SC2024 # the redirect targets are plain files this
  # unprivileged process already owns (mktemp/workspace paths) — only
  # dmesg/journalctl themselves need root to read the kernel log; `sudo
  # tee` would be needed only if the *target* required root, which it does
  # not here.
  if sudo dmesg > "$host_dmesg" 2>"$lab/result/host-dmesg.err"; then
    dmesg_rc=0
  else
    dmesg_rc=$?
  fi
  # shellcheck disable=SC2024 # see above
  if sudo journalctl -k -n 1000 --no-pager > "$host_journal" 2>"$lab/result/host-journalctl.err"; then
    journal_rc=0
  else
    journal_rc=$?
  fi

  # Every command inside this group is individually guarded against
  # `set -e`: `grep` returning 1 (no match, the expected common case on a
  # host with AppArmor disabled or no denial) is not an error here, and a
  # bare `cond && grep ...` would let that 1 propagate and kill the whole
  # script on the very first branch that finds nothing — so each capture
  # uses an `if`/`fi` body with its own `|| true`, never a bare `&&` chain.
  local merged
  merged=$(mktemp)
  {
    if [[ -r "$lab/result/audit-apparmor-lines.log" ]]; then
      cat -- "$lab/result/audit-apparmor-lines.log"
    fi
    if [[ "$dmesg_rc" -eq 0 ]]; then
      grep -i 'apparmor' -- "$host_dmesg" || true
    fi
    if [[ "$journal_rc" -eq 0 ]]; then
      grep -i 'apparmor' -- "$host_journal" || true
    fi
  } | awk '!seen[$0]++' > "$merged"
  mv -- "$merged" "$lab/result/audit-apparmor-lines.log"
  rm -f -- "$host_dmesg" "$host_journal"

  # describe_audit_capture (apparmor-classify.sh) returns 1 on the "both
  # sources unreadable" case, by design (so test-apparmor-classify.sh can
  # tell that path apart from "ok" by exit status too, not only text) —
  # captured with `|| true` so that nonzero return doesn't hit this
  # script's own `set -e`, the same trap classify_verdict/
  # classify_branch_pair used to have (see apparmor-classify.sh's own
  # comment on why those two now always `return 0`; this call sits
  # upstream of any verdict decision, so unlike decide_final_verdict there
  # is no reason for its return status to be terminal here).
  local line_count status
  line_count=$(wc -l < "$lab/result/audit-apparmor-lines.log" | tr -d '[:space:]')
  status=$(describe_audit_capture "$dmesg_rc" "$journal_rc" "$line_count") || true
  echo "$status" > "$lab/result/audit-capture-status"
}

# capture_host_apparmor_state LAB
# issue #10 host-audit fix round, requirement 3 (issue dispatch): the
# per-branch "apparmor profiles listing: not available in this container"
# line every run has printed so far has the same root cause as the audit
# capture above — an unprivileged probe container cannot read
# /sys/kernel/security/apparmor/*. Captured ONCE, on the host, not
# per-branch (this is host/runner-image state, not something either
# branch's own mount call could change) — see print_host_apparmor_state
# for where this is shown, once, ahead of both probes.
capture_host_apparmor_state() {
  local lab="$1"
  mkdir -p -- "$lab/result"

  if [[ -r /sys/module/apparmor/parameters/enabled ]]; then
    cat -- /sys/module/apparmor/parameters/enabled > "$lab/result/apparmor-enabled" 2>/dev/null \
      || echo "unknown: /sys/module/apparmor/parameters/enabled present but unreadable" > "$lab/result/apparmor-enabled"
  else
    echo "unknown: /sys/module/apparmor/parameters/enabled not present on this kernel/runner" > "$lab/result/apparmor-enabled"
  fi

  # shellcheck disable=SC2024 # see capture_host_audit's own comment above
  if sudo cat /sys/kernel/security/apparmor/profiles > "$lab/result/host-apparmor-profiles.log" 2>"$lab/result/host-apparmor-profiles.err"; then
    if grep -qi 'docker-default' "$lab/result/host-apparmor-profiles.log"; then
      echo "present" > "$lab/result/docker-default-profile"
    else
      echo "absent (not listed in /sys/kernel/security/apparmor/profiles)" > "$lab/result/docker-default-profile"
    fi
  else
    echo "unknown: could not read /sys/kernel/security/apparmor/profiles ($(cat -- "$lab/result/host-apparmor-profiles.err" 2>/dev/null))" > "$lab/result/docker-default-profile"
  fi
}

# print_host_apparmor_state LAB -> stderr, once, ahead of both probes —
# mirrors print_branch_diagnostics' formatting so the two are easy to
# read side by side, but this is host/runner-image state, not a
# per-branch result.
print_host_apparmor_state() {
  local lab="$1"
  {
    echo "### host AppArmor state (captured once, on the runner host)"
    echo "kernel AppArmor enabled (/sys/module/apparmor/parameters/enabled): $(read_result "$lab" apparmor-enabled 'unknown')"
    echo "docker-default profile present on host: $(read_result "$lab" docker-default-profile 'unknown')"
    echo
  } >&2
}

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
    echo "apparmor profiles listing (in-container, expected unavailable — see host AppArmor state above):"
    read_result "$lab" apparmor-profiles-listing.log '(n/a)'
    echo "kernel/audit capture status (host dmesg + host journalctl -k, this branch):"
    read_result "$lab" audit-capture-status 'not attempted (unexpected — capture_host_audit should have run for every branch)'
    echo "kernel/audit apparmor lines (host + in-container, merged, best-effort):"
    read_result "$lab" audit-apparmor-lines.log '(none)'
    echo
  } >&2
}

echo "############################################" >&2
echo "## Host state (captured once, on the runner host, ahead of both probes)" >&2
echo "############################################" >&2
HOST_STATE_LAB=$(mktemp -d)
capture_host_apparmor_state "$HOST_STATE_LAB"
print_host_apparmor_state "$HOST_STATE_LAB"

echo "############################################" >&2
echo "## Probe 1/2: faithful loop/XFS recipe" >&2
echo "############################################" >&2
echo "## === loop probe: control (apparmor=unconfined) ===" >&2
run_branch LOOP_CONTROL apparmor-loop-probe.sh loop --security-opt apparmor=unconfined
capture_host_audit "$LOOP_CONTROL_LAB"
print_branch_diagnostics "loop probe control" "$LOOP_CONTROL_LAB" "$LOOP_CONTROL_DOCKER_EXIT"
echo "## === loop probe: variant (default AppArmor profile) ===" >&2
run_branch LOOP_VARIANT apparmor-loop-probe.sh loop
capture_host_audit "$LOOP_VARIANT_LAB"
print_branch_diagnostics "loop probe variant" "$LOOP_VARIANT_LAB" "$LOOP_VARIANT_DOCKER_EXIT"
loop_verdict=$(classify_branch_pair "$LOOP_CONTROL_LAB" "$LOOP_CONTROL_DOCKER_EXIT" "$LOOP_VARIANT_LAB" "$LOOP_VARIANT_DOCKER_EXIT")
echo "## loop probe verdict: $loop_verdict" >&2

echo >&2
echo "############################################" >&2
echo "## Probe 2/2: loop-free tmpfs mediation probe (forward order: control, then variant)" >&2
echo "############################################" >&2
echo "## === tmpfs probe: control (apparmor=unconfined) ===" >&2
run_branch TMPFS_CONTROL apparmor-tmpfs-probe.sh no-loop --security-opt apparmor=unconfined
capture_host_audit "$TMPFS_CONTROL_LAB"
print_branch_diagnostics "tmpfs probe control" "$TMPFS_CONTROL_LAB" "$TMPFS_CONTROL_DOCKER_EXIT"
echo "## === tmpfs probe: variant (default AppArmor profile) ===" >&2
run_branch TMPFS_VARIANT apparmor-tmpfs-probe.sh no-loop
capture_host_audit "$TMPFS_VARIANT_LAB"
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
capture_host_audit "$TMPFS_R_VARIANT_LAB"
print_branch_diagnostics "tmpfs probe reversed variant" "$TMPFS_R_VARIANT_LAB" "$TMPFS_R_VARIANT_DOCKER_EXIT"
echo "## === tmpfs probe (reversed order): control (apparmor=unconfined) ===" >&2
run_branch TMPFS_R_CONTROL apparmor-tmpfs-probe.sh no-loop --security-opt apparmor=unconfined
capture_host_audit "$TMPFS_R_CONTROL_LAB"
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
