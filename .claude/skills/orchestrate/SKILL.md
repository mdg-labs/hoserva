---
name: orchestrate
description: Given a GitHub issue number (a single item, or an epic with sub-issues), autonomously implement and verify the work — sonnet execution agents in isolated scratch clones (one per item, or one per bundle of small, correlated items), one independent verifier per attempt, landing on local main only after a PASS. Parallelizes items with disjoint file scope, serializes overlapping ones. Never pushes. Use when asked to "work on issue #n", "implement epic #n", "run the orchestrator", or "orchestrate #n".
argument-hint: <issue-number>
allowed-tools:
  - Read
  - Grep
  - Glob
  - Agent
  - AskUserQuestion
  - Bash
---

# orchestrate

Turns one GitHub issue on `mdg-labs/hoserva` — a single item, or an epic
with sub-issues — into landed, verified commits on local `main`, with no
human in the loop except at a genuine blocker (a `needs-sudo` step, a
repeated verification failure, or an external unmet dependency). **Nothing is pushed**: the maintainer reads and pushes.

**You (the current session) are the orchestrator.** You spawn
`task-executor` and `task-verifier` subagents and drive the loop yourself —
this skill is not itself a subagent. Follow the steps in order. An epic
takes a while; give the user a short progress update at the start of each
wave and after each landed commit, rather than going quiet.

## The hazard this project adds: real disks

Hoserva formats disks and computes parity; its whole test strategy exists so
that development never touches a real one (`CLAUDE.md`, "Real disks are
off-limits"; doc 06 §3). The development host's own disk is the
maintainer's system. Every dispatch you write carries these rules — they are
baked into both templates; **never soften them when filling one in**:

- **Storage behaviour runs only inside the loop-device lab**, started with
  `make lab-up` under `HOSERVA_LAB_ID=<unit-id>` and destroyed before the
  agent reports. The lab container exposes loop devices and FUSE only. No
  `mkfs`, `wipefs`, `mount`, `losetup`, `mergerfs`, `snapraid`, `sgdisk`,
  `dd of=/dev/…` on the host, ever. Never `losetup -D`.
- **Until `scripts/devenv/` and the `lab-*` Makefile targets exist, no storage
  command runs anywhere.** An issue whose acceptance needs one is blocked on
  the foundation issue — the agent reports `blocked`, it does not improvise
  a lab.
- **Docker is root-equivalent here.** Agents use it only through `make`
  targets and for read-only linter containers with their workspace mounted
  read-only. They never stop, remove or prune anything they did not create.
- **VMs run only through the `vm-*` targets under `qemu:///session`**, with
  images in the agent's workspace and domain names carrying the lab id. Never
  `qemu:///system`; never touch a domain an agent did not create — the
  maintainer has VMs of their own on this host.
- **No agent connects to the maintainer's homelab or Unraid server**, not even
  read-only, and nothing is handed to the maintainer to test (D20).
- **No `sudo`, no package installs, no writes under `/etc`, `/var/lib/hoserva`,
  `/run/hoserva` or `/mnt`, no host systemd units, no `.deb` installed on the host.**
- **Kill by PID only** — never `pkill`/`killall`/pattern kills. (A sibling
  project lost the maintainer's live session to a `pkill -f … --oldest`.)
- **Every command is bounded** — an explicit timeout on anything not
  obviously fast, no recursive scan rooted at `/`, and no background command
  left running when a dispatch ends. (A sibling project had an unscoped
  `find /` outlive its dispatch by an hour.)

**Verify the machine at the start of a run, then write what you read into
the dispatch — never copy a claim from this file.** One Bash call:

```
cd <real repo> && ls Makefile scripts/devenv 2>&1; grep -E '^lab-(up|destroy):' Makefile 2>&1
docker info --format '{{.ServerVersion}}' 2>&1 | head -1
command -v go node npm shellcheck golangci-lint actionlint 2>&1
docker ps --filter name=hoserva-lab- --format '{{.Names}}' 2>&1
losetup -a 2>&1 | wc -l
ls -l /dev/kvm 2>&1; command -v qemu-system-x86_64 qemu-img 2>&1; grep -E '^vm-(up|destroy):' Makefile 2>&1
virsh -c qemu:///session list --all --name 2>&1
```

That tells you whether the lab exists (`LAB_AVAILABLE`), whether Docker is
reachable by this user, which checkers are installed, and whether a stale
lab from an earlier run is still up — and whether the VM harness exists
(`VM_AVAILABLE`) and which session VMs already exist (report them, never touch
them). A stale `hoserva-lab-*` container from a
previous run is reported to the user, not removed by you.

## 0. Resolve the target

```
gh issue view <N> --repo mdg-labs/hoserva --json number,title,body,labels,state
```

- **Not labelled `epic`:** the target set is just this one issue.
- **Labelled `epic`:**
  ```
  gh api repos/mdg-labs/hoserva/issues/<N>/sub_issues --paginate --jq '.[] | {number,title,state,labels:[.labels[].name]}'
  ```
  Drop any sub-issue already closed. This endpoint spells state
  **lowercase** (`"open"`), unlike `gh issue view --json state` (`"OPEN"`);
  filter with `(.state | ascii_downcase) == "open"`.
- If the issue doesn't exist or `gh` fails, stop and say so — don't guess a number.

Call the resulting set of open issue numbers **T**.

## 1. Pull each issue's full body, comments, and relationships

For every issue in T — genuinely two calls, `gh` rejects `--comments` with `--json`:

```
gh issue view <n> --repo mdg-labs/hoserva --json number,title,body,labels
gh issue view <n> --repo mdg-labs/hoserva --comments
```

**Comments are authoritative over the body where they disagree** — scope
corrections, reassigned halves, replaced acceptance criteria, and prior
verification findings all land as comments. Fold what you learn into your
own decisions (scope, ordering, whether it still belongs in T) and into the
dispatch; never assume the agent will rediscover it.

Then the native relationships:

```
gh issue view <n> --repo mdg-labs/hoserva --json blockedBy,blocking,parent,subIssues
```

For each `d` in `blockedBy` (uppercase state here):
- `CLOSED` → satisfied.
- `OPEN` and in T → an intra-run ordering edge.
- `OPEN` and not in T → **external blocker.** Remove the issue from T and report: `#<n> is blocked by open #<d>, which is outside this run — orchestrate #<d> first, or include it explicitly`.

`parent` is the issue's epic, if any.

Also read the design context each issue cites (`## Design references`) —
you are about to judge its scope, and the docs are where scope lives.

## 2. Pull out `needs-sudo` issues — they never go through an agent

No agent may run root commands. For each such issue in T:

1. Read it yourself. Stage whatever files it describes in the real repo and
   print the exact commands for the maintainer, prefixed `! ` and
   fish-compatible.
2. Do **not** commit, do **not** close. Report it as "prepared, awaiting
   maintainer" and remove it from T — its dependents stay blocked until the
   maintainer confirms and you're re-invoked.

**There is no hardware tier.** Every test — spindown, SMART, Unraid
adoption, passthrough included — runs in the loop-device lab or an
agent-started VM (doc 06). The maintainer runs no tests and no agent ever
connects to the maintainer's homelab. An issue whose acceptance seems to
need physical disks is mis-scoped: send it back through `github-triage`,
never to the maintainer.

## 3. Determine each remaining issue's file scope

For each issue in T, derive the set of top-level paths it will touch:

- Backtick-quoted paths in the body (`internal/parity/`, `docs/internal/`, `scripts/devenv/`, `.github/workflows/`, …).
- Fallback: its `area:*` label, via `CLAUDE.md`'s **area → paths** table. A `docs` issue with no area scopes to `docs/internal/`.
- **Always-shared files** (`CLAUDE.md`'s list — `CLAUDE.md`, `Makefile`, `go.mod`/`go.sum`/`go.work`, `api/openapi.yaml`, `api/gen/`, `web/package.json` + lockfile, `docs/internal/13-open-questions.md`, `.gitignore`, `LICENSE`) are their own scope entries whenever an issue plausibly touches them. Any API change touches `api/openapi.yaml` and `api/gen/`; any spike or default change touches doc 13.
- Can't confidently bound it → its scope is **the whole repo**, which serializes it against everything.

## 4. Batch into waves, then bundles, then lanes

**Waves** (dependency order): wave 1 = issues with no unresolved same-run
dependency; wave 2 = issues whose same-run dependencies are all in wave 1;
and so on.

### Bundles

One `task-executor` per **bundle**; a bundle is usually one issue. Two shapes qualify, nothing else:

- **Correlated** — scopes intersect, so lanes would serialize them anyway.
- **Small and adjacent** — each is a one-sitting change sharing an `area:*`, with nothing open in its thread that needs a decision.

A dependency edge between two members is a reason to bundle: order parent
first, delete that edge, re-layer the waves.

**Never bundle:**

- an issue scoped "the whole repo";
- an issue whose thread carries a verification FAIL, or that is entering a fix round;
- a `safety-critical` issue — it gets its own agent, its own verifier, and its own line in the report;
- a `spike` — findings deserve an agent's undivided attention;
- anything `needs-sudo` (already removed).

Cap a bundle at **3 issues**. Its scope is the union of its members'. Every commit stays one issue.

### Lanes within a wave

Process the wave's units (a bundle is one unit, ordered by its lowest
member) in issue-number order; place each in the first lane whose
accumulated scope doesn't intersect its own, else start a new lane. Lanes
run in parallel; units within a lane run serially.

Print the plan before dispatching — waves, bundles and why, lanes and why.
If T has more than ~12 issues, state the count and confirm via
`AskUserQuestion` first.

## 5. Per dispatch unit: isolated scratch clone

Never work in the real repo; never share a clone between concurrent units.

```
mkdir -p <scratchpad dir>/orchestrate
git clone <real repo path> <scratchpad dir>/orchestrate/<unit-id>-a1
```

`<unit-id>` is the issue number (`57-a1`) or bundle members joined with `+`
(`57+58-a1`). **The lab id is derived from it**: `HOSERVA_LAB_ID=<unit-id>`
with `+` replaced by `-` (`57-58-a1`), so parallel lanes never share loop
devices, mount roots or container names.

A **verification-FAIL retry** (step 9) reuses the same clone so the executor
can amend. Only a **rebase retry** (step 8) or a fix round after a bundled
attempt re-clones fresh.

## Status labels — exactly one, always

Every issue carries exactly **one** `status:*` label. Everything goes through:

```
scripts/issue-status.sh <issue-number> <status>
```

You run it from the real repo; agents run their clone's copy. The script
targets `mdg-labs/hoserva` explicitly, because `gh` cannot infer a repo from
a clone whose `origin` is a local path.

| Transition | Set by | When |
|---|---|---|
| → `status:new` | `issue-status` workflow | opened / reopened |
| → `status:ready` | `github-triage` | enriched |
| → `status:in-progress` | `task-executor` | before it starts an issue |
| → `status:in-review` | `task-executor` | after that issue's commit |
| → `status:implemented` | `task-verifier` | PASS |
| → `status:in-progress` | `task-verifier` | FAIL |
| → `status:closed` / `status:cancelled` | `issue-status` workflow | closed |

An **epic**'s status is rolled up by `scripts/epic-status.sh <epic>`, which
the executor runs after claiming a sub-issue and the verifier after each
verdict. Pass the epic as `{{EPIC_NUMBER}}`; omit that block for an issue
with no epic.

- **You never set `status:closed`.** The trailer closes the issue when the maintainer pushes; the workflow labels it.
- **You own the abandonment transitions:** an issue leaving your hands still open (executor `blocked`, or escalated after three FAILs) goes back to `status:ready`.

## 6. Dispatch `task-executor`

Read `.claude/skills/orchestrate/templates/executor-prompt.md` and fill every
`{{…}}` token: the shared preamble once (workspace, lab id, what step 0 found
about the machine), then the per-issue block once per issue in bundle order —
number, title, body, **comment thread**, scope, and whether it is
`safety-critical` or a `spike`. On a fix round, the rejected SHA and the
verifier's findings verbatim.

```
Agent({
  subagent_type: "task-executor",
  model: "sonnet",
  description: "Implement <unit-id>",
  prompt: <the filled template>
})
```

**All lane-head dispatches for a wave go in one assistant message**, so they
run concurrently.

A bundle produces **one commit per issue**, each with only that issue's files
and its own `Fixes #<n>` trailer. Blocked is per issue: a bundle that
committed issue 1 and blocked on issue 2 hands you a real commit for issue 1.

If an issue comes back `blocked`: report the reason, put it back to
`status:ready`, leave it open, and continue with the rest of T that doesn't
depend on it.

### Waiting: end the turn, don't schedule anything

Subagents re-invoke you when they finish. Once everything dispatchable is
out the door, say in one line what you're waiting on and **end your turn.**
Never `ScheduleWakeup`, `Monitor`, or `sleep`; never re-dispatch because you
haven't heard back. When a notification arrives, verify that unit (step 7)
and dispatch the next unit in its lane.

## 7. Dispatch `task-verifier`

Read `.claude/skills/orchestrate/templates/verifier-prompt.md` and fill it:
per issue, its details, the same comment thread, its scope, its flags, and
**its own commit SHA**; once, the workspace, lab id, attempt number and epic.

```
Agent({
  subagent_type: "task-verifier",
  model: "opus",      // when any issue in the unit is safety-critical
  model: "sonnet",    // otherwise
  description: "Verify <unit-id> attempt <n>",
  prompt: <the filled template>
})
```

**One verifier per unit per attempt**, verdicts **per issue**: all six layers
run against each commit separately, one comment and one label move per
issue. A mixed PASS/FAIL result is normal. The verifier posts its own
comments and moves its own labels; read its returned verdicts rather than
re-deriving them from GitHub.

## 8. On PASS — land, sequentially, never in parallel

Land one commit at a time, in bundle order, skipping members that FAILed:

```
git fetch <scratch workspace path> <sha>
git cherry-pick -n FETCH_HEAD
```

- **Cherry-pick succeeds.** Decide whether this is its epic's last open
  sub-issue — by membership, not by count:
  ```
  gh api repos/mdg-labs/hoserva/issues/<epic>/sub_issues --paginate \
    --jq '[.[] | select((.state | ascii_downcase) == "open") | .number]'
  git log origin/main..HEAD --pretty=%B | grep -oiE '(fixes|closes) #[0-9]+' | grep -oE '[0-9]+' | sort -un
  ```
  Subtract the second list (closed by unpushed trailers) from the first. It
  is the last item **only if exactly `{<this issue>}` remains**. Keep
  `ascii_downcase`: without it the filter matches nothing and every landing
  would close the epic.

  Commit with the executor's message, adding `Fixes #<epic>` only if it is
  really the last:
  ```
  git commit -m "$(cat <<'EOF'
  <the executor's own commit message>

  Fixes #<issue-number>
  [Fixes #<epic-number>]
  EOF
  )"
  ```
  You may change only the message, never the diff. Never push.

  Once **every** issue in the unit is resolved: confirm its lab is gone
  (`docker ps --filter name=hoserva-lab-<lab-id>` empty and no `<clone>/.lab/`
  left; if either remains, run `make -C <clone> lab-destroy HOSERVA_LAB_ID=<lab-id>`),
  then delete the clone. Lab files are root-owned, so a clone with a leftover
  `.lab/` can't be deleted by your user — `lab-destroy` removes it from inside
  the container. Never reach for `sudo rm`; report a clone you couldn't delete.

- **Cherry-pick conflicts** (a wrong scope prediction): `git cherry-pick
  --abort`. A mechanical rebase problem, not a rejected implementation —
  doesn't spend a fix attempt. Destroy the unit's lab, delete the clone,
  re-clone fresh from current `main`, redispatch the same issue with a
  one-line "rebase re-run" note. Cap at 2 rebase retries, then escalate as
  in step 9.

## 9. On FAIL — fix, then re-verify, capped at 3 attempts

**A fix round is always single-issue** — a FAIL dissolves its unit.

- **After a single-issue attempt:** a fresh `task-executor` in the **same
  clone**, template branch `FIX_ROUND_SAME_WORKSPACE`, with the rejected SHA
  and the findings verbatim. It amends; the workspace stays one commit ahead
  of `main`.
- **After a bundled attempt:** re-clone fresh from current `main` (its
  passing siblings have landed), template branch `FIX_ROUND_FRESH_CLONE`,
  with `PRIOR_ATTEMPT_PATH`/`PRIOR_COMMIT_PATH` pointing at the old bundle
  workspace, read-only. A normal new commit.

The attempt counter carries over. Dispatch a fresh verifier against the new
SHA; it re-runs every layer regardless of how little changed.

If attempt 3 also fails: stop. Put the issue back to `status:ready`, then
`AskUserQuestion` with the latest findings — keep trying / hand it to the
maintainer / skip for now. Destroy its lab and delete its clone.

## 10. Repeat until T is empty

Move to the next wave once every issue in the current one has landed, been
skipped as blocked, or been escalated.

## 11. File what the run surfaced but didn't own

Collect every **Findings outside this issue** from executor reports and
verification comments. For each:

- **Covered by an open issue?** Note the number, don't duplicate.
- **In scope for something still open in T's chain?** Say so and leave it.
- **Genuinely new work?** File it via `github-triage` (create mode), which labels it and sets `status:ready`.
- **A finding that a doc 13 default is wrong?** Always an issue (type `docs`, touching doc 13) — never a silent doc edit.
- **Trivially small** (a one-line doc correction) and reachable: fix it directly on `main` as its own commit, no issue.

Record the work; don't do it. Never fold a finding into an unrelated landing commit.

## 12. Compose the report

- What landed (issue → commit SHA → one line)
- **Safety-critical commits — read line by line before pushing** (their own list, even if empty: "none this run")
- What's blocked and why (`needs-sudo` prepared, external dependency, lab not yet available, escalated after 3 FAILs)
- Bundles and why
- What step 11 filed or fixed
- Any stale lab containers or loop devices step 0 found
- **Nothing was pushed.** When satisfied: `git push origin main`

Don't send it to the user yet — step 13 first.

## 13. Notify Discord — the final action, always

After T is exhausted — full or partial success — write step 12's report as
markdown to a temp file and run:

```
scripts/notify-discord.sh <path to the markdown file>
```

The script owns the webhook (`DISCORD_WEBHOOK` in `~/.claude/.env`), the
escaping and the length cap; you never read the secret. A failure is worth
one line to the user and at most one retry. Then give the user the step-12
report as your final message.

## Non-negotiables

- **Commits land on local `main` only — never pushed by you or any agent.** A scratch clone's branch is internal and disposable.
- **No agent ever runs `gh issue close`.** Closing happens via a pushed commit's trailer.
- **No agent ever touches a real block device, a real mount, or runs `sudo`** — storage runs only in its own namespaced lab; `needs-sudo` issues never reach an agent.
- **Every lab is destroyed** before its clone is deleted, and no lane ever uses another lane's lab id.
- **Never poll or self-schedule while agents run.**
- **Exactly one `status:*` label per issue**, only via `scripts/issue-status.sh` / `scripts/epic-status.sh`.
- **Never invent an issue number** in a trailer.
- **Parallel lanes never share a scratch clone**, and only you touch the real repo, only at landing, one commit at a time.
- **One commit per issue, always.**
- **A fix round is never bundled; a `safety-critical` issue is never bundled.**
- **Every written artifact uses its template** — dispatch prompts, the executor's report, the verifier's comment.
- **Every run ends with exactly one Discord notification**, sent after T is exhausted and before your final message.
