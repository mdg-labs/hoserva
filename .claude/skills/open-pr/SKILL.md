---
name: open-pr
description: Opens (or updates) the dev→main promotion pull request, titled for what actually changed (never "release"/"promote" framing — this project doesn't cut a release here, it's a branch promotion), with a description generated from the commits and issues on dev since main last moved. Use when the maintainer says "open a PR to main", "promote dev", "release dev to main", or similar. Never touches main directly and never merges — it only prepares and files the dev→main PR.
argument-hint: (no arguments — always operates on origin/dev → origin/main)
allowed-tools:
  - Read
  - Write
  - Bash(git fetch *)
  - Bash(git status)
  - Bash(git branch *)
  - Bash(git log *)
  - Bash(git diff *)
  - Bash(git rev-list *)
  - Bash(git rev-parse *)
  - Bash(gh pr list *)
  - Bash(gh pr view *)
  - Bash(gh pr create *)
  - Bash(gh pr edit *)
  - Bash(gh issue view *)
  - Bash(gh repo view *)
  - Bash(gh run list *)
  - AskUserQuestion
---

# open-pr

Opens the **dev → main** promotion pull request — per `CLAUDE.md` (Q46) and
the `orchestrate` skill, that PR is the *only* way `main` ever moves. This
skill prepares and files it (or updates one already open); it never merges
anything and never touches `main` itself. Merging is gated by required CI
checks and is the maintainer's call.

## Hard constraints

- **Base is always `main`, head is always `dev`.** Never the reverse.
- **Never merge.** No `gh pr merge`, no approving/requesting review on the
  maintainer's behalf.
- **Never push on the maintainer's behalf.** If local `dev` is ahead of
  `origin/dev`, stop and say so — don't push to make the PR "complete".
  (Under `orchestrate`'s push policy every landed commit already reaches
  `origin/dev` immediately, so this should be rare.)
- **Invoke-only.** Run this only when asked directly — opening a PR is a
  visible, shared-state action; being asked to run this skill *is* that
  request, so no extra confirmation is needed once invoked.

## Steps

1. `git fetch origin` for current refs.
2. `git rev-parse dev` vs `git rev-parse origin/dev` — if they differ,
   local `dev` has unpushed commits. Stop and tell the maintainer to push
   first rather than pushing it yourself.
3. `git rev-list --left-right --count origin/main...origin/dev` — if
   `dev` is 0 commits ahead, there is nothing to promote. Report that and
   stop.
4. Check for an existing open promotion PR:
   `gh pr list --repo mdg-labs/hoserva --base main --head dev --state open --json number,url,title`.
   If one exists, skip to step 7 and **update** it (`gh pr edit`) instead of
   creating a duplicate.
5. Gather the promotion's contents:
   - `git log --oneline origin/main..origin/dev` for the commit list.
   - `git log origin/main..origin/dev --format=%B` to pull every
     `Fixes #n` trailer, then `gh issue view <n> --json title,labels` for
     each to get titles and check for `safety-critical`.
   - `git diff --stat origin/main...origin/dev` for files/areas touched.
   - Cross-check touched issues/paths against CLAUDE.md's area→paths table
     and its `safety-critical` label — call out any safety-critical content
     explicitly, never bury it.
   - `gh run list --repo mdg-labs/hoserva --branch dev --workflow CI --commit $(git rev-parse origin/dev) --limit 1 --json status,conclusion,workflowName,headSha`
     for the CI result on the exact commit being promoted. If none exists
     for that SHA, or it isn't green, say so plainly — don't fall back to
     an older or unrelated run to make the PR look ready.
6. Draft:
   - **Title** describes what changed, never that a promotion is happening —
     GitHub already shows this is a dev→main PR, and this project has no
     "release" event at this step (that's the separate, later, tag-triggered
     release.yml). Never `chore(release): promote dev to main` or any
     "release"/"promote" framing.
     - One commit clearly dominates (e.g. the only `safety-critical` one,
       or the only non-chore one): reuse its own Conventional Commits
       subject line verbatim, e.g. `fix(shares): close TOCTOU race in
       DeleteFile`.
     - Several commits are comparably significant: pick the single most
       consequential one as the base subject and note the rest are
       included in the body, not the title — don't try to cram every
       commit into one title.
   - **Body** (write to a scratchpad temp file for `--body-file`):
     - `## Summary` — one or two sentences on what this promotion contains.
     - `## Issues closed` — bulleted `Fixes #n — <title>` list.
     - `## Changes by area` — bulleted, grouped by CLAUDE.md's area→paths table.
     - `## Safety-critical` — only if applicable; name the issue(s) and what makes them so.
     - `## CI status` — the latest `dev` run's result from step 5.
7. `gh pr create --repo mdg-labs/hoserva --base main --head dev --title "..." --body-file <tmp>`
   (or `gh pr edit <n> --title "..." --body-file <tmp>` if updating an
   existing one).
8. Report the PR URL, the closed-issue list, and the CI status. Stop there
   — no merge, no review request, no further action.
