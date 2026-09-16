#!/usr/bin/env bash
# Pure text classification for the AppArmor necessity probe (S9, issue #10).
#
# Extracted into its own file, sourced by both the workflow step and
# test-apparmor-classify.sh, so the one piece of logic that has produced
# three void verdicts in a row — never run against a real loop device or
# AppArmor, purely a function of an exit status and a stderr string — can
# finally be exercised directly instead of trusted by inspection.
#
# Run history this fixes:
#   - run 35049304081: no failure classification at all — a `losetup`
#     "device node ... is lost" failure in *both* branches (a probe bug,
#     the container's own /dev/loopN nodes were never mknod-ed) was read as
#     "apparmor=unconfined is required".
#   - run 35056076616: same absence — a `mkfs.xfs` "Filesystem must be
#     larger than 300MB." failure in the *control* branch voided the run
#     correctly (the control-failure guard already existed and worked) but
#     nothing would have stopped the identical bug from mis-firing on the
#     variant branch instead, which is exactly what happened next.
#   - run 35057453620: this is the one that would have been wrongly
#     answered even with a working control guard. The variant's `mount`
#     failed with libmount's `already mounted or mount point busy.` — an
#     EBUSY, not a denial — and the step's `else` branch attributed it to
#     AppArmor anyway ("apparmor=unconfined IS required"). EBUSY means the
#     kernel's `security_sb_mount()` LSM hook — which AppArmor's mount
#     mediation is implemented as — already returned before the busy check
#     ran; an EBUSY return is proof mediation was *passed*, not evidence it
#     denied anything.
#   - fix round (this file's read_result/classify_branch_pair, moved here
#     from run-apparmor-check.sh): apparmor-loop-probe.sh used to write a
#     success-shaped placeholder result file (exit=0, step=none, empty
#     stderr) before apt-get/mknod/losetup/mkfs.xfs/mount ever ran, so a
#     routine failure earlier than the mount step killed the script with a
#     nonzero, non-97 exit while leaving that placeholder behind — and the
#     old classify_branch_pair only fell back to the container's real
#     docker exit when the result file was *absent*, never when it was
#     stale, so the branch silently read SUCCESS. Fixed on both sides: the
#     probe script no longer writes that placeholder (see its own header),
#     and classify_branch_pair below now trusts a nonzero, non-97 docker
#     exit over anything a result file claims, so neither probe script has
#     to get its write-order exactly right for the reader to stay correct.
#
# classify_mount_failure never infers a denial from "some string on
# stderr" or "nonzero exit" alone: it requires text that names the
# mediated operation failing with EACCES/EPERM ("permission denied",
# "operation not permitted"), the specific errno family AppArmor's mount
# hook returns on a real denial (`apparmor(7)`, `security_sb_mount()`).
# Every other nonzero exit — EBUSY, ENODEV, "too small for mkfs.xfs", a
# missing loop node, anything else — classifies as OTHER-FAILURE, which
# the caller must turn into VOID, never REQUIRED.

# classify_mount_failure EXIT_CODE STDERR_TEXT
# Echoes exactly one of: SUCCESS DENIED OTHER-FAILURE
classify_mount_failure() {
  local exit_code="$1" stderr_text="$2"

  if [[ "$exit_code" -eq 0 ]]; then
    echo "SUCCESS"
    return 0
  fi

  # Matched case-insensitively, word-bounded so "permission denied" inside
  # an unrelated sentence still counts (mount/losetup/mkfs.xfs wording
  # varies by tool and libmount version) but a substring inside an
  # otherwise-unrelated word does not.
  if grep -qiE '(^|[^[:alnum:]])(permission denied|operation not permitted)([^[:alnum:]]|$)' <<<"$stderr_text"; then
    echo "DENIED"
    return 0
  fi

  echo "OTHER-FAILURE"
  return 0
}

# audit_corroborates_denial AUDIT_TEXT
# True (exit 0) if AUDIT_TEXT contains a kernel AppArmor denial record for
# a mount operation (apparmor(7) audit format: apparmor="DENIED" ...
# operation="mount"). This is corroborating evidence only — requirement B
# asks that a DENIED verdict be "ideally corroborated" by it, not that its
# absence downgrade a real permission-denied string; a runner that exposes
# no readable audit log (dmesg restricted, no journalctl -k access) must
# not turn a genuine denial into a void result for lack of a log line it
# was never going to get. The caller records whether corroboration was
# found or not; it never gates REQUIRED on it alone.
audit_corroborates_denial() {
  local audit_text="$1"
  grep -qiE 'apparmor="DENIED".*operation="mount"|apparmor="DENIED".*operation="mount"' <<<"$audit_text" 2>/dev/null && return 0
  grep -qiE 'apparmor="DENIED"' <<<"$audit_text" 2>/dev/null && grep -qiE 'operation="mount"' <<<"$audit_text" 2>/dev/null
}

# classify_verdict CONTROL_EXIT CONTROL_STDERR VARIANT_EXIT VARIANT_STDERR [VARIANT_AUDIT]
# Echoes exactly one of: NOT-REQUIRED REQUIRED VOID
# and prints its reasoning to stderr. Never infers REQUIRED from anything
# but a variant classified DENIED; never infers NOT-REQUIRED unless the
# control itself succeeded, since a failed control means the recipe is
# broken and neither branch's result means anything (the guard run
# 35049304081 and 35056076616 both correctly hit, unchanged here).
#
# Return status is ALWAYS 0, on every path, including VOID (issue #10 fix
# round). It used to `return 1` on both VOID paths below, on the theory
# that VOID was an "error" the caller should notice; in practice the only
# caller (run-apparmor-check.sh) captures the echoed verdict with a bare
# `verdict=$(classify_branch_pair ...)` under `set -euo pipefail`, and
# classify_branch_pair's own return status is just this function's,
# forwarded (see below) — so a VOID at probe 1 killed the whole script on
# the spot, on its own return status, before probe 2 (the loop-free tmpfs
# probe — the one designed to answer the AppArmor question without any of
# the loop/XFS machinery that produced every void verdict so far) ever
# ran. Run 35068617498 is the proof: it printed "Probe 1/2", ran both loop
# branches, printed a correct VOID reason, and exited — "Probe 2/2" never
# appeared in the log. The verdict was already fully communicated on
# stdout as one of the three literal strings above; encoding it a second
# time in the exit status was redundant, and that redundancy is what
# created the trap. Callers that need to know a verdict was VOID compare
# the echoed string, exactly as run-apparmor-check.sh's own combined-
# decision step (decide_final_verdict, below) already does.
classify_verdict() {
  local control_exit="$1" control_stderr="$2" variant_exit="$3" variant_stderr="$4"
  local variant_audit="${5:-}"

  local control_status
  control_status=$(classify_mount_failure "$control_exit" "$control_stderr")
  if [[ "$control_status" != "SUCCESS" ]]; then
    echo "VOID"
    echo "reason: control (apparmor=unconfined) itself failed [$control_status, exit=$control_exit] — the probe recipe (mknod/loop/xfs mechanics) is broken here, not an AppArmor result. Fix the recipe before trusting either branch." >&2
    return 0
  fi

  local variant_status
  variant_status=$(classify_mount_failure "$variant_exit" "$variant_stderr")
  case "$variant_status" in
    SUCCESS)
      echo "NOT-REQUIRED"
      echo "reason: control succeeded, and the variant (default AppArmor profile) also succeeded — apparmor=unconfined is not required for this recipe on this runner." >&2
      return 0
      ;;
    DENIED)
      echo "REQUIRED"
      if [[ -n "$variant_audit" ]] && audit_corroborates_denial "$variant_audit"; then
        echo "reason: control succeeded; the variant failed with a confirmed AppArmor denial (permission-denied/operation-not-permitted at the mediated operation), corroborated by a kernel audit record (apparmor=\"DENIED\" ... operation=\"mount\")." >&2
      else
        echo "reason: control succeeded; the variant failed with a confirmed AppArmor denial (permission-denied/operation-not-permitted at the mediated operation). No corroborating kernel audit record was found or available on this runner — the stderr classification alone is the basis for this verdict." >&2
      fi
      return 0
      ;;
    OTHER-FAILURE)
      echo "VOID"
      echo "reason: control succeeded, but the variant failed for a reason other than a confirmed AppArmor denial [exit=$variant_exit]. An EBUSY, a missing device node, an undersized filesystem, or any other non-denial failure says nothing about AppArmor necessity — see run 35057453620, whose EBUSY (\"already mounted or mount point busy\") was wrongly read as a denial by the probe this file replaces." >&2
      return 0
      ;;
  esac
}

# read_result LAB FIELD DEFAULT -> echoes the file's content, or DEFAULT if
# the branch never wrote it (e.g. it died before that field's real outcome
# was known, and correctly left the file absent — see
# apparmor-loop-probe.sh/apparmor-tmpfs-probe.sh's own headers for why
# neither ever writes a field before it is true).
read_result() {
  local lab="$1" field="$2" default="$3"
  if [[ -r "$lab/result/$field" ]]; then
    cat -- "$lab/result/$field"
  else
    echo "$default"
  fi
}

# classify_branch_pair CONTROL_LAB CONTROL_DOCKER_EXIT VARIANT_LAB VARIANT_DOCKER_EXIT
# Echoes exactly one of NOT-REQUIRED / REQUIRED / VOID on stdout, and its
# reasoning to stderr — the caller (run_branch/run-apparmor-check.sh) is
# expected to redirect the reasoning and capture only the verdict line.
# Return status is ALWAYS 0 (issue #10 fix round) — both its own explicit
# `return 0`s below and classify_verdict's now-unconditional `return 0`
# (see that function's own comment for why) — so a caller may safely
# capture its output with a bare `verdict=$(classify_branch_pair ...)`
# even under `set -euo pipefail`, and does not need `|| true` here to keep
# running past a VOID.
#
# Fix-round hardening (issue #10 fix round; see also apparmor-loop-
# probe.sh's own comment on why it no longer pre-writes a placeholder):
# a branch's own container exit status is trusted over anything a stale or
# prematurely-written result file might claim. Both probe scripts are
# written so that reaching their own outcome-determining step always ends
# in docker exit 0 (success or a cleanly-classified failure recorded in
# result/) or the distinct sentinel 97 (handled above, before any file is
# ever read) — never anything else. So a nonzero, non-97 docker exit can
# only mean the container died *before* it ever got there: an apt-get
# mirror flake, a missing tool, `mknod`/`truncate`/`losetup` failing under
# `set -euo pipefail`, all outside either probe's own if/else result-
# writing logic. In exactly that case, whatever result/exit contains — even
# if the file exists, even if it happens to read "0" — is not trusted; the
# branch is scored on its own docker_exit instead, indistinguishable from
# having written no result file at all. This closes the "placeholder file
# present but the branch never reached the mount step reads as SUCCESS"
# class of bug on the reader side, independent of whichever probe script
# does the writing — see test-apparmor-classify.sh's
# "stale-file-vs-docker_exit contract" cases for the exact scenario
# reproduced and proven fixed.
classify_branch_pair() {
  local control_lab="$1" control_docker_exit="$2" variant_lab="$3" variant_docker_exit="$4"

  # A branch that hit an INTERNAL_VOID condition (attribution failure or a
  # detach-barrier timeout) exits its container with 97 — apparmor-loop-
  # probe.sh's own sentinel, distinct from any exit code `mount`/`mkfs.xfs`
  # itself could produce. That is always VOID, and is never handed to
  # classify_mount_failure as if it were the mediated operation's own
  # result (requirement C/D: an attribution or barrier failure is a probe
  # infrastructure problem, not evidence about AppArmor either way).
  if [[ "$control_docker_exit" -eq 97 ]]; then
    echo "reason: the control branch's container hit its own internal-void condition (device attribution or detach-barrier failure, exit 97) — see its diagnostics above. This is a probe-infrastructure problem, not an AppArmor result." >&2
    echo "VOID"
    return 0
  fi
  if [[ "$variant_docker_exit" -eq 97 ]]; then
    echo "reason: the variant branch's container hit its own internal-void condition (device attribution or detach-barrier failure, exit 97) — see its diagnostics above. This is a probe-infrastructure problem, not an AppArmor result." >&2
    echo "VOID"
    return 0
  fi

  local c_exit c_stderr v_exit v_stderr v_audit
  if [[ "$control_docker_exit" -ne 0 ]]; then
    c_exit="$control_docker_exit"
    c_stderr=$(read_result "$control_lab" stderr '')
  else
    c_exit=$(read_result "$control_lab" exit "$control_docker_exit")
    c_stderr=$(read_result "$control_lab" stderr '')
  fi
  if [[ "$variant_docker_exit" -ne 0 ]]; then
    v_exit="$variant_docker_exit"
    v_stderr=$(read_result "$variant_lab" stderr '')
  else
    v_exit=$(read_result "$variant_lab" exit "$variant_docker_exit")
    v_stderr=$(read_result "$variant_lab" stderr '')
  fi
  v_audit=$(read_result "$variant_lab" audit-apparmor-lines.log '')

  classify_verdict "$c_exit" "$c_stderr" "$v_exit" "$v_stderr" "$v_audit"
}

# decide_final_verdict LOOP_VERDICT TMPFS_VERDICT TMPFS_REVERSED_VERDICT
# The orchestrator's combined-decision step (requirements F and G): both
# probes must agree, and the tmpfs probe must agree with itself regardless
# of which branch (control or variant) ran first. Moved here from
# run-apparmor-check.sh (issue #10 fix round) so it can be exercised
# directly by test-apparmor-classify.sh, without Docker, a loop device, or
# AppArmor — the same reason classify_verdict/classify_branch_pair above
# are unit-tested in isolation rather than trusted by inspection.
#
# Unlike classify_verdict/classify_branch_pair, this function's return
# status is deliberately NOT always 0: it returns 1 on every VOID path,
# and 0 only when both probes agree and the tmpfs probe is order-
# consistent. That asymmetry is safe, not a repeat of the bug those two
# functions used to have, because this is the very last decision
# run-apparmor-check.sh ever makes — there is no probe 3 to reach, nothing
# after it that must still run once the verdict is known — so a genuine
# VOID killing the script right here, on its own exit status, via
# `set -euo pipefail`, is exactly the outcome CI needs (VOID must fail the
# job). The caller must still never capture this function's output with a
# bare `x=$(decide_final_verdict ...)`: that would defeat the same trap
# again, one level up, by absorbing this function's own nonzero exit into
# an assignment. run-apparmor-check.sh instead calls it directly inside an
# `if`, which both lets its stdout stream straight through uncaptured and
# reads its exit status explicitly, then chooses `exit 0`/`exit 1` itself
# — see that script's own call site. test-apparmor-classify.sh's
# check_decide() below captures it only for inspection, using the
# `x=$(...) && rc=0 || rc=$?` idiom, which — unlike a bare assignment — is
# exempt from `set -e` because the assignment is checked by the `&&`/`||`
# that follows it on the same command list.
decide_final_verdict() {
  local loop_verdict="$1" tmpfs_verdict="$2" tmpfs_reversed_verdict="$3"

  if [[ "$tmpfs_verdict" != "$tmpfs_reversed_verdict" ]]; then
    echo "reason: The tmpfs probe disagreed with itself depending on which branch ran first: forward order said $tmpfs_verdict, reversed order said $tmpfs_reversed_verdict. That is an order-sensitivity confound (requirement G) — exactly the kind of thing that produced run 35057453620's wrong verdict — and it is reported as a finding, not resolved by picking either order's answer." >&2
    echo "VOID"
    return 1
  fi

  if [[ "$loop_verdict" == "VOID" || "$tmpfs_verdict" == "VOID" ]]; then
    echo "reason: At least one probe was individually VOID — loop probe: $loop_verdict, tmpfs probe: $tmpfs_verdict (see each probe's own reasoning above, on stderr). A VOID probe cannot be outvoted by the other probe's result." >&2
    echo "VOID"
    return 1
  fi

  if [[ "$loop_verdict" != "$tmpfs_verdict" ]]; then
    echo "reason: The two independent probes DISAGREED: the faithful loop/XFS probe said $loop_verdict, the loop-free tmpfs mediation probe said $tmpfs_verdict. Per requirement F, neither probe may claim the other's result — this disagreement is itself the finding this run establishes, and needs investigating (which operation differs between the two recipes under the default profile) before either answer can be trusted." >&2
    echo "VOID"
    return 1
  fi

  echo "reason: Both probes agree (loop: $loop_verdict, tmpfs: $tmpfs_verdict), and the tmpfs probe is order-consistent — this is a strong result." >&2
  echo "$loop_verdict"
  return 0
}
