#!/usr/bin/env bash
# Contract tests for scripts/gh-rest.sh (issue #410) against a fake `gh` on
# PATH — the same pattern as scripts/release/test-pages-site.sh — so this
# never reaches the live GitHub API. Asserts the exact `gh api` argv each
# subcommand issues (repository-scoped REST, self-paged, never
# `--paginate`, never `graphql`, never `search/`) and its output.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
GH_REST="$script_dir/gh-rest.sh"

fail=0
note() { printf 'test-gh-rest: %s\n' "$*" >&2; }
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

mock_bin="$work/mock-bin"
mkdir -p "$mock_bin"

# --- fake gh -------------------------------------------------------------
#
# Every call scripts/gh-rest.sh makes is `gh api [--method M] <path>
# [-f k=v]... [-F k=v]...` — nothing else. This fixture logs the raw argv
# (space-joined; test values below never need exact token boundaries) and
# answers only the paths this test suite actually calls.
cat >"$mock_bin/gh" <<MOCK_EOF
#!/usr/bin/env bash
set -euo pipefail
log="\${GH_REST_TEST_LOG:?}"
printf '%s\n' "\$*" >>"\$log"

repo="$REPO"
sub="\${1:-}"; shift || true
if [ "\$sub" != "api" ]; then
  echo "mock gh: unexpected non-api invocation: \$sub \$*" >&2
  exit 1
fi

method="GET"
path=""
jqf=""
while [ \$# -gt 0 ]; do
  case "\$1" in
    --method) method="\$2"; shift 2 ;;
    -f|-F) shift 2 ;;
    --jq) jqf="\$2"; shift 2 ;;
    --input) printf 'body: %s\n' "\$(cat)" >>"\$log"; shift 2 ;;
    --*) shift ;;
    *) path="\$1"; shift ;;
  esac
done

hundred() {
  # \$1 items, numbered starting at \$2, as a JSON array of plain issues.
  jq -cn --argjson n "\$1" --argjson start "\$2" \
    '[range(\$n) | {number: (\$start + .), title: "bulk", body: "bulk"}]'
}

body=""
case "\$method \$path" in
  "GET repos/\$repo/issues/55")
    body='{"id":9001,"number":55,"title":"Sub issue","body":"b","labels":[{"name":"bug"}],"state":"open","html_url":"https://x/55"}' ;;
  "GET repos/\$repo/issues/60")
    body='{"id":9002,"number":60,"title":"Dep issue","body":"b","labels":[],"state":"open","html_url":"https://x/60"}' ;;
  "GET repos/\$repo/issues/62/parent")
    body='{"number":59,"title":"Epic"}' ;;
  "GET repos/\$repo/issues/63/parent")
    echo "gh: Not Found (HTTP 404)" >&2; exit 1 ;;
  "GET repos/\$repo/issues/64/parent")
    echo "gh: Internal Server Error (HTTP 500)" >&2; exit 1 ;;
  "GET repos/\$repo/issues/55/comments?per_page=100&page=1")
    body='[{"user":{"login":"alice"},"created_at":"2026-01-01T00:00:00Z","body":"first comment"}]' ;;
  "GET repos/\$repo/issues?state=open&per_page=100&page=1")
    body='[{"number":1,"title":"Config backup safety","body":"nothing special"},{"number":2,"title":"Random","body":"This mentions CONFIG and Backup separately"},{"number":3,"title":"Config backup PR","body":"pr body","pull_request":{"url":"x"}}]' ;;
  "GET repos/\$repo/issues?state=all&labels=bug&per_page=100&page=1")
    body='[{"number":1,"title":"Config backup safety","body":"nothing special","labels":[{"name":"bug"}]}]' ;;
  "GET repos/\$repo/issues?state=open&labels=threepage&per_page=100&page=1")
    body="\$(hundred 100 100000)" ;;
  "GET repos/\$repo/issues?state=open&labels=threepage&per_page=100&page=2")
    body="\$(hundred 100 100100)" ;;
  "GET repos/\$repo/issues?state=open&labels=threepage&per_page=100&page=3")
    body="\$(hundred 42 100200)" ;;
  "GET repos/\$repo/issues?state=open&labels=exact100&per_page=100&page=1")
    body="\$(hundred 100 200000)" ;;
  "GET repos/\$repo/issues?state=open&labels=exact100&per_page=100&page=2")
    body='[]' ;;
  "GET repos/\$repo/milestones?state=all&per_page=100&page=1")
    body='[{"number":3,"title":"Phase 2"}]' ;;
  "POST repos/\$repo/issues")
    body='{"number":999}' ;;
  "PATCH repos/\$repo/issues/71")
    body='{"number":71}' ;;
  "PATCH repos/\$repo/issues/73")
    body='{"number":73}' ;;
  "PATCH repos/\$repo/issues/74")
    body='{"number":74}' ;;
  "POST repos/\$repo/issues/70/labels")
    body='{}' ;;
  "DELETE repos/\$repo/issues/70/labels/bar")
    body='{}' ;;
  "POST repos/\$repo/issues/72/comments")
    body='{}' ;;
  "POST repos/\$repo/issues/61/sub_issues")
    body='{}' ;;
  "DELETE repos/\$repo/issues/61/sub_issue")
    body='{}' ;;
  "GET repos/\$repo/issues/61/sub_issues?per_page=100&page=1")
    body='[{"number":55,"title":"Sub issue","state":"open"}]' ;;
  "GET repos/\$repo/issues/61/dependencies/blocked_by?per_page=100&page=1")
    body='[{"number":60,"title":"Dep issue","state":"open"}]' ;;
  "GET repos/\$repo/issues/61/dependencies/blocking?per_page=100&page=1")
    body='[{"number":277,"title":"Downstream","state":"open"}]' ;;
  "POST repos/\$repo/issues/61/dependencies/blocked_by")
    body='{}' ;;
  "DELETE repos/\$repo/issues/61/dependencies/blocked_by/9002")
    body='{}' ;;
  "GET repos/\$repo/pulls/80")
    body='{"number":80,"title":"A PR","html_url":"https://x/pulls/80"}' ;;
  "GET repos/\$repo/pulls?state=open&base=main&head=test-owner%3Adev&per_page=100&page=1")
    body='[{"number":81,"title":"Promotion PR","html_url":"https://x/pulls/81"}]' ;;
  "POST repos/\$repo/pulls")
    body='{"number":82}' ;;
  "PATCH repos/\$repo/pulls/80")
    body='{"number":80}' ;;
  "POST repos/\$repo/issues/80/comments")
    body='{}' ;;
  "GET repos/\$repo")
    body='{"full_name":"test-owner/test-repo","default_branch":"main"}' ;;
  "GET repos/\$repo/labels?per_page=100&page=1")
    body='[{"name":"bug"},{"name":"feat"}]' ;;
  "GET repos/\$repo/security-advisories?per_page=100")
    body='[{"ghsa_id":"GHSA-2345-cfgh-jmpq","state":"draft"},{"ghsa_id":"GHSA-2345-cfgh-jmpr","state":"triage"}]' ;;
  "GET repos/\$repo/security-advisories?per_page=100&state=triage")
    body='[{"ghsa_id":"GHSA-2345-cfgh-jmpr","state":"triage"}]' ;;
  "GET repos/\$repo/security-advisories?per_page=100&state=closed")
    body="\$(hundred 100 300000)" ;;
  "GET repos/\$repo/security-advisories/GHSA-2345-cfgh-jmpq")
    body='{"ghsa_id":"GHSA-2345-cfgh-jmpq","severity":"high"}' ;;
  "POST repos/\$repo/security-advisories")
    body='{"ghsa_id":"GHSA-2345-cfgh-jmpq"}' ;;
  "PATCH repos/\$repo/security-advisories/GHSA-2345-cfgh-jmpq")
    body='{"ghsa_id":"GHSA-2345-cfgh-jmpq"}' ;;
  "POST repos/\$repo/security-advisories/GHSA-2345-cfgh-jmpq/forks")
    body='{}' ;;
  "GET repos/\$repo/security-advisories/GHSA-2345-cfgh-jmpq/comments?per_page=100")
    body='[{"id":41,"body":"first note","html_url":"https://x/security/advisories/GHSA-2345-cfgh-jmpq#advisory-comment-41"},{"id":42,"body":"second note","html_url":"https://x/security/advisories/GHSA-2345-cfgh-jmpq#advisory-comment-42"}]' ;;
  "GET repos/\$repo/security-advisories/GHSA-2345-cfgh-jmpr/comments?per_page=100")
    echo "gh: Not Found (HTTP 404)" >&2; exit 1 ;;
  "GET repos/\$repo/security-advisories/GHSA-2345-cfgh-jmpx/comments?per_page=100")
    body="\$(hundred 100 400000)" ;;
  "POST repos/\$repo/security-advisories/GHSA-2345-cfgh-jmpq/comments")
    body='{"id":43,"body":"posted","html_url":"https://x/security/advisories/GHSA-2345-cfgh-jmpq#advisory-comment-43"}' ;;
  "POST repos/\$repo/security-advisories/GHSA-2345-cfgh-jmpr/comments")
    echo "gh: Not Found (HTTP 404)" >&2; exit 1 ;;
  "POST repos/\$repo/security-advisories/GHSA-2345-cfgh-jmpv/comments")
    body='{"id":44}' ;;
  "GET repos/\$repo/pulls/80/comments?per_page=100&page=1")
    body='[{"id":1,"body":"inline finding"}]' ;;
  *)
    echo "mock gh: unexpected invocation: \$method \$path" >&2
    exit 1
    ;;
esac

# gh api --jq applies the filter server-side (raw output, like jq -r) —
# resolve_issue_id relies on this to get a bare id back, not a JSON object.
if [ -n "\$jqf" ]; then
  printf '%s' "\$body" | jq -r "\$jqf"
else
  printf '%s\n' "\$body"
fi
MOCK_EOF
chmod +x "$mock_bin/gh"

PATH="$mock_bin:$PATH"
export PATH

run() { "$GH_REST" "$@"; }

# --- issue-view / issue-comments -----------------------------------------
out="$(run issue-view 55 --jq '.title')"
assert_eq "$out" "Sub issue" "issue-view reads the raw REST object"

out="$(run issue-comments 55)"
case "$out" in
  *"alice"*"first comment"*) : ;;
  *) note "FAIL: issue-comments did not render the comment"; fail=1 ;;
esac

# --- issue-list: pull requests are filtered out ---------------------------
out="$(run issue-list --jq '[.[].number]')"
assert_eq "$out" "[1,2]" "issue-list drops the entry with a pull_request key"

out="$(run issue-list --state all --label bug --jq '[.[].number]')"
assert_eq "$out" "[1]" "issue-list passes --state and --label through to the query"

# --- issue-search: multi-word, case-insensitive, title or body -----------
out="$(run issue-search "config backup" --jq '[.[].number]')"
assert_eq "$out" "[1,2]" "issue-search matches both words case-insensitively across title/body, and still drops the PR"

out="$(run issue-search "nonexistent term" --jq 'length')"
assert_eq "$out" "0" "issue-search finds nothing when a term is absent from both fields"

# --- pagination: a three-page list and an exactly-100-item page ----------
: >"$LOG"
out="$(run issue-list --label threepage --jq 'length')"
assert_eq "$out" "242" "a three-page list (100+100+42) is combined into one array"
assert_eq "$(grep -c 'issues?state=open&labels=threepage' "$LOG")" "3" "three-page list makes exactly three GET calls"

: >"$LOG"
out="$(run issue-list --label exact100 --jq 'length')"
assert_eq "$out" "100" "an exactly-100-item first page is not mistaken for the last page"
assert_eq "$(grep -c 'issues?state=open&labels=exact100' "$LOG")" "2" "a full page always triggers one more page request"

# --- parent: 404 vs. a genuine failure ------------------------------------
out="$(run parent 62 --jq '.number')"
assert_eq "$out" "59" "parent returns the parent issue's number"

out="$(run parent 63)"; rc=$?
assert_eq "$out" "" "parent on a 404 prints nothing"
assert_eq "$rc" "0" "parent on a 404 exits 0"

set +e
run parent 64 >/dev/null 2>"$work/parent64.err"
rc=$?
set -e
if [ "$rc" -eq 0 ]; then
  note "FAIL: parent must not swallow a non-404 failure"
  fail=1
fi

# --- number -> database id resolution ------------------------------------
: >"$LOG"
run add-sub-issue 61 55 >/dev/null
assert_contains "$LOG" "issues/55" "add-sub-issue resolves #55's database id first"
assert_contains "$LOG" "sub_issue_id=9001" "add-sub-issue posts the resolved id, not the issue number"

: >"$LOG"
run add-blocked-by 61 60 >/dev/null
assert_contains "$LOG" "issues/60" "add-blocked-by resolves #60's database id first"
assert_contains "$LOG" "issue_id=9002" "add-blocked-by posts the resolved id, not the issue number"

: >"$LOG"
run remove-sub-issue 61 55 >/dev/null
assert_contains "$LOG" "DELETE repos/$REPO/issues/61/sub_issue" "remove-sub-issue"
assert_contains "$LOG" "sub_issue_id=9001" "remove-sub-issue removes by the resolved id"

: >"$LOG"
run remove-blocked-by 61 60 >/dev/null
assert_contains "$LOG" "DELETE repos/$REPO/issues/61/dependencies/blocked_by/9002" "remove-blocked-by deletes the resolved id from the path"

# --- relationships: read side ---------------------------------------------
out="$(run sub-issues 61 --jq '.[0].number')"
assert_eq "$out" "55" "sub-issues"
out="$(run blocked-by 61 --jq '.[0].number')"
assert_eq "$out" "60" "blocked-by"
out="$(run blocking 61 --jq '.[0].number')"
assert_eq "$out" "277" "blocking"

# --- milestone title resolution -------------------------------------------
: >"$LOG"
bodyfile="$work/body.md"
echo "a body" >"$bodyfile"
run issue-create --title "New" --body-file "$bodyfile" --milestone "Phase 2" >/dev/null
assert_contains "$LOG" "milestones?state=all" "issue-create looks the milestone title up"
assert_contains "$LOG" "milestone=3" "issue-create resolves the milestone title to its number"

# --- labels never go through a full-set PATCH -----------------------------
: >"$LOG"
run issue-edit 70 --add-label foo --remove-label bar
assert_contains "$LOG" "POST repos/$REPO/issues/70/labels" "issue-edit adds a label via POST"
assert_contains "$LOG" "labels[]=foo" "issue-edit adds a label via POST"
assert_contains "$LOG" "DELETE repos/$REPO/issues/70/labels/bar" "issue-edit removes a label via DELETE"
assert_not_contains "$LOG" "PATCH repos/$REPO/issues/70" "a labels-only edit never PATCHes the issue"

: >"$LOG"
run issue-edit 71 --title "New title" >/dev/null
assert_contains "$LOG" "PATCH repos/$REPO/issues/71" "a title/body edit still uses PATCH"

# --- issue-edit: --milestone resolves a title and PATCHes the number -----
: >"$LOG"
run issue-edit 73 --milestone "Phase 2" >/dev/null
assert_contains "$LOG" "milestones?state=all" "issue-edit --milestone looks the title up"
assert_contains "$LOG" "PATCH repos/$REPO/issues/73" "issue-edit --milestone PATCHes the issue"
assert_contains "$LOG" "milestone=3" "issue-edit --milestone resolves the title to its number"

: >"$LOG"
run issue-edit 74 --title "Renamed" --milestone "Phase 2" >/dev/null
assert_eq "$(grep -c 'PATCH repos/'"$REPO"'/issues/74' "$LOG")" "1" "a combined title+milestone edit issues one PATCH"
assert_contains "$LOG" "title=Renamed" "the combined PATCH still carries the title"
assert_contains "$LOG" "milestone=3" "the combined PATCH also carries the resolved milestone"

set +e
run issue-edit 74 --milestone "No Such Milestone" >/dev/null 2>"$work/badmilestone.err"
rc=$?
set -e
if [ "$rc" -eq 0 ]; then
  note "FAIL: issue-edit --milestone must fail on an unknown title"
  fail=1
fi
if ! grep -q "no milestone titled" "$work/badmilestone.err"; then
  note "FAIL: issue-edit --milestone's error should name the missing title"
  fail=1
fi

# --- issue-comment / pr-comment (shared endpoint) -------------------------
: >"$LOG"
commentfile="$work/comment.md"
echo "a comment" >"$commentfile"
run issue-comment 72 --body-file "$commentfile"
assert_contains "$LOG" "POST repos/$REPO/issues/72/comments" "issue-comment"

: >"$LOG"
run pr-comment 80 --body-file "$commentfile"
assert_contains "$LOG" "POST repos/$REPO/issues/80/comments" "pr-comment reuses the issue comments endpoint"

# --- pull requests ---------------------------------------------------------
out="$(run pr-view 80 --jq '.title')"
assert_eq "$out" "A PR" "pr-view"

out="$(run pr-list --base main --head dev --jq '[.[].number]')"
assert_eq "$out" "[81]" "pr-list filters head as owner:branch"

: >"$LOG"
run pr-create --base main --head dev --title "Promote" --body-file "$bodyfile" >/dev/null
assert_contains "$LOG" "POST repos/$REPO/pulls" "pr-create"

: >"$LOG"
run pr-edit 80 --title "Renamed" >/dev/null
assert_contains "$LOG" "PATCH repos/$REPO/pulls/80" "pr-edit"

# --- repo / labels / generic paged --------------------------------------
out="$(run repo-view --jq '.full_name')"
assert_eq "$out" "test-owner/test-repo" "repo-view"

out="$(run label-list --jq '[.[].name]')"
assert_eq "$out" '["bug","feat"]' "label-list"

out="$(run paged "pulls/80/comments" --jq '.[0].body')"
assert_eq "$out" "inline finding" "paged is a generic self-paging GET for an endpoint with no named subcommand"

# --- security advisories -----------------------------------------------------
GHSA="GHSA-2345-cfgh-jmpq"
descfile="$work/advisory.md"
# shellcheck disable=SC2016
printf 'Line one with "quotes" and $(not run) and `ticks`\nLine two\n' >"$descfile"

out="$(run advisory-list --jq '[.[].ghsa_id]')"
assert_eq "$out" '["GHSA-2345-cfgh-jmpq","GHSA-2345-cfgh-jmpr"]' "advisory-list reads every state when none is given"

: >"$LOG"
out="$(run advisory-list --state triage --jq '[.[].state]')"
assert_eq "$out" '["triage"]' "advisory-list --state filters through the query"
assert_contains "$LOG" "api repos/$REPO/security-advisories?per_page=100&state=triage" "advisory-list --state reaches the repository-scoped endpoint"
assert_not_contains "$LOG" "&page=" "advisory-list never sends a page number: the endpoint pages by cursor"

set +e
run advisory-list --state closed >/dev/null 2>"$work/adv-full.err"
rc=$?
set -e
assert_eq "$([ "$rc" -ne 0 ] && echo refused)" "refused" "a full page of advisories is refused, not returned as if complete"
if ! grep -q "may be only the first page" "$work/adv-full.err"; then
  note "FAIL: the full-page refusal should say why"
  fail=1
fi

out="$(run advisory-get "$GHSA" --jq '.severity')"
assert_eq "$out" "high" "advisory-get"

# A real write: the exact method, path and body gh receives.
: >"$LOG"
run advisory-create --summary 'A "quoted" summary' --description-file "$descfile" \
  --severity high --cwe CWE-22 --cwe CWE-59 >/dev/null
assert_contains "$LOG" "api --method POST repos/$REPO/security-advisories --input -" "advisory-create POSTs to the repository's advisories"
body_line="$(grep '^body: ' "$LOG")"
assert_eq "$(jq -r '.summary' <<<"${body_line#body: }")" 'A "quoted" summary' "advisory-create carries the summary verbatim"
assert_eq "$(jq -r '.description' <<<"${body_line#body: }")" "$(cat "$descfile")" "advisory-create carries the description file verbatim, metacharacters included"
assert_eq "$(jq -c '[.severity, .cwe_ids, .vulnerabilities]' <<<"${body_line#body: }")" '["high",["CWE-22","CWE-59"],[]]' "advisory-create sends severity, CWE ids and an empty vulnerabilities list"

# --dry-run prints method, path and body and calls gh not at all.
: >"$LOG"
out="$(run advisory-create --summary S --description-file "$descfile" --severity critical --dry-run)"
assert_eq "$(head -n1 <<<"$out")" "POST repos/$REPO/security-advisories" "advisory-create --dry-run prints the method and path"
assert_eq "$(tail -n1 <<<"$out" | jq -r '.severity')" "critical" "advisory-create --dry-run prints the JSON body"
assert_eq "$(wc -c <"$LOG" | tr -d ' ')" "0" "advisory-create --dry-run never calls gh"

: >"$LOG"
out="$(run advisory-update "$GHSA" --severity low --dry-run)"
assert_eq "$out" "PATCH repos/$REPO/security-advisories/$GHSA
{\"severity\":\"low\"}" "advisory-update --dry-run sends only the fields given"
assert_eq "$(wc -c <"$LOG" | tr -d ' ')" "0" "advisory-update --dry-run never calls gh"

: >"$LOG"
run advisory-update "$GHSA" --summary "New summary" >/dev/null
assert_contains "$LOG" "api --method PATCH repos/$REPO/security-advisories/$GHSA --input -" "advisory-update PATCHes the advisory"
assert_contains "$LOG" 'body: {"summary":"New summary"}' "advisory-update sends only the summary"

for pair in "accept:draft" "reject:closed" "publish:published"; do
  name=${pair%%:*}; state=${pair##*:}
  out="$(run "advisory-$name" "$GHSA" --dry-run)"
  assert_eq "$out" "PATCH repos/$REPO/security-advisories/$GHSA
{\"state\":\"$state\"}" "advisory-$name --dry-run moves the advisory to $state"
  : >"$LOG"
  run "advisory-$name" "$GHSA" >/dev/null
  assert_contains "$LOG" "body: {\"state\":\"$state\"}" "advisory-$name sends state $state"
done

out="$(run advisory-fork "$GHSA" --dry-run)"
assert_eq "$(head -n1 <<<"$out")" "POST repos/$REPO/security-advisories/$GHSA/forks" "advisory-fork --dry-run prints the fork endpoint"
: >"$LOG"
run advisory-fork "$GHSA" >/dev/null
assert_contains "$LOG" "api --method POST repos/$REPO/security-advisories/$GHSA/forks" "advisory-fork POSTs to the forks endpoint"

# Every advisory subcommand refuses what it does not know, before any call.
: >"$LOG"
refused() {
  local why=$1; shift
  local rc
  set +e
  run "$@" >/dev/null 2>&1
  rc=$?
  set -e
  assert_eq "$([ "$rc" -ne 0 ] && echo refused)" "refused" "$why"
}
# Advisory comments: an endpoint GitHub does not document.
out="$(run advisory-comments "$GHSA" --jq '[.[].id]')"
assert_eq "$out" "[41,42]" "advisory-comments lists the advisory's comments"
: >"$LOG"
out="$(run advisory-comments "$GHSA")"
assert_eq "$(jq 'length' <<<"$out")" "2" "advisory-comments without --jq prints the whole array"
assert_contains "$LOG" "api repos/$REPO/security-advisories/$GHSA/comments?per_page=100" "advisory-comments GETs the advisory's comments endpoint"
assert_not_contains "$LOG" "--method" "advisory-comments only reads"

set +e
run advisory-comments GHSA-2345-cfgh-jmpr >/dev/null 2>"$work/adv-cmt.err"
rc=$?
set -e
assert_eq "$([ "$rc" -ne 0 ] && echo failed)" "failed" "advisory-comments fails when the request fails"
if ! grep -q "GHSA-2345-cfgh-jmpr" "$work/adv-cmt.err"; then
  note "FAIL: the advisory-comments failure should name the advisory id"
  fail=1
fi
set +e
run advisory-comments GHSA-2345-cfgh-jmpx >/dev/null 2>"$work/adv-cmt-full.err"
rc=$?
set -e
assert_eq "$([ "$rc" -ne 0 ] && echo refused)" "refused" "a full page of advisory comments is refused, not returned as if complete"
if ! grep -q "may be only the first page" "$work/adv-cmt-full.err"; then
  note "FAIL: the full-page refusal of advisory-comments should say why"
  fail=1
fi

# shellcheck disable=SC2016
printf 'Fix on dev: abc "quoted" $(not run) `ticks`\nsecond line\n' >"$work/comment.md"
: >"$LOG"
out="$(run advisory-comment "$GHSA" --body-file "$work/comment.md")"
assert_eq "$out" "https://x/security/advisories/$GHSA#advisory-comment-43" "advisory-comment prints the new comment's html_url"
assert_contains "$LOG" "api --method POST repos/$REPO/security-advisories/$GHSA/comments --input -" "advisory-comment POSTs to the advisory's comments endpoint"
body_line="$(grep '^body: ' "$LOG")"
assert_eq "$(jq -c 'keys' <<<"${body_line#body: }")" '["body"]' "advisory-comment sends only a body field"
assert_eq "$(jq -r '.body' <<<"${body_line#body: }")" "$(cat "$work/comment.md")" "advisory-comment carries the file verbatim, metacharacters included"

: >"$LOG"
out="$(run advisory-comment --dry-run "$GHSA" --body-file "$work/comment.md")"
assert_eq "$(head -n1 <<<"$out")" "POST repos/$REPO/security-advisories/$GHSA/comments" "advisory-comment --dry-run prints the method and path"
assert_eq "$(tail -n1 <<<"$out" | jq -r '.body')" "$(cat "$work/comment.md")" "advisory-comment --dry-run prints the JSON body"
assert_eq "$(wc -c <"$LOG" | tr -d ' ')" "0" "advisory-comment --dry-run never calls gh"

set +e
run advisory-comment GHSA-2345-cfgh-jmpr --body-file "$work/comment.md" >/dev/null 2>"$work/adv-post.err"
rc=$?
set -e
assert_eq "$([ "$rc" -ne 0 ] && echo failed)" "failed" "advisory-comment fails when the request fails"
if ! grep -q "GHSA-2345-cfgh-jmpr" "$work/adv-post.err"; then
  note "FAIL: the advisory-comment failure should name the advisory id"
  fail=1
fi
set +e
run advisory-comment GHSA-2345-cfgh-jmpv --body-file "$work/comment.md" >/dev/null 2>"$work/adv-nourl.err"
rc=$?
set -e
assert_eq "$([ "$rc" -ne 0 ] && echo failed)" "failed" "advisory-comment fails when the response carries no html_url"
if ! grep -q "GHSA-2345-cfgh-jmpv" "$work/adv-nourl.err"; then
  note "FAIL: the missing-html_url failure should name the advisory id"
  fail=1
fi

: >"$work/empty.md"
printf ' \n\t\n' >"$work/blank.md"
: >"$LOG"
refused "advisory-comment refuses a missing body file" advisory-comment "$GHSA" --body-file "$work/missing.md"
refused "advisory-comment refuses an empty body file" advisory-comment "$GHSA" --body-file "$work/empty.md"
refused "advisory-comment refuses a whitespace-only body file" advisory-comment "$GHSA" --body-file "$work/blank.md"
refused "advisory-comment refuses a directory as the body file" advisory-comment "$GHSA" --body-file "$work"
refused "advisory-comment needs --body-file" advisory-comment "$GHSA"
refused "advisory-comment needs an id" advisory-comment --body-file "$work/comment.md"
refused "advisory-comment refuses a non-GHSA id" advisory-comment "../issues/1" --body-file "$work/comment.md"
refused "advisory-comment refuses an unknown flag" advisory-comment "$GHSA" --body-file "$work/comment.md" --bogus
refused "advisory-comment refuses a second positional argument" advisory-comment "$GHSA" extra --body-file "$work/comment.md"
refused "advisory-comment refuses --body-file with no value" advisory-comment "$GHSA" --body-file
refused "advisory-comments refuses a non-GHSA id" advisory-comments 123
refused "advisory-comments refuses an unknown flag" advisory-comments "$GHSA" --bogus
refused "advisory-comments needs an id" advisory-comments
assert_eq "$(wc -c <"$LOG" | tr -d ' ')" "0" "no refused advisory-comment command reached gh"

: >"$LOG"
refused "advisory-list refuses an unknown flag" advisory-list --bogus
refused "advisory-list refuses an unknown state" advisory-list --state open
refused "advisory-get refuses an unknown flag" advisory-get "$GHSA" --bogus
refused "advisory-get refuses a non-GHSA id" advisory-get 123
refused "advisory-create refuses an unknown flag" advisory-create --summary S --description-file "$descfile" --severity high --bogus x --dry-run
refused "advisory-create refuses an unknown severity" advisory-create --summary S --description-file "$descfile" --severity severe --dry-run
refused "advisory-create refuses a malformed CWE" advisory-create --summary S --description-file "$descfile" --severity high --cwe 22 --dry-run
refused "advisory-create needs a summary" advisory-create --description-file "$descfile" --severity high --dry-run
refused "advisory-create needs a description file that exists" advisory-create --summary S --description-file "$work/missing.md" --severity high --dry-run
refused "advisory-update refuses an unknown flag" advisory-update "$GHSA" --bogus --dry-run
refused "advisory-update needs something to change" advisory-update "$GHSA" --dry-run
refused "advisory-update refuses a non-GHSA id" advisory-update nope --severity low --dry-run
for name in accept reject publish fork; do
  refused "advisory-$name refuses an unknown flag" "advisory-$name" "$GHSA" --bogus
  refused "advisory-$name refuses a second positional argument" "advisory-$name" "$GHSA" extra --dry-run
  refused "advisory-$name needs an id" "advisory-$name" --dry-run
  refused "advisory-$name refuses a non-GHSA id" "advisory-$name" "../issues/1" --dry-run
done
assert_eq "$(wc -c <"$LOG" | tr -d ' ')" "0" "no refused advisory command reached gh"

out="$(run help)"
for sub in advisory-list advisory-get advisory-create advisory-update advisory-accept advisory-reject advisory-fork advisory-publish advisory-comments advisory-comment; do
  case "$out" in
    *"  $sub "*) : ;;
    *) note "FAIL: usage text does not list $sub"; fail=1 ;;
  esac
done

# --- never GraphQL, never search/, never --paginate -----------------------
# The log above only ever accumulates the *last* scenario's calls (each
# scenario truncates it first) — run the full suite once more into one
# combined log so this check covers everything this test exercised.
: >"$LOG"
run issue-view 55 >/dev/null
run issue-list >/dev/null
run issue-search "config backup" >/dev/null
run sub-issues 61 >/dev/null
run blocked-by 61 >/dev/null
run pr-list --base main --head dev >/dev/null
run repo-view >/dev/null
run label-list >/dev/null
run advisory-list >/dev/null
run advisory-get "$GHSA" >/dev/null
if grep -iE 'graphql|search/|--paginate' "$LOG" >/dev/null; then
  note "FAIL: the log contains a forbidden invocation"
  grep -iE 'graphql|search/|--paginate' "$LOG" >&2
  fail=1
fi

if [ "$fail" -eq 0 ]; then
  note "PASS"
fi
exit "$fail"
