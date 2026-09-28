#!/usr/bin/env bash
# Repository-scoped REST wrapper around `gh api`, so no skill, agent
# template or script has to know GitHub's REST quirks or call a
# GraphQL-backed `gh` subcommand directly (issue #410).
#
# Why this exists: in a Claude Code cloud session the egress proxy answers
# every `https://api.github.com/graphql` request with HTTP 403 (every
# `gh issue`/`gh pr`/`gh repo view`/`gh label list`/`gh release list`
# subcommand is a GraphQL query or mutation under the hood), and also
# refuses `search/issues` and any `--paginate`-followed
# `repositories/{id}/...` link (a numeric-id repository path). Every
# subcommand below only ever calls `gh api repos/$REPO/...` — a plain
# `gh api` call, an argv built with `-f`/`-F` (never `sh -c`, never a
# string-built JSON body) — which works identically on the maintainer's
# own machine and in a cloud session.
#
# Usage: gh-rest.sh <subcommand> [args...]
set -euo pipefail

REPO=${GH_REPO:-mdg-labs/hoserva}

die() { printf 'gh-rest: %s\n' "$*" >&2; exit 1; }

urlencode() { jq -rn --arg s "$1" '$s|@uri'; }

# Pages a GET list endpoint (a path already rooted at "repos/$REPO/...",
# with or without its own query string) into one combined JSON array.
# GitHub's own pagination follows the response's `Link` header, which
# points at `repositories/{id}/...` — refused by the cloud proxy — so this
# pages itself with `?per_page=100&page=N` instead, and never `--paginate`.
# A page with fewer than 100 items is the last one; the loop never assumes
# a fixed count of pages.
#
# Pages accumulate into a file, one JSON array per line, combined at the
# end with `jq -s add` reading from that file — never `--argjson` on a
# growing value: a few full pages of real issue bodies is easily past
# ARG_MAX, and `--argjson` failing mid-loop silently truncated the result
# instead of erroring, because `combined=$(...)` under `set -e` inside a
# `while` loop does not stop the script (a failing command substitution in
# an assignment is not checked here any more than it is anywhere else).
gh_rest_list() {
  local path=$1
  local sep='?'
  [[ $path == *'?'* ]] && sep='&'
  local page=1 out count pages_file
  pages_file=$(mktemp)
  while :; do
    if ! out=$(gh api "${path}${sep}per_page=100&page=${page}"); then
      rm -f "$pages_file"
      return 1
    fi
    count=$(jq 'length' <<<"$out")
    printf '%s\n' "$out" >>"$pages_file"
    (( count < 100 )) && break
    page=$(( page + 1 ))
  done
  jq -cs 'add' "$pages_file"
  rm -f "$pages_file"
}

# Resolves an issue (or PR — they share the same table) number to its
# opaque database id, which the sub-issues and dependency endpoints take
# instead of the number a caller actually knows.
resolve_issue_id() {
  local n=$1
  gh api "repos/$REPO/issues/$n" --jq '.id' \
    || die "could not resolve #$n to a database id"
}

resolve_milestone_number() {
  local title=$1 out num
  out=$(gh_rest_list "repos/$REPO/milestones?state=all") \
    || die "could not list milestones"
  num=$(jq -r --arg t "$title" '[.[] | select(.title == $t)][0].number // empty' <<<"$out")
  [[ -n $num ]] || die "no milestone titled '$title'"
  printf '%s' "$num"
}

# Prints $1 (a JSON value) as-is, or filtered through $2 (a --jq-style
# filter) when non-empty — the same shape `gh ... --jq` callers are used to.
print_json() {
  local out=$1 jqf=$2
  if [[ -n $jqf ]]; then
    jq -cr "$jqf" <<<"$out"
  else
    printf '%s\n' "$out"
  fi
}

# Consumes a trailing "--jq <filter>" from the remaining args into $JQF.
# Dies on anything else so a typo'd flag doesn't silently do nothing.
JQF=""
parse_optional_jq() {
  JQF=""
  while [[ $# -gt 0 ]]; do
    case $1 in
      --jq) JQF=$2; shift 2 ;;
      *) die "unrecognized argument: $1" ;;
    esac
  done
}

# --- issues ------------------------------------------------------------

cmd_issue_view() {
  [[ $# -ge 1 ]] || die "usage: issue-view <n> [--jq f]"
  local n=$1; shift
  parse_optional_jq "$@"
  local out
  out=$(gh api "repos/$REPO/issues/$n") || die "issue-view: could not read #$n"
  out=$(jq '. + {url: .html_url}' <<<"$out")
  print_json "$out" "$JQF"
}

cmd_issue_comments() {
  [[ $# -ge 1 ]] || die "usage: issue-comments <n> [--jq f]"
  local n=$1; shift
  parse_optional_jq "$@"
  local out
  out=$(gh_rest_list "repos/$REPO/issues/$n/comments") \
    || die "issue-comments: could not read #$n"
  if [[ -n $JQF ]]; then
    jq -cr "$JQF" <<<"$out"
  else
    jq -r '.[] | "--- \(.user.login) at \(.created_at) ---\n\(.body)\n"' <<<"$out"
  fi
}

cmd_issue_list() {
  local state="open" label="" jqf=""
  while [[ $# -gt 0 ]]; do
    case $1 in
      --state) state=$2; shift 2 ;;
      --label) label=$2; shift 2 ;;
      --jq) jqf=$2; shift 2 ;;
      *) die "issue-list: unrecognized argument: $1" ;;
    esac
  done
  local path
  path="repos/$REPO/issues?state=$(urlencode "$state")"
  [[ -n $label ]] && path+="&labels=$(urlencode "$label")"
  local out
  out=$(gh_rest_list "$path") || die "issue-list: could not list issues"
  # This endpoint also returns pull requests; a PR always carries a
  # "pull_request" key an issue never has.
  out=$(jq -c '[.[] | select(has("pull_request") | not)]' <<<"$out")
  print_json "$out" "$jqf"
}

cmd_issue_search() {
  [[ $# -ge 1 ]] || die "usage: issue-search <terms> [--state s] [--jq f]"
  local terms=$1; shift
  local state="open" jqf=""
  while [[ $# -gt 0 ]]; do
    case $1 in
      --state) state=$2; shift 2 ;;
      --jq) jqf=$2; shift 2 ;;
      *) die "issue-search: unrecognized argument: $1" ;;
    esac
  done
  # No search/issues (refused in a cloud session): list every issue at the
  # requested state and match every whitespace-separated term
  # case-insensitively against title or body. Results come back in the
  # list endpoint's own order (most recently updated first), not GitHub's
  # relevance ranking.
  local out
  out=$(gh_rest_list "repos/$REPO/issues?state=$(urlencode "$state")") \
    || die "issue-search: could not list issues"
  out=$(jq -c --arg terms "$terms" '
    ($terms | ascii_downcase | split(" ") | map(select(length > 0))) as $words
    | [ .[]
        | select(has("pull_request") | not)
        | select(
            ((((.title // "") + "\n" + (.body // "")) | ascii_downcase)) as $hay
            | all($words[]; . as $w | $hay | contains($w))
          )
      ]' <<<"$out")
  print_json "$out" "$jqf"
}

cmd_issue_create() {
  local title="" bodyfile="" milestone=""
  local labels=()
  while [[ $# -gt 0 ]]; do
    case $1 in
      --title) title=$2; shift 2 ;;
      --body-file) bodyfile=$2; shift 2 ;;
      --label) labels+=("$2"); shift 2 ;;
      --milestone) milestone=$2; shift 2 ;;
      *) die "issue-create: unrecognized argument: $1" ;;
    esac
  done
  [[ -n $title && -n $bodyfile ]] \
    || die "usage: issue-create --title <t> --body-file <f> [--label l]... [--milestone t]"
  [[ -f $bodyfile ]] || die "issue-create: body file not found: $bodyfile"
  local args=(--method POST "repos/$REPO/issues" -f "title=$title" -F "body=@$bodyfile")
  local label
  for label in ${labels[@]+"${labels[@]}"}; do
    args+=(-f "labels[]=$label")
  done
  if [[ -n $milestone ]]; then
    local num
    num=$(resolve_milestone_number "$milestone")
    args+=(-F "milestone=$num")
  fi
  gh api "${args[@]}" || die "issue-create: failed"
}

cmd_issue_edit() {
  [[ $# -ge 1 ]] \
    || die "usage: issue-edit <n> [--title t] [--body-file f] [--milestone t] [--add-label l]... [--remove-label l]..."
  local n=$1; shift
  local title="" bodyfile="" milestone=""
  local add_labels=() remove_labels=()
  while [[ $# -gt 0 ]]; do
    case $1 in
      --title) title=$2; shift 2 ;;
      --body-file) bodyfile=$2; shift 2 ;;
      --milestone) milestone=$2; shift 2 ;;
      --add-label) add_labels+=("$2"); shift 2 ;;
      --remove-label) remove_labels+=("$2"); shift 2 ;;
      *) die "issue-edit: unrecognized argument: $1" ;;
    esac
  done
  # Resolve the milestone title before any write, so a bad title fails with
  # nothing written yet.
  local milestone_num=""
  [[ -n $milestone ]] && milestone_num=$(resolve_milestone_number "$milestone")
  if [[ -n $title || -n $bodyfile || -n $milestone_num ]]; then
    local args=(--method PATCH "repos/$REPO/issues/$n")
    [[ -n $title ]] && args+=(-f "title=$title")
    [[ -n $bodyfile ]] && args+=(-F "body=@$bodyfile")
    [[ -n $milestone_num ]] && args+=(-F "milestone=$milestone_num")
    gh api "${args[@]}" >/dev/null || die "issue-edit: could not update #$n"
  fi
  # Labels go through the dedicated add/remove endpoints, never a full-set
  # PATCH: a PATCH here could race scripts/issue-status.sh's own read-then-
  # PATCH of the status:* label and drop whichever write lost the race.
  if (( ${#add_labels[@]} )); then
    local args=(--method POST "repos/$REPO/issues/$n/labels")
    local label
    for label in "${add_labels[@]}"; do
      args+=(-f "labels[]=$label")
    done
    gh api "${args[@]}" >/dev/null || die "issue-edit: could not add labels to #$n"
  fi
  local label
  for label in ${remove_labels[@]+"${remove_labels[@]}"}; do
    gh api --method DELETE "repos/$REPO/issues/$n/labels/$(urlencode "$label")" >/dev/null \
      || die "issue-edit: could not remove label '$label' from #$n"
  done
}

cmd_issue_comment() {
  [[ $# -eq 3 && $2 == --body-file ]] || die "usage: issue-comment <n> --body-file <f>"
  local n=$1 bodyfile=$3
  [[ -f $bodyfile ]] || die "issue-comment: body file not found: $bodyfile"
  gh api --method POST "repos/$REPO/issues/$n/comments" -F "body=@$bodyfile" >/dev/null \
    || die "issue-comment: could not comment on #$n"
}

# --- relationships -------------------------------------------------------

cmd_parent() {
  [[ $# -ge 1 ]] || die "usage: parent <n> [--jq f]"
  local n=$1; shift
  parse_optional_jq "$@"
  local err out rc
  err=$(mktemp)
  # rc must be captured in the else branch, not after fi: `$?` read there is
  # the status of the *if statement*, and an if whose branch didn't run
  # reads back 0 — so checking it after fi would report success for a call
  # that just failed (the same pitfall issue-status.sh's retry loop avoids).
  if out=$(gh api "repos/$REPO/issues/$n/parent" 2>"$err"); then
    rm -f "$err"
    print_json "$out" "$JQF"
    return 0
  else
    rc=$?
  fi
  # An issue with no parent is a 404, not a failure: print nothing and
  # succeed. Any other failure (auth, 5xx, network) still exits non-zero —
  # collapsing that into "no parent" would silently hide a broken read.
  if grep -q 'HTTP 404' "$err"; then
    rm -f "$err"
    return 0
  fi
  cat "$err" >&2
  rm -f "$err"
  return "$rc"
}

cmd_sub_issues() {
  [[ $# -ge 1 ]] || die "usage: sub-issues <n> [--jq f]"
  local n=$1; shift
  parse_optional_jq "$@"
  local out
  out=$(gh_rest_list "repos/$REPO/issues/$n/sub_issues") \
    || die "sub-issues: could not read #$n"
  print_json "$out" "$JQF"
}

cmd_add_sub_issue() {
  [[ $# -eq 2 ]] || die "usage: add-sub-issue <epic> <n>"
  local epic=$1 n=$2 id
  id=$(resolve_issue_id "$n")
  gh api --method POST "repos/$REPO/issues/$epic/sub_issues" -F "sub_issue_id=$id" >/dev/null \
    || die "add-sub-issue: could not add #$n under #$epic"
}

cmd_remove_sub_issue() {
  [[ $# -eq 2 ]] || die "usage: remove-sub-issue <epic> <n>"
  local epic=$1 n=$2 id
  id=$(resolve_issue_id "$n")
  gh api --method DELETE "repos/$REPO/issues/$epic/sub_issue" -F "sub_issue_id=$id" >/dev/null \
    || die "remove-sub-issue: could not remove #$n from #$epic"
}

cmd_blocked_by() { generic_dep_list blocked_by "$@"; }
cmd_blocking() { generic_dep_list blocking "$@"; }

generic_dep_list() {
  local which=$1; shift
  [[ $# -ge 1 ]] || die "usage: $which <n> [--jq f]"
  local n=$1; shift
  parse_optional_jq "$@"
  local out
  out=$(gh_rest_list "repos/$REPO/issues/$n/dependencies/$which") \
    || die "$which: could not read #$n"
  print_json "$out" "$JQF"
}

cmd_add_blocked_by() {
  [[ $# -eq 2 ]] || die "usage: add-blocked-by <n> <dep>"
  local n=$1 dep=$2 id
  id=$(resolve_issue_id "$dep")
  gh api --method POST "repos/$REPO/issues/$n/dependencies/blocked_by" -F "issue_id=$id" >/dev/null \
    || die "add-blocked-by: could not link #$n blocked-by #$dep"
}

cmd_remove_blocked_by() {
  [[ $# -eq 2 ]] || die "usage: remove-blocked-by <n> <dep>"
  local n=$1 dep=$2 id
  id=$(resolve_issue_id "$dep")
  gh api --method DELETE "repos/$REPO/issues/$n/dependencies/blocked_by/$id" >/dev/null \
    || die "remove-blocked-by: could not unlink #$n blocked-by #$dep"
}

# --- pull requests -------------------------------------------------------

cmd_pr_view() {
  [[ $# -ge 1 ]] || die "usage: pr-view <n> [--jq f]"
  local n=$1; shift
  parse_optional_jq "$@"
  local out
  out=$(gh api "repos/$REPO/pulls/$n") || die "pr-view: could not read PR #$n"
  out=$(jq '. + {url: .html_url}' <<<"$out")
  print_json "$out" "$JQF"
}

cmd_pr_list() {
  local base="" head="" state="open" jqf=""
  while [[ $# -gt 0 ]]; do
    case $1 in
      --base) base=$2; shift 2 ;;
      --head) head=$2; shift 2 ;;
      --state) state=$2; shift 2 ;;
      --jq) jqf=$2; shift 2 ;;
      *) die "pr-list: unrecognized argument: $1" ;;
    esac
  done
  local path
  path="repos/$REPO/pulls?state=$(urlencode "$state")"
  [[ -n $base ]] && path+="&base=$(urlencode "$base")"
  if [[ -n $head ]]; then
    # GitHub's pulls list filters `head` on "owner:branch", not a bare
    # branch name.
    local owner=${REPO%%/*}
    path+="&head=$(urlencode "$owner:$head")"
  fi
  local out
  out=$(gh_rest_list "$path") || die "pr-list: could not list PRs"
  out=$(jq -c '[.[] | . + {url: .html_url}]' <<<"$out")
  print_json "$out" "$jqf"
}

cmd_pr_create() {
  local base="" head="" title="" bodyfile=""
  while [[ $# -gt 0 ]]; do
    case $1 in
      --base) base=$2; shift 2 ;;
      --head) head=$2; shift 2 ;;
      --title) title=$2; shift 2 ;;
      --body-file) bodyfile=$2; shift 2 ;;
      *) die "pr-create: unrecognized argument: $1" ;;
    esac
  done
  [[ -n $base && -n $head && -n $title && -n $bodyfile ]] \
    || die "usage: pr-create --base b --head h --title t --body-file f"
  [[ -f $bodyfile ]] || die "pr-create: body file not found: $bodyfile"
  gh api --method POST "repos/$REPO/pulls" \
    -f "base=$base" -f "head=$head" -f "title=$title" -F "body=@$bodyfile" \
    || die "pr-create: failed"
}

cmd_pr_edit() {
  [[ $# -ge 1 ]] || die "usage: pr-edit <n> [--title t] [--body-file f]"
  local n=$1; shift
  local title="" bodyfile=""
  while [[ $# -gt 0 ]]; do
    case $1 in
      --title) title=$2; shift 2 ;;
      --body-file) bodyfile=$2; shift 2 ;;
      *) die "pr-edit: unrecognized argument: $1" ;;
    esac
  done
  [[ -n $title || -n $bodyfile ]] || die "pr-edit: nothing to change"
  local args=(--method PATCH "repos/$REPO/pulls/$n")
  [[ -n $title ]] && args+=(-f "title=$title")
  [[ -n $bodyfile ]] && args+=(-F "body=@$bodyfile")
  gh api "${args[@]}" >/dev/null || die "pr-edit: could not update PR #$n"
}

cmd_pr_comment() {
  # A top-level PR comment is an issue comment — PRs and issues share the
  # same numbering and the same /issues/{n}/comments endpoint.
  [[ $# -eq 3 && $2 == --body-file ]] || die "usage: pr-comment <n> --body-file <f>"
  cmd_issue_comment "$1" "$2" "$3"
}

# --- repo ------------------------------------------------------------

cmd_repo_view() {
  parse_optional_jq "$@"
  local out
  out=$(gh api "repos/$REPO") || die "repo-view: failed"
  print_json "$out" "$JQF"
}

cmd_label_list() {
  parse_optional_jq "$@"
  local out
  out=$(gh_rest_list "repos/$REPO/labels") || die "label-list: failed"
  print_json "$out" "$JQF"
}

# A generic paged GET for a REST list endpoint this helper has no named
# subcommand for yet (e.g. a pull request's inline review comments or
# review submissions) — still repository-scoped, still self-paging, never
# `--paginate`.
cmd_paged() {
  [[ $# -ge 1 ]] || die "usage: paged <repo-relative-path> [--jq f]"
  local path=$1; shift
  parse_optional_jq "$@"
  local out
  out=$(gh_rest_list "repos/$REPO/$path") || die "paged: could not list $path"
  print_json "$out" "$JQF"
}

main() {
  [[ $# -ge 1 ]] || die "usage: gh-rest.sh <subcommand> [args...]"
  local sub=$1; shift
  case $sub in
    issue-view) cmd_issue_view "$@" ;;
    issue-comments) cmd_issue_comments "$@" ;;
    issue-list) cmd_issue_list "$@" ;;
    issue-search) cmd_issue_search "$@" ;;
    issue-create) cmd_issue_create "$@" ;;
    issue-edit) cmd_issue_edit "$@" ;;
    issue-comment) cmd_issue_comment "$@" ;;
    parent) cmd_parent "$@" ;;
    sub-issues) cmd_sub_issues "$@" ;;
    add-sub-issue) cmd_add_sub_issue "$@" ;;
    remove-sub-issue) cmd_remove_sub_issue "$@" ;;
    blocked-by) cmd_blocked_by "$@" ;;
    blocking) cmd_blocking "$@" ;;
    add-blocked-by) cmd_add_blocked_by "$@" ;;
    remove-blocked-by) cmd_remove_blocked_by "$@" ;;
    pr-view) cmd_pr_view "$@" ;;
    pr-list) cmd_pr_list "$@" ;;
    pr-create) cmd_pr_create "$@" ;;
    pr-edit) cmd_pr_edit "$@" ;;
    pr-comment) cmd_pr_comment "$@" ;;
    repo-view) cmd_repo_view "$@" ;;
    label-list) cmd_label_list "$@" ;;
    paged) cmd_paged "$@" ;;
    *) die "unknown subcommand: $sub" ;;
  esac
}

main "$@"
