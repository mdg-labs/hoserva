#!/usr/bin/env bash
# Fixture tests for l3-junit.sh (doc 06 §7), the writer run-l3-suite.sh uses
# for its JUnit report. They feed it recorded STEP_NAMES / STEP_RESULTS, so no
# VM is needed: a pass, a failure with its reason, a not-yet-implemented step
# and an unselected one as skipped, markup and control characters in names and
# reasons, an unrecognised result as a failure, an aborted run, no file with
# TEST_REPORT_DIR unset, and `make vm-suite-plan` writing nothing. Report files
# are read with python3's XML parser, so an invalid document fails.
set -uo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/vm/l3-junit.sh
source "$script_dir/l3-junit.sh"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fail=0
note() { printf 'test-l3-junit: %s\n' "$*" >&2; }
check() {
  if [[ $2 != "$3" ]]; then
    note "FAIL: $1: got '$2', want '$3'"
    fail=1
  fi
}

# xml_summary <file>: the suite's tests, failures, skipped and time, then one
# line per testcase: name, outcome, and the failure or skipped message.
xml_summary() {
  python3 -I - "$1" <<'PY'
import sys
import xml.etree.ElementTree as ET

root = ET.parse(sys.argv[1]).getroot()
print(root.tag, root.get("name"), root.get("tests"), root.get("failures"), root.get("skipped"), root.get("time"), sep="\t")
for case in root.iter("testcase"):
    failure = case.find("failure")
    skipped = case.find("skipped")
    if failure is not None:
        print(case.get("name"), "failed", failure.get("message"), (failure.text or "").strip().replace("\n", "\\n"), sep="\t")
    elif skipped is not None:
        print(case.get("name"), "skipped", skipped.get("message"), sep="\t")
    else:
        print(case.get("name"), "passed", sep="\t")
PY
}

reset() {
  STEP_NAMES=()
  STEP_RESULTS=()
  STEP_SECONDS=()
  unset L3_SUMMARY_DONE L3_GROUP
}

reset
STEP_NAMES=(
  "install"
  'array stop/start with a <live> share & "quotes"'
  "disk yank and reconstruction"
  "UPS on-battery/power-restored/low-battery"
  "NFS export mount"
  "mystery step"
)
STEP_RESULTS=(
  "PASS"
  "PASS"
  $'FAIL: see disk-yank-check.sh output above (issue #1) <b>& "x"\nsecond line\x01'
  "NOT-YET-IMPLEMENTED: no running domain (install step above did not complete)"
  "SKIPPED (not selected)"
  "weird"
)
STEP_SECONDS=(12 30 7 0 0 1)

out="$tmp/l3-rest.xml"
l3_junit_write l3-rest "$out" || check "write: exit status" "$?" 0
mapfile -t got_lines < <(xml_summary "$out" 2>&1)
check "summary: suite line" "${got_lines[0]:-}" $'testsuite\tl3-rest\t6\t2\t2\t50.000'
check "case: passed" "${got_lines[1]:-}" $'install\tpassed'
check "case: passed, names escaped" "${got_lines[2]:-}" $'array stop/start with a <live> share & "quotes"\tpassed'
check "case: failure carries its reason" "${got_lines[3]:-}" $'disk yank and reconstruction\tfailed\tsee disk-yank-check.sh output above (issue #1) <b>& "x" second line\tsee disk-yank-check.sh output above (issue #1) <b>& "x"\\nsecond line'
check "case: not-yet is skipped with its message" "${got_lines[4]:-}" $'UPS on-battery/power-restored/low-battery\tskipped\tNOT-YET-IMPLEMENTED: no running domain (install step above did not complete)'
check "case: unselected is skipped, not passed" "${got_lines[5]:-}" $'NFS export mount\tskipped\tnot selected'
check "case: unrecognised result fails" "${got_lines[6]:-}" $'mystery step\tfailed\tunrecognised result: weird\t'

# l3_junit_finish: with TEST_REPORT_DIR unset nothing is written and the status
# is not touched.
reset
STEP_NAMES=(install)
STEP_RESULTS=(PASS)
unset_dir="$tmp/unset"
mkdir -p "$unset_dir"
rc=0
(cd "$unset_dir" && unset TEST_REPORT_DIR && L3_SUMMARY_DONE=1 && l3_junit_finish 3) || rc=$?
check "unset: return status" "$rc" 0
check "unset: files written" "$(find "$unset_dir" -mindepth 1 | wc -l | tr -d ' ')" 0

# A finished run: the file is named for L3_GROUP.
rep="$tmp/rep"
(TEST_REPORT_DIR=$rep L3_GROUP=spindown L3_SUMMARY_DONE=1 l3_junit_finish 0)
mapfile -t got_lines < <(xml_summary "$rep/l3-spindown.xml" 2>&1)
check "finish: suite line" "${got_lines[0]:-}" $'testsuite\tl3-spindown\t1\t0\t0\t0.000'
check "finish: case" "${got_lines[1]:-}" $'install\tpassed'

# An aborted run: the steps that passed are not a green run.
(TEST_REPORT_DIR=$rep L3_GROUP=rest l3_junit_finish 7)
mapfile -t got_lines < <(xml_summary "$rep/l3-rest.xml" 2>&1)
check "abort: suite line" "${got_lines[0]:-}" $'testsuite\tl3-rest\t2\t1\t0\t0.000'
check "abort: recorded step kept" "${got_lines[1]:-}" $'install\tpassed'
check "abort: failing case" "${got_lines[2]:-}" $'suite run\tfailed\trun-l3-suite.sh exited with status 7 before its summary\trun-l3-suite.sh exited with status 7 before its summary'

# No group: l3.xml. A group that is not a plain name writes nothing.
(TEST_REPORT_DIR=$rep L3_SUMMARY_DONE=1 l3_junit_finish 0)
check "no group: file" "$([[ -f $rep/l3.xml ]] && echo yes || echo no)" yes
before=$(find "$rep" -type f | wc -l | tr -d ' ')
rc=0
(TEST_REPORT_DIR=$rep L3_GROUP='../x' L3_SUMMARY_DONE=1 l3_junit_finish 0) 2>/dev/null || rc=$?
check "bad group: return status" "$rc" 0
check "bad group: files written" "$(find "$rep" -type f | wc -l | tr -d ' ')" "$before"

# An unwritable report directory is reported and does not change the status.
blocked="$tmp/blocked"
: >"$blocked"
rc=0
(TEST_REPORT_DIR=$blocked/dir L3_SUMMARY_DONE=1 l3_junit_finish 0) 2>"$tmp/stderr" || rc=$?
check "unwritable: return status" "$rc" 0
check "unwritable: reported" "$(grep -c 'could not write' "$tmp/stderr")" 1

# make vm-suite-plan stops before any step: with TEST_REPORT_DIR set it must
# still write no report.
plan_dir="$tmp/plan"
rc=0
HOSERVA_VM_REPO_ROOT="$tmp/repo" HOSERVA_LAB_ID=test-l3-junit L3_PLAN=1 L3_STEPS=array-stop-start \
  TEST_REPORT_DIR=$plan_dir timeout 30 "$script_dir/run-l3-suite.sh" >/dev/null 2>&1 || rc=$?
check "plan: exit status" "$rc" 0
check "plan: report written" "$([[ -e $plan_dir ]] && echo yes || echo no)" no

exit "$fail"
