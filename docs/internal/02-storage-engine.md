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

### Startup order and a disk missing at boot

A container that starts before `/mnt/user` is mounted writes its data onto the boot device, and the user sees an empty app. Hoserva prevents that structurally (Q69):

- Every data, parity and cache mount is `nofail`, so a dead disk never hangs boot — `nofail` alone does this: it drops the mount from `local-fs.target`'s required ordering, so boot proceeds without ever waiting on the device. `x-systemd.device-timeout=` is not part of this: that option only applies to an `/etc/fstab` entry and is silently ignored in a native `.mount` unit's own `Options=`, so Hoserva's generated units never emit it.
- Every mountpoint directory is made immutable while empty, so a write to an unmounted path fails instead of landing on the boot device
- Samba, NFS, Docker and libvirt start after `hoserva-storage.target`, through managed systemd drop-ins; `hoservad` reaches that target only when every expected disk is present by identity (Q21), or once the user acknowledges the degraded state — and never while a larger-data-disk upgrade is pending (§4, UR2)
- With a disk missing, the pool still mounts from the remaining disks, and the guard's zero-files rule holds (§2)

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

**Acceptance criterion for v1** (Q31): with no SMB/NFS clients connected, no containers holding pool paths open, and appdata on cache, array disks stay in standby for at least 30 minutes. Automated in the lab and the VM harness as a zero-IO proxy (doc 06 §4, §6); the result and its stated residual risk are published with the release.

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

The thresholds are configurable but cannot be disabled entirely; the minimum is a confirmation prompt. The defaults are revisited with the soak test's diff history at the end of Phase 1 (doc 06 §6) (Q16).

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

### Stopping the array

*Stop array* (`hoserva array stop`) puts the system in maintenance mode (Q70): new jobs are refused, running resumable jobs stop at their next checkpoint and the rest are marked interrupted, VMs shut down (gracefully, then forced after a timeout), containers stop, Samba and NFS stop, and the per-share mounts, the catch-all and the disks unmount in that order. *Start array* reverses it, and is refused while a larger-data-disk upgrade is pending (see the upgrade's state machine below). System shutdown and reboot run the same sequence. No disk is physically touched outside maintenance mode or a powered-off box.

### Adding a disk

1. Detect and show the disk with model, serial, size, existing filesystem, SMART status
2. Warn if it contains data
3. Format (default XFS) or adopt an existing XFS filesystem as-is — through the disk's own stable `/dev/disk/by-id` path when one is known (Q21), not its `/dev/sdX` path, so the disk that is actually formatted or checked is the one this step confirmed, even if a udev event renumbers `/dev/sdX` paths between confirmation and the call
4. Mount at the next free `/mnt/diskN`
5. Add to the branch lists of the catch-all and every share mount, and to the SnapRAID data list
6. Remount, regenerate configs
7. Trigger a sync through the threshold guard (an empty new disk syncs fast; an adopted disk with data does not, and the UI says so)

**No rebuild.** Capacity is available immediately. The UI should say so explicitly, because users coming from ZFS expect hours of resilvering and will assume something went wrong.

### Removing a disk

Full procedure in doc 09 §4. The ordering is SnapRAID-driven (Q14): files are copied to the remaining disks and verified, parity is synced so the copies are protected, and only then are the originals deleted and the disk removed from the configuration — deleting first would weaken recovery of *other* disks until the next sync.

Long-running, interruptible, resumable on user action (Q29). Progress in files and bytes.

### Upgrading a disk to a larger one

A healthy disk is never rebuilt from parity to replace it — that would leave the array without redundancy while a perfectly good source exists. Two guided flows, each keeping the old disk untouched until the new one verifies (Q71):

- **Larger parity disk:** copy the parity file to the new disk, verify it byte for byte, switch the configuration, pass `snapraid check`, then release the old parity disk. Protection is continuous. When a new data disk would be larger than the current parity, the flow offers this first and reuses the old parity disk as a data disk.
- **Larger data disk:** with the array stopped (Q70), first confirm that `snapraid diff` has nothing to sync. Then format the new disk and copy the old disk's files to it with ownership, xattrs and timestamps. Verify the copy, and mount the new disk at the same `/mnt/diskN`. Require `snapraid diff` to show no removed or updated files, then release the old disk. The array stays stopped from submit until the upgrade succeeds, fails or is cancelled. The state machine below defines every step, cancel, interruption and restart.

### Larger data disk: the upgrade's state machine

This is the specification the larger-data-disk upgrade (Q71) implements. For every state the job can be in, and every event that can reach it, it says:

- whether the event is allowed, or the error that refuses it;
- what happens;
- what is mounted before and after;
- what SQLite names;
- the state that results.

Where a cell needs a mechanism Hoserva does not have yet, the mechanism is listed under **Requirements**.

**Names.**

- **A** is the old disk and **B** the new one. **N** is the slot being upgraded, and **S** is its mountpoint, `/mnt/diskN`.
- **G** is the staging path where B is mounted during the copy.
- *Other disks* are every other array data and parity disk.
- The *pool* is the catch-all, the per-share mounts and the mover targets.
- *Services* are Samba, NFS, containers and VMs.
- *SQLite names* means slot N's array-disk row: its disk identity and filesystem UUID. S's generated mount unit follows that row only once Release regenerates it.
- An upgrade is **pending** from submit until it ends `succeeded`, `failed` or `cancelled`.

**Mount sets.** Each cell gives the mounts before and after as one of these sets.

| Set | What is mounted |
|---|---|
| Stopped | Nothing: no array disk, nothing at G and no pool mount. Services are stopped |
| Old | Every other disk, and A at S, each by filesystem UUID. Nothing is at G. The pool and services are down |
| Copy | *Old*, plus B at G |
| New | Every other disk, and B at S by B's filesystem UUID. A is not mounted, and nothing is at G. The pool and services are down |
| Unknown | Whatever a killed run, a failed unmount or boot left behind. It is never trusted: the next Unwind clears it |

**Procedures the cells use.**

- **Unwind** brings the system to *Stopped* and proves it. It runs the stop sequence's teardown without its drain: services first, then the pool, then every array disk mountpoint including S, then G.
  - It unmounts each path whoever mounted it, and repeats until the path is no longer a mountpoint.
  - A path that does not exist counts as unmounted.
  - It succeeds only once the kernel mount table shows none of those paths mounted.
  - It never unmounts a disk while a pool mount or a service above it is still up.
- **Establish** mounts *Old*, *Copy* or *New* by filesystem UUID, then confirms each path's UUID in the kernel mount table.
  - A's and B's UUIDs are checked against the values fixed in the job (UR4). The other disks are checked against SQLite.
  - Establish(*New*) never mounts A.
- **Release** is one SQLite transaction that names B for slot N, followed by regenerating every generated file that names slot N's disk, S's mount unit among them, and then rebuilding the daemon's array sequence.
  - If SQLite already names B, only the transaction is skipped. The regeneration and the rebuild always run.
  - A is then an unassigned disk.
  - Release needs no disk mounted.
- **Startup recovery** runs when `hoservad` starts and finds a pending upgrade. It runs before the daemon accepts any request that submits or resumes a job.
  - It enters maintenance mode (UR1) and holds the storage readiness gate (Q69) not ready (UR2).
  - Then it runs Unwind.

Every run starts with Unwind, then Establish for its checkpoint. Every run ends with Unwind, whether it succeeds, fails, stops or is cancelled.

A run records `succeeded`, `failed` or `cancelled` only after that final Unwind succeeds. If the Unwind fails, the job is recorded as follows instead:

- status `interrupted`, at its last saved checkpoint;
- error code `disk_upgrade_cleanup_failed`, with the paths that are still mounted;
- mounts *Unknown*.

That rule applies to every cell below that ends in `succeeded`, `failed` or `cancelled`.

**States.** The persisted checkpoint names the phase that a resume starts at. Formatting saves no checkpoint of its own, so a run stopped there resumes at *none*. From Copying on, the checkpoint also carries B's filesystem UUID (UR4).

| State | Job status | Checkpoint | Mounts | SQLite names |
|---|---|---|---|---|
| Queued | `queued` | none, or the one it was resumed from, up to diffing | *Stopped* or *Unknown* | A |
| Queued at releasing | `queued` | releasing, the one it was resumed from | *Stopped* or *Unknown* | A or B |
| None | `running` | none | *Stopped*, then *Old* for the pre-format diff | A |
| Formatting | `running` | none | *Old*, then *Copy* once B is at G | A |
| Copying | `running` | copying, with the last completed path | *Copy* | A |
| Verifying | `running` | verifying | *Copy* | A |
| Remounting | `running` | remounting | *Copy*, then *New* | A |
| Diffing | `running` | diffing | *New* | A |
| Releasing | `running` | releasing | *New*, then *Stopped* | A until Release's transaction commits, then B |
| Interrupted at *c* | `interrupted` | *c*: none, copying, verifying, remounting, diffing or releasing | *Stopped*. After `disk_upgrade_cleanup_failed` or a failed startup recovery, *Unknown* | A. At releasing, A or B |
| Done | `succeeded` | — | *Stopped* | B |
| Ended | `failed` or `cancelled` | — | *Stopped* | A |

An interrupted job runs nothing. Only E3 to E8 reach it: cancel, restart, `array start`, `array stop`, resume, and a second submit.

**E1 — The run proceeds.**

| State | Before | Action | After | SQLite names | Result |
|---|---|---|---|---|---|
| Queued, or queued at releasing | *Stopped* or *Unknown* | It starts once no conflicting job runs. Maintenance mode and UR3 leave no other job running, so it starts at once | as the state it starts in | A. Queued at releasing: A or B | None, or the state for its checkpoint (E5) |
| None | *Stopped* | Unwind. Confirm A and B by their by-id identities. Establish(*Old*). `snapraid diff` must report nothing to sync | *Old* | A | Formatting |
| Formatting | *Old* | Format B through its by-id path, mount B at G, and confirm that G holds B's new UUID. Save *copying* with B's UUID | *Copy* | A | Copying |
| Copying | *Copy* | Copy S to G with ownership, xattrs and timestamps, saving the last completed path as it goes. Then save *verifying* | *Copy* | A | Verifying |
| Verifying | *Copy* | Compare G with S. On a full match, save *remounting* | *Copy* | A | Remounting |
| Remounting | *Copy* | Unmount G, unmount S, then mount B at S by B's UUID. Confirm that S holds B's UUID, then save *diffing* | *New* | A | Diffing |
| Diffing | *New* | Confirm that S holds B's UUID. `snapraid diff` must show no removed or updated files. Then save *releasing*; if a cancel races this save, E3 decides | *New* | A | Releasing |
| Releasing | *New* | Release, then Unwind | *Stopped* | B | Done. The array stays stopped until the user starts it |

**E2 — A step fails.** A step fails when its command errors, a disk is missing or a UUID confirmation fails. Every row runs Unwind before it records its result.

| State | Before | Failure | After | SQLite names | Result |
|---|---|---|---|---|---|
| None | *Stopped* or *Old* | `snapraid diff` reports anything to sync | *Stopped* | A | `failed`, `disk_upgrade_array_not_synced`. The message says to start the array, sync, stop it and submit again. Nothing was formatted |
| None | *Stopped* or *Old* | any other error | *Stopped* | A | `failed`, with the error. Nothing was formatted |
| Formatting | *Old* or *Copy* | the format fails, G's mount fails, or G's UUID does not match | *Stopped* | A | `failed`, with the error. B holds nothing of value |
| Copying | *Copy* | a read, write or mount error, or a UUID mismatch | *Stopped* | A | `interrupted` at copying, keeping the last completed path, `job_needs_retry` |
| Verifying | *Copy* | G does not match S | *Stopped* | A | `failed`, `disk_upgrade_copy_mismatch`, naming the first mismatched path. A is still the array's disk, unchanged |
| Verifying | *Copy* | any other error | *Stopped* | A | `interrupted` at verifying, `job_needs_retry` |
| Remounting | *Copy*, or partway to *New* | an unmount fails, B's mount fails, or the UUID confirmation fails | *Stopped* | A | `interrupted` at remounting, `job_needs_retry` |
| Diffing | *New* | `snapraid diff` shows removed or updated files | *Stopped* | A | `failed`, `disk_upgrade_diff_not_clean`, with the counts. A is the array's disk, unchanged since submit |
| Diffing | *New* | the diff errors, or S does not hold B's UUID | *Stopped* | A | `interrupted` at diffing, `job_needs_retry` |
| Releasing | *New* | the transaction fails and rolls back | *Stopped* | A | `interrupted` at releasing, `job_needs_retry` |
| Releasing | *New* | regeneration or the array-sequence rebuild fails after the commit | *Stopped* | B | `interrupted` at releasing, `job_needs_retry`. S's mount unit may still name A; E6 refusing `array start` keeps it from being mounted, and the resume's Release regenerates it (E5) |
| Any | as the row | the final Unwind fails | *Unknown* | as the row | `interrupted` at the last saved checkpoint, `disk_upgrade_cleanup_failed` |

**E3 — Cancel (`cancelJob`), which is the abort.**

| State | Before | Response | Action | After | SQLite names | Result |
|---|---|---|---|---|---|---|
| Queued, at none to diffing | *Stopped* or *Unknown* | accepted | Remove it from the queue, then Unwind | *Stopped* | A | `cancelled` |
| Queued at releasing | *Stopped* or *Unknown* | refused, 409 `job_not_cancellable`: the upgrade is past its release decision, so it finishes once it runs | none: it stays queued | unchanged | A or B | Queued at releasing |
| None to Diffing, running | *Old*, *Copy* or *New* | accepted. The response is the running job; the outcome follows | Cancel the run and wait for it to return, then Unwind | *Stopped* | A | `cancelled` |
| Diffing, with a cancel racing the save of *releasing* | *New* | decided under one lock together with the save (UR7) | If the cancel comes first, the save is refused, Release never runs, and the row above applies. If the save comes first, the next row applies | as the winning row | A | `cancelled`, or Releasing |
| Releasing, running: a fresh run, or one resumed at releasing | *New* or *Stopped* | refused, 409 `job_not_cancellable`: the upgrade is past its release decision and finishes on its own | none | unchanged | A or B | Releasing |
| Interrupted at none to diffing | *Stopped* or *Unknown* | 200 with the cancelled job. If Unwind fails: 409 `disk_upgrade_cleanup_failed` | Unwind, while holding the job so that no resume can start (UR7) | *Stopped*. *Unknown* if Unwind fails | A | `cancelled`. If Unwind fails, still `interrupted` |
| Interrupted at releasing | *Stopped* or *Unknown* | refused, 409 `job_not_cancellable`: the upgrade has committed to the new disk, so resume it to finish | none | unchanged | A or B | unchanged |
| Done or Ended | *Stopped* | refused, 409 `job_not_running` | none | *Stopped* | unchanged | unchanged |

After `cancelled`:

- SQLite names A, and A holds exactly what it held at submit.
- `array start` is allowed, and mounts A.
- B keeps a discarded copy and is an unassigned disk.

**E4 — The daemon is killed or restarted.** This covers a crash, `systemctl restart`, a package upgrade, a reboot and a power loss. No cleanup inside the old process is assumed. On the next start, `RecoverFromRestart` marks every queued or running job interrupted at its last saved checkpoint. Then startup recovery runs for each pending upgrade.

| State | Before | After recovery | SQLite names | Result |
|---|---|---|---|---|
| Queued, at none to diffing | *Stopped* or *Unknown* | *Stopped* | A | `interrupted` at its checkpoint |
| Queued at releasing | *Stopped* or *Unknown* | *Stopped* | A or B | `interrupted` at releasing |
| None or Formatting | *Old*, *Copy* or partway. G may not exist yet | *Stopped* | A | `interrupted` at none |
| Copying | *Copy* | *Stopped* | A | `interrupted` at copying, keeping the last saved path |
| Verifying | *Copy* | *Stopped* | A | `interrupted` at verifying |
| Remounting | *Unknown*: A, B or nothing at S | *Stopped* | A | `interrupted` at remounting |
| Diffing | *New* | *Stopped* | A | `interrupted` at diffing |
| Releasing | *New* or *Stopped* | *Stopped* | A or B: the transaction either committed or did not | `interrupted` at releasing |
| Interrupted | *Stopped* or *Unknown* | *Stopped* | unchanged | unchanged. Startup recovery runs again |
| Done or Ended | *Stopped* | nothing is done | unchanged | unchanged |

After a reboot, "before" also includes anything boot mounted, such as array disks systemd activated from their units. That can put A at S, or B once Release has regenerated the unit. Startup recovery unmounts it.

If startup recovery fails:

- the failure is recorded on the job as `disk_upgrade_cleanup_failed`, with the paths still mounted, and the mounts are *Unknown*;
- the daemon still starts and serves the API, so resume and cancel stay reachable (UR8).

A pending upgrade does not refuse a clean shutdown or reboot, including the UPS low-battery path (Q77).

- The shutdown asks a running upgrade to stop at its next checkpoint. The job then runs Unwind and ends interrupted.
- The shutdown then proceeds, bounded by its own timeout.
- Correctness never depends on that stop finishing. Whatever it leaves behind is covered by this table.

**E5 — Resume.**

- Resume needs maintenance mode. That always holds while an upgrade is pending (UR1).
- The job's cancellability is fixed from its persisted checkpoint before the job becomes visible to Cancel, whether it is then queued or running (UR7). A job resumed at releasing is never cancellable, queued or running.
- Resume and an abort of the same job never run at the same time (UR7).
- Every resume starts with Unwind. If that Unwind fails, the resume ends interrupted at the same checkpoint, `disk_upgrade_cleanup_failed`.
- If Establish cannot mount a disk, or a UUID does not match, the resume runs Unwind and ends interrupted at the same checkpoint, `disk_upgrade_mount_unconfirmed`. It reads and writes no file.
- Once Verifying has passed, no resume ever returns to Copying.

| State | Before | Response | Action | After | SQLite names | Result |
|---|---|---|---|---|---|---|
| Queued or running | as the state | refused, 409 `job_not_interrupted` | none | unchanged | unchanged | unchanged |
| Interrupted at none | *Stopped* or *Unknown* | accepted; cancellable | Unwind, then E1's None row. The diff gate runs again, and B is formatted again | *Old* | A | None |
| Interrupted at copying | *Stopped* or *Unknown* | accepted; cancellable | Unwind, then Establish(*Copy*). Continue after the last saved path | *Copy* | A | Copying |
| Interrupted at verifying | *Stopped* or *Unknown* | accepted; cancellable | Unwind, then Establish(*Copy*). Verify from the start | *Copy* | A | Verifying |
| Interrupted at remounting | *Stopped* or *Unknown* | accepted; cancellable | Unwind, then Establish(*New*), which never mounts A. Save *diffing* | *New* | A | Diffing |
| Interrupted at diffing | *Stopped* or *Unknown* | accepted; cancellable | Unwind, then Establish(*New*) | *New* | A | Diffing |
| Interrupted at releasing | *Stopped* or *Unknown* | accepted; not cancellable | Unwind, Release, then Unwind. Release regenerates S's mount unit and the other generated files even when SQLite already names B. No disk is mounted: the clean diff that saved *releasing* ran against B, confirmed at S, and nothing has served S since | *Stopped* | B | Done |
| Done or Ended | *Stopped* | refused, 409 `job_not_interrupted` | none | *Stopped* | unchanged | unchanged |

A resume at remounting or later does not need A, so an old disk that fails after Verifying does not stop the upgrade.

A resume at releasing does not need B either. If B has gone missing, Release still commits, the array then starts degraded (Q69), and B is replaced like any other failed disk. The job's result notice says that B is missing, and that the old disk A still holds a complete copy of the slot's files until B is rebuilt.

**E6 — `array start`.** Boot is `array start` by another route.

| State | Before | Response | Action | After | SQLite names | Result |
|---|---|---|---|---|---|---|
| Any pending state: queued, running at any checkpoint, or interrupted at any checkpoint | as the state | refused, 409 `disk_upgrade_pending`, naming the job and saying to resume or cancel it | Nothing is mounted or started, and maintenance mode stays on | unchanged | unchanged | unchanged |
| Done or Ended | *Stopped* | accepted | The normal start (UR9). Disks mount from their generated units, which follow SQLite: B after `succeeded`, A after `failed` or `cancelled`. UR9 confirms each against SQLite before anything above the disks starts. Then the pool mounts and services start, and maintenance mode ends | the array is started | unchanged | the array is started |

While an upgrade is pending:

- the storage readiness gate (Q69) reports not ready (UR2), so boot mounts no pool and starts none of Samba, NFS, Docker or libvirt;
- no other operation mounts a pool mount or starts a service (UR6).

So while an upgrade is pending, neither A nor B can serve S, whatever the checkpoint.

**E7 — `array stop`.**

| State | Before | Response | Action | After | SQLite names | Result |
|---|---|---|---|---|---|---|
| Any pending state | as the state | refused, 409 `disk_upgrade_pending`: the array is already stopped for this upgrade | Nothing: no drain, no signal to the job, no unmount | unchanged | unchanged | unchanged |
| Done or Ended | *Stopped*, or the started array | accepted | The normal stop sequence (Q70). It finds everything already stopped unless the array was started since | *Stopped* | unchanged | the array is stopped |

The pending-state refusal belongs to the `array stop` command and its API operation, not to the shared stop sequence. System shutdown, reboot and the UPS low-battery path run that sequence directly, and must still proceed while an upgrade is pending (E4).

**E8 — A second upgrade is submitted.**

| State | Before | Response | Action | After | SQLite names | Result |
|---|---|---|---|---|---|---|
| Any pending state | as the state | A data-disk upgrade is refused, 409 `disk_upgrade_pending`, naming the pending job. A parity-disk upgrade or any other job is refused, 409 `maintenance_mode` | none | unchanged | unchanged | unchanged |
| Done or Ended, after a stop sequence that completed (UR3) | *Stopped* | accepted | A new job starts at None (E1) | *Old*, for the new job's slot | the new slot's current disk | the new job is running |
| Done or Ended, with the array started or its stop sequence incomplete | as found | refused, 409 `array_not_stopped`: stop the array first | none | unchanged | unchanged | unchanged |

**Invariants** the implementation keeps.

1. **SQLite and the mounted slot never disagree while anything can serve the slot.** While an upgrade is pending, nothing can serve S:
   - maintenance mode holds across restarts (UR1);
   - `array start` and boot's readiness gate refuse (E6);
   - Unwind runs before every terminal status.

   Once the upgrade ends, S is unmounted. After that, either `array start` mounts it from its generated unit and, before any pool mount or service comes up, confirms by filesystem UUID that S holds the disk SQLite names (UR9), or a later upgrade mounts it through Establish.
2. **The old disk is never written after Verifying.** In fact no file on A is created, changed, renamed or removed from submit on:
   - maintenance mode keeps every writer away, and the job only reads A;
   - once Verifying passes, the job never mounts A again (Establish(*New*)), and no path leads back to Copying.

   A is written again only once the upgrade has failed or been cancelled, when it is once more the disk SQLite names.
3. **No reported outcome is ever untrue.**
   - `succeeded` means SQLite names B, the run that recorded it regenerated the generated files from that row, and Unwind succeeded.
   - `failed` and `cancelled` mean SQLite names A, A holds what it held at submit, and Unwind succeeded.
   - Whenever any of that cannot be established, the job is `interrupted` and still holds the array stopped.
4. **Once the *releasing* checkpoint is saved, the only outcome is `succeeded`.** Cancel is refused while the job is queued, running or interrupted, and every failure leaves the job resumable at releasing (E2, E3, E5).
5. **Nothing is trusted across runs.**
   - Every run starts with Unwind and establishes its own mounts by UUID.
   - Every run ends with Unwind.
   - Unwind accepts paths that are already unmounted, or that never existed.

**Requirements.** These are the mechanisms the cells need that Hoserva does not have yet.

- **UR1.** Maintenance mode is held in memory. While a data-disk upgrade is pending in SQLite, `hoservad` must start in maintenance mode, before it accepts any job.
- **UR2.** The storage readiness gate (Q69) reports not ready while an upgrade is pending.
- **UR3.** A data-disk upgrade is admitted only once the stop sequence has completed. Having entered maintenance mode is not enough: a stop that failed partway can leave services or mounts up.
- **UR4.** The identities are fixed:
  - A's identity and filesystem UUID are fixed in the job's parameters at submit.
  - B's filesystem UUID is saved in the checkpoint when Formatting finishes.
  - Every UUID confirmation reads the source of the path from the kernel mount table and compares it with these values. It never checks S against SQLite, and never re-reads a device instead.
- **UR5.** Unmounting works by path, whatever mounted it: a systemd unit, a raw mount or a stacked mount.
  - A path that is not a mountpoint, or does not exist, counts as unmounted.
  - Success is judged from the mount table, not from a command's exit status.
- **UR6.** While an upgrade is pending, nothing mounts a pool mount or starts a service. That includes share changes.
- **UR7.** The scheduler rules for this job type:
  - It decides a cancel and the save of *releasing* under one lock.
  - It fixes a resumed job's cancellability from its persisted checkpoint before the job is visible, whether it is queued or running.
  - It reports `cancellable: false` from releasing on, queued included, so the UI offers no Cancel.
  - It records `cancelled`, `failed` or `succeeded` only after the run's final Unwind has succeeded.
  - Cancelling an interrupted upgrade runs Unwind before recording anything, and never runs at the same time as a resume of the same job.
- **UR8.** A startup-recovery failure is recorded on the job and never stops the daemon.
- **UR9.** `array start` mounts the array disks, then confirms by filesystem UUID that every array-disk mountpoint that is mounted holds the disk SQLite names. That covers the mountpoints it mounted as well as those it found already mounted. The check runs before any pool mount or service starts. If one does not match, it refuses: it unmounts the array disks again, starts no pool mount or service, and stays in maintenance mode.

**Edge cases and the cells that resolve them.**

| Edge case | Resolved by |
|---|---|
| G does not exist at recovery or abort, because the run stopped before Formatting mounted it | UR5; E4 None or Formatting; E3 Interrupted at none to diffing |
| Recovery meets a unit systemd never loaded, or a path its unit name does not escape | UR5, UR8 |
| Cancel at releasing on a fresh run | E3 Releasing, running; E3 Diffing race row |
| Cancel at releasing after a resume | E5 Interrupted at releasing (not cancellable before it is visible); E3 Queued at releasing; E3 Releasing, running |
| Cancel at releasing while interrupted | E3 Interrupted at releasing |
| Regeneration fails after Release's commit, then the job is resumed | E2 Releasing; Release (regeneration always runs); E5 Interrupted at releasing; UR9 |
| An unmount fails during a running cancel | E3 None to Diffing, running, under the Unwind rule; UR7 |
| An unmount fails during a cancel of an interrupted job | E3 Interrupted at none to diffing |
| Abort while the array is live | E6 (no pending state admits `array start`); UR1, UR2, UR3; Unwind's order |
| Dirty diff before Formatting | E2 None, `disk_upgrade_array_not_synced` |
| Dirty diff after Remounting | E2 Diffing, `disk_upgrade_diff_not_clean`: Unwind, then `failed`, with SQLite naming A |
| Dirty diff reported as success | E2 Diffing; invariant 3 |
| Restart, or a package upgrade that rebuilds `hoservad`, before the first checkpoint | E4 None or Formatting; E5 Interrupted at none; E3 Interrupted at none to diffing |
| `array start` while running before the first checkpoint | E6 |
| The copy source is an unmounted directory after `array stop` | E1 None: Establish(*Old*) confirms A's UUID at S before Copying; E5 Interrupted at copying |
| A at S on resume after a reboot | E4 (boot mounts); E5 Interrupted at remounting or diffing: Unwind, then Establish(*New*) |
| B left at S across a restart, then `array start` | E4 startup recovery; E6; UR9 |
| `array start` between an interruption and its resume | E6 |
| Going back to copying after B may have served S | E5: no path back to Copying; invariant 2 |
| A write, delete or rename on A while the upgrade is pending | Invariant 2; E6; UR3; UR6 |
| An error or cancel after Remounting that leaves no way forward | E2 Remounting to Releasing; E3; E5 |
| S already mounted before the first run, after a stop that failed partway | UR3; E1 None starts with Unwind |

### Replacing a failed disk

1. Mark the disk failed; pool continues serving the remaining disks (degraded, with a persistent banner)
2. User stops the array (maintenance mode) or powers off, and physically swaps the disk
3. Identify the new disk, format (again through its by-id path when known, as "Adding a disk" step 3 does), mount at the same `/mnt/diskN`
4. `snapraid fix -d dN` reconstructs the contents from parity + remaining disks
5. Verify, then resume the normal schedule

**Honest constraint that must be surfaced in the UI:** reconstruction can only restore data that was present at the last successful sync. Files written after it are gone. The UI shows the last sync time and the files on that disk that were pending at failure — counted and, from the change journal (§2), named — so the user knows exactly what they lost instead of discovering it months later.

### SMART monitoring

- Poll all disks on a schedule (default 15 min; does not wake sleeping disks — use `smartctl -n standby`)
- Track: reallocated sectors, pending sectors, offline uncorrectable, CRC errors, temperature, power-on hours
- Alert on: any increase in reallocated/pending, temperature above threshold, self-test failure
- Scheduled short self-tests weekly, long self-tests monthly, configurable
- **Trend, not just current value** — reallocated sector count going from 0 to 4 is the signal; the absolute number is not
- History is kept in a separate, downsampled `metrics.db` (Q74)

### Disks outside the array

A disk with the Ignore role, or a USB disk plugged in later, can be mounted by filesystem UUID at `/mnt/disks/<label>` and ejected safely (Q72). External disks are never in the pool or parity and are ignored by the threshold guard and the change journal; they exist to be a backup destination or a container path. Nothing mounts automatically on plug-in.

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
| Disk missing at boot | Boot completes; pool mounts from the remaining disks; shares, containers and VMs wait until the user acknowledges the degraded state (Q69) |
| Boot device fails | Array data untouched; reinstall Debian + Hoserva, restore config from backup |
| Power loss with a UPS | On battery: mover paused, syncs held; at low battery: jobs checkpointed and a clean shutdown (Q77) |
| Power loss mid-sync | Sync marked interrupted on restart; not auto-resumed; user prompted to re-run |
| Power loss mid-mover / rebalance / evacuation | Job marked interrupted; copy-verify-delete leaves a duplicate, never a gap; resumes from its checkpoint on user action (doc 09, Q29) |
| Pool full | `moveonenospc` handles in-flight writes; `minfreespace` prevents total fill; alert at configurable threshold |
| Ransomware over SMB | Threshold guard blocks the sync, preserving pre-encryption parity; alert; user can `snapraid fix` to recover |

That last row is worth designing toward deliberately. It is the scenario where a correctly built SnapRAID system meaningfully outperforms live-parity RAID, and it should be a documented, tested recovery path — not an accident.
