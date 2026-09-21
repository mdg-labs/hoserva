---
name: beta-diff
description: Fast-forwards local main from origin/main (without switching away from whatever branch is currently checked out) and reports how beta currently differs from it — commits ahead each direction, and the number and names of files that differ. Use for "how far ahead is beta", "diff main and beta", "how many files differ between beta and main".
argument-hint: (no arguments)
allowed-tools:
  - Bash(git fetch *)
  - Bash(git status)
  - Bash(git branch *)
  - Bash(git symbolic-ref *)
  - Bash(git rev-parse *)
  - Bash(git rev-list *)
  - Bash(git diff *)
  - Bash(git pull *)
  - Bash(git log *)
---

# beta-diff

Read-only reporting. Updates local `main` and tells the maintainer how
`beta` and `main` currently differ. Makes no commits, opens no PR, and never
leaves the maintainer on a different branch than the one they started on.

## Hard constraints

- Never `checkout`, `reset --hard`, `merge`, or anything `--force`.
- Never touch `beta`'s ref, never push.
- If fast-forwarding local `main` isn't clean, stop and report the
  divergence — don't force it.

## Steps

1. `git status` — note the current branch (so it's clear nothing moved
   afterward) and confirm the working tree is clean before touching any ref.
2. `git fetch origin` for current remote state.
3. Update local `main`:
   - Current branch is `main` → `git pull --ff-only origin main`.
   - Otherwise (the common case, e.g. running this from `beta`) →
     `git fetch origin main:main`. If that's rejected as a non-fast-forward
     (local `main` diverged from `origin/main`), stop and report the
     divergence instead of forcing it.
4. Check whether local `beta` matches `origin/beta`
   (`git rev-parse beta` vs `git rev-parse origin/beta`); note any gap so
   the report is honest about what it's comparing.
5. Compute the diff between the now-updated local `main` and `beta`:
   - `git rev-list --left-right --count main...beta` — commits each side is
     ahead of their merge base.
   - `git diff --name-only main...beta | wc -l` — file count.
   - `git diff --stat main...beta` — full file list with +/- counts.
6. Report plainly:
   - How many commits `beta` is ahead of (and, if nonzero, behind) `main`.
   - How many files differ.
   - The file list — grouped by CLAUDE.md's area→paths table when that's
     more useful than a flat list, otherwise the raw `--stat` output.
   - Whether local `beta` matched `origin/beta` at diff time.
