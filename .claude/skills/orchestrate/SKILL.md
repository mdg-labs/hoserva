---
name: orchestrate
description: Given a GitHub issue number (a single item, or an epic with sub-issues), autonomously implement and verify the work — sonnet execution agents in isolated scratch clones (one per item, or one per bundle of small, correlated items), one independent verifier per attempt, landing on local dev only after a PASS and pushing it there immediately, including safety-critical commits, unless it's blocked by a fresh follow-up. Parallelizes items with disjoint file scope, serializes overlapping ones. Never touches main — that's a separate, maintainer-run dev-to-main promotion. Use when asked to "work on issue #n", "implement epic #n", "run the orchestrator", or "orchestrate #n".
argument-hint: <issue-number>... [--no-discord]
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
with sub-issues — into landed, verified commits on local `dev`, with no
human in the loop except at a genuine blocker (a `needs-sudo` step, a
repeated verification failure, or an external unmet dependency).
**Every landed commit is pushed to `dev` as soon as it lands, including
`safety-critical` ones** — `dev` is a working branch, not a release
branch, so the gate that matters is the independent verifier's PASS before
landing, not a manual pre-push read — one push per issue, unless the issue
itself carries an open `blockedBy` added during this run, which waits for
the maintainer to read and push instead: a fresh dependency limits trust in
the fix, regardless of the issue's own labels. `main` is out of scope for
this skill entirely: it only moves via a `dev → main` pull request the
maintainer opens by hand, gated by GitHub's required status checks (doc 12
§6).

An issue whose body carries a `Lands in: mdg-labs/hoserva-catalog` line is
still tracked here, but its commits land in the catalog repository instead,
on that repository's `dev` (step 1a), which follows the same `dev`/`main`
model. Everything below says "the real repo" for the default,
`mdg-labs/hoserva`, case.

**You (the current session) are the orchestrator.** You spawn `issue-refiner`,
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
  `dd of=/dev/…` on the host, ever. Never `losetup -D`, never `losetup -a`/`-l`
  on the host either — to check what's attached, use the sanctioned
  `/sys`-based check (`CLAUDE.md`, "Real disks are off-limits").
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
find /sys/devices/virtual/block -maxdepth 3 -path '*/loop/backing_file' -exec cat {} + 2>&1
ls -l /dev/kvm 2>&1; command -v qemu-system-x86_64 qemu-img 2>&1; grep -E '^vm-(up|destroy):' Makefile 2>&1
virsh -c qemu:///session list --all --name 2>&1
C=$(cd <real repo> && realpath -- "${HOSERVA_CATALOG_REPO:-../hoserva-catalog}") && git -C "$C" status -sb 2>&1 | head -1; ls "$C/scripts/devenv/hooks" 2>&1   # only when an issue lands in the catalog repository (step 1a)
```

That tells you whether the lab exists (`LAB_AVAILABLE`), whether Docker is
reachable by this user, which checkers are installed, and whether a stale
lab from an earlier run is still up (the `find` line prints one backing-file
path per loop device still attached from an earlier run, and nothing on a
clean host — never `losetup -a`/`-l` on the host for this) — and whether the
VM harness exists (`VM_AVAILABLE`) and which session VMs already exist
(report them, never touch them). A stale `hoserva-lab-*` container from a
previous run is reported to the user, not removed by you.

## 0. Resolve the target

```
scripts/gh-rest.sh issue-view <N> --jq '{number,title,body,labels,state}'
```

- **Not labelled `epic`:** the target set is just this one issue.
- **Labelled `epic`:**
  ```
  scripts/gh-rest.sh sub-issues <N> --jq '.[] | {number,title,state,labels:[.labels[].name]}'
  ```
  Drop any sub-issue already closed — REST spells state lowercase
  (`"open"`), so filter with `.state == "open"`.
- If the issue doesn't exist or the call fails, stop and say so — don't guess a number.

Call the resulting set of open issue numbers **T**.

## 1. Pull each issue's full body, comments, and relationships

For every issue in T — the body and its comments are two different REST
endpoints, so this is always two calls:

```
scripts/gh-rest.sh issue-view <n>
scripts/gh-rest.sh issue-comments <n>
```

**Comments are authoritative over the body where they disagree** — scope
corrections, reassigned halves, replaced acceptance criteria, and prior
verification findings all land as comments. Fold what you learn into your
own decisions (scope, ordering, whether it still belongs in T) and into the
dispatch; never assume the agent will rediscover it.

Then the native relationships:

```
scripts/gh-rest.sh blocked-by <n>
scripts/gh-rest.sh blocking <n>
scripts/gh-rest.sh parent <n>
scripts/gh-rest.sh sub-issues <n>
```

For each `d` in `blockedBy`, check its **labels**, not its open/closed
state — `main` is release-only (Q46) and stays open on GitHub long after
the fix has actually landed:
- `d` carries `status:implemented` or `status:closed` → satisfied: the fix
  has landed on local `dev` — pushed already unless it was itself held
  back by a fresh `blockedBy` — which is enough for a scratch clone made
  from the real repo's current state to build on.
- Otherwise, and `d` is in T → an intra-run ordering edge.
- Otherwise, and `d` is not in T → **external blocker.** Remove the issue
  from T and report: `#<n> is blocked by open #<d>, which is outside this
  run — orchestrate #<d> first, or include it explicitly`.

`d`'s GitHub open/closed state is not a signal here — it only reflects
whether the fix has reached `main`, a promotion the maintainer runs
separately, never something a dependent issue needs to wait for.

`parent` is the issue's epic, if any.

Also read the design context each issue cites (`## Design references`) —
you are about to judge its scope, and the docs are where scope lives.

## 1a. Resolve each issue's landing repository

For every issue in T, from the body already read in step 1:

```
scripts/gh-rest.sh issue-view <n> --jq '.body' | grep -m1 '^Lands in:'
```

- **No such line:** `LANDING_REPO = mdg-labs/hoserva`, the real repo.
  `HOSERVA_ROOT` is the scratch clone itself.
- **`Lands in: mdg-labs/hoserva-catalog`:** the landing clone is
  ```
  CATALOG=$(cd <real repo> && realpath -- "${HOSERVA_CATALOG_REPO:-../hoserva-catalog}")
  ```
  — the path comes from `HOSERVA_CATALOG_REPO`, never hard-coded. Require
  that it is a git clone whose `origin` URL names `mdg-labs/hoserva-catalog`,
  on `dev`, clean, and in sync with `origin/dev` (`git -C "$CATALOG" fetch
  origin && git -C "$CATALOG" merge --ff-only origin/dev`, then
  `git -C "$CATALOG" rev-parse HEAD` must equal `git -C "$CATALOG" rev-parse
  origin/dev` — a fast-forward also succeeds when local `dev` is *ahead*, and
  those unpushed commits would be cloned and later pushed with work no
  verifier reviewed); if any of that fails, leave the issue out of T and
  report why. The catalog repository has
  the same `dev`/`main` model as this one (Q46): its commits land on `dev`
  and `main` only moves via a `dev → main` pull request. `HOSERVA_ROOT` for its
  executor and verifier is the real `mdg-labs/hoserva` repo, read-only —
  it holds the status scripts, `CLAUDE.md`, the design docs and
  `known-escapes.md`, none of which the catalog clone has.
- **Any other value:** leave the issue out of T and report it.

The issue, its `status:*` labels, its epic and its comments stay on
`mdg-labs/hoserva` either way: `scripts/issue-status.sh`,
`scripts/epic-status.sh` and `scripts/gh-rest.sh` are always run from the real
`mdg-labs/hoserva` repo's `scripts/` with `GH_REPO` unset, and the catalog
repository gets no `status:*` label.

## 1b. Readiness gate — refine thin or stale issues before anything else

Issues that reached an executor without triage failed verification nearly
three times as often (39% vs 14%) and let five times as many defects
through to CodeRabbit (3.6 vs 0.7 per issue). Check every issue in T,
mechanically, from the real repo:

```
scripts/issue-readiness.sh <n>
```

It reads only the issue text: no `## Original report`, no acceptance
criteria, no out-of-scope, no `Reachable via:` on a `feat`/`bug`, or a
stale reference (the retired `beta` branch, a hardware tier, the Unraid CA
feed) → `NOT-READY` with reasons. Path warnings alone never block.

**You do not investigate or rewrite a NOT-READY issue yourself** — that is
code reading your context must not carry through the rest of the run.
Dispatch one `issue-refiner` per epic (or per issue without one), all in
one message, from `.claude/skills/orchestrate/templates/issue-refiner-prompt.md`
with `MODE = apply`:

```
Agent({
  subagent_type: "issue-refiner",
  model: "opus",      // any issue is safety-critical, or in the migration or VM epics
  model: "sonnet",    // otherwise
  description: "Refine <issue numbers>",
  prompt: <the filled template>
})
```

Fill `WORKSPACE_PATH` with a fresh clone for that refiner
(`git clone <real repo> <scratchpad>/orchestrate/refine-<unit>`),
`OUT_DIR` with `<scratchpad>/orchestrate/refine-<unit>-out`, and
`VERDICT_TEMPLATE_PATH` with the real repo's
`.claude/skills/orchestrate/templates/refiner-verdict.md`. The refiner
inventories what already exists on `dev` and returns one short verdict per
issue. Read **only the verdicts**:

- `refined` — the refiner already applied the new body. Wire the
  relationships it lists natively, run `scripts/issue-status.sh <n> ready`,
  re-read the issue (step 1's two calls) and continue with it.
- `split-proposed` — `AskUserQuestion` with the one-line-per-part
  proposal. On approval, apply `OUT_DIR/<n>.md` to the original issue with
  `scripts/gh-rest.sh issue-edit <n> --body-file OUT_DIR/<n>.md` (it keeps its number as
  part A) and create each `OUT_DIR/<n>-NEW-*.md` as a new issue with
  `scripts/gh-rest.sh issue-create`, with the labels and relationships the
  verdict lists (same epic and milestone), replace every `<NEW-…>`
  placeholder in the bodies with the real number
  (`scripts/gh-rest.sh issue-edit <m> --body-file <file>` for each part
  `<m>` whose body file `<file>` held a placeholder), set each to `ready`, and
  put all parts in T. The refiner already wrote the bodies — don't rewrite
  them.
- `already-done` / `obsolete` — `AskUserQuestion` with the evidence; drop
  the issue from T. Never cancel or close it yourself.
- `needs-decision` — `AskUserQuestion` with the refiner's question and
  recommended default; record the answer on the issue, then treat it as
  `refined` after a second refiner pass.

Delete each refiner clone when its verdicts are in. An issue that is still
NOT-READY after one refine pass is reported and left out of T — never
dispatched thin.

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
- **Entry points are in scope.** An issue's `Reachable via:` criterion names where its capability must be reachable from — `cmd/hoservad/main.go` (and its sibling wiring files), `cmd/hoserva/`, `web/src/routes/`, `Makefile`, `.github/workflows/`. Add every such file to the issue's scope as its own entry, the same way as an always-shared file, so lanes serialize on it. An issue with a runtime capability but no `Reachable via:` criterion is not ready — step 1b sends it to `issue-refiner` first. Leaving the entry point out of scope is what turned 17 finished features into later "wire it into hoservad" issues.
- An `api/openapi.yaml` change also puts `cmd/mockapi/` in scope: the mock mirrors production validation.
- Can't confidently bound it → its scope is **the whole repo**, which serializes it against everything.
- A scope is relative to the issue's landing repository. Scopes in different
  landing repositories never intersect, but two units landing in the same
  clone are placed by their paths in it as usual, and "the whole repo" means
  that landing repository.

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

- issues with different landing repositories (step 1a);
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

### Promotion-diff budget

CodeRabbit reviews at most 100 files per pull request, counted after
`.coderabbit.yaml`'s `path_filters` exclusions, and `dev` only reaches `main`
through one `dev → main` PR (`open-pr`, then `cr-review`). Once that diff
passes 100 reviewable files the promotion has to be split or goes partly
unreviewed, so check what this run would add to it before dispatching
anything. Issues that land in `mdg-labs/hoserva-catalog` (step 1a) are not
counted — that repository promotes on its own.

1. From the real repo, run
   `.claude/skills/dev-diff/dev-diff.sh --list`. It prints the reviewable
   paths of the current `main...dev` diff, one per line: the set `R`. If it
   exits non-zero (unclean tree, `main` diverged from `origin/main`), show
   its error and ask via `AskUserQuestion` whether to stop or to proceed
   without the budget check — never treat a failed run as an empty `R`.
2. Take each issue's expected files `E` from the `Expected files:` line of
   its scope hint (an estimate of reviewable files, excluding what
   `.coderabbit.yaml` filters out, plus the likely paths). An issue with no
   such line — triaged before the estimate existed — is not blocked and not
   sent back to refinement: derive a rough `E` from its step-3 scope (one
   file per backticked file path, about two per backticked directory) and
   say in the plan that you did.
3. Net the estimate: the issue's net new files `N` are the expected files not
   already in `R` — a file named by exact path that is in `R` is zero growth.
   Paths given as a directory, or not named, count as new. A path two issues
   in T both name is counted once, against the earlier one in wave order.
4. Print one line per issue, `#n: ~E expected, ~N new`, and the total:
   `dev→main reviewable: |R| now → ~|R|+ΣN projected (cap 100)`.
   - **Projected over 100** — stop before dispatching anything and ask via
     `AskUserQuestion`. Options: *promote first* (recommended — run
     `open-pr`, merge the promotion, then run this again); *trim T* to the
     issues that fit under the cap, in wave order; *proceed anyway*, with
     the overrun stated in the report.
   - **Projected 90 to 100** — proceed, and say in the plan that the next run
     will need a promotion first.

Print the plan before dispatching — waves, bundles and why, lanes and why,
and the budget lines above. If T has more than ~12 issues, state the count
and confirm via `AskUserQuestion` first.

## 5. Per dispatch unit: isolated scratch clone

Never work in the real repo; never share a clone between concurrent units.

```
mkdir -p <scratchpad dir>/orchestrate
git clone <landing clone path> <scratchpad dir>/orchestrate/<unit-id>-a1
git -C <scratchpad dir>/orchestrate/<unit-id>-a1 config core.hooksPath scripts/devenv/hooks
```

`<landing clone path>` is the real repo for a `mdg-labs/hoserva` landing and
`$CATALOG` (step 1a, from `HOSERVA_CATALOG_REPO`) for a
`mdg-labs/hoserva-catalog` one.

A plain `git clone` doesn't carry hooks over — the second line points this
clone at the repo-tracked `prepare-commit-msg` hook (CONTRIBUTING.md, doc 13
Q2) so every commit the executor makes here is signed off automatically,
the same as `make hooks-install` does for a human clone. **Run it only if
`<clone>/scripts/devenv/hooks` exists.** A clone without it (the catalog
repository has none) gets no hook, so its commits — the executor's, and your
landing commit in step 8 — are made with `git commit -s`, and every one still
carries `Signed-off-by`. Fill the executor template's `NO_HOOKS` blocks
accordingly.

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

You run it from the real repo; agents run their clone's copy — or, for a
`mdg-labs/hoserva-catalog` landing, the real repo's copy (`HOSERVA_ROOT`),
since the catalog clone has none. The script targets `mdg-labs/hoserva`
explicitly, because `gh` cannot infer a repo from a clone whose `origin` is a
local path, and so it targets it for every tracked issue whichever repository
the issue lands in.

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

- **You never set `status:closed`.** The trailer only closes the issue once its commit reaches `main` — that's the later `dev → main` promotion, not this run's push to `dev`; the workflow labels it when it happens. This holds for a `mdg-labs/hoserva-catalog` landing too: its `Fixes mdg-labs/hoserva#<n>` trailer closes the issue when the commit reaches the catalog's `main`.
- **You own the abandonment transitions:** an issue leaving your hands still open (executor `blocked`, or escalated after three FAILs) goes back to `status:ready`.

## 6. Dispatch `task-executor`

Read `.claude/skills/orchestrate/templates/executor-prompt.md` and fill every
`{{…}}` token: the shared preamble once (workspace, lab id, what step 0 found
about the machine, and from step 1a `LANDING_REPO` — named in the dispatch —
and `HOSERVA_ROOT`), then the per-issue block once per issue in bundle order —
number, title, body, **comment thread**, scope, `FIXES_TRAILER`, and whether
it is `safety-critical` or a `spike`. On a fix round, the rejected SHA and the
verifier's findings verbatim.

`FIXES_TRAILER` is `Fixes #<n>` for a `mdg-labs/hoserva` landing and
`Fixes mdg-labs/hoserva#<n>` for a `mdg-labs/hoserva-catalog` one. Fill the
template's `IF LANDING_REPO is mdg-labs/hoserva-catalog` blocks for the latter,
so its executor and verifier run the catalog repository's own checks instead
of `make test`, and the `NO_HOOKS` blocks when the clone has no
`scripts/devenv/hooks` (step 5). The verifier dispatch (step 7) takes the same
`LANDING_REPO` and `HOSERVA_ROOT`.

```
Agent({
  subagent_type: "task-executor",
  model: "sonnet",
  description: "Implement <unit-id>",
  prompt: <the filled template>
})
```

**The filled template is the prompt, inline and in full** — never write it
to a file and send a short prompt that points the agent at that file. This
holds for every dispatch: refiner, executor and verifier, and fix rounds
too. A script may fill the template, but its output goes into `prompt`
verbatim, however long it is.

**All lane-head dispatches for a wave go in one assistant message**, so they
run concurrently.

**Claim the unit's first issue yourself, right after dispatching it.** The
executor template's own "claim it" step is instructional, not structurally
guaranteed — a model can defer the actual `issue-status.sh` call until well
after it has started real work, or skip straight to it only right before
committing, so the label can lag true progress by a long margin. Don't wait
for that: immediately after the `Agent()` call above, from the real repo,
run
```
scripts/issue-status.sh <first issue's number> in-progress
scripts/epic-status.sh <epic number>   # only if the unit has one
```
for the **first** issue in the unit (bundle order). This makes the label
accurate the instant dispatch happens, independent of whatever the executor
itself does. It's harmless if the executor's own claim call runs again
later for the same issue — the script replaces the whole status-label set
each time, so a repeat call is a no-op in effect. This backstop only covers
a unit's first issue: for a bundle's second and later issues, you have no
way to know when the executor moves on to them inside one async dispatch,
so the executor template's own per-issue claim step is still what covers
those.

A bundle produces **one commit per issue**, each with only that issue's files
and its own `Fixes #<n>` trailer. Blocked is per issue: a bundle that
committed issue 1 and blocked on issue 2 hands you a real commit for issue 1.

If an issue comes back `blocked`: report the reason, put it back to
`status:ready`, leave it open, and continue with the rest of T that doesn't
depend on it.

**The Unraid calibration bundle.** When an issue cites the optional Diagnostics
bundle (doc 06 §5), name `~/.local/share/hoserva/calibration/` in the executor
and verifier dispatches as a **read-only path outside the workspace**, with
the rule stated: read it in place, record only general layout facts, never
copy it into the workspace, commit it, or quote any value from it in a file,
issue, comment or commit message. Never name it for an issue that doesn't
cite it.

### Waiting: end the turn, don't schedule anything

Subagents re-invoke you when they finish. Once everything dispatchable is
out the door, say in one line what you're waiting on and **end your turn.**
Never `ScheduleWakeup`, `Monitor`, or `sleep`; never re-dispatch because you
haven't heard back. When a notification arrives, route its findings
(step 11), verify that unit (step 7), and dispatch the next unit in its lane.

## 7. Dispatch `task-verifier`

Read `.claude/skills/orchestrate/templates/verifier-prompt.md` and fill it:
per issue, its details, the same comment thread, its scope, its flags, and
**its own commit SHA**; once, the workspace, lab id, attempt number, epic and
`LANDING_REPO`/`HOSERVA_ROOT` (step 1a).
On a fix round, fill the `FIX_ROUND` block too: the rejected SHA, where it
can be read, and the previous round's **blocking** findings verbatim — the
verifier checks those are closed and reviews what changed, rather than
restarting the review.

```
Agent({
  subagent_type: "task-verifier",
  model: "opus",      // any issue in the unit is safety-critical, OR the unit's diff is large (below)
  model: "sonnet",    // otherwise
  description: "Verify <unit-id> attempt <n>",
  prompt: <the filled template>
})
```

**Acceptance that only a real GitHub Actions run can show** — a changed
workflow under `.github/workflows/`, a nightly L3 step, a release or Pages
job — cannot be verified from the scratch clone. Before dispatching the
verifier, run it for real, without touching `dev`:
```
git -C <workspace> push <real repo's origin URL> HEAD:refs/heads/ci/<unit-id>
gh workflow run <workflow file> --repo mdg-labs/hoserva --ref ci/<unit-id>
gh run list --repo mdg-labs/hoserva --branch ci/<unit-id> --limit 1 --json databaseId,url
```
For `nightly-l3.yml` specifically (issue #391), pass only the step ids the
unit's own diff touches as its `l3_steps` input — doc 06 §4 lists them —
e.g. `gh workflow run nightly-l3.yml --repo mdg-labs/hoserva --ref
ci/<unit-id> -f l3_steps=storage-target,disk-yank`, which turns the ~56-
minute full suite into roughly what those steps plus the always-run setup
take. Run the full suite instead — no `l3_steps`, or `-f l3_steps=` left
empty — once before landing a change to `scripts/vm/run-l3-suite.sh`
itself or to `.github/workflows/nightly-l3.yml`: a partial selection
cannot prove such a change left every *other* step working.
`ci.yml`, `nightly-l3.yml`, `pages.yml` and `s9-hosted-probe.yml` accept
`workflow_dispatch` (a dispatch needs the trigger on `main`, so a workflow
newly given one only works after the next promotion). Put the run's URL
and id in the verifier dispatch, so the verifier reads its result and logs
(`gh run view <id> --log-failed`) as layer-1 evidence instead of guessing.
Wait for it with `timeout 5400 gh run watch <id> --repo mdg-labs/hoserva --exit-status`
as a **background** Bash command, then end your turn — its exit re-invokes
you, which keeps the "never poll or sleep" rule intact. Delete the
branch once the unit is resolved:
`git push <origin URL> --delete ci/<unit-id>`. Never push such a commit to
`dev` before its PASS.

**Large diffs get the Opus verifier too.** Before dispatching, measure the
unit's changed lines excluding generated code:
```
git -C <workspace> diff --numstat <landing branch>..HEAD -- . ':!api/gen' ':!internal/store/db' ':!**/package-lock.json' | awk '{s+=$1+$2} END {print s}'
```
Above ~1000, dispatch the verifier on Opus and say so in the report. Escapes
scale with size — the top quarter of issues by size (over ~1300 lines)
produced 61% of CodeRabbit's findings — and Opus-verified issues let
through about 40% fewer per changed line. An issue that lands far above its
triage estimate is worth a line in the report as well.

**One verifier per unit per attempt**, verdicts **per issue**: all seven layers
run against each commit separately, one comment and one label move per
issue. **Only blocking findings fail an issue**; notes are recorded in the
comment and go nowhere else — which is why a note may never describe a
defect: a wrong behaviour in code the diff adds is blocking, one in
untouched code is a finding outside the issue (step 11), and a capability
with no production caller fails layer 7. If a PASS comment's notes
describe a concrete defect anyway, treat that note as a finding outside the
issue and route it in step 11. A mixed PASS/FAIL result is normal. The verifier posts its own
comments and moves its own labels; read its returned verdicts rather than
re-deriving them from GitHub.

## 8. On PASS — land, sequentially, never in parallel

Land one commit at a time, in bundle order, skipping members that FAILed.
Every git command in this step runs in the unit's landing clone, the real repo
or, for a `mdg-labs/hoserva-catalog` landing, `$CATALOG` (step 1a) — add
`-C "$CATALOG"`. The same gates apply in both: a verifier PASS first, the
same `dev` branch, the same held-back rule, no force-push.

**Invoking this skill is the maintainer's authorization to commit to and push
`dev`** (and the catalog's `dev`, for a catalog landing). If a permission
check denies one of those exact steps, report the denial, name the command,
and retry it once the maintainer says it is granted — never hand the commit
or the push back to them. The project allow rules `Bash(git commit:*)` and
`Bash(git push origin dev)` match the real repo's plain forms below. The
catalog and CI-run forms are not matched, and each needs its own rule for the
run to go through unprompted:

- `git -C "$CATALOG" commit …` and `git -C "$CATALOG" push origin dev`
  (catalog landing): `Bash(git -C * commit:*)` and `Bash(git -C * push origin dev)`;
- `git -C <workspace> push <origin URL> HEAD:refs/heads/ci/<unit-id>` (step 7's
  real-run push): `Bash(git -C * push * HEAD:refs/heads/ci/*)`;
- `git push <origin URL> --delete ci/<unit-id>` (its cleanup):
  `Bash(git push * --delete ci/*)`.

```
git fetch <scratch workspace path> <sha>
git cherry-pick -n FETCH_HEAD
```

- **Cherry-pick succeeds.** Decide whether this is its epic's last open
  sub-issue — by membership, not by count. `main` stays open on GitHub long
  after a sub-issue's fix has actually landed (Q46: `main` only moves via
  promotion), so ask the label, not the open/closed state:
  ```
  scripts/gh-rest.sh sub-issues <epic> \
    --jq '[.[] | select(.state == "open") | select(all(.labels[]; .name != "status:implemented" and .name != "status:closed")) | .number]'
  ```
  This lists open sub-issues that are genuinely still unfinished. The
  verifier already set this issue's own `status:implemented` before you got
  here, so it never appears in that list — the epic's last sub-issue is the
  one where this list comes back **empty**. `scripts/gh-rest.sh` reads state
  over REST, which spells it lowercase (`"open"`) — the filter above must
  compare against that, not `"OPEN"`, or it matches nothing and every
  landing would close the epic.

  Commit with the executor's message, adding `Fixes #<epic>` only if it is
  really the last. This runs in the real repo, so it needs `make
  hooks-install` run there once (CONTRIBUTING.md, doc 13 Q2) — the
  `prepare-commit-msg` hook then adds the `Signed-off-by` trailer itself. In
  a clone without `scripts/devenv/hooks` (the catalog clone) there is no hook,
  so the commit is made with `-s` and the trailer is still there:
  ```
  git commit -s -m "$(cat <<'EOF'
  <the executor's own commit message>

  Fixes #<issue-number>
  [Fixes #<epic-number>]
  EOF
  )"
  ```
  Drop the `-s` only where the hook exists (the real repo). You may change
  only the message, never the diff.

  For a `mdg-labs/hoserva-catalog` landing the trailers are
  `Fixes mdg-labs/hoserva#<issue-number>` and, only if it is really the
  epic's last, `Fixes mdg-labs/hoserva#<epic-number>` — never the bare
  `Fixes #<n>`, which the catalog repository would read as one of its own
  issues. The push is `git -C "$CATALOG" push origin dev`. Like a
  `mdg-labs/hoserva` landing, it closes the issue (and the epic, when that
  trailer is present) only when the commit reaches the catalog's `main`
  through a `dev → main` pull request; `status:implemented` is the landing
  signal until then.

  **Push, unless this issue now carries an open `blockedBy`** (a follow-up
  step 11 filed against it during this run, limiting trust in the fix —
  `safety-critical` alone is no longer a reason to hold a push back: `dev`
  is a working branch, not `main`, and the independent verifier's PASS is
  the gate that matters):
  ```
  git push origin dev
  ```
  **Before pushing, confirm no unpushed ancestor is itself held back.**
  `git push` moves the remote to your current local tip, carrying every
  commit underneath it along — so if an earlier commit in this run was held
  back by a fresh `blockedBy` and is still sitting, unpushed, under the one
  you're about to land, pushing now would push that held-back commit too
  and silence the reason it was held. Check first:
  ```
  git log origin/dev..dev --oneline
  ```
  If that list contains anything besides the commit you just landed, stop
  and look at what it is before pushing — if any of it is a commit this run
  deliberately held back, **don't push**; report the pair (or run) as still
  local and explain the dependency in step 12 instead. This can only happen
  after a hold has already occurred earlier in the same run; the common
  case is an empty list beyond your own commit, and you push normally.

  A commit held back by a fresh `blockedBy` stays local — list it in the
  report (step 12) for the maintainer to read and push by hand. Everything
  else pushes immediately, one push per landed issue: that is what gives
  the maintainer a live CI status on `dev` as the run progresses, instead
  of one batched push at the end. If the push is rejected (someone else
  moved `dev` meanwhile), don't force it — report it and stop touching
  that remote for the rest of this run.

  Once **every** issue in the unit is resolved: confirm its lab is gone
  (`docker ps --filter name=hoserva-lab-<lab-id>` empty and no `<clone>/.lab/`
  left; if either remains, run `make -C <clone> lab-destroy HOSERVA_LAB_ID=<lab-id>`),
  then delete the clone. Lab files are root-owned, so a clone with a leftover
  `.lab/` can't be deleted by your user — `lab-destroy` removes it from inside
  the container. Never reach for `sudo rm`; report a clone you couldn't delete.

- **Cherry-pick conflicts** (a wrong scope prediction): `git cherry-pick
  --abort`. A mechanical rebase problem, not a rejected implementation —
  doesn't spend a fix attempt. Destroy the unit's lab, delete the clone,
  re-clone fresh from current `dev`, redispatch the same issue with a
  one-line "rebase re-run" note. Cap at 2 rebase retries, then escalate as
  in step 9.

## 9. On FAIL — fix, then re-verify, capped at 3 attempts

**A fix round is always single-issue** — a FAIL dissolves its unit.

- **After a single-issue attempt:** a fresh `task-executor` in the **same
  clone**, template branch `FIX_ROUND_SAME_WORKSPACE`, with the rejected SHA
  and the **blocking** findings verbatim (never the notes). It amends; the workspace stays one commit ahead
  of `dev`.
- **After a bundled attempt:** re-clone fresh from current `dev` (its
  passing siblings have landed), template branch `FIX_ROUND_FRESH_CLONE`,
  with `PRIOR_ATTEMPT_PATH`/`PRIOR_COMMIT_PATH` pointing at the old bundle
  workspace, read-only. A normal new commit.

The attempt counter carries over. Dispatch a fresh verifier against the new
SHA with the `FIX_ROUND` block filled: it re-runs every check, confirms each
previous blocking finding is closed, and reviews what changed — it does not
hunt for new findings in code an earlier round already accepted. If a fix
round nonetheless raises new blocking findings in unchanged code, read them
yourself before dispatching another round: a real data-loss or security
defect stays; anything else is a note, and you say so in the report.

If attempt 3 also fails: stop. Put the issue back to `status:ready`, then
`AskUserQuestion` with the latest findings — keep trying / hand it to the
maintainer / skip for now. Destroy its lab and delete its clone.

## 10. Repeat until T is empty

Move to the next wave once every issue in the current one has landed, been
skipped as blocked, or been escalated — including issues step 11 pulled into
this run.

## 11. Route what the run surfaces — as it arrives, not at the end

Every executor report and every verdict can carry **Findings outside these
issues**. Route each one **as soon as that report arrives**, before you
dispatch the next unit. Notes never qualify — they stay in the verification
comment.

1. **Is it real?** Check it yourself against the file, line or command it
   names: a defect with a concrete scenario, an untrue doc or doc 13
   statement, or work a planned feature cannot do without. If it isn't, drop
   it and list it under "dropped" in the report with a one-line reason.
2. **Is it already tracked?** If an open issue's scope already covers it,
   file nothing — note the number, and comment on that issue only if the
   finding adds a concrete detail it lacks. If it is really a missing
   acceptance criterion of an open issue nobody has started, add it there
   (`github-triage`, enrich mode) instead of filing a new issue.
3. **Otherwise file it now**, via `github-triage` (create mode), and decide
   where it belongs:
   - **This run** — it belongs to this run's scope (it touches the same area
     or paths as an issue in T, or an issue in T is not really right without
     it) **and** it can be done now (no `needs-sudo`, no open dependency
     outside the run, no pending maintainer decision). Attach it to the same
     epic as the T issue it came from, add any blocked-by edges ordering
     needs, add it to T, place it in the waves — usually right after the
     issue that surfaced it — and dispatch it in this run like any other
     issue, with its own commit and `Fixes #` trailer. A decision issue
     whose recorded decision needs code (a `docs` issue settling a default)
     is the typical case: the code is pulled in and done in the same run.
   - **Deferred** — it belongs to other work: attach it natively to the open
     epic whose scope it falls under (sub-issue, plus that epic's
     milestone), with blocked-by edges to the open issues it needs. It waits
     until that epic is worked. If no open epic fits, file it without a
     parent and ask the maintainer where it belongs.
4. **A doc 13 default that looks wrong** is always an issue (type `docs`,
   touching doc 13), routed the same way — never a silent doc edit.
5. **A decision only the maintainer can make** — ask with `AskUserQuestion`
   when it comes up, and put the answer in the issue.

**Growth guard.** If pulled-in issues would grow T by more than three beyond
its original size, or a pulled-in issue surfaces yet another pull-in, ask the
maintainer before pulling in more; defer whatever they don't want in this
run.

Never fold a finding into an unrelated landing commit: a pulled-in finding
gets its own issue and its own commit.

## 12. Compose the report

- What landed (issue → commit SHA → one line), and whether it reached
  `origin/dev`
- **Safety-critical commits landed this run** — their own list, even if
  empty ("none this run"), noting they were pushed like everything else;
  worth naming separately so the maintainer knows which ones deserve a
  closer read even though they're already on `origin/dev`
- **Commits held back by a fresh `blockedBy`** — landed locally, not pushed, because step 11 filed a follow-up against them during this run (their own list, even if empty: "none this run")
- What's blocked and why (`needs-sudo` prepared, external dependency, lab not yet available, escalated after 3 FAILs)
- Bundles and why
- What step 11 routed: pulled into this run (issue → commit), deferred
  (issue → epic/milestone), added to an existing issue, and dropped (with
  why)
- **Promotion-diff budget** — `dev→main reviewable: before → after`
  (actual: re-run `.claude/skills/dev-diff/dev-diff.sh --list` from the real
  repo after the last landing; `before` is step 4's `|R|`), next to step 4's
  projection. Per issue, its actual new files are the files of its commit
  that are in the final list and were not in `R` or in an earlier issue's
  commit this run; name every issue whose actual count exceeds its estimate
  by more than about 50%, as for changed lines in step 7. If the user chose
  to proceed past a projected overrun, say so here.
- Any stale lab containers or loop devices step 0 found
- **What's still local, for the maintainer to read and push**: the
  `blockedBy`-held list above (and, in the rare stacking case step 8
  describes, anything sitting underneath a held commit that therefore
  couldn't be pushed either). `git push origin dev` once satisfied —
  everything else already reached `origin/dev` during the run.

Don't send it to the user yet — step 13 first.

## 13. Notify Discord — the final action, unless the run was started with `--no-discord`

If the invocation carries `--no-discord` (or the maintainer asked in words
to skip the Discord message), skip this step and say so in one line.
Otherwise, after T is exhausted — full or partial success — write step 12's report as
markdown to a temp file and run:

```
scripts/notify-discord.sh <path to the markdown file>
```

The script owns the webhook (`DISCORD_WEBHOOK` in `~/.claude/.env`), the
escaping and the length cap; you never read the secret. A failure is worth
one line to the user and at most one retry. Then give the user the step-12
report as your final message.

## Non-negotiables

- **Commits land on local `dev` and are pushed to `origin/dev` immediately after landing, including `safety-critical` ones** — or, for an issue with a `Lands in: mdg-labs/hoserva-catalog` line, on `dev` of the clone at `HOSERVA_CATALOG_REPO`, pushed to its `origin/dev`. Only a commit held back by a fresh `blockedBy` added during this run is never pushed by you — the maintainer reads and pushes that. Before any push, confirm no held-back commit sits unpushed underneath the one you're landing (step 8) — pushing would carry it along too. `main` — in either repository — is never touched by this skill at all; it only moves via a maintainer-run `dev → main` promotion. A scratch clone's own branch is internal and disposable.
- **No agent ever runs `gh issue close`.** Closing happens via a pushed commit's trailer.
- **No agent ever touches a real block device, a real mount, or runs `sudo`** — storage runs only in its own namespaced lab; `needs-sudo` issues never reach an agent.
- **Every lab is destroyed** before its clone is deleted, and no lane ever uses another lane's lab id.
- **Never poll or self-schedule while agents run.** The one sanctioned wait on something outside the harness is a bounded background `gh run watch` for a CI-dependent unit (step 7).
- **Exactly one `status:*` label per issue**, only via `scripts/issue-status.sh` / `scripts/epic-status.sh`.
- **Never invent an issue number** in a trailer.
- **Parallel lanes never share a scratch clone**, and only you touch the real repo, only at landing, one commit at a time.
- **One commit per issue, always.**
- **A fix round is never bundled; a `safety-critical` issue is never bundled.**
- **Only blocking findings fail an issue or reach a fix round**; a fix round's verifier checks closure and the change, not the whole issue afresh.
- **Surfaced findings are filed and routed as they arrive** — pulled into this run when they belong to its scope, otherwise attached to the open epic they belong to.
- **Every written artifact uses its template** — dispatch prompts, the executor's report, the verifier's comment.
- **Every dispatch prompt is passed inline in full** — never as a pointer to a file holding it.
- **Every run ends with exactly one Discord notification**, sent after T is exhausted and before your final message — unless it was started with `--no-discord`.
