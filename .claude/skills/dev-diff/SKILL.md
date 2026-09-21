---
name: dev-diff
description: Fast-forwards local main from origin/main (without switching away from whatever branch is currently checked out) and reports how dev currently differs from it — commits ahead each direction, and the number and names of files that differ. Use for "how far ahead is dev", "diff main and dev", "how many files differ between dev and main".
argument-hint: (no arguments)
allowed-tools:
  - Bash(git status)
  - Bash(git fetch origin)
  - Bash(git fetch origin main:main)
  - Bash(git pull --ff-only origin main)
  - Bash(git rev-parse *)
  - Bash(git rev-list --left-right --count main...dev)
  - Bash(git diff --name-only main...dev)
  - Bash(git diff --stat main...dev)
---

# dev-diff

Read-only reporting. Updates local `main` and tells the maintainer how
`dev` and `main` currently differ. Makes no commits, opens no PR, and never
leaves the maintainer on a different branch than the one they started on.

## Hard constraints

- Never `checkout`, `reset --hard`, `merge`, or anything `--force`.
- Never touch `dev`'s ref, never push.
- If fast-forwarding local `main` isn't clean, stop and report the
  divergence — don't force it.

## Steps

1. `git status` — note the current branch (so it's clear nothing moved
   afterward) and confirm the working tree is clean before touching any ref.
2. `git fetch origin` for current remote state.
3. Update local `main`:
   - Current branch is `main` → `git pull --ff-only origin main`.
   - Otherwise (the common case, e.g. running this from `dev`) →
     `git fetch origin main:main`. If that's rejected as a non-fast-forward
     (local `main` diverged from `origin/main`), stop and report the
     divergence instead of forcing it.
4. Check whether local `dev` matches `origin/dev`
   (`git rev-parse dev` vs `git rev-parse origin/dev`); note any gap so
   the report is honest about what it's comparing.
5. Compute the diff between the now-updated local `main` and `dev`:
   - `git rev-list --left-right --count main...dev` — commits each side is
     ahead of their merge base.
   - `git diff --name-only main...dev | wc -l` — file count.
   - `git diff --stat main...dev` — full file list with +/- counts.
6. Report plainly:
   - How many commits `dev` is ahead of (and, if nonzero, behind) `main`.
   - How many files differ.
   - The file list — grouped by CLAUDE.md's area→paths table when that's
     more useful than a flat list, otherwise the raw `--stat` output.
   - Whether local `dev` matched `origin/dev` at diff time.
