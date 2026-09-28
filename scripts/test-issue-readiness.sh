#!/usr/bin/env bash
# Contract tests for scripts/issue-readiness.sh (issue #410): its read now
# goes over REST via scripts/gh-rest.sh, but the verdicts it produces must
# be unchanged for a ready issue, an issue missing '## Acceptance
# criteria', a feat missing 'Reachable via', and an epic — against a fake
# `gh` on PATH, never the live API.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
READINESS="$script_dir/issue-readiness.sh"

fail=0
note() { printf 'test-issue-readiness: %s\n' "$*" >&2; }
assert_contains() {
  if ! grep -qF -- "$2" "$1"; then
    note "FAIL: output does not contain '$2' ($3): $(cat "$1")"
    fail=1
  fi
}

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

export GH_REPO="test-owner/test-repo"
REPO="$GH_REPO"

mock_bin="$work/mock-bin"
mkdir -p "$mock_bin"

# shellcheck disable=SC2016 # the backticks below are literal Markdown, not a substitution
ready_body='## Original report
Something broke.

## Acceptance criteria
- [ ] Fixes it.
- [ ] Reachable via: `cmd/hoservad/main.go` -> the fix applies at startup.

## Out of scope
- Nothing else.
'
no_criteria_body='## Original report
Something broke.

## Out of scope
- Nothing else.
'
no_reachable_body='## Original report
Something broke.

## Acceptance criteria
- [ ] Fixes it, somehow.

## Out of scope
- Nothing else.
'

# jq -Rs slurps stdin as one raw string, so each fixture body (its own
# variable above) becomes valid JSON without hand-escaping newlines/quotes.
ready_json="$(jq -Rs --arg n 601 '{number: ($n|tonumber), title: "Ready issue", body: ., labels: [{name: "feat"}], state: "open"}' <<<"$ready_body")"
no_criteria_json="$(jq -Rs --arg n 602 '{number: ($n|tonumber), title: "Thin issue", body: ., labels: [{name: "feat"}], state: "open"}' <<<"$no_criteria_body")"
no_reachable_json="$(jq -Rs --arg n 603 '{number: ($n|tonumber), title: "No wiring", body: ., labels: [{name: "feat"}], state: "open"}' <<<"$no_reachable_body")"
epic_json='{"number":604,"title":"An epic","body":"## Original report\nSeed phase N.\n","labels":[{"name":"epic"}],"state":"open"}'

cat >"$mock_bin/gh" <<MOCK_EOF
#!/usr/bin/env bash
set -euo pipefail
repo="$REPO"
sub="\${1:-}"; shift || true
[ "\$sub" = "api" ] || { echo "mock gh: unexpected invocation \$sub \$*" >&2; exit 1; }
path=""
while [ \$# -gt 0 ]; do
  case "\$1" in
    -f|-F|--jq) shift 2 ;;
    --*) shift ;;
    *) path="\$1"; shift ;;
  esac
done
case "\$path" in
  "repos/\$repo/issues/601") cat <<'JSON'
$ready_json
JSON
    ;;
  "repos/\$repo/issues/602") cat <<'JSON'
$no_criteria_json
JSON
    ;;
  "repos/\$repo/issues/603") cat <<'JSON'
$no_reachable_json
JSON
    ;;
  "repos/\$repo/issues/604") cat <<'JSON'
$epic_json
JSON
    ;;
  *)
    echo "mock gh: unexpected invocation: \$path" >&2
    exit 1
    ;;
esac
MOCK_EOF
chmod +x "$mock_bin/gh"

PATH="$mock_bin:$PATH"
export PATH

# --- a ready issue ----------------------------------------------------
out="$work/601.out"
"$READINESS" 601 >"$out"; rc=$?
assert_contains "$out" "READY" "a fully-formed feat issue is READY"
if [ "$rc" -ne 0 ]; then
  note "FAIL: a ready issue must exit 0 (got $rc)"
  fail=1
fi
if grep -q NOT-READY "$out"; then
  note "FAIL: a ready issue must not read NOT-READY: $(cat "$out")"
  fail=1
fi

# --- missing '## Acceptance criteria' ----------------------------------
out="$work/602.out"
set +e
"$READINESS" 602 >"$out"; rc=$?
set -e
assert_contains "$out" "NOT-READY" "an issue with no Acceptance criteria section is NOT-READY"
assert_contains "$out" "no '## Acceptance criteria'" "the reason names the missing section"
if [ "$rc" -ne 1 ]; then
  note "FAIL: NOT-READY must exit 1 (got $rc)"
  fail=1
fi

# --- a feat missing 'Reachable via' -------------------------------------
out="$work/603.out"
set +e
"$READINESS" 603 >"$out"; rc=$?
set -e
assert_contains "$out" "NOT-READY" "a feat with no Reachable via criterion is NOT-READY"
assert_contains "$out" "Reachable via" "the reason names the missing wiring criterion"

# --- an epic is never dispatched itself, and is always READY -----------
out="$work/604.out"
"$READINESS" 604 >"$out"; rc=$?
assert_contains "$out" "READY" "an epic is READY"
assert_contains "$out" "epic: not dispatched itself" "an epic's reason names itself as an epic"
if [ "$rc" -ne 0 ]; then
  note "FAIL: an epic must exit 0 (got $rc)"
  fail=1
fi

if [ "$fail" -eq 0 ]; then
  note "PASS"
fi
exit "$fail"
