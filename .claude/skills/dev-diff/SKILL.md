---
name: dev-diff
description: Fast-forwards local main from origin/main (without switching away from whatever branch is currently checked out) and reports how dev currently differs from it — commits ahead each direction, and the number and names of files that differ. Use for "how far ahead is dev", "diff main and dev", "how many files differ between dev and main".
argument-hint: (no arguments)
allowed-tools:
  - Bash(.claude/skills/dev-diff/dev-diff.sh)
---

# dev-diff

Read-only reporting. Runs `dev-diff.sh`, which fast-forwards local `main`
and reports how `dev` and `main` currently differ, scoped to what
CodeRabbit actually reviews (it reads `.coderabbit.yaml`'s
`reviews.path_filters` exclusions — generated code, recorded spike
evidence, etc. — and headlines the count that's left, since that's what
the 100-file cap applies to). Makes no commits, opens no PR, and never
leaves the maintainer on a different branch than the one they started on.

## Hard constraints

- Never `checkout`, `reset --hard`, `merge`, or anything `--force`. The
  script only ever fetches, ff-only-pulls/fetches `main`, and diffs — it
  never touches `dev`'s ref.
- Don't re-run the git commands the script already ran, and don't add your
  own extra `git diff`/`git log` calls on top of it — the script is the
  single source of truth for this report.

## Steps

1. Run `.claude/skills/dev-diff/dev-diff.sh` (no arguments).
2. If it exits non-zero (unclean working tree, or `main` diverged from
   `origin/main`), report the exact error line(s) it printed and stop —
   don't attempt a fix yourself.
3. If it printed "No local 'dev' branch exists", report that and stop.
4. Otherwise, relay its output **verbatim, unmodified** as the whole
   answer — headline the `FILES REVIEWABLE BY CODERABBIT` and `PRs needed
   at 100-file cap` lines since that's what scoping a PR depends on. Do not
   re-derive, re-summarize, or add narration/step-by-step commentary around
   it: the script's report already **is** the report.
