# Hoserva — Roadmap, Risks, Open Questions

---

## 1. Phasing

### Phase 0 — Feasibility spikes (before committing to the build)

Short, disposable experiments that answer the questions capable of invalidating the whole plan. **Do these first.** Each should take days, not weeks.

| Spike | Question | Kill criterion |
|---|---|---|
| **Spindown under mergerfs** | Do array disks stay in standby during idle and during appdata-only activity? | If disks wake constantly and cannot be tuned, the product fails at something users care about daily |
| **Unraid disk adoption** | Can a real Unraid XFS array be mounted and unioned on Debian with the share structure intact? | If not, the migration story collapses and with it the main adoption argument |
| **Template conversion rate** | What percentage of a few hundred real CA templates convert cleanly? | Below ~80% clean, the app catalog needs a different approach |
| **CA feed licensing** | Is consuming the feed acceptable, legally and to its maintainer? | If not, fall back to a native catalog (doc 04 §4) — not fatal, but changes the plan |
| **Loop-device harness fidelity** | Does SnapRAID behave identically on loop devices? | If not, the entire dev workflow needs rethinking before any code is written |
| **S6 — Per-share mount topology** | Do per-share mergerfs mounts over a catch-all mount (doc 02 §1, Q12) mount reliably at boot, stay quiet for spindown, and does `mspmfs` fall back as documented (Q11)? | If not, fall back to the two-mount tiered design and shrink per-share cache modes (R12) |
| **S7 — Change journal** | Does a fanotify filesystem mark on each data disk count changes accurately without waking disks (Q13)? | If not, the parity indicator shows "last synced" only, and a timer diff stays forbidden |
| **S8 — Dependency sourcing** | Do Debian 13's mergerfs and SnapRAID packages cover what the design needs, or must Hoserva's apt repo carry its own builds (Q7)? | Not fatal; decides packaging work in Phase 1 |
| **S9 — Lab container and CI runners** | Does the narrowed lab container (loop + FUSE only, no `--privileged`) work on the dev host and hosted CI runners, and do hosted runners offer KVM (Q42, Q45)? | If not, labs run in a disposable libvirt VM; never fall back to `--privileged` |

**Status:** spindown and adoption are answered from public research in doc 08 — neither kills the plan — but both still need their hands-on confirmation, listed there. **S9 is confirmed on the dev host** (hosted runners pending) and **S8 is partly answered** — both packages are in Debian 13 (doc 08). The rest are open. The spindown and adoption spikes are still the two that matter most; S6 is the one most likely to change the storage design.

### Phase 1 — Storage core (MVP)

**Starts with the foundation, before any feature code** (doc 12 §5): loop-device harness and lab container, provider interfaces and fakes, golden-file infrastructure, `openapi.yaml` and generation, the central database schema with schema-migration generation, checksums and fixture upgrade tests (D16, Q60), mock API server.

Then: array setup (one or two parity disks), pool management with the per-share mount topology, parity with the threshold guard and change journal, the nightly maintenance chain, disks and SMART, spin-state event log, dashboard, jobs, notifications, config backup (CLI, local destinations), CLI, `.deb` and the apt repository, depending on Debian 13's mergerfs and SnapRAID packages with an explicit version range (Q7). UI tier 1 from doc 03 §10.

**Definition of done:** you can run your own array on it. Nothing ships publicly until it has run the author's own data for a month — and that month's diff history is what tunes the guard thresholds (Q16).

### Phase 2 — NAS completeness

Shares with SMB/NFS, users, roles and permissions, API tokens, cache and mover, share relocation, rebalance and evacuation, appdata backup, multi-destination backup with restore drill, backup and restore UI. UI tier 2 from doc 03 §10.

### Phase 3 — Apps and migration

Container management, curated template catalog, Unraid XML converter, CA feed source (subject to Q34), migration tooling, documentation site. UI tier 3.

**This is the phase that makes it a complete home server rather than a storage manager.**

### Phase 3.5 — Virtual machines

libvirt/KVM integration (doc 14): `internal/vm`, domain-XML generation, VM lifecycle, disks on the pool/cache under `/mnt/user/domains`. PCI/USB passthrough with IOMMU-group detection and VFIO binding, gated behind a pre-flight compatibility check. Bridged networking (`vmbr0`). Browser console via noVNC over the existing authenticated connection. Unraid VM (libvirt domain XML) import folded into migration tooling (doc 05, doc 14 §5) — structurally easier than the container converter, since Unraid's own VM Manager already emits libvirt XML. UI tier addition (doc 03).

**Gated on its own Phase 0-style spikes before it starts** (doc 13 Q51–Q58 for the design defaults):

| Spike | Question | Kill criterion |
|---|---|---|
| **S10 — Nested KVM for VM-in-VM testing** | Does the L3 test VM (which already runs on libvirt/QEMU, doc 06 §4) support nested KVM for a domain that Hoserva-under-test creates, on both the dev host and hosted/self-hosted CI runners? | If hosted runners don't support it, the suite runs on the self-hosted nightly runner only (doc 06 §7's existing posture for L3), not a blocker |
| **S11 — Unraid domain XML compatibility** | How much of a real exported Unraid VM's libvirt domain XML loads with only the remapping in doc 14 §5, on real Debian 13 libvirt/QEMU versions? | If divergence is larger than expected, doc 14 §5's field-remap table grows; not fatal, since the fallback is the same manual-review path doc 04 already has for containers |

**Definition of done:** create, start, and use a VM with a browser console; pass PCI passthrough for one device class (GPU or USB) on the L4 hardware box; import one real Unraid VM export with checksum-verified disks.

### Phase 4 — Polish and release

Hardware beta, wake attribution (may slip past 1.0 — Q32), diagnostics, ISO bundle, remaining UI tier 4, documentation completeness, name clearance (Q50), 1.0.

### Post-1.0 candidates

- Built-in AI assistant (doc 11, Q47)
- OIDC authentication
- Encrypted (LUKS) pools and migration of encrypted Unraid arrays (Q22)
- Container network creation (Q37)
- UI translations (Q48)
- Multiple pools
- Native cloud backup for pool data (currently served by backup containers)
- Plugin or extension system
- Official arm64 support, once an arm64 box joins L4 (builds exist from Phase 1 — Q5)
- Ubuntu LTS as a tested base (Q4)

---

## 2. Risk register

| # | Risk | Impact | Mitigation |
|---|---|---|---|
| R1 | **Disks don't stay spun down** | Medium (was High; doc 08 §1 — a property of union filesystems generally, not specific to Hoserva) — still daily-visible | Hands-on spike; acceptance test on hardware (Q31); nothing Hoserva runs on a timer walks a data disk (Q13); wake-event log, then attribution |
| R2 | **A bug destroys user data** | Fatal to the project's reputation | Storage engine is not reinvented (D1); threshold guard on every sync; parity is written only by a user-configured schedule or an explicit user action, and never past a tripped guard; two-phase array relocations (Q14); destructive tests in CI |
| R3 | **Nightly-parity model is rejected by users** | High | Be honest about it everywhere; make the tradeoff explicit rather than discovered, including the per-file throughput ceiling that comes from the same non-striped pooling design (doc 02 §1); make the ransomware-resistance upside visible |
| R4 | **CA feed becomes unavailable or unacceptable** | Medium | Pluggable catalog sources from day one; native catalog fallback; converter is valuable regardless |
| R5 | **Unraid changes its on-disk or config layout** | Medium | Version detection in the migration scan; test fixtures per Unraid version; fail loudly on unknown layouts rather than guessing |
| R6 | **Scope creep into a general Docker manager** | High — it is the most tempting direction | Decision D6 is written down; every Apps feature request gets tested against "does this help someone get from *I want X* to *X is running*" |
| R7 | **Solo maintainer burnout** | High | Ship phase 1 narrow; resist feature requests until the core is solid; the `.deb`-first decision (D9) exists partly for this reason |
| R8 | **Security incident from an exposed instance** | High | Safe defaults (doc 01 §7); no default credentials; LAN-bound by default; an explicit "don't expose this" guide |
| R9 | **mergerfs or SnapRAID upstream stalls** | Medium | Both are mature and stable; the abstraction interfaces mean a replacement is possible without a rewrite of everything above |
| R10 | **Performance disappoints compared with users' current setups** | Medium | Benchmark on hardware early; mergerfs options are the usual cause and are tunable |
| R11 | **Untrusted PR code runs on a privileged self-hosted runner** | High — the repository is public | PR code runs only on ephemeral hosted runners; self-hosted runners only on trusted triggers; approval for first-time contributors (doc 06 §7, Q42) |
| R12 | **Per-share mount topology fails spike S6** | Medium — per-share cache modes and allocation shrink | Two-mount tiered fallback designed in advance (Q12); decided in Phase 0, before storage code exists |
| R13 | **An agent touches a real disk during development** | High — the dev host's own disk | Labs only via `make lab-up` in a loop-and-FUSE-only container, namespaced per lane; hard rules in `CLAUDE.md` and in every orchestrate dispatch; the real array is touched only by a human (doc 12 §5, Q45) |
| R14 | **PCI/USB passthrough is unreliable across the hardware variety homelab boxes actually have** (IOMMU groups, ACS, BIOS quirks) | Medium-High — the single hardest part of doc 14 | Pre-flight `hoserva vm passthrough check` reports compatibility before commit; a device the host needs is structurally unassignable; explicitly documented as best-effort and hardware-dependent, never promised (doc 14 §3) |
| R15 | **`libvirtd`'s privileged surface (direct device binding) widens the daemon's attack surface** | Medium | Same threat-model posture as doc 01 §7; console proxied through existing session auth, never a raw exposed port; passthrough attach/detach audit-logged; no VM image gallery to introduce an untrusted-image problem (doc 14 §6) |
| R16 | **A schema migration silently loses configuration** | High — shares, users and disk mappings live in the database | One central schema with generated, immutable schema migrations; drops only as the contract step of expand/contract; pre-migration snapshot and single-transaction apply; every released schema's fixture database upgraded in CI (D16) |

---

## 3. Open questions

**All open questions, each with a recommended default, now live in doc 13.** The questions this section used to hold map as follows:

| Former question | Now |
|---|---|
| License | Q1 |
| Default create policy | Q11 |
| Minimum Debian version, Ubuntu | Q4 |
| Dual parity | Q19 |
| Encrypted arrays | Q22 |
| Catalog default source | Q33 |
| btrfs and ZFS Unraid arrays | Q23 |
| Unraid 7.x | Q24 (hands-on in spike 2) |
| Disk identification stability | Q21 |
| mergerfs branch-mode and spindown | Absorbed into doc 08 §1 hands-on work and spike S6 |
| `snapraid touch` | Q17 |

---

## 4. What would make this project fail

Worth naming explicitly, because these are the failure modes that actually kill homelab OSS projects:

1. **Building the storage engine.** Decision D1 exists for this reason. The moment this project starts writing parity code, it acquires the ability to destroy data in novel ways and a maintenance burden that never ends.

2. **Chasing feature parity with established NAS platforms before the core is solid.** Mature platforms have many years of accumulated features. Matching them is not the goal; solving the *core* problem well is.

3. **Hiding the nightly-parity tradeoff.** The first well-publicised "Hoserva lost my data and never told me it wasn't protected" thread would be unrecoverable — and would be deserved.

4. **Becoming a Docker manager with a storage feature.** Container management is more immediately gratifying to build than parity scheduling. Decision D6 is the guardrail.

5. **Shipping before running it personally for a long time.** The author's own 24 TB array is the first and most important test. If it isn't trustworthy enough for that, it isn't ready for anyone.
