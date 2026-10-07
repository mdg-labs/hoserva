#!/usr/bin/env bash
# Post or update the Hoserva test-summary comment on a pull request (doc 06 §7).
# Run by .github/workflows/test-report-comment.yml, which holds a token that can
# write to pull requests, for a finished `CI` run. Everything it reads from that
# run's `test-summary` artifact was written by pull-request code, so it is data:
# nothing from it is executed, sourced or put into a shell command, the PR number
# is checked to be a positive integer, and the comment body reaches the API as a
# file.
#
# Inputs, all from the workflow_run event (the workflow passes them through the
# environment, never into a command line):
#   REPO             owner/name
#   RUN_ID           the CI run's id
#   RUN_CONCLUSION   its conclusion
#   RUN_EVENT        the event that triggered it
#   RUN_HEAD_SHA     the commit it ran on
#   RUN_URL          its page
#   RUN_TIME         when it finished, RFC 3339
# and GH_TOKEN for gh.
#
# The comment is skipped, with the reason logged and exit status 0, when the run
# was cancelled, was not a pull_request run, has no usable artifact, names no
# valid PR number, or the PR's head is no longer the commit the run tested (a
# newer push supersedes it, and an artifact cannot steer the comment onto another
# PR). A failed CI run still updates the comment: it shows the failures. An API
# error is not a skip; it fails the job.
set -euo pipefail

marker='<!-- hoserva-test-report -->'
max_artifact_bytes=$((5 * 1024 * 1024))
max_pr_json_bytes=4096
max_comment_pages=50

die() { printf 'pr-comment: %s\n' "$*" >&2; exit 2; }
skip() { printf 'pr-comment: skipped: %s\n' "$*"; exit 0; }

: "${REPO:?}" "${RUN_ID:?}" "${RUN_CONCLUSION:?}" "${RUN_EVENT:?}" "${RUN_HEAD_SHA:?}" "${RUN_URL:?}" "${RUN_TIME:?}"
[[ $REPO =~ ^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$ ]] || die "invalid REPO"
[[ $RUN_ID =~ ^[1-9][0-9]*$ ]] || die "invalid RUN_ID"
[[ $RUN_HEAD_SHA =~ ^[0-9a-f]{40}$ ]] || die "invalid RUN_HEAD_SHA"

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"

[[ $RUN_CONCLUSION != cancelled ]] || skip "run $RUN_ID was cancelled"
[[ $RUN_EVENT == pull_request ]] || skip "run $RUN_ID was triggered by '$RUN_EVENT', not pull_request"

work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT

artifacts=$(gh api "repos/$REPO/actions/runs/$RUN_ID/artifacts?name=test-summary&per_page=100")
size=$(jq -r '[.artifacts[] | select(.name == "test-summary" and .expired == false) | .size_in_bytes][0] // empty' <<<"$artifacts")
[[ -n $size ]] || skip "run $RUN_ID has no unexpired test-summary artifact"
[[ $size =~ ^[0-9]+$ ]] || die "unreadable artifact size"
((size <= max_artifact_bytes)) || skip "the test-summary artifact is $size bytes, over the $max_artifact_bytes limit"

gh run download "$RUN_ID" --repo "$REPO" --name test-summary --dir "$work/artifact"

summary="$work/artifact/test-summary.md"
pr_json="$work/artifact/pr.json"
for f in "$summary" "$pr_json"; do
  [[ -f $f && ! -L $f ]] || skip "the artifact has no regular file ${f##*/}"
done
(($(wc -c <"$pr_json") <= max_pr_json_bytes)) || skip "pr.json is over $max_pr_json_bytes bytes"

number=$(jq -er '.number | if type == "number" and . == floor and . > 0 and . < 1000000000 then tostring else empty end' "$pr_json" 2>/dev/null) \
  || skip "pr.json holds no positive integer PR number"
[[ $number =~ ^[1-9][0-9]*$ ]] || skip "pr.json holds no positive integer PR number"

current_sha=$(gh api "repos/$REPO/pulls/$number" --jq .head.sha)
[[ $current_sha == "$RUN_HEAD_SHA" ]] \
  || skip "PR #$number is now at ${current_sha:0:7}, not the ${RUN_HEAD_SHA:0:7} that run $RUN_ID tested"

(cd "$repo_root" && go run ./scripts/devenv/testreport -comment "$summary" -run-url "$RUN_URL" -sha "$RUN_HEAD_SHA" -time "$RUN_TIME") >"$work/body.md" \
  || die "could not render the comment"
grep -qF -- "$marker" "$work/body.md" || die "the rendered comment has no marker"

comment_id=""
for ((page = 1; page <= max_comment_pages; page++)); do
  comments=$(gh api "repos/$REPO/issues/$number/comments?per_page=100&page=$page")
  comment_id=$(jq -r --arg m "$marker" '[.[] | select(.user.login == "github-actions[bot]" and .user.type == "Bot" and ((.body // "") | contains($m))) | .id][0] // empty' <<<"$comments")
  [[ -z $comment_id ]] || break
  (($(jq 'length' <<<"$comments") == 100)) || break
done

if [[ -n $comment_id ]]; then
  [[ $comment_id =~ ^[1-9][0-9]*$ ]] || die "unreadable comment id"
  gh api --method PATCH "repos/$REPO/issues/comments/$comment_id" -F "body=@$work/body.md" >/dev/null
  printf 'pr-comment: updated comment %s on PR #%s\n' "$comment_id" "$number"
else
  gh api --method POST "repos/$REPO/issues/$number/comments" -F "body=@$work/body.md" >/dev/null
  printf 'pr-comment: created the comment on PR #%s\n' "$number"
fi
