# Hoserva — Roadmap, Risks, Open Questions

---

## 1. Phasing

This section is the rationale; **the plan itself lives in GitHub issues** — one epic per phase, with its sub-issues, blocked-by relationships and milestone. The issues are authoritative for status and scope.

### Phase 0 — Feasibility spikes (before committing to the build)

Short, disposable experiments that answer the questions capable of invalidating the whole plan. **Do these first.** Each should take days, not weeks.

| Spike | Question | Kill criterion |
|---|---|---|
| **S1 — Spindown under mergerfs** | Does any IO reach idle array disks during idle and appdata-only activity (the zero-IO proxy, doc 06 §6)? | If disks wake constantly and cannot be tuned, the product fails at something users care about daily |
| **S2 — Unraid disk adoption** | Can data disks laid out the way Unraid formats them be mounted read-only and unioned on Debian with the share structure intact (synthetic fixtures, doc 06 §5)? | If not, the migration story collapses and with it the main adoption argument |
| **S5 — Loop-device harness fidelity** | Does SnapRAID behave identically on loop devices? | If not, the entire dev workflow needs rethinking before any code is written |
| **S6 — Per-share mount topology** | Do per-share mergerfs mounts over a catch-all mount (doc 02 §1, Q12) mount reliably at boot, stay quiet for spindown, and does `mspmfs` fall back as documented (Q11)? | If not, fall back to the two-mount tiered design and shrink per-share cache modes (R12) |
| **S7 — Change journal** | Does a fanotify filesystem mark on each data disk count changes accurately without waking disks (Q13)? | If not, the parity indicator shows "last synced" only, and a timer diff stays forbidden |
| **S8 — Dependency sourcing** | Do Debian 13's mergerfs and SnapRAID packages cover what the design needs, or must Hoserva ship its own builds (Q7)? | Not fatal; decides packaging work in Phase 1 |
| **S9 — Lab container and CI runners** | Does the narrowed lab container (loop + FUSE only, no `--privileged`) work on the dev host and hosted CI runners, and do hosted runners offer KVM (Q42, Q45)? | If not, labs run in a disposable libvirt VM; never fall back to `--privileged` |

**Status:** spindown and adoption are answered from public research in doc 08 — neither kills the plan. **S1 is confirmed in the lab, with no exceptions** — six 30-minute windows through the zero-IO proxy (three scenarios x two mergerfs cache-timeout settings, doc 06 §6) all showed zero movement in any array disk's `/sys/block/<dev>/stat` fields, with per-array-disk attribution verified by a positive control covering both a direct-disk write and a pool-routed write (doc 08 §1); a settle gate ahead of the matrix, justified by a standalone characterization of deferred XFS writeback timing (settles within ~65s on this lab's loop devices), removed the one measurement-harness artifact a first pass had found and mis-explained. L3 confirmation is still open. **S2 is confirmed in the lab, with no exceptions on the primary XFS path** — three synthetic Unraid-layout data disks (MBR, 4K-aligned partition, XFS) passed `xfs_repair -n`, mounted read-only by filesystem UUID with `norecovery`, and every file matched the fixture's manifest by sha256 after a mergerfs union; a fourth, disposable disk with a deliberately dirty log was refused by the same pre-flight check and characterized separately (doc 08 §2). GPT (>2TB) disks, ext4/btrfs disks and a VM fixture remain open. **S5 is confirmed, with one named structural gap** — agent-run in the lab, 2026-09-16: sync, diff, scrub, fix, undeleting, touch, and whole-array reconstruction under both single and dual parity all match `snapraid.txt` exactly, including a corrupt/detect/repair cycle verified by sha256 and two independent byte-for-byte manifest reconstructions after a whole-disk failure; the loop-device lab cannot read any data disk's UUID at all (no `udevd`), so intra-disk moves are always reported as remove+copy instead of moved and the `-U`/`--force-uuid` disk-identity guard is never exercised — a real but narrow gap, covered by L3 rather than fixable in the lab itself (doc 08 §5, doc 06 §6). S3 and S4 were cancelled when the app catalog became Hoserva's own (D19). **S9 is confirmed on the dev host, its lab half (loop devices, FUSE, a real SnapRAID sync) is confirmed on a hosted runner on the pinned `ubuntu-24.04` image, and its `/dev/kvm` half is now CONFIRMED on a hosted runner too** — `ci.yml`'s own `lab` job has run L2 successfully in production (run 34950031773), the pinned-image probe's own SnapRAID-sync step succeeded hosted too (run 35049304081), and that same probe's `kvm` job, re-triggered after a kernel-acquisition fix, booted a real QEMU guest with KVM acceleration confirmed host-side via QMP (run 35056076616, doc 08 §9) — **this closes S9's KVM acceptance criterion; S10 (nested KVM, VM-in-VM) is a separate, still-open spike, below. AppArmor necessity is still open**: two separate hosted runs' AppArmor comparisons both came back void, each from a different unrelated probe bug (missing `mknod`, then an undersized probe image), not from AppArmor itself, and the fixed step has not yet run hosted — **S8 is confirmed, agent-run in the lab, 2026-09-16** — both packages are in Debian 13, and now also behave as doc 02 needs: every doc 02 §1 mergerfs option and all four named create policies are accepted and correctly reported by 2.40.2's own runtime control file, SnapRAID 12.4's `-Z`/`--force-zero` and `-E`/`--force-empty` guards refuse and then, with the flag, proceed exactly as documented, and a changelog review to each project's current upstream release found nothing that meets Q7's bar for carrying a build (doc 08 §8). Q7 now reads `mergerfs (>= 2.40.2)`, `snapraid (>= 12.4)`, no upper bound. **S6 — the spike most likely to change the storage design — is confirmed, agent-run in the lab, 2026-09-16 (redone once after an independent verifier found and fixed a run-order confound in the original throughput measurement)**: a catch-all plus a dozen per-share mounts (all three cache modes) mount and unmount cleanly in dependency order, `mspmfs` falls back to the parent path exactly as documented while `epmfs` (changed only in that option) returns ENOSPC on the identical branches and fill state, `NC` branches stay readable while new files land on cache, a stray top-level write lands on a data disk never the boot device, the twelve-share RSS cost (≈87 MiB) is well inside R12's own bar, and a killed share mount fails safely rather than exposing the array beneath it (doc 08 §6). A counterbalanced, warmed-up re-measurement of throughput shows a relative loss by median of −0.01% in the one committed run, comfortably under R12's 10% bar, with an order-effect check (Pearson r = −0.090, close to zero this run) this shared lab host cannot use to close the caveat out for good, named as a measurement limitation rather than dismissed. The per-share topology stands; the two-mount fallback (Q12, R12) is not adopted. **S7 is confirmed, agent-run in the lab, 2026-09-16**: a `FAN_MARK_FILESYSTEM` mark per data disk, counted by distinct directory-FID-plus-name pair (this lab cannot resolve a fanotify handle to a real path — no `CAP_DAC_READ_SEARCH`, same gap S1/S6 already found), matched `snapraid diff`'s own changed-file total exactly (16/16) for a workload covering every operation the issue named, both direct and through the mergerfs pool; a queue overflow was produced and correctly detected against a same-size non-overflowing control; a listener restart and a filesystem remount were both measured to lose events silently with no self-reported gap; ext4 and single-device btrfs data disks behave identically to XFS, and a btrfs subvolume mounted separately from its root fails to mark with exactly the `EXDEV` the issue's own research predicted (doc 08 §7). Q13 is confirmed, not just adopted.

### Phase 1 — Storage core (MVP)

**Starts with the foundation, before any feature code** (doc 12 §5): loop-device harness and lab container, provider interfaces and fakes, golden-file infrastructure, `openapi.yaml` with generated server interfaces and clients (D18), the central database schema with schema-migration generation, checksums and fixture upgrade tests (D16, Q60), mock API server.

Then: array setup (one or two parity disks), pool management with the per-share mount topology, parity with the threshold guard and change journal, the nightly maintenance chain, disks and SMART, spin-state event log, dashboard, jobs, notifications, config backup (CLI, local destinations), CLI, `.deb` published on GitHub Releases with self-update and rollback (Q66, Q67), startup ordering and maintenance mode (Q69, Q70), metrics retention (Q74), depending on Debian 13's mergerfs and SnapRAID packages with an explicit version range (Q7). UI tier 1 from doc 03 §10.

**Definition of done:** the storage suite passes in the lab and L3, and the soak test (doc 06 §6) has run clean — its diff history is what tunes the guard thresholds (Q16). Nothing ships publicly before that.

### Phase 2 — NAS completeness

Shares with SMB/NFS, users, roles and permissions, API tokens, cache and mover, share relocation, rebalance and evacuation, appdata backup, multi-destination backup with restore drill and off-box encryption (Q80), disk upgrades (Q71), external disks (Q72), UPS support (Q77), host network settings (Q75), backup and restore UI. UI tier 2 from doc 03 §10.

### Phase 3 — Apps and migration

Container management, curated template catalog (D19), GPU access for containers (Q82), Unraid XML converter, migration tooling, documentation site. UI tier 3.

**This is the phase that makes it a complete home server rather than a storage manager.**

### Phase 3.5 — Virtual machines

libvirt/KVM integration (doc 14): `internal/vm`, domain-XML generation, VM lifecycle, disks on the pool/cache under `/mnt/user/domains`. PCI/USB passthrough with IOMMU-group detection and VFIO binding, gated behind a pre-flight compatibility check. Bridged networking (`vmbr0`). Browser console via noVNC over the existing authenticated connection. Unraid VM (libvirt domain XML) import folded into migration tooling (doc 05, doc 14 §5) — structurally easier than the container converter, since Unraid's own VM Manager already emits libvirt XML. UI tier addition (doc 03).

**Gated on its own Phase 0-style spikes before it starts** (doc 13 Q51–Q58 for the design defaults):

| Spike | Question | Kill criterion |
|---|---|---|
| **S10 — Nested KVM for VM-in-VM testing** | Does the L3 test VM (which already runs on libvirt/QEMU, doc 06 §4) support nested KVM for a domain that Hoserva-under-test creates, on both the dev host and hosted CI runners? | If hosted runners don't support it, agents run the suite on the dev host before every release (Q79), not a blocker |
| **S11 — Unraid domain XML compatibility** | How much of a real exported Unraid VM's libvirt domain XML loads with only the remapping in doc 14 §5, on real Debian 13 libvirt/QEMU versions? | If divergence is larger than expected, doc 14 §5's field-remap table grows; not fatal, since the fallback is the same manual-review path doc 04 already has for containers |

**Definition of done:** create, start, and use a VM with a browser console; pass passthrough of one PCI and one USB device in a nested guest with an emulated IOMMU (doc 06 §6); import one Unraid VM fixture with checksum-verified disks.

### Phase 4 — Polish and release

Opt-in public beta, wake attribution (may slip past 1.0 — Q32), diagnostics, ISO bundle, the signed apt repository (Q66), remaining UI tier 4, documentation completeness, name clearance (Q50), 1.0.

### Post-1.0 candidates

- Built-in AI assistant (doc 11, Q47)
- OIDC authentication
- Encrypted (LUKS) pools and migration of encrypted Unraid arrays (Q22)
- Container network creation (Q37)
- UI translations (Q48)
- Multiple pools
- Native cloud backup for pool data (currently served by backup containers)
- Plugin or extension system
- Official arm64 support, once the public beta covers arm64 boards (builds and emulated tests exist from Phase 1 — Q5)
- Ubuntu LTS as a tested base (Q4)

---

## 2. Risk register

| # | Risk | Impact | Mitigation |
|---|---|---|---|
| R1 | **Disks don't stay spun down** | Medium (was High; doc 08 §1 — a property of union filesystems generally, not specific to Hoserva) — still daily-visible | Zero-IO proxy acceptance test **confirmed in the lab, with no exceptions** (S1, 2026-09-15 — six 30-minute windows, zero movement in any array disk's stat fields from each scenario's own workload, per-disk attribution verified by a positive control covering both direct-disk and pool-routed writes, a settle gate ahead of the matrix justified by a measured writeback-timing characterization, doc 08 §1, Q31); L3 confirmation still open; public beta for firmware-level wakes; nothing Hoserva runs on a timer walks a data disk (Q13); wake-event log, then attribution |
| R2 | **A bug destroys user data** | Fatal to the project's reputation | Storage engine is not reinvented (D1); threshold guard on every sync; parity is written only by a user-configured schedule or an explicit user action, and never past a tripped guard; two-phase array relocations (Q14); destructive tests in CI |
| R3 | **Nightly-parity model is rejected by users** | High | Be honest about it everywhere; make the tradeoff explicit rather than discovered, including the per-file throughput ceiling that comes from the same non-striped pooling design (doc 02 §1); make the ransomware-resistance upside visible |
| R4 | **The curated catalog is too small to be useful** | Medium | Seed it with the applications homelab users run most (doc 04 §7); user-added catalog sources; the converter covers users' own templates regardless (D12) |
| R5 | **Unraid changes its on-disk or config layout** | Medium | Version detection in the migration scan; test fixtures per Unraid version; fail loudly on unknown layouts rather than guessing |
| R6 | **Scope creep into a general Docker manager** | High — it is the most tempting direction | Decision D6 is written down; every Apps feature request gets tested against "does this help someone get from *I want X* to *X is running*" |
| R7 | **Solo maintainer burnout** | High | Ship phase 1 narrow; resist feature requests until the core is solid; the `.deb`-first decision (D9) exists partly for this reason |
| R8 | **Security incident from an exposed instance** | High | Safe defaults (doc 01 §7); no default credentials; LAN-bound by default; an explicit "don't expose this" guide |
| R9 | **mergerfs or SnapRAID upstream stalls** | Medium | Both are mature and stable; the abstraction interfaces mean a replacement is possible without a rewrite of everything above |
| R10 | **Performance disappoints compared with users' current setups** | Medium | Relative benchmarks in L3 early; public beta; mergerfs options are the usual cause and are tunable |
| R11 | **Untrusted PR code runs on a privileged self-hosted runner** | High — the repository is public | PR code runs only on ephemeral hosted runners; there are no self-hosted runners (Q79); approval for first-time contributors (doc 06 §7, Q42) |
| R12 | ~~Per-share mount topology fails spike S6~~ — **retired for the topology**: confirmed, agent-run in the lab, 2026-09-16, redone once after an independent verifier fixed a run-order confound in the original throughput measurement (doc 08 §6) | Was Medium — per-share cache modes and allocation would have shrunk | Two-mount tiered fallback was designed in advance (Q12) but is not needed: the per-share topology mounts and unmounts cleanly in dependency order, `mspmfs` falls back as documented, `NC` branches behave as documented, the RSS cost (≈87 MiB for twelve shares) is well inside this risk's own bar, and a counterbalanced re-measurement of throughput shows a relative loss by median of −0.01% in the one committed run, comfortably under the 10% bar — but with an order-effect check (Pearson r = −0.090, close to zero this run) named as a measurement caveat this lab's shared dev host cannot use to close out for good, not a clean "no cost" claim |
| R13 | **An agent touches a real disk during development** | High — the dev host's own disk | Labs only via `make lab-up` in a loop-and-FUSE-only container, namespaced per lane; hard rules in `CLAUDE.md` and in every orchestrate dispatch; no agent ever connects to the maintainer's own machines (D20, doc 12 §5, Q45) |
| R14 | **PCI/USB passthrough is unreliable across the hardware variety homelab boxes actually have** (IOMMU groups, ACS, BIOS quirks) | Medium-High — the single hardest part of doc 14 | Pre-flight `hoserva vm passthrough check` reports compatibility before commit; a device the host needs is structurally unassignable; explicitly documented as best-effort and hardware-dependent, never promised (doc 14 §3) |
| R15 | **`libvirtd`'s privileged surface (direct device binding) widens the daemon's attack surface** | Medium | Same threat-model posture as doc 01 §7; console proxied through existing session auth, never a raw exposed port; passthrough attach/detach audit-logged; no VM image gallery to introduce an untrusted-image problem (doc 14 §6) |
| R16 | **A schema migration silently loses configuration** | High — shares, users and disk mappings live in the database | One central schema with generated, immutable schema migrations; drops only as the contract step of expand/contract; pre-migration snapshot and single-transaction apply; every released schema's fixture database upgraded in CI (D16) |
| R17 | **The API falls behind the UI, or breaks integrations silently** | Medium — scripting and integrations are a stated use of the API | Every UI and CLI capability is a spec operation, implemented through generated interfaces and called through generated clients; breaking changes fail CI against the last release (D18, Q63) |

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

5. **Shipping before it has run for a long time.** The soak test (doc 06 §6) is the first and most important test. If a month of simulated churn and failures isn't clean, it isn't ready for anyone's 24 TB.
