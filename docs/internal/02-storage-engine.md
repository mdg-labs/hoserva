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
/run/hoserva/array/<share>  mergerfs over /run/hoserva/branches/mnt/disk*/<share>=RW   mover write target (doc 09 §2)
/run/hoserva/branches/mnt/diskN   bind of /mnt/diskN, nosymfollow                one per data disk, mover branches only
```

`NC` (no-create) keeps already-moved files readable through the share while new files land on cache. The catch-all keeps `ls /mnt/user` and stray top-level directories on the array rather than the boot device. Mount ordering is expressed with systemd `RequiresMountsFor=`. If S6 rejects this topology, the fallback and its feature cost are recorded in Q12 and doc 07 R12.

**The mover's branches never follow a symlink (#656).** The mover write target is the one mergerfs mount where root writes on a share user's behalf: the mover and a relocation to the array copy through it. mergerfs carries out every branch operation by path — the create of the temp file, the directories it clones, the rename into place, the unlink of a failed temp, `moveonenospc`'s copy — so on a branch that follows symlinks the kernel resolves whatever a share user has put in that path at that moment, and a directory swapped for a symlink to `/etc` takes the write there. No check in Hoserva can close that race, because the swap happens on the data disk behind mergerfs, and the kernel's FUSE entry cache can keep the old lookup for the whole of `cache.entry`. So each data disk is bind-mounted a second time at `/run/hoserva/branches/mnt/diskN` with `nosymfollow`, and only the mover write targets use those binds as their branches, in the same order and with the same modes and create policy (`pool.MoverTargetMount`). On them the kernel refuses to follow a symlink anywhere in a path (`ELOOP`), while a user's own symlinks still read as symlinks through the mount and the mover can still recreate one. The copy routine's own descriptor-relative walk (`internal/cache` `copyEntry`) stays as the guard for the plain-filesystem routes — a relocation to the cache, a rebalance and an evacuation — and turns either refusal into a failed entry naming the symlink, with the source kept.

- Each bind is its own generated unit, `run-hoserva-branches-mnt-diskN.mount` (`disk.BranchBind`): `What=/mnt/diskN`, `Type=none`, `Options=bind,nosymfollow,nofail`, `BindsTo=`/`After=` the disk's own mount unit — it never mounts the bare mountpoint directory and stops when the disk stops — and the same array-stopped `ConditionPathExists=!` as every other generated mount. `config.WritePoolMounts` writes one for every data disk of the pool, whether or not a mover write target uses it (none with no share, or every share cache-only), and its reconciliation never removes one: the unit is what binds a mounted bind to its disk, and systemd drops `BindsTo=` once the file is gone and it reloads (`hoservad` itself reloads at every start), so a bind left mounted would keep the disk's filesystem mounted past `array stop`. A disk that left the pool keeps its bind unit until the removal job's last step, which unmounts the disk and its bind and then removes both units (`config.RemoveDiskMount`). The read-only pool of a pending migration has no mover write target and gets no bind units.
- Each mover write target's unit lists its binds in `RequiresMountsFor=`, so a start pulls them in. `pool.SystemdMounter` also starts any bind not yet mounted before the target's own unit, and again before it changes the branches of a target that is already live — that is how a disk added to the array reaches a live mover target with its bind up first. A bind that is already up with `nosymfollow`, on its disk's current filesystem (the same device and inode as `/mnt/diskN`), costs no `systemctl` call. One on another filesystem — a disk that has since left its slot, still attached, with another disk now at `/mnt/diskN` — has its unit stopped and started again, so the mover never writes onto that old disk; a bind still off its disk afterwards, or without `nosymfollow`, is refused.
- A disk's bind is unmounted before the disk itself: unmounting `/mnt/diskN` alone would leave its filesystem mounted at the bind, and its device busy. Every disk unmount does this itself rather than relying on `BindsTo=`: `array stop`'s own disk unmount (`disk.MountUnitController.Unmount`) and the removal job's (`disk.SystemdMounter.Unmount`) stop a still-mounted bind's unit first — systemd has a unit for every mounted bind, from the mount table, even with no unit file — and `disk.DirectMounter.Unmount` (the lab) unmounts it. Each then confirms from the mount table that the bind is gone; if it is not, the disk is left mounted and the unmount fails, so `array stop` reports the failure instead of a stopped array whose disk is still mounted read-write.
- In the loop-device lab, with no init system, `pool.Mounter` makes each bind itself with `mount --bind -o nosymfollow` when its source exists, keeps an existing one only while it is still its source's own directory with `nosymfollow` in force, binds it again otherwise (the disk was unmounted or remounted underneath it), and refuses a bind that did not come up with `nosymfollow`.
- The catch-all and the share mounts keep `/mnt/diskN` as their branches; the same branch-side race through them is tracked separately (#684).

### Startup order and a disk missing at boot

A container that starts before `/mnt/user` is mounted writes its data onto the boot device, and the user sees an empty app. Hoserva prevents that structurally (Q69):

- Every data, parity and cache mount is `nofail`, so a dead disk never hangs boot — `nofail` alone does this: it drops the mount from `local-fs.target`'s required ordering, so boot proceeds without ever waiting on the device. `x-systemd.device-timeout=` is not part of this: that option only applies to an `/etc/fstab` entry and is silently ignored in a native `.mount` unit's own `Options=`, so Hoserva's generated units never emit it.
- Every mountpoint directory is made immutable while empty, so a write to an unmounted path fails instead of landing on the boot device. `hoservad` applies `chattr +i` (`disk.EnsureEmptyMountpoint`) to each array slot — data, parity and cache — in three places: once when a job assigns the slot's mountpoint, just before its disk is first mounted there (array create, disk add, disk replace and a parity-disk upgrade all mount through `disk.GuardedMounter`); on every `storageTargetSync.Startup` and `Update` pass over the array's slots, which covers an array created before the guard existed and one a bare-metal restore has just described (the restore mounts nothing itself, so its slots reach the guard through the array rebuild that follows it); and on the array stop/start path, where the array sequence guards each slot right after `array stop` has unmounted its disk and again just before `array start` mounts it. The stop-time guard is what protects a slot that was mounted at every Startup and Update pass (an install upgraded in place without a reboot, which those passes skipped as mounted): a host job that writes to `/mnt/diskN` while the array is stopped for a disk swap hits an immutable directory instead of the boot device. A slot that is already mounted is skipped, and the guard looks only at the mountpoint directory, never at a data disk's contents. A slot whose mountpoint already holds files, or whose filesystem refuses the immutable bit, is left as it was and logged as a warning; it never stops a mount or startup. The pool's catch-all mountpoint, `/mnt/user` (`pool.CatchAllPath`), gets the same treatment in three places: the `storageTargetSync` pass guards it alongside the slots, before that same pass mounts the pool; the array sequence guards it right after `array stop` has unmounted it, and again just before `array start` mounts it. The stop-time guard is what protects an install whose pool stayed mounted through every daemon restart (it lives in its own mount unit, so those passes skipped it as mounted): a container or host job that writes under `/mnt/user` while the array is stopped for a disk swap hits an immutable directory instead of the boot device. Like the slots, a catch-all directory that already holds files is left as it was and logged. The per-share mountpoints (`/mnt/user/<share>`) live inside the catch-all mount and need no guard of their own.
- Samba, NFS, Docker and libvirt start after `hoserva-storage.target`, through managed systemd drop-ins; `hoservad` reaches that target only when every expected disk is present by identity (Q21), or once the user acknowledges the degraded state through `POST /array/degraded/acknowledge` (`hoserva array acknowledge-degraded`, #385) — which calls `disk.StorageGate.Acknowledge` and then runs the same not-ready→ready transition a returning disk reaches (`storageTargetSync.UpdateOrError`), never a second mechanism, refusing instead of reporting success if that transition does not actually start anything (maintenance mode, or a mount failure) — and never while a larger-data-disk upgrade is pending (§4, UR2). The acknowledgement itself survives every later rebuild of the daemon's array sequence (a share change, a disk-topology change, a SIGHUP) for as long as the same disk stays missing, and the array's reported status keeps `arrayDegraded` true the whole time — only a separate `arrayDegradedAcknowledged` field clears. That field alone can still be true while the transition it triggered did not actually start anything (maintenance mode, or a mount failure) — a further `storageServicesReleased` field, read live from the same storage-target gate state (never derived from the acknowledgement), is what the persistent banner and top-bar pill require before showing "acknowledged, running degraded" rather than the plain degraded state with its acknowledge/retry action still offered
- With a disk missing, the pool still mounts from the remaining disks, and the guard's zero-files rule holds (§2)
- A disk present by strong identity (Q21: matched by serial/WWN) but carrying a filesystem UUID SQLite positively read as different from what it recorded for its slot — a replacement disk that kept the original disk's serial (a cloned or reused drive) but was formatted with something else — is not treated as present (#388): `disk.StorageGate.WrongFilesystem()` reports it distinctly from `Missing()`, the gate is not ready for it, and `GET /pool`'s own `PoolDiskEntry.state` reports it `wrong_filesystem`, never `active` — the UI and CLI show the slot needing attention and offer `hoserva disk replace` against it (§4), which is itself allowed to format exactly that disk (`job.ConfirmReplacementTargetAbsent`'s own identity-plus-filesystem exception) rather than refusing it as still present. The web Replace dialog's own device field reflects that same narrow exception: for a `wrong_filesystem` slot it offers that slot's own device as the only replacement choice, pre-filled, rather than the ordinary unassigned-disk list — `ConfirmReplacementTargetAbsent` refuses every other disk for this slot with `slot_disk_present`, so widening the choice would only ever offer a target the server then rejects. An acknowledgement of a degraded array (`POST /array/degraded/acknowledge` above) covers a `wrong_filesystem` slot exactly as it covers a missing one, and survives a rebuild the same way — `acknowledgedDegraded` records both `Missing()` and `WrongFilesystem()` behind an acknowledgement and re-applies whichever of the two a freshly rebuilt gate now reports for the same identity. This is deliberately fail-closed on an *unread* filesystem UUID, not only a matching one: a same-serial disk that is genuinely blank — no filesystem at all, the literal shape a fresh replacement normally has — reads the same as one this build simply could not probe, since `udev`'s own `blkid` builtin exports nothing to its cache on either outcome, and confirming the difference conclusively (`blkid`'s own exit code, distinct for "no signature" versus a real error) means opening the device — which `disk.Provider.List()` must never do (Q13; it is polled by `GET /pool` and re-run on every SIGHUP and topology rebuild). A same-serial disk in that genuinely-blank state still never restart-loops `hoservad` (`startupMountTimeout` below is unconditional).
- That same same-serial-blank disk is still stuck after the fix above, though: `disk.StorageGate` reports it ready (its filesystem UUID was never positively read, so #388's own check finds no mismatch), but `hoservad`'s own bounded attempt to mount that slot's disk — its `.mount` unit's device dependency waiting on a `/dev/disk/by-uuid/<uuid>` symlink that never appears, since the disk has nothing formatted on it at all — still fails or times out, so the gate never actually opens for it either (#398). `storageTargetSync` records that mountpoint's own last bounded-mount failure in memory (never a device probe: it is the outcome of the mount attempt `Startup` or `Update` already made, read back, not re-checked) — `Update`'s own not-ready→ready transition (the live SIGHUP path a disk arriving while `hoservad` is already running reaches, doc 02 §1 Q69) runs the identical bounded array-disk mount `Startup` runs at boot, not only the pool mount, so a same-serial-blank disk that arrives live is recorded exactly the same way one already present at boot is. The record is read through its own lock, never the lock `Startup`/`Update` hold for their whole (bounded) mount attempt, so `GET /pool` never waits behind a live, in-flight mount. `GET /pool`'s `PoolDiskEntry.state` reports the slot `mount_failed`, distinct from both `active` and `wrong_filesystem` — present by identity, but never confirmed to be the array's own live disk. The record self-heals on the next rebuild or restart once the slot's mountpoint is genuinely mounted again (a stat against the live mount table, not a device open), so a slot never stays "needs attention" past whatever actually fixed it. The web Replace dialog offers a `mount_failed` slot's own device as its only replacement choice, exactly as it does for `wrong_filesystem` — and this is where the disk is actually opened: `planDiskReplace`/`replaceDisk`/`RunDiskReplace`'s own re-check calls a one-off, bounded probe (`disk.BlankProber.ProbeBlank`) against exactly that one device, never `List()` or any poll path. That probe runs `blkid -p` first, but its exit 2 is never trusted on its own: `blkid`(8) documents exit 2 as covering both "no signature found" *and* "impossible to gather any information about the device" — a nonexistent path, a permission-denied path, a directory, and a device with unreadable sectors all exit 2 identically to a genuinely blank one (confirmed against util-linux 2.41.5) — so `ProbeBlank` only reports blank once a read-only readback of `dev` itself (its first and last MiB) has also succeeded; any open failure, read error or short read refuses instead. A found filesystem, a found partition table (exit 0), an ambiguous result (exit 8), a readback failure or any other error all refuse — as does a timeout: both the `blkid` exec and the readback run under one bounded deadline, so a genuinely stuck device refuses rather than hanging the request that reached it. The exception applies only when the slot itself carries a recorded filesystem UUID (it was actually formatted before, never a slot mid-setup). Settles the "open, not settled by #388" question above.

**How the boot-ordering gate is installed.** `config.Generator.WriteStorageTarget` (D4) writes `hoserva-storage.target`, `hoserva-storage-ready.service` and one managed drop-in per `pool.DependentServiceUnits` (smbd, nfs-kernel-server, docker, libvirtd) the same way it writes the pool's own mount units — through the generator's manifest and hand-edit protection. `hoserva-storage-ready.service`'s own `ExecStart` is a fixed existence test against a runtime flag, `pool.StorageReadyFlagPath` (`/run/hoserva/storage-ready`) — never a command this generated content itself encodes the readiness bool into. `/run` is tmpfs, so the flag is gone on every reboot, and `cmd/hoservad`'s own `storageTargetSync` additionally clears it before every startup evaluation: a hoservad that never starts, crashes, or never completes a `disk.StorageGate` evaluation this boot leaves the gate closed, with no stale unit content for a refused write or a failed reload to leave behind. `hoservad` only ever creates the flag once that evaluation says ready (never while a larger-data-disk upgrade is pending, §4 UR2). It sends systemd's own `READY=1` once that evaluation has run, whether or not it succeeded: a failed write or `daemon-reload` is logged and leaves the flag cleared — the flag itself is what keeps the gate closed, not the notification — and withholding `READY=1` over it would instead put `hoservad` in a permanent kill-and-restart loop under `Type=notify`'s own `TimeoutStartSec`, taking the API and UI down with it every cycle for no safety gained. That promise depends on `Startup` itself always returning in time: every mount call it can issue (a physical array disk, the catch-all, a per-share mount) is bounded well under `TimeoutStartSec` (`storageTargetSync`'s own `startupMountTimeout`, #388), since a generated `.mount` unit carries no timeout of its own and `systemctl start` against one whose device dependency never resolves — the wrong-filesystem case above, or any other reason a slot cannot actually be mounted — otherwise waits with no bound at all. A mount that times out is treated exactly like one that failed outright: the flag stays cleared, and the next SIGHUP or rebuild retries it.

`packaging/debian/hoserva.service` is `Type=notify`, and `hoserva-storage-ready.service` is ordered `After=hoserva.service`, so a dependent service's own boot-time activation of the readiness unit can never race ahead of `hoservad`'s first evaluation. For that same reason, `hoservad`'s startup call never issues a `systemctl start` or `restart` of a unit ordered `After=hoserva.service` itself — doing so would block on a job systemd defers until `hoserva.service`'s own start job finishes, which only happens once `READY=1` arrives, a genuine deadlock; nothing needs to, since each dependent service's own drop-in (`BindsTo=hoserva-storage.target`) already pulls the target in once systemd starts that service. Startup does start the pool's own mount units (below) — they carry no such ordering, so no deadlock follows. Every later call — a live disk-topology change, a share create, a disk reappearing (below), or an explicit `hoserva array start` — reflects only what actually changed: unchanged readiness and unchanged disk topology issue no `systemctl` call at all, and a not-ready→ready transition starts every *enabled, unmasked* unit in `pool.DependentServiceUnits` directly (smbd, nfs-kernel-server, docker, libvirtd), reusing the same `LoadState`/`UnitFileState` rule `disk.ServiceUnitController` already applies to Samba and NFS in the array stop/start sequence itself — a unit an admin disabled or masked on purpose is never started back up just because the gate opened — never against `hoserva-storage.target` itself, and never a `restart`. `pool.ServiceDropIn`'s `BindsTo=hoserva-storage.target` lives on each dependent's own unit, so starting a dependent pulls the target (and the readiness unit behind it) in as a side effect; the reverse edge does not exist in the generated units, so starting the target on its own leaves an already-inactive dependent exactly where it was — confirmed against a real systemd, not assumed from its documentation alone. A `restart` would propagate through that same `BindsTo=`/`Requires=` and stop and restart every dependent service even while it was already running correctly; `start` on one already active is a no-op, also confirmed directly. A ready→not-ready transition only removes the flag; this gate governs starting, never stopping, a service already up — that is the array-stop sequence's own job (§4). `hoserva array start` (`job.ArraySequence.Start`) reaches this same transition itself, through its own `StorageTarget` hook, once its own disk, catch-all and share mounts are up and confirmed but before it starts Samba or NFS directly: without that, starting a service whose drop-in binds it to `hoserva-storage.target` would hit whatever an earlier, not-ready boot left the flag as, and fail every time — exactly the case a data-disk upgrade's own reboot-then-resume flow (§4 E4–E6) and a degraded boot followed by `array stop`/`array start` both hit.

A disk reappearing while `hoservad` is already running — a reseated cable, or the disk a degraded boot was waiting on — reaches the gate the same way: `packaging/debian/hoserva-storage.rules` fires `systemctl --no-block reload hoserva.service` on a block-device `add` event carrying a filesystem UUID, `ExecReload=` turns that into `hoservad`'s own `SIGHUP`, and `cmd/hoservad`'s handler re-lists disks and re-evaluates the gate — event-driven, reading only udev's own cache, never a data disk's content, so it is not the kind of timer this document's own conventions forbid.

**Disk presence is not the same as the pool being mounted.** `disk.StorageGate.Ready()` only reports every expected disk present by identity (Q21) — a physical disk's own `.mount` unit is `nofail` and mounts itself the moment its device appears, but mergerfs's own catch-all and per-share mounts are not tied to any device and never activate the same way (`RequiresMountsFor=` only pulls a mount's dependencies in when that mount itself starts, never the reverse — the identical one-directional gap `pool.ServiceDropIn` has around `hoserva-storage.target`). Reporting the gate ready from disk identity alone, the moment every disk-topology job or a live not-ready→ready transition confirms it, would let Samba and NFS start against a pool that was never actually remounted — exactly the hazard this whole mechanism exists to close, just moved one layer down. `storageTargetSync.Startup` (an ordinary boot, outside maintenance mode), `Update` (a live transition while `hoservad` is already running, also outside maintenance mode) and `job.ArraySequence.Start`'s own `StorageTarget` hook therefore mount `ArraySequence`'s own catch-all and every share mount (`ArrayMount.Mount`, the same mount-unit path `ArraySequence.Start`/`RefreshLive` already use — never a new mount mechanism) and confirm the catch-all is genuinely a live mount before writing the readiness flag or starting any dependent; a failed mount or an unconfirmed one is treated exactly like the gate itself reporting not ready — no flag, no dependent start, and the next rebuild retries it. `Startup` mounting the pool itself is not optional: the generated pool mount units carry no `[Install]` section (D4), and a physical disk's own `nofail` `.mount` unit activating on its own at boot never pulls the mergerfs layer over it up with it. (A version of this fix that made `Startup` only ever confirm, on the theory that mounting there was itself the hazard, left a share unmounted forever after a real reboot even with the catch-all itself already live — caught by the nightly L3 suite's own reboot step, run 36226407069, which found `/mnt/user/massdel` still unmounted with `smbd`/`nfs-kernel-server` reporting active over it the whole time.) `Startup` skips mounting only while the array is already in maintenance mode when it runs: a hoservad that restarts mid-swap must not remount storage the user explicitly took down. Maintenance mode is persisted in SQLite (#387, D16) the moment `array stop` enters it, and `main.go` restores it — before building the array's stop/start sequence, evaluating this gate, or sending `READY=1` — through `Scheduler.RestorePersistedMaintenance`, so this guard holds across a crash, a restart, or a reboot, not only within the process that entered it. A not-ready→ready transition arriving while the array is in maintenance mode (an explicit `array stop`, §4/Q70) mounts nothing and starts nothing either, for the same reason: an in-progress disk swap is exactly the case where every other expected disk staying present must not be read as "bring the array back" — that transition is left to the explicit `array start` above, once the swap is actually done.

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

**Which `minfreespace` the catch-all uses.** The catch-all at `/mnt/user` uses the array's own `minfreespace`, the one value `createArray` takes (default 50G, `array_settings.min_free_space`); a share's own `minfreespace` (doc 09 §1) applies to that share's mount only, and no setting changes the array's afterwards. The Unraid import records the default for the array it adopts. mergerfs skips a branch with less than `minfreespace` free for every create operation and answers ENOSPC when it skips all of them, even for an empty directory: with every data disk below the floor nothing can be made through `/mnt/user`, whatever a share's own floor says. The point of no return's gate (`PlanParityInit`, doc 05 §4 step 17) therefore first checks, with one `statfs(2)` per data disk (`pool.CheckCreatable`), that some data disk has the floor free, and refuses up front with the largest room found when none has, so that step never fails part-way on it. The check covers that gate only: other places that make a directory through `/mnt/user` (the config backup's destination, share mount points made when the array becomes ready) do not call it. The floor is not lowered to make room, because it is also what keeps parity headroom (§2).

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
- `content` — **content files on at least 3 distinct physical devices, and at least `parity disks + 2` copies** (Q18): the boot device first (`/var/lib/hoserva/snapraid.content`, so `status` polling never touches the array), cache if present, then data disks with the most free space. A cache that is a partition of the boot disk is the same physical device as the boot copy, so it gets no copy of its own and the data disks supply the rest — one parity disk then needs two data disks. The config generator refuses a layout that violates this. Easy to get wrong and catastrophic to get wrong: losing all content files means parity is useless.
- `data d1..dN` — data disks by mount point
- `exclude` — sensible defaults: `/lost+found/`, `/.Trash-*/`, `/appdata/`, `*.unrecoverable`, `/snapraid.content*`, `*.hoserva-moving-*` (in-flight mover copies, doc 09 §2), `.DS_Store`, thumbnail caches, and any share explicitly marked "exclude from parity"
- `blocksize`, `autosave` — defaults, exposed under advanced settings

### Operations

| Op | What it does | Scheduled |
|---|---|---|
| `sync` | Update parity to match current data | Nightly, in the maintenance chain after the mover (Q30) |
| `scrub` | Verify a percentage of existing data against parity | Weekly, after that night's sync; default 8%, older than 10 days. A requested scrub can cover blocks of every age (`hoserva scrub --all-blocks`, SnapRAID `-o 0`): the default skips blocks scrubbed or synced within 10 days, so one right after a sync can find nothing to check |
| `diff` | Report exactly what changed since last sync | Immediately before every sync, and on explicit request — **never on a timer**, because it stats every file and spins up every data disk |
| `touch` | Set non-zero sub-second timestamps | Before a sync, only when `status` reports files needing it (Q17) |
| `status` | Parity age, disk usage, error counts | Polled for the dashboard; reads the boot-device content file |
| `fix` | Restore data from parity. Without a path, a fix covers the whole array, or one disk with `--disk`, and brings back the state of the last sync: a file edited since is reverted and a file deleted on purpose comes back. `hoserva fix --confirm --path /mnt/user/<share>/<file>` restores one file (`-f`) and leaves every other change since the last sync as it is, which is what recovering an accidentally deleted file and the restore drill (doc 05 §4 step 25) need. The path is a canonical absolute path to one file under `/mnt/user`, refused with 400 `invalid_fix_path` otherwise, and when it holds `*`, `?`, `[`, `]` or `\`, which `-f` reads as pattern syntax and which could match more than the one file. A path and a disk are never combined: the request is refused. A fix that leaves unrecoverable blocks ends `failed`, never `succeeded`, whatever it covered: SnapRAID exits 1, and the job's error gives the number of unrecoverable blocks and names the partial copies it left as `<name>.unrecoverable` on their disks (the first ten, then a count), or says SnapRAID leaves them beside the file when its log names none; files it did recover stay restored. Without a path the error also says that SnapRAID can only rebuild what parity held at the last sync and no more failed blocks than parity covers; with a path it names the file and, when the log names the partial copy, gives one example cause: another file that shares its parity positions changed after the last sync. Any other non-zero exit fails the job too. A whole-disk fix inside a disk replacement fails that replacement job the same way, after the new disk is already in the array's place, and `hoserva fix --disk` can be run again. A fix of a path that does not restore the file ends `failed`, never `succeeded`. SnapRAID exits 0 with "Nothing to do" when `-f` matches nothing in parity (a misspelt name, the wrong case, a directory, a file made after the last sync, a file only on the cache) and when the file is intact and needs no restoring; the job's error says nothing was restored and names those causes. The restore drill's evidence is therefore only ever a fix that recovered the file | Manual, guided |
| `check` | Verify without repairing | Manual |

**Owner, group and mode of a restored file.** SnapRAID stores neither ownership nor mode, and a restore does not set them: `fix` creates a file with `open(…, O_CREAT, 0600)` (`cmdline/handle.c` in SnapRAID's source) and the directories above it with `mkdir` mode `0755` (`mkancestor` in `cmdline/support.c`), and neither those two files nor `state.c` or `util.c` contain a `chown` or `chmod` call (read from SnapRAID's master branch on 2026-10-06; the lab's `snapraid` 12.4-1 does the same, a restored file there is `root:root`, mode `0600`, in `root:root` `0755` directories). A file restored as it stands is therefore the daemon's user with mode `0600`, and a directory it had to recreate is `0755`, which locks out the Samba and container users (UID 99, GID 100 on migrated data) that owned them. Hoserva keeps the record SnapRAID lacks. At the end of every sync that changed parity, the one place the disks are already awake, it reads the owner, group and mode of each file `snapraid list` tracks and of each directory above one, and writes them to the `file_metadata` table of the central database (one row per tracked file and directory). Every path is read by descending from the data disk's mount one directory descriptor at a time without following a symlink, so a directory swapped for a link cannot record some other file's owner and mode; a path that is now a symlink or no longer a regular file is left out of the record. That is never a timer, a request or a poll. The walk holds no database transaction: the rows are written in batches of 2000 as a new generation that nothing reads, and one single-row statement then makes it the record, so the longest write transaction a sync holds is one batch, and a sync that fails to read a file, or is cancelled, leaves the previous record complete and in use. A sync with nothing to do, with a record already present, writes nothing. After a `fix`, for each file the log names as recovered:

- only what this fix created is touched. `status:recovered` names a file whether `fix` created it or rewrote it where it stood, so the log cannot say which; Hoserva uses the inode's creation time (`statx` `btime`, which the lab's XFS data disks report): a file or directory is the fix's own when it was created no earlier than one second before the fix started, is owned by the daemon's user and has the mode SnapRAID creates with (`0600` for a file, `0755` for a directory). A file `fix` rewrote in place, a directory that already existed and anything created in the second before the fix started keep the owner and mode they have, recorded or not. On a filesystem that reports no creation time the restore cannot tell them apart, touches nothing and fails the fix naming the path;
- a path that is touched is given the recorded owner, group and mode, for the file and for each directory above it that this fix created;
- a path nothing was recorded for (no sync has run since this record was introduced, or it is newer than the last sync that changed parity) gets the share defaults of Q26: the owner and group of the directory above it, mode `0664` for a file and `2775` for a directory, but only below a share's own top-level directory, which Hoserva's share service owns; a file directly under a data disk's root is left as SnapRAID made it;
- every path is reached from the mount by descriptors without following a symlink, and owner and mode are set through the descriptor, never by path. A directory or file that is a symlink where `fix` created a real one (a share user can swap one while the fix runs) fails the fix naming it; the link and whatever it points to are never opened for a change;
- a path that cannot be given its owner or mode fails the fix, whatever else it reports, with the paths named (the first ten), and the rest still restored. A cancelled fix still gives back what it had already created.

A chown, chmod or ACL change made after the last sync that changed parity is not in the record, so a file restored from before it comes back with the owner and mode it had at that sync, the same last-sync state the rest of `fix` restores. After a configuration-backup import the record is the archive's, since the import replaces `file_metadata` with the other runtime tables, until the next sync that changes parity rewrites it from the disks; a file restored in between gets the archive's owner and mode, or the defaults for a path the archive lacks. Symlinks and hard-linked names are not recorded: a hard link shares its inode's owner and mode, and a symlink's own owner is not one a share user can see.

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

**What "files" counts.** SnapRAID tracks symlinks and hardlinked names apart from regular files: `snapraid status` reports `disk_file_count` for regular files only, while `snapraid diff` reports a symlink or hardlinked name as an added or removed entry exactly like a file. A disk's count is therefore its regular files (status) plus its links (the `link_symlink:` and `link_hardlink:` lines of `snapraid list`, which Hoserva runs only when status's loading header reports any link), and the projection of what the disk holds after the sync is that count plus the diff's additions and copies minus its removals, so a disk that is really emptied projects to exactly zero and a disk that keeps files never does. The removed-percent rule is unchanged: it divides by the array's regular files alone, the sum of `disk_file_count`. A projection below zero, which only a disagreement between those three reads could produce, is treated as an emptied disk.

The thresholds are configurable but cannot be disabled entirely; the minimum is a confirmation prompt. The defaults are revisited with the soak test's diff history at the end of Phase 1 (doc 06 §6) (Q16).

**The guard applies to every sync, whatever triggered it** — the nightly chain, adding a disk, the sync inside an evacuation, or a manual click. A block must raise the notification above on whichever of these paths hit it; today only the nightly chain's diff step sends it, and the other paths are not yet built (#758). The guard judges the diff taken immediately before the sync; the sync's own scan follows it, a window doc 01 §7 accepts.

**Hoserva's own relocations are accounted, not exempted** (Q15). Rebalance, evacuation and share relocation write a manifest of every file they move (relative path, size, mtime, source and target disk). A removal that matches a manifest entry *and* reappears on the recorded target disk in the same diff is shown as its own "moved by Hoserva" group and does not count toward thresholds. Two shapes cannot show that in one diff and are accounted by Q15's rules instead: the trailing sync of a two-phase array-to-array relocation, whose copy an earlier sync already recorded, is accounted once SnapRAID's tracked state confirms the file on its target disk, and an array-to-cache share relocation, whose cache target is not a SnapRAID-tracked disk and so never appears in a diff, is accounted as soon as its source-side removal does. Every other removal counts as before. A relocation's manifest lives only while the job that wrote it can still resume: evacuation and share relocation clear it on every ending they cannot resume from (finished, failed, cancelled, including cancelling an interrupted one), and only if the stored manifest is still their own, so a manifest never outlives its job to exempt later removals at the same paths. A disk in removal (doc 09 §4) is exempt from the zero-files rule, and only that disk is synced with `--force-empty`.

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

*Stop array* (`hoserva array stop`) puts the system in maintenance mode (Q70): new jobs are refused, running resumable jobs stop at their next checkpoint and the rest are marked interrupted, VMs shut down (gracefully, then forced after a timeout), containers stop, Samba and NFS stop, Docker and libvirt stop with them (below), and the per-share mounts, the catch-all and the disks unmount in that order. *Start array* reverses it, and is refused while a larger-data-disk upgrade is pending (see the upgrade's state machine below). System shutdown and reboot run the same sequence. No disk is physically touched outside maintenance mode or a powered-off box.

A user-requested `array stop` persists maintenance mode and, once the sequence completes, whether it has, in SQLite (#387, D16) the instant either changes — held only in `job.Scheduler`'s own memory before this, a crash or a package-upgrade restart of the daemon alone while the array was stopped this way silently returned to normal operation: every job type admitted again, and nothing telling the user the array they believed stopped was live again. `hoservad` restores this state (`Scheduler.RestorePersistedMaintenance`) before it admits any job, builds the array's stop/start sequence, evaluates the storage-target gate (§1), or sends `READY=1` — so a restart after a user `array stop` mounts nothing, starts nothing, and the API/UI report the array as stopped exactly as before the restart.

System shutdown, a reboot (the UI/API *Reboot*) and the UPS low-battery shutdown (Q77) run the identical stop sequence — every job checkpointed or interrupted, every service stopped, the storage-target gate closed, everything unmounted — through `ArraySequence.StopForShutdown`, never `Stop`: none of the three is a user asking the array to stay stopped once the box comes back, so none of them newly persists a stopped state, and none of them creates or clears the durable array-stopped condition flag described below. A plain reboot or a UPS shutdown of a running array therefore comes back up on the next ordinary boot exactly as it was before, not stuck in maintenance mode; a persisted user `array stop` already in force when one of them runs is left exactly as it is — including its own durable flag — and is restored the same way across that restart too.

`array stop` also closes the storage-target gate itself (§1): `hoserva-storage-ready.service` is `RemainAfterExit=yes`, so once it has succeeded it stays "active (exited)" regardless of the runtime flag behind it, and stopping Samba/NFS directly does not touch it. Left alone, anything that later starts Samba or NFS during maintenance — an unattended Debian security update, a hand-run `systemctl` — would still find `hoserva-storage.target` satisfied and serve the unmounted pool straight off the boot disk. `array stop` removes that runtime flag and stops the gate unit, so its fixed `test -e` ExecStart runs — and fails — the next time anything needs it. Docker and libvirt stop at the same point: both read the pool the way Samba and NFS do, but neither was ever in the array sequence's own service list (#309), so this is their only stop. A refusal to stop either one holds the whole sequence up, exactly like a refused Samba or NFS stop, rather than being skipped past on the way to unmounting.

A persisted `array stop` also sets a second, durable flag (`disk.StorageStoppedFlagPath`, under Hoserva's own state directory): every generated mount unit — every physical disk, the catch-all, every per-share mount — carries `ConditionPathExists=!` that flag, because nfs-utils' own systemd integration derives a `RequiresMountsFor=` directly on `nfs-server.service` for every NFS-exported path, an edge entirely outside anything Hoserva itself writes, reachable the instant the box boots and well before `hoservad` is even exec'd. `hoserva-storage.target`'s own `Wants=`/`Requires=` is not enough to stop that. This flag is created by a persisted `array stop` (`Close`, persist=true), by a rollback's own `Close` call, by `Reclose` on either of its own two paths — the pre-`ConfirmReady` failure path and the rollback's own fallback below — and by `Startup`'s own reconcile from the persisted row on every daemon start (below). `Start` clears it (`StorageTarget.Open`) before its own first mount, ahead of persisting the exited-maintenance state — every mount, `ConfirmReady`, every Service and the exited-maintenance write itself can still fail after that. A failure before `ConfirmReady` ever runs — a disk mount, UR9's own disk-identity check, or a catch-all/share mount failure — puts the flag straight back (`StorageTarget.Reclose`) while maintenance mode stays on: nothing above those mount loops has started a service, a dependent, or the storage-target gate itself, so restoring the flag alone is enough. A failure at or after `ConfirmReady` — `ConfirmReady` itself, a Service's own `Start`, or the persisted exited-maintenance write — is different: `ConfirmReady`'s own failure lands before it ever opens the gate or starts Docker/libvirt (it can only fail writing the storage-target units, confirming the pool mounted, or setting the readiness flag, all before it starts a single dependent), so on that failure the gate stays closed and the pool may not even be mounted yet — but by then every mount call `Start`'s own loops made has already returned without error and needs undoing all the same. Once `ConfirmReady` has succeeded, though, a Service's own `Start` failure or the persisted exited-maintenance write failing lands with the gate already open and Docker/libvirt already running, and, once the Services loop is under way, a VM, a container, Samba or NFS may be running too. `Start` never just restores the flag there; it rolls the whole sequence back through `ArraySequence`'s own stop path (`stopSequence`) — the same one `Stop`/`StopForShutdown` use, never a second mechanism — stopping every service it had already started (in stop order, only the ones actually started), calling `StorageTarget.Close(persist=true)` (which restores this same flag and, if every one of its own steps succeeds, also stops Docker/libvirt and closes the gate unit — a failure inside `Close` itself, covered next, leaves the gate open instead), and unmounting the per-share mounts, the catch-all and the disks in stop order. `Start` is reached from an HTTP request (the API's `StartArray` passes its own request context straight through), so this whole rollback runs on `context.WithoutCancel` of that context: a client disconnecting the instant a service's own `Start` fails must never also cancel the `systemctl` calls this rollback makes to undo it. `stopSequence` can fail at any of three points, and `Reclose` restores the flag on its own on every one of them, without waiting on the rest of the sequence: a service (Samba or NFS) that genuinely refuses to stop before `stopSequence`'s own loop ever reaches `Close`; a failure inside `Close` itself — `Close` sets the flag, stops Docker then libvirt, clears the readiness flag, and only then stops the gate unit, so a failure at any of those steps before the last one returns with the gate still open; or an unmount failure once `Close` has already returned successfully. The first two leave the gate open, with whatever was still running underneath it (Docker, libvirt, a VM, a container, Samba or NFS) until an operator resolves it and retries; the third leaves the gate already closed, since `Close` itself already succeeded. In every case the persisted row already says the array is stopped, and the flag it depends on must never disagree with it, the same way a plain `array stop` leaves maintenance mode on and asks for a retry rather than declaring success. `Start` returns the original failure joined with any rollback failure, and a rollback failure says plainly the array could not be returned to stopped, rather than silently reporting only the original error over a host that may still be partly live. `ArraySequence.StopForShutdown` never touches the flag either way. On every daemon start, `hoservad` reconciles this flag from the persisted row before it regenerates every managed disk and pool mount unit from SQLite, reloads systemd, or evaluates the storage-target gate at all: an array or a share created before this flag's condition existed is rewritten with it, and — once the row says the array is not stopped and the storage-target gate itself reports every expected disk present — every physical disk, the catch-all and every share mount is (re)mounted explicitly, rather than assumed to have already come up on its own; if the gate is not yet ready, `Startup` mounts nothing and retries the check on the next rebuild. This is what keeps a boot where the flag happened to be stale from ever leaving a disk or the pool silently unmounted.

### Setting up and extending the array

Array setup (`createArray`) and adding or replacing a disk refuse the Unraid USB stick in every role, array data, parity or cache, and a disk whose filesystem UUID is the stick's (`disk.IsUnraidStick`, the FAT filesystem labelled `UNRAID`): it is the user's rollback (doc 05 §4 step 11, §5), and the typed confirmation alone would stand between it and `mkfs`. The check is `resolveFormatTarget`'s, which every path that formats, adopts or checks a disk goes through, and the API answers it with `unraid_stick` (409) before anything is queued. The Unraid import's adoption refuses it as well, in each role (doc 05 §4).

An *adoption* is not an array setup: it never formats, mounts a disk read-write or writes a parity disk's signature. It has its own plan, `disk.AdoptionPlan` beside `disk.TopologyPlan`, because `TopologyPlan` formats its parity disks at once and mounts everything read-write. A parity disk it records is neither formatted nor mounted, and an adopted data disk is mounted by its own device, never by filesystem UUID alone (doc 05 §4). While the array is *migration pending* its disk units are `ro` (with `norecovery` for XFS, `noload` for ext4 and `rescue=nologreplay` for btrfs), the catch-all's branches are `RO` and the mount itself `ro`, and no `snapraid.conf` is generated.

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

Two jobs. The evacuation (doc 09 §4 steps 1–6) is long-running, interruptible and resumable on user action (Q29), with progress in files and bytes. Finishing the removal (steps 7–9) is a separate, short job that is re-run rather than resumed: each run carries on from the last step the disk's persisted removal state records.

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
| None | *Stopped* | Unwind. Confirm A and B by their by-id identities, and that slot N is not itself in removal (doc 09 §4) — an evacuation queued ahead of this upgrade can mark it after the plan was confirmed, and `store.ReplaceDataDisk` never touches `removal_state`, so B would otherwise silently inherit A's removing/removed state the moment it is adopted (#368). Unlike replace ("Replacing a failed disk" below, #384), this refusal is unconditional here: an upgrade never abandons a removal, whatever state it is in. Establish(*Old*). `snapraid diff` must report nothing to sync | *Old* | A | Formatting |
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

When the slot's old disk is present by identity — a same-serial replacement, most commonly a genuinely blank one (`mount_failed`, §1) or one already carrying a positively different filesystem (`wrong_filesystem`, #388) — `ConfirmReplacementTargetAbsent` refuses every device but that slot's own, and allows that one only through a narrow exception: `wrong_filesystem` needs nothing further (the difference was already positively read), while `mount_failed` needs step 3's own device to first pass a one-off, bounded probe (`disk.BlankProber.ProbeBlank` against that exact device — never `List()`, never any poll path, #398) that positively confirms no filesystem or partition-table signature at all. That probe never trusts `blkid -p`'s own exit 2 alone — it also covers a device the tool simply could not read, not only a genuinely blank one (§1) — so it only reports blank once a read-only readback of the device itself has also succeeded; a found signature, an ambiguous result, a readback failure or a timeout on either step all refuse, same as if the slot's old disk were simply still there.

Before formatting, the job refuses if the slot is still `evacuating` or already `unlisted` (doc 09 §4) — an evacuation still running is not replace's job, and a disk already dropped from `snapraid.conf` has nothing left for `snapraid fix` to rebuild against. `evacuated` and `unpooled` are not refused on the removal state alone: the evacuation has already succeeded, and the disk can since have died — its own loop device detached, in the lab — with SnapRAID still recording a file the evacuation's own post-check never inspected (one outside every share, doc 09 §4 step 6, #369). Rebuilding that file onto a fresh disk is the only path that keeps parity's last copy, so replace is allowed there once the old disk is genuinely missing: nothing mounted at its mountpoint, and nothing in a fresh inventory still carrying its identity — the same check (`ConfirmReplacementTargetAbsent`) that already refuses replacing a disk that has not actually failed. `store.ReplaceDataDiskAbandoningRemoval` clears `removal_state`/`removal_job_id` in the same statement that adopts the replacement's identity, so the slot rejoins the pool as an ordinary disk and every pool mount is regenerated create-eligible again; a failure before that statement leaves the old removal state intact (#384). Upgrade never takes this path — a removal-state slot stays refused there (`disk_leaving_array`) unconditionally, since `store.ReplaceDataDisk` alone does not touch `removal_state`, so a plain replacement or upgrade would otherwise silently inherit the old disk's removing/removed state the moment it is adopted (#368).

**Honest constraint that must be surfaced in the UI:** reconstruction can only restore data that was present at the last successful sync. Files written after it are gone. The UI shows the last sync time and the files on that disk that were pending at failure — counted and, from the change journal (§2), named — so the user knows exactly what they lost instead of discovering it months later.

### SMART monitoring

- Poll all disks on a schedule (default 15 min; does not wake sleeping disks — use `smartctl -n standby`)
- Track: reallocated sectors, pending sectors, offline uncorrectable, CRC errors, temperature, power-on hours
- Alert on: any increase in reallocated/pending, temperature above threshold, self-test failure
- Scheduled short self-tests weekly, long self-tests monthly, configurable
- **Trend, not just current value** — reallocated sector count going from 0 to 4 is the signal; the absolute number is not
- History is kept in a separate, downsampled `metrics.db` (Q74)

### Cache on a spare partition of the boot disk

Array disks are whole disks that are not the boot disk, and Hoserva never formats a partition — with one exception: the **cache** slot may be a spare partition of the boot disk, for the partitioned-NVMe layout doc 01 §6 calls acceptable. The user creates that partition in the Debian installer; Hoserva formats that existing, unmounted, blank partition and nothing else, and never edits the boot disk's partition table. Data and parity slots stay whole, non-boot disks.

- **Candidates.** `Provider.List` reports a boot disk's partitions that qualify (doc 01 §6) without opening the device, each with its identity (the `…-partN` by-id link and PARTUUID), size and why it qualifies. The signature probe (`ProbeBlank`) runs only when the user picks one.
- **Assignment.** The cache role accepts a listed candidate; data and parity refuse any partition of the boot disk (`boot_partition_cache_only`), and a boot-disk partition that is not a candidate — the root, EFI, swap, or a partition that stopped qualifying — is refused as an unmanaged device. Adopting an existing filesystem on such a partition is refused.
- **Format.** Before any disk in the plan is formatted, and again immediately before the partition's `mkfs`, the partition's identity is re-resolved against a fresh `List` (same by-id name and PARTUUID, still a candidate) and `ProbeBlank` must positively find no signature; a missing probe or probe error refuses. Only then is that one by-id path formatted; the boot-device guard stays in force for the whole disk and every other partition.
- **Identity.** The cache row stores the parent disk's WWN or serial and the partition's by-id name, so the storage-target gate matches the boot disk, which is always present; the recorded filesystem UUID is not compared against the boot disk's own cached filesystem, and array start still confirms the mounted partition's UUID.
- **Content files (Q18).** The cache shares the boot copy's device and gets no content file of its own.

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
