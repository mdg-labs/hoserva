# Hoserva — Storage Engine

How mergerfs and SnapRAID are orchestrated, and the behaviours Hoserva must get right on top of them.

---

## 1. mergerfs — pooling

### What it does

Unions several independent filesystems into one mount point. Each file lives whole on exactly one underlying disk. There is no striping, no metadata spread, no rebuild when a disk is added.

This is the property that makes mixed disk sizes work, and it has a consequence worth stating plainly to users: **if a disk dies and parity cannot recover it, only the files on that disk are lost.** The rest of the pool is intact and readable. That is a materially better failure mode than RAID/ZFS, where a pool loss is total, and it is a selling point Hoserva should communicate.

The same "whole file on one disk" property has a cost that deserves the same plain statement, not just the upside: **a single file transfer runs at one disk's speed, never striped across the pool.** A large sequential copy tops out around what one spinning disk delivers, well below a striped array (RAIDZ, RAID10, btrfs raid) built from the same disks — the same tradeoff Unraid ships with, since it is inherent to pooling rather than striping, and there is no configuration that removes it. Multiple *concurrent* transfers to different files still spread across disks, which covers the common home-server pattern (several people streaming, several containers writing at once); a single big transfer does not get faster by adding disks. The UI and docs state this plainly rather than let someone discover it mid-copy (doc 07 R3).

### Mount topology — one mount per share

A single mergerfs mount has exactly one create policy and one branch list. That cannot express per-share cache modes (§3) or the per-share allocation settings Unraid users migrate with (doc 05). Hoserva therefore mounts (Q12, validated by spike S6):

```
/mnt/user                   mergerfs over /mnt/disk*=RW                     catch-all, default policy
/mnt/user/<share>           mergerfs, branches by the share's cache mode:
                              cache then move → /mnt/cache/<share>=RW : /mnt/disk*/<share>=NC
                              cache only      → /mnt/cache/<share>=RW
                              array only      → /mnt/disk*/<share>=RW
/run/hoserva/array/<share>  mergerfs over /mnt/disk*/<share>=RW               mover write target (doc 09 §2)
```

`NC` (no-create) keeps already-moved files readable through the share while new files land on cache. The catch-all keeps `ls /mnt/user` and stray top-level directories on the array rather than the boot device. Mount ordering is expressed with systemd `RequiresMountsFor=`. If S6 rejects this topology, the fallback and its feature cost are recorded in Q12 and doc 07 R12.

### Configuration

Key options Hoserva sets — **starting values**, validated against Debian 13's mergerfs package (2.40.2, Q7) by spikes S1 and S6:

| Option | Value | Why |
|---|---|---|
| `category.create` | per share — see below | Where new files land |
| `moveonenospc` | `true` | Move a file mid-write if the target disk fills |
| `dropcacheonclose` | `true` | Limits double caching when `cache.files` is enabled |
| `minfreespace` | configurable, default 50G | Don't fill a disk completely; also guarantees parity headroom (§2) |
| `fsname` | `hoserva-<share>` / `hoserva-pool` | Recognisable in `df` and mount listings |
| `cache.files` | `partial` | Sane default for mixed workloads |
| `cache.entry`, `cache.attr`, `cache.negative_entry`, `cache.statfs` | one "Responsiveness vs. quiet disks" setting | Doc 08 §1; `cache.statfs` favours accuracy (doc 09 §5) |

### Create policies, in plain language

mergerfs policy names are opaque. The UI must translate, per share (Q11):

| mergerfs policy | UI label | Behaviour |
|---|---|---|
| `mspmfs` | **Keep folders together** | Prefers a disk that already holds this directory; if none has room, retries with the parent directory, then its parent — no hard ENOSPC |
| `mfs` | **Balance across disks** | New file goes to the disk with the most free space |
| `lfs` | **Quiet disks** | Least free space that still fits — fills one disk at a time, maximises spindown |
| `ff` | **Fill disks in order** | First disk with room, in branch order — used for Unraid shares imported with *Fill-up* allocation |

**Default for new shares: "Keep folders together".** It keeps a TV series on one disk instead of scattering episodes across six, which both limits blast radius on disk failure and lets the other disks stay spun down during playback. This matches what most Unraid users configure ("split level") without knowing it. `mspmfs` is chosen over `epmfs` because `epmfs` fails with ENOSPC when the disk holding the path fills, instead of falling back (doc 08 §1, doc 09 §1); if spike S1 or S6 shows Debian 13's mergerfs (Q7) does not behave as described, the default reverts to `epmfs` with the per-disk alerts and rebalance suggestion in doc 09 §1.

The UI should explain the tradeoff at the point of choice, not in documentation nobody reads.

### Spindown

Researched in doc 08 §1: every union filesystem shares the problem, Unraid included, so it is not a regression — but it is daily-visible, and Hoserva must actively avoid waking disks itself. Measures:

- Appdata on cache; cache-only shares never touch array branches
- Kernel entry/attribute caching exposed as one plain-language setting (doc 08 §1)
- `updatedb` pruned from `/mnt/user` and `/mnt/disk*` on install (doc 08 §1)
- SMART polling with `smartctl -n standby` (§4)
- **Nothing Hoserva does on a timer walks a data disk**: "files changed since last sync" comes from a fanotify change journal, not a polled `snapraid diff` (§2, Q13); per-disk directory breakdowns are computed at sync time, not live
- A spin-state event log from Phase 1, and "What woke my disks" process attribution later (doc 08 §1, Q32)

**Acceptance criterion for v1** (Q31): with no SMB/NFS clients connected, no containers holding pool paths open, and appdata on cache, array disks stay in standby for at least 30 minutes. Automated in the VM harness and measured on the L4 hardware box (doc 06 §4, §6); the result is published with the release.

---

## 2. SnapRAID — parity

### The model, and the honest caveat

SnapRAID computes parity across the data disks **on demand**, not on write. Between two syncs, newly written or changed data is unprotected.

This is the fundamental difference from Unraid, which writes parity synchronously, and **the UI's primary job is to make sure no user is ever surprised by it.** A user who loses a day of photos because they assumed live parity will — correctly — blame the tool.

### Why it is still the right choice

- The alternative (live parity) means reimplementing Unraid's array engine, which is the one thing decision D1 rules out
- The workload is a good fit: media and backups are large, static, and largely re-acquirable
- Nightly sync plus a genuine off-box backup for irreplaceable data is a sound posture, and the UI can say so

### What Hoserva configures

- `parity` (and `2-parity` when a second parity disk is assigned — Q19) — each parity disk must be ≥ the largest data disk, formatted **XFS**: the parity file is a single file roughly the size of the largest data disk, and ext4's 16 TiB file limit is below today's large disks (Q20). Data disks' `minfreespace` keeps parity headroom when sizes match exactly.
- `content` — **content files on at least 3 distinct physical devices, and at least `parity disks + 2` copies** (Q18): the boot device first (`/var/lib/hoserva/snapraid.content`, so `status` polling never touches the array), cache if present, then data disks with the most free space. The config generator refuses a layout that violates this. Easy to get wrong and catastrophic to get wrong: losing all content files means parity is useless.
- `data d1..dN` — data disks by mount point
- `exclude` — sensible defaults: `/lost+found/`, `/.Trash-*/`, `/appdata/`, `*.unrecoverable`, `/snapraid.content*`, `*.hoserva-moving-*` (in-flight mover copies, doc 09 §2), `.DS_Store`, thumbnail caches, and any share explicitly marked "exclude from parity"
- `blocksize`, `autosave` — defaults, exposed under advanced settings

### Operations

| Op | What it does | Scheduled |
|---|---|---|
| `sync` | Update parity to match current data | Nightly, in the maintenance chain after the mover (Q30) |
| `scrub` | Verify a percentage of existing data against parity | Weekly, after that night's sync; default 8%, older than 10 days |
| `diff` | Report exactly what changed since last sync | Immediately before every sync, and on explicit request — **never on a timer**, because it stats every file and spins up every data disk |
| `touch` | Set non-zero sub-second timestamps | Before a sync, only when `status` reports files needing it (Q17) |
| `status` | Parity age, disk usage, error counts | Polled for the dashboard; reads the boot-device content file |
| `fix` | Restore data from parity | Manual, guided |
| `check` | Verify without repairing | Manual |

**Change journal.** Between syncs, "files changed since last sync" is counted from a fanotify journal: `hoservad` marks each data-disk filesystem and records create/modify/delete/rename events per disk (Q13, spike S7). The events are generated by writes that already woke the disk, so counting never wakes one. The count is shown as approximate; the exact `diff` above replaces it before every sync. The journal also records *which* files changed, which is what makes "files lost on the failed disk, by name" possible (§4).

### The deletion threshold guard

**The single most important safety feature in the product.**

`snapraid diff` reports added / removed / updated / moved / copied counts before a sync runs. If a large number of files were removed or changed, that is either an intentional cleanup or a disaster in progress: ransomware encrypting the share, a failing disk dropping its filesystem, an `rm -rf` with a bad variable, or a share that unmounted and left an empty directory.

**Syncing after such an event destroys the parity that could have recovered it.**

Hoserva therefore blocks the sync when:

- removed files exceed `N` (default 500), **or**
- removed + updated files exceed `X%` of the array (default 10%), **or**
- any data disk reports zero files where it previously had files (the unmounted-disk case)

On block: the sync is held, a high-priority notification fires through every configured channel, and the dashboard shows a prominent banner with the diff and two actions — *Review the diff and sync anyway* or *Cancel and investigate*. The sync does not proceed until a human decides.

The thresholds are configurable but cannot be disabled entirely; the minimum is a confirmation prompt. The defaults are revisited with real diff history at the end of the Phase 1 month-on-real-data gate (Q16).

**The guard applies to every sync, whatever triggered it** — the nightly chain, adding a disk, the sync inside an evacuation, or a manual click.

**Hoserva's own relocations are accounted, not exempted** (Q15). Rebalance, evacuation and share relocation write a manifest of every file they move (relative path, size, mtime, source and target disk). A removal that matches a manifest entry *and* reappears on the recorded target disk in the same diff is shown as its own "moved by Hoserva" group and does not count toward thresholds; every other removal counts as before. A disk in `removing` state (doc 09 §4) is exempt from the zero-files rule, and only that disk is synced with `--force-empty`.

This is what SnapRAID's `--force-empty` and friends exist for, and what almost nobody configures correctly when rolling their own cron job. Shipping it correctly by default is a large part of the product's value.

### Parity freshness, always visible

The dashboard carries a permanent indicator:

- **Green** — parity current, last sync within the schedule window
- **Amber** — ≈N files changed since last sync (always shown with the count, never hidden; counted by the change journal, so approximate until the pre-sync diff)
- **Red** — sync failed, sync blocked by threshold, or parity older than 3× the schedule interval

Hovering or tapping shows the exact timestamp and the diff summary.

---

## 3. Cache and mover

### Purpose

The NVMe absorbs writes at SSD speed, keeps container appdata off the spinning array (so disks can sleep), and shields the array from the constant small-IO chatter of databases and container logs.

### Two distinct roles — the UI must separate them

1. **Write cache** — new files land on cache, the mover relocates them to the array later
2. **Permanent residence** — appdata, databases, container volumes live on cache and are *never* moved

Conflating these is a common support case on cache-based NAS setups ("my Plex database got moved to the array and everything is slow"). Hoserva makes it a per-share setting with three explicit values, each implemented as that share's own mergerfs branch list (§1):

| Setting | Behaviour |
|---|---|
| **Cache then move** | Writes to cache, mover relocates to array. For media ingest, downloads. |
| **Cache only** | Lives on cache permanently, never moved. For appdata, databases. **Not covered by parity** — needs its own backup. |
| **Array only** | Bypasses cache entirely. For bulk writes larger than the cache. |

The "cache only" option must carry a visible warning that this data is not protected by parity, plus a pointer to the appdata backup job.

Changing a share's mode does not move existing files by itself; the UI offers a one-shot **share relocation** job (doc 09 §2) at the moment the mode changes.

### Mover

Full design in doc 09 §2. In summary:

- Scheduled as the first step of the nightly maintenance chain (default 02:00), so moved files are always included in that night's sync — ordering is structural, not a clock coincidence (Q30)
- Threshold-triggered (default: also run when cache exceeds 70%)
- Skips files with open file handles rather than corrupting them
- Writes through the share's array-only mergerfs mount, so mergerfs places every file (§1)
- Preserves ownership, permissions, xattrs, ACLs and timestamps
- Runs as a resumable job with progress and a log, like everything else
- Respects per-share settings above

### Appdata backup

Because "cache only" data is outside parity, Hoserva ships a built-in scheduled appdata backup job — per-container stop policy, per-container archives, multi-destination, verified. Full design in doc 10 §2. It is not optional infrastructure — without it, the cache is a single point of failure for every service.

---

## 4. Disk lifecycle

### Adding a disk

1. Detect and show the disk with model, serial, size, existing filesystem, SMART status
2. Warn if it contains data
3. Format (default XFS) or adopt an existing XFS filesystem as-is
4. Mount at the next free `/mnt/diskN`
5. Add to the branch lists of the catch-all and every share mount, and to the SnapRAID data list
6. Remount, regenerate configs
7. Trigger a sync through the threshold guard (an empty new disk syncs fast; an adopted disk with data does not, and the UI says so)

**No rebuild.** Capacity is available immediately. The UI should say so explicitly, because users coming from ZFS expect hours of resilvering and will assume something went wrong.

### Removing a disk

Full procedure in doc 09 §4. The ordering is SnapRAID-driven (Q14): files are copied to the remaining disks and verified, parity is synced so the copies are protected, and only then are the originals deleted and the disk removed from the configuration — deleting first would weaken recovery of *other* disks until the next sync.

Long-running, interruptible, resumable on user action (Q29). Progress in files and bytes.

### Replacing a failed disk

1. Mark the disk failed; pool continues serving the remaining disks (degraded, with a persistent banner)
2. User physically swaps the disk
3. Identify the new disk, format, mount at the same `/mnt/diskN`
4. `snapraid fix -d dN` reconstructs the contents from parity + remaining disks
5. Verify, then resume the normal schedule

**Honest constraint that must be surfaced in the UI:** reconstruction can only restore data that was present at the last successful sync. Files written after it are gone. The UI shows the last sync time and the files on that disk that were pending at failure — counted and, from the change journal (§2), named — so the user knows exactly what they lost instead of discovering it months later.

### SMART monitoring

- Poll all disks on a schedule (default 15 min; does not wake sleeping disks — use `smartctl -n standby`)
- Track: reallocated sectors, pending sectors, offline uncorrectable, CRC errors, temperature, power-on hours
- Alert on: any increase in reallocated/pending, temperature above threshold, self-test failure
- Scheduled short self-tests weekly, long self-tests monthly, configurable
- **Trend, not just current value** — reallocated sector count going from 0 to 4 is the signal; the absolute number is not

---

## 5. Filesystem choice

**Default XFS**, for two reasons: it is what Unraid uses (so migrated disks are adopted directly, doc 05) and it handles large files and large directories well.

**ext4 offered** as an alternative for data disks. **btrfs available but not default** — it works, but its own redundancy features overlap confusingly with SnapRAID, and per-disk btrfs on a SnapRAID array is a support burden with little upside. Migrated single-device btrfs and ext4 disks are adopted as-is (Q23).

**Parity disks are always XFS** (§2, Q20).

**ZFS not supported.** Out of scope (doc 00 §4).

---

## 6. Failure modes and expected behaviour

| Scenario | Hoserva behaviour |
|---|---|
| One data disk fails | Pool degraded, other disks keep serving, alert fires, guided replace flow offered |
| Two data disks fail (single parity) | Only recoverable to the extent of one disk; UI states plainly what is and isn't recoverable |
| Parity disk fails | Data intact and fully accessible, no redundancy; alert; replace and full re-sync |
| Cache disk fails | Pending writes and all "cache only" data lost; array intact; restore appdata from backup |
| Disk unmounts unexpectedly | Sync blocked by threshold guard; pool marked degraded; alert |
| Boot device fails | Array data untouched; reinstall Debian + Hoserva, restore config from backup |
| Power loss mid-sync | Sync marked interrupted on restart; not auto-resumed; user prompted to re-run |
| Power loss mid-mover / rebalance / evacuation | Job marked interrupted; copy-verify-delete leaves a duplicate, never a gap; resumes from its checkpoint on user action (doc 09, Q29) |
| Pool full | `moveonenospc` handles in-flight writes; `minfreespace` prevents total fill; alert at configurable threshold |
| Ransomware over SMB | Threshold guard blocks the sync, preserving pre-encryption parity; alert; user can `snapraid fix` to recover |

That last row is worth designing toward deliberately. It is the scenario where a correctly built SnapRAID system meaningfully outperforms live-parity RAID, and it should be a documented, tested recovery path — not an accident.
