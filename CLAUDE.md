# Hoserva

An open-source home server platform for mixed-size disks: a management layer (Go daemon + CLI + web UI) over mergerfs and SnapRAID on Debian, with a guided migration path from Unraid. GitHub: `mdg-labs/hoserva` (public).

**Positioning and tone** (doc 00 §6): describe Hoserva by what it does, never as "an X alternative". Mention Unraid and other projects only factually — migration, compatibility, a design comparison. No disparaging remarks about other projects or their users. This applies to docs, issues, commit messages, UI copy and the docs site.

**Current state: design phase.** No code exists yet. The design lives in `docs/internal/`, and the first implementation work is Phase 0 spikes and the Phase 1 foundation (doc 07 §1, doc 12 §5). Code conventions below are the plan; tighten them as the first real code lands.

# Documentation map

`docs/internal/` — read the relevant doc before touching an area; it is the authority for anything an issue doesn't spell out.

| Doc | Read before working on |
|---|---|
| `00-overview.md` | Anything — scope and the **decision log (D1–D14)** |
| `01-architecture.md` | Daemon, API, CLI, jobs, security, disk layout |
| `02-storage-engine.md` | Pool mounts, parity, threshold guard, change journal, disk lifecycle |
| `03-webui-spec.md` | Any UI page |
| `04-containers.md` | Apps, templates, converter, catalog |
| `05-migration.md` | The Unraid migrator |
| `06-dev-and-testing.md` | Tests, the loop-device lab, VMs, CI |
| `07-roadmap.md` | Phases, spikes, risks |
| `08-spike-findings.md` | Spindown and adoption research |
| `09-allocation-and-mover.md` | Mover, rebalance, evacuation, share relocation |
| `10-backup.md` | Config and appdata backup |
| `11-ai-assistant.md` | Post-1.0 assistant |
| `12-repo-architecture.md` | Repo layout, agent workflow |
| `13-open-questions.md` | **Every unsettled question, each with a recommended default** |
| `14-virtual-machines.md` | VM management, libvirt/KVM, PCI/USB passthrough, Unraid VM migration |

**Decisions vs. defaults.** A `Dn` in doc 00 §5 is settled — reopening it needs a new reason, not a new preference. A `Qn` in doc 13 is a recommended default the docs are written to — follow it, and if your work shows it is wrong, say so in your report or issue rather than silently diverging. Work that settles or changes a default updates its doc 13 entry in the same change.

# Non-negotiable architecture rules

- **Orchestrate, never reimplement, the storage engine** (D1). No parity code, no pooling code. Hoserva generates config and drives `mergerfs`/`snapraid`.
- **SQLite is the source of truth; config files are generated** (D4). Never write a managed config file directly — change state, regenerate.
- **CLI and UI consume the same API** (D5). No logic that only one of them has.
- **Every system-touching subsystem sits behind a package interface with a scriptable fake** (`disk.Provider`, `parity.Engine`) — that is what makes development possible without a NAS.
- **Every long-running operation is a job** — never bypass the job system; respect the mutually exclusive job classes (doc 01 §4).
- **Never interpolate user or template input into a shell command.** Parse into structured arguments; `exec` with an argv, never `sh -c`.
- **One placement algorithm**: mergerfs's. The mover writes through a mergerfs mount; nothing computes placement itself (doc 09 §2).
- **`api/openapi.yaml` is the hand-written contract**; generated Go and TS types are committed.
- **Nothing on a timer walks a data disk** — no polled `snapraid diff`, no live `du`; spindown is a product requirement (doc 02 §1, Q13).

# Safety rules — hard constraints

- **Parity is written only by a user-configured schedule or an explicit user action. Every sync goes through the threshold guard. Nothing syncs past a tripped guard.**
- **The threshold guard is not optional, and its tests may never be weakened, skipped or loosened** — not to make a build pass, not "temporarily".
- **Copy-verify-delete, never move-and-hope.** Array-to-array relocations are **two-phase**: copy, verify, sync, then delete (Q14).
- **Anything that can lose data gets its test before its implementation.**
- **Migration is never destructive before the point of no return** (doc 05 §5), and verifies with checksums, not counts.
- Issues touching the guard, the mover/relocation delete path, the migration import, `packaging/`, or PCI/USB passthrough's VFIO/bootloader changes (doc 14 §3) carry `safety-critical`.

# Real disks are off-limits — the lab is the only storage environment

The development host's disks are the maintainer's real system. **No command in this repo's development — by a human shell or an agent — may touch a real block device, a real mount, or system storage config.** Concretely, never run any of these on the host, against anything:

`mkfs*`, `wipefs`, `sgdisk`, `parted`, `fdisk`, `sfdisk`, `dd of=/dev/…`, `mount`/`umount` (other than inside the lab), `losetup` (other than inside the lab), `mergerfs`, `snapraid`, `hdparm`/`sdparm` write or spindown options, `smartctl` self-tests, `blkdiscard`, `xfs_repair` without `-n`.

Storage behaviour is exercised **only inside the lab**:

- `make lab-up` / `make lab-destroy` with `HOSERVA_LAB_ID` set (an agent uses its unit id). The lab container gets loop devices and FUSE only — no `--privileged`, no `/dev` bind mount — so a mistyped device path cannot reach a real disk (doc 06 §3, Q45).
- Loop devices are host-global: detach only the ones backed by your own lab's image files. **Never `losetup -D`.**
- Docker is root-equivalent on this host. Use it only through `make` targets and for read-only tool containers (linters) with your workspace mounted read-only. Never stop, remove or prune a container, image or volume you did not create; never `docker system prune`.
- Until `scripts/devenv/` and the `lab-*` targets exist, **no storage command runs anywhere** — work that needs one is blocked on the foundation issue, not improvised.
- A dev `hoservad` uses a workspace-local state directory. Never write `/etc/hoserva`, `/var/lib/hoserva`, `/run/hoserva`, or anything under `/mnt`; never install the `.deb` on the host; never touch system systemd units.
- The L4 hardware box and the author's real array are touched only by the maintainer (`needs-hardware`).

# Root access is the maintainer's

No agent runs `sudo`, a package manager install, or edits anything under `/etc`, under any circumstance. Work that needs root is labelled `needs-sudo`: the agent prepares everything and prints the exact commands for the maintainer, prefixed `! ` so they run in-session. The maintainer's shell is **fish** — print commands that work there (`env VAR=value cmd`, no bash heredocs, no `export`).

# Issues are the plan

GitHub issues on `mdg-labs/hoserva` are this project's plan and memory between sessions.

- **Doc 07 seeds issues; issues are the truth.** Once a phase item is an issue, status, scope and discussion live there. Seed a phase as an epic with sub-issues via `github-triage` (create mode).
- **New work starts as an issue** via `github-triage`, not as a roadmap edit.
- **Two levels: epic → sub-issue.** Nothing deeper.
- **Epic membership and ordering dependencies are native GitHub relationships, never body prose.** `gh issue edit <epic> --add-sub-issue <n>`, `gh issue edit <n> --add-blocked-by <dep>` (needs `gh` ≥ 2.100); read back with `gh issue view <n> --json parent,subIssues,blockedBy,blocking`. A body may explain *why*; it is never the record *that*.
- **Always label**: exactly one type, one `area:*` where one applies, plus any extras. The set is below and in `scripts/bootstrap-labels.sh` — don't invent labels without saying so.
- **`status:*` is machine-managed — exactly one per issue, never set by hand.** `.github/workflows/issue-status.yml` owns opened/reopened → `status:new` and closed → `status:closed`/`status:cancelled`; everything between goes through `scripts/issue-status.sh <issue> <status>`. `github-triage` sets `ready`; `orchestrate`'s executor and verifier own `in-progress` → `in-review` → `implemented`. An epic's status is rolled up by `scripts/epic-status.sh <epic>`. Never `gh issue edit --add-label status:…`.
- **Close via commit trailers.** A commit finishing tracked work carries `Fixes #<n>`; the commit finishing an epic's last sub-issue also carries `Fixes #<epic>`. **Never invent or guess a number** — no tracked issue in context, no trailer.
- **Never close an issue by hand** (`gh issue close`) unless the maintainer explicitly asks.
- Don't open an issue for something finished in the same session — that's bookkeeping theatre.
- **Executing an issue or epic end to end is the `orchestrate` skill's job**: executors in isolated scratch clones, an independent verifier per attempt, landing on local `main` only after a PASS. **Nothing agent-made is pushed automatically** — the maintainer reads (safety-critical commits line by line) and pushes.

## Label set

| Kind | Labels |
|---|---|
| Type (exactly one) | `feat`, `bug`, `chore`, `docs`, `spike` |
| Area | `area:storage`, `area:api`, `area:web`, `area:cli`, `area:shares`, `area:containers`, `area:vm`, `area:migration`, `area:backup`, `area:packaging`, `area:devenv`, `area:site` |
| Extras | `epic`, `safety-critical`, `needs-sudo`, `needs-hardware`, `blocked` |
| Status (machine-managed) | `status:new`, `status:ready`, `status:in-progress`, `status:in-review`, `status:implemented`, `status:closed`, `status:cancelled` |

## Area → paths

| Area | Paths |
|---|---|
| `area:storage` | `internal/disk/`, `internal/pool/`, `internal/parity/`, `internal/cache/`, `testdata/configs/` |
| `area:api` | `api/`, `internal/api/`, `internal/store/`, `internal/model/`, `internal/job/`, `internal/config/`, `internal/notify/`, `cmd/hoservad/`, `cmd/mockapi/`, `web/fixtures/` |
| `area:web` | `web/` |
| `area:cli` | `cmd/hoserva/` |
| `area:shares` | `internal/share/` |
| `area:containers` | `internal/container/`, `internal/template/`, `templates/`, `testdata/unraid-templates/` |
| `area:vm` | `internal/vm/` (Hoserva's own VM-management feature, doc 14 — not `scripts/vm/`, which is the L3 test harness) |
| `area:migration` | `internal/migrate/` |
| `area:backup` | `internal/backup/` |
| `area:packaging` | `packaging/`, `scripts/release/` |
| `area:devenv` | `scripts/devenv/`, `scripts/vm/`, `testdata/parsers/`, `.github/workflows/`, `docker-compose.dev.yml` |
| `area:site` | `site/` |
| `docs` (type, no area) | `docs/internal/` |

**Always-shared files** — any change touching them serializes against every other change that does: `CLAUDE.md`, `Makefile`, `go.mod`, `go.sum`, `go.work`, `api/openapi.yaml`, `api/gen/`, `web/package.json` and its lockfile, `docs/internal/13-open-questions.md`, `.gitignore`, `LICENSE`.

## Spikes

A `spike` issue's deliverable is **recorded findings, not product code**: a findings section in `docs/internal/` (extend doc 08 or add a doc), the exact commands and outputs that support it, any experiment scripts under `spikes/<spike-id>/`, and the doc 13 entries it confirms or overturns. A spike that needs real disks is `needs-hardware`; one that runs in the loop-device lab is agent work once the lab exists.

# Conventions (planned — enforce as code lands)

- Go: `gofmt`, `go vet`, `golangci-lint`; errors wrapped with context (`fmt.Errorf("…: %w", err)`); `context.Context` first parameter on anything that does IO or runs long.
- No comments unless the *why* is non-obvious. No speculative abstraction. No half-finished work. No error handling for cases that cannot happen.
- No business logic in API handlers; the frontend computes nothing the backend should own.
- Every UI string goes through the i18n catalog (Q48); every technical term gets a plain-language label (doc 03).
- Golden files change only deliberately — a golden diff is explained in the commit message, never regenerated to make a test pass.
- Conventional commits (`feat(parity): …`, `fix(mover): …`), one issue per commit, `Fixes #n` trailer.
- `make test` (L1 + L2) before landing, once the Makefile exists. If a workflow isn't a `make` target, it doesn't exist.

# Anti-patterns specific to this project

- Generic Docker management features (D6) — test every Apps request against "does this get someone from *I want X* to *X is running*".
- A second placement algorithm beside mergerfs's create policy.
- Anything on a timer that walks a data disk.
- Weakening the threshold guard or its tests.
- Deleting a source file on a data disk before the sync that covers its copy.
- Vendoring the Community Applications feed or its templates into the repo (doc 04 §4, doc 06 §2).
