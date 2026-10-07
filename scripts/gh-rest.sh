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

# --- security advisories -------------------------------------------------
#
# Repository security advisories (Q91): where a Critical or High finding is
# recorded instead of a public issue. Every write subcommand takes
# --dry-run, which prints the method, the path and the JSON body it would
# send and calls nothing — the way to check a write without creating or
# changing a real advisory. JSON bodies are built with jq from --arg and
# --rawfile values and sent with `gh api --input -`, never assembled as a
# string.

ADVISORY_STATES="triage draft published closed"
ADVISORY_SEVERITIES="critical high medium low"

require_ghsa() {
  [[ $2 =~ ^GHSA(-[2-9cfghjmpqrvwx]{4}){3}$ ]] || die "$1: not a GHSA id: $2"
}

require_one_of() {
  local what=$1 value=$2 allowed=$3 item
  for item in $allowed; do
    [[ $value == "$item" ]] && return 0
  done
  die "$what: '$value' is not one of: ${allowed// /, }"
}

# Sends one advisory write — or, with ADV_DRY_RUN=1, prints what it would
# send and sends nothing. $1 method, $2 path below repos/$REPO, $3 JSON body.
advisory_send() {
  local method=$1 path=$2 body=$3
  if [[ $ADV_DRY_RUN == 1 ]]; then
    printf '%s repos/%s/%s\n%s\n' "$method" "$REPO" "$path" "$body"
    return 0
  fi
  gh api --method "$method" "repos/$REPO/$path" --input - <<<"$body"
}

# GitHub pages this endpoint with before/after cursors, not page numbers,
# so one request for the largest page is made, and a full page is refused
# rather than silently truncated.
cmd_advisory_list() {
  local state="" jqf=""
  while [[ $# -gt 0 ]]; do
    case $1 in
      --state) [[ $# -ge 2 ]] || die "advisory-list: --state needs a value"; state=$2; shift 2 ;;
      --jq) [[ $# -ge 2 ]] || die "advisory-list: --jq needs a value"; jqf=$2; shift 2 ;;
      *) die "advisory-list: unrecognized argument: $1" ;;
    esac
  done
  local path="repos/$REPO/security-advisories?per_page=100"
  if [[ -n $state ]]; then
    require_one_of "advisory-list --state" "$state" "$ADVISORY_STATES"
    path+="&state=$state"
  fi
  local out
  out=$(gh api "$path") || die "advisory-list: could not list security advisories"
  [[ $(jq 'length' <<<"$out") -lt 100 ]] \
    || die "advisory-list: 100 advisories came back, which may be only the first page; narrow it with --state"
  print_json "$out" "$jqf"
}

cmd_advisory_get() {
  [[ $# -ge 1 ]] || die "usage: advisory-get <ghsa_id> [--jq f]"
  local id=$1; shift
  require_ghsa advisory-get "$id"
  parse_optional_jq "$@"
  local out
  out=$(gh api "repos/$REPO/security-advisories/$id") \
    || die "advisory-get: could not read $id"
  print_json "$out" "$JQF"
}

# Reads the field flags advisory-create and advisory-update share into
# ADV_SUMMARY, ADV_DESC_FILE, ADV_SEVERITY, ADV_CWES and ADV_DRY_RUN;
# $1 names the calling subcommand for its errors.
parse_advisory_fields() {
  local who=$1; shift
  ADV_SUMMARY="" ADV_DESC_FILE="" ADV_SEVERITY="" ADV_DRY_RUN=0
  ADV_CWES=()
  while [[ $# -gt 0 ]]; do
    case $1 in
      --summary) [[ $# -ge 2 ]] || die "$who: --summary needs a value"; ADV_SUMMARY=$2; shift 2 ;;
      --description-file) [[ $# -ge 2 ]] || die "$who: --description-file needs a value"; ADV_DESC_FILE=$2; shift 2 ;;
      --severity) [[ $# -ge 2 ]] || die "$who: --severity needs a value"; ADV_SEVERITY=$2; shift 2 ;;
      --cwe) [[ $# -ge 2 ]] || die "$who: --cwe needs a value"; ADV_CWES+=("$2"); shift 2 ;;
      --dry-run) ADV_DRY_RUN=1; shift ;;
      *) die "$who: unrecognized argument: $1" ;;
    esac
  done
  [[ -z $ADV_SEVERITY ]] || require_one_of "$who --severity" "$ADV_SEVERITY" "$ADVISORY_SEVERITIES"
  local cwe
  for cwe in ${ADV_CWES[@]+"${ADV_CWES[@]}"}; do
    [[ $cwe =~ ^CWE-[0-9]+$ ]] || die "$who --cwe: '$cwe' is not a CWE id like CWE-22"
  done
  [[ -z $ADV_DESC_FILE || -f $ADV_DESC_FILE ]] || die "$who: description file not found: $ADV_DESC_FILE"
}

# Prints the JSON body holding only the fields that were given. The
# description is read from its file by jq itself, so its text is never
# interpolated into anything.
advisory_fields_json() {
  local desc_args=()
  [[ -n $ADV_DESC_FILE ]] && desc_args=(--rawfile description "$ADV_DESC_FILE")
  local cwes_json='[]'
  if (( ${#ADV_CWES[@]} )); then
    cwes_json=$(printf '%s\n' "${ADV_CWES[@]}" | jq -R . | jq -cs .)
  fi
  jq -cn --arg summary "$ADV_SUMMARY" --arg severity "$ADV_SEVERITY" \
    --argjson cwes "$cwes_json" ${desc_args[@]+"${desc_args[@]}"} '
    {}
    + (if $summary != "" then {summary: $summary} else {} end)
    + (if $ARGS.named | has("description") then {description: $ARGS.named.description} else {} end)
    + (if $severity != "" then {severity: $severity} else {} end)
    + (if ($cwes | length) > 0 then {cwe_ids: $cwes} else {} end)'
}

cmd_advisory_create() {
  parse_advisory_fields advisory-create "$@"
  [[ -n $ADV_SUMMARY && -n $ADV_DESC_FILE && -n $ADV_SEVERITY ]] \
    || die "usage: advisory-create --summary <s> --description-file <f> --severity critical|high|medium|low [--cwe CWE-n]... [--dry-run]"
  local fields body
  fields=$(advisory_fields_json) || die "advisory-create: could not build the request body"
  body=$(jq -c '. + {vulnerabilities: []}' <<<"$fields")
  advisory_send POST security-advisories "$body" || die "advisory-create: failed"
}

cmd_advisory_update() {
  [[ $# -ge 1 ]] \
    || die "usage: advisory-update <ghsa_id> [--summary s] [--description-file f] [--severity s] [--cwe CWE-n]... [--dry-run]"
  local id=$1; shift
  require_ghsa advisory-update "$id"
  parse_advisory_fields advisory-update "$@"
  if [[ -z $ADV_SUMMARY && -z $ADV_DESC_FILE && -z $ADV_SEVERITY ]] && (( ${#ADV_CWES[@]} == 0 )); then
    die "advisory-update: nothing to change"
  fi
  local body
  body=$(advisory_fields_json) || die "advisory-update: could not build the request body"
  advisory_send PATCH "security-advisories/$id" "$body" || die "advisory-update: could not update $id"
}

# Moves an advisory to a new state: triage to draft (accept), anything to
# closed (reject), a draft to published (publish).
advisory_set_state() {
  local who=$1 state=$2; shift 2
  local id="" dry=0
  while [[ $# -gt 0 ]]; do
    case $1 in
      --dry-run) dry=1; shift ;;
      -*) die "$who: unrecognized argument: $1" ;;
      *) [[ -z $id ]] || die "$who: unrecognized argument: $1"; id=$1; shift ;;
    esac
  done
  [[ -n $id ]] || die "usage: $who <ghsa_id> [--dry-run]"
  require_ghsa "$who" "$id"
  ADV_DRY_RUN=$dry
  advisory_send PATCH "security-advisories/$id" "$(jq -cn --arg s "$state" '{state: $s}')" \
    || die "$who: could not move $id to $state"
}

cmd_advisory_accept() { advisory_set_state advisory-accept draft "$@"; }
cmd_advisory_reject() { advisory_set_state advisory-reject closed "$@"; }
cmd_advisory_publish() { advisory_set_state advisory-publish published "$@"; }

cmd_advisory_fork() {
  local id="" dry=0
  while [[ $# -gt 0 ]]; do
    case $1 in
      --dry-run) dry=1; shift ;;
      -*) die "advisory-fork: unrecognized argument: $1" ;;
      *) [[ -z $id ]] || die "advisory-fork: unrecognized argument: $1"; id=$1; shift ;;
    esac
  done
  [[ -n $id ]] || die "usage: advisory-fork <ghsa_id> [--dry-run]"
  require_ghsa advisory-fork "$id"
  ADV_DRY_RUN=$dry
  advisory_send POST "security-advisories/$id/forks" '{}' || die "advisory-fork: could not fork $id"
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

usage() {
  cat <<'USAGE_EOF'
Usage: gh-rest.sh <subcommand> [args...]

Issues
  issue-view <n> [--jq f]
  issue-comments <n> [--jq f]
  issue-list [--state s] [--label l] [--jq f]
  issue-search <terms> [--state s] [--jq f]
  issue-create --title t --body-file f [--label l]... [--milestone t]
  issue-edit <n> [--title t] [--body-file f] [--milestone t] [--add-label l]... [--remove-label l]...
  issue-comment <n> --body-file f
Relationships
  parent <n> [--jq f]
  sub-issues <n> [--jq f]
  add-sub-issue <epic> <n>
  remove-sub-issue <epic> <n>
  blocked-by <n> [--jq f]
  blocking <n> [--jq f]
  add-blocked-by <n> <dep>
  remove-blocked-by <n> <dep>
Pull requests
  pr-view <n> [--jq f]
  pr-list [--base b] [--head h] [--state s] [--jq f]
  pr-create --base b --head h --title t --body-file f
  pr-edit <n> [--title t] [--body-file f]
  pr-comment <n> --body-file f
Security advisories (Q91; write subcommands take --dry-run, which prints the
method, path and JSON body and sends nothing)
  advisory-list [--state triage|draft|published|closed] [--jq f]
  advisory-get <ghsa_id> [--jq f]
  advisory-create --summary s --description-file f --severity critical|high|medium|low [--cwe CWE-n]... [--dry-run]
  advisory-update <ghsa_id> [--summary s] [--description-file f] [--severity s] [--cwe CWE-n]... [--dry-run]
  advisory-accept <ghsa_id> [--dry-run]     triage to draft
  advisory-reject <ghsa_id> [--dry-run]     to closed
  advisory-fork <ghsa_id> [--dry-run]       temporary private fork
  advisory-publish <ghsa_id> [--dry-run]
Repository
  repo-view [--jq f]
  label-list [--jq f]
  paged <repo-relative-path> [--jq f]
USAGE_EOF
}

main() {
  [[ $# -ge 1 ]] || { usage >&2; exit 1; }
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
    advisory-list) cmd_advisory_list "$@" ;;
    advisory-get) cmd_advisory_get "$@" ;;
    advisory-create) cmd_advisory_create "$@" ;;
    advisory-update) cmd_advisory_update "$@" ;;
    advisory-accept) cmd_advisory_accept "$@" ;;
    advisory-reject) cmd_advisory_reject "$@" ;;
    advisory-fork) cmd_advisory_fork "$@" ;;
    advisory-publish) cmd_advisory_publish "$@" ;;
    help | -h | --help) usage ;;
    *) die "unknown subcommand: $sub (gh-rest.sh help lists them)" ;;
  esac
}

main "$@"
