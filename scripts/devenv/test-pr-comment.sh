#!/usr/bin/env bash
# Fixture tests for pr-comment.sh (doc 06 §7), the decision logic of the PR
# test-summary comment, against a stub `gh` that serves canned REST responses and
# keeps the PR's comments in a file. They cover: a cancelled run and a run from a
# push leave the comment alone; a PR number that is not a positive integer, an
# artifact that is too large or has no regular files, and a PR whose head moved
# on are skipped before any write; the first run creates one comment and the next
# runs edit it in place (exactly one marker comment); a marker comment by another
# author, or a bot comment without the marker, is never edited; a failed CI run
# updates the comment; an API error fails the script; and artifact content is
# never executed.
set -uo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fail=0
note() { printf 'test-pr-comment: %s\n' "$*" >&2; }
check() {
  if [[ $2 != "$3" ]]; then
    note "FAIL: $1: got '$2', want '$3'"
    fail=1
  fi
}

marker='<!-- hoserva-test-report -->'
sha=0123456789abcdef0123456789abcdef01234567
other_sha=fedcba9876543210fedcba9876543210fedcba98

mkdir -p "$tmp/bin"
cat >"$tmp/bin/gh" <<'STUB'
#!/usr/bin/env bash
# Stub gh: state lives in $STUB_DIR.
all="$*"
printf '%s\n' "$all" >>"$STUB_DIR/calls.log"
case "$all" in
  "api repos/o/r/actions/runs/"*"/artifacts?"*)
    cat "$STUB_DIR/artifacts.json" ;;
  "api repos/o/r/pulls/"*" --jq .head.sha")
    [[ -z ${STUB_PULLS_FAIL:-} ]] || { echo "HTTP 502" >&2; exit 1; }
    cat "$STUB_DIR/head_sha" ;;
  "run download "*)
    dir=${all##* --dir }
    mkdir -p "$dir"
    cp -a "$STUB_DIR/artifact/." "$dir/" ;;
  "api repos/o/r/issues/"*"/comments?per_page=100&page=1")
    cat "$STUB_DIR/comments.json" ;;
  "api repos/o/r/issues/"*"/comments?per_page=100&page="*)
    echo '[]' ;;
  "api --method PATCH repos/o/r/issues/comments/"*" -F body=@"*)
    id=${4##*/}
    body=${all##* -F body=@}
    jq --argjson id "$id" --rawfile b "$body" 'map(if .id == $id then .body = $b else . end)' "$STUB_DIR/comments.json" >"$STUB_DIR/c.tmp" && mv "$STUB_DIR/c.tmp" "$STUB_DIR/comments.json" ;;
  "api --method POST repos/o/r/issues/"*"/comments -F body=@"*)
    body=${all##* -F body=@}
    jq --rawfile b "$body" '. + [{id: (100 + length), user: {login: "github-actions[bot]", type: "Bot"}, body: $b}]' "$STUB_DIR/comments.json" >"$STUB_DIR/c.tmp" && mv "$STUB_DIR/c.tmp" "$STUB_DIR/comments.json" ;;
  *)
    echo "stub gh: unexpected call: $*" >&2
    exit 99 ;;
esac
STUB
chmod +x "$tmp/bin/gh"

# new_case <name> [pr-json]: a fresh stub state with a valid artifact.
new_case() {
  STUB_DIR="$tmp/$1"
  export STUB_DIR
  rm -rf "$STUB_DIR"
  mkdir -p "$STUB_DIR/artifact"
  : >"$STUB_DIR/calls.log"
  printf '%s\n' "$sha" >"$STUB_DIR/head_sha"
  echo '[]' >"$STUB_DIR/comments.json"
  printf '## Test report\n\n**3 tests: 3 passed, 0 failed, 0 skipped**\n\nfailure by @octocat\n' >"$STUB_DIR/artifact/test-summary.md"
  printf '{"number": 7, "head_sha": "%s"}\n' "$sha" >"$STUB_DIR/artifact/pr.json"
  echo '{"artifacts":[{"name":"test-summary","expired":false,"size_in_bytes":2048}]}' >"$STUB_DIR/artifacts.json"
  unset STUB_PULLS_FAIL
}

# run_script: the workflow's environment, with the stub on PATH.
run_script() {
  PATH="$tmp/bin:$PATH" REPO=o/r RUN_ID=555 RUN_CONCLUSION="${RUN_CONCLUSION:-success}" RUN_EVENT="${RUN_EVENT:-pull_request}" \
    RUN_HEAD_SHA="$sha" RUN_URL=https://github.com/o/r/actions/runs/555 RUN_TIME=2026-10-07T05:54:58Z \
    GH_TOKEN=unused timeout 120 "$script_dir/pr-comment.sh" >"$STUB_DIR/stdout" 2>"$STUB_DIR/stderr"
}

writes() { grep -c -- '--method \(PATCH\|POST\)' "$STUB_DIR/calls.log" || true; }
marker_comments() { jq --arg m "$marker" '[.[] | select(.body | contains($m))] | length' "$STUB_DIR/comments.json"; }

# A cancelled run and a push run: skipped, exit 0, no API call at all.
new_case cancelled
RUN_CONCLUSION=cancelled run_script; rc=$?
check "cancelled: exit" "$rc" 0
check "cancelled: calls" "$(wc -l <"$STUB_DIR/calls.log" | tr -d ' ')" 0
check "cancelled: reason logged" "$(grep -c 'skipped: .*cancelled' "$STUB_DIR/stdout")" 1

new_case push
RUN_EVENT=push run_script; rc=$?
check "push: exit" "$rc" 0
check "push: calls" "$(wc -l <"$STUB_DIR/calls.log" | tr -d ' ')" 0
check "push: reason logged" "$(grep -c "skipped: .*'push'" "$STUB_DIR/stdout")" 1
unset RUN_EVENT

# PR numbers that are not positive integers never reach the API.
n=0
for bad in '0' '-3' '1.5' '"7"' '"7; touch pwned"' 'null' '[7]' '1e9' '12345678901234' '{"a":1}' ; do
  n=$((n + 1))
  new_case "badnum$n"
  printf '{"number": %s}\n' "$bad" >"$STUB_DIR/artifact/pr.json"
  run_script; rc=$?
  check "number $bad: exit" "$rc" 0
  check "number $bad: skipped" "$(grep -c 'skipped:' "$STUB_DIR/stdout")" 1
  check "number $bad: no pulls lookup or write" "$(grep -c -E 'pulls/|--method' "$STUB_DIR/calls.log" || true)" 0
done
new_case notjson
echo 'not json {' >"$STUB_DIR/artifact/pr.json"
run_script; rc=$?
check "pr.json garbage: exit" "$rc" 0
check "pr.json garbage: skipped" "$(grep -c 'skipped:' "$STUB_DIR/stdout")" 1
new_case nopr
rm "$STUB_DIR/artifact/pr.json"
run_script; rc=$?
check "no pr.json: exit" "$rc" 0
check "no pr.json: skipped" "$(grep -c 'skipped:' "$STUB_DIR/stdout")" 1
new_case symlink
rm "$STUB_DIR/artifact/pr.json"
ln -s /etc/hostname "$STUB_DIR/artifact/pr.json"
run_script; rc=$?
check "symlinked pr.json: skipped" "$(grep -c 'skipped:' "$STUB_DIR/stdout")" 1
check "symlinked pr.json: writes" "$(writes)" 0

# Artifact guards.
new_case big
echo '{"artifacts":[{"name":"test-summary","expired":false,"size_in_bytes":99999999}]}' >"$STUB_DIR/artifacts.json"
run_script; rc=$?
check "oversized artifact: exit" "$rc" 0
check "oversized artifact: not downloaded" "$(grep -c '^run download' "$STUB_DIR/calls.log" || true)" 0
new_case expired
echo '{"artifacts":[{"name":"test-summary","expired":true,"size_in_bytes":10}]}' >"$STUB_DIR/artifacts.json"
run_script; rc=$?
check "expired artifact: skipped" "$(grep -c 'skipped:' "$STUB_DIR/stdout")" 1

# A superseded run: the PR's head is another commit.
new_case superseded
printf '%s\n' "$other_sha" >"$STUB_DIR/head_sha"
run_script; rc=$?
check "superseded: exit" "$rc" 0
check "superseded: writes" "$(writes)" 0
check "superseded: comments not read" "$(grep -c 'issues/7/comments' "$STUB_DIR/calls.log" || true)" 0
check "superseded: reason logged" "$(grep -c 'skipped: PR #7 is now at fedcba9' "$STUB_DIR/stdout")" 1

# The first run creates the comment, the following runs edit it in place.
new_case once
run_script; rc=$?
check "create: exit" "$rc" 0
check "create: one marker comment" "$(marker_comments)" 1
check "create: one POST" "$(grep -c -- '--method POST' "$STUB_DIR/calls.log")" 1
check "create: mention neutralized" "$(jq -r '.[0].body' "$STUB_DIR/comments.json" | grep -c '@octocat')" 0
RUN_CONCLUSION=failure run_script; rc=$?
check "failed run updates: exit" "$rc" 0
run_script; rc=$?
check "third run: exit" "$rc" 0
check "three runs: still one comment" "$(jq length "$STUB_DIR/comments.json")" 1
check "three runs: one POST, two PATCH" "$(grep -c -- '--method POST' "$STUB_DIR/calls.log"):$(grep -c -- '--method PATCH' "$STUB_DIR/calls.log")" 1:2

# Only the bot's own marker comment is edited.
new_case others
jq -n --arg m "$marker" '[
  {id: 11, user: {login: "someone", type: "User"}, body: ("forged " + $m)},
  {id: 12, user: {login: "github-actions[bot]", type: "Bot"}, body: "a bot comment without the marker"},
  {id: 13, user: {login: "github-actions[bot]", type: "Bot"}, body: ("old " + $m)}
]' >"$STUB_DIR/comments.json"
run_script; rc=$?
check "others: exit" "$rc" 0
check "others: one write, a PATCH of comment 13" "$(grep -c -- '--method' "$STUB_DIR/calls.log"):$(grep -c -- '--method PATCH repos/o/r/issues/comments/13 ' "$STUB_DIR/calls.log")" 1:1
check "others: forged comment untouched" "$(jq -r '.[0].body' "$STUB_DIR/comments.json")" "forged $marker"
check "others: no second comment" "$(jq length "$STUB_DIR/comments.json")" 3

# Artifact content is data: a summary with shell syntax runs nothing.
new_case inject
{
  printf '## Test report\n\n'
  # shellcheck disable=SC2016
  printf '$(touch %s/pwned) `touch %s/pwned2` ; touch %s/pwned3\n' "$tmp" "$tmp" "$tmp"
} >"$STUB_DIR/artifact/test-summary.md"
run_script; rc=$?
check "inject: exit" "$rc" 0
check "inject: nothing executed" "$(find "$tmp" -maxdepth 1 -name 'pwned*' | wc -l | tr -d ' ')" 0
check "inject: text kept as data" "$(jq -r '.[0].body' "$STUB_DIR/comments.json" | grep -c 'touch')" 1

# An API error is not a skip.
new_case apierr
STUB_PULLS_FAIL=1 run_script; rc=$?
check "api error: fails" "$([[ $rc -ne 0 ]] && echo failed || echo passed)" failed
check "api error: no write" "$(writes)" 0

exit "$fail"
