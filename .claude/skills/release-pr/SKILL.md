---
name: release-pr
description: Opens (or updates) the beta→main promotion pull request with a conventional title and a description generated from the commits and issues on beta since main last moved. Use when the maintainer says "open a PR to main", "promote beta", "release beta to main", or similar. Never touches main directly and never merges — it only prepares and files the beta→main PR.
argument-hint: (no arguments — always operates on origin/beta → origin/main)
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

# release-pr

Opens the **beta → main** promotion pull request — per `CLAUDE.md` (Q46) and
the `orchestrate` skill, that PR is the *only* way `main` ever moves. This
skill prepares and files it (or updates one already open); it never merges
anything and never touches `main` itself. Merging is gated by required CI
checks and is the maintainer's call.

## Hard constraints

- **Base is always `main`, head is always `beta`.** Never the reverse.
- **Never merge.** No `gh pr merge`, no approving/requesting review on the
  maintainer's behalf.
- **Never push on the maintainer's behalf.** If local `beta` is ahead of
  `origin/beta`, stop and say so — don't push to make the PR "complete".
  (Under `orchestrate`'s push policy every landed commit already reaches
  `origin/beta` immediately, so this should be rare.)
- **Invoke-only.** Run this only when asked directly — opening a PR is a
  visible, shared-state action; being asked to run this skill *is* that
  request, so no extra confirmation is needed once invoked.

## Steps

1. `git fetch origin` for current refs.
2. `git rev-parse beta` vs `git rev-parse origin/beta` — if they differ,
   local `beta` has unpushed commits. Stop and tell the maintainer to push
   first rather than pushing it yourself.
3. `git rev-list --left-right --count origin/main...origin/beta` — if
   `beta` is 0 commits ahead, there is nothing to promote. Report that and
   stop.
4. Check for an existing open promotion PR:
   `gh pr list --repo mdg-labs/hoserva --base main --head beta --state open --json number,url,title`.
   If one exists, skip to step 7 and **update** it (`gh pr edit`) instead of
   creating a duplicate.
5. Gather the promotion's contents:
   - `git log --oneline origin/main..origin/beta` for the commit list.
   - `git log origin/main..origin/beta --format=%B` to pull every
     `Fixes #n` trailer, then `gh issue view <n> --json title,labels` for
     each to get titles and check for `safety-critical`.
   - `git diff --stat origin/main...origin/beta` for files/areas touched.
   - Cross-check touched issues/paths against CLAUDE.md's area→paths table
     and its `safety-critical` label — call out any safety-critical content
     explicitly, never bury it.
   - `gh run list --repo mdg-labs/hoserva --branch beta --workflow CI --commit $(git rev-parse origin/beta) --limit 1 --json status,conclusion,workflowName,headSha`
     for the CI result on the exact commit being promoted. If none exists
     for that SHA, or it isn't green, say so plainly — don't fall back to
     an older or unrelated run to make the PR look ready.
6. Draft:
   - **Title** (Conventional Commits shape): `chore(release): promote beta to main`,
     optionally with a parenthetical dominant-area note, e.g.
     `chore(release): promote beta to main (storage engine)`.
   - **Body** (write to a scratchpad temp file for `--body-file`):
     - `## Summary` — one or two sentences on what this promotion contains.
     - `## Issues closed` — bulleted `Fixes #n — <title>` list.
     - `## Changes by area` — bulleted, grouped by CLAUDE.md's area→paths table.
     - `## Safety-critical` — only if applicable; name the issue(s) and what makes them so.
     - `## CI status` — the latest `beta` run's result from step 5.
7. `gh pr create --repo mdg-labs/hoserva --base main --head beta --title "..." --body-file <tmp>`
   (or `gh pr edit <n> --title "..." --body-file <tmp>` if updating an
   existing one).
8. Report the PR URL, the closed-issue list, and the CI status. Stop there
   — no merge, no review request, no further action.
