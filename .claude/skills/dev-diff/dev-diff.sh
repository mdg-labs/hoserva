#!/usr/bin/env bash
# Fast-forwards local main from origin/main (without switching branches) and
# reports how dev differs from it. Read-only w.r.t. dev: never touches its ref.
set -euo pipefail

start_branch="$(git rev-parse --abbrev-ref HEAD)"

if [[ -n "$(git status --porcelain)" ]]; then
    echo "ERROR: working tree not clean on '$start_branch'. Aborting without touching refs."
    exit 1
fi

git fetch origin --quiet

if [[ "$start_branch" == "main" ]]; then
    if ! git pull --ff-only origin main --quiet 2>/tmp/dev-diff-err; then
        echo "ERROR: local main did not fast-forward from origin/main (diverged)."
        cat /tmp/dev-diff-err
        exit 1
    fi
else
    if ! git fetch origin main:main --quiet 2>/tmp/dev-diff-err; then
        echo "ERROR: local main did not fast-forward from origin/main (diverged)."
        cat /tmp/dev-diff-err
        exit 1
    fi
fi

if ! git rev-parse --verify dev >/dev/null 2>&1; then
    echo "No local 'dev' branch exists — nothing to compare (origin/dev is not a stand-in)."
    exit 0
fi

local_dev="$(git rev-parse dev)"
origin_dev="$(git rev-parse origin/dev 2>/dev/null || echo "")"
if [[ -z "$origin_dev" ]]; then
    dev_sync="origin/dev not found"
elif [[ "$local_dev" == "$origin_dev" ]]; then
    dev_sync="in sync with origin/dev"
else
    dev_sync="DIFFERS from origin/dev"
fi

counts="$(git rev-list --left-right --count main...dev)"
main_only="$(cut -f1 <<<"$counts")"
dev_only="$(cut -f2 <<<"$counts")"

files="$(git diff --name-only main...dev)"
if [[ -z "$files" ]]; then
    file_count=0
else
    file_count="$(wc -l <<<"$files" | tr -d ' ')"
fi

# CodeRabbit's own path_filters (.coderabbit.yaml, reviews.path_filters)
# decide what it actually reviews — pull the exclude patterns (entries
# starting with "!") from there instead of hardcoding them, so this stays
# correct if that config changes. The 100-file cap is CodeRabbit's, so the
# PR-scoping count must exclude what CodeRabbit itself never looks at
# (generated code, recorded spike evidence, etc.) — otherwise the count
# includes files that don't count against the cap.
exclude_specs=()
if [[ -f .coderabbit.yaml ]] && command -v python3 >/dev/null 2>&1; then
    while IFS= read -r pattern; do
        exclude_specs+=(":!$pattern")
    done < <(python3 -c "
import yaml, sys
try:
    with open('.coderabbit.yaml') as f:
        cfg = yaml.safe_load(f) or {}
except Exception:
    sys.exit(0)
for p in (cfg.get('reviews') or {}).get('path_filters') or []:
    if isinstance(p, str) and p.startswith('!'):
        print(p[1:])
" 2>/dev/null)
fi

if [[ "${#exclude_specs[@]}" -gt 0 ]]; then
    reviewable_files="$(git diff --name-only main...dev -- . "${exclude_specs[@]}")"
else
    reviewable_files="$files"
fi
if [[ -z "$reviewable_files" ]]; then
    reviewable_count=0
else
    reviewable_count="$(wc -l <<<"$reviewable_files" | tr -d ' ')"
fi
pr_count=$(( (reviewable_count + 99) / 100 ))

echo "started-on: $start_branch"
echo "local dev: $dev_sync"
echo "dev ahead of main: $dev_only commits"
echo "main ahead of dev: $main_only commits"
echo "FILES REVIEWABLE BY CODERABBIT: $reviewable_count"
echo "PRs needed at 100-file cap: $pr_count"
echo "(files differing, raw total incl. excluded paths: $file_count)"

if [[ "$reviewable_count" -gt 0 ]]; then
    echo
    echo "--- by top-level path (reviewable only) ---"
    cut -d/ -f1 <<<"$reviewable_files" | sort | uniq -c | sort -rn
    echo
    echo "--- reviewable files ---"
    echo "$reviewable_files"
fi

excluded_count=$(( file_count - reviewable_count ))
if [[ "$excluded_count" -gt 0 ]]; then
    echo
    echo "--- excluded by .coderabbit.yaml path_filters ($excluded_count files, not shown) ---"
fi
