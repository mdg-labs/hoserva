#!/usr/bin/env bash
# Fixture tests for junit-step.sh (doc 06 §7): pass, fail with the exit status
# kept, markup characters in a name and in output, no report when
# TEST_REPORT_DIR is unset, and an argv that is never read as shell. Report
# files are parsed with python3's XML parser, so an invalid document fails.
set -uo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
step="$script_dir/junit-step.sh"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fail=0
note() { printf 'test-junit-step: %s\n' "$*" >&2; }
check() {
  if [[ $2 != "$3" ]]; then
    note "FAIL: $1: got '$2', want '$3'"
    fail=1
  fi
}

# xml_summary <file>: "<tests> <failures>" from the root, then one line per
# testcase: name, whether it failed, the failure message and the failure body.
xml_summary() {
  python3 -I - "$1" <<'PY'
import sys
import xml.etree.ElementTree as ET

root = ET.parse(sys.argv[1]).getroot()
print(root.get("tests"), root.get("failures"))
for case in root.iter("testcase"):
    failure = case.find("failure")
    if failure is None:
        print(case.get("name"), "pass", sep="\t")
    else:
        print(case.get("name"), "fail", failure.get("message"), (failure.text or "").replace("\n", "\\n"), sep="\t")
PY
}

run_step() {
  local rc=0
  "$step" "$@" >"$tmp/stdout" 2>"$tmp/stderr" || rc=$?
  return "$rc"
}

# TEST_REPORT_DIR unset: the command runs as it would without the wrapper and
# no file appears anywhere in the working directory.
unset_dir="$tmp/unset"
mkdir -p "$unset_dir"
rc=0
(cd "$unset_dir" && env -u TEST_REPORT_DIR "$step" suite name -- bash -c 'echo plain-out; echo plain-err >&2; exit 4') >"$tmp/stdout" 2>"$tmp/stderr" || rc=$?
check "unset: exit status" "$rc" 4
check "unset: stdout" "$(cat "$tmp/stdout")" plain-out
check "unset: stderr kept apart" "$(cat "$tmp/stderr")" plain-err
check "unset: files written" "$(find "$unset_dir" -mindepth 1 | wc -l | tr -d ' ')" 0

rc=0
(cd "$unset_dir" && TEST_REPORT_DIR='' "$step" suite name -- true) >/dev/null 2>&1 || rc=$?
check "empty: exit status" "$rc" 0
check "empty: files written" "$(find "$unset_dir" -mindepth 1 | wc -l | tr -d ' ')" 0

# pass
report="$tmp/report"
rc=0
TEST_REPORT_DIR="$report" run_step pass-suite 'scripts/ok.sh' -- bash -c 'echo hello' || rc=$?
check "pass: exit status" "$rc" 0
check "pass: output passed through" "$(cat "$tmp/stdout")" hello
check "pass: report" "$(xml_summary "$report/pass-suite.xml" 2>&1 | sed 's/\t/|/g')" "1 0
scripts/ok.sh|pass"

# fail: the exit status survives, and the report carries it with the output
rc=0
TEST_REPORT_DIR="$report" run_step fail-suite 'scripts/bad.sh' -- bash -c 'echo line-one; echo line-two >&2; exit 7' || rc=$?
check "fail: exit status" "$rc" 7
check "fail: output passed through" "$(cat "$tmp/stdout")" "line-one
line-two"
check "fail: report" "$(xml_summary "$report/fail-suite.xml" 2>&1 | sed 's/\t/|/g')" "1 1
scripts/bad.sh|fail|exit status 7: line-two|line-one\\nline-two\\n"

# a second case lands in the same suite file, and the totals follow
rc=0
TEST_REPORT_DIR="$report" run_step fail-suite 'scripts/ok.sh' -- true || rc=$?
check "append: exit status" "$rc" 0
check "append: totals" "$(xml_summary "$report/fail-suite.xml" 2>&1 | head -n 1)" "2 1"

# a command that does not exist is a failure with the shell's 127
rc=0
TEST_REPORT_DIR="$report" run_step missing-suite 'no-such' -- "$tmp/no-such-command" || rc=$?
check "missing: exit status" "$rc" 127
check "missing: totals" "$(xml_summary "$report/missing-suite.xml" 2>&1 | head -n 1)" "1 1"

# markup in a name and in output is escaped, and survives a round trip
tricky_name='a <b> & "c" d'
rc=0
TEST_REPORT_DIR="$report" run_step markup 'a <b> & "c" d' -- bash -c 'echo "<tag attr=\"v\"> & done"; exit 1' || rc=$?
check "markup: exit status" "$rc" 1
check "markup: report" "$(xml_summary "$report/markup.xml" 2>&1 | sed 's/\t/|/g')" "1 1
$tricky_name|fail|exit status 1: <tag attr=\"v\"> & done|<tag attr=\"v\"> & done\\n"
check "markup: no raw markup in the file" "$(grep -c '<tag' "$report/markup.xml" || true)" 0

# control characters that XML 1.0 forbids do not make the report invalid
rc=0
TEST_REPORT_DIR="$report" run_step control 'ctl' -- bash -c 'printf "red\033[31m\001 end\n"; exit 1' || rc=$?
check "control: exit status" "$rc" 1
check "control: report parses" "$(xml_summary "$report/control.xml" 2>&1 | head -n 1)" "1 1"

# the command is an argv: shell syntax in an argument reaches it untouched
rc=0
# shellcheck disable=SC2016
(cd "$tmp" && TEST_REPORT_DIR="$report" "$step" argv 'argv' -- printf '%s|' '$(touch pwned); echo x' '`touch pwned2`') >"$tmp/stdout" 2>"$tmp/stderr" || rc=$?
check "argv: exit status" "$rc" 0
# shellcheck disable=SC2016
check "argv: arguments untouched" "$(cat "$tmp/stdout")" '$(touch pwned); echo x|`touch pwned2`|'
check "argv: nothing executed" "$(find "$tmp" -maxdepth 1 -name 'pwned*' | wc -l | tr -d ' ')" 0

# usage and suite-name errors are refused before anything runs
rc=0
TEST_REPORT_DIR="$report" run_step ../escape name -- touch "$tmp/ran" || rc=$?
check "bad suite: exit status" "$rc" 2
check "bad suite: command not run" "$(test -e "$tmp/ran" && echo ran || echo not-run)" not-run
rc=0
run_step suite name touch "$tmp/ran" || rc=$?
check "missing --: exit status" "$rc" 2

# an unwritable report directory is reported and never changes the exit status
touch "$tmp/not-a-dir"
rc=0
TEST_REPORT_DIR="$tmp/not-a-dir/sub" run_step suite name -- bash -c 'echo ran; exit 5' || rc=$?
check "unwritable: exit status" "$rc" 5
check "unwritable: command output" "$(cat "$tmp/stdout")" ran
check "unwritable: reported" "$(grep -c 'could not write the report' "$tmp/stderr" || true)" 1

if ((fail)); then
  exit 1
fi
note "ok"
