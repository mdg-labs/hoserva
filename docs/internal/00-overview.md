# Hoserva — Overview

*An open-source Unraid alternative: mergerfs + SnapRAID storage, modern web UI, Debian base.*

---

## Document set

| Doc | Contents |
|---|---|
| **00-overview.md** (this file) | Vision, gap analysis, target user, scope, decision log |
| **01-architecture.md** | Stack, source-of-truth model, backend design, API, boot/disk layout, security |
| **02-storage-engine.md** | mergerfs and SnapRAID mechanics, cache/mover, spindown, failure handling |
| **03-webui-spec.md** | Complete page inventory and per-page functionality |
| **04-containers.md** | Container management scope, template system, Unraid CA feed findings |
| **05-migration.md** | Unraid migration: technical basis, pre-flight, sequence, risks, docs site |
| **06-dev-and-testing.md** | Development environment, loop-device harness, test tiers, CI |
| **07-roadmap.md** | Phasing, feasibility spikes, deferred features, risk register |
| **08-spike-findings.md** | Researched answers to the two kill-criteria questions — **read before committing to the build** |
| **09-allocation-and-mover.md** | What mergerfs owns vs. what Hoserva must build: mover, rebalance, evacuation, free-space accounting |
| **10-backup.md** | Config backup, appdata backup, multi-destination, and where the user's responsibility starts |
| **11-ai-assistant.md** | Built-in assistant (post-1.0): BYOK and local models, tools, redaction, safety boundaries |
| **12-repo-architecture.md** | Monorepo layout, `CLAUDE.md` strategy, agent-driven development workflow |
| **13-open-questions.md** | **Every unsettled question, each with a recommended default** — the docs are written to those defaults |

Where a section says *(Qn)*, the choice it describes is a recommended default from doc 13, not a settled decision.

---

## 1. The gap

No open-source solution today combines all three of:

1. **Mixed disk sizes in a single pool** — add any disk, any size, no rebuild, no capacity loss
2. **Single-disk parity** — one parity drive protects `n` data drives
3. **A polished management UI** — setup, monitoring, shares, and apps without hand-editing config files

The existing options and why each falls short:

| Option | Mixed sizes | Single parity | Modern UI | Verdict |
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

Homelab operators migrating off Unraid, or those unwilling to buy a license. Technical enough to install Debian; unwilling to hand-maintain mergerfs policies, SnapRAID cron jobs, and Samba configs.

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

### v1 explicitly out of scope

| Excluded | Reason |
|---|---|
| Custom storage backend or kernel module | Enormous risk and work for no differentiation |
| ZFS support | Different model entirely; TrueNAS already serves it well |
| Clustering, multi-node | Wrong audience |
| VM management (libvirt/KVM) | Large surface area; roadmap, not v1 |
| Generic Docker management | Portainer exists and is good; see doc 04 |
| Three or more parity disks | SnapRAID supports up to six; no homelab need justifies the test matrix (Q19) |
| Encrypted (LUKS) arrays | Detected and refused in v1; recovery risk outweighs convenience (Q22) |
| Built-in AI assistant | Post-1.0 (doc 11, Q47) |
| Native cloud backup for pool data | rclone-backed destinations cover config/appdata (doc 10); bulk data uses a backup container |

---

## 5. Decision log

Decisions already settled, with rationale. Reopening any of these needs a new reason, not a new preference. Recommended defaults for everything still open live in doc 13; a default is promoted here once it has survived real code or real hardware.

| # | Decision | Rationale |
|---|---|---|
| D1 | mergerfs + SnapRAID as storage engine, shipped as dependencies | Mature, proven; rewriting risks user data |
| D2 | Debian stable as base OS | Hardware support, stability, fully CLI-manageable |
| D3 | Go backend, single binary, systemd service | Easy distribution, no runtime deps, good for shelling out to system tools |
| D4 | SQLite as source of truth; config files are generated artifacts | Prevents config drift between UI, CLI, and hand edits |
| D5 | CLI and web UI both consume the same API | Guarantees they cannot disagree |
| D6 | Container management: build it, but narrow | App catalog is Unraid's killer feature; generic Docker management is not |
| D7 | Plain Compose files on disk, no proprietary format | Portainer/Dockge can coexist; user is never locked in |
| D8 | Docker is a prerequisite, not shipped by the `.deb` | Clean separation; ISO bundle ships it preinstalled |
| D9 | `.deb` first, ISO bundle second | Validates the product without also building distro release engineering |
| D10 | Pool at `/mnt/user`, cache at `/mnt/cache` | Path-identical to Unraid → zero rewrites in migrated Compose files |
| D11 | Real boot device (small SSD or NVMe partition), not USB | Debian writes far too much for a stick; Unraid's RAM-boot model is not replicable |
| D12 | Unraid XML template conversion is a first-class feature | Doubles as migration tooling *and* a day-one app catalog |

---

## 6. Naming and licensing

**Name:** Hoserva — decided. A blend of "Home" and "Serva" (Latin/Romance root for serve, and echoing "server"): home, serve, and server in one short, pronounceable word. Domain confirmed available at `.dev`. No collisions found in the storage/NAS/dev-tooling space. `.io`/`.com` and a formal EU trademark check are still worth doing before heavy public branding investment, but the name itself is settled.

**License:** open, and **urgent** — the repository is already public without a `LICENSE` file, which legally means all rights reserved (Q1). Two candidate paths:

- **AGPL-3.0** — maximum community trust, strong signal against a proprietary fork, consistent with the "we are the open alternative to Unraid" positioning. Given that the entire pitch is "Unraid but open", a permissive or source-available license undercuts the message.
- **ELv2** — consistent with the existing product line, preserves the option of a hosted offering (remote monitoring, fleet update management, support).

**Recommendation: AGPL-3.0.** The positioning against a proprietary incumbent is the whole point, and the plausible commercial surface here (hosted remote monitoring for homelabs) is small and would be better sold as a separate optional service than defended with license terms. A source-available license on *this specific project* would invite exactly the criticism the project exists to answer.
