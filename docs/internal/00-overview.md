# Hoserva — Overview

*An open-source home server platform for mixed-size disks: mergerfs + SnapRAID storage, a modern web UI, Debian base — with a guided migration path from Unraid.*

---

## Document set

| Doc | Contents |
|---|---|
| **00-overview.md** (this file) | Vision, gap analysis, target user, scope, decision log |
| **01-architecture.md** | Stack, source-of-truth model, backend design, API, boot/disk layout, security |
| **02-storage-engine.md** | mergerfs and SnapRAID mechanics, cache/mover, spindown, failure handling |
| **03-webui-spec.md** | Complete page inventory and per-page functionality |
| **04-containers.md** | Container management scope, template system, catalog sources, converter, curated catalog |
| **05-migration.md** | Unraid migration: technical basis, pre-flight, sequence, risks, docs site |
| **06-dev-and-testing.md** | Development environment, loop-device harness, test tiers, CI |
| **07-roadmap.md** | Phasing, feasibility spikes, deferred features, risk register |
| **08-spike-findings.md** | Researched answers to the two kill-criteria questions — **read before committing to the build** |
| **09-allocation-and-mover.md** | What mergerfs owns vs. what Hoserva must build: mover, rebalance, evacuation, free-space accounting |
| **10-backup.md** | Config backup, appdata backup, multi-destination, and where the user's responsibility starts |
| **11-ai-assistant.md** | Built-in assistant (post-1.0): BYOK and local models, tools, redaction, safety boundaries |
| **12-repo-architecture.md** | Monorepo layout, `CLAUDE.md` strategy, agent-driven development workflow |
| **13-open-questions.md** | **Every unsettled question, each with a recommended default** — the docs are written to those defaults |
| **14-virtual-machines.md** | VM management scope, libvirt/KVM architecture, PCI/USB passthrough, Unraid VM migration |

Where a section says *(Qn)*, the choice it describes is a recommended default from doc 13, not a settled decision.

---

## 1. The gap

No open-source solution today combines all three of:

1. **Mixed disk sizes in a single pool** — add any disk, any size, no rebuild, no capacity loss
2. **Single-disk parity** — one parity drive protects `n` data drives
3. **A polished management UI** — setup, monitoring, shares, and apps without hand-editing config files

The existing options, compared on those three properties:

| Option | Mixed sizes | Single parity | Modern UI | Notes |
|---|---|---|---|---|
| **Unraid** | yes | yes | yes | Array engine and web UI are proprietary; paid license |
| **TrueNAS SCALE** | no (matched vdevs) | no (vdev-level) | yes | Structural capacity loss with heterogeneous disks |
| **OpenMediaVault** | via plugins | via plugins | dated | Generic NAS UI, storage pooling is bolted on |
| **mergerfs + SnapRAID (manual)** | yes | yes | none | Solves storage, zero management layer |
| **Rockstor** | btrfs-based | btrfs raid | dated | Effectively unmaintained |
| **CasaOS / Runtipi / Dockge** | n/a | n/a | yes | Container UIs only, no storage layer at all |

mergerfs + SnapRAID has solved the storage problem for years. **The missing piece is the management layer.**

### Reference case (the author's own hardware)

2× 8 TB HDD, 4× 4 TB HDD, 1× 1 TB NVMe. Currently Unraid with one 8 TB as parity → **24 TB usable, single-disk fault tolerance**.

- TrueNAS equivalent (8 TB mirror + 4× 4 TB RAIDZ1) → 20 TB usable. **4 TB lost.**
- RAIDZ1 across all six → ZFS truncates to the smallest disk → 20 TB usable and 8 TB of the large disks stranded. Worse.
- mergerfs + SnapRAID → **24 TB usable.** Identical to Unraid.

This is not a niche preference. It is the defining constraint of homelab storage, where disks are bought one at a time as they get cheap.

---

## 2. What Hoserva is

The management layer that mergerfs and SnapRAID lack. **The storage engine is orchestrated, not reinvented.**

This is a deliberate and load-bearing decision. mergerfs and SnapRAID are battle-tested with years of production use and data-loss bugs long since shaken out. Rewriting them would be both enormous work and the single most dangerous thing this project could do — bugs in that layer destroy user data irrecoverably.

**Hoserva owns:** configuration modelling, lifecycle orchestration, scheduling, monitoring, alerting, the web UI, the CLI, container templating, and migration tooling.

**Hoserva does not own:** pooling, parity computation, filesystem implementation, SMART polling, SMB/NFS protocol.

---

## 3. Target user

Homelab operators who want mixed-size disk pooling on a fully open-source stack — including people coming from Unraid who want a guided, low-risk migration. Technical enough to install Debian; unwilling to hand-maintain mergerfs policies, SnapRAID cron jobs, and Samba configs.

They are characterised by:

- Disks accumulated over years in mismatched sizes
- Media libraries, backups, and self-hosted services (the classic \*arr stack, Plex/Jellyfin, Home Assistant, Immich)
- Tolerance for a nightly-parity model, because the data is large, mostly static, and partially replaceable
- **Low tolerance for data loss and for surprises**

That last point drives most of the UI decisions in doc 03.

---

## 4. Scope

### v1 in scope

- Pool management (create, add disk, remove disk, replace disk, rebalance)
- SnapRAID parity with one or two parity disks (Q19): scheduling, diff preview, threshold guards, scrub
- NVMe cache pool with per-share cache modes and mover (Q12)
- SMB and NFS shares with per-user access
- SMART monitoring and alerting; disk spin-state event log (Q32)
- Container management via templates (narrow scope — see doc 04)
- Unraid migration tooling and template conversion
- Local users, TOTP
- Config backup/restore and appdata backup (doc 10)
- CLI with parity to the web UI
- VM management via libvirt/KVM: lifecycle and PCI/USB passthrough, single node — Phase 3.5 (doc 14)

### v1 explicitly out of scope

| Excluded | Reason |
|---|---|
| Custom storage backend or kernel module | Enormous risk and work for no differentiation |
| ZFS support | Different model entirely; TrueNAS already serves it well |
| Clustering, multi-node, live VM migration between hosts | Wrong audience; single-node product (doc 14) |
| A VM image/OS gallery, mediated/vGPU passthrough | First cut is local lifecycle plus full PCI/USB passthrough only; no image marketplace, no SR-IOV slicing (doc 14) |
| Generic Docker management | Portainer exists and is good; see doc 04 |
| iSCSI target | Local VM vdisks (doc 14) already cover the datastore use case; a network block-device server is its own security and orchestration surface for a narrow audience (Q61) |
| Three or more parity disks | SnapRAID supports up to six; no homelab need justifies the test matrix (Q19) |
| Encrypted (LUKS) arrays | Detected and refused in v1; recovery risk outweighs convenience (Q22) |
| Built-in AI assistant | Post-1.0 (doc 11, Q47) |
| Native cloud backup for pool data | rclone-backed destinations cover config/appdata (doc 10); bulk data uses a backup container |

---

## 5. Decision log

Decisions already settled, with rationale. Reopening any of these needs a new reason, not a new preference. Recommended defaults for everything still open live in doc 13; a default is promoted here once it has survived real code.

| # | Decision | Rationale |
|---|---|---|
| D1 | mergerfs + SnapRAID as storage engine, shipped as dependencies | Mature, proven; rewriting risks user data |
| D2 | Debian stable as base OS | Hardware support, stability, fully CLI-manageable |
| D3 | Go backend, single binary, systemd service | Easy distribution, no runtime deps, good for shelling out to system tools |
| D4 | SQLite as source of truth; config files are generated artifacts | Prevents config drift between UI, CLI, and hand edits |
| D5 | CLI and web UI both consume the same API | Guarantees they cannot disagree |
| D6 | Container management: build it, but narrow | A guided app catalog is what makes a home server usable day to day, and what migrating users rely on; generic Docker management is not |
| D7 | Plain Compose files on disk, no proprietary format | Portainer/Dockge can coexist; user is never locked in |
| D8 | Docker is a prerequisite, not shipped by the `.deb` | Clean separation; ISO bundle ships it preinstalled |
| D9 | `.deb` first, ISO bundle second | Validates the product without also building distro release engineering |
| D10 | Pool at `/mnt/user`, cache at `/mnt/cache` | Path-identical to Unraid → zero rewrites in migrated Compose files |
| D11 | Real boot device (small SSD or NVMe partition), not USB | Debian writes far too much for a stick; Unraid's RAM-boot model is not replicable |
| D12 | Unraid XML template conversion is a first-class feature | Doubles as migration tooling *and* a day-one app catalog |
| D13 | VM engine is libvirt/KVM, orchestrated not reimplemented, sourced as a Debian package dependency like mergerfs/SnapRAID rather than an external prerequisite like Docker | Same engine Unraid's own VM Manager runs on; D1's principle extends cleanly; `libvirt-daemon-system`/`qemu-system-x86` are stable, slow-moving Debian packages, unlike the fast-release Docker Engine that D8 deliberately keeps external |
| D14 | VM management is narrow: single-node lifecycle plus local PCI/USB passthrough, no live migration or clustering, no VM image gallery | Same narrowing discipline as D6 for containers; matches Unraid's own single-box VM model and the existing multi-node exclusion above |
| D15 | The web UI is built on coss ui — Base UI primitives with Tailwind CSS v4, vendored into `web/` with the shadcn CLI, using `lucide-react` icons, zod forms and TanStack Table — and doc 03 names the component and particle for every page element | One accessible, consistent component vocabulary for a UI built largely by agents; copy-and-own means no runtime dependency on a moving design system; coss is AGPL-3.0, compatible with Hoserva's license (D17) |
| D16 | One central database schema (`internal/store/schema/schema.sql`); schema migrations are generated from it, never edited after creation, and always data-safe | The database is the system's definition (D4) — a lost column is a lost share, user or disk mapping. Generated migrations cannot drift from the schema, immutability means an applied migration means the same thing on every install, and expand/contract with tested transforms keeps every upgrade loss-free |
| D17 | AGPL-3.0, with the license text in `LICENSE` at the repository root | An open, inspectable management layer whose derivatives stay open; the commercial surface is small enough to serve as a separate service; compatible with the vendored coss ui components (D15) |
| D18 | The API is REST + SSE, specified first in a hand-written `api/openapi.yaml`; the Go server interfaces, the Go client the CLI uses and the TypeScript client the web UI uses are generated from it and committed; every UI and CLI capability is a documented operation, with no undocumented endpoints | Scripts and integrations can do everything the UI can; a missing or mismatched handler fails to compile; UI, mock server and backend build in parallel from one contract; per-operation authorization keeps a root daemon's surface small — GraphQL's flexible reads don't outweigh that for an API made mostly of guarded, job-producing actions |
| D19 | Hoserva's app catalog is its own: templates the project writes from each application's upstream documentation, plus catalog source URLs a user adds. No third-party catalog feed is built in | A catalog written from upstream docs is fully under the project's control and carries no third-party template license. The converter (D12) is unaffected — it converts a migrating user's own templates |
| D20 | All testing is agent-run on the development host and CI: L1 unit tests, the L2 loop-device lab, and L3 QEMU VMs under the user's own `qemu:///session`. There is no physical test box, no testing on the maintainer's homelab or Unraid server, and no maintainer-run test step; behaviour only real hardware shows gets the closest lab or VM proxy, its residual risk is stated, and an opt-in public beta exercises it | Development is done entirely by agents, and the maintainer's own machines are never put at risk. A proxy with an honestly stated gap keeps the plan moving where a manual hardware step would block it (doc 06 §6) |

---

## 6. Naming and licensing

**Name:** Hoserva — decided. A blend of "Home" and "Serva" (Latin/Romance root for serve, and echoing "server"): home, serve, and server in one short, pronounceable word. Domain confirmed available at `.dev`. No collisions found in the storage/NAS/dev-tooling space. `.io`/`.com` and a formal EU trademark check are still worth doing before heavy public branding investment, but the name itself is settled.

**License:** AGPL-3.0 — decided (D17), `LICENSE` at the repository root. Being fully open is central to what Hoserva offers: an open, inspectable management layer whose derivatives stay open. The plausible commercial surface (hosted remote monitoring for homelabs) is small, and is better offered as a separate optional service than protected with license terms. A source-available license such as ELv2 was the alternative and was declined for that reason; the vendored coss ui components (D15) are AGPL-3.0 as well.

**Positioning and tone.** Hoserva is described by what it does — an open-source home server platform for mixed-size disks — not by what it replaces. Other projects are mentioned factually: Unraid where migration and compatibility require it (disk adoption, template conversion, path compatibility), TrueNAS, OpenMediaVault and others where a comparison informs a design choice. No "X alternative" or "X, but open" framing, no disparaging remarks about other projects or their users, and no claims about competitors that aren't sourced facts. Unraid is a trademark of Lime Technology, Inc.; Hoserva is not affiliated with it, and user-facing copy says so wherever Unraid is named.
