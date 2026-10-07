#!/usr/bin/env bash
# Run one command and record it as a JUnit test case (doc 06 §7).
#
#   junit-step.sh <suite> <name> -- <command> [args...]
#
# With TEST_REPORT_DIR unset or empty this only execs the command: nothing is
# written and nothing about the run changes. With it set, the command runs as
# an argv (never through a shell), its output is passed through unchanged
# apart from stderr being merged into stdout, its exit status is this
# script's exit status, and one <testcase> is appended to
# $TEST_REPORT_DIR/<suite>.xml — a pass, or a failure carrying the exit
# status and the tail of the output. A failure to write the report is
# reported on stderr and never changes the exit status: reporting is not a
# gate.
set -uo pipefail

die() { printf 'junit-step: %s\n' "$*" >&2; exit 2; }

if (($# < 4)) || [[ $3 != -- ]]; then
  die "usage: junit-step.sh <suite> <name> -- <command> [args...]"
fi
suite=$1
name=$2
shift 3

if [[ -z ${TEST_REPORT_DIR:-} ]]; then
  exec "$@"
fi

[[ $suite =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] \
  || die "invalid suite '$suite': letters, digits, '.', '_' and '-' only, starting with a letter or digit"

xml_attr() {
  printf '%s' "$1" | tr '\n\r\t' '   ' | xml_text
}

xml_text() {
  {
    if command -v iconv >/dev/null 2>&1; then iconv -c -f UTF-8 -t UTF-8 2>/dev/null; else cat; fi
  } \
    | LC_ALL=C tr -d '\000-\010\013\014\016-\037\177' \
    | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g' -e 's/"/\&quot;/g'
}

record() {
  local rc=$1 started=$2 finished=$3 out=$4
  local dir=$TEST_REPORT_DIR cases_dir="$TEST_REPORT_DIR/.junit"
  local cases="$cases_dir/$suite.cases" xml="$TEST_REPORT_DIR/$suite.xml"
  local ms=$(((finished - started) / 1000000))
  local secs
  secs=$(printf '%d.%03d' $((ms / 1000)) $((ms % 1000)))

  mkdir -p -- "$dir" "$cases_dir" || return 1
  {
    printf '<testcase classname="%s" name="%s" time="%s">\n' "$(xml_attr "$suite")" "$(xml_attr "$name")" "$secs"
    if ((rc != 0)); then
      local last
      last=$(grep -a -v '^[[:space:]]*$' -- "$out" | tail -n 1 | cut -c1-300)
      printf '<failure message="%s" type="exit status">' "$(xml_attr "exit status $rc${last:+: $last}")"
      tail -n 60 -- "$out" | tail -c 8000 | xml_text
      printf '</failure>\n'
    fi
    printf '</testcase>\n'
  } >>"$cases" || return 1

  local tests failures total
  tests=$(grep -c '^<testcase ' "$cases" || true)
  failures=$(grep -c '^<failure ' "$cases" || true)
  total=$(awk -F'time="' '/^<testcase /{split($2, a, "\""); s += a[1]} END{printf "%.3f", s}' "$cases")
  {
    printf '<?xml version="1.0" encoding="UTF-8"?>\n'
    printf '<testsuite name="%s" tests="%s" failures="%s" errors="0" skipped="0" time="%s">\n' "$(xml_attr "$suite")" "$tests" "$failures" "$total"
    cat -- "$cases"
    printf '</testsuite>\n'
  } >"$xml.tmp.$$" && mv -f -- "$xml.tmp.$$" "$xml"
}

out=$(mktemp) || die "cannot create a temporary file"
trap 'rm -f -- "$out"' EXIT

started=$(date +%s%N)
"$@" 2>&1 | tee -- "$out"
rc=${PIPESTATUS[0]}
finished=$(date +%s%N)

record "$rc" "$started" "$finished" "$out" \
  || printf 'junit-step: could not write the report for %s / %s under %s\n' "$suite" "$name" "$TEST_REPORT_DIR" >&2

exit "$rc"
