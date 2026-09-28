#!/usr/bin/env bash
# Contract tests for scripts/epic-status.sh (issue #411): a failed
# sub-issue read must never be indistinguishable from an epic with no
# sub-issues. All against a fake `gh` on PATH, never the live API.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
EPIC_STATUS="$script_dir/epic-status.sh"

fail=0
note() { printf 'test-epic-status: %s\n' "$*" >&2; }
assert_eq() {
  if [ "$1" != "$2" ]; then
    note "FAIL: expected '$2', got '$1' ($3)"
    fail=1
  fi
}
assert_contains() {
  if ! grep -qF -- "$2" "$1"; then
    note "FAIL: log does not contain '$2' ($3)"
    fail=1
  fi
}
assert_not_contains() {
  if grep -qF -- "$2" "$1"; then
    note "FAIL: log unexpectedly contains '$2' ($3)"
    fail=1
  fi
}

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

export GH_REPO="test-owner/test-repo"
REPO="$GH_REPO"
LOG="$work/gh.log"
: >"$LOG"
export GH_REST_TEST_LOG="$LOG"
export ISSUE_STATUS_RETRIES=1

mock_bin="$work/mock-bin"
mkdir -p "$mock_bin"
cat >"$mock_bin/gh" <<MOCK_EOF
#!/usr/bin/env bash
set -euo pipefail
log="\${GH_REST_TEST_LOG:?}"
printf '%s\n' "\$*" >>"\$log"

repo="$REPO"
sub="\${1:-}"; shift || true
[ "\$sub" = "api" ] || { echo "mock gh: unexpected invocation \$sub \$*" >&2; exit 1; }

method="GET"
path=""
jqf=""
while [ \$# -gt 0 ]; do
  case "\$1" in
    --method) method="\$2"; shift 2 ;;
    -f|-F) shift 2 ;;
    --jq) jqf="\$2"; shift 2 ;;
    --*) shift ;;
    *) path="\$1"; shift ;;
  esac
done

hundred_closed() {
  # 100 already-closed sub-issues, so a failure on page 2 is the only
  # thing standing between this epic and "implemented".
  jq -cn '[range(100) | {number: (900000 + .), state: "closed", labels: []}]'
}

body=""
case "\$method \$path" in
  "GET repos/\$repo/issues/700/sub_issues?per_page=100&page=1")
    body='[]' ;;
  "GET repos/\$repo/issues/701/sub_issues?per_page=100&page=1")
    body='[{"number":1,"state":"open","labels":[{"name":"status:ready"}]},{"number":2,"state":"open","labels":[{"name":"status:new"}]}]' ;;
  "GET repos/\$repo/issues/702/sub_issues?per_page=100&page=1")
    body='[{"number":1,"state":"closed","labels":[{"name":"status:implemented"}]},{"number":2,"state":"open","labels":[{"name":"status:in-review"}]}]' ;;
  "GET repos/\$repo/issues/702")
    body='{"labels":[{"name":"epic"},{"name":"status:new"}]}' ;;
  "PATCH repos/\$repo/issues/702")
    body='{"labels":[{"name":"epic"},{"name":"status:in-progress"}]}' ;;
  "GET repos/\$repo/issues/703/sub_issues?per_page=100&page=1")
    body='[{"number":1,"state":"closed","labels":[]},{"number":2,"state":"open","labels":[{"name":"status:implemented"}]}]' ;;
  "GET repos/\$repo/issues/703")
    body='{"labels":[{"name":"epic"},{"name":"status:in-progress"}]}' ;;
  "PATCH repos/\$repo/issues/703")
    body='{"labels":[{"name":"epic"},{"name":"status:implemented"}]}' ;;
  "GET repos/\$repo/issues/704/sub_issues?per_page=100&page=1")
    echo "gh: Internal Server Error (HTTP 500)" >&2; exit 1 ;;
  "GET repos/\$repo/issues/705/sub_issues?per_page=100&page=1")
    body="\$(hundred_closed)" ;;
  "GET repos/\$repo/issues/705/sub_issues?per_page=100&page=2")
    echo "gh: Internal Server Error (HTTP 500)" >&2; exit 1 ;;
  *)
    echo "mock gh: unexpected invocation: \$method \$path" >&2
    exit 1
    ;;
esac

if [ -n "\$jqf" ]; then
  printf '%s' "\$body" | jq -r "\$jqf"
else
  printf '%s\n' "\$body"
fi
MOCK_EOF
chmod +x "$mock_bin/gh"

PATH="$mock_bin:$PATH"
export PATH

# --- a genuinely empty epic still reports "nothing to roll up" -----------
: >"$LOG"
out="$("$EPIC_STATUS" 700)"
assert_eq "$out" "#700 has no sub-issues — nothing to roll up" "an epic with no sub-issues"
assert_not_contains "$LOG" "PATCH" "an epic with no sub-issues writes nothing"

# --- nothing started yet: leave the epic alone ----------------------------
: >"$LOG"
out="$("$EPIC_STATUS" 701)"
assert_eq "$out" "#701: no sub-issue started yet — leaving its status alone" "nothing started"
assert_not_contains "$LOG" "PATCH" "an epic with nothing started writes nothing"

# --- one done, one active: rolls up to in-progress ------------------------
: >"$LOG"
out="$("$EPIC_STATUS" 702)"
assert_eq "$out" "#702 status:in-progress" "a mixed epic rolls up to in-progress"
assert_contains "$LOG" "PATCH repos/$REPO/issues/702" "the rollup writes the epic's status"

# --- every sub-issue done: rolls up to implemented ------------------------
: >"$LOG"
out="$("$EPIC_STATUS" 703)"
assert_eq "$out" "#703 status:implemented" "a fully-done epic rolls up to implemented"
assert_contains "$LOG" "PATCH repos/$REPO/issues/703" "the rollup writes the epic's status"

# --- a failed sub-issue read refuses to roll up, and is not silent -------
: >"$LOG"
set +e
"$EPIC_STATUS" 704 >"$work/704.out" 2>"$work/704.err"
rc=$?
set -e
if [ "$rc" -eq 0 ]; then
  note "FAIL: epic-status.sh must fail when the sub-issue read fails"
  fail=1
fi
if [ -s "$work/704.out" ]; then
  note "FAIL: a failed read must print nothing to stdout ($(cat "$work/704.out"))"
  fail=1
fi
if ! grep -q 'refusing to roll up' "$work/704.err"; then
  note "FAIL: the failure should say it is refusing to roll up ($(cat "$work/704.err"))"
  fail=1
fi
assert_not_contains "$LOG" "PATCH" "a failed sub-issue read must never write the epic's status"
if grep -qxF "api repos/$REPO/issues/704" "$LOG"; then
  note "FAIL: a failed sub-issue read must never even reach issue-status.sh's own label read"
  fail=1
fi

# --- a failure on a later page is not swallowed by an earlier success ----
: >"$LOG"
set +e
"$EPIC_STATUS" 705 >"$work/705.out" 2>"$work/705.err"
rc=$?
set -e
if [ "$rc" -eq 0 ]; then
  note "FAIL: epic-status.sh must fail when a later page's read fails"
  fail=1
fi
if [ -s "$work/705.out" ]; then
  note "FAIL: a failed later-page read must print nothing to stdout ($(cat "$work/705.out"))"
  fail=1
fi
if ! grep -q 'refusing to roll up' "$work/705.err"; then
  note "FAIL: the later-page failure should say it is refusing to roll up ($(cat "$work/705.err"))"
  fail=1
fi
assert_not_contains "$LOG" "PATCH" "a failed later-page read must never write the epic's status"

if [ "$fail" -eq 0 ]; then
  note "PASS"
fi
exit "$fail"
