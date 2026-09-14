# Hoserva — Migration from Unraid

The strongest adoption argument the project has. It deserves first-class tooling, not a wiki page.

---

## 1. Why it works — the technical basis

Three properties of Unraid make a near-zero-cost migration possible.

### 1.1 Data disks are independent filesystems

Unraid data disks are **individually formatted XFS filesystems with no striping**. Each disk mounts and reads independently. That is exactly what mergerfs expects as a branch.

**Therefore no data needs to be copied.** The migrator mounts existing disks as-is into the pool. A 24 TB migration that would otherwise require 24 TB of temporary space and days of copying becomes a mount operation.

### 1.2 User shares are already a union of identical directory names

An Unraid user share is a FUSE overlay across identically-named top-level directories on each disk. `/mnt/user/media` is the union of `/mnt/disk1/media`, `/mnt/disk2/media`, and so on.

mergerfs unions the same directories the same way. **The entire share structure survives migration untouched** — no reorganisation, no path changes, nothing for the user to fix.

### 1.3 Path parity makes container migration nearly free

Mounting the pool at `/mnt/user` and cache at `/mnt/cache` (decision D10) means converted Compose files need **zero path rewrites**. A Plex container pointed at `/mnt/user/media` finds its library exactly where it left it, with the same inodes on the same disks.

---

## 2. What must be verified before relying on this

These properties hold for a standard single-parity XFS Unraid array. Each of the following is a distinct path that must be tested or explicitly declared unsupported in v1:

| Variant | Status | Note |
|---|---|---|
| XFS data disks, single parity | **Primary path** | Must work flawlessly |
| Single-device btrfs or ext4 data disks | **Supported** (Q23) | mergerfs and SnapRAID are filesystem-agnostic; each disk passes its own read-only check before adoption |
| ZFS data disks (Unraid 6.12+) | **Detected and refused** (Q23) | Needs OpenZFS as a dependency; out of scope (doc 00 §4) |
| Multi-device btrfs array member | **Detected and refused** (Q23) | Not a self-contained filesystem per disk |
| Encrypted array (LUKS) | **Detected and refused** (Q22) | Recovery risk per doc 08; docs page explains manual options |
| Multi-disk btrfs cache pool | **Supported by evacuation** | The cache is re-created, not adopted; Phase A step 5 moves its data to the array first |
| Dual parity | **Supported** (Q19) | Both Unraid parity disks become SnapRAID parity and are fully rewritten — exactly as with single parity |
| Array with no cache disk | **Supported** | Appdata already lives on the array; Phase D step 18 is optional |
| Multiple named pools (Unraid 6.9+) | **Supported, flagged** | One pool maps to `/mnt/cache`; paths into other `/mnt/<pool>` names are flagged in template conversion (doc 04 §5) |
| Parity disk smaller than largest data disk | Impossible in Unraid | Non-issue |
| Unraid 6.12.x and 7.x | **Supported, fixture-verified** (Q24) | Any other version or unrecognised flash layout is refused unless overridden with a recorded warning |

**Every one of these — including every refusal — gets a VM test case in doc 06.** Assumptions about someone else's on-disk format are exactly the kind of thing that is right in testing and wrong on a stranger's hardware.

---

## 3. Pre-flight — `hoserva migrate scan`

Runs on the freshly installed Hoserva, after the Debian install and **before any import** — still well inside the fully reversible part of the sequence (rollback is clean through step 16, §5). There is no live environment before the Phase 4 ISO, and none is needed. Also available in the UI at `/tools/migrate`.

**Where Unraid's configuration comes from** (Q25): the **Flash Backup zip** produced in Phase A step 1 — uploaded through the UI or given as `--flash-backup <path>` — or the Unraid USB stick itself, mounted **read-only**. Hoserva never writes to the stick. Everything the scan and import read from Unraid's config (shares, users, Docker templates, disk assignments, parity-check history) comes from this one source.

### What it checks

| Check | Pass condition | On failure |
|---|---|---|
| Unraid version and flash layout | 6.12.x or 7.x, layout recognised (Q24) | Refuse; `--unverified-layout` overrides with a warning recorded in the report |
| Data disk filesystems | XFS, ext4 or single-device btrfs (Q23) | ZFS, multi-device btrfs, or LUKS → refuse with the specific disk named (Q22, Q23) |
| **Filesystem integrity, every data disk** | `xfs_repair -n` / `e2fsck -n` / `btrfs check --readonly` clean (doc 08 §2) | **Refuse to adopt that disk.** Computing parity over a corrupt filesystem bakes the corruption in |
| Parity configuration | One or two parity disks (Q19) | Informational: parity is fully rewritten either way |
| Parity disk size | ≥ largest data disk | Block; SnapRAID requires it |
| **SMART status, every disk** | No reallocated or pending sectors | **Recommend aborting.** The unprotected window (§5) is precisely when a marginal disk dies |
| Last Unraid parity check | Completed clean, recently (from the flash parity-check log) | Warn; migrating on a degraded array risks everything |
| Disk identity | Every disk has a WWN or serial (Q21) | Weak-identity disks flagged; refused as parity |
| Free space for content files | Placement per doc 02 §2 possible (Q18) | Flag |
| appdata location | Identified | Report size and current disk |
| File ownership | UID/GID distribution recorded; UID 99 free for `hoserva-apps` (Q26) | Flag if UID 99 is taken on the new host |
| Docker templates | Found and parsed | Report count, and how many convert cleanly vs. with warnings (clean per Q36) |
| Share configuration | Parsed from the flash `config/shares/` | Report count, and each share's allocation method → create policy mapping (Q11) |
| Disk serial mapping | All disks readable | Report the serial → Unraid disk-number table |
| File counts, sizes and sample checksums per disk and share | Recorded | Baseline for the verify phase (§4 step 16) |
| Estimated initial sync duration | Computed from array size | Informational, but it sets expectations for a multi-hour job |

Checks that need a *running* Unraid — the final parity check itself — are a printable pre-cutover checklist (docs site, `before-you-start`), not scan items.

### Output

A written go / no-go report, downloadable, that the user reads **before** committing. Not a green checkmark — an actual document that says what will happen, what the risks are, and what they will need.

---

## 4. Migration sequence

### Phase A — Preparation (Unraid still running, fully reversible)

1. **Back up the Unraid flash drive.** Main → Flash → Flash Backup, and **copy the zip off the server** (to a laptop or another machine). It is both the rollback artifact and the migrator's input (Q25): it contains the Docker templates (`config/plugins/dockerMan/templates-user/`), share configuration (`config/shares/` — names, cache settings, export flags, allocation method), user accounts, and disk assignments.
2. **Confirm the templates are in the backup.** The migration docs show where to look; a container installed without a saved template will not convert.
3. **Note the share list.** Shares are pre-seeded from the backup so the user doesn't recreate twelve shares by hand; this note is the user's own cross-check.
4. **Tell everyone who uses SMB that passwords are being reset.** User accounts are recreated from the backup, but passwords cannot be migrated (hashes differ); the user sets new ones in step 15 and must plan the client-side reconnections in advance.
5. **Move appdata off the cache onto the array** (Unraid mover, or `rsync`). The NVMe is about to be wiped or repartitioned.
6. **Run a final Unraid parity check** and confirm it completes clean. Migrating on top of an already-degraded array is how people lose everything.
7. **Record the disk serial → Unraid disk-number mapping.** `/dev/sdX` names are not stable across reboots and OS changes; serials are. The migrator matches on serial, and the user needs the table to sanity-check it.
8. **Note which disk is parity.** Its contents are worthless to Hoserva — it will be fully rewritten as SnapRAID parity.
9. **Photograph or note the physical disk positions** if the case has more than four bays. Finding the right disk later is a real problem.

### Phase B — Cutover (the point of no easy return)

10. Stop the array, shut down cleanly.
11. **Remove the Unraid USB stick and keep it safe.** This is the rollback mechanism.
12. Install Debian stable to the boot device (doc 01 §6).
13. Install Hoserva from the `.deb`.

### Phase C — Import

14. **Scan, then import.** Provide the Flash Backup zip from step 1 (or attach the stick read-only) and run the scan (§3). Hoserva recognises the Unraid data disks by filesystem, directory structure and the serials recorded in the backup, and offers to adopt them into the pool **without formatting.** The user confirms the disk-role mapping against the serial table from step 7.
15. Pool mounts at `/mnt/user`, share mounts at `/mnt/user/<share>`, cache at `/mnt/cache`. Shares pre-seeded from the backup, including each share's allocation method mapped to a create policy (Q11). Users recreated, passwords set now.
16. **Verify before parity.** Browse the pool. The verify phase compares file counts, sizes and sample checksums per disk and per share against the scan baseline (§3). **This is the last checkpoint where problems are cheap.**
17. Assign the former parity disk(s) as SnapRAID parity, reformatted XFS (Q20). Place content files per doc 02 §2. Start the **initial sync.** This takes hours and is IO-heavy; progress is shown, the system remains usable but slow.

### Phase D — Services

18. Move appdata back onto the cache: a **share relocation** job moves the `appdata` share to cache-only (doc 09 §2), with the same copy-verify-delete guarantees as the mover.
19. Convert Docker templates (doc 04). Review the generated Compose files and all warnings.
20. Start containers one at a time, not all at once. Verify each sees its data before starting the next.
21. Reconnect SMB clients with the new credentials.
22. Once the initial sync completes, run a **full scrub** to confirm parity is consistent.
23. Configure notification channels and send a test through each.
24. Set the sync, scrub, mover, and appdata backup schedules.
25. Run a **restore drill**: pick one unimportant file, delete it, recover it with `snapraid fix`. A backup system that has never been restored from is a hypothesis, not a backup.

---

## 5. The unprotected window — must be impossible to miss

**Between step 11 and the completion of step 17, the array has no redundancy whatsoever.**

Unraid's parity is meaningless the moment the array stops and the disk is repurposed. SnapRAID's parity does not exist until the initial sync finishes. A disk failure in that window means that disk's data is gone.

The docs and the UI must state this in those terms, at the point where the user is about to cross the line — not in a footnote.

**Mitigations, stated explicitly:**

- Irreplaceable data must have a backup that is not this array. Always true, *acutely* true here.
- Do not migrate on a disk with any SMART warning. The scan enforces this with a hard warning.
- Complete the initial sync before writing significant new data.
- Do the migration when you have the hours available to finish it, not on a weeknight.

**Rollback** is clean through step 16: reinsert the Unraid stick, boot, the array returns exactly as it was, because nothing has been written to the data disks. Once step 17 begins writing parity, the old parity disk's contents are destroyed and rollback means restoring from backup.

The UI must mark step 17 as the point of no return and require explicit confirmation.

---

## 6. Migration UI — `/tools/migrate`

Four phases, matching above:

**Scan** — takes the Flash Backup zip (upload) or a read-only attached stick, runs detection, displays the full report, downloadable. Nothing is written to any disk or to the stick.

**Review** — the disk mapping table (serial, Unraid disk number, size, filesystem, proposed Hoserva role), editable. Share import preview. Template conversion preview with per-template warning counts. Prominent unprotected-window warning with a link to the docs.

**Import** — the adoption job, with progress. Ends at the verification checkpoint, explicitly *before* parity is touched.

**Verify** — side-by-side comparison of pre- and post-migration file counts and sizes per disk and per share. Green means proceed to parity; any mismatch means stop and investigate. Only from here is the "initialise parity" action offered, behind the point-of-no-return confirmation.

---

## 7. Documentation site

Migration needs its own documentation, not a README section.

**Astro Starlight on GitHub Pages** — consistent with the existing Astro stack, with versioning, search, and dark mode out of the box, and it builds in CI without a server. Lives in `site/` at the repo root; `docs/internal/` holds the design docs (Q3).

### Structure

```
/                          What Hoserva is, who it's for
/getting-started/
  requirements             Hardware, disks, boot device
  install-deb              Install onto existing Debian
  install-iso              ISO bundle (phase 2)
  first-array              Creating your first array
/migrating-from-unraid/
  overview                 What migrates, what doesn't, what it costs
  before-you-start         Pre-flight checklist (printable)
  the-migration            Happy path, step by step, screenshots
  after-the-migration      Verification checklist, restore drill
  rollback                 How to go back
  special-cases/
    dual-parity
    btrfs-and-ext4-data-disks
    named-pools
    no-cache-disk
    unraid-7
    unsupported-arrays     Encrypted, ZFS, multi-device btrfs: why refused, manual options
  troubleshooting          Disk not detected, share empty, container can't find appdata, permissions wrong
/concepts/
  how-pooling-works        mergerfs, in plain language
  how-parity-works         SnapRAID, and the honest caveat about sync timing
  parity-is-not-backup     Doc 10 §3's stated posture
  cache-and-mover
  what-happens-when-a-disk-dies
  why-disks-wake-up        Union filesystems and spindown, doc 08 §1
/guides/
  adding-a-disk
  replacing-a-failed-disk
  recovering-files         Including "undelete" via the guided fix flow, and its limits
  backing-up-appdata
  backing-up-your-data     Pool data backup with the curated backup containers (doc 10 §3)
  exposing-safely          The "don't put this on the internet" guide
/reference/
  cli
  api
  config-files
  template-format
```

### Tone requirements

Two pieces carry disproportionate weight:

**`/concepts/how-parity-works`** must be honest about the nightly-sync model, because a user who learns this after losing data will say so publicly and they will be right to. The page should present it as a deliberate tradeoff with clear reasoning, not bury it.

**`/migrating-from-unraid/before-you-start`** must be printable and must lead with the unprotected window. People do this migration at 11pm and skim.
