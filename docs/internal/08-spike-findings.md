# Hoserva — Spike Findings

Research answers to the two kill-criteria questions from doc 07 §1. Both are answered from public sources; both still need agent-run confirmation in the lab (doc 06 §5, §6), but the plan does not need to wait on that.

---

## Spike 1 — Spindown under mergerfs

**Verdict: the concern is real, but it is not specific to Hoserva. It is a property of union filesystems in general. Proceed, with mitigations — and the acceptance test in doc 06 §6 is now agent-run confirmed (below), not just designed.**

### What mergerfs upstream says

The mergerfs author maintains a wiki page on exactly this, and the answer is blunt: limiting drive spinup through mergerfs alone is not possible, because mergerfs is a proxy rather than a cache — it translates client calls across N filesystems, so a `readdir` runs against all branches and aggregates, and an `open` must scan across branches to find and select the file. Doing otherwise would require a significant rewrite. The recommended approach is to limit spinup at the client level — keeping software from touching the filesystem in the first place.

Confirmed elsewhere by the same author: mergerfs stores nothing of its own on the drives and does not query them on its own; the access pattern depends entirely on what is requesting data and the policy of that request. The OS caches some information, but when the OS asks mergerfs for data it has no choice but to pass the request through. The kernel already caches, and the user controls which parts of mergerfs the kernel may cache; mergerfs does no explicit caching of its own, because keeping caches aligned is non-trivial when mergerfs does not own the underlying data.

A long-standing feature request to have mergerfs maintain a file-index so a read could target a single disk exists — motivated by exactly the Plex/Kodi case, where the application only wants one file and the whole pool gets woken — and it has not been implemented, for the reasons above. The author's position is that something must track where files live, users change things out of band and expect that to work, and any interaction with a drive that the OS has not cached means spinup.

### Why this does not kill the project

**Other union-filesystem NAS systems have the same property.** Unraid's user-share layer (`shfs`), for example, is also a FUSE union over per-disk filesystems, and "my disks won't stay spun down" is a long-running topic in its community forums — from browsing a share spinning up all disks, through reads landing on every array disk every few seconds with no open files, persisting after stopping VMs, containers, and the Docker service, to disks refusing to stay down for more than a few minutes after a manual spindown.

So the honest framing is: **this is not a Hoserva-specific regression. It is a property union-filesystem NAS setups share, and the difference is in how much help the tool gives you.** That reframes R1 from a kill criterion to a feature opportunity.

Practical reports are also more positive than the theory suggests. One user with a mixed 2×16 TB / 2×4 TB mergerfs pool reports that with a reasonable Linux setup and enough RAM (32 GB in their case), the kernel's filesystem cache keeps the other drives from spinning up frequently. Kernel dentry and attribute caching does most of the work in practice, provided nothing is actively walking the tree.

### Implications for the spec

Three changes, none structural:

**1. Create policy default — reconsider.** `epmfs` remains a good general default, but for spindown specifically, a fill-one-disk-at-a-time policy is better: `lfs` (least free space) writes to the branch with the least free space that still has room, filling disks one at a time, which is useful when you want most drives spun down most of the time, at the cost of no balancing. Note the `epmfs` fallback caveat — users report that when the disk holding an existing path hits `minfreespace`, writes fail with ENOSPC rather than falling back to another disk, which the setup wizard must account for when choosing defaults.

Recommendation: keep folder-locality as the default for balance and blast-radius reasons, but ship a **"Quiet mode" preset** in the wizard that sets `lfs` plus tuned cache options, with the tradeoff stated plainly. *(Carried forward as Q11: the default is `mspmfs`, which keeps `epmfs`'s locality but walks up the path instead of failing with ENOSPC — to be confirmed on the pinned mergerfs build in spike S6.)*

**2. Cache the kernel can use.** mergerfs caching options (`cache.files`, `cache.entry`, `cache.attr`, `cache.negative_entry`, `cache.statfs`) are the actual lever. Longer entry and attribute TTLs mean fewer passthrough lookups and fewer wakeups, at the cost of staleness when things change out of band. Hoserva should expose this as a single plain-language setting — *Responsiveness vs. quiet disks* — rather than five opaque TTLs.

**3. The real differentiator: tell the user what woke the disk.** The recurring theme in these community threads is that users cannot find the culprit. Hoserva can make that visible, which few tools do today:

- Track spin-state transitions per disk with timestamps
- Correlate each wake against process-level IO (`fanotify`, or `/proc` IO accounting sampled around the event) to attribute it to a process and, where possible, a container
- Surface it as a **"What woke my disks"** view: disk, time, duration awake, most likely cause
- Common culprits to detect and name explicitly: Plex/Jellyfin library scans, \*arr rescans, the SMART polling loop itself, Samba `recycle`/`vfs` modules, Docker log drivers writing to array paths, `updatedb`/`mlocate` indexing the pool, file indexers on connected Windows clients

**`updatedb` is worth a special mention** — it walks the entire pool nightly and wakes every disk, it is installed by default on Debian, and it is a classic self-inflicted wound. Hoserva should add `/mnt/user` and `/mnt/disk*` to `PRUNEPATHS` automatically on install.

**4. Keep the acceptance test, adjust the target.** Doc 06's criterion (30 minutes standby with shares idle and appdata on cache) stays, but "idle" must be defined as no Samba clients connected and no containers touching pool paths. Measure it, publish the result, and be honest about what breaks it. *(Adopted as Q31; docs 02 and 06 updated.)*

**5. Hoserva must not be its own culprit.** Beyond SMART polling, anything Hoserva runs on a timer that walks the pool defeats all of the above. The one that was hiding in the spec: a live "files changed since last sync" count implies a polled `snapraid diff`, which stats every file. Replaced by a fanotify change journal (Q13, spike S7).

### Remaining hands-on work

- ~~Measure IO reaching array disks under a realistic container load, with the zero-IO proxy in the lab (doc 06 §6)~~ — done, agent-run in the lab, 2026-09-15 (below)
- Quantify what the cache TTL settings actually buy — partially answered below: no measurable difference found across the scenarios this run covers; a workload that actually re-walks the pool tree periodically (directory browsing, an indexer, `updatedb`) is needed to see the TTL setting bite, and none of this run's three scenarios does that
- Verify in L3 that SMART polling causes no read IO on an idle disk; firmware behaviour across controllers is residual risk for the public beta (doc 06 §6)
- Build and validate the wake-attribution mechanism for the product itself (Q32, Phase 4) — this run validates `fatrace` as the *measurement* tool for further spikes and the lab/L3 test suite, which is a different, smaller claim than a shipped "what woke my disks" feature

### Agent-run confirmation, 2026-09-15 (S1 acceptance test, doc 06 §6, doc 07 §1)

**Verdict: confirmed, with no exceptions.** Six 30-minute windows — the three scenarios named in issue #2, each at both a short (1 s) and a long (600 s) mergerfs `cache.entry`/`cache.attr`/`cache.negative_entry` setting — every one showed **zero** movement in any of the 17 `/sys/block/<dev>/stat` fields on any array disk, and **zero** `fatrace` events on any array disk, with working, verified attribution throughout (below).

A first attempt at this confirmation (kept in git history, not reproduced here) found one window with a small, non-zero array-disk delta and explained it in prose, but got its own supporting arithmetic wrong: it named `read_sectors`/`read_ticks`/`io_ticks` as the fields that moved, parsed by eye against a `snapshot.sh` comment that listed only 11 of this kernel's 17 `/sys/block/<dev>/stat` fields. Re-parsing the same committed raw snapshot lines field-by-field (now done by `spikes/s1/scripts/stat-delta.sh`, never by eye) showed the actual delta was entirely in `write_ios` (+8), `write_merges` (+1), `write_sectors` (+59 sectors), and `flush_ios` (+6) — a write-side artifact, not a read, and not `io_ticks`. This run replaces that one: it adds a settle gate ahead of the acceptance matrix, characterizes the write-side artifact directly instead of asserting an explanation, and re-runs the full matrix once, end to end, from the corrected and extended scripts — producing every result below in one pass.

**Setup.** Loop-device lab (doc 06 §3), lab id `2-a1`: 1 parity + 3 data disks (1 G XFS each) + 1 cache disk (512 M XFS), mergerfs pool over the 3 data disks. Mount options matched doc 02 §1's table: `category.create=mspmfs,moveonenospc=true,minfreespace=50M,dropcacheonclose=true,cache.files=partial,cache.statfs=0`, plus `cache.entry`/`cache.attr`/`cache.negative_entry` at `1` ("default") or `600` ("raised") seconds. Packages added to the lab container for this spike only — `attr 1:2.5.2-3`, `fatrace 0.18.0-1`, `samba 2:4.22.11+dfsg-0+deb13u1`, `smbclient 2:4.22.11+dfsg-0+deb13u1` (full setup in `spikes/s1/README.md`) — are not part of the standing lab image; a spike is allowed to do this in its own disposable container (`CLAUDE.md`, "Spikes").

**Scenarios**, each run for the full 1800 s, one after another (a single 3-disk array can't serve two scenarios at once without contaminating both scenarios' disk counters):

- **idle** — nothing touches the pool.
- **appdata** — a proxy loop writes one 4 KiB file every 5 s directly to the cache disk's `appdata` directory, standing in for a container's cache-only appdata bind mount (doc 02 §1: cache-only shares have no array branches at all, so this workload never enters the pool's FUSE layer by construction, not just by observation).
- **smb** — the appdata proxy, plus Samba (`smbd`) exporting the pool as a guest share, plus one `smbclient` session held open and genuinely idle (stdin fed from a FIFO, no command sent until `quit` at the very end) for the full window.

**Attribution.** A first attempt used one whole-mount-namespace `fatrace` instance per window, filtered afterward to paths under each array disk's mountpoint. It could not work: this lab container's capability set (`docker-compose.dev.yml` grants `SYS_ADMIN` only) lacks `CAP_DAC_READ_SEARCH`, which `open_by_handle_at(2)` needs to resolve a fanotify file handle back to a path, so every event's path printed as `(deleted)` regardless of what happened — a path-based filter can never match in this lab, confirmed producing zero true positives. The working mechanism instead runs **one `fatrace -c` ("current mount") instance per array disk**, each launched with its working directory on that disk's own mountpoint before `fatrace` starts. `-c` scopes the fanotify mark to the filesystem of the current directory (`FAN_MARK_FILESYSTEM`), so any event a disk's own tracer logs is IO on that disk by construction, attributed by PID and process name (which `fatrace` reports without needing to resolve a path) — no path resolution required.

**Positive control**, run once before the six-window matrix and gating it (`spikes/s1/scripts/positive-control.sh`, `spikes/s1/results/positive-control.log`), now covering both halves of the mechanism:

1. **Direct-disk probe**: a probe file is written, read and deleted directly under `disk2`'s real mountpoint, with all three per-disk tracers running. The probe's four events (`bash`/`cat`, open/write/read/close) appear only in `disk2`'s log; `disk1`'s and `disk3`'s logs are empty for the same window — proving a tracer sees real IO on its own disk and does not see IO on another disk (no cross-contamination between the three `FAN_MARK_FILESYSTEM` scopes).
2. **Pool-routed probe**: a probe file is written and read through the mergerfs pool mount (`$LAB/mnt/user`), not a disk's own mountpoint. mergerfs's `mspmfs` create policy placed it on `disk3` this run; `disk3`'s tracer recorded all six events (open/write/close-write/open/read/close), attributed to the `mergerfs` process by name, and `disk1`'s and `disk2`'s logs stayed empty. A previous attempt's positive control only covered case 1; an independent verifier check in a separate lab confirmed case 2 held, but the recorded evidence didn't demonstrate it — this run's positive control now does.

`run-window.sh` also clears each disk's log/err file before starting its tracer and checks the tracer's exit status (not merely whether it printed anything to stderr — this lab's inability to resolve paths makes `open_by_handle_at: Operation not permitted` an expected, harmless line on every logged event); a tracer that exits non-zero fails the window loudly rather than reporting a silent `(none)`.

**Settle gate**, run once after the positive control and before the six-window matrix (`spikes/s1/scripts/settle-gate.sh`, `spikes/s1/results/settle-gate.log`): an explicit `sync`, then disk1-disk3's `/sys/block/<dev>/stat` sampled every 5 s until every array disk has been unchanged for 180 s (3 minutes), or the gate fails after 900 s. This exists because the positive control's own probe writes (above) leave `disk2` and `disk3` with pending writeback that does not appear immediately, even after `sync` — the gate makes the acceptance matrix start only once that has genuinely finished, rather than assuming it has. This run's gate needed 230 s total: two further small write-side deltas on `disk2` and `disk3` (`write_ios` +2, `write_sectors` +3, `flush_ios` +2 each, roughly 30 s apart — T+20s and T+50s after the gate's own `sync`) before going fully quiet for the required 180 s. No array disk moved again for the rest of the ~3-hour run (below) — the gate's 180 s requirement, chosen before this run as "at least 3 minutes" per issue #2, turned out to be roughly 3.5x the margin this run actually needed.

**Trailing-write characterization**, run once before the positive control, on `disk1` alone, with no other array disk yet touched (`spikes/s1/scripts/trailing-write-characterization.sh`, `spikes/s1/results/trailing-write-characterization.log`): after one `sync` for a clean baseline, a create+write+unlink probe directly on `disk1`'s own mountpoint, then `disk1`'s stat line sampled every 5 s for 6 minutes with **no further sync**, to see when and by how much deferred writeback moves the counters on its own. Result: `read_ios` (+3) and `read_sectors` (+48 sectors) appeared already at the first 5 s sample and never moved again — a synchronous, cold-cache metadata read from the probe's own `open()`/`create()` on a filesystem nothing had touched since `mkfs`/mount, not a deferred effect. The write side arrived in two further steps, each roughly 30 s apart: `write_ios`/`write_sectors`/`flush_ios` were `+5`/`+55`/`+2` at T+5s, `+7`/`+58`/`+4` by T+35s, and reached their final `+9`/`+61`/`+6` (plus `write_merges` +1) by T+65s — then stayed exactly there through T+360s, the full 6 minutes. The container's own read-only sysctls: `fs.xfs.xfssyncd_centisecs=3000` (30 s), `vm.dirty_expire_centisecs=3000` (30 s), `vm.dirty_writeback_centisecs=1500` (15 s) — the ~30 s spacing between the write-side steps matches XFS's own periodic background sync interval, not a single writeback pass. The final write-side magnitude (`write_ios` +9, `write_sectors` +61, `flush_ios` +6) is the same order as the miscited attempt's original `disk2` delta once corrected (`write_ios` +8, `write_sectors` +59, `flush_ios` +6, see above) — close enough (disk2's probe was a warm-cache write, without the cold-cache read component disk1 shows here) to support deferred XFS background sync of a just-completed write as the mechanism, now with measured timing and magnitude behind it rather than an assumption. This is a measured finding, not a general claim: it describes this lab's XFS-on-loop-device behavior at these sysctl values, and Q31's acceptance procedure now states explicitly that the idle window must start only after array disks have settled, per this measurement.

**Results** (every window ran exactly 1800 s; full logs in `spikes/s1/results/`, per-disk fatrace logs in `spikes/s1/results/fatrace/`, every delta computed by `spikes/s1/scripts/window-delta.sh <log>`, never by eye):

| Scenario | Cache TTL | Array-disk `/sys/block/<dev>/stat` (disk1-disk3, BEFORE vs. AFTER) | Cache-disk write IOs (delta) | `fatrace` events on any array disk |
|---|---|---|---|---|
| idle | default (1s) | no field changed on disk1, disk2 or disk3 | 0 | 0 |
| idle | raised (600s) | no field changed on disk1, disk2 or disk3 | 8 (trailing writeback of `smb-default`'s own cache writes, see below) | 0 |
| appdata | default (1s) | no field changed on disk1, disk2 or disk3 | 551 | 0 |
| appdata | raised (600s) | no field changed on disk1, disk2 or disk3 | 669 | 0 |
| smb | default (1s) | no field changed on disk1, disk2 or disk3 | 535 | 0 |
| smb | raised (600s) | no field changed on disk1, disk2 or disk3 | 667 | 0 |

"No field changed" is `stat-delta.sh`'s literal output: every one of the 17 `/sys/block/<dev>/stat` fields matched byte-for-byte between the window's `BEFORE` and `AFTER` snapshot, for every array disk, in every window — the acceptance bar issue #2 states ("counters stay flat for ≥ 30 minutes") is met exactly as written, not approximately. `run-window.sh` now runs an explicit `sync` immediately before its `BEFORE` snapshot (not only once, before the whole matrix) — the fix the settle gate and characterization above establish is necessary, applied everywhere a window could otherwise start mid-writeback. The cache disk's write-IO counters moved by a comparable, substantial amount in every `appdata`/`smb` window regardless of cache-TTL setting (hundreds of write IOs vs. single digits or zero in `idle`), confirming the proxy workload genuinely ran each time — this is what makes the array disks' zero-delta result meaningful rather than an artifact of nothing happening at all. `idle-raised`'s own small cache-disk delta (`write_ios` +8, `write_sectors` +63, `flush_ios` +4) is the same deferred-writeback pattern as above, this time on the cache disk, trailing from `smb-default`'s heavy cache writes in the immediately preceding window — the cache disk is not part of the acceptance criterion, and this delta does not touch any array disk (confirmed by that window's own flat array-disk row). Every array-disk `fatrace` log and its matching `.err` file is empty in every one of the six windows (`spikes/s1/results/fatrace/*-disk[123].log`), and the positive control (above) confirms that an empty log means "no IO", not "the tracer wasn't watching".

**Why this run has no exception where the previous one did.** The previous attempt's positive control ran immediately before `idle-default`, with no settle gate between them, so `idle-default`'s `BEFORE` snapshot was taken while the probe's deferred writeback was still in flight. This run's characterization and positive control both run — and settle — *before* the matrix starts, and every window also `sync`s immediately before its own `BEFORE` snapshot. Incidentally, the characterization (on `disk1`) and the positive control (on `disk2` and `disk3`) between them touch every array disk once before the matrix begins, which is also why no array disk in this run shows the cold-cache read effect the characterization found on `disk1`'s very first-ever operation — by the time `idle-default` starts, every array disk's metadata is already warm.

**The raised-timeout question, answered as measured, not assumed.** An earlier draft of the test plan dropped `idle:raised` and `appdata:raised` on the reasoning that a mergerfs-FUSE-level cache-TTL setting cannot matter to a workload that never queries the FUSE layer. The maintainer correctly overruled that for this issue — narrowing an explicit acceptance bar ("each with default and raised cache.* timeouts") is a maintainer call, not an executor one — and the six-window run above is the actual measurement: **raised timeouts made no observable difference in any of the three scenarios.** For `idle`/`appdata` this matches the architectural reasoning (nothing ever asks the pool for anything, so there is nothing for a cache TTL to serve stale-vs-fresh). For `smb`, the reason is different and more interesting: an idle, connected, non-browsing `smbclient` session generated **no periodic traffic at all** — `smbd`'s log shows the one-time startup banner and a one-time `passdb.tdb` version conversion, then nothing for the remaining ~30 minutes, in both the default and raised window. The hypothesis that `smbd`'s own housekeeping (oplock breaks, change-notify polling) would periodically walk the pool tree was not observed in this minimal Samba 4.22 config with a single idle, non-browsing client. That is a real result, not a proxy artifact — but it is also a narrower test than "a real SMB client on a real desktop" (see residual risk).

**Recommendation on "Quiet mode" timeouts (doc 02 §1's `cache.entry`/`cache.attr`/`cache.negative_entry` row).** This run did not find a scenario where raising the TTL from 1 s to 600 s bought anything — the default already kept every tested scenario silent — but it also tested none of the scenarios doc 08's own "common culprits" list names (directory browsing, an indexer, `updatedb`, multiple clients), which are precisely the cases a longer TTL should help with, since those genuinely re-query the tree. Recommendation: ship the "Quiet mode" preset at `cache.entry=cache.attr=cache.negative_entry=600` (10 minutes) as planned, stating the staleness tradeoff plainly in the wizard — a file added or removed by another process can take up to 10 minutes to show up in a client's cached directory listing — rather than concluding from this run that the setting is unnecessary. The defaults (short TTLs) already suffice for the scenarios tested here; the raised setting remains a recommendation for the untested culprits, not a confirmed necessity.

**Residual risk**, beyond the standing "firmware/controller wakes with no host IO" (doc 06 §6): everything this run's proxies don't cover, plus what the settle gate and characterization narrow but do not eliminate. No real Docker container (the lab has no Docker-in-Docker, doc 06 §3) — the appdata proxy is a shell loop writing directly to the cache disk, not a container's own filesystem/image-pull/log-driver traffic. One idle SMB client, not a real desktop's directory browsing, thumbnail generation, search indexer (Windows Search, Spotlight) or antivirus real-time scanner — doc 08's own culprit list names several of these and none were exercised. No NFS client at all. No concurrent multi-client load. No `updatedb`/`mlocate` run against the pool. No SMART polling (tracked separately, doc 06 §6's own table row). **Fanotify-based attribution cannot see writeback-only IO** — deferred flush of an already-completed file operation reaches the block device with no corresponding file-level event, confirmed and timed by this run's characterization (up to ~65 s after the triggering operation, on this lab's XFS/loop-device setup with 30 s `xfssyncd_centisecs`) — so a real "what woke my disk" feature (Q32) built purely on fanotify would misreport a deferred flush as unattributed IO; a production implementation needs either a settle/sync discipline around measurement windows (as this run now applies) or a complementary block-layer signal for this case. The ~65 s settling time and the 30 s XFS sync-thread cadence behind it are measured on loop devices in this lab, not on real disks with real firmware and real controllers — a real disk's own settling time before it is safe to call "quiet" for spindown purposes is not established by this run. This run confirms the zero-organic-IO property holds for the three scenarios and both cache settings issue #2 asked for, exactly as stated (no exceptions, no explained-away deltas); it does not confirm spindown survives everything doc 08 names as a risk.

Commands, raw before/after snapshots and named deltas, per-disk `fatrace` logs (and their stderr), the positive control's log (both probes), the settle gate's full sample timeline, the trailing-write characterization's full sample timeline, and `smbd`/`smbclient` logs for all six windows: `spikes/s1/`.

---

## Spike 2 — Unraid disk adoption

**Verdict: confirmed. This works, and it is well-established outside the Unraid ecosystem.**

The strongest confirmation comes from professional data recovery practice, where mounting Unraid disks without Unraid is routine work:

- For each healthy data disk, the XFS or btrfs filesystem is mounted directly and files extracted — unlike RAID recovery, no array reassembly is needed for disks that read cleanly, and each disk's share directories are catalogued and merged into a unified recovery set.
- The USB flash drive is not needed to recover data: each data disk is self-contained, the individual filesystems are mounted, and shares are reassembled from the per-disk directory structures. A dead Unraid stick is a configuration loss, not a data loss.
- Even after a parity rebuild onto wrong assignments — where the parity data itself is destroyed — every individual data disk still contains its original XFS or btrfs filesystem and is extracted directly.

That is precisely the Hoserva import model, described by a third party as standard practice. It also confirms the two secondary assumptions in doc 05: shares are reconstructable purely from per-disk directory structure, and the flash drive carries configuration only.

Supporting details from Unraid's own documentation: XFS is described as generally the best choice for array drives, with EXT4 as a solid traditional alternative, while ZFS and BTRFS are positioned for cache pools and multi-device setups — so the XFS-majority assumption holds for the array specifically. And Unraid identifies disks by serial number and size rather than by SATA port, so drives can be moved between ports without affecting assignments, which validates the serial-based mapping in doc 05 §4 step 7: the serials Hoserva matches on are the same identifiers Unraid itself uses.

### Implications for the spec

- **The primary migration path is sound.** Doc 05's technical basis is confirmed rather than assumed.
- **Filesystem repair belongs in the pre-flight.** Since recovery practice leans on `xfs_repair`, the migration scan should run `xfs_repair -n` (dry run) on each disk and refuse to adopt a filesystem that reports errors. Adopting a subtly corrupt filesystem into a new pool and then computing parity over it would bake the corruption in. *(Adopted for every supported filesystem — doc 05 §3, Q23.)*
- **Encryption is a real complication, per Unraid's own warning** that encrypted drives can pose challenges for recovery if corruption occurs and that encryption complicates data recovery if something goes wrong. Keep LUKS arrays out of v1 or behind an explicit "advanced, you understand the risks" path. *(Adopted: detect and refuse in v1 — Q22.)*
- **ZFS-formatted array disks (Unraid 6.12+) are now plausible enough to need a stated position.** Recommendation: detect and refuse in v1 with a clear message, rather than half-supporting. *(Adopted — Q23.)*

### Remaining hands-on work

Unchanged from doc 06 §5 — build the synthetic Unraid fixtures, snapshot them, and run the checksum-verified migration against them. The point is no longer "does this work" but "does our implementation of it work", which is a normal testing problem.

---

## Spike 9 — Lab container (dev host) and Spike 8 — dependency availability (partial)

Hands-on, 2026-09-14, on the primary dev host (CachyOS, Docker 29.8, cgroup v2, no AppArmor), `debian:trixie-slim`.

**Verdict S9 (dev host): confirmed.** The narrowed lab container from doc 06 §3 runs real loop devices, XFS and mergerfs without `--privileged` and without a `/dev` bind mount, and cannot open the host's disk. Hosted CI runners are still to check.

Recipe run:

```
docker run --rm --name hoserva-lab-s9check \
  --cap-add SYS_ADMIN --device /dev/fuse --device /dev/loop-control \
  --device-cgroup-rule 'b 7:* rmw' \
  -v "$S:/lab/s9check" debian:trixie-slim sh -c '…'
# inside: mknod /dev/loop0..63 (b 7 N); truncate 2×4G images; losetup --find --show;
# mkfs.xfs; mount; mergerfs -o category.create=mspmfs,minfreespace=100M disk1:disk2 user;
# write a file; cleanup trap detaches loops by `losetup -j <own image>`
```

Observed:

| Check | Result |
|---|---|
| Loop attach, `mkfs.xfs`, mount inside container | Works (`/dev/loop0`, `/dev/loop1`) |
| mergerfs pool with `category.create=mspmfs` | Mounts, 7.8G pool, file written through it landed on a branch — the policy name is accepted by 2.40.2 (its fallback behaviour is still S6's job) |
| Host NVMe visible in container | No — `/dev/nvme0n1` absent |
| `mknod` of the NVMe's `b 259:0` and open it | `mknod` succeeds, `open` fails with *Operation not permitted* — the device cgroup rule is what blocks it |
| Host loop devices before / after | 0 / 0 — the per-image detach left nothing behind |
| AppArmor | Not present on this host; `apparmor=unconfined` is still needed on Debian/Ubuntu hosts (unverified here) |

**Two things the design had missed:**

1. **The lab leaves root-owned files in its bind mount.** Images and mount directories are created by root inside the container, so the unprivileged user cannot delete them afterwards (`rm: … Keine Berechtigung`). Teardown must remove `.lab/<id>` **from inside the container** before it exits — otherwise the orchestrator cannot delete an agent's scratch clone. Doc 06 §3 and the orchestrate skill now say so.
2. **Debian's mergerfs build reports `mergerfs vunknown`** for `--version`. `hoserva doctor` and version pinning must read the installed version from package metadata (`dpkg-query -W -f '${Version}' mergerfs`), never from the binary.

**Partial S8: both storage dependencies are in Debian 13.** `apt-cache policy` in `debian:trixie-slim`: `mergerfs 2.40.2-5`, `snapraid 12.4-1`. The assumption behind Q7 that SnapRAID might be unavailable is wrong; Q7's default is simplified accordingly. Still open: whether 2.40.2 behaves as doc 02 needs (`mspmfs` fallback, branch modes — S6).

---

## Net effect on the risk register

| Risk | Was | Now |
|---|---|---|
| R1 — spindown regression | **High**, potential kill criterion | **Medium.** Not Hoserva-specific; a property of union filesystems. Converts into a useful feature via wake attribution. Agent-run in the lab, 2026-09-15: zero array-disk IO across all 6 acceptance-matrix windows (above) — residual risk (firmware wakes, real clients, L3) unchanged |
| Migration viability | Assumed, unverified | **Confirmed** by independent professional practice |

Neither spike kills the plan. The spindown finding actually improves it: "we tell you which process woke your disks" is rarely offered by NAS tooling today, and it addresses a long-standing frustration for anyone running a union-filesystem pool.

---

## Sources

This research originally carried citation markers without URLs; the markers have been removed and the claims kept as paraphrase. Primary sources to re-link before any of it is quoted publicly:

- mergerfs documentation and project discussions: `github.com/trapexit/mergerfs` (spinup behaviour, caching, create policies, the file-index feature request)
- Unraid documentation: `docs.unraid.net` (filesystem recommendations, disk identification by serial, encryption warnings)
- Unraid forum threads on disks not staying spun down (multiple, spanning a decade)
- Professional Unraid data-recovery write-ups describing per-disk extraction without the flash drive
