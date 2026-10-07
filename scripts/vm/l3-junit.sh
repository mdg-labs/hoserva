#!/usr/bin/env bash
# Sourced by run-l3-suite.sh (doc 06 §4, §7): turns the suite's recorded
# STEP_NAMES / STEP_RESULTS (and STEP_SECONDS, when present) into one JUnit
# file, so the nightly run's summary lists every L3 step. Sourcing defines
# functions only. scripts/vm/test-l3-junit.sh drives the same functions from
# recorded data, without a VM.
#
# A result is read exactly as record() writes it:
#   PASS                              passed
#   FAIL: <reason>                    failure carrying the reason
#   NOT-YET-IMPLEMENTED: <message>    skipped with the message
#   SKIPPED (not selected)            skipped, "not selected"
# Anything else is a failure: an unrecognised result never counts as a pass.

l3_xml_attr() {
  printf '%s' "$1" | tr '\n\r\t' '   ' | l3_xml_text
}

l3_xml_text() {
  {
    if command -v iconv >/dev/null 2>&1; then iconv -c -f UTF-8 -t UTF-8 2>/dev/null; else cat; fi
  } \
    | LC_ALL=C tr -d '\000-\010\013\014\016-\037\177' \
    | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g' -e 's/"/\&quot;/g'
}

# l3_junit_write <suite> <file>: writes the file atomically.
l3_junit_write() {
  local suite=$1 file=$2
  local i name result secs tests=0 failures=0 skipped=0 total=0 cases=""
  for i in "${!STEP_NAMES[@]}"; do
    name=${STEP_NAMES[$i]}
    result=${STEP_RESULTS[$i]-}
    secs=${STEP_SECONDS[$i]:-0}
    [[ $secs =~ ^[0-9]+$ ]] || secs=0
    tests=$((tests + 1))
    total=$((total + secs))
    cases+="<testcase classname=\"$(l3_xml_attr "$suite")\" name=\"$(l3_xml_attr "$name")\" time=\"$secs.000\">"$'\n'
    case $result in
      PASS) ;;
      FAIL:*)
        failures=$((failures + 1))
        local reason=${result#FAIL:}
        reason=${reason# }
        cases+="<failure message=\"$(l3_xml_attr "$reason")\" type=\"FAIL\">$(printf '%s' "$reason" | l3_xml_text)</failure>"$'\n'
        ;;
      NOT-YET-IMPLEMENTED:*)
        skipped=$((skipped + 1))
        cases+="<skipped message=\"$(l3_xml_attr "$result")\"/>"$'\n'
        ;;
      "SKIPPED (not selected)")
        skipped=$((skipped + 1))
        cases+="<skipped message=\"not selected\"/>"$'\n'
        ;;
      *)
        failures=$((failures + 1))
        cases+="<failure message=\"$(l3_xml_attr "unrecognised result: $result")\" type=\"unrecognised\"/>"$'\n'
        ;;
    esac
    cases+="</testcase>"$'\n'
  done

  mkdir -p -- "$(dirname -- "$file")" || return 1
  {
    printf '<?xml version="1.0" encoding="UTF-8"?>\n'
    printf '<testsuite name="%s" tests="%s" failures="%s" errors="0" skipped="%s" time="%s.000">\n' \
      "$(l3_xml_attr "$suite")" "$tests" "$failures" "$skipped" "$total"
    printf '%s' "$cases"
    printf '</testsuite>\n'
  } >"$file.tmp.$$" && mv -f -- "$file.tmp.$$" "$file"
}

# l3_junit_finish <exit-status>: the EXIT hook of run-l3-suite.sh. Does
# nothing with TEST_REPORT_DIR unset. A run that ended before it reached its
# summary (L3_SUMMARY_DONE unset) gets a failing "suite run" case, so steps
# that passed before an abort never read as a complete, green run. A failure to
# write is reported on stderr and never changes the exit status: reporting is
# not a gate.
l3_junit_finish() {
  local rc=$1
  [[ -n ${TEST_REPORT_DIR:-} ]] || return 0
  if [[ -z ${L3_SUMMARY_DONE:-} ]]; then
    STEP_NAMES+=("suite run")
    STEP_RESULTS+=("FAIL: run-l3-suite.sh exited with status $rc before its summary")
  fi
  local group=${L3_GROUP:-}
  local suite=l3
  if [[ -n $group ]]; then
    if [[ ! $group =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]]; then
      printf 'l3-junit: invalid L3_GROUP "%s"; no report written\n' "$group" >&2
      return 0
    fi
    suite=l3-$group
  fi
  l3_junit_write "$suite" "$TEST_REPORT_DIR/$suite.xml" \
    || printf 'l3-junit: could not write %s/%s.xml\n' "$TEST_REPORT_DIR" "$suite" >&2
  return 0
}
