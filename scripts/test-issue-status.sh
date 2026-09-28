#!/usr/bin/env bash
# Contract tests for scripts/issue-status.sh (issue #410): its label read
# now goes over REST via scripts/gh-rest.sh, but the guarantees this script
# exists for must survive that move unchanged — a failed label read still
# refuses to PATCH, and a PATCH response without exactly the new status
# label still fails, either way with no half-applied label set. All against
# a fake `gh` on PATH, never the live API.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ISSUE_STATUS="$script_dir/issue-status.sh"

fail=0
note() { printf 'test-issue-status: %s\n' "$*" >&2; }
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
# Keep the retry loop's own backoff out of a unit test's runtime — one
# attempt is enough to prove the refuse-to-PATCH behaviour.
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

body=""
case "\$method \$path" in
  "GET repos/\$repo/issues/500")
    body='{"labels":[{"name":"area:api"},{"name":"status:ready"}]}' ;;
  "PATCH repos/\$repo/issues/500")
    body='{"labels":[{"name":"area:api"},{"name":"status:in-progress"}]}' ;;
  "GET repos/\$repo/issues/501")
    echo "gh: Internal Server Error (HTTP 500)" >&2
    exit 1 ;;
  "GET repos/\$repo/issues/502")
    body='{"labels":[{"name":"status:ready"}]}' ;;
  "PATCH repos/\$repo/issues/502")
    # Simulates a concurrent writer having raced this PATCH: the response
    # does not carry the status this call asked for.
    body='{"labels":[{"name":"status:ready"}]}' ;;
  *)
    echo "mock gh: unexpected invocation: \$method \$path" >&2
    exit 1
    ;;
esac

# gh api --jq applies the filter server-side (raw output) — issue-status.sh
# relies on this for both the label read and the PATCH's own confirmation.
if [ -n "\$jqf" ]; then
  printf '%s' "\$body" | jq -r "\$jqf"
else
  printf '%s\n' "\$body"
fi
MOCK_EOF
chmod +x "$mock_bin/gh"

PATH="$mock_bin:$PATH"
export PATH

# --- happy path: read over REST, PATCH, verified from the PATCH's own
# response, non-status labels preserved -----------------------------------
: >"$LOG"
out="$("$ISSUE_STATUS" 500 in-progress)"
assert_eq "$out" "#500 status:in-progress" "the happy path reports the new status"
assert_contains "$LOG" "api repos/$REPO/issues/500" "the label read goes through gh-rest.sh's issue-view, over REST"
assert_contains "$LOG" "labels[]=status:in-progress" "the PATCH sets the requested status"
assert_contains "$LOG" "labels[]=area:api" "the PATCH preserves the issue's non-status label"

# --- a failed label read refuses to PATCH ---------------------------------
: >"$LOG"
set +e
"$ISSUE_STATUS" 501 in-progress >"$work/501.out" 2>"$work/501.err"
rc=$?
set -e
if [ "$rc" -eq 0 ]; then
  note "FAIL: issue-status.sh must fail when the label read fails"
  fail=1
fi
assert_not_contains "$LOG" "PATCH" "a failed label read must never reach the PATCH"
if ! grep -q 'refusing to PATCH' "$work/501.err"; then
  note "FAIL: the failure should say it is refusing to PATCH ($(cat "$work/501.err"))"
  fail=1
fi

# --- a PATCH response without exactly the new status still fails ---------
: >"$LOG"
set +e
"$ISSUE_STATUS" 502 in-progress >"$work/502.out" 2>"$work/502.err"
rc=$?
set -e
if [ "$rc" -eq 0 ]; then
  note "FAIL: issue-status.sh must fail when the PATCH response lacks the new status"
  fail=1
fi
assert_contains "$LOG" "api repos/$REPO/issues/502" "the read still happens"
assert_contains "$LOG" "PATCH repos/$REPO/issues/502" "the PATCH is still attempted"
if ! grep -q "expected exactly 'status:in-progress'" "$work/502.err"; then
  note "FAIL: the failure should name the expected status ($(cat "$work/502.err"))"
  fail=1
fi

if [ "$fail" -eq 0 ]; then
  note "PASS"
fi
exit "$fail"
