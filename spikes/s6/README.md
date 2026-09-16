# Spike S6 — Per-share mergerfs mount topology, agent-run confirmation

Findings are in `docs/internal/08-spike-findings.md` (Spike 6 section). This
directory holds the experiment scripts and the raw output of the run that
section is based on.

## Verdict

**Confirmed. The topology in doc 02 §1 / Q12 stands; the two-mount fallback
is not adopted.** A catch-all plus a dozen per-share mounts (all three
cache modes) mount and unmount cleanly in dependency order; `mspmfs` falls
back to the parent path exactly as documented while `epmfs`, changed only
in that one option, returns ENOSPC on the identical branches and fill
state; `NC` branches stay readable while new files land on cache; a stray
top-level write lands on a data disk, never the boot device; the cost of
twelve share mounts is well inside R12's own "too high" bar; and a killed
share mount fails safely (`ENOTCONN`) rather than silently exposing the
array underneath it, recovering cleanly on remount.

## What ran, and where

Lab `7-a1` (loop-device lab, doc 06 §3), built with `make lab-up`'s own
defaults — `LAB_DATA_DISKS=3`, `LAB_DATA_SIZE=1G`, `LAB_PARITY_SIZE=1G`,
`LAB_CACHE_SIZE=512M`, `LAB_MINFREESPACE=50M` — no override needed; this
spike's own dedicated three-disk array for the fallback test (below) uses
300 MiB images, matching spike S2's own finding that xfsprogs 6.13 refuses
to format much below that.

`mergerfs 2.40.2-5` (`dpkg-query -W -f '${Version}' mergerfs`, per doc 08
S9's own lesson — never `--version`, Debian's build reports `vunknown`) —
already in the base lab image (`scripts/devenv/Dockerfile`), nothing
additional installed for this spike. `xfsprogs 6.13.0-2+deb13u1`.
`results/versions.log`.

Run via `make lab-up HOSERVA_LAB_ID=<id>` (create-array.sh's own defaults),
then:

```
docker compose -f docker-compose.dev.yml -p hoserva-lab-<id> exec -T lab \
  bash /src/spikes/s6/scripts/run-all.sh
```

`run-all.sh` writes its own results under `$LAB/s6-results` (the container's
bind-mounted, but host-read-only, `/src` rules out writing straight into
`spikes/s6/results/` from inside the lab — see "A read-only `/src` finding"
below) and, **by default, removes that directory itself once every step has
passed**, so `make lab-up` → `run-all.sh` → `make lab-destroy` completes
unattended with no manual step in between — confirmed end to end this run
(below, "Collecting results and tearing down"). Capturing the results for
this commit uses a separate, explicit opt-out (`S6_KEEP_RESULTS=1`),
described in that same section.

## Scripts

Run in order by `scripts/run-all.sh`, sharing helpers and the twelve-share
topology definition from `scripts/lib.sh` so `02-topology-up.sh`,
`06-cost.sh` (which tears the topology down and rebuilds it around its own
"single mount" comparison) and `08-topology-down.sh` don't carry three
copies of the same mount list that could drift apart:

- `scripts/lib.sh` — shared helpers. `s6_mount` starts every mergerfs
  instance this spike ever creates the same way: **in the foreground
  (`-f`), backgrounded by the shell (`cmd & pid=$!`)** — the exact pattern
  the mergerfs wiki's own systemd unit uses (reference checkout
  `man/mergerfs.1`, "systemd mount": `ExecStart=... -f`, `Type=simple`,
  `ExecStop=fusermount -uz`) and the pattern this dispatch's kill-by-PID
  rule requires, since the PID killed in step 7 is exactly the one
  captured at that mount's own launch. `s6_unmount` unmounts and waits for
  that same PID to exit. `s6_pid_alive` and the zombie-accumulation finding
  it exists for are below. `s6_bring_up_shares`/`s6_tear_down_shares` own
  the twelve-share topology definition (four of each cache mode) shared
  across scripts. `s6_locate` reports which physical branch a file
  actually landed on — every "landed on X" claim in this spike's findings
  is backed by this function's output in `results/`, not eyeballed.
- `scripts/01-ordering-shadow-mount.sh` — the "wrong order" ordering
  experiment (below) using a disposable `shadow-test` share, and ends by
  establishing the catch-all mount every later script builds on.
- `scripts/02-topology-up.sh` — mounts the twelve shares + the mover
  target, confirms exactly 13 mounts under `$LAB/mnt/user` via `findmnt`,
  then confirms the *other* ordering direction: the catch-all refuses to
  unmount while a share is still nested inside it.
- `scripts/03-fallback-mspmfs-epmfs.sh` — the `mspmfs`/`epmfs` fallback
  experiment (below), on its own dedicated three-disk array kept separate
  from the standing lab array so filling one branch never touches anything
  else this spike measures.
- `scripts/04-nc-branches.sh` — a file already on an `NC` branch stays
  readable through the share; five new files all land on cache, never on
  an `NC` branch; `NC` still permits change and delete.
- `scripts/05-catchall-stray-write.sh` — a top-level write with no
  per-share mount of its own lands on a data disk through the catch-all,
  never on cache and never outside the pool.
- `scripts/06-cost.sh` — RSS and process count with the topology up, then a
  counterbalanced (ABBA), warmed-up throughput comparison between the
  twelve-share topology and a single mount, tearing the shares down and
  rebuilding them several times in the process, ending back up for
  `07-failure-kill.sh`.
- `scripts/07-failure-kill.sh` — kills exactly one share's own mergerfs
  PID (confirmed against its own `/proc/<pid>/cmdline` immediately before
  the kill, never a name or pattern match), records what the dead mount
  exposes, and confirms remount recovery.
- `scripts/08-topology-down.sh` — unmounts everything in dependency order
  (shares + mover target, then the catch-all), confirms zero *live*
  mergerfs processes remain, removes the now-empty per-share mountpoint
  directories this spike itself created (never a disk's own top-level
  directory), and archives every mount command this spike ran to
  `results/mount-commands.log`.
- `scripts/run-all.sh` — runs all eight above in order on a freshly built
  lab, checking for `mergerfs` first and failing loudly if it's missing.

## Step-by-step findings

### Step 2 — Ordering

**Wrong order (a share mounted before its catch-all): the mount succeeds,
accepts writes, and then becomes silently unreachable — not corrupted,
not torn down, just unreachable via the intended path — the moment the
catch-all mounts over its parent directory**, because the child's own
mount is attached to a dentry inside the *pre-catch-all* directory tree,
which the new catch-all mount now covers entirely. `01-ordering-shadow-mount.sh`:
a disposable `shadow-test` share (cache=RW, disk1=NC) is mounted directly
at `$LAB/mnt/user/shadow-test` while the catch-all is deliberately down,
a marker file is written through it (confirmed on cache, not on the `NC`
branch), then the catch-all is mounted over `$LAB/mnt/user`. Result
(`results/shadow-mount-after-catchall.log`): `ls` on the same path now
shows an **empty directory** — the catch-all's own view of `disk1`'s
"shadow-test" (real, but empty; the marker file lives on cache, which the
catch-all's own branch list — `disk1:disk2:disk3` — doesn't include).
The orphaned process is still running and still listed in
`/proc/self/mountinfo` (`results/shadow-mount-mountinfo-while-shadowed.log`)
— confirmed, not asserted. **Recovery is exactly Linux's own mount-stacking
rule**: unmounting the catch-all (the top of the stack at that parent
path) re-exposes the pre-existing directory tree underneath, including the
shadow-test mountpoint, without anyone touching shadow-test's own mount at
all — the marker file reappears byte-identical (sha256 confirmed
before/after). This is the concrete, structural reason doc 02 §1's
`RequiresMountsFor=` requirement is not a tidiness suggestion: a share that
starts before its catch-all doesn't fail loudly, it silently stops being
what it claims to be.

**Correct order, the other direction (unmounting the parent while children
are nested inside it): refused.** `02-topology-up.sh`, with all twelve
shares up: `fusermount -u $LAB/mnt/user` fails with `Device or resource
busy` (`results/catchall-busy-unmount.log`), and every share stays mounted
— confirmed by re-checking each one, not just trusting the non-zero exit.
The kernel enforces this half of the ordering on its own; only the
"child before parent" direction needed doc 02 §1's own systemd ordering to
prevent it.

*Not covered by this run*: systemd's own `RequiresMountsFor=` ordering at
boot — this lab container has no init system to exercise it (doc 06 §3);
deferred to L3, which does not exist yet. What this run establishes is the
*mechanism* systemd ordering exists to prevent, empirically, on this
mergerfs build.

### Step 3 — `mspmfs`/`epmfs` fallback

Two pools (`fallback-mspmfs`, `fallback-epmfs`) mounted over the identical
three branches (`mfp1:mfp2:mfp3`, three dedicated 300 MiB disks, separate
from the standing array), with **every option identical except
`category.create`** — `moveonenospc=true`, `minfreespace=50M`, same
branch list — so the outcome is attributable to the policy alone, per the
dispatch's own instruction for this step.

- **Positive control**: with `mfp1` not yet full, a write to
  `media/baseline.bin` lands on `mfp1` — the only branch where `media`
  already existed (`results/fallback-baseline-location.log`) — confirming
  the existing-path mechanism this experiment is about to stress is real
  before stressing it.
- `mfp1` filled via `fallocate` (doc 06 §3's own filler-file technique)
  from 224,575,488 B free down to 20,971,520 B (`results/fallback-df-mfp1-before-fill.log`,
  `-after-fill.log`) — under the 50 MiB `minfreespace` filter, with real
  free space still remaining (this is a `minfreespace` filter case, not a
  literal-ENOSPC case).
- **`epmfs`, run first** (deliberately — see "restarts" below), while
  `media` still exists only on the now-filtered `mfp1`: the write returns
  **`No space left on device`**, exit 1 (`results/fallback-epmfs-write.log`).
  man/mergerfs.1's `epmfs` entry: *"Of all the branches on which the
  relative path exists choose the branch with the most free space."* — no
  parent-directory retry, and none was observed.
- **`mspmfs`, run second**, same branches, same fill state: the write
  **succeeds**, landing on `mfp3` (`results/fallback-mspmfs-location.log`,
  confirmed absent from `mfp1` and `mfp2`), not on the filtered `mfp1`.
  man/mergerfs.1's `mspmfs` entry: *"Like epmfs but if it fails to find a
  branch it will try again with the parent directory. Continues this
  pattern till finding one."* — confirmed on 2.40.2: it walked from the
  filtered `media` branch up to the pool root, found `mfp3` had room, and
  created `media/` there too.

**Q11's default (`mspmfs`) is confirmed as-designed, not reverted to
`epmfs`.**

**A test-design bug found and fixed while writing this script, recorded
because it is exactly the kind of contamination this dispatch's rigor
rules exist to catch**: an earlier version ran `mspmfs` first. Because
both pools share the same physical branches, `mspmfs`'s own fallback write
created `media/` on `mfp3` as a side effect — which then gave `epmfs` a
*second* branch with the existing path, and `epmfs`'s write succeeded too
(landing on `mfp3`), which is not what `epmfs`'s own documented behaviour
predicts and was purely an artifact of running the two cases in the wrong
order against shared state. Fixed by reordering: `epmfs` now always runs
first, while `media` still exists on exactly one (filtered) branch.

### Step 4 — `NC` branches

`04-nc-branches.sh`, against `ctm2` (cache=RW, disk1/disk2/disk3=NC):

- A file written directly to `disk2/ctm2/existing.bin` (standing in for a
  file the mover already relocated, or one present before Hoserva started
  managing the share) reads back **byte-identical** through the share
  (sha256 confirmed).
- Five new files written through the share all land on cache
  (`results/nc-new-file-locations.log`); none land on `disk1`, `disk2` or
  `disk3` — man/mergerfs.1: *"NC: (no-create) - Will be excluded from
  create policies."* — confirmed, five for five, not one sample.
- Appending to `existing.bin` through the share changes its content on
  `disk2` directly, and deleting it through the share removes it from
  `disk2` — man/mergerfs.1's own next clause, *"You can't create on that
  branch but you can change or delete"* — both confirmed.

### Step 5 — Catch-all stray write

`05-catchall-stray-write.sh`: a write to `$LAB/mnt/user/stray/file.bin`
(no per-share mount covers `stray`) lands on `disk3` (a data disk), never
on cache — the catch-all's own branch list is `disk1:disk2:disk3` only —
confirmed present on a data disk, absent from cache, byte-identical
through the catch-all's own view.

### Step 6 — Cost

**This step was redesigned after an independent verifier reproduced a
run-order confound in an earlier attempt** (below, in "Restarts and
anomalies") — the numbers and methodology here are the corrected version;
the earlier scenario-B-always-second design and the conclusion drawn from
it are superseded, not extended.

**Method, stated plainly**: this lab container cannot drop the page cache.
No `procps`, and checked directly this run — `mount | grep proc/sys` shows
`/proc/sys` itself is bind-mounted `ro` in this container
(`results/cost-drop-caches-check.log`), so `echo 1 > /proc/sys/vm/drop_caches`
fails with `Read-only file system` even though the file's own permission
bits (`--w-------`) would otherwise allow root to write it — not assumed
from S1, checked again for this step specifically. `dd ... iflag=direct`
bypasses the page cache on the read side instead and worked on this build
(`direct_io=yes` in every rep). A fixed 200 MiB file, written once
directly to `disk1` (bypassing every pool, identical bytes for every
scenario and every rep — sha256 confirmed unchanged across the whole
step, `results/cost-rss.log`'s sibling checks in `06-cost.sh`). "Single
mount" means the catch-all alone; "twelve share mounts" means the same
catch-all with the full topology nested inside it. Since the test file is
a top-level path with no per-share mount, the kernel crosses exactly one
FUSE layer in both scenarios — what this measures is whether a dozen
*other*, idle mergerfs processes measurably affect an unrelated read's
throughput on a shared host, not a nested-mount read-path tax.

**RSS** (`results/cost-rss.log`): each of the twelve shares uses
7,004–9,136 KiB VmRSS (a spread, not one number); the twelve total
**88,812 KiB** (≈87 MiB). Including the catch-all (9,836 KiB) and the
mover target (7,180 KiB), the full 14-process topology totals **105,828
KiB** (≈103 MiB) — well under R12's own "too high" bar of 200 MiB combined
for twelve share mounts. This figure was not affected by the throughput
redesign below and is unchanged in kind from the earlier attempt.

**Throughput — corrected design.** The earlier attempt ran every
twelve-shares rep, then every single-mount rep, and attributed a
depressed first twelve-shares rep to mount count. It was a run-order
artifact: this run's own script (`06-cost.sh`) now **counterbalances**
scenario order (an "ABBA" pattern — pair 1 is single-mount-then-twelve-shares,
pair 2 is twelve-shares-then-single-mount, and so on, so each scenario's
mean position across the run is nearly identical) and takes **four
discarded warm-up reads in each state** before any counted repetition,
because one warm-up (tried first) was not enough — a first pass at this
fix still showed the first *measured* read after the first state
transition depressed, and a second pass (one warm-up per state) still
showed a multi-repetition climb affecting the earliest reps of *both*
scenarios alike (`results/cost-throughput-warmup.log` shows this run's own
four-rep-per-state climb: 3.3 → 5.1 → 5.5 → 5.5 GB/s in the first state,
5.4 → 5.4 → 5.4 → 5.5 GB/s in the second, before either scenario's first
*counted* rep). Five reps per scenario, ten reads total, order `A B B A A
B B A A B` (`results/cost-throughput-interleaved.log`, the real run
order; `-single-mount.log`/`-twelve-shares.log` are the same data
extracted per scenario for convenience):

| Scenario | Reps, in run order (GB/s) | Median | Range |
|---|---|---|---|
| Single-mount | 5.2766, 5.2810, 5.2821, 5.3572, 5.3449 | 5.2821 | 5.2766–5.3572 |
| Twelve-shares | 5.2903, 5.4487, 5.2673, 5.1781, 5.2826 | 5.2826 | 5.1781–5.4487 |

Relative loss by median (twelve-shares vs. single-mount): **−0.01%**
(twelve-shares read marginally *faster* — within noise, not a real
speed-up). Comfortably inside R12's 10% bar, and the two scenarios'
spreads overlap almost entirely.

**An order-effect check is run and reported either way, not just when it
turns something up.** This run's own check — the Pearson correlation
between each read's sequence position (1–10) and its GB/s value, pooled
across both scenarios — is **r = −0.090**, close to zero: this
particular run shows no visible linear drift left after counterbalancing
(`results/cost-throughput-summary.log`). **That does not retire the
caveat, because this finding rests on this one committed run alone.** The
four-warm-up-reps-per-state design itself was arrived at by iterating in
the lab — a single generic warm-up, then one warm-up per state, then four
— against freshly rebuilt arrays, discarding each earlier attempt's own
run once its own warm-up log showed a still-climbing ramp; no figures from
those earlier, superseded attempts are quoted here, since only this run's
raw files are committed (`results/`), per this dispatch's "one consistent
run" instruction. On its own, this run's −0.01% relative loss by median is
comfortably under the 10% bar, and the two scenarios' spreads overlap
almost entirely; the near-zero order effect in this particular run is not,
by itself, proof the residual order effect this shared host can produce is
gone for good — a single run showing a near-zero r is one data point, no
stronger a basis for a general claim than a single run showing a
non-trivial one would be. It is recorded plainly as an open measurement
caveat (see "Residual risk" in doc 08 §6), not resolved by this run either
way. Loop-device timings on a shared host, relative only, never product
numbers (doc 06 §3): these are GB/s because the loop image is backed by fast host
storage, not a real spinning disk, and every comparison here is
scenario-against-scenario within one run, never an absolute figure.

### Step 7 — Failure (kill)

`07-failure-kill.sh` kills exactly `ao1`'s own mergerfs PID, confirmed
against its own `/proc/<pid>/cmdline` (mentions `ao1`'s mountpoint) and
`comm` (`mergerfs`) immediately before the kill (`results/kill-target-confirmation.log`)
— never a name or pattern match, per the dispatch's kill-by-PID rule.

**While dead**, every operation against `$LAB/mnt/user/ao1` fails with
`Transport endpoint is not connected` — `ls`, `stat`, and `cat` on a file
that was there moments before all fail identically
(`results/kill-dead-mount-behaviour.log`). `findmnt` still lists it as
mounted — the kernel keeps the stale mount entry rather than silently
removing it. **Crucially, the catch-all's own view of that path is never
exposed as a fallback**: a dead per-share mount is a stuck, inaccessible
path, not a hole a stray write could land in by accident. This is the
answer to this step's own question, and it's the good outcome: failure is
loud and contained, not silent and permissive.

**Recovery**: a plain `fusermount -u` on the dead mount succeeded on the
first try — no lazy-unmount escalation needed (`results/kill-unmount-attempt.log`
is empty, meaning no stderr from the successful attempt). A fresh
`s6_mount` of `ao1` immediately afterward reads back the pre-kill marker
file byte-identical (sha256 confirmed) — the underlying data was never at
risk, only path availability during the outage.

### Step 1 — Full lifecycle, mount and unmount

`02-topology-up.sh` mounts the catch-all + twelve shares (four of each
cache mode) + the mover target and confirms exactly 13 mounts under
`$LAB/mnt/user` via `findmnt` (`results/topology-findmnt.log`).
`08-topology-down.sh` unmounts everything in dependency order (shares +
mover first, catch-all last) and confirms **zero live mergerfs processes**
remain (`s6_process_count`, excluding zombies — see below) and
`$LAB/mnt/user` is fully unmounted. Every mount command this spike ever
ran is archived verbatim in `results/mount-commands.log`.

## A container-environment finding, general and worth recording separately

**A mergerfs process unmounted from a *different* script (process) than
the one that started it becomes a permanent zombie in this container,
because PID 1 here (`sleep infinity`) never reaps its orphaned children.**
Found empirically: `mover-ctm1` (mounted by `02-topology-up.sh`, unmounted
later by `06-cost.sh`, a different bash process) reported "still alive
5s after unmount" under a naive `kill -0` check, even though `fusermount
-u` had already succeeded — `/proc/<pid>/stat`'s third field read `Z`
(zombie), not a real hang. `kill -0` succeeds against a zombie's PID (it's
still a valid, if defunct, process table entry) so it cannot distinguish
"still doing work" from "finished, orphaned, unreaped" — `lib.sh`'s
`s6_pid_alive` checks `/proc/<pid>/stat`'s state field instead, treating a
zombie as gone. `08-topology-down.sh`'s final report counts both: **0
live** processes and **174 zombie** entries at the end of this run — every
one of them a mergerfs instance this spike itself started and cleanly
unmounted at some point, just never reaped afterward (higher than an
earlier run's 44: the corrected Step 6 mounts and unmounts the twelve
shares many more times — the ABBA-counterbalanced throughput comparison
tears the topology down and rebuilds it repeatedly rather than once each
way — so this is more mount/unmount churn from the same fix, not a new
kind of leak). They hold no memory
or file descriptors (confirmed: `s6_rss_kb` on a tracked PID mid-run always
read a real, non-zero value; nothing in this run measured RSS on an
already-zombie PID) and are not a resource leak in the product sense —
this is purely a property of this container having no init system (matches
the existing "no systemd inside the lab container" residual-risk note,
doc 06 §3), not something `hoservad`'s own supervision (which will be its
own direct parent for every mount it starts, and can `wait()` on it) would
ever exhibit. Recorded here because it is a real trap for any future spike
or test in this same container that checks "is my mergerfs process still
running" with a plain `kill -0`.

## A read-only `/src` finding

`docker-compose.dev.yml` mounts the repository at `/src` **read-only**
(`.:/src:ro`) — confirmed the hard way: the first run of `run-all.sh`
failed every `tee "$OUT/...")` call with `Read-only file system` when
`$OUT` defaulted to `/src/spikes/s6/results`. Fixed by writing to
`$LAB/s6-results` (inside the lab's own read-write bind mount) instead, and
copying the results out to `spikes/s6/results/` via `docker cp` (which
preserves the calling host user's ownership on the copied files —
confirmed: no `chown` was needed after the copy, unlike the
root-owned-bind-mount pattern doc 08's S9 and S5 sections both describe
for files left *inside* `$LAB`) — see "Collecting results and tearing
down" below for when that copy happens relative to `$LAB/s6-results`
itself being removed. This is a real, general constraint for any future
spike or test that writes output: `/src` inside the lab container is for
reading the repo, never for writing results directly; results always go
through `$LAB`.

## Collecting results and tearing down

`run-all.sh`'s default behaviour (no `S6_KEEP_RESULTS`) is self-cleaning:
once every step has passed, it removes `$LAB/s6-results` itself, in
addition to its own `$S6` scratch directory — the same discipline
`08-topology-down.sh` already applies to its mountpoint directories. This
means the plain three-command sequence — `make lab-up`, `run-all.sh`,
`make lab-destroy` — needs no manual step in between and leaves nothing
behind for `make lab-destroy`'s host-side `rm -rf .lab/<id>` to trip over
(`destroy-array.sh`, run first, only ever removes `$LAB/img` and
`$LAB/mnt` — never `$LAB/s6-results` — so anything left there was always
this spike's own responsibility, not `destroy-array.sh`'s).

**Capturing the results committed in this directory used the opt-out**,
because the results need to leave the container (via `docker cp`, a
host-side command) before anything removes them:

```
docker compose -f docker-compose.dev.yml -p hoserva-lab-<id> exec -T lab \
  bash -c 'S6_KEEP_RESULTS=1 bash /src/spikes/s6/scripts/run-all.sh'
docker cp hoserva-lab-<id>:/lab/<id>/s6-results/. spikes/s6/results/
docker compose -f docker-compose.dev.yml -p hoserva-lab-<id> exec -T lab \
  rm -rf -- /lab/<id>/s6-results
make lab-destroy HOSERVA_LAB_ID=<id>
```

**Both sequences were run end to end this fix round, each against its own
freshly built lab**: the `S6_KEEP_RESULTS=1` sequence above (the source of
every result under `results/`, `docker cp` confirmed to preserve host
ownership, the manual `rm -rf` and the following `make lab-destroy`
confirmed exit 0); and, separately, the plain default sequence — `make
lab-up`, `run-all.sh` with no environment override, `make lab-destroy`,
no `docker cp`, no manual cleanup step — confirmed to also exit 0, closing
the gap the previous fix round's commit message claimed but had not
actually run.

## Restarts and anomalies

- The `mspmfs`-before-`epmfs` ordering bug in `03-fallback-mspmfs-epmfs.sh`,
  described above under Step 3 — found and fixed before this run, not
  after a "passing" run was later found wrong.
- `s6_process_count` initially used `pgrep -c mergerfs`, which does not
  exist in this lab image (no `procps` — machine-check notes). Replaced
  with a direct `/proc` scan (`s6_process_count` in `lib.sh`) before this
  run, matching the "no procps" constraint already known going in.
- The zombie-accumulation finding (above) required two fixes, both applied
  before the clean run these results are from: `s6_process_count` (used at
  the end of `06-cost.sh` and `08-topology-down.sh`) excludes zombie state;
  `s6_pid_alive` (used by every `s6_mount`/`s6_unmount` liveness check
  throughout) does too.
- Two prior full-array leftover-loop-device incidents (this spike's own
  `rm -rf $LAB/img` run manually between attempts, before understanding the
  read-only-`/src` and zombie fixes above needed a second and third full
  rebuild) — each time, the fix was the same discipline `scripts/devenv/lib.sh`
  already documents: resolve each loop device's backing file via
  `losetup -O BACK-FILE`, detach only the ones confirmed backed by this
  lab's own now-deleted images, never `losetup -D`. Not a script bug; a
  reminder that `rm -rf` on a lab's `img/` directory without first letting
  `destroy-array.sh` (or an equivalent explicit `losetup -d`) run leaves
  the loop devices themselves still attached, since deleting a file never
  detaches whatever has it open.
- **Step 6 was rebuilt in a fix round after an independent verifier
  reproduced a run-order confound** in the original scenario-B-always-second
  design (see "Throughput — corrected design" above) and, separately, found
  that `08-topology-down.sh` unmounted everything it started but never
  removed the now-empty per-share mountpoint directories it had `mkdir
  -p`'d, so a subsequent `make lab-destroy` needed them cleaned by hand
  first — `08-topology-down.sh` now does that itself. Getting the
  throughput redesign right took several full `run-all.sh` executions
  against a freshly rebuilt array each time (single generic warm-up, then
  one warm-up per state, then four warm-up reps per state — each version
  run to completion, not aborted mid-way), because each earlier design
  still showed a multi-repetition warm-up climb the previous one hadn't
  caught. None of those earlier attempts' own numbers are quoted anywhere
  in this spike's findings — only this run's raw output, below, is
  committed, and every figure in the Step 6 write-up above and in doc 08
  §6, doc 07's R12 row and doc 13's Q12 is drawn from it alone.
- **A second fix round** addressed two problems an independent verifier
  found in the first fix round: uncommitted, unverifiable numbers from the
  superseded design attempts were quoted in the docs as if they were
  independent supporting evidence (closed by the point above — this run's
  own numbers now stand on their own), and `run-all.sh` left `$LAB/s6-results`
  behind, root-owned, after copying it out by hand, which made a
  follow-up `make lab-destroy` (run without that manual step) fail with
  `Permission denied` on its own final `rm -rf .lab/<id>` — see
  "Collecting results and tearing down" below for the fix and the
  end-to-end confirmation.

Every result in `results/` is from one final, clean, end-to-end
`scripts/run-all.sh` invocation (`results/run-all.log`), against a freshly
rebuilt array, after all of the above were fixed — not a patchwork of the
individual debugging runs above.

## Contents of `results/`

- `run-all.log` — the full, single `run-all.sh` invocation this spike's
  findings are based on: every command actually run, its output, and (for
  every mergerfs mount) its own `+ mergerfs ...` command line and PID.
- `versions.log` — mergerfs's and xfsprogs's dpkg versions, kernel version.
- `mount-commands.log` — every mount command this spike ran, archived
  verbatim by `08-topology-down.sh`.
- `topology-findmnt.log` — the full 13-mount tree under `$LAB/mnt/user`
  plus the mover target, from `findmnt`.
- `catchall-busy-unmount.log` — the refused catch-all unmount attempt
  while shares are nested inside it.
- `shadow-mount-after-catchall.log`, `shadow-mount-mountinfo-while-shadowed.log`
  — the ordering experiment's `ls` output and the orphaned mount's
  `/proc/self/mountinfo` entry.
- `fallback-baseline-location.log`, `fallback-df-mfp1-before-fill.log`,
  `fallback-df-mfp1-after-fill.log`, `fallback-epmfs-write.log`,
  `fallback-mspmfs-write.log`, `fallback-mspmfs-location.log` — the full
  `mspmfs`/`epmfs` fallback experiment's raw `df`, write output/exit codes,
  and branch-location checks.
- `nc-new-file-locations.log` — all five new-file branch-location checks
  for the `NC` step.
- `catchall-stray-location.log` — the stray top-level write's branch
  location.
- `cost-rss.log` — per-share RSS, totals, process count.
- `cost-drop-caches-check.log` — the checked-not-assumed confirmation that
  `/proc/sys` is read-only in this container, so `dd iflag=direct` (not
  `drop_caches`) is this step's own cache-avoidance mechanism.
- `cost-throughput-warmup.log` — the discarded warm-up reads (four per
  state) that motivated this step's redesign, showing the multi-repetition
  climb they exist to absorb.
- `cost-throughput-interleaved.log` — the ten measured throughput reps in
  their real, ABBA-counterbalanced run order (the source of truth for this
  step; `-single-mount.log`/`-twelve-shares.log` are the same data split
  per scenario for convenience).
- `cost-throughput-summary.log` — median/range per scenario, the relative
  loss by median, and the order-effect (Pearson r) check.
- `kill-target-confirmation.log`, `kill-marker-location.log`,
  `kill-dead-mount-behaviour.log`, `kill-unmount-attempt.log` — the
  failure/kill step: the confirmed kill target, the pre-kill marker's
  branch, the dead mount's exact error text, and the (empty, i.e.
  no-escalation-needed) unmount-attempt stderr.
