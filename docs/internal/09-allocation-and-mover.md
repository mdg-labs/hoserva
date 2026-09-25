# Hoserva — Space Allocation and the Mover

## The short answer to "does a dependency handle this?"

**Partly, and the split matters.**

mergerfs handles **placement at create time** and nothing else. The mergerfs author is explicit: mergerfs does no automatic moving of files; it does placement at create time.

So:

| Concern | Owner |
|---|---|
| Which disk a *new* file is created on | **mergerfs** (create policy) |
| Falling back when the chosen disk is full mid-write | **mergerfs** (`moveonenospc`) |
| Reserving headroom per disk | **mergerfs** (`minfreespace`) |
| Moving files from cache to array | **Hoserva** — nothing else does this |
| Rebalancing an unevenly filled pool | **Hoserva** |
| Evacuating a disk before removal | **Hoserva** |
| Deciding *when* any of the above runs | **Hoserva** |
| Not corrupting open files while moving | **Hoserva** |

**The mover is entirely ours to build.** This is one of the two biggest pieces of original engineering in the project (the other is the parity scheduling and threshold guard). It is also where a bug loses data, so it gets disproportionate design and test attention.

---

## 1. What mergerfs gives us

### Create policies

The relevant ones, in plain language (UI labels and defaults in doc 02 §1, Q11):

| Policy | Behaviour | Good for |
|---|---|---|
| `mfs` | Most free space | Balanced pool, even wear |
| `lfs` | Least free space that still fits — fills one disk at a time | Quiet disks, maximum spindown |
| `ff` | First branch with room, in branch order | Unraid *Fill-up* imports |
| `epmfs` | Existing path only, then most free space among those | Keeping a library on one disk — fails with ENOSPC when those disks are full |
| `mspmfs` | Like `epmfs`, but walks up to the parent path when no branch with the full path has room | **Default** — folder locality without the hard ENOSPC (confirmed in spike S6) |

### `moveonenospc`

When a write fills the target disk mid-file, mergerfs relocates the in-flight file to another branch and continues. This prevents the most annoying failure mode (a 60 GB file failing at 58 GB) but it is *reactive* — it fires after the disk is already full.

### `minfreespace`

Reserved headroom per branch. Disks below it are excluded from create policies.

**Known sharp edge:** with path-preserving policies, hitting `minfreespace` on the branch holding the existing path can produce ENOSPC rather than a clean fallback — users report that when the disk containing the path fills or hits `minfreespace`, writes fail with "No space left on device" instead of falling back to another disk as expected.

This is why the default is `mspmfs` rather than `epmfs` (Q11). Whichever policy a share uses, Hoserva must:
- Warn in the setup wizard and share settings when a strictly path-preserving policy (`ep*`) is chosen, explaining this behaviour
- Alert on per-disk free space, not just pool-wide free space — a pool that is 40% free can still refuse writes
- Offer rebalancing as the remedy, surfaced at the moment the condition is detected rather than in a settings page

---

## 2. The mover

### Purpose

Relocate files from the cache to the array on a schedule, so the cache stays fast and the array holds the long-term data. Same concept as Unraid's mover, and it has the same failure modes, which are well documented and worth learning from rather than rediscovering.

### Per-share modes (from doc 02 §3)

| Mode | Mover behaviour |
|---|---|
| Cache then move | Files written to cache, relocated to array on the next run |
| Cache only | Never moved. Appdata, databases. **Not parity-protected.** |
| Array only | Never on cache; mover ignores |

### Algorithm

```
for each share with mode = cache-then-move:
  enumerate files under /mnt/cache/<share>
  for each file:
    skip if open by any process — including mergerfs itself (see below)
    skip if modified within grace period (default 5 min)
    skip if in an excluded pattern
    pre-check the array has room (size + minfreespace) on at least one eligible disk
    copy to /run/hoserva/array/<share>/<path>.hoserva-moving-<uuid>
      — through the share's array-only mergerfs mount, so mergerfs picks the disk
      — preserving mode, ownership, xattrs, ACLs, timestamps
    verify (size always; checksum optionally)
    fsync target
    rename to the final name (same branch, atomic)
    re-check open handles on the source
    unlink source
    checkpoint + record in the job log
```

**Why "including mergerfs".** Clients reach cache files through `/mnt/user/<share>`, so a file a container or SMB client has open shows up in `fuser`/`lsof` as held by the **mergerfs process**, not by the client. An open-handle check that ignores mergerfs's own file descriptors would see nothing open and move a file mid-write.

### Design requirements, each learned from a way this goes wrong elsewhere

**Never move an open file.** Check open handles before touching anything, and re-check immediately before the unlink. A container writing to a file that gets moved out from under it produces corruption that surfaces days later.

**Copy-verify-delete, never move-and-hope.** The source is unlinked only after the target is written, verified, and fsynced. An interrupted mover must leave a duplicate, never a gap.

**Idempotent and resumable.** Power loss mid-run leaves a partial copy; the job is marked interrupted, and on resume — or the mover's next scheduled run (Q29) — it detects and completes or discards it. Partial copies get a temp suffix (`<name>.hoserva-moving-<uuid>`) so they are never mistaken for real files: excluded from SnapRAID (doc 02 §2) and hidden from share browsing. They *are* visible through the union until renamed, which is why the suffix exists.

**Respect the create policy — by construction.** The mover writes through `/run/hoserva/array/<share>`, an array-only mergerfs mount with the share's own create policy (doc 02 §1). mergerfs places the file exactly as it would have placed a direct write, because it *is* mergerfs placing it. Two placement algorithms in one system is a bug factory; this design has one.

**Order matters: mover before sync.** The nightly maintenance chain runs the mover first and starts the sync only when it finishes (Q30). Files moved after the nightly sync would sit unprotected for 24 hours.

**Rate limiting.** Default to a throughput cap and low IO priority (`ionice -c3`) so a nightly mover run does not make the server unusable. Configurable, with an "as fast as possible" option for people who want it.

**Free-space awareness.** Check the target has room *including* `minfreespace` before starting each file, not just at the start of the run. Re-check as the run progresses — the pool is a moving target.

**Honest reporting.** Every run logs: files moved, bytes, duration, files skipped and why. "Skipped 14 files because they were open" is information the user needs; silent skipping is how people discover a year later that their cache never empties.

### Triggers

- Scheduled (default: first step of the nightly maintenance chain, 02:00)
- Threshold (default: cache above 70%)
- Manual, from the UI or `hoserva mover run`
- **Not** continuous. A daemon watching for writes and moving them immediately defeats the purpose of a write cache.

### Share relocation

A one-shot move of a whole share between cache and array, using the same machinery: when a share's cache mode changes (doc 03 §4.2), and in migration Phase D step 18, where the `appdata` share moves back onto the cache (doc 05 §4). `hoserva share relocate <share> --to cache|array`.

- **Array → cache:** copy from the array branches to `/mnt/cache/<share>`, verify, then delete the array originals. Deleting from a data disk weakens recovery of other disks until the next sync, exactly as in §3 — so array-side deletes follow the two-phase order (Q14) and a sync runs before the originals are removed.
- **Cache → array:** identical to a mover run restricted to one share, ignoring the grace period.
- Containers using the share are listed before starting, with an offer to stop them — relocating a live database is the same hazard as moving an open file.

### VM disk relocation (Phase 3.5, doc 14 §2, Q56)

Same machinery again, for one VM's vdisk between cache and array. `hoserva vm relocate-disk <name> --to cache|array`, or from the VM's cache-mode picker in the UI.

- **The VM must already be stopped.** A vdisk is one large file a running QEMU process holds open for the VM's entire lifetime — copying it out from under a live domain is the open-file hazard above, except here the "process" is the VM itself, not a client. Hoserva refuses to start the relocation while the domain is running, rather than trying to detect the open handle mid-copy.
- **Array-involved moves are two-phase** (Q14), same as rebalance and evacuation: copy and verify to the target, sync parity, then delete the source. Cache-only moves (both ends outside parity) are single-phase copy-verify-delete, same as a normal mover run.
- The VM stays stopped for the whole job. The UI states this plainly rather than leaving the user to wonder why *Start* is greyed out mid-relocation.

---

## 3. Rebalancing

Distinct from the mover: redistributes files *between array disks* to even out fill levels.

**When it is needed:**
- After adding a disk (new disk is empty, old ones are full — `mfs` will fix this over time, but only for new writes)
- After the ENOSPC-on-path-preserving-policy situation above
- When one disk is near `minfreespace` while others are half empty

**How it works:** the mover's copy-verify machinery, with a target-selection function that picks the least-full disk instead of following the create policy — but **two-phase, not copy-verify-delete** (Q14):

1. Copy and verify every file in the plan to its target disk; record each in the relocation manifest
2. Sync parity, through the threshold guard (doc 02 §2, Q15) — the copies are now protected
3. Delete the sources
4. Sync parity again

**Why not delete immediately, as the mover does.** SnapRAID reconstructs one disk from parity plus the *other* data disks as they were at the last sync. A file deleted from disk A before the next sync takes blocks that recovering disk B would need. Cache files are outside parity, so the mover can delete right away; array-to-array moves cannot. Runs as a cancellable, checkpointed job with progress; resumable on user action (Q29).

**Path-preserving caveat:** under `epmfs`, moving a file to a disk that lacks the parent path changes future placement behaviour for that directory. The UI should surface this: *"Rebalancing will spread `media/tv` across more disks, which reduces the benefit of keeping folders together."*

**Not automatic.** Rebalancing moves potentially terabytes and hammers the disks. It is offered when the condition is detected, with a clear explanation, and the user decides.

---

## 4. Disk evacuation

Removing a disk from the pool. Mechanically a rebalance targeting one specific source disk, with stricter requirements:

1. **Pre-check:** remaining disks must fit everything, with `minfreespace` respected on each
2. Put the disk in `removing` state: set its branches to no-create (`NC`) in every mount, so nothing new lands on it; the zero-files guard rule is suspended for this disk only (Q15). The state is persisted with the job that set it, so the generated units keep `NC` across a restart and a resume re-applies it to the running pool. If the running pool cannot be switched, the job fails before copying anything. Cancelling that job clears the state; a failed job leaves it, until the disk is evacuated again or that run is cancelled. Only one evacuation is pending at a time
3. Enumerate everything on the source, record total count and bytes
4. Copy and verify each file to the remaining disks; record each in the relocation manifest
5. Sync parity through the threshold guard — the copies are protected before anything is deleted (Q14)
6. Delete the sources; **post-check:** source disk contains nothing but empty directories
7. Remove from every mergerfs branch list, remount
8. Remove from the SnapRAID data list and sync with `--force-empty` for that disk only
9. Only then: unmount and report the disk as safe to physically remove

Interruptible, checkpointed and resumable. This can run for a day on a full 8 TB disk. After a daemon restart it is marked interrupted, and resumes from its checkpoint — not from zero — when the user resumes it (Q29).

---

## 5. Free space accounting — a UX trap worth naming

`df` on a mergerfs mount reports pool-wide free space. **That number is misleading and it will generate support requests.**

A pool with 3 TB free across six disks cannot accept a 4 TB file. Neither can a pool with 3 TB free where 2.9 TB of it is on one disk and the create policy is path-preserving toward a different disk.

Hoserva must therefore show, everywhere space is displayed:

- Pool total free
- **Largest single-disk free space** — the real answer to "what is the biggest file I can write"
- Per-disk breakdown, with any disk near `minfreespace` highlighted

And alert on *per-disk* thresholds, not only the pool total. A pool at 60% capacity with one disk at 99% is in a worse state than a pool at 85% evenly distributed, and only the per-disk view shows it.

`cache.statfs` tuning affects how often this is recomputed; the default should favour accuracy over the microscopic performance gain of stale numbers.

---

## 6. Test coverage (extends doc 06)

Every one of these runs on the loop-device harness:

- Mover skips a file held open by a running process — **both directly on `/mnt/cache` and through the `/mnt/user/<share>` mount**, where the holder is mergerfs
- Mover interrupted (`SIGKILL`) mid-copy leaves a duplicate, never a gap; the resumed run cleans up
- Mover respects the create policy — files land where mergerfs would have put them (it writes through mergerfs)
- Mover refuses to start when the target has insufficient space
- Mover preserves ownership, permissions, xattrs, and timestamps exactly
- `*.hoserva-moving-*` temp files never appear in a SnapRAID diff
- Rebalance evens out a deliberately skewed pool
- **Rebalance and evacuation never delete a source before the sync that covers its copy** — kill the job between copy and sync, fail a *different* data disk, and assert full reconstruction (Q14)
- **Evacuation does not trip the threshold guard**, and an unrelated mass deletion during the same window still does (Q15)
- Evacuation empties a disk completely and the pool remounts cleanly without it
- Share relocation array → cache moves the share and follows the two-phase order on the array side
- VM disk relocation refuses to start while the domain is running, and array-involved moves follow the two-phase order (Phase 3.5)
- ENOSPC-under-path-preserving-policy is detected and surfaced as a rebalance suggestion; `mspmfs` falls back to the parent path instead (spike S6)
- Free-space reporting distinguishes pool-free from largest-disk-free
- Mover ordering: the nightly chain starts the sync only after the mover finishes, however long it runs

---

## Sources

The mergerfs claims above originally carried citation markers without URLs; the markers have been removed. Primary source: the mergerfs documentation and project discussions at `github.com/trapexit/mergerfs` (create policies, `moveonenospc`, `minfreespace`, branch modes, the author's statement that mergerfs only places at create time, and the reported `ep*` ENOSPC behaviour). SnapRAID behaviour (reconstruction from other disks' last-synced blocks, `--force-empty`) per the SnapRAID manual, `snapraid.it/manual`.
