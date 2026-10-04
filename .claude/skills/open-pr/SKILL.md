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
  - Bash(git merge-tree *)
  - Bash(git merge --no-ff origin/main *)
  - Bash(git merge --abort)
  - Bash(git push origin dev)
  - Bash(scripts/gh-rest.sh *)
  - Bash(gh run list *)
  - Bash(gh run view *)
  - Bash(timeout * gh run watch *)
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
- **Never push on the maintainer's behalf**, with one exception. If local
  `dev` is ahead of `origin/dev`, stop and say so — don't push to make the
  PR "complete". (Under `orchestrate`'s push policy every landed commit
  already reaches `origin/dev` immediately, so this should be rare.) The one
  push this skill makes is step 4's back-merge of `main` into `dev`: a merge
  commit that brings no content, needed because the `Main` ruleset only
  merges a branch that is up to date with `main`, and every promotion
  leaves `main` one merge commit ahead of `dev`.
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
4. **Bring `dev` up to date with `main` — only when it is behind.**
   - `git rev-list --count origin/dev..origin/main` — if `0`, skip this
     whole step.
   - **Only a merge that brings no content is made here.** Under Q46 every
     change on `main` came from `dev`, so merging `main` back must leave
     `dev`'s tree exactly as it is:
     `git merge-tree --write-tree origin/dev origin/main` must succeed and
     print the same tree as `git rev-parse origin/dev^{tree}`. If it fails
     (a conflict) or prints a different tree, `main` holds something `dev`
     lacks — a hotfix or a hand edit. Stop, show
     `git log --oneline origin/dev..origin/main` and
     `git diff origin/dev origin/main --stat`, and leave it to the
     maintainer. Never resolve a conflict or merge real content here.
   - The current branch must be `dev` with a clean working tree
     (`git status`). Otherwise stop and say so — never switch branches or
     stash the maintainer's work.
   - `git merge --no-ff origin/main -m "Merge branch 'main' into dev"`.
     If it fails anyway, `git merge --abort` and stop.
   - `git push origin dev`. Never force. If the push is rejected (dev moved
     meanwhile, e.g. an `orchestrate` landing), stop and report it.
   - **Wait for CI on the merge commit before going on.** Find the run for
     the exact pushed SHA:
     `gh run list --repo mdg-labs/hoserva --branch dev --workflow CI --commit $(git rev-parse dev) --limit 1 --json databaseId,status,conclusion`
     — it can take a few seconds to appear; retry a bounded number of
     times (at most ~10 tries, a few seconds apart), never an open-ended
     loop. Then wait on it with
     `timeout 3600 gh run watch <id> --repo mdg-labs/hoserva --exit-status`
     run as a **background** Bash command, and end the turn; its exit
     re-invokes you. Never poll with `sleep` in the foreground.
   - **CI must pass.** If the run fails, is cancelled or times out, stop —
     don't create or update the PR. Report the run URL and the failing jobs
     (`gh run view <id> --repo mdg-labs/hoserva --log-failed`, bounded and
     filtered). Since the merge brought no content, a red run means `dev`
     itself is red, which is the thing to fix first.
5. Check for an existing open promotion PR:
   `scripts/gh-rest.sh pr-list --base main --head dev --state open`.
   If one exists, go on through steps 6 and 7 as usual, then in step 8
   **update** it (`scripts/gh-rest.sh pr-edit`) instead of creating a
   duplicate.
6. Gather the promotion's contents:
   - `git log --oneline origin/main..origin/dev` for the commit list.
   - `git log origin/main..origin/dev --format=%B` to pull every
     `Fixes #n` trailer, then `scripts/gh-rest.sh issue-view <n>
     --jq '{title,labels}'` for each to get titles and check for
     `safety-critical`.
   - `git diff --stat origin/main...origin/dev` for files/areas touched.
   - Cross-check touched issues/paths against CLAUDE.md's area→paths table
     and its `safety-critical` label — call out any safety-critical content
     explicitly, never bury it.
   - `gh run list --repo mdg-labs/hoserva --branch dev --workflow CI --commit $(git rev-parse origin/dev) --limit 1 --json status,conclusion,workflowName,headSha`
     for the CI result on the exact commit being promoted. If none exists
     for that SHA, or it isn't green, say so plainly — don't fall back to
     an older or unrelated run to make the PR look ready.
7. Draft:
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
     - `## CI status` — the latest `dev` run's result from step 6.
8. `scripts/gh-rest.sh pr-create --base main --head dev --title "..." --body-file <tmp>`
   (or `scripts/gh-rest.sh pr-edit <n> --title "..." --body-file <tmp>` if
   updating an existing one).
9. Report the PR URL, the closed-issue list, and the CI status. Stop there
   — no merge, no review request, no further action.
