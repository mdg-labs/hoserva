# Roadmap

**This file is the seed, GitHub issues are the truth.** Every item below
becomes one GitHub issue on `mdg-labs/hoserva` via `scripts/roadmap-sync.py`.
From the moment an item has an issue number, **the issue is authoritative** —
status, discussion and scope changes live there, and this file keeps only the
`issue:` pointer so the plan stays readable in one place. New work after that
starts as an issue via `github-triage`, not as a new roadmap entry.

The phases, their rationale, the spikes' kill criteria and the risk register
live in `docs/internal/07-roadmap.md`; this file is that plan broken into
issue-sized work. Every item names the design docs it implements — read them
before starting (`CLAUDE.md`, "Documentation map").

```
scripts/roadmap-sync.py --validate      # parse and validate, no GitHub calls
scripts/roadmap-sync.py                 # dry run against GitHub
scripts/roadmap-sync.py --apply         # create issues, relationships, milestones
```

## Structure

Two levels, mirrored in GitHub with native fields only:

- **`##` epic → epic issue** with an ` ```epic ` block. Its `phase` becomes the
  GitHub **milestone** of the epic and every item in it.
- **`###` item → sub-issue** of that epic (native parent), with a ` ```meta `
  block. `depends` becomes native **blocked-by** relationships.

Nothing deeper, and no relationship is ever written as body prose.

## Phases → milestones

```phases
P0: Phase 0 — Feasibility spikes
P1: Phase 1 — Storage core
P2: Phase 2 — NAS completeness
P3: Phase 3 — Apps and migration
P3.5: Phase 3.5 — Virtual machines
P4: Phase 4 — Polish and release
```

## Format

**Epic block:** `id` (`M<n>`), `phase` (from the table above), `status`,
`labels`, `issue`.

**Item block:**

| Field | Values |
|---|---|
| `id` | stable, never reused — `M<epic>.<n>` |
| `epic` | the parent epic id |
| `status` | `todo` · `doing` · `blocked` · `done` |
| `labels` | exactly one type, at most one `area:*`, plus extras — the set in `scripts/bootstrap-labels.sh` |
| `sudo` | `true` exactly when labelled `needs-sudo` — the maintainer runs those steps |
| `depends` | item ids, or `[]` — cross-epic and cross-phase allowed |
| `issue` | `null` until synced, then the issue number |

`status:*` labels are machine-managed and never appear here. The item body is
the issue body: **Summary**, **Design references**, **Acceptance criteria**
(checkable — required, the script refuses stubs) and **Scope** (paths, so
`orchestrate` can bound the work). Bodies must not contain `##`/`###`
headings or `---` lines, which delimit entries.

---

## M0 — Phase 0: feasibility spikes

```epic
id: M0
phase: P0
status: todo
labels: [spike]
issue: 1
```

Short, disposable experiments answering the questions that could invalidate
the plan (doc 07 §1). Each deliverable is recorded findings, not product code
(`CLAUDE.md`, "Spikes"): a findings section in doc 08, the exact commands and
outputs, experiment scripts under `spikes/<id>/`, and the doc 13 entries it
confirms or overturns. Every spike is agent work (D20); spikes that run in the
loop-device lab wait for the lab (M1.2).

### S1 — Spindown under mergerfs

```meta
id: M0.1
epic: M0
status: todo
labels: [spike, area:storage]
sudo: false
depends: [M1.2]
issue: 2
```

**Summary** Confirm with agent-run measurements what doc 08 §1 established from
research: with Hoserva's intended mergerfs options, no IO reaches idle array
disks while idle or during appdata-only activity — the property that keeps
real disks in standby (doc 06 §6).

**Design references** doc 08 §1, doc 06 §6, doc 02 §1 (Spindown), Q31, Q13, Q32, R1, D20

**Acceptance criteria**
- [ ] In the lab, per-disk read and write counters on every array disk stay flat for ≥ 30 minutes in three scenarios — fully idle; a container writing appdata on cache; that plus a connected, idle SMB client — each with default and raised `cache.*` timeouts
- [ ] Every IO that reaches an array disk is attributed to a process (fanotify or blktrace) and recorded
- [ ] A recommendation for the "Quiet mode" timeouts with the staleness tradeoff, or a finding that the defaults suffice
- [ ] Residual risk (firmware- and controller-level wakes) stated; doc 08 S1 updated with commands and raw outputs; Q31 confirmed or updated; R1 revisited

**Scope** `docs/internal/08-spike-findings.md`, `spikes/s1/`

### S2 — Unraid disk adoption

```meta
id: M0.2
epic: M0
status: todo
labels: [spike, area:migration]
sudo: false
depends: [M1.2]
issue: 3
```

**Summary** Prove the adoption path against synthetic Unraid-layout disks: data
disks formatted the way Unraid formats them mount read-only on Debian 13, union
with mergerfs, and keep their share structure and checksums. No agent runs
Unraid or connects to a real Unraid server (D20).

**Design references** doc 08 §2, doc 06 §5 (Building Unraid fixtures without Unraid), doc 05 §1–§3, Q21, Q23, Q24, Q25, Q55, R5, D20

**Acceptance criteria**
- [ ] A throwaway fixture script under `spikes/s2/` builds XFS data disks in the lab with Unraid's partition layout and share directories, from public sources cited in doc 08
- [ ] Every disk passes `xfs_repair -n` and mounts read-only by filesystem UUID; the mergerfs union shows every file with a matching sha256
- [ ] The flash configuration layout for 6.12 and 7.x — shares, users, disk assignments, `templates-user/`, `libvirt.img` — documented from public sources (Q24, Q25, Q55)
- [ ] What synthetic fixtures cannot prove stated as residual risk; doc 08 S2 updated; Q24 and Q25 confirmed or updated

**Scope** `docs/internal/08-spike-findings.md`, `spikes/s2/`

### S5 — SnapRAID fidelity on loop devices

```meta
id: M0.5
epic: M0
status: todo
labels: [spike, area:devenv]
sudo: false
depends: [M1.2]
issue: 6
```

**Summary** Confirm SnapRAID behaves on loop devices exactly as on real
disks for everything the L2 harness will test, so the development workflow
rests on real behaviour.

**Design references** doc 06 §3, doc 07 §1, Q45

**Acceptance criteria**
- [ ] In the lab: sync, diff, scrub, fix and full-disk reconstruction after detaching a loop device all behave as the SnapRAID manual describes
- [ ] Corruption injected with `dd` into an image is detected by scrub and repaired by fix
- [ ] Any behaviour that differs from real disks listed, with which test layer must cover it instead
- [ ] Findings recorded in doc 08

**Scope** `spikes/s5/`, `docs/internal/08-spike-findings.md`

### S6 — Per-share mergerfs mount topology

```meta
id: M0.6
epic: M0
status: todo
labels: [spike, area:storage]
sudo: false
depends: [M1.2]
issue: 7
```

**Summary** Validate the catch-all plus per-share mount design: reliable
mounting over a FUSE directory, correct ordering, and `mspmfs` falling back
to the parent path as documented.

**Design references** doc 02 §1, Q11, Q12, R12, doc 09 §1

**Acceptance criteria**
- [ ] In the lab: a catch-all mount plus a dozen per-share mounts with all three cache modes mount and unmount cleanly, in dependency order
- [ ] `mspmfs` falls back to the parent path when the disk holding the full path is full; `epmfs` returns ENOSPC in the same case
- [ ] `NC` branches stay readable while new files land on cache
- [ ] Result recorded in doc 08, Q11 and Q12 updated; if the topology fails, the two-mount fallback is adopted there

**Scope** `spikes/s6/`, `docs/internal/08-spike-findings.md`, `docs/internal/13-open-questions.md`

### S7 — fanotify change journal

```meta
id: M0.7
epic: M0
status: todo
labels: [spike, area:storage]
sudo: false
depends: [M1.2]
issue: 8
```

**Summary** Prove a `FAN_MARK_FILESYSTEM` mark per data disk counts
creates, modifications, deletions and renames accurately without adding any
disk access of its own.

**Design references** doc 02 §2 (Change journal), Q13, doc 08 §1

**Acceptance criteria**
- [ ] In the lab: event counts per filesystem match a following `snapraid diff` for scripted workloads, including renames and writes through mergerfs
- [ ] Behaviour across a daemon restart and a filesystem remount documented
- [ ] Nothing in the journal reads the filesystem on a timer
- [ ] Findings recorded in doc 08, Q13 confirmed or updated

**Scope** `spikes/s7/`, `docs/internal/08-spike-findings.md`

### S8 — Debian 13 dependency coverage

```meta
id: M0.8
epic: M0
status: todo
labels: [spike, area:packaging]
sudo: false
depends: [M0.6]
issue: 9
```

**Summary** Finish S8: confirm Debian 13's `mergerfs` 2.40.2 and `snapraid`
12.4 packages provide everything the design needs, or name exactly what an
upstream build would add.

**Design references** doc 08 (S8 partial), Q7, doc 02 §1

**Acceptance criteria**
- [ ] Every mergerfs option and policy doc 02 §1 sets is confirmed on 2.40.2, using S6's results
- [ ] SnapRAID features doc 02 §2 relies on (`2-parity`, `--force-empty`, `touch`) confirmed on 12.4
- [ ] Q7 updated with the verdict and the version range for the `hoserva` package

**Scope** `docs/internal/08-spike-findings.md`, `docs/internal/13-open-questions.md`

### S9 — Hosted CI runners: loop devices, FUSE and KVM

```meta
id: M0.9
epic: M0
status: todo
labels: [spike, area:devenv]
sudo: false
depends: [M1.3]
issue: 10
```

**Summary** Finish S9 for GitHub-hosted runners: can they run the L2 lab and
offer `/dev/kvm` for L3, so pull-request code never needs a self-hosted
runner?

**Design references** doc 06 §7, doc 08 (S9), Q42, Q45, R11

**Acceptance criteria**
- [ ] A workflow job on a hosted runner creates loop devices, XFS and a mergerfs mount via the lab recipe
- [ ] `/dev/kvm` availability and a minimal QEMU boot tested on a hosted runner
- [ ] Q42 updated with which suites run hosted and which stay on self-hosted runners with trusted triggers

**Scope** `.github/workflows/`, `docs/internal/08-spike-findings.md`

---

## M1 — Phase 1: foundation

```epic
id: M1
phase: P1
status: todo
labels: [chore]
issue: 11
```

Everything feature work stands on, built before any feature code (doc 12 §5):
the repository skeleton, the loop-device lab, CI, provider interfaces and
fakes, golden files, the API contract and its generation, the central schema
with data-safe schema migrations, the job system, the mock API server, the
web app skeleton on coss ui, and the daemon skeleton. A fast, safe, real
verification loop first — agents are only effective with one.

### Repository skeleton, Makefile and contribution terms

```meta
id: M1.1
epic: M1
status: todo
labels: [chore, area:devenv]
sudo: false
depends: []
issue: 12
```

**Summary** Create the Go module, directory layout, Makefile entry points and
contribution terms, so every later workflow has a `make` target to live in.

**Design references** doc 12 §2, §3, doc 01 §4, D3, Q2, Q6

**Acceptance criteria**
- [ ] `go.mod` for `github.com/mdg-labs/hoserva`; `cmd/hoservad`, `cmd/hoserva` and `cmd/mockapi` build as stubs
- [ ] `make build`, `make test`, `make test-unit`, `make lint` exist; lint runs `gofmt`, `go vet` and `golangci-lint`
- [ ] `.gitignore` covers `.lab/`, build output and fetched corpora
- [ ] `CONTRIBUTING.md` states DCO sign-off with no CLA (Q2) and points to `CLAUDE.md`
- [ ] Static, CGO-free build verified for amd64 and arm64 (D3, Q5)

**Scope** `go.mod`, `Makefile`, `cmd/`, `.gitignore`, `CONTRIBUTING.md`

### Loop-device lab container

```meta
id: M1.2
epic: M1
status: todo
labels: [chore, area:devenv, safety-critical]
sudo: false
depends: [M1.1]
issue: 13
```

**Summary** The narrowed lab from doc 06 §3 and Q45: loop devices and FUSE
only, namespaced by `HOSERVA_LAB_ID`, started and destroyed only through
`make`. It is the guard between agent work and the developer's real disks.

**Design references** doc 06 §3, Q45, R13, doc 08 (S9 recipe), `CLAUDE.md` "Real disks are off-limits"

**Acceptance criteria**
- [ ] `make lab-up` / `make lab-destroy` with `HOSERVA_LAB_ID` required; no `--privileged`, no `/dev` bind mount, device cgroup limited to loop devices
- [ ] `scripts/devenv/create-array.sh` builds parity, data and cache loop disks with XFS and a mergerfs mount; `seed-data.sh` provides the doc 06 profiles
- [ ] Every script refuses any device that is not a loop device backed by its own `$LAB/img/`; `losetup -D` appears nowhere
- [ ] A test proves opening a host block device from inside the lab fails
- [ ] Teardown deletes `.lab/<id>` from inside the container, is idempotent, and never touches another lab's devices
- [ ] Two labs with different ids run concurrently without collision

**Scope** `scripts/devenv/`, `docker-compose.dev.yml`, `Makefile`

### CI on hosted runners

```meta
id: M1.3
epic: M1
status: todo
labels: [chore, area:devenv]
sudo: false
depends: [M1.1, M1.2]
issue: 14
```

**Summary** The every-push part of doc 06 §7's pipeline, on GitHub-hosted
runners only, with no path for pull-request code to reach a self-hosted
runner.

**Design references** doc 06 §7, Q42, R11

**Acceptance criteria**
- [ ] Workflow runs lint, vet, L1 unit tests and the L2 lab suite on every push and pull request, on hosted runners
- [ ] No job uses `pull_request_target` with a checkout of PR code; no self-hosted runner is referenced
- [ ] arm64 and amd64 builds run on every push
- [ ] The repository setting requiring approval for first-time contributors is documented for the maintainer to enable

**Scope** `.github/workflows/`

### Provider interfaces and scriptable fakes

```meta
id: M1.4
epic: M1
status: todo
labels: [feat, area:storage]
sudo: false
depends: [M1.1]
issue: 15
```

**Summary** The `disk.Provider` and `parity.Engine` interfaces with fakes
that are simulators, not stubs, so storage logic is testable at L1 and the
frontend can run without a NAS.

**Design references** doc 01 §4 (Subsystem abstraction), doc 06 §2, `CLAUDE.md` architecture rules

**Acceptance criteria**
- [ ] Interfaces take `context.Context` first on anything doing IO
- [ ] The fake disk provider scripts disks, SMART trends, spin state, failure after a delay and slowdown (doc 06 §2)
- [ ] The fake parity engine scripts diff reports, sync progress and failures
- [ ] A test asserts SMART polling never queries a standby disk in a waking mode

**Scope** `internal/disk/`, `internal/parity/`

### Golden-file test infrastructure

```meta
id: M1.5
epic: M1
status: todo
labels: [chore, area:storage]
sudo: false
depends: [M1.1]
issue: 16
```

**Summary** State-in, text-out testing for every generated config file, with
reviewable diffs and deliberate updates only.

**Design references** doc 06 §2 (Golden-file config tests), `CLAUDE.md` conventions

**Acceptance criteria**
- [ ] A helper compares rendered output with `testdata/configs/<case>/*.golden`
- [ ] Updating goldens needs an explicit flag and prints the diff; a plain test run never rewrites them
- [ ] One example case wired end to end

**Scope** `testdata/configs/`, `internal/config/`

### API contract, generated server and clients

```meta
id: M1.6
epic: M1
status: todo
labels: [feat, area:api]
sudo: false
depends: [M1.1]
issue: 17
```

**Summary** A hand-written OpenAPI 3.1 `api/openapi.yaml` with the Go server
interfaces, the CLI's Go client and the web UI's TypeScript client generated
from it and committed, plus the checks that make the spec the whole API.

**Design references** doc 01 §5 (The contract), doc 12 §2, §3, D5, D18, Q63

**Acceptance criteria**
- [ ] `api/openapi.yaml` (OpenAPI 3.1) covers `/api/v1/` versioning, auth schemes, errors, jobs, and every SSE event type as a schema
- [ ] ogen confirmed on the jobs, errors and SSE parts of the spec, or the Q63 fallback adopted and Q63 updated
- [ ] `make gen` regenerates the Go server interfaces and client and the TypeScript client into `api/gen/`; CI fails if committed output is stale
- [ ] `make api-check` runs Spectral with a Hoserva ruleset (every operation has an `operationId` and an `x-hoserva-role`), and oasdiff against the last release once one exists
- [ ] A handler that doesn't implement its generated interface fails the build — demonstrated with a test operation

**Scope** `api/`, `Makefile`

### Central schema and data-safe schema migrations

```meta
id: M1.7
epic: M1
status: todo
labels: [feat, area:api, safety-critical]
sudo: false
depends: [M1.1]
issue: 18
```

**Summary** D16 end to end: one central `schema.sql`, schema migrations
generated from it and immutable, typed queries, the startup runner with
snapshot and single-transaction apply, and the checks that keep all of it
data-safe.

**Design references** doc 01 §4 (Database schema and schema migrations), D16, Q60, Q6, doc 06 §2, R16

**Acceptance criteria**
- [ ] sqldef's SQLite output confirmed for added columns and a table rebuild before the first table lands; the result recorded in Q60
- [ ] `make db-migration NAME=` generates the next migration and updates checksums; `make db-check` fails on an edited migration, on drift, and on a drop outside a registered contract step
- [ ] sqlc generates `internal/store/db/` from `schema.sql`
- [ ] The runner refuses a newer database, snapshots with `VACUUM INTO` before applying, applies in one transaction with `foreign_key_check` and `integrity_check`, and keeps the last three snapshots
- [ ] Tests: an injected failure leaves the database unchanged; the fixture-upgrade harness for `testdata/db/` exists, with a first fixture
- [ ] A data-transform example is tested against that fixture

**Scope** `internal/store/`, `testdata/db/`, `Makefile`

### Job system

```meta
id: M1.8
epic: M1
status: todo
labels: [feat, area:api]
sudo: false
depends: [M1.7]
issue: 19
```

**Summary** Persisted, cancellable, observable long-running jobs with the
mutually exclusive classes enforced by the scheduler, not the UI.

**Design references** doc 01 §4 (Job system), Q29, Q30

**Acceptance criteria**
- [ ] Jobs persist in SQLite and are marked `interrupted` after a restart, never resumed automatically
- [ ] Checkpoint API for resumable job types (Q29)
- [ ] Parity, Array-write, Topology, Service and VM classes enforced as in doc 01 §4's table, with tests for each exclusion
- [ ] Progress streams over SSE; stdout/stderr captured and downloadable
- [ ] Non-cancellable jobs say so rather than accepting a cancel

**Scope** `internal/job/`, `internal/api/`

### Mock API server with scenarios

```meta
id: M1.9
epic: M1
status: todo
labels: [feat, area:api]
sudo: false
depends: [M1.6]
issue: 20
```

**Summary** A Go binary serving the API from fixtures shared with backend
tests, so UI work never needs the daemon or a lab.

**Design references** doc 06 §8, doc 12 §2 (Fixtures are shared)

**Acceptance criteria**
- [ ] `go run ./cmd/mockapi --scenario=<name>` serves healthy, degraded, rebuilding, sync-blocked, fresh-install and migration-pending
- [ ] Implements the generated server interfaces, so a spec change it doesn't follow fails to compile (D18)
- [ ] Fixtures in `web/fixtures/` validate against `api/openapi.yaml`, and backend tests consume the same files
- [ ] `make mock` starts it together with the web dev server

**Scope** `cmd/mockapi/`, `web/fixtures/`

### Web app skeleton on coss ui

```meta
id: M1.10
epic: M1
status: todo
labels: [feat, area:web]
sudo: false
depends: [M1.6, M1.9]
issue: 21
```

**Summary** The Vite + React SPA with coss ui, the i18n catalog, bundled
fonts, the app shell and the first shared patterns, running against the mock
server.

**Design references** doc 03 (Component system, Navigation structure), D15, Q8, Q48, Q59, Q49

**Acceptance criteria**
- [ ] Vite + React + client-side router; Tailwind CSS v4 with coss tokens and `components.json` for the `@coss` registry
- [ ] Light and dark themes follow the system preference
- [ ] Inter and Geist Mono bundled locally and wired to `--font-sans`, `--font-heading`, `--font-mono`; the built app makes no outbound request
- [ ] Every string comes from the i18n catalog from the first component
- [ ] `app-shell`, `section-nav`, `status-badge`, `banner`, `empty-state`, `loading` and `job-progress` patterns built under `src/components/patterns/`
- [ ] All API access through the generated TypeScript client in `src/lib/api/`, with a lint rule rejecting direct `fetch` calls elsewhere (D18); the build output embeds into the Go binary

**Scope** `web/`

### Daemon skeleton: listeners, access and authentication

```meta
id: M1.11
epic: M1
status: todo
labels: [feat, area:api]
sudo: false
depends: [M1.6, M1.7]
issue: 22
```

**Summary** `hoservad` serving the API over the Unix socket and a TLS-only
TCP port, with the LAN source filter, first-run admin creation, sessions,
TOTP and login rate limiting.

**Design references** doc 01 §5, §7, Q9, Q10, Q28, Q44, doc 03 §1

**Acceptance criteria**
- [ ] Unix socket for root and the `hoserva` group; TLS-only `:8008` with a self-signed certificate on first start, plain HTTP answered with a pointer to `https://`
- [ ] Source filter allows loopback, RFC 1918, link-local, ULA and `100.64.0.0/10` by default, with tests per range
- [ ] No route but first-run setup is reachable before an admin exists
- [ ] Sessions, TOTP enrolment and verification, login rate limiting and lockout
- [ ] Machine key at a configurable path, `/etc/hoserva/secret.key` in production, encrypts secret columns (Q28); dev runs use a workspace-local state directory
- [ ] Handlers implement the generated server interfaces; middleware enforces each operation's `x-hoserva-role` (D18)
- [ ] The SPA is served for every non-API path

**Scope** `cmd/hoservad/`, `internal/api/`

---

## M2 — Phase 1: storage engine

```epic
id: M2
phase: P1
status: todo
labels: [feat, area:storage]
issue: 23
```

The storage core the product exists for: disks and their health, array setup,
the per-share pool mounts, SnapRAID orchestration with the threshold guard and
change journal, the nightly chain, and the disk lifecycle. Orchestrated, never
reimplemented (D1); every sync through the guard; nothing on a timer walks a
data disk.

### Disk enumeration, identity, SMART and spin-state log

```meta
id: M2.1
epic: M2
status: todo
labels: [feat, area:storage]
sudo: false
depends: [M1.4, M1.8]
issue: 24
```

**Summary** The real `disk.Provider`: enumerate block devices, identify them
stably, poll SMART without waking standby disks, and record spin-state
transitions.

**Design references** doc 02 §4 (SMART monitoring), Q21, Q32, doc 08 §1, doc 03 §3.3, §3.3a

**Acceptance criteria**
- [ ] Identity by WWN, falling back to serial; USB-enclosure disks flagged as weak identity (Q21)
- [ ] SMART via `smartctl -j -n standby`; parser tests against a real output corpus in `testdata/parsers/`
- [ ] Trend tracking for reallocated, pending, uncorrectable and CRC counts, and temperature
- [ ] Spin-state event log recorded without waking disks, exposed through the API
- [ ] The boot device is always identified and excluded from anything destructive

**Scope** `internal/disk/`, `testdata/parsers/`

### Config generator and drift detection

```meta
id: M2.2
epic: M2
status: todo
labels: [feat, area:api]
sudo: false
depends: [M1.5, M1.7]
issue: 25
```

**Summary** The one path from SQLite state to generated files: headers,
hashes, drift detection, and the three user choices when a file was edited by
hand.

**Design references** doc 01 §2, D4

**Acceptance criteria**
- [ ] Every generated file carries the doc 01 §2 header with the config revision
- [ ] Hashes recorded on write and compared on apply and on a timer that never touches a data disk
- [ ] Drift offers view diff, regenerate, or keep and stop managing; unmanaged files stay unmanaged
- [ ] Writes are atomic (temp file plus rename) and go to a configurable root, so tests and dev runs never write `/etc`

**Scope** `internal/config/`

### Array setup: disks, mounts and SnapRAID configuration

```meta
id: M2.3
epic: M2
status: todo
labels: [feat, area:storage, safety-critical]
sudo: false
depends: [M2.1, M2.2]
issue: 26
```

**Summary** Create an array from assigned disks — format or adopt, mount at
the standard paths, and generate `snapraid.conf` with content files placed
safely.

**Design references** doc 02 §2 (What Hoserva configures), §5, doc 01 §6, Q18, Q19, Q20, Q23, D10

**Acceptance criteria**
- [ ] One or two parity disks, each at least as large as the largest data disk; parity always XFS
- [ ] Data disks formatted XFS by default, or adopted as-is after a read-only filesystem check
- [ ] Mounts by filesystem UUID at `/mnt/diskN`, `/mnt/parityN`, `/mnt/cache`, as systemd mount units
- [ ] Content files on at least three distinct devices and `parity + 2` copies; the generator refuses a layout that violates it (Q18)
- [ ] Lab tests: format refuses any non-loop device in the lab and any disk not explicitly assigned; golden files for 1- and 2-parity layouts
- [ ] Formatting is a Topology job with typed confirmation required at the API

**Scope** `internal/disk/`, `internal/parity/`, `testdata/configs/`

### Pool mounts: catch-all and per-share mergerfs

```meta
id: M2.4
epic: M2
status: todo
labels: [feat, area:storage]
sudo: false
depends: [M2.3, M0.6]
issue: 27
```

**Summary** The mount topology S6 validated: a catch-all `/mnt/user`, one
mergerfs mount per share with branches by cache mode, and the internal
array-only mount the mover writes through.

**Design references** doc 02 §1 (Mount topology, Configuration), Q11, Q12, doc 09 §2

**Acceptance criteria**
- [ ] Generated systemd mount units with `RequiresMountsFor=` ordering; golden files per cache mode
- [ ] Create policy per share with the plain-language labels (doc 02 §1)
- [ ] Lab test: mounts survive a remount cycle and come up in order; a stray top-level write lands on the array, not the boot device
- [ ] Options from doc 02 §1's table, confirmed against S6 and S8 findings

**Scope** `internal/pool/`, `testdata/configs/`

### Parity engine: SnapRAID operations and parsers

```meta
id: M2.5
epic: M2
status: todo
labels: [feat, area:storage]
sudo: false
depends: [M1.4, M2.3]
issue: 28
```

**Summary** The real `parity.Engine`: sync, diff, scrub, check, fix, touch and
status as jobs, with parsers for SnapRAID's output.

**Design references** doc 02 §2 (Operations), Q17, doc 01 §4

**Acceptance criteria**
- [ ] Every operation runs as a Parity-class job with progress, cancellation where SnapRAID allows it, and captured output
- [ ] `diff` and `status` parsers tested against a real output corpus
- [ ] `touch` runs before a sync only when `status` reports zero sub-second timestamps (Q17)
- [ ] Lab tests: sync, scrub-detects-corruption and fix-restores-a-deleted-file pass on loop disks
- [ ] Argv-only execution; no shell interpolation

**Scope** `internal/parity/`, `testdata/parsers/`

### Threshold guard

```meta
id: M2.6
epic: M2
status: todo
labels: [feat, area:storage, safety-critical]
sudo: false
depends: [M2.5]
issue: 29
```

**Summary** The single most important safety feature: every sync, whatever
triggered it, is blocked when a diff looks like a disaster, until a human
decides. Tests come first.

**Design references** doc 02 §2 (The deletion threshold guard), Q15, Q16, R2, doc 06 §3 (Failure injection)

**Acceptance criteria**
- [ ] Blocks on removed files above N (default 500), removed+updated above X% (default 10%), or a data disk reporting zero files where it had files
- [ ] No code path syncs without passing the guard — enforced structurally in the engine, with a test that tries each trigger
- [ ] Relocation manifests: accounted removals shown as their own group and excluded from thresholds; unrelated removals in the same diff still count (Q15)
- [ ] Lab test written before the implementation: fill the array, unmount a disk, run diff, assert the sync is blocked and a notification fires
- [ ] Thresholds configurable, never disableable below a confirmation prompt

**Scope** `internal/parity/`

### Change journal

```meta
id: M2.7
epic: M2
status: todo
labels: [feat, area:storage]
sudo: false
depends: [M0.7, M2.3]
issue: 30
```

**Summary** Per-disk fanotify journal of changes since the last sync, giving
the approximate "files changed" count and the names of files at risk on a
failed disk without waking anything.

**Design references** doc 02 §2 (Change journal), Q13, doc 02 §4 (Replacing a failed disk)

**Acceptance criteria**
- [ ] A `FAN_MARK_FILESYSTEM` mark per data disk as S7 validated; events persisted per disk
- [ ] Count reset at each successful sync; shown as approximate through the API
- [ ] "Files changed since last sync on disk N, by name" queryable
- [ ] Lab test: counts match the pre-sync diff for a scripted workload

**Scope** `internal/parity/`

### Nightly maintenance chain and schedules

```meta
id: M2.8
epic: M2
status: todo
labels: [feat, area:api]
sudo: false
depends: [M1.8, M2.6]
issue: 31
```

**Summary** One chained nightly run — mover, diff and guard, touch, sync,
config backup, and the weekly scrub — where each step starts when the
previous finishes, plus schedules for everything else.

**Design references** Q30, doc 02 §2, doc 03 §8.4

**Acceptance criteria**
- [ ] Chain steps run in order, each holding its job class in turn; individual steps can be disabled, not reordered
- [ ] A blocked guard stops the chain after diff and notifies
- [ ] Conflict detection for jobs scheduled outside the chain
- [ ] Test: a long first step delays the sync rather than overlapping it

**Scope** `internal/job/`, `internal/api/`

### Disk lifecycle: add a disk, replace a failed disk

```meta
id: M2.9
epic: M2
status: todo
labels: [feat, area:storage, safety-critical]
sudo: false
depends: [M2.4, M2.6]
issue: 32
```

**Summary** Grow the pool with no rebuild, and recover from a dead disk with
the guided replace flow and an honest statement of what cannot be recovered.

**Design references** doc 02 §4 (Adding a disk, Replacing a failed disk), doc 03 §3.5 (Guided fix flow), doc 02 §6

**Acceptance criteria**
- [ ] Add disk: format or adopt, mount at the next `/mnt/diskN`, add to every branch list and the SnapRAID data list, then sync through the guard
- [ ] Replace: mark failed, degraded banner state, identify the new disk, `snapraid fix -d`, verify
- [ ] Before a fix, the API lists files written after the last sync on that disk from the change journal
- [ ] Lab tests: detach a data disk, replace it, reconstruct, verify checksums; adding a disk triggers no rebuild

**Scope** `internal/disk/`, `internal/pool/`, `internal/parity/`

### Spindown acceptance test

```meta
id: M2.10
epic: M2
status: todo
labels: [chore, area:storage]
sudo: false
depends: [M0.1, M2.4, M2.7, M3.9]
issue: 33
```

**Summary** The v1 spindown criterion as an automated test with the real daemon
running: no IO reaches idle array disks, and Hoserva is never the source of any
that does (doc 06 §6).

**Design references** Q31, Q32, doc 02 §1 (Spindown), doc 06 §6, R1, D20

**Acceptance criteria**
- [ ] With no SMB/NFS clients, no containers holding pool paths, appdata on cache, and SMART polling and the change journal running, per-disk read and write counters on every array disk stay flat for 30+ minutes, in the lab and in L3
- [ ] In L3, the SMART poller issues only standby-aware queries and causes no read IO on an idle disk
- [ ] The spin-state event log and IO attribution show no IO caused by `hoservad`
- [ ] Runs nightly; the result and its stated residual risk recorded for publication with the release

**Scope** `docs/internal/08-spike-findings.md`, `scripts/vm/`

---

## M3 — Phase 1: surfaces, packaging and the soak-test gate

```epic
id: M3
phase: P1
status: todo
labels: [feat]
issue: 34
```

What turns the storage engine into something the author can run their own
array on: notifications, config backup, the CLI, UI tier 1, the `.deb`, the
L3 VM harness, and the Phase 1 definition of done — a clean soak test
(doc 07 §1).

### Notifications

```meta
id: M3.1
epic: M3
status: todo
labels: [feat, area:api]
sudo: false
depends: [M1.8]
issue: 35
```

**Summary** Deliver alerts through email, Gotify, ntfy, Discord and a generic
webhook, with per-event routing, quiet hours and a test send per channel.

**Design references** doc 03 §8.3, doc 01 §7, Q28

**Acceptance criteria**
- [ ] All five channel types, with credentials stored encrypted (Q28)
- [ ] Every event in doc 03 §8.3's list can be routed per channel and severity
- [ ] Quiet hours with a critical-alerts override that cannot be disabled
- [ ] "Send test notification" reports success or the delivery error
- [ ] Delivery failures retried and logged, never silently dropped

**Scope** `internal/notify/`

### Config backup: archive, verification and local destinations

```meta
id: M3.2
epic: M3
status: todo
labels: [feat, area:backup]
sudo: false
depends: [M1.7, M2.2]
issue: 36
```

**Summary** The doc 10 §1 archive with a consistent database snapshot,
secrets re-encrypted under the backup passphrase, verification, and the two
local default destinations.

**Design references** doc 10 §1, §4, Q28, Q40, doc 01 §6

**Acceptance criteria**
- [ ] Archive layout per doc 10 §1; `state.db` produced with `VACUUM INTO`, never copied live
- [ ] Secrets section re-encrypted under the backup passphrase; the machine key is never included
- [ ] Every archive verified after writing (checksums, snapshot opens, `integrity_check` passes)
- [ ] Defaults: boot device and a pool path (Q40); retention 7 daily, 4 weekly, 6 monthly
- [ ] Runs as the last step of the nightly chain and before every self-update and topology change

**Scope** `internal/backup/`

### CLI for Phase 1

```meta
id: M3.3
epic: M3
status: todo
labels: [feat, area:cli]
sudo: false
depends: [M1.11, M2.8]
issue: 37
```

**Summary** `hoserva` over the Unix socket with parity to the Phase 1 API:
status, pool, disks, sync, scrub, fix, jobs, logs, config export/import and
doctor, each with `--json`.

**Design references** doc 01 §3, D5

**Acceptance criteria**
- [ ] Commands from doc 01 §3 that Phase 1 implements, all through the generated Go client — no logic only the CLI has (D5, D18)
- [ ] `--json` on every command; stable exit codes
- [ ] `hoserva doctor` reports dependencies, versions from package metadata (Q7), mount state, parity freshness, SMART and free space in plain language
- [ ] Destructive commands require the same confirmation the API requires

**Scope** `cmd/hoserva/`

### UI: onboarding and login

```meta
id: M3.4
epic: M3
status: todo
labels: [feat, area:web]
sudo: false
depends: [M1.10, M1.11]
issue: 38
```

**Summary** `/welcome` and `/login` as doc 03 specifies them, built from the
coss patterns named there.

**Design references** doc 03 §1 (and its Components line), doc 03 Component system, Q28, Q48

**Acceptance criteria**
- [ ] All four onboarding steps, blocking other routes until complete
- [ ] System check shows remediation commands with `copy-value`; missing Docker is a warning, a missing storage dependency blocks
- [ ] Skipping the notification channel or the backup passphrase states the consequence
- [ ] Login with TOTP; lockout message after repeated failures
- [ ] Components match doc 03's Components lines; strings through the i18n catalog

**Scope** `web/src/routes/`, `web/src/components/patterns/`

### UI: array setup wizard

```meta
id: M3.5
epic: M3
status: todo
labels: [feat, area:web]
sudo: false
depends: [M1.10, M2.3]
issue: 39
```

**Summary** `/storage/setup`: discovery, role assignment, filesystems, pool
options, review with generated config preview, and typed confirmation of
every disk that will be erased.

**Design references** doc 03 §3.1 (and its Components line), Q11, Q19, Q20, Q21

**Acceptance criteria**
- [ ] Live validation of parity sizes, parity count and weak-identity disks (refused as parity)
- [ ] Review shows the plain-language summary and a `code-view` of the generated config
- [ ] Confirmation lists every disk to be erased and requires typed confirmation
- [ ] Works against the mock server's fresh-install scenario

**Scope** `web/src/routes/`

### UI: dashboard, storage pages and jobs

```meta
id: M3.6
epic: M3
status: todo
labels: [feat, area:web]
sudo: false
depends: [M1.10, M2.1, M2.6]
issue: 40
```

**Summary** The tier 1 pages that make the array observable: dashboard, pool
overview, disk list, wake events, parity with the guard and the guided fix
flow, and jobs.

**Design references** doc 03 §2, §3.2–§3.5, §6, §10, doc 02 §2 (Parity freshness)

**Acceptance criteria**
- [ ] Top bar with array status, parity freshness chip and active jobs on every page
- [ ] Persistent banners for degraded array, blocked sync and config drift
- [ ] Parity page shows grouped diffs with removals first, guard state, and the guided fix flow with `unsaved-guard`
- [ ] No page load or poll triggers anything that wakes a data disk; Run diff states that it will
- [ ] Mobile-usable dashboard, job progress and notifications
- [ ] Degraded, rebuilding and sync-blocked mock scenarios render correctly

**Scope** `web/src/routes/`, `web/src/components/`

### UI: notification and schedule settings

```meta
id: M3.7
epic: M3
status: todo
labels: [feat, area:web]
sudo: false
depends: [M3.1, M2.8]
issue: 41
```

**Summary** `/settings/notifications` and `/settings/schedules`, the two
settings pages Phase 1 cannot ship without.

**Design references** doc 03 §8.3, §8.4, Q30

**Acceptance criteria**
- [ ] Channel cards with test send, routing matrix and quiet hours as doc 03 §8.3 specifies
- [ ] The nightly chain as ordered steps that can be disabled but not reordered, with the next run shown
- [ ] Schedule conflicts shown as a warning banner

**Scope** `web/src/routes/`

### Debian package and apt repository

```meta
id: M3.8
epic: M3
status: todo
labels: [chore, area:packaging, safety-critical]
sudo: false
depends: [M1.11, M0.8]
issue: 42
```

**Summary** The `.deb` with its dependencies, systemd unit, maintainer
scripts and the signed apt repository with stable and beta channels.

**Design references** D8, D9, Q7, Q41, doc 04 §2, doc 12 §6, doc 01 §6

**Acceptance criteria**
- [ ] Depends on Debian 13's `mergerfs` and `snapraid` within the version range from S8; `Recommends: rclone`; Docker not a dependency (D8)
- [ ] `postinst` creates the `hoserva` group and machine key and never touches data disks; `purge` never removes `/var/lib/hoserva/stacks/`
- [ ] Built in CI for amd64 and arm64; installs cleanly on a fresh Debian 13 in L3
- [ ] Apt repository layout, signing and channels documented; publishing credentials are the maintainer's

**Scope** `packaging/`, `scripts/release/`

### L3 VM harness and nightly end-to-end suite

```meta
id: M3.9
epic: M3
status: todo
labels: [chore, area:devenv]
sudo: false
depends: [M3.8]
issue: 43
```

**Summary** libvirt test VMs with named snapshots, `.deb` deployment, and the
nightly L3 suite including Playwright journeys and the power-loss and
disk-yank cases.

**Design references** doc 06 §4, §7, Q42

**Acceptance criteria**
- [ ] `make vm-up`, `vm-snapshot`, `vm-restore`, `vm-deploy` driving VMs through `scripts/vm/`, never touching host disks
- [ ] Nightly on `main` on a trusted runner: install, onboarding, array setup, disk yank and reconstruction, `virsh destroy` mid-sync recovery, reboot persistence
- [ ] Playwright journey 5 (mass deletion blocks the sync) runs and can never be skipped
- [ ] Config backup to a fresh VM and full restore verified

**Scope** `scripts/vm/`, `.github/workflows/`, `web/`

### Soak test

```meta
id: M3.10
epic: M3
status: todo
labels: [chore, area:devenv]
sudo: false
depends: [M2.10, M3.2, M3.6, M3.8, M3.9]
issue: 44
```

**Summary** Phase 1's definition of done: Hoserva runs in an L3 VM through a
month of simulated use and failures, and that run's diff history tunes the
guard thresholds before anything ships publicly (doc 06 §6).

**Design references** doc 07 §1 (Phase 1 definition of done), doc 06 §6 (Soak test), Q16, R2, R7, D20

**Acceptance criteria**
- [ ] A scripted L3 soak run of at least 30 nightly chains back to back — sync, scrub, mover — over seeded daily churn: adds, edits, renames, and mass deletes that must trip the guard
- [ ] Injected failures during the run — a yanked disk, a full disk, power loss mid-sync — each recovered with checksums verified
- [ ] Every blocked sync reviewed and explained in the run report
- [ ] Q16's thresholds revisited against the recorded diff history and updated or confirmed
- [ ] Every problem found filed as an issue

**Scope** `scripts/vm/`, `docs/internal/13-open-questions.md`

---

## M4 — Phase 2: shares and access

```epic
id: M4
phase: P2
status: todo
labels: [feat, area:shares]
issue: 45
```

SMB and NFS shares with a coherent ownership model, users and roles, API
tokens, and their UI — the part that makes the pool a NAS people in the
household actually use (doc 07 §1, Phase 2).

### Share model and SMB configuration

```meta
id: M4.1
epic: M4
status: todo
labels: [feat, area:shares]
sudo: false
depends: [M2.2, M2.4]
issue: 46
```

**Summary** Shares as first-class state: create, edit and delete, each backed
by its per-share mergerfs mount, with Samba configuration generated from
SQLite.

**Design references** doc 03 §4, doc 02 §1, doc 01 §2 (Escape hatches), D4, D10

**Acceptance criteria**
- [ ] Creating a share creates its directory tree on the branches and its per-share mount
- [ ] `smb.conf` generated with guest, read-only, browseable, recycle bin and Time Machine options; ends with the user-owned `include`
- [ ] Golden files for representative share sets
- [ ] Deleting a share definition and deleting its data are two separate API operations
- [ ] Browse endpoint lists a directory with the holding disk per file from `user.mergerfs.basepath`, as an explicit call that may wake disks

**Scope** `internal/share/`, `testdata/configs/`

### NFS exports

```meta
id: M4.2
epic: M4
status: todo
labels: [feat, area:shares]
sudo: false
depends: [M4.1]
issue: 47
```

**Summary** Per-share NFS exports with allowed hosts and squash options,
generated like every other managed file.

**Design references** doc 03 §4.2 (NFS), doc 01 §2

**Acceptance criteria**
- [ ] `/etc/exports` generated per share with hosts, subnets and squash options; golden files
- [ ] Invalid host or subnet entries rejected at the API with a clear message
- [ ] L3 test mounts an export from the test runner and writes a file

**Scope** `internal/share/`

### Ownership and UID/GID model

```meta
id: M4.3
epic: M4
status: todo
labels: [feat, area:shares]
sudo: false
depends: [M4.1]
issue: 48
```

**Summary** The Q26 model: group `users` as the shared data group, setgid
share directories, Samba masks, and a `hoserva-apps` user pinned to UID 99 so
migrated data and templates keep working unchanged.

**Design references** Q26, doc 04 §5 (Ownership variables), doc 05 §4

**Acceptance criteria**
- [ ] Share directories `2775` with group `users`; Samba create and directory masks `0664`/`2775`
- [ ] `hoserva-apps` created at UID 99 when free; a pre-flight reports when it isn't
- [ ] Existing file ownership is never rewritten
- [ ] Lab test: files written over SMB and by a container as `99:100` are mutually readable and writable

**Scope** `internal/share/`, `packaging/`

### Users, roles and Samba accounts

```meta
id: M4.4
epic: M4
status: todo
labels: [feat, area:api]
sudo: false
depends: [M1.11, M4.1]
issue: 49
```

**Summary** Admin, Viewer and Share-only roles, with one password action that
updates both the UI credential and the Samba passdb entry, and per-user share
permissions.

**Design references** Q27, doc 03 §7

**Acceptance criteria**
- [ ] New users default to Share-only, with no UI login
- [ ] Setting a password writes the UI hash and the Samba passdb entry in one operation, rolled back together on failure
- [ ] Per-user and per-group share access (none, read-only, read-write) editable from the user or the share
- [ ] Viewer role cannot reach any mutating endpoint — tested per route
- [ ] Session list with revoke

**Scope** `internal/api/`, `internal/share/`

### API tokens

```meta
id: M4.5
epic: M4
status: todo
labels: [feat, area:api]
sudo: false
depends: [M4.4]
issue: 50
```

**Summary** Personal, role-scoped API tokens for scripting and the remote
CLI.

**Design references** Q43, doc 01 §5

**Acceptance criteria**
- [ ] Tokens scoped to admin or viewer, shown once at creation, stored hashed
- [ ] Revocation takes effect immediately
- [ ] The CLI accepts a token and a host for remote use over TLS
- [ ] Token use recorded in the audit log

**Scope** `internal/api/`, `cmd/hoserva/`

### UI: shares and users

```meta
id: M4.6
epic: M4
status: todo
labels: [feat, area:web]
sudo: false
depends: [M4.1, M4.2, M4.4, M4.5]
issue: 51
```

**Summary** `/shares`, `/shares/[name]` and `/users` with the components doc
03 names.

**Design references** doc 03 §4.1, §4.2, §7 (and their Components lines)

**Acceptance criteria**
- [ ] Share detail tabs for general, allocation, cache, SMB, NFS, browse and danger zone
- [ ] Danger zone separates removing the definition (`confirm`) from deleting data (`typed-confirm`)
- [ ] Users page makes UI-login and SMB access explicit per account
- [ ] Browsing warns that it may wake disks before the first listing

**Scope** `web/src/routes/`

---

## M5 — Phase 2: cache, mover and relocation

```epic
id: M5
phase: P2
status: todo
labels: [feat, area:storage]
issue: 52
```

The piece of original engineering where a bug loses data (doc 09): the mover,
share relocation, rebalance and evacuation — copy-verify-delete, two-phase on
the array, writing through mergerfs so there is one placement algorithm.
Every item here writes its data-loss test before its implementation.

### Mover

```meta
id: M5.1
epic: M5
status: todo
labels: [feat, area:storage, safety-critical]
sudo: false
depends: [M2.4, M2.8]
issue: 53
```

**Summary** Relocate cache-then-move shares' files from cache to the array,
never moving an open file, writing through the array-only mergerfs mount.

**Design references** doc 09 §2, doc 02 §3, Q29, Q30, `CLAUDE.md` "One placement algorithm"

**Acceptance criteria**
- [ ] Algorithm per doc 09 §2: open-handle check including mergerfs's own descriptors, grace period, space pre-check, temp-suffixed copy, verify, fsync, atomic rename, re-check, unlink
- [ ] Resumable Array-write job with checkpoints; interrupted runs leave a duplicate, never a gap
- [ ] First step of the nightly chain; threshold-triggered and manual runs
- [ ] Every doc 09 §6 mover test passes in the lab, including an open file held through `/mnt/user/<share>`
- [ ] Run report lists moved, skipped and why

**Scope** `internal/cache/`

### Share relocation

```meta
id: M5.2
epic: M5
status: todo
labels: [feat, area:storage, safety-critical]
sudo: false
depends: [M5.1, M2.6]
issue: 54
```

**Summary** Move a whole share between cache and array when its cache mode
changes, two-phase whenever array originals are deleted.

**Design references** doc 09 §2 (Share relocation), Q14, Q15

**Acceptance criteria**
- [ ] Cache → array behaves as a mover run limited to one share, without the grace period
- [ ] Array → cache copies, verifies, syncs through the guard, then deletes the array originals, with a manifest so the guard accounts the removals
- [ ] Containers using the share listed with an offer to stop them before starting
- [ ] Lab test: interrupt between copy and sync, fail a different data disk, reconstruct fully

**Scope** `internal/cache/`

### Rebalance

```meta
id: M5.3
epic: M5
status: todo
labels: [feat, area:storage, safety-critical]
sudo: false
depends: [M5.1, M2.6]
issue: 55
```

**Summary** Even out fill across data disks on request, two-phase so no
source is deleted before the sync that protects its copy.

**Design references** doc 09 §3, Q14, Q15

**Acceptance criteria**
- [ ] Plan computed and shown before starting; never automatic
- [ ] Copy and verify, sync through the guard, delete sources, sync again
- [ ] The UI-facing plan warns when it spreads a path-preserving share across disks
- [ ] Lab tests from doc 09 §6: skewed pool evened out; no source deleted before its sync; evacuation-style removals don't trip the guard while an unrelated mass deletion still does

**Scope** `internal/cache/`

### Disk evacuation and removal

```meta
id: M5.4
epic: M5
status: todo
labels: [feat, area:storage, safety-critical]
sudo: false
depends: [M5.3, M2.9]
issue: 56
```

**Summary** Remove a disk from the pool by moving everything off it first, in
doc 09 §4's nine steps, resumable across a day-long run.

**Design references** doc 09 §4, Q14, Q15, Q29

**Acceptance criteria**
- [ ] Pre-check that remaining disks fit everything with `minfreespace` respected
- [ ] `removing` state sets `NC` on every mount and suspends the zero-files guard rule for that disk only
- [ ] Sources deleted only after the covering sync; post-check that only empty directories remain
- [ ] Removed from branch lists and SnapRAID with `--force-empty` for that disk only, then unmounted
- [ ] Resumes from its checkpoint after a daemon restart on user action
- [ ] Lab test: full evacuation and clean remount without the disk

**Scope** `internal/cache/`, `internal/pool/`, `internal/parity/`

### Free-space accounting and per-disk alerts

```meta
id: M5.5
epic: M5
status: todo
labels: [feat, area:storage]
sudo: false
depends: [M2.1, M3.1]
issue: 57
```

**Summary** Report pool-free, largest-single-disk-free and per-disk space
everywhere, and alert on per-disk thresholds, not only the pool total.

**Design references** doc 09 §5, doc 09 §1 (`minfreespace`), doc 03 §3.2

**Acceptance criteria**
- [ ] API exposes pool free, largest single-disk free and per-disk free, with disks near `minfreespace` flagged
- [ ] Per-disk alert fires when any disk nears `minfreespace`
- [ ] ENOSPC under a path-preserving policy detected and surfaced as a rebalance suggestion
- [ ] Figures come from filesystem stats, never a directory walk

**Scope** `internal/pool/`, `internal/notify/`

### UI: cache page and relocation flows

```meta
id: M5.6
epic: M5
status: todo
labels: [feat, area:web]
sudo: false
depends: [M5.1, M5.2, M5.3, M5.4]
issue: 58
```

**Summary** `/storage/cache` plus the rebalance, remove-disk and cache-mode
change flows on the pool and share pages.

**Design references** doc 03 §3.2, §3.6, §4.2 (and their Components lines)

**Acceptance criteria**
- [ ] Cache page shows fill, breakdown, per-share modes, the last mover run and the skipped-because-in-use list
- [ ] The not-covered-by-parity warning links to appdata backup status
- [ ] Changing a cache mode offers the relocation job; nothing moves silently
- [ ] Remove disk and rebalance show their plan before a `typed-confirm`

**Scope** `web/src/routes/`

---

## M6 — Phase 2: backup and restore

```epic
id: M6
phase: P2
status: todo
labels: [feat, area:backup]
issue: 59
```

The backups only Hoserva can take correctly (doc 10): config across multiple
destinations, appdata with the right stop order, restore in place and on bare
metal, and a restore drill so a backup is never just a hypothesis.

### Backup destinations

```meta
id: M6.1
epic: M6
status: todo
labels: [feat, area:backup]
sudo: false
depends: [M3.2]
issue: 60
```

**Summary** Multiple destinations per backup — local path, SMB/NFS, S3, SFTP,
WebDAV and rclone remotes — with retention, encryption and a real test
connection.

**Design references** doc 10 §1 (Multi-destination), Q41, D1

**Acceptance criteria**
- [ ] Local paths work without rclone; remote types go through `rclone copy` with structured arguments only
- [ ] When rclone is missing, the API reports the install command instead of failing opaquely
- [ ] Test connection writes, reads back and deletes a file
- [ ] Retention pruned per destination; the last successful backup per destination tracked and stale destinations alerted

**Scope** `internal/backup/`

### Appdata backup

```meta
id: M6.2
epic: M6
status: todo
labels: [feat, area:backup]
sudo: false
depends: [M6.1]
issue: 61
```

**Summary** Per-container appdata archives with a stop policy, minimal
downtime, and known database images flagged — including the small Docker
Engine client for stop and start that Phase 3 extends.

**Design references** doc 10 §2, doc 04 §3, Q62

**Acceptance criteria**
- [ ] Docker Engine API client for list, stop and start, with version negotiation (Q38); no `docker` CLI string building
- [ ] Containers stopped per policy, archived locally, restarted in reverse order, then uploaded
- [ ] Known database images flagged when a user opts out of stopping them
- [ ] Per-container archives; per-container restore with a pre-restore snapshot
- [ ] Service-class job; weekly by default once the first container exists

**Scope** `internal/backup/`, `internal/container/`

### Restore: in place and bare metal

```meta
id: M6.3
epic: M6
status: todo
labels: [feat, area:backup, safety-critical]
sudo: false
depends: [M3.2, M2.3, M1.7]
issue: 62
```

**Summary** Roll back a bad config change, or rebuild a whole system from an
archive onto a fresh install — the "OS is disposable" claim made real.

**Design references** doc 10 §1 (Restore), doc 01 §6, D16, Q21, Q28

**Acceptance criteria**
- [ ] In-place restore previews every change before applying
- [ ] Bare metal: manifest compatibility check, older archives upgraded by the schema-migration runner, newer ones refused
- [ ] Disks matched by recorded identity and the mapping confirmed before any mount; absent or replaced disks reported
- [ ] Without the backup passphrase everything but secrets restores, and the result says so
- [ ] Never formats a disk; never writes to a data disk before the mapping is confirmed

**Scope** `internal/backup/`

### Backup verification and restore drill

```meta
id: M6.4
epic: M6
status: todo
labels: [feat, area:backup]
sudo: false
depends: [M6.1]
issue: 63
```

**Summary** A monthly automated drill that extracts the newest backup,
validates it and discards it, alerting loudly when it fails.

**Design references** doc 10 §1 (Verification), §4

**Acceptance criteria**
- [ ] Drill extracts to a temporary directory, checks checksums and schema, then deletes everything it extracted
- [ ] Failure raises a high-priority notification
- [ ] Last drill result exposed through the API for the dashboard

**Scope** `internal/backup/`

### UI: backup and restore settings

```meta
id: M6.5
epic: M6
status: todo
labels: [feat, area:web]
sudo: false
depends: [M6.1, M6.2, M6.3, M6.4]
issue: 64
```

**Summary** `/settings/backup` with destinations, config backup, restore,
appdata backup and the drill.

**Design references** doc 03 §8.5 (and its Components line), doc 10

**Acceptance criteria**
- [ ] Destinations table with test connection and add-destination form
- [ ] Restore uploads an archive, previews its contents grouped, then requires `typed-confirm`
- [ ] Appdata stop policy per container with database images flagged
- [ ] Drill status shown; stale destinations surface on the dashboard's attention row

**Scope** `web/src/routes/`

### Bare-metal restore in the L3 suite

```meta
id: M6.6
epic: M6
status: todo
labels: [chore, area:devenv]
sudo: false
depends: [M6.3, M3.9]
issue: 65
```

**Summary** Make "export, wipe, reinstall, import, identical system" a
routine nightly test rather than an assumption.

**Design references** doc 06 §4 (What runs here), doc 01 §6, doc 10 §1

**Acceptance criteria**
- [ ] Nightly job: configure an array with shares and data, export config, destroy the VM's OS disk, install fresh, import
- [ ] Asserts shares, users, schedules and pool mounts match and file checksums are unchanged
- [ ] Runs on the trusted runner as part of the L3 suite

**Scope** `scripts/vm/`, `.github/workflows/`

---

## M7 — Phase 3: apps

```epic
id: M7
phase: P3
status: todo
labels: [feat, area:containers]
issue: 66
```

Container management, deliberately narrow (D6): the path from "I want
Jellyfin" to "Jellyfin is running against my media share", with plain Compose
files on disk (D7), a curated catalog, the Unraid template converter (D12),
and safe updates.

### Docker Engine integration and Compose stacks

```meta
id: M7.1
epic: M7
status: todo
labels: [feat, area:containers]
sudo: false
depends: [M6.2]
issue: 67
```

**Summary** Full lifecycle for plain Compose stacks under
`/var/lib/hoserva/stacks/`, alongside unmanaged containers, with Docker's
storage on a plain directory.

**Design references** doc 04 §1–§3, D6, D7, D8, Q38, Q62

**Acceptance criteria**
- [ ] Stacks stored as `docker-compose.yml`, `.env` and `meta.json`; `docker compose` runs with argv only
- [ ] Start, stop, restart, recreate, remove (appdata deletion a separate choice), logs, stats, health
- [ ] Unmanaged containers listed and controllable, never modified by the template system
- [ ] Prerequisite detection: missing Docker or Compose v2 reported with install commands; Engine API version negotiated
- [ ] Docker data-root on a plain directory on cache, never a loopback image (Q62)

**Scope** `internal/container/`

### Template model and curated catalog

```meta
id: M7.2
epic: M7
status: todo
labels: [feat, area:containers]
sudo: false
depends: [M7.1]
issue: 68
```

**Summary** The template format — Compose files with an `x-hoserva` block — the
seeded curated catalog in `templates/`, CI validation and signed publishing, and
the privilege summary.

**Design references** doc 04 §7, §1, Q39, Q64, Q65, Q26, D19, doc 01 §7 (Container privilege warnings)

**Acceptance criteria**
- [ ] `x-hoserva` schema version 1 per doc 04 §7: inputs with kind, path role and default; metadata; revision
- [ ] Seeded with the doc 04 §7 app list, each written from its official or linuxserver.io image documentation, with appdata on cache, explicit tags where published, `PUID=99`/`PGID=100` where supported
- [ ] CI validates the schema, `docker compose config`, image and tag existence, path conventions and privileges on every change to `templates/`
- [ ] On merge, CI builds `catalog.tar.zst` with its `index.json`, signs it with a key held only as a CI secret, and publishes it as static files; the serial only ever increases
- [ ] Privilege summary computed from the Compose content for privileged mode, host networking, Docker socket and paths outside the pool
- [ ] Install resolves inputs, writes Compose and `.env` with port-conflict detection and share-aware path defaults, and records source, id and revision in `meta.json`

**Scope** `internal/template/`, `templates/`, `.github/workflows/`

### Unraid XML template converter

```meta
id: M7.3
epic: M7
status: todo
labels: [feat, area:containers]
sudo: false
depends: [M7.2]
issue: 69
```

**Summary** Convert Unraid XML templates to reviewable Compose, translating
known `ExtraParams`, never dropping anything silently, and tracking the
clean-conversion rate as a release metric.

**Design references** doc 04 §5, D12, Q36, Q37, doc 06 §2

**Acceptance criteria**
- [ ] Field mapping per doc 04 §5; `ExtraParams` parsed with a flag parser, never a regex and never a shell
- [ ] Untranslatable flags emitted as a Compose comment and a warning; flagged paths (`/boot`, `/mnt/disks/`, `/mnt/user0`, other pools) listed for review
- [ ] Missing networks reported with the exact `docker network create` command
- [ ] `make test-corpus` converts the project-authored template corpus in `testdata/unraid-templates/` (doc 06 §2); the clean rate is computed and CI fails if it regresses
- [ ] Output always shown beside the source XML before anything runs

**Scope** `internal/template/`, `testdata/unraid-templates/`

### Catalog distribution and sources

```meta
id: M7.4
epic: M7
status: todo
labels: [feat, area:containers]
sudo: false
depends: [M7.2]
issue: 70
```

**Summary** How the catalog reaches an installation and stays current: the
embedded snapshot, the signed daily refresh, template-update diffs, and catalog
source URLs a user adds (D19).

**Design references** doc 04 §4, §7, Q33, Q64, Q65, D19, doc 01 §7

**Acceptance criteria**
- [ ] One source interface; the curated catalog is the only built-in source and is on by default
- [ ] `hoservad` embeds a catalog snapshot at build time and works offline from the on-disk copy in `/var/lib/hoserva/catalog/`
- [ ] Refresh is one conditional request a day with jitter, plus manual refresh; an unchanged catalog downloads nothing; refresh can be disabled
- [ ] A catalog replaces the on-disk copy only if its signature verifies against the compiled-in key and its serial is higher; a failed check keeps the previous catalog and notifies
- [ ] A newer template revision is offered as "template update available" with a diff against the installed Compose file; nothing changes without the user's action
- [ ] A user can add, refresh and remove a catalog source URL in the same archive format; its entries are badged as user-added, and unsigned sources as unsigned
- [ ] Every entry shows its source

**Scope** `internal/template/`

### Updates and rollback

```meta
id: M7.5
epic: M7
status: todo
labels: [feat, area:containers]
sudo: false
depends: [M7.1, M6.2]
issue: 71
```

**Summary** Detect new builds and new versions separately, update in bulk
with opt-outs, snapshot appdata first, and offer a one-click revert.

**Design references** doc 04 §6, doc 10 §2 (Schedule)

**Acceptance criteria**
- [ ] Registry polling respects rate limits and distinguishes a digest change on the same tag from a new version tag
- [ ] Pre-update appdata snapshot before every update of a container with appdata on cache
- [ ] Previous image kept for a configurable period; revert restores image and snapshot
- [ ] Updates run as Service-class jobs

**Scope** `internal/container/`, `internal/backup/`

### UI: apps, catalog, install wizard and Compose editor

```meta
id: M7.6
epic: M7
status: todo
labels: [feat, area:web]
sudo: false
depends: [M7.2, M7.3, M7.5]
issue: 72
```

**Summary** `/apps` and all its sub-pages as doc 03 §5 specifies.

**Design references** doc 03 §5.1–§5.6 (and their Components lines)

**Acceptance criteria**
- [ ] Installed view with unmanaged badges, port links, bulk actions and the Docker prerequisite banner
- [ ] Catalog with search, filters, source badges and privilege summaries
- [ ] Install wizard with conflict-checked ports, share-aware paths, secret inputs and a Compose preview with converter warnings
- [ ] Compose editor marks stacks manually edited and guards unapplied changes
- [ ] Container start, stop and logs usable on a phone

**Scope** `web/src/routes/`

---

## M8 — Phase 3: Unraid migration

```epic
id: M8
phase: P3
status: todo
labels: [feat, area:migration]
issue: 73
```

The guided, low-risk path from an Unraid array (doc 05): scan, adopt the data
disks without copying, verify with checksums, and only then cross the point of
no return. Never destructive before it, and every supported variant and every
refusal backed by a fixture.

### Unraid source fixtures in L3

```meta
id: M8.1
epic: M8
status: todo
labels: [chore, area:devenv]
sudo: false
depends: [M3.9]
issue: 74
```

**Summary** Build the synthetic Unraid source fixtures every migration test
starts from, including the refusal fixtures — without running Unraid.

**Design references** doc 06 §5 (Building Unraid fixtures without Unraid, Variant fixtures), doc 05 §2, Q22, Q23, Q24, D20

**Acceptance criteria**
- [ ] `unraid-fixtures` built synthetically per doc 06 §5 — Unraid partition layout and filesystems, share directories with seeded data and varied cache settings, a flash tree with authored `templates-user/` XML — and snapshotted
- [ ] Optional calibration against an anonymised Unraid Diagnostics zip at a gitignored local path, with divergences recorded in doc 05; nothing from it committed
- [ ] Variant snapshots from doc 06 §5's list, including `unraid-encrypted`, `unraid-zfs-disk` and `unraid-with-vms`
- [ ] Each fixture records per-disk file counts, sizes and checksums plus its Flash Backup zip
- [ ] Build steps scripted and documented; no third-party templates committed; no agent runs Unraid or connects to a real Unraid server

**Scope** `scripts/devenv/`, `scripts/vm/`, `testdata/`, `.gitignore`

### Migration scan

```meta
id: M8.2
epic: M8
status: todo
labels: [feat, area:migration]
sudo: false
depends: [M2.1, M8.1]
issue: 75
```

**Summary** `hoserva migrate scan`: read the Flash Backup or a read-only stick,
run every pre-flight check, and produce the written go/no-go report.

**Design references** doc 05 §3, Q21, Q22, Q23, Q24, Q25, Q26

**Acceptance criteria**
- [ ] Reads configuration from the Flash Backup zip or a stick mounted read-only; a test proves the stick is never written
- [ ] Every check in doc 05 §3's table, with its pass condition and failure behaviour
- [ ] Unknown Unraid versions or layouts refused unless `--unverified-layout`, which is recorded in the report
- [ ] Filesystem checks run read-only; a failing disk is refused and the rest proceed
- [ ] Baseline counts, sizes and sample checksums per disk and share recorded for verify
- [ ] Every doc 05 §2 variant fixture produces the expected report

**Scope** `internal/migrate/`

### Disk adoption and configuration seeding

```meta
id: M8.3
epic: M8
status: todo
labels: [feat, area:migration, safety-critical]
sudo: false
depends: [M8.2, M2.4, M4.1, M4.3, M4.4]
issue: 76
```

**Summary** Adopt the Unraid data disks into the pool without formatting, and
seed shares, allocation policies and users from the backup.

**Design references** doc 05 §4 (Phase C), Q11, Q25, Q26, D10

**Acceptance criteria**
- [ ] The disk-role mapping is confirmed by the user against serials before anything mounts
- [ ] Data disks mounted and unioned as-is; no data-disk write before verify passes
- [ ] Shares seeded with allocation methods mapped to create policies (Q11), High-water flagged
- [ ] Users recreated; passwords set by the user, never migrated
- [ ] Former parity disks untouched until the point of no return
- [ ] Rollback test: re-attach the Unraid stick after import and the array returns unchanged

**Scope** `internal/migrate/`

### Verify phase and the point of no return

```meta
id: M8.4
epic: M8
status: todo
labels: [feat, area:migration, safety-critical]
sudo: false
depends: [M8.3, M2.5, M2.6]
issue: 77
```

**Summary** Compare the adopted pool with the scan baseline using checksums,
and gate parity initialisation behind an explicit, typed point-of-no-return
confirmation.

**Design references** doc 05 §4 (steps 16–17), §5, Q19, Q20

**Acceptance criteria**
- [ ] Per disk and per share: counts, sizes and sample checksums compared with the baseline; any mismatch stops the flow
- [ ] Initialise parity is offered only after a green verify, with the unprotected-window warning
- [ ] Parity disks reformatted XFS and the initial sync run through the guard as a job
- [ ] The API refuses parity initialisation without a completed, passing verify
- [ ] Migration suite step 5 asserts every file present with matching checksums

**Scope** `internal/migrate/`

### Services after migration

```meta
id: M8.5
epic: M8
status: todo
labels: [feat, area:migration]
sudo: false
depends: [M8.4, M5.2, M7.3]
issue: 78
```

**Summary** Phase D: move appdata back to cache, convert the user's
templates, and bring containers back one at a time.

**Design references** doc 05 §4 (Phase D), doc 09 §2 (Share relocation), doc 04 §5

**Acceptance criteria**
- [ ] The `appdata` share relocation runs as a job with its usual guarantees
- [ ] `templates-user/` templates converted with warnings shown before any container starts
- [ ] Containers started one at a time, each confirming it sees its data
- [ ] A post-migration checklist covers scrub, notifications, schedules and a restore drill

**Scope** `internal/migrate/`

### UI: migration workspace

```meta
id: M8.6
epic: M8
status: todo
labels: [feat, area:web]
sudo: false
depends: [M8.2, M8.3, M8.4]
issue: 79
```

**Summary** `/tools/migrate` with its four phases, the report, and the
unprotected-window warning impossible to miss.

**Design references** doc 05 §6, doc 03 §9.3 (and its Components line)

**Acceptance criteria**
- [ ] Scan, review, import and verify as a `wizard`, resumable across sessions
- [ ] Review shows the editable disk mapping and share and template previews with warning counts
- [ ] The unprotected-window banner cannot be dismissed
- [ ] Initialise parity uses `typed-confirm`

**Scope** `web/src/routes/`

### Migration suite in nightly CI

```meta
id: M8.7
epic: M8
status: todo
labels: [chore, area:devenv]
sudo: false
depends: [M8.1, M8.5]
issue: 80
```

**Summary** Run doc 06 §5's migration test procedure against every variant
fixture nightly and before releases.

**Design references** doc 06 §5 (Migration test procedure), §7, doc 05 §2

**Acceptance criteria**
- [ ] All nine procedure steps automated against each supported variant
- [ ] Refusal fixtures assert the right refusal message and that nothing was written
- [ ] A red migration suite blocks the next release

**Scope** `scripts/vm/`, `.github/workflows/`

---

## M9 — Phase 3: documentation site

```epic
id: M9
phase: P3
status: todo
labels: [docs, area:site]
issue: 81
```

The public documentation site (doc 05 §7), starting with what migrating users
need and the honest concepts pages — parity is not backup, what happens when
a disk dies.

### Documentation site scaffold

```meta
id: M9.1
epic: M9
status: todo
labels: [chore, area:site]
sudo: false
depends: [M1.1]
issue: 82
```

**Summary** An Astro Starlight site under `site/` with the doc 05 §7
structure, built in CI.

**Design references** Q3, doc 05 §7 (Structure), doc 12 §7

**Acceptance criteria**
- [ ] Starlight project under `site/`, built on every push that touches it
- [ ] Navigation matches doc 05 §7's outline, with placeholder pages marked as drafts
- [ ] No external analytics or font CDNs
- [ ] The Unraid trademark notice appears wherever Unraid is named (doc 00 §6)

**Scope** `site/`, `.github/workflows/`

### Migration guide

```meta
id: M9.2
epic: M9
status: todo
labels: [docs, area:site]
sudo: false
depends: [M9.1, M8.4]
issue: 83
```

**Summary** The `/migrating-from-unraid/` pages, written against the real
migrator's behaviour.

**Design references** doc 05 §4, §5, §7 (Tone requirements), doc 00 §6

**Acceptance criteria**
- [ ] Before-you-start checklist, step-by-step sequence, unprotected window and rollback, special cases and troubleshooting
- [ ] The unprotected window stated in the terms doc 05 §5 requires, at the point it is crossed
- [ ] Factual about Unraid throughout; no comparisons beyond what migration needs

**Scope** `site/`

### Concepts and guides

```meta
id: M9.3
epic: M9
status: todo
labels: [docs, area:site]
sudo: false
depends: [M9.1]
issue: 84
```

**Summary** The concepts and guides sections: pooling, parity and its timing,
parity is not backup, cache and mover, disk failure, spindown, and the
day-to-day guides.

**Design references** doc 05 §7, doc 10 §3 (The stated posture), doc 02 §1, §2

**Acceptance criteria**
- [ ] Concepts pages from doc 05 §7, including the throughput ceiling and the sync-timing caveat stated plainly
- [ ] Guides for adding a disk, replacing a failed disk, recovering files and its time limit, backing up, and exposing the UI safely
- [ ] Each page links to the UI and CLI it describes

**Scope** `site/`

### API reference

```meta
id: M9.4
epic: M9
status: todo
labels: [docs, area:site]
sudo: false
depends: [M9.1, M1.6]
issue: 85
```

**Summary** The `/reference/api` pages generated from `api/openapi.yaml` on
every site build, so the published reference is always exactly the API.

**Design references** D18, Q63, doc 05 §7, doc 01 §5

**Acceptance criteria**
- [ ] starlight-openapi renders every operation, schema and SSE event type from the committed spec
- [ ] Each operation shows its required role and an example call with an API token over TLS
- [ ] The site build fails if the spec doesn't validate
- [ ] A short guide walks through creating a token and scripting one common task end to end

**Scope** `site/`

---

## M10 — Phase 3.5: virtual machines

```epic
id: M10
phase: P3.5
status: todo
labels: [feat, area:vm]
issue: 86
```

VM management on libvirt/KVM, deliberately narrow (doc 14, D13, D14): local
lifecycle, disks on the pool or cache, bridged networking, a browser console
over the existing session, PCI/USB passthrough behind a compatibility check,
and Unraid VM import. Gated on spikes S10 and S11.

### S10 — Nested KVM for testing VM management

```meta
id: M10.1
epic: M10
status: todo
labels: [spike, area:devenv]
sudo: false
depends: [M3.9, M0.9]
issue: 87
```

**Summary** Find out whether an L3 test VM can run nested KVM for a domain
Hoserva-under-test creates, on the dev host and on CI runners.

**Design references** doc 14 §8, doc 06 §4 (Testing Hoserva's own VM management), doc 07 §1 (S10)

**Acceptance criteria**
- [ ] A guest domain boots inside the L3 VM with KVM acceleration on the dev host
- [ ] Same test on a hosted runner and on the self-hosted runner, results recorded
- [ ] Doc 06 §7's pipeline row for the VM suite updated with where it runs

**Scope** `spikes/s10/`, `docs/internal/08-spike-findings.md`

### S11 — Unraid domain XML compatibility

```meta
id: M10.2
epic: M10
status: todo
labels: [spike, area:vm]
sudo: false
depends: [M8.1]
issue: 88
```

**Summary** Measure how much of real Unraid VM domain XML loads on Debian 13's
libvirt with only doc 14 §5's remapping.

**Design references** doc 14 §5, Q55, doc 07 §1 (S11)

**Acceptance criteria**
- [ ] Domain XML read from `libvirt.img` on the `unraid-with-vms` fixture's array, read-only
- [ ] Each domain defined on Debian 13 libvirt after remapping; failures classified by field
- [ ] `libvirt.img` location and layout per supported Unraid version recorded (Q24)
- [ ] Doc 14 §5's remap table and Q55 updated with the findings

**Scope** `spikes/s11/`, `docs/internal/14-virtual-machines.md`

### VM engine and domain XML generation

```meta
id: M10.3
epic: M10
status: todo
labels: [feat, area:vm]
sudo: false
depends: [M1.4, M1.5, M1.7]
issue: 89
```

**Summary** `vm.Engine` on go-libvirt with a scriptable fake, and domain XML
generated from SQLite state and tested with golden files.

**Design references** doc 14 §2, §7, D13, Q58, D4

**Acceptance criteria**
- [ ] `vm.Engine` interface per doc 14 §2 with a fake covering state transitions, IOMMU groups and console sockets
- [ ] Real implementation over go-libvirt, no cgo
- [ ] Domain XML is pure state-in, text-out with golden files for representative VMs
- [ ] `libvirt-daemon-system` and `qemu-system-x86` added as package dependencies (Q58)

**Scope** `internal/vm/`, `testdata/configs/`, `packaging/`

### VM lifecycle and the VM job class

```meta
id: M10.4
epic: M10
status: todo
labels: [feat, area:vm]
sudo: false
depends: [M10.3, M1.8]
issue: 90
```

**Summary** Create, start, stop, restart, clone and delete VMs as VM-class
jobs, with autostart and CPU pinning.

**Design references** doc 14 §1 (In scope), §2 (Job system), Q56, doc 01 §3

**Acceptance criteria**
- [ ] Every lifecycle action is a VM-class job, exclusive per VM
- [ ] Graceful ACPI shutdown with a force option; delete keeps or removes disks as chosen
- [ ] Autostart on boot and CPU pinning
- [ ] `hoserva vm` commands from doc 01 §3 through the API
- [ ] Tested against the fake at L1 and nested KVM where S10 allows

**Scope** `internal/vm/`, `cmd/hoserva/`

### VM storage and disk relocation

```meta
id: M10.5
epic: M10
status: todo
labels: [feat, area:vm, safety-critical]
sudo: false
depends: [M10.4, M5.2]
issue: 91
```

**Summary** vdisks under `/mnt/user/domains`, never moved while a VM runs, and
the stopped-VM relocation between cache and array.

**Design references** doc 14 §2 (Storage placement), doc 09 §2 (VM disk relocation), Q51, Q52, Q14

**Acceptance criteria**
- [ ] qcow2 vdisks created sparse under `/mnt/user/domains/<vm>/`
- [ ] The mover skips any vdisk a running domain holds open
- [ ] Relocation refuses a running VM; array-involved moves follow the two-phase order
- [ ] Domain disk shares marked as not diff-protected inside the VM (Q52)
- [ ] Lab test: relocation interrupted between copy and sync leaves the VM bootable from the original

**Scope** `internal/vm/`, `internal/cache/`

### Bridged networking

```meta
id: M10.6
epic: M10
status: todo
labels: [feat, area:vm]
sudo: false
depends: [M10.4]
issue: 92
```

**Summary** A generated `vmbr0` bridge over the host NIC so VMs get LAN
addresses, plus an isolated network option.

**Design references** doc 14 §4 (Networking), Q54

**Acceptance criteria**
- [ ] Bridge configuration generated as a managed file, applied without cutting the management connection
- [ ] A failed bridge apply rolls back to the previous network configuration
- [ ] Isolated/NAT network available per VM
- [ ] L3 test: a guest gets a DHCP address on the test network

**Scope** `internal/vm/`, `internal/config/`

### Browser console

```meta
id: M10.7
epic: M10
status: todo
labels: [feat, area:vm]
sudo: false
depends: [M10.4, M1.11]
issue: 93
```

**Summary** Proxy each VM's graphics console over the authenticated TLS
connection to an embedded noVNC viewer; no VNC port is ever exposed.

**Design references** doc 14 §4 (Console), Q57, doc 01 §7

**Acceptance criteria**
- [ ] libvirt graphics bound to a local socket only
- [ ] WebSocket proxy requires the session and a role that may use the VM
- [ ] A test asserts no VNC or SPICE port listens on any non-loopback address
- [ ] Console access audit-logged

**Scope** `internal/vm/`, `internal/api/`

### IOMMU detection and passthrough check

```meta
id: M10.8
epic: M10
status: todo
labels: [feat, area:vm]
sudo: false
depends: [M10.3]
issue: 94
```

**Summary** Read-only IOMMU group listing and the `passthrough check` report,
marking host-critical devices unassignable by construction.

**Design references** doc 14 §3, Q53, R14

**Acceptance criteria**
- [ ] Groups, member devices and ACS isolation read from sysfs without changing anything
- [ ] The boot controller, and the console GPU when it is the only one, are never assignable
- [ ] `hoserva vm passthrough check` gives a plain-language verdict per device, naming single-GPU setups explicitly
- [ ] Parser tests against sysfs trees captured from real machines

**Scope** `internal/vm/`, `testdata/parsers/`

### VFIO binding and device assignment

```meta
id: M10.9
epic: M10
status: todo
labels: [feat, area:vm, safety-critical]
sudo: false
depends: [M10.8]
issue: 95
```

**Summary** Assign a device to a VM through a generated, boot-time VFIO
configuration that takes effect on reboot — never a live unbind.

**Design references** doc 14 §3 (IOMMU groups and VFIO binding), Q53, R14, `CLAUDE.md` safety-critical list

**Acceptance criteria**
- [ ] IOMMU kernel parameter and `vfio-pci` device list generated as managed files, with a pending-reboot state
- [ ] Assignment refused for any device the check marks unassignable or that shares a group with one
- [ ] Removing an assignment restores the previous boot configuration
- [ ] Attach and detach audit-logged
- [ ] Verified in a nested L3 guest with an emulated IOMMU for one PCI and one USB device (doc 06 §6); generated files golden-tested at L1

**Scope** `internal/vm/`, `internal/config/`

### VM backup

```meta
id: M10.10
epic: M10
status: todo
labels: [feat, area:backup]
sudo: false
depends: [M10.4, M6.1]
issue: 96
```

**Summary** The off-by-default VM backup job from doc 10 §5: stop, copy domain
XML and vdisks to a destination, verify, restart.

**Design references** doc 10 §5, doc 14 §2

**Acceptance criteria**
- [ ] Stops the VM, copies domain XML and vdisks through the destination system, verifies, restarts
- [ ] Off by default; weekly when enabled; automatic before material domain changes
- [ ] Restore defines the domain and restores vdisks after confirmation
- [ ] The UI states that snapshots are not backups

**Scope** `internal/backup/`, `internal/vm/`

### Unraid VM import

```meta
id: M10.11
epic: M10
status: todo
labels: [feat, area:migration, safety-critical]
sudo: false
depends: [M10.2, M10.4, M10.8, M8.4]
issue: 97
```

**Summary** `hoserva migrate vm-scan` and `vm-import`: read domain definitions
from `libvirt.img` on the adopted pool, remap the divergent fields, and
re-validate passthrough against this machine.

**Design references** doc 14 §5, doc 05 §1.4 and step 26, Q55

**Acceptance criteria**
- [ ] `libvirt.img` attached read-only; a non-default location read from the Flash Backup; a missing image reported, not guessed
- [ ] Bridge name and firmware paths remapped; passthrough addresses re-validated against this machine's IOMMU groups
- [ ] vdisks adopted in place and covered by the verify phase; nothing copied
- [ ] Rewritten XML shown beside the source; nothing autostarts
- [ ] Migration suite covers `unraid-with-vms`

**Scope** `internal/migrate/`, `internal/vm/`

### UI: VM pages

```meta
id: M10.12
epic: M10
status: todo
labels: [feat, area:web]
sudo: false
depends: [M10.4, M10.7, M10.8, M10.9]
issue: 98
```

**Summary** `/vms`, `/vms/create`, `/vms/[name]` and `/vms/passthrough` as
doc 03 §11 specifies.

**Design references** doc 03 §11 (and its Components lines), doc 14

**Acceptance criteria**
- [ ] VM list with state, autostart and console access
- [ ] Create form with path picker, network choice, passthrough candidates with verdicts, and a domain XML preview
- [ ] Console tab embeds the noVNC viewer behind the session
- [ ] Passthrough page shows groups read-only, unassignable reasons inline, and a pending-reboot banner

**Scope** `web/src/routes/`

### VM suite in nightly CI

```meta
id: M10.13
epic: M10
status: todo
labels: [chore, area:devenv]
sudo: false
depends: [M10.1, M10.4, M10.5]
issue: 99
```

**Summary** Nightly nested-KVM end-to-end tests for VM lifecycle, storage
relocation and console, on the runners S10 found capable.

**Design references** doc 06 §4, §7, doc 14 §8

**Acceptance criteria**
- [ ] Lifecycle, relocation of a stopped VM, and console connection covered end to end
- [ ] Runs where S10 allows, otherwise on the trusted self-hosted runner only
- [ ] Passthrough covered by M10.9's nested emulated-IOMMU test, not duplicated here

**Scope** `scripts/vm/`, `.github/workflows/`

---

## M11 — Phase 4: polish and release

```epic
id: M11
phase: P4
status: todo
labels: [chore]
issue: 100
```

From "the author runs it" to 1.0: diagnostics, wake attribution, the remaining
UI, the ISO bundle, an opt-in public beta, name clearance and the release checklist
(doc 07 §1, Phase 4; doc 06 §7).

### Diagnostics bundle with verifiable redaction

```meta
id: M11.1
epic: M11
status: todo
labels: [feat, area:api]
sudo: false
depends: [M3.3]
issue: 101
```

**Summary** One-click diagnostics archive whose redaction the user can inspect
before posting it anywhere public.

**Design references** doc 03 §9.4, doc 11 §3, doc 01 §3 (`hoserva diagnostics`)

**Acceptance criteria**
- [ ] Archive contains doctor output, versions, redacted config, recent logs, job history, disk inventory and SMART reports
- [ ] One redaction engine; the UI and CLI list exactly what was removed
- [ ] A test asserts known secret fields never appear in the archive

**Scope** `internal/api/`, `cmd/hoserva/`

### Wake attribution

```meta
id: M11.2
epic: M11
status: todo
labels: [feat, area:storage]
sudo: false
depends: [M2.7, M2.1]
issue: 102
```

**Summary** Attach the likely process, and where possible the container, to
each disk wake in the event log. May slip past 1.0 without blocking it.

**Design references** Q32, doc 08 §1, doc 03 §3.3a

**Acceptance criteria**
- [ ] fanotify-based attribution recorded per wake without causing wakes itself
- [ ] Container resolved from the process's cgroup where possible
- [ ] Shown on the wake events page with "unknown" when attribution fails
- [ ] Explicitly marked non-blocking for 1.0 in the release checklist

**Scope** `internal/disk/`, `internal/parity/`

### UI tier 4

```meta
id: M11.3
epic: M11
status: todo
labels: [feat, area:web]
sudo: false
depends: [M3.6, M11.1]
issue: 103
```

**Summary** The remaining pages: disk history graphs, logs, diagnostics,
advanced settings with drift management, updates, network, general settings
and the off-by-default terminal.

**Design references** doc 03 §3.4, §8.1, §8.2, §8.6, §8.7, §9.1, §9.2, §9.4, §10, Q59

**Acceptance criteria**
- [ ] Every tier 4 page from doc 03 §10 built with its Components line
- [ ] The terminal is off by default and enabling it states that it is root shell access
- [ ] Updates page refuses an update while a Parity, Array-write or Topology job runs, naming the job
- [ ] Charts, editors and the terminal use Q59's wrapped libraries only

**Scope** `web/src/routes/`, `web/src/components/`

### ISO bundle

```meta
id: M11.4
epic: M11
status: todo
labels: [chore, area:packaging, safety-critical]
sudo: false
depends: [M3.8, M7.1]
issue: 104
```

**Summary** An installer ISO with Debian 13, Hoserva, and Docker
preinstalled, which refuses removable boot targets.

**Design references** D9, D11, doc 04 §3, doc 01 §6

**Acceptance criteria**
- [ ] Reproducible ISO build in CI for amd64
- [ ] The installer warns on and refuses a removable target device (D11)
- [ ] The installer never offers a disk that looks like it holds data without an explicit, typed choice
- [ ] Installs and completes onboarding in the L3 suite

**Scope** `packaging/iso/`, `scripts/release/`

### Opt-in public beta

```meta
id: M11.5
epic: M11
status: todo
labels: [chore, area:packaging]
sudo: false
depends: [M11.1, M11.4, M3.10]
issue: 105
```

**Summary** Volunteers run the beta on their own varied hardware and send
diagnostics bundles, exercising the real-hardware risks the lab and VMs cannot
prove (doc 06 §6). The maintainer runs nothing (D20).

**Design references** doc 06 §6 (Opt-in public beta), doc 12 §6 (Release channels), R1, R10, R14, D20

**Acceptance criteria**
- [ ] Beta channel in the apt repository; joining is opt-in, with the safety expectations stated up front
- [ ] Diagnostics bundles collected against a tracking template
- [ ] Results recorded against doc 06 §6's residual-risk table — each risk confirmed, refuted or still open
- [ ] Every beta-found issue filed and triaged

**Scope** `scripts/release/`, `docs/internal/06-dev-and-testing.md`

### Name clearance

```meta
id: M11.6
epic: M11
status: todo
labels: [chore]
sudo: false
depends: []
issue: 106
```

**Summary** Check trademark registries and secure remaining domains before the
1.0 announcement. The searches and registrations are the maintainer's; an
agent prepares the checklist and search terms.

**Design references** Q50, doc 00 §6

**Acceptance criteria**
- [ ] EUIPO/TMview searches for the name in the relevant classes, results recorded
- [ ] `.io` and `.com` availability checked and the decision recorded
- [ ] Q50 updated with the outcome

**Scope** `docs/internal/13-open-questions.md`

### 1.0 release checklist

```meta
id: M11.7
epic: M11
status: todo
labels: [chore, area:packaging]
sudo: false
depends: [M11.3, M11.5, M11.6, M8.7, M9.2, M6.6]
issue: 107
```

**Summary** Doc 06 §7's release checklist, automated where possible, as the
gate for 1.0.

**Design references** doc 06 §7 (Release checklist), doc 07 §4, R2, R3

**Acceptance criteria**
- [ ] All test layers green, including the migration suite on every supported variant and the bare-metal restore
- [ ] Upgrade from the previous beta verified, including schema migrations against every fixture database
- [ ] No breaking API change since the previous release without a new API version (oasdiff)
- [ ] Clean-conversion rate has not regressed; spindown acceptance result published
- [ ] The nightly-parity tradeoff and throughput ceiling stated on the download page and in onboarding
- [ ] Release notes generated from conventional commits and reviewed by the maintainer

**Scope** `scripts/release/`, `.github/workflows/`
