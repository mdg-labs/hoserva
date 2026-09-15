---
name: github-triage
description: Enrich an existing GitHub issue or draft new ones from a raw report (including seeding a roadmap phase as an epic with sub-issues), using `gh` CLI only. Use when the user gives an issue number to clean up/enrich, or a raw bug/feature/spike report to turn into a well-structured issue. Never edits local files — read-only against the repo, all writes go through `gh issue edit`/`gh issue create`/`scripts/issue-status.sh`.
argument-hint: <issue-number> | <free-form report text>
allowed-tools:
  - Read
  - Grep
  - Glob
  - AskUserQuestion
  - Bash(gh issue view *)
  - Bash(gh issue list *)
  - Bash(gh issue edit *)
  - Bash(gh issue create *)
  - Bash(gh pr view *)
  - Bash(gh pr list *)
  - Bash(gh repo view *)
  - Bash(gh label list *)
  - Bash(gh api *)
  - Bash(git log *)
  - Bash(git show *)
  - Bash(git blame *)
  - Bash(git diff *)
  - Bash(git status)
  - Bash(git grep *)
  - Bash(git clone *)
  - Bash(scripts/issue-status.sh *)
---

# github-triage

Turns a rough issue into a well-structured one — either by enriching an
existing GitHub issue on `mdg-labs/hoserva` or drafting new ones from a raw
report — using the `gh` CLI for every GitHub-side effect.

**Hard constraint: this skill is read-only against the local repository.**
It never uses `Edit`, `Write`, or `NotebookEdit`, and never runs a `git`
command that mutates this repo's tracked content or state (no `commit`,
`add`, `push`, `branch`, `checkout`, `reset`, …) or a `gh` command that
touches anything but issues. The one exception is `git clone --depth 1` of
an external reference repo into `../reference/<name>`, *outside* this repo
(see "Consult external context"). Temp files for `--body-file` go in the
scratchpad directory. If a task seems to require touching code or docs,
stop and say so — that's `orchestrate`'s job, not this skill's.

## Determine the mode

Look at `$ARGUMENTS` (or the user's message):

- **A bare issue number, or "issue #N" / "issue N"** → **Enrich mode** on that issue.
- **Anything else** (free-form bug/feature/spike report, or "seed Phase N") → **Create mode** from that text.
- **Nothing usable in either shape** → ask once via `AskUserQuestion`: enrich an existing issue (get the number) or create from a report (get the text).

## Investigate (both modes)

Hoserva is design-first: for most of its early life the design docs *are*
the codebase. Ground every issue in them.

1. `gh repo view mdg-labs/hoserva --json nameWithOwner,defaultBranchRef` to confirm the target.
2. **Read the design docs the report touches** — `CLAUDE.md`'s documentation map says which. Note the exact sections (`doc 02 §2`) the issue implements or changes.
3. **Check the decision log and the open questions.**
   - Conflicts with a decision (`D1`–`D18`, doc 00 §5)? Say so in `## Constraints`; the issue does not quietly reopen it. A genuine new reason to reopen one becomes its own `docs` issue.
   - Touches an open question (`Qn`, doc 13)? The issue follows the recommended default and names it. If investigation shows the default is wrong, the issue proposes the change *and* its acceptance criteria include updating the doc 13 entry.
4. Grep/Read any code, scripts or workflows the report mentions; `git log`/`git blame`/`git show` for recent history on them.
5. `gh issue list --repo mdg-labs/hoserva --state all --search ...` for related or duplicate issues.

Keep this proportional — a typo needs none of it; a vague storage bug needs all of it.

## Consult external context (when relevant)

Hoserva orchestrates other projects rather than reimplementing them (D1), so
upstream behaviour is often the real answer. Only reach for a source the
issue actually touches; clone into `../reference/<name>` with `git clone
--depth 1 <url>` if missing, never speculatively:

- **mergerfs** (`github.com/trapexit/mergerfs`) — create policies, branch modes, caching options, mount behaviour.
- **SnapRAID** (`github.com/amadvance/snapraid`, manual at `snapraid.it/manual`) — sync/diff/fix semantics, content files, parity layout.
- **Unraid Community Applications** (`github.com/Squidly271/AppFeed`, `github.com/Squidly271/Community-Applications-Moderators`) — template format, moderation data. **Read only; never copy templates into this repo** (doc 04 §4, doc 06 §2).
- **Comparable projects** (OpenMediaVault, TrueNAS, Dockge, …) when the report names one or is clearly "how do others solve X".

Read for **behaviour and intent**, never to transcribe code. When any of
these applies, the body gains a short `## Upstream / reference context`
section with what was found, at which version or commit.

## Open questions get a recommended default, not a stalled thread

This is the same rule doc 13 is built on. Never leave an `## Open questions`
item as a bare question. Investigate, then commit to a recommended default:
the question, the default, and a one-line rationale. Where doc 13 already
has an entry, cite it (`Q12`) instead of re-deriving it. The issue still
surfaces the question to override later, but triage always lands on a
concrete, sane default. `AskUserQuestion` is available; prefer the default
rule over asking.

This skill is **invoke-only** — no workflow triggers it. It runs here when
the maintainer wants it.

## Labels

Every issue gets, per `CLAUDE.md` ("Label set"):

- **Exactly one type**: `feat`, `bug`, `chore`, `docs`, `spike`
- **One `area:*`** where one applies (see `CLAUDE.md`'s area → paths table)
- **Extras when true:**
  - `epic` on an epic
  - `safety-critical` when the work touches the threshold guard, the mover/relocation delete path, the migration import, or `packaging/` — or anything else where a plausible bug loses data
  - `needs-hardware` when acceptance requires the L4 hardware box or real disks (spindown on real drives, SMART across controllers, thermals)
  - `needs-sudo` when the work requires root on the host
  - `blocked` only for an external blocker that isn't expressible as a native blocked-by relationship

`gh label list --repo mdg-labs/hoserva` shows what exists. Never apply a
`status:*` label directly — see below.

## Issue body shape

- `## Original report` — the reporter's text, verbatim (enrich mode: the existing body and relevant comments; create mode: the user's text).
- `## Summary` — one or two sentences on what this actually is, once investigated.
- `## Design references` — the doc sections and `D`/`Q` numbers it implements or touches.
- Then, as warranted: `## Reproduction`, `## Root cause / relevant code`, `## Upstream / reference context`, `## Proposed approach`, `## Constraints`, `## Acceptance criteria`, `## Open questions`. Don't force sections that don't apply.
- **Acceptance criteria are checkable.** For storage work, name the loop-harness test that proves it (doc 06 §3, doc 09 §6); for `safety-critical` work, the data-loss scenario the test reproduces. For a `spike`, the deliverable is recorded findings (`CLAUDE.md`, "Spikes"): what is measured, the kill or pass criterion (doc 07 §1), and which doc 13 entries it confirms or overturns.
- **Scope hint.** Name the top-level paths the work will touch in backticks (`internal/parity/`, `docs/internal/`), so `orchestrate` can bound it.

## Epic/sub-issue structure and dependencies — native relationships, never body prose

When triage produces more than one issue — an epic with sub-issues, or an
issue that depends on another tracked issue — wire the relationship through
GitHub's native fields. **Never** as body prose ("Part of #N", "Depends on
#N"): that is a second, driftable copy of a fact GitHub already tracks.

- **Epic → sub-issue:** `gh issue edit <epic> --repo mdg-labs/hoserva --add-sub-issue <n>` (or `--parent <epic>` on the child). Verify with `gh issue view <epic> --json subIssuesSummary`.
- **Blocking dependency:** `gh issue edit <n> --repo mdg-labs/hoserva --add-blocked-by <dep>`. Verify with `gh issue view <n> --json blockedBy,blocking`.
- Needs `gh` ≥ 2.100.
- The body may *explain* why an ordering exists; it is never the only record that it does.
- These calls happen **after** every issue in the relationship exists — create first, link second.

## Mode 1 — Enrich an existing issue

1. `gh issue view <n> --repo mdg-labs/hoserva --json number,title,body,labels,comments,url,state,assignees` and `gh issue view <n> --repo mdg-labs/hoserva --comments`.
2. Investigate as above, starting from the body and comments (comments override the body where they disagree).
3. Rewrite title and body in the shape above. If this is really an epic, or depends on / blocks another issue, decide that now; wire it in 4a.
4. `gh issue edit <n> --repo mdg-labs/hoserva --title "..." --body-file <tmpfile>`, plus `--add-label`/`--remove-label` for type, area and extras (never `status:*`).
   - **4a.** Wire any epic/sub-issue or blocking relationship via the native flags, once every issue involved exists.
5. `scripts/issue-status.sh <n> ready` — an enriched issue can be picked up. Skip for a closed issue.
6. Report the issue URL and a short summary of what was added, including any doc 13 defaults applied or challenged.

## Mode 2 — Create issues from a report

1. Investigate as above, starting from the raw report.
2. Draft title + body in the shape above. If the report is more than one piece of work, decide the epic/sub-issue split here.
   - **Seeding a phase** ("seed Phase 0"): don't hand-create it. Phases are already broken into epics and sub-issues in `docs/roadmap.md`; run `scripts/roadmap-sync.py <ids>` (dry run) and hand the maintainer the `--apply` command. Triage then enriches the created issues one by one (Mode 1). Work that isn't in the roadmap yet is created here as usual.
   - **Mass-creation guard:** if this would create more than ~12 issues, state the count and list the titles, and confirm via `AskUserQuestion` before creating anything.
3. `gh issue create --repo mdg-labs/hoserva --title "..." --body-file <tmpfile> --label ...` — epic first, then each sub-issue, so every number a relationship needs exists.
4. Wire relationships via the native flags. If a body referenced another issue's number before it existed, patch it in with `gh issue edit --body-file` now — no placeholders left behind.
5. `scripts/issue-status.sh <number> ready` for every issue created. The `issue-status` workflow stamps new issues `status:new`; one this skill created was enriched at birth.
6. Report every new issue's URL.

## Non-negotiables

- Original report text is never paraphrased away — it is preserved verbatim in its own section.
- No local file in this repo is created, modified, or deleted. Temp files go in the scratchpad directory.
- No git commits, branches, stashes, or pushes.
- Every GitHub-side write goes through `gh issue edit`, `gh issue create`, or `scripts/issue-status.sh`, always with `--repo mdg-labs/hoserva` where `gh` takes it.
- **Exactly one `status:*` label per issue, always**, set only by `scripts/issue-status.sh`.
- **Epic/sub-issue and blocking relationships are native GitHub fields, never body prose.**
- **Every open question lands on a recommended default**, citing doc 13 where an entry exists.
- **An issue never silently contradicts a decision (`Dn`) or a doc 13 default** — it names the conflict and, for a default, includes updating doc 13 in its acceptance criteria.
