# Hoserva — Migration from Unraid

The strongest adoption argument the project has. It deserves first-class tooling, not a wiki page.

---

## 1. Why it works — the technical basis

These properties of Unraid make a near-zero-cost migration possible.

### 1.1 Data disks are independent filesystems

Unraid data disks are **individually formatted XFS filesystems with no striping**. Each disk mounts and reads independently. That is exactly what mergerfs expects as a branch.

**Therefore no data needs to be copied.** The migrator mounts existing disks as-is into the pool. A 24 TB migration that would otherwise require 24 TB of temporary space and days of copying becomes a mount operation.

**Partition layout** (spike S2, doc 08 §2, sourced from `unraid/webgui`'s `disk_default_partition_format_help`, identical in the `6.12.15` and `7.3.2` tags): array disks 2 TB or smaller get a single MBR partition starting at the 64th 512-byte sector — Unraid's "MBR: 4K-aligned" default — spanning to the end of the disk; disks over 2 TB always get a GPT table instead. Unraid's own partition-start behaviour for the GPT case is not in that public source and was not built by S2; a read-only calibration against a real 7.3.2 server (doc 08 §2) found a single partition starting at sector 64 and ending 33 sectors before the end of the disk, of type `0fc63daf-8483-4772-8e79-3d69d8477de4` (Linux filesystem data). That is an observation of one server, not a specification; the fixtures lay GPT disks out the same way (doc 06 §5: `unraid-7x-xfs-single-parity` has two disks larger than 2 TiB, and an L3 guest with the default disk sizes has only such disks). A cleanly-stopped array leaves every disk's XFS log clean; a disk pulled or crashed while the array was still started can have a dirty log, which `xfs_repair -n` reports distinctly (S2 measured this directly) and which a read-only adoption mount must never silently replay — see doc 06 §5 and doc 08 §2 for the `norecovery` mount-option finding this settles.

### 1.2 User shares are already a union of identical directory names

An Unraid user share is a FUSE overlay across identically-named top-level directories on each disk. `/mnt/user/media` is the union of `/mnt/disk1/media`, `/mnt/disk2/media`, and so on.

mergerfs unions the same directories the same way. **The entire share structure survives migration untouched** — no reorganisation, no path changes, nothing for the user to fix.

### 1.3 Path parity makes container migration nearly free

Mounting the pool at `/mnt/user` and cache at `/mnt/cache` (decision D10) means converted Compose files need **zero path rewrites**. A Plex container pointed at `/mnt/user/media` finds its library exactly where it left it, with the same inodes on the same disks.

### 1.4 VM migration is adoption, not translation (Phase 3.5, doc 14)

Unraid's VM Manager is libvirt underneath, so its domain definitions — kept in `libvirt.img` on the array, adopted along with the data disks — are already libvirt domain XML — the format Hoserva itself generates. Unlike Docker templates, there is no foreign format to convert; VM migration is closer to §1.1's disk-adoption case than to a translation problem. See doc 14 §5 for the small set of fields (network bridge name, passthrough device addresses) that still need remapping.

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
| Cache of any layout: single disk, multi-disk btrfs pool, or a partition shared with the boot device | **Supported by evacuation** | The cache is re-created, not adopted, whichever layout it has; Phase A step 5 moves its data to the array first. When and how it is reformatted depends on the layout (§4, "The cache device through the sequence") |
| Unraid boots from an internal device, dedicated boot device (Unraid 7.3) | **Recognised and left alone** (Q23, Q24) | Identified by its GPT partition names and types, never adopted, never refused as a ZFS disk. The configuration comes from the Flash Backup zip only: the boot pool is ZFS, which Hoserva cannot read without OpenZFS (doc 00 §4). No internal-boot server exists to test on (D20), so this is verified against fixtures only (`unraid-internal-boot`, doc 08 §2) |
| Unraid boots from an internal device, mirrored pair (Unraid 7.3) | **Recognised and left alone** (Q23, Q24) | As above; both devices are recognised and reported as one pair. Fixture-only, same variant with a second device |
| Unraid boots from an internal device, boot + data device (Unraid 7.3) | **Recognised and left alone; its data area is the cache, re-created** (Q23, Q24) | As above for partitions 1–3. Partition 4 is Unraid's cache and follows the cache row: evacuated in Phase A step 5, then re-created, never adopted. Fixture-only (`unraid-internal-boot-shared`, doc 08 §2) |
| Dual parity | **Supported** (Q19) | Both Unraid parity disks become SnapRAID parity and are fully rewritten — exactly as with single parity |
| Array with no cache disk | **Supported** | Appdata already lives on the array; Phase D step 18 is optional |
| Multiple named pools (Unraid 6.9+) | **Supported, flagged** | One pool maps to `/mnt/cache`; paths into other `/mnt/<pool>` names are flagged in template conversion (doc 04 §5) |
| Parity disk smaller than largest data disk | Impossible in Unraid | Non-issue |
| Unraid 6.12.x and 7.x | **Supported, fixture-verified** (Q24) | Any other version or unrecognised flash layout is refused unless overridden with a recorded warning |
| VMs present (Phase 3.5) | **Supported, fixture-verified** (Q55, S11) | Domain XML read from `libvirt.img` on the adopted pool, not the Flash Backup; see §1.4, step 26, and doc 14 §5 |

**Every one of these — including every refusal — gets a VM test case in doc 06.** Assumptions about someone else's on-disk format are exactly the kind of thing that is right in testing and wrong on a stranger's hardware.

---

## 3. Pre-flight — `hoserva migrate scan`

Runs on the freshly installed Hoserva, after the Debian install and **before any import** — still well inside the fully reversible part of the sequence (rollback is clean through step 16, §5). There is no live environment before the Phase 4 ISO, and none is needed. Also available in the UI at `/tools/migrate`.

**Where Unraid's configuration comes from** (Q25): the **Flash Backup zip** produced in Phase A step 1 (which contains the prepare script's capture under `config/hoserva/`, Q89) — uploaded through the UI or given as `--flash-backup <path>` — or the Unraid USB stick itself, mounted **read-only**. Hoserva never writes to the stick. Everything the scan and import read from Unraid's config (shares, users, Docker templates, the container list, disk assignments, parity-check history) comes from this one source. **When Unraid boots from an internal device (Unraid 7.3), the zip is the only source:** its boot pool is ZFS and Hoserva cannot read it without OpenZFS, and a USB stick that may still be attached for licensing is a copy Unraid no longer writes once the internal pool is mounted on `/boot` (doc 08 §2), so the scan does not read configuration from it.

### What it checks

| Check | Pass condition | On failure |
|---|---|---|
| Unraid version and flash layout | 6.12.x or 7.x, layout recognised (Q24), including the flash tree of an internal-boot server | Refuse; `--unverified-layout` overrides with a warning recorded in the report |
| **Unraid boot device** | A FAT filesystem labelled `UNRAID` (the USB stick), or a device whose GPT partitions 1 to 4 carry Unraid's internal-boot names and types (doc 08 §2: `BIOS Boot Partition`, `EFI System Partition`, `Unraid Boot Partition`, and a Linux data partition). Both are read from udev's database (the partition table, the filesystem type and label); no partition is opened | Classed as "Unraid boot device" and listed in the report with that role: never adopted, never mounted by the scan's data-disk reads, never refused as a ZFS data disk (Q23). Partition 4 of a boot + data device is reported as the Unraid cache. A device that matches only part of the layout is not classed this way, and a `zfs_member` partition on it falls under Q23's refusal. A data slot that the machine matches to a boot device is refused as one |
| Data disk filesystems | Per data disk, the flash's `diskFsType.N` (`luks:` prefix included; the capture's `disks.ini` `fsType` when the flash says `auto`) cross-checked with the device's own filesystem type from udev. XFS, ext4 or single-device btrfs (Q23), the flash and the device agreeing, with a filesystem UUID that is its own among the data disks. Only slots `disks.ini` records as data disks are looked at: the parity slot is never mounted or checked, and without `disks.ini` no disk is treated as data until the roles are assigned | ZFS, multi-device btrfs (the device count is read from the disk's own superblock with `btrfs inspect-internal dump-super`), LUKS (Q22), any other filesystem, a flash that disagrees with the device, a missing filesystem UUID, a UUID that another **data** disk also carries (an XFS filesystem is not mounted twice under one UUID, and Unraid mounts XFS with `nouuid`, so two of its disks can share one), or a UUID that a disk that is not adopted carries when the disk has no `/dev/disk/by-id` link to be mounted through (see "Mount identity" below) → **refuse that disk by name**, with the specific reason. The report says the disk is not adopted and why, and the scan goes on with the others |
| **Filesystem integrity, every data disk** | `xfs_repair -n` / `e2fsck -n` / `btrfs check --readonly` clean (doc 08 §2), run on the unmounted device through `disk.AdoptCheck` before anything is mounted | **Refuse to adopt that disk.** Computing parity over a corrupt filesystem bakes the corruption in. A dirty XFS log fails `xfs_repair -n` and is refused the same way: the scan never replays a log on a disk Hoserva does not own (replaying it with explicit consent, which S2 left as a product decision, is not offered), and the refusal says to stop the Unraid array cleanly and scan again |
| Parity configuration | One or two parity disks (Q19) | Informational: parity is fully rewritten either way |
| Parity disk size | ≥ largest data disk | Block; SnapRAID requires it |
| **SMART status, every disk** | No reallocated or pending sectors | **Recommend aborting.** The unprotected window (§5) is precisely when a marginal disk dies |
| Last Unraid parity check | Completed clean, within 35 days, and not running when the capture was taken (the capture's `var.ini` fields `sbSynced`, `sbSyncExit`, `sbSyncErrs` and `mdResync`, falling back to the flash's `config/parity-checks.log`) | Warn; migrating on a degraded array risks everything. With no history in either, warn that the check cannot be confirmed and point at the pre-cutover checklist |
| Disk identity | Every disk has a WWN or serial (Q21) | Weak-identity disks flagged; refused as parity |
| Free space for content files | Placement per doc 02 §2 possible (Q18): the parity disks + 2 copies, the first on the boot device, one on the cache when it is a device of its own, the rest on adopted data disks, each with room for the file. The file's size is a planning figure from the baseline's file count and total size (about 200 bytes per file and 24 per 256 KiB block, an assumption and not a measurement), compared with the free space of each disk as it is mounted and of this machine's boot device | Flag when too few data disks will be adopted or too few have room; warn when the boot device's free space cannot be read |
| appdata location | Identified: the `appdata` share's cache setting and the matched disks that hold its directory (the share is the one `docker.cfg`'s appdata path names, `appdata` by default) | Warn when it is on the cache, because Phase A step 5 moves it. The row states its size: on the array from the baseline, per disk, and on a pool device by a metadata walk of its top-level directory through a read-only mount |
| What would be lost with the cache | Nothing still on it: every share set to `prefer` or `only`, any directory a share has on a pool, `libvirt.img` (from the capture's location class, with `domain.cfg`'s path) | Warn that Phase A step 5 must move it. Docker's own storage is reported, not moved, with the writable-layer size per container instead (doc 04 §5); a capture that is missing or says Docker was not running warns that the layers are unmeasured |
| File ownership | UID/GID distribution recorded; UID 99 free for `hoserva-apps` (Q26) | Flag if UID 99 is taken on the new host |
| Docker templates | Every template in `templates-user/` found and parsed | Report how many templates exist, how many are autostart, running, stopped or template only, name any file that does not parse, and report how many convert cleanly vs. with warnings (clean per Q36; the conversion counts are the converter's). The two conversion counts cover **installed** templates (those with a running or stopped container) only; template-only ones stay previewable but are not counted, because a template is a record of every app ever installed, not of the containers that exist. The scan runs the converter (doc 04 §5) over every parsed template in memory, with the capture's Docker networks as its network input so a custom network's `docker network create` command is exact, and keeps only each template's **outcome** in the session (clean, with warnings or failed, plus the warning codes), never its content. The **preview** (source XML, generated Compose, every warning, the privileges) is built when it is asked for, from the Flash Backup zip the session keeps, and served by `hoserva migrate templates [NAME]` and the review screen; when the zip is not kept (it was removed, or the source was the stick) the operation says so and shows nothing. A template the converter cannot read is reported by name and counted as failed, never as clean. Without the capture's container list the counts cover every template and the report says so. Compose Manager projects are previewed as their own `compose.yaml`, counted apart and not converted. Nothing is written to `/var/lib/hoserva/stacks/` and no container is created. A preview holds the template's environment, secrets included, so the session and the report's rows only name and count and the preview is an admin-only operation |
| Container list (from the capture) | Each container is classed by origin: dockerMan (label `net.unraid.docker.managed=dockerman`), Compose Manager (label `com.docker.compose.project`), or created by hand (neither). Each dockerMan container is matched to a template on the template's XML `<Name>` element, case-sensitive and never on its file name, and classed **autostart** (on Unraid's autostart list), **running**, **stopped**, or **template only** (a template with no container) | Never fails the scan; the classification is the report |
| Containers that cannot convert | Every dockerMan container has a template with its `<Name>`; none was created by hand | A dockerMan container with no such template, and a container created by hand, are **flagged by name** — doc 05 §4 step 2's case, now detectable. Neither converts; hand-made ones are listed so the user knows there is nothing to convert |
| Capture present | `config/hoserva/` is in the source and was written no earlier than the newest template in it | A missing capture **warns** (the row points at Phase A step 0), and the scan falls back to listing every template as "unknown", with none pre-selected. A capture file that does not parse warns the same way, naming the file. A capture older than the newest template **warns** that it may be stale; a flash stores modification times without a time zone, so a gap of hours can be that |
| Share configuration | Every `config/shares/*.cfg` parsed (name, cache setting, export flags, allocation method, split level, floor, included and excluded disks), with `config/share.cfg`'s disk limits as the default. A config whose share has no top-level directory on any matched disk, and no entry in the capture's `shares.ini` when it has one, is an orphan | Report the count and each share's allocation method → create policy (Q11), High-water flagged as "no exact equivalent, mapped to Balance across disks (`mfs`)", and its cache setting → cache mode. A share with no directory on a matched data disk is flagged; an orphan is reported and not imported. No config is called an orphan while a disk it would be on is unmatched or its directories cannot be read, because dropping a real share is worse than keeping an orphan. The directories are the top-level directories of the data disks and pool devices, read through the scan's read-only mounts; a disk that is refused or cannot be read leaves every config kept, and the report says which disk and why |
| User accounts | `config/passwd` read for names | Report the names. UID 1000 and up only; passwords are never read (§4 step 1, step 4) |
| User Scripts (plugin) | Each entry under `config/plugins/user.scripts/scripts/<name>/` found, with its schedule from `customSchedule.cron` | Report each script by name and schedule, and an enabled state only where the files show one — never executed or auto-translated (Q83). When entries cannot be found, the row says so and never reports "none" |
| Plugins and custom configuration | `config/plugins/*.plg` listed; `config/smb-extra.conf` and `config/go` read for lines beyond their defaults | Name each plugin and the Hoserva counterpart of the ones that have one (User Scripts, Compose Manager, appdata backup). Custom Samba lines and custom `go` lines are counted and warned as not imported; their content is never quoted |
| Schedules and settings | The mover schedule, the parity-check schedule and mode, the global spin-down delay and the notification agents' names, kept in the session for the post-migration checklist | Info. A setting the flash does not have reads as "not available", never as off; no agent's content or secret is read |
| Disk serial mapping | All disks readable, and the capture's `disks.ini` gives each serial its slot | Report the serial → Unraid disk-number table. Without `disks.ini` the mapping comes only from the user's step 7 table, and the report flags that |
| File counts, sizes and sample checksums per disk and share | Recorded for every adopted data disk: every file's path and size, every symlink with its target (never followed), every special file (FIFO, socket, device node) by type, and counts and total bytes per disk and per share. A hidden top-level directory (`.Trash-*` and the like) is not a share and is left out, as Unraid leaves it out. A path present on more than one disk is recorded as such. Content hashes: **sha256 of every file of 1 MiB or less, plus a deterministic 1 in 100 (at least 200) of the larger files on each disk**, chosen by a stable hash of the path so verify re-hashes exactly the same set; `hoserva migrate scan --full-checksums` hashes every file, for users willing to wait. The sample and its selection rule are stored with the baseline | Baseline for the verify phase (§4 step 16). A path on more than one disk is **flagged** and listed in the report, because the pool shows only one copy of it (mergerfs, as Unraid's user shares do). A disk that cannot be read completely is refused, not recorded with a hole in it |
| Estimated initial sync duration | Computed from array size | Informational, but it sets expectations for a multi-hour job |

**The parity disk is identified by its slot, never by its filesystem.** A real 7.3.2 parity disk reports `xfs` and a filesystem UUID to udev and `blkid`: with an odd number of data disks whose XFS superblocks share geometry, single parity (a bytewise XOR) leaves a valid-looking superblock at the start of the parity partition. The scan therefore takes the parity role from the slot in the capture's `disks.ini` (or the user's step 7 table), and never infers it from a filesystem signature, and the data-disk filesystem checks above skip the parity slot.

**Mount identity.** Adoption mounts each data disk through its own device, the `/dev/disk/by-id` link of the partition the filesystem is on (`<by-id name>-partN`, the disk's WWN-or-serial identity of Q21, as `disk.Disk.FSByIDName` records it from udev's links), and never by `/dev/disk/by-uuid`. The reason is a one-data-disk array with single parity: Unraid's parity is the bytewise XOR of the data disks, which for one disk is a copy of it, so the parity partition carries that disk's XFS superblock and with it its filesystem UUID, and `by-uuid` could name either of the two. The link is bound to the disk's identity, not to a filesystem, so the former parity disk is never mounted in its place; each mount is then shown from the kernel's mount table to be that device, read-only. A disk with no such link (a weak-identity disk, Q21) can be mounted by UUID only when no other disk on the machine carries it; otherwise it is refused, in the scan and again by the import. Two data disks with one UUID are always refused. The unit records the link in `array_disks.mount_source`; a row with none is mounted by UUID as before.

**How the scan reads the data disks.** It runs only when the user starts it (Q13) and never writes a byte to a source disk. Each disk that passed its checks is mounted read-only, one at a time, at a private mountpoint under `hoservad`'s state directory, never through the array's own mount units (which are read-write): XFS as `ro,norecovery` (a plain `-o ro` mount still rewrites the superblock and replays the log, doc 08 §2), ext4 as `ro,noload`, btrfs as `ro,rescue=nologreplay` (a plain `ro` btrfs mount replays a pending tree log, which writes), each with `nosuid,nodev,noexec` and no access-time updates. The mount options alone are not enough, because a filesystem whose log was never applied would be read with files missing: before any mount the scan reads the superblock (`btrfs inspect-internal dump-super`, `dumpe2fs -h`; neither writes) and refuses a btrfs disk whose `log_root` is not zero and an ext4 disk that has `needs_recovery`, by name, with the advice to start Unraid, stop the array cleanly and scan again (a dirty XFS log is refused the same way by `xfs_repair -n`); a superblock that cannot be read or parsed is refused as well. A pool (cache) device read for its directories goes through the same mount options and the same inspection, and is not read at all while its log is pending: the report warns that its directories could not be read. A mount counts only once the kernel's mount table shows it read-only on the mount and on the superblock and a probe of the device reports the expected filesystem UUID. Every disk is unmounted before the job ends, whether it succeeded, failed or was cancelled; a disk that cannot be unmounted fails the scan, and the next start releases a mount a killed process left. The job reports its progress (an estimate: the walk of each disk, then its hashing) and can be cancelled. The baseline is a compressed file beside the session's row in `<state dir>/migrate`, not in the database or in a config archive: it has a row per file, which on a real array is millions. The lab tests (`internal/migrate/datadisks_lab_test.go`, on #74's `unraid-6.12-xfs-single-parity`, `unraid-corrupt-xfs` and `unraid-btrfs-and-ext4-disks`) hashes every whole source device before and after a full scan and asserts the hashes are identical, with a control arm showing the same harness sees a plain `-o ro` XFS mount write. `internal/migrate/crashlog_lab_test.go` does the same on btrfs and ext4 images copied while mounted (so their log was never applied): each is refused and every device is byte-identical after the scan, with control arms showing a plain `-o ro` mount of such an image changes it.

**`looksLikeUnraid` in `GET /pool`.** The hint is kept, with a heuristic that matches a real array. A real Unraid array carries no filesystem label (doc 08 §2), so the earlier label match (`diskN`, `parity`, `cache`) was always false on one. It now holds for a disk with an MBR or GPT partition table whose partition 1 starts at sector 64 and holds XFS, btrfs or ext4, which is how Unraid lays out array and pool disks (§1.1). It is a hint for a warning in the storage setup, never a role: a parity disk has the same layout, and the scan takes every role from `disks.ini`.

Checks that need a *running* Unraid — the final parity check itself — are a printable pre-cutover checklist (docs site, `before-you-start`), not scan items.

### Output

A written go / no-go report, downloadable, that the user reads **before** committing. Not a green checkmark — an actual document that says what will happen, what the risks are, and what they will need.

Beside its rows the report carries the **review**, the same findings as data for the Review step, so no client reads them out of the rows' prose. It is built by the scan in the same pass as the rows (never derived from their text), kept in the session with the report, and served as `report.review` by `getMigration`; the rows and the downloadable document are unchanged, and a report made before the review existed is served without one.

- **Disks.** One entry per disk the capture names (each `disks.ini` parity and data slot and each pool device), per Unraid boot device (the capture's boot devices, matched to this machine's disks by serial, and any Unraid stick or internal-boot disk on this machine) and per disk of this machine the capture does not name. Each carries the capture's slot, Unraid disk number and role (`parity`, `data`, `cache`, `boot`; `unassigned` for an unnamed disk, and only when `disks.ini` could be read), this machine's device, serial, WWN, by-id name, model, size and filesystem, whether its identity is weak (Q21; unknown when no disk matched), whether it is the disk this machine boots from (`hostBoot`: the scan's own boot-disk detection; false for any other disk of this machine, unknown when no disk matched and in a report made before the field existed), and whether the scan refused it with a code (`boot_device`, `host_boot`, `failed`, `encrypted`, `zfs`, `unsupported_filesystem`, `filesystem_mismatch`, `no_filesystem`, `no_filesystem_node`, `duplicate_uuid`, `multi_device_btrfs`, `filesystem_unverified`, `pending_log`, `integrity_check`, `unreadable`, `weak_identity_parity`) beside the row's own words. A disk of this machine is listed once: an internal boot that shares its disk with the cache (§4, Unraid boot + data device) is that slot's row with `unraidBoot` set (the cache pool's, or a parity or data slot's that names a boot device, which is refused), not a second `boot` row for the same device. A field that is absent is unknown, never zero or strong.
- **Proposed role.** The role the import pre-fills (`parity`, `data`, `cache` or `ignore`), from the capture's `disks.ini` and pool configs: a slot whose disk is on this machine gets its own role, a parity slot is never proposed as data whatever its device reports, and a slot with no matching disk, a refused disk and every disk when `disks.ini` is absent are proposed nothing (absent, never guessed; the user decides). An Unraid boot device is only ever `ignore`, and so is an Unraid boot device or stick found in a parity or data slot (refused as `boot_device`; a data slot is refused only where the scan checks the data disks, and is proposed `ignore` either way); the one exception is the cache pool's row of an internal boot that shares its disk with the cache, which is `cache` and marked `unraidBoot` (§4: its data partition becomes Hoserva's cache). The disk this machine boots from is never proposed a role as a parity or data slot's disk: a parity slot's is refused as `host_boot` by the mapping check, a data slot's by the data disk checks (without those checks it is proposed nothing), and a refusal makes the verdict no-go. As the cache pool's disk it is the shared NVMe (doc 01 §6; §4 step 12, "Shared NVMe"): not refused, and proposed `cache`.

- **Shares.** One entry per share the import would create: its name, Unraid's allocation method, whether it is High-water (the Q11 note), the disks it is limited to or kept off, and how many things its row flags or warns about.
- **Boot.** The boot mode (`usb` or `internal`; absent when the capture does not say) and, for an internal boot, whether the pool is a mirrored pair and whether its device also holds Unraid's cache, which are the inputs to §5's layout and rollback table. With `hostBoot`, the Review step decides which of §5's two layouts the plan gives, a separate boot device or a shared NVMe (Unraid's cache is on the disk this machine boots from), and shows that one row. The cache that counts is the one Unraid has, the disks whose Unraid role is `cache`, whatever role the mapping gives them: nothing the user maps changes Unraid's devices before the point of no return, so the Hoserva cache choice cannot change which rollback is true (a cache disk set to ignore still leaves the shared NVMe row). The chosen cache role only says whether there is a cache to decide on. For an internal boot that also holds the cache the cache that counts is the Unraid boot device's own, taken from the chosen cache disks. When the data cannot decide (an unknown or partly stated boot mode, no cache chosen, a cache disk whose `hostBoot` is absent), it keeps every row for the boot kind or the general statement, and an absent `hostBoot` is never read as "not the boot disk". The other internal-boot rows (a dedicated or mirrored pair device) are chosen by which Unraid boot device Debian is installed on, which the cache role does not say, so they stay as every row.
- **Capture.** `present`, `missing`, `unreadable` or `stale` (a template on the flash was saved after the capture was taken, Q89), with the capture's time when it states one. A `present` capture without a time was not checked for staleness.

---

## 4. Migration sequence

### Phase A — Preparation (Unraid still running, fully reversible)

0. **Run the prepare script on the Unraid server.** From the user's own computer, over SSH, against the still-running server:

   ```
   ssh root@<server> 'curl -fsSL https://github.com/mdg-labs/hoserva/releases/download/<tag>/prepare-migration.sh | bash -s -- --zip' > <server>-boot.zip
   ```

   `<tag>` is a Hoserva release tag, so the script the user runs is the one that release shipped. Users who prefer to read it first download it, check it and pipe it in:

   ```
   curl -fsSLO https://github.com/mdg-labs/hoserva/releases/download/<tag>/prepare-migration.sh
   curl -fsSLO https://github.com/mdg-labs/hoserva/releases/download/<tag>/prepare-migration.sh.sha256
   sha256sum -c prepare-migration.sh.sha256
   less prepare-migration.sh
   ssh root@<server> 'bash -s -- --zip' < prepare-migration.sh > <server>-boot.zip
   ```

   The script is read-only apart from one directory. It reads the Docker daemon (`docker ps`, `docker inspect`, `docker network inspect`), Unraid's runtime state under `/var/local/emhttp/`, Docker's autostart list, and the boot mode, and it writes **only** `/boot/config/hoserva/` on the flash: `containers.json`, `networks.json`, `disks.ini`, `autostart`, the parity-check fields of `var.ini`, `smart/`, and `capture.json` (Q89). It then prints the Phase A report on the terminal (and saves it as `report.txt` beside the capture): containers by origin and state, those with no template, the autostart order, what is still on the cache and whether it fits on the array, the last parity check, the serial → slot table, and the boot mode with its rollback consequence (§5). Fix what the report shows while Unraid is still running. With `--zip` it also streams the Flash Backup zip to the user's computer (step 1).

   **It runs before the Flash Backup** because the backup must contain the capture; the one-liner with `--zip` captures first and zips second, so steps 0 and 1 are one command. **The capture contains the containers' environment variables, secrets included**, just like the templates on the same flash, so the zip is as sensitive as the flash itself and is stored like it.
1. **Back up the Unraid flash drive, and copy the zip off the server.** The zip is both the rollback artifact and the migrator's input (Q25). The default is step 0's `--zip` stream: it is made by the script, its root is `/boot` like Unraid's own Flash Backup (so `config/` is a path inside it), and it reaches the user's computer without a copy on the server. The alternative is Main → Flash → Flash Backup in the Unraid UI, run **after** step 0 so the backup contains the capture; its layout and exclusions vary with the Unraid version, which is why the script makes the zip itself. **Do not use that page's "save to server" option**: it asks for a cache pool first, and the zip may land on the cache that is about to be wiped. The zip contains the Docker templates (`config/plugins/dockerMan/templates-user/`), share configuration (`config/shares/` — names, cache settings, export flags, allocation method), user accounts, disk assignments, the capture (`config/hoserva/`), and the User Scripts plugin's scripts and schedules (`config/plugins/user.scripts/scripts/<name>/{script,name,description}` and `config/plugins/user.scripts/customSchedule.cron`; Q83). Disk assignments and per-slot filesystem type live in `config/disk.cfg`, global share defaults in `config/share.cfg`, named-pool (cache pool) assignments in `config/pools/*.cfg`, and — when VMs are present — VM Manager settings including the `libvirt.img` path in `config/domain.cfg` (spike S2, doc 08 §2, sourced from `unraid/webgui`, identical in the `6.12.15` and `7.3.2` tags).

   **User accounts are an assumption, stated the way Q83 stated User Scripts before its confirmation:** the scan reads account names from `config/passwd` on the flash (UID 1000 and up; root, nobody and the system's are not accounts). That path is where the fixtures put them and Unraid's usual persistence of accounts; it is not confirmed against `unraid/webgui` or a real flash tree, and the capture's `users.ini` is not used. `config/shadow` and `config/smbpasswd` hold password material and are never read. The same status applies to the settings the post-migration checklist offers: the parity-check schedule is read from `[parity]` in `config/plugins/dynamix/dynamix.cfg` (`write="NOCORRECT"` means a non-correcting check), the global spin-down delay from `spindownDelay` in `config/disk.cfg`, the mover schedule from `shareMoverSchedule` in `config/share.cfg`, and the notification agents from the file names under `config/plugins/dynamix/notifications/agents/`. A file or key that is not there reads as "not available", never as a setting that is off.
2. **Confirm the templates are in the backup.** The migration docs show where to look; a container installed without a saved template will not convert, and the scan names each one (§3), so this is also where the user fixes it while Unraid still runs.
3. **Note the share list.** Shares are pre-seeded from the backup so the user doesn't recreate twelve shares by hand; this note is the user's own cross-check.
4. **Tell everyone who uses SMB that passwords are being reset.** User accounts are recreated from the backup, but passwords cannot be migrated (hashes differ); the user sets new ones in step 15 and must plan the client-side reconnections in advance.
5. **Move everything off the cache onto the array.** The cache device is about to be wiped or repartitioned (see "The cache device through the sequence" below), and appdata is only part of what a real server keeps there. Move:
   - every share whose cache setting is `prefer` or `only`;
   - `appdata`;
   - `domains`;
   - `system`, except Docker's own storage (the Docker directory, or the `docker.img` file in image mode);
   - `libvirt.img`, which sits in `system`.

   Stop the Docker service and the VM service first (Settings → Docker, Settings → VM Manager: Enable set to No), because the Unraid mover skips files that are open. The mover only moves a share toward the array when the share says so: a `prefer` share is moved onto the cache, not off it, and an `only` share is never moved. So for each share above, set Primary storage to the cache pool, Secondary storage to Array and Mover action to Cache → Array (the Unraid 6.12 and 7 share settings), then run the mover, and confirm the cache is empty afterwards; the prepare script reports what is still there.

   **Docker's own storage is deliberately not moved.** In directory mode it keeps every image layer as a btrfs subvolume, which a file-level move does not reproduce, and the images are pulled again when containers are recreated (steps 19 and 20). What does not come back is each container's writable-layer state; the prepare script lists it per container, and doc 04 §5's warning names it again at conversion time (step 19).
6. **Run a final Unraid parity check** and confirm it completes clean. Migrating on top of an already-degraded array is how people lose everything.
7. **Record the disk serial → Unraid disk-number mapping.** `/dev/sdX` names are not stable across reboots and OS changes; serials are. The migrator matches on serial, and the user needs the table to sanity-check it.
8. **Note which disk is parity.** Its contents are worthless to Hoserva — it will be fully rewritten as SnapRAID parity.
9. **Photograph or note the physical disk positions** if the case has more than four bays. Finding the right disk later is a real problem.

### Phase B — Cutover (the point of no easy return)

Before step 10, the user finds their layout in §5's rollback table: what going back means after step 12 depends on where Unraid boots from and where Debian is going.

10. Stop the array, shut down cleanly.
11. **If Unraid boots from a USB stick, remove it and keep it safe.** It is the rollback mechanism. If Unraid boots from an internal device (Unraid 7.3's internal boot) there is no boot stick to remove: the Flash Backup zip from step 1 is the rollback artifact, and the internal boot device or devices are left alone unless step 12 installs onto one (§5). A stick that is still attached only for licensing stays where the user keeps it; it is Unraid's licence key, not Hoserva's input.
12. **Install Debian stable to the boot device** (doc 01 §6). Three things to get right in the installer's partitioner, and one more afterwards if Unraid boots internally:
    - **Choose the target by model and serial from step 7's table, never by size or `sdX` name alone.** The installer lists every disk in the machine, array disks included, and installing onto one destroys that disk's data. The surest way is to power off and disconnect every array and parity disk for the installation, reconnecting them before step 14.
    - **Separate boot device:** give Debian the whole device. Unraid's cache device is not touched by the installation.
    - **Unraid with internal boot:** its UEFI entries (`Unraid Internal Boot - <serial>`, set first by Unraid's wizard) stay in the firmware after Debian is installed on another device. After the installation, confirm that the firmware boots Debian first; the rollback table in §5 relies on the same boot order.
    - **Shared NVMe** (Debian root and cache on one device): partition the device with root at about 60 GB and a second partition left unused, with no filesystem, no mount point and no swap. The installation destroys everything Unraid kept on that device, its cache included, so step 5's empty-cache check must be clean before step 10. Hoserva formats the second partition as its cache at the point of no return (step 17) and never edits the boot device's partition table (doc 01 §6).
13. Install Hoserva from the `.deb`.

**The cache device through the sequence.** The sequence does not add a step for the cache reformat; where it happens depends on the layout.
- **Separate boot device.** The Unraid cache device survives steps 10 to 16 unchanged. Hoserva formats it, as its own cache, in step 17, in the same confirmed action as the former parity disks.
- **Shared NVMe.** The Unraid cache is destroyed in step 12 by the Debian installation. The spare partition created there becomes Hoserva's cache, formatted in step 17.

- **Unraid boot + data device** (internal boot with the cache on the same device; Debian goes on another device). Partitions 1 to 3 hold Unraid's boot pool and partition 4 the cache. The device survives steps 10 to 16 unchanged and is still Unraid's boot device for the rollback in §5. Step 15 records partition 4 alone as the cache, by its own by-id link and PARTUUID, never the whole device (see "The import's adoption" below). Step 17 formats that partition as Hoserva's cache and leaves partitions 1 to 3 alone (Q23); erasing Unraid's boot partitions is a separate action the user confirms on its own. If Debian goes on this same device, the shared-NVMe bullet above applies and the installation destroys the Unraid cache.

In every layout Hoserva records the cache role at step 15 and creates nothing on the device until step 17. Anything still on an Unraid cache when step 12 starts on a shared device, or when step 17 starts on a separate one or on an Unraid boot + data device, is gone with it.

### Phase C — Import

14. **Scan, then import.** Provide the Flash Backup zip from step 1 (or, for a USB-boot server, attach the stick read-only) and run the scan (§3). Hoserva recognises the Unraid data disks by filesystem, directory structure and the serials recorded in the backup, and offers to adopt them into the pool **without formatting.** The user confirms the disk-role mapping against the serial table from step 7: `hoserva migrate import` prints it and acts only with `--yes`, and the API refuses a request without `confirm`. Nothing is mounted before that.
15. The data disks are adopted and the pool mounts at `/mnt/user`, **read-only**; share mounts at `/mnt/user/<share>` come with the shares. The former parity and cache disks are **recorded by identity and left untouched**: not formatted, not mounted, not opened. The cache role is recorded, `/mnt/cache` does not exist yet, and the device is neither formatted nor mounted until step 17 (formatting it any earlier would break step 16's promise for the cache pool). Shares pre-seeded from the backup, including each share's allocation method mapped to a create policy (Q11). Users recreated, passwords set now. See "The import's adoption" below for the adoption itself.
16. **Verify before parity.** Browse the pool. The verify phase compares file counts, sizes and sample checksums per disk and per share against the scan baseline (§3). **This is the last checkpoint where problems are cheap.**
17. Assign the former parity disk(s) as SnapRAID parity, reformatted XFS (Q20), and format the cache device (a whole disk, the spare partition from step 12, or partition 4 of an Unraid boot + data device) in the same confirmed action. Place content files per doc 02 §2. Start the **initial sync.** This takes hours and is IO-heavy; progress is shown, the system remains usable but slow.

### The import's adoption (steps 14 to 16)

`POST /migrate/import` (`startMigrationImport`, `hoserva migrate import [--role <serial>=<role> ...] [--cache-partition <by-id>:<partuuid>] --yes`) queues a `migration_import` job in the Topology class (doc 01 §4). The request is the disk-role mapping, one entry per disk keyed by serial or WWN, with the roles `parity`, `data`, `cache` and `ignore`; a cache on a spare partition of the boot disk (§5's shared NVMe, doc 01 §6) is keyed by that partition's by-id name and PARTUUID instead. The roles are pre-filled from the scan's disk table (the capture's `disks.ini`); without it nothing is pre-filled and every role is the user's. The request is refused, before anything is queued, unless:

- the session holds a finished scan whose verdict is not no-go, every role but `ignore` names a disk the scan listed (a cache on a spare boot-disk partition names the partition, which `listDisks` reports), and every role, `ignore` included, matches exactly one disk that this machine still has (a role is never given to the first of several candidates);
- a disk the scan refused has no role but `ignore`, and an Unraid boot device or the Unraid USB stick (`disk.IsUnraidStick`) has no role but `ignore`, with one exception: the cache pool's row of an internal boot that shares its disk with the cache (§4, Unraid boot + data device) may be `cache`, sent by the disk's serial or WWN as for any cache. It records only the device's data partition (partition 4 of Unraid's layout, `disk.Disk.UnraidDataPartition`, read from udev and by-id like the rest) by its own `-part4` by-id link and PARTUUID under the disk's identity, never the disk, so that step 17 can format that partition and leave partitions 1 to 3, Unraid's boot pool, alone. It is refused for a dedicated or mirrored boot device (a boot row, or a capture that says the boot holds no cache), for a device whose data partition has no by-id link, PARTUUID or size, and for the stick; nothing is formatted, mounted or opened at import;
- a disk `disks.ini` records as parity is never `data`, whatever filesystem it reports (§3), and one it records as data is never `parity` or `cache`: the point of no return would erase the files being adopted;
- a weak-identity disk is never parity (Q21); parity is one or two disks, each at least as large as the largest data disk (Q19, Q20);
- the disk this machine boots from is never parity or data, and is the cache only through a spare partition of it, never as a whole disk. This is checked against the live inventory too, so it holds for whichever client sends the mapping, the CLI's `--role` included, and a stale report cannot let it through;
- every data disk has a filesystem Hoserva adopts (Q23), a device node and a UUID that is not another data disk's.

The Unraid stick is refused in every role, and the general array setup refuses it as well (doc 02 §4): it is the rollback, never an array member.

**The job.** It resolves the mapping again from a fresh inventory and refuses, before anything is written, unless that is the plan the request confirmed (a disk swapped, repartitioned or replaced since is refused by identity, filesystem, UUID and size) and every data disk passes `xfs_repair -n`, `e2fsck -n` or `btrfs check --readonly` (`disk.AdoptCheck`) through the device it will be mounted by. Only then does it record the array in SQLite with `migration_pending` set, write the read-only disk units and the read-only catch-all unit, mount each disk (`ro,norecovery` for XFS, `ro,noload` for ext4, `ro,rescue=nologreplay` for btrfs, with no setuid, devices, execution or access-time updates), show from the mount table that each is the confirmed device and read-only, and mount `/mnt/user` over them with every branch `RO`. No `snapraid.conf` is generated, so no parity engine is registered and no sync can be scheduled. The former parity and cache disks are recorded in `array_settings.migration_recorded` (role, identity, size; the cache's partition identity when it is one), never as `array_disks` rows, which hold a mounted filesystem's UUID, unique across the array.

**Pending.** From here until the point of no return the session's phase is `imported`, and the scheduler refuses every Parity, Array-write and Topology job but a retry of this one with `migration_in_progress` (409), a new scan and forgetting the session included (doc 01 §4). `array stop` and `array start` work: the units and the pool come back read-only, from the same devices, which `array start` confirms against the device as well as the UUID. The point of no return (step 17) clears `migration_pending`; it is a later step.

**Failure.** A failure after the array is recorded undoes it, in this order: the pool and the disks that were mounted are unmounted; only when every mount is gone is the record deleted, so the record never claims a disk that is not mounted, and a disk that cannot be released is reported with its record kept; then the generated units are removed and the array sequence rebuilt. The undo runs without the request's cancellation. A crash leaves the record, and running the same mapping again applies it again without checking or formatting anything; another mapping is refused as an existing array.

**What it never does.** It runs no `mkfs`, `wipefs`, repair or partitioning tool, mounts nothing read-write, opens no parity or cache device, and reads nothing from the Unraid flash: its inputs are the confirmed mapping, this machine's disks and the report the scan stored (Q25). `internal/migrate/import_lab_test.go` runs it on #74's `unraid-6.12-xfs-single-parity` in the lab and on a one-data-disk array whose parity partition holds a copy of the data disk's filesystem: it takes the whole-device sha256 of every source disk (data, parity and cache) before, reads every file through `/mnt/user` against the fixture's manifest, stops and starts the array through `ArraySequence`, and asserts every device is byte-identical, that the parity device was never mounted and never named in a `mount`, and that a failed mount leaves no record, no mount and no unit; a control arm shows a read-write mount of a copy changes the device and is reported read-write. That byte-identity is what "re-attach the Unraid stick and the array returns unchanged" rests on, since no agent runs Unraid (D20).

### Phase D — Services

18. Move appdata back onto the cache: a **share relocation** job moves the `appdata` share to cache-only (doc 09 §2), with the same copy-verify-delete guarantees as the mover.
19. Convert Docker templates (doc 04). Review the generated Compose files and all warnings, including each container's writable-layer warning (doc 04 §5) — this is the last point in the sequence where the user can act on it before recreating a container. Whether the state it names is still recoverable depends on where the source container's Docker storage sat, not on this step. On an **array** disk, `docker.img` was adopted unformatted in step 14 and is still an ordinary file on the pool. On the **cache** device it is lost at the reformat: in step 12 on a shared NVMe, in step 17 on a separate cache device. A Docker directory on the cache is lost the same way, and step 5 deliberately does not move it. Hoserva has no way to read either afterwards, so state that needs recovering has to be recovered from the still-running Unraid system, before step 10.
    Selection comes from the capture. Containers on Unraid's autostart list are **pre-selected**; other installed containers are listed but not pre-selected; template-only entries are previewable and not offered by default. Compose Manager projects are offered as their own `compose.yaml`, not converted from a template.
20. Start containers one at a time, not all at once, with the pre-selected ones in Unraid's autostart order (waiting the seconds the list gives where it does). Verify each sees its data before starting the next.
21. Reconnect SMB clients with the new credentials.
22. Once the initial sync completes, run a **full scrub** to confirm parity is consistent.
23. Configure notification channels and send a test through each.
24. Set the sync, scrub, mover, and appdata backup schedules. Also work through the User Scripts inventory the scan reported (§3, Q83): recreate anything still wanted as a plain cron job or systemd timer on the new host. Hoserva neither executes nor auto-translates a migrated script — the inventory only tells the user what existed.
25. Run a **restore drill**: pick one unimportant file, delete it, recover it with `snapraid fix`. A backup system that has never been restored from is a hypothesis, not a backup.
26. **If the source array had VMs** (Phase 3.5, doc 14): `hoserva migrate vm-scan` reads the domain definitions from Unraid's `libvirt.img` on the adopted pool (read-only — they are not in the Flash Backup), vdisks under `/mnt/user/domains` are already in place and were covered by step 16's verification, and each VM's passthrough devices (if any) are re-validated against this machine's own IOMMU groups before the domain is offered for review (doc 14 §5) — never trusted from the source as-is, since the target hardware is not guaranteed to match. Reviewed and started one at a time, same as step 20.

---

## 5. The unprotected window — must be impossible to miss

**Between step 10, when the array stops and the server shuts down, and the completion of step 17, the array has no redundancy whatsoever.**

Unraid's parity is meaningless the moment the array stops and the disk is repurposed. SnapRAID's parity does not exist until the initial sync finishes. A disk failure in that window means that disk's data is gone.

The docs and the UI must state this in those terms, at the point where the user is about to cross the line — not in a footnote.

**Mitigations, stated explicitly:**

- Irreplaceable data must have a backup that is not this array. Always true, *acutely* true here.
- Do not migrate on a disk with any SMART warning. The scan enforces this with a hard warning.
- Complete the initial sync before writing significant new data.
- Do the migration when you have the hours available to finish it, not on a weeknight.

**Rollback** is clean through step 16 as far as the data disks go: nothing has been written to them, so the array returns exactly as it was. Once step 17 begins writing parity, the old parity disk's contents are destroyed and rollback means restoring from backup.

What else rollback needs depends on where Unraid boots from and where Debian was installed. The docs and the UI show the user their row **before they cross step 12**:

| Unraid boots from | Debian goes on | Rollback after step 12 |
|---|---|---|
| USB stick | separate device | Reinsert the stick and boot |
| USB stick | shared NVMe | Reinsert the stick, re-create the Unraid cache, and move appdata back |
| internal, dedicated device | another device | Switch the firmware boot order back |
| internal, dedicated device | that device | Restore the Flash Backup zip to a USB stick (USB Flash Creator) and boot it |
| internal, mirrored pair | another device | Switch the firmware boot order back |
| internal, mirrored pair | one device of the pair | Switch the firmware boot order to the other device; Unraid runs with a degraded boot pool (Internal Boot FAQ), and the wiped device is replaced through its normal drive assignment |
| internal, mirrored pair | both devices | Restore the zip to a USB stick and boot it |
| internal, boot + data | another device | Switch the firmware boot order back; partition 4, the cache, is untouched until step 17 |
| internal, boot + data | the same NVMe | Restore the zip to a USB stick and boot it; re-create the cache |

The shared-NVMe rows differ because step 12 destroyed the Unraid cache; what was on it is on the array since step 5. The internal-boot rows follow Unraid's own Internal Boot documentation and the device layout in doc 08 §2 (the mirrored-pair rows rest on the FAQ's statement that a mirror continues degraded, and on each device keeping its own EFI partition); Hoserva has no internal-boot server to test them against, so they can only ever be verified against fixtures (§2), and until #293 builds the internal-boot fixtures and the scan passes against them they are not verified at all.

The UI must mark step 17 as the point of no return and require explicit confirmation.

---

## 6. Migration UI — `/tools/migrate`

Four phases, matching above:

**Scan** — takes the Flash Backup zip (upload) or, for a server that boots from a USB stick, that stick attached read-only; an internal-boot server has the zip only (§3). It runs detection and displays the full report, downloadable. Nothing is written to any disk or to the stick.

**Review** — the disk mapping table (serial, Unraid disk number, size, filesystem, proposed Hoserva role), editable, filled from the report's structured review (§3). An Unraid internal-boot device appears in it as a row with the role "Unraid boot device", not hidden and not offered for adoption (§3). Share import preview. Template conversion preview with per-template warning counts. Prominent unprotected-window warning with a link to the docs.

**Import** — the adoption job, with progress. Ends at the verification checkpoint, explicitly *before* parity is touched.

**Verify** — side-by-side comparison of pre- and post-migration file counts and sizes per disk and per share. Green means proceed to parity; any mismatch means stop and investigate. Only from here is the "initialise parity" action offered, behind the point-of-no-return confirmation.

**Once Phase 3.5 ships**, Review also lists any VMs the scan found (name, disk size, passthrough devices referenced), read from the adopted pool after Import; vdisks are adopted in place like any other data. See step 26 and doc 14 §5.

---

## 7. Documentation site

Migration needs its own documentation, not a README section.

**Docusaurus, at the root of the project site on GitHub Pages (Q66)** — docs-only mode, with its native docs versioning, a local build-time search index (no hosted search service), and dark mode, and it builds in CI without a server. React matches the web UI's stack. Lives in `site/` at the repo root; `docs/internal/` holds the design docs (Q3).

**Versioning (Q90).** Every stable Hoserva minor release gets its own docs version, a snapshot kept in the repo that `npx docusaurus docs:version X.Y` writes in the release-prep commit for `vX.Y.0`, so an old version's docs are fixed with an ordinary commit rather than a new release. Beta pre-release tags are never snapshotted. Before the first stable release the current docs are the whole site, at the root, with the "unreleased" banner. Once a version exists, the root serves the latest stable version, the current docs (`main`) are served at `/next/` with the "unreleased" banner and `noindex`, older versions carry the "unmaintained" banner, and a navbar dropdown switches between them. All versions stay published until build time or the Pages size canary forces the oldest to be dropped.

### Structure

```
/                          What Hoserva is, who it's for
/getting-started/
  requirements             Hardware, disks, boot device
  install-deb              Install onto existing Debian
  install-iso              ISO bundle (Phase 4)
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
  migrating-vms            Domain XML and passthrough remapping, once Phase 3.5 ships (doc 14 §5)
  exposing-safely          The "don't put this on the internet" guide
/reference/
  cli
  api                      Generated from api/openapi.yaml on every site build (D18)
  config-files
  template-format
```

### Tone requirements

Two pieces carry disproportionate weight:

**`/concepts/how-parity-works`** must be honest about the nightly-sync model, because a user who learns this after losing data will say so publicly and they will be right to. The page should present it as a deliberate tradeoff with clear reasoning, not bury it.

**`/migrating-from-unraid/before-you-start`** must be printable and must lead with the unprotected window. People do this migration at 11pm and skim.
