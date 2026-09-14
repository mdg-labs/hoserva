# Hoserva — Spike Findings

Research answers to the two kill-criteria questions from doc 07 §1. Both are answered from public sources; both still need hands-on confirmation, but the plan does not need to wait on that.

---

## Spike 1 — Spindown under mergerfs

**Verdict: the concern is real, but it is not a regression against Unraid. It is a property of union filesystems in general, including Unraid's own. Proceed, with mitigations.**

### What mergerfs upstream says

The mergerfs author maintains a wiki page on exactly this, and the answer is blunt: limiting drive spinup through mergerfs alone is not possible, because mergerfs is a proxy rather than a cache — it translates client calls across N filesystems, so a `readdir` runs against all branches and aggregates, and an `open` must scan across branches to find and select the file. Doing otherwise would require a significant rewrite. The recommended approach is to limit spinup at the client level — keeping software from touching the filesystem in the first place.

Confirmed elsewhere by the same author: mergerfs stores nothing of its own on the drives and does not query them on its own; the access pattern depends entirely on what is requesting data and the policy of that request. The OS caches some information, but when the OS asks mergerfs for data it has no choice but to pass the request through. The kernel already caches, and the user controls which parts of mergerfs the kernel may cache; mergerfs does no explicit caching of its own, because keeping caches aligned is non-trivial when mergerfs does not own the underlying data.

A long-standing feature request to have mergerfs maintain a file-index so a read could target a single disk exists — motivated by exactly the Plex/Kodi case, where the application only wants one file and the whole pool gets woken — and it has not been implemented, for the reasons above. The author's position is that something must track where files live, users change things out of band and expect that to work, and any interaction with a drive that the OS has not cached means spinup.

### Why this does not kill the project

**Unraid has the same problem.** Its user-share layer (`shfs`) is also a FUSE union over per-disk filesystems, and "my disks won't stay spun down" is a perennial Unraid support topic spanning well over a decade — from browsing a share spinning up all disks, through reads landing on every array disk every few seconds with no open files, persisting after stopping VMs, containers, and the Docker service, to disks refusing to stay down for more than a few minutes after a manual spindown.

So the honest framing is: **this is not "Hoserva is worse than Unraid at spindown". It is "union-filesystem NASes all share this, and the difference is in how much help the tool gives you."** That reframes R1 from a kill criterion to a feature opportunity.

Practical reports are also more positive than the theory suggests. One user with a mixed 2×16 TB / 2×4 TB mergerfs pool reports that with a reasonable Linux setup and enough RAM (32 GB in their case), the kernel's filesystem cache keeps the other drives from spinning up frequently. Kernel dentry and attribute caching does most of the work in practice, provided nothing is actively walking the tree.

### Implications for the spec

Three changes, none structural:

**1. Create policy default — reconsider.** `epmfs` remains a good general default, but for spindown specifically, a fill-one-disk-at-a-time policy is better: `lfs` (least free space) writes to the branch with the least free space that still has room, filling disks one at a time, which is useful when you want most drives spun down most of the time, at the cost of no balancing. Note the `epmfs` fallback caveat — users report that when the disk holding an existing path hits `minfreespace`, writes fail with ENOSPC rather than falling back to another disk, which the setup wizard must account for when choosing defaults.

Recommendation: keep folder-locality as the default for balance and blast-radius reasons, but ship a **"Quiet mode" preset** in the wizard that sets `lfs` plus tuned cache options, with the tradeoff stated plainly. *(Carried forward as Q11: the default is `mspmfs`, which keeps `epmfs`'s locality but walks up the path instead of failing with ENOSPC — to be confirmed on the pinned mergerfs build in spike S6.)*

**2. Cache the kernel can use.** mergerfs caching options (`cache.files`, `cache.entry`, `cache.attr`, `cache.negative_entry`, `cache.statfs`) are the actual lever. Longer entry and attribute TTLs mean fewer passthrough lookups and fewer wakeups, at the cost of staleness when things change out of band. Hoserva should expose this as a single plain-language setting — *Responsiveness vs. quiet disks* — rather than five opaque TTLs.

**3. The real differentiator: tell the user what woke the disk.** The recurring theme in every Unraid thread is that users cannot find the culprit. Hoserva can, and no competitor does it well:

- Track spin-state transitions per disk with timestamps
- Correlate each wake against process-level IO (`fanotify`, or `/proc` IO accounting sampled around the event) to attribute it to a process and, where possible, a container
- Surface it as a **"What woke my disks"** view: disk, time, duration awake, most likely cause
- Common culprits to detect and name explicitly: Plex/Jellyfin library scans, \*arr rescans, the SMART polling loop itself, Samba `recycle`/`vfs` modules, Docker log drivers writing to array paths, `updatedb`/`mlocate` indexing the pool, file indexers on connected Windows clients

**`updatedb` is worth a special mention** — it walks the entire pool nightly and wakes every disk, it is installed by default on Debian, and it is a classic self-inflicted wound. Hoserva should add `/mnt/user` and `/mnt/disk*` to `PRUNEPATHS` automatically on install.

**4. Keep the acceptance test, adjust the target.** Doc 06's criterion (30 minutes standby with shares idle and appdata on cache) stays, but "idle" must be defined as no Samba clients connected and no containers touching pool paths. Measure it, publish the result, and be honest about what breaks it. *(Adopted as Q31; docs 02 and 06 updated.)*

**5. Hoserva must not be its own culprit.** Beyond SMART polling, anything Hoserva runs on a timer that walks the pool defeats all of the above. The one that was hiding in the spec: a live "files changed since last sync" count implies a polled `snapraid diff`, which stats every file. Replaced by a fanotify change journal (Q13, spike S7).

### Remaining hands-on work

- Measure wake frequency under a realistic container load on the test box
- Quantify what the cache TTL settings actually buy
- Verify `smartctl -n standby` genuinely avoids waking disks across both HBA and onboard SATA
- Build and validate the wake-attribution mechanism — this is the piece with no prior art to copy

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

Unchanged from doc 06 §5 — build the Unraid source VM, snapshot it, and run the checksum-verified migration. The point is no longer "does this work" but "does our implementation of it work", which is a normal testing problem.

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
| R1 — spindown regression | **High**, potential kill criterion | **Medium.** Not a regression; Unraid shares the flaw. Converts into a differentiator via wake attribution |
| Migration viability | Assumed, unverified | **Confirmed** by independent professional practice |

Neither spike kills the plan. The spindown finding actually improves it: "we tell you which process woke your disks" is a feature nobody in this space ships, and it addresses a complaint fifteen years old in the incumbent's own forums.

---

## Sources

This research originally carried citation markers without URLs; the markers have been removed and the claims kept as paraphrase. Primary sources to re-link before any of it is quoted publicly:

- mergerfs documentation and project discussions: `github.com/trapexit/mergerfs` (spinup behaviour, caching, create policies, the file-index feature request)
- Unraid documentation: `docs.unraid.net` (filesystem recommendations, disk identification by serial, encryption warnings)
- Unraid forum threads on disks not staying spun down (multiple, spanning a decade)
- Professional Unraid data-recovery write-ups describing per-disk extraction without the flash drive
