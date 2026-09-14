# Hoserva — Repository Architecture

## Recommendation: monorepo. Single repository, no exceptions in v1.

---

## 1. Why monorepo

The general case for multi-repo is independent release cycles, independent teams, and independent consumers. **None of those apply here.**

- **One developer.** Multi-repo overhead (cross-repo PRs, version pinning, synchronised releases) is pure cost with no offsetting benefit.
- **Everything ships together.** The backend, frontend, and CLI are one `.deb`. There is no scenario where the frontend releases independently of the API it consumes.
- **The API is the seam, and it must not drift.** Backend and frontend share generated types and fixtures. In one repo, an API change breaks both in the same PR. Across repos, it breaks the frontend three days later.
- **Atomic changes are the norm, not the exception.** Adding a field to the disk model touches the Go model, the API handler, the OpenAPI spec, the generated TS types, the UI component, and the fixtures. That is one commit in a monorepo and five coordinated PRs otherwise.

### The Claude Code argument, which is decisive here

This will be built entirely with Claude Code, and that strengthens the monorepo case considerably:

- **Full-stack context in one working tree.** An agent implementing a feature can read the Go handler, the OpenAPI spec, and the React component without repo-switching. Cross-repo work means the agent either loses context or needs multiple checkouts coordinated by hand.
- **One command runs everything.** `make test` covering L1+L2 in a single tree gives the agent a single, reliable verification signal. Split repos mean partial verification and a class of bug that only appears at integration.
- **One `CLAUDE.md`.** Conventions, architecture rules, and the decision log live in one place the agent always has. Duplicating that across repos guarantees drift.
- **Refactors stay tractable.** Renaming a domain concept across backend, API, and UI is a single mechanical change an agent can complete and verify.

**Exception, later:** the template catalog and the docs site are genuine candidates for separate repos, because they have external contributors, different review standards, and different release cadence. Split them out when that becomes true, not before.

---

## 2. Layout

```
hoserva/
├── CLAUDE.md                   agent instructions — see §4
├── LICENSE                     AGPL-3.0 once adopted (Q1)
├── Makefile                    every workflow, one entry point
├── go.work                     if multiple Go modules become necessary
│
├── .claude/
│   ├── agents/                 task-executor, task-verifier (§5)
│   └── skills/                 github-triage, orchestrate (§5)
├── .github/workflows/          CI (doc 06 §7), issue-status lifecycle (§5)
│
├── cmd/
│   ├── hoservad/              daemon
│   ├── hoserva/               CLI
│   └── mockapi/                frontend mock server (doc 06 §8)
│
├── internal/                   per doc 01 §4
│   ├── api/
│   ├── store/
│   ├── model/
│   ├── disk/
│   ├── pool/
│   ├── parity/
│   ├── cache/                  mover, rebalance, evacuation (doc 09)
│   ├── share/
│   ├── container/
│   ├── template/
│   ├── vm/                  libvirt/QEMU orchestration, passthrough (doc 14)
│   ├── migrate/
│   ├── backup/                 config + appdata backup (doc 10)
│   ├── assistant/              AI assistant (doc 11)
│   ├── job/
│   ├── notify/
│   └── config/
│
├── api/
│   ├── openapi.yaml            hand-written, the contract
│   └── gen/                    generated Go + TS types — committed
│
├── web/                        Vite + React SPA, embedded via go:embed (Q8)
│   ├── components.json         shadcn CLI config for the @coss registry (D15)
│   ├── src/routes/
│   ├── src/components/ui/      coss primitives, added and updated with the shadcn CLI
│   ├── src/components/patterns/ shared UI patterns from doc 03's component system
│   ├── src/components/         wrappers for Q59's libraries (chart, editor, terminal, console)
│   ├── src/lib/api/            uses api/gen TS types
│   └── fixtures/               shared with backend tests
│
├── templates/                  curated app catalog (doc 04 §7, Q39)
│
├── scripts/
│   ├── devenv/                 loop-device harness + lab container (doc 06 §3)
│   ├── vm/                     VM lifecycle and snapshots
│   ├── release/
│   ├── bootstrap-labels.sh     creates the issue label set (§5)
│   ├── issue-status.sh         the one way to set a status:* label
│   ├── epic-status.sh          rolls an epic's status up from its sub-issues
│   └── notify-discord.sh       end-of-run notification for orchestrate
│
├── testdata/
│   ├── configs/                golden files
│   ├── parsers/                real tool output corpus
│   └── unraid-templates/       XML corpus
│
├── packaging/
│   ├── debian/
│   └── iso/
│
├── docs/internal/              design docs (this document set)
└── site/                       Astro Starlight public docs (Q3; split out later)
```

### Notable choices

**`internal/` for everything.** Nothing here is a library for others to import. Using `internal/` makes that explicit and prevents accidental API-surface commitments.

**Generated API types are committed.** Not generated at build time. Committing them means the diff is visible in review — an accidentally breaking API change shows up as a large generated diff, which is exactly the signal wanted. It also means an agent can read the current types without running a generator first.

**`openapi.yaml` is hand-written and authoritative.** Both Go and TS types generate from it. The alternative (generating the spec from Go annotations) makes the contract a side effect of the implementation, which is backwards when the CLI, the UI, and potentially third parties all consume it.

**Fixtures are shared.** `web/fixtures/` is consumed by both the mock API server and backend tests. They cannot drift because breaking one breaks both.

---

## 3. Makefile as the single interface

Everything an agent or a human does goes through `make`. No remembered incantations, no "which directory do I run this from".

```make
make setup            # install toolchain, hooks, generators
make build            # backend + frontend + CLI
make dev              # daemon against the loop-device lab
make mock             # mock API + frontend dev server

make lab-up           # loop-device array in the narrowed lab container; needs HOSERVA_LAB_ID
make lab-seed         # synthetic data
make lab-destroy

make vm-up
make vm-snapshot NAME=
make vm-restore  NAME=
make vm-deploy        # build .deb, install into VM

make test             # L1 + L2
make test-unit
make test-integration
make test-e2e         # L3, needs a VM
make test-migration

make gen              # openapi → go + ts types
make lint
make deb
make iso
```

**Rule: if a workflow isn't in the Makefile, it doesn't exist.** This matters disproportionately with an agent — a documented `make` target is a reliable action; a shell pipeline described in prose is a guess.

---

## 4. `CLAUDE.md`

The highest-leverage file in the repository. It exists at the repo root from the first commit — before any code — and grows the code conventions below as the code does. It should contain:

**Architecture rules that are not negotiable**
- The decision log (doc 00 §5) inline or linked, with a note that reopening a decision requires a new reason
- Source-of-truth model: SQLite is authoritative, config files are generated, never write config directly
- Provider interfaces exist for testability — new system-touching code goes behind an interface with a fake
- Never bypass the job system for long-running work
- Never shell out with string interpolation of user or template input

**Safety rules, stated as hard constraints**
- Parity is written only by a user-configured schedule or an explicit user action, every sync goes through the threshold guard, and nothing syncs past a tripped guard
- The threshold guard is not optional and its tests may not be weakened
- Destructive operations are copy-verify-delete, never move-and-hope — and array-to-array relocations are two-phase: copy, verify, sync, then delete (Q14)
- Development never touches a real block device: labs only via `make lab-up` (doc 06 §3, Q45)
- Anything touching data loss gets a test before it gets an implementation

**Conventions**
- Error handling, logging, context propagation patterns
- Where new code goes, with the module map
- Test requirements by change type
- Commit and PR format

**Workflow**
- `make test` before any PR
- How to run the loop-device lab
- How to add a golden-file test
- Where fixtures live and why they are shared

**Anti-patterns specific to this project**
- Don't add generic Docker management features (D6)
- Don't put business logic in API handlers
- Don't add a second placement algorithm alongside mergerfs's create policy (doc 09 §2)
- Don't let the frontend compute anything the backend should own

### Scoped `CLAUDE.md` files

Subdirectory-level files for areas with their own rules:

- `internal/parity/CLAUDE.md` — SnapRAID invariants, what must never be run without confirmation, the threshold guard contract
- `internal/cache/CLAUDE.md` — the copy-verify-delete contract, open-file checks, resumability requirements
- `internal/migrate/CLAUDE.md` — never destructive, checksum verification, the point-of-no-return boundary
- `web/CLAUDE.md` — coss-first rule and doc 03's component map (D15), local edits to `src/components/ui/` kept minimal so `shadcn add --diff` stays readable, the plain-language labelling rule, fixtures
- `internal/assistant/CLAUDE.md` — the safety boundaries from doc 11 §6

These are the areas where a wrong-but-plausible implementation destroys data, and where local, specific instructions beat general ones.

---

## 5. Working with an agent on this codebase

### What makes this project agent-friendly

- **The loop-device harness is the key enabler.** An agent can create a real array, run a real sync, break a disk, and verify recovery — all in a container, in seconds, with no risk. Very few systems-level projects have a fast, safe, real-behaviour test loop. This one can, and it is worth the upfront investment in `scripts/devenv/` before writing feature code.
- **Golden-file config tests give unambiguous feedback.** Config generation either matches or produces a reviewable diff.
- **The OpenAPI contract is machine-checkable.** Drift between layers surfaces as a build failure, not a runtime surprise.

### Recommended sequencing

Build the harness and the test infrastructure **first**, before feature work. An agent with a fast verification loop is dramatically more effective than one guessing at correctness. Order:

1. `scripts/devenv/` loop-device harness + Makefile targets
2. Provider interfaces + fakes
3. Golden-file test infrastructure
4. `openapi.yaml` + generation pipeline
5. Mock API server
6. *Then* features

That ordering feels slow for the first week and pays back continuously afterwards.

### Guardrails worth having

- **Pre-commit hooks** running lint and unit tests, so broken code doesn't accumulate
- **CI as the arbiter**, not local runs — doc 06 §7's pipeline
- **Safety-critical paths** requiring a human line-by-line read before push: the threshold guard, the mover/relocation delete path, the migration import, anything in `packaging/`. Issues touching them carry the `safety-critical` label; the verifier applies an extra data-safety review to them, and every orchestrate report lists their commits separately (Q46)
- **Never point the agent at real hardware.** Doc 06 §5's hard rule applies with more force when an agent is driving: the lab container and the VMs are the only environments, and the real array is touched only by a human who has read the diff. The lab container exposes loop devices and FUSE only, so a mistyped device path cannot reach a real disk (doc 06 §3, Q45)

### Issue-driven workflow

GitHub issues on `mdg-labs/hoserva` are the plan and the memory between sessions, with the same skill set proven on the maintainer's other projects:

- **`github-triage`** (`.claude/skills/github-triage/`) turns a rough report into a structured issue, or enriches an existing one — grounded in these design docs, every open question landed on a recommended default (the doc 13 rule), epics and dependencies wired as native GitHub relationships.
- **`orchestrate`** (`.claude/skills/orchestrate/`) takes an issue or an epic and lands verified commits: `task-executor` agents implement in isolated scratch clones, parallel where file scopes are disjoint; an independent `task-verifier` reviews each commit; only a PASS is landed on local `main`. Nothing is pushed automatically.
- **Status labels** (`status:new` → `ready` → `in-progress` → `in-review` → `implemented` → `closed`) are machine-managed: `.github/workflows/issue-status.yml` owns the ends, `scripts/issue-status.sh` everything in between — exactly one `status:*` label per issue, always.
- `CLAUDE.md` carries the full rules; `scripts/bootstrap-labels.sh` creates the label set.

---

## 6. Branching and releases

Minimal, since there is one developer (Q46):

- `main` is always releasable
- **Agent work lands as verified local commits on `main`** via the `orchestrate` skill — one commit per issue, closed by its `Fixes #n` trailer. The maintainer reads (safety-critical commits line by line) and pushes; nothing agent-made is pushed automatically. CI runs on every push.
- **External contributors** use pull requests from forks, squash-merged, with CI on hosted runners only (doc 06 §7)
- Tags drive releases; CI builds the `.deb` and publishes to the apt repo
- Conventional commits (`feat(parity): …`), since the changelog generates from them and the agent will write most of them

**Release channels:** `stable` and `beta` in the apt repo. Beta exists for the hardware beta group (doc 06 §6) and for anything touching the mover, parity, or migration — the three areas where a bad release costs someone their data.

---

## 7. When to split the monorepo

Split when a component acquires **external contributors with a different review bar**:

| Component | Split when |
|---|---|
| `site/` | Community starts contributing guides — different review standards, different cadence |
| Template catalog | First external template PR — needs its own CI, its own review process, and should not gate a Hoserva release |
| Anything else | Probably never |

Both splits are easy later and premature now. Neither is on the critical path for v1.
