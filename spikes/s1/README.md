# Spike S1 — Spindown under mergerfs, agent-run confirmation

Findings are in `docs/internal/08-spike-findings.md` (Spike 1 section). This
directory holds the experiment scripts and the raw output of the run that
findings section is based on.

## What ran, and where

Lab `2-a1` (loop-device lab, doc 06 §3), array: 1 parity + 3 data disks
(`disk1`-`disk3`, 1G XFS each) + 1 cache disk (512M XFS), mergerfs pool over
the 3 data disks at `/lab/2-a1/mnt/user`.

Packages added to the lab image for this spike only (not baked into
`scripts/devenv/Dockerfile` — recorded here per the Spikes rule in
`CLAUDE.md` that a spike may `apt-get install` inside its own disposable lab
container and must record what and why): `attr 1:2.5.2-3`, `fatrace
0.18.0-1`, `samba 2:4.22.11+dfsg-0+deb13u1`, `smbclient
2:4.22.11+dfsg-0+deb13u1`, `procps 2:4.0.4-9` (the last only needed for
`ps`/`kill` during interactive debugging, not by the scripts themselves).

## Scripts

- `scripts/snapshot.sh` — prints `/sys/block/<dev>/stat` for every lab disk,
  resolved from the loop device backing each image (never a hardcoded loop
  number, since `losetup --find` assigns them arbitrarily). Its comment
  lists the kernel's full 17-field `/sys/block/<dev>/stat` layout — an
  earlier version of this comment listed only 11 fields, which led a
  previous attempt to misattribute a delta to fields it never checked.
- `scripts/stat-delta.sh "<before line>" "<after line>"` — computes and
  prints named, per-field deltas between two `snapshot.sh` lines for the
  same disk. Every delta anywhere in this spike's results or write-up is
  computed by this script, never parsed by eye.
- `scripts/window-delta.sh <run-window.sh log file>` — re-parses a
  committed `run-window.sh` log and prints `stat-delta.sh`'s deltas for
  `disk1`-`disk3` between its `BEFORE` and `AFTER` snapshot, so a reader can
  check any window's claim against the committed raw log directly.
- `scripts/positive-control.sh` — the attribution mechanism's positive
  control, run once before the acceptance matrix and gating it. Two parts:
  (1) a direct-disk probe — writes, reads and deletes a probe file directly
  under `disk2`'s own mountpoint, confirming the probe's events appear only
  in `disk2`'s tracer; (2) a pool-routed probe — writes and reads a probe
  file through the mergerfs pool mount (`$LAB/mnt/user`), confirming the
  event appears only in the tracer of whichever disk mergerfs's create
  policy actually placed it on, attributed to the `mergerfs` process by
  name. Fails loudly if any tracer exits non-zero, or if either probe's
  events don't land where expected.
- `scripts/settle-gate.sh <settle_seconds> <max_wait_seconds>` — run once
  after the positive control and before the acceptance matrix: an explicit
  `sync`, then samples `disk1`-`disk3` every 5s until every array disk has
  been unchanged for `settle_seconds`, or fails after `max_wait_seconds`.
  Exists because the positive control's own probe writes leave deferred
  writeback pending that does not appear immediately, even after `sync` —
  see `scripts/trailing-write-characterization.sh` below.
- `scripts/trailing-write-characterization.sh <disk>` — standalone
  measurement, run once before the positive control on a disk nothing else
  has touched: one `sync` for a clean baseline, a create+write+unlink probe,
  then samples every 5s for 6 minutes with **no further sync**, printing a
  named delta at every sample. Also prints the container's
  `fs.xfs.xfssyncd_centisecs`, `vm.dirty_expire_centisecs` and
  `vm.dirty_writeback_centisecs` (read-only). Measures how long deferred
  writeback takes to settle on its own, and by how much, so the settle
  gate's period is justified by a measurement rather than assumed.
- `scripts/run-window.sh <idle|appdata|smb> <default|raised> <seconds>` —
  one measurement window: remounts the pool with Hoserva's intended
  mergerfs options at the given cache-timeout setting, `sync`s, snapshots
  before, runs the scenario's workload and, per array disk, a dedicated
  `fatrace -c` capture scoped to that disk only, snapshots after, prints a
  named per-field delta for each array disk (via `stat-delta.sh`), then
  everything else (mount line, both snapshots, each disk's fatrace log and
  stderr, and — for `smb` — the smbclient and smbd logs). Clears each
  disk's previous log/err before starting its tracer and fails the window
  loudly if any tracer's exit status is non-zero, rather than silently
  reporting an empty result for a tracer that never ran.
- `scripts/run-all.sh <seconds> [settle_seconds] [max_wait_seconds]` — runs,
  in order: the trailing-write characterization (on `disk1`), the positive
  control (gating the rest), the settle gate, then the full 3 x 2 = 6-window
  matrix sequentially (one array, so windows cannot run concurrently without
  contaminating each other's disk counters — doc 06 §6, §3). One run of this
  script produces every result file the acceptance matrix needs.

Run from the host, against your own lab id, once it is up
(`make lab-up HOSERVA_LAB_ID=<id>`):

```
docker compose -f docker-compose.dev.yml -p hoserva-lab-<id> exec -T lab \
  bash /src/spikes/s1/scripts/run-all.sh 1800 180 900
```

## Why the attribution mechanism changed from the attempt before last

The attempt before last's `fatrace` invocation ran one instance covering the
container's whole mount namespace, then filtered its output to paths under
each array disk's mountpoint after the fact. That mechanism cannot work in
this lab: the container's capability set (`docker-compose.dev.yml` grants
`SYS_ADMIN` only, confirmed via `/proc/1/status` → `CapEff=0xa82425fb`) lacks
`CAP_DAC_READ_SEARCH`, which `open_by_handle_at(2)` needs to resolve a
fanotify file handle back into a path — every event's path printed as
`(deleted)` regardless of what happened, so the path filter could never
match, and "0 attributed events" in every prior result file was not a
meaningful zero.

The fix, per an explicit orchestrator decision against widening the lab's
capabilities (out of scope for a spike, Q45): run **one `fatrace -c`
("current mount") instance per array disk**, each launched with its working
directory on that disk's own mountpoint before `fatrace` starts. `-c` marks
only the filesystem of the current directory (`FAN_MARK_FILESYSTEM`), so any
event a disk's own tracer records is IO on that disk by construction,
attributed by PID and process name — which `fatrace` reports without needing
to resolve a path at all. `scripts/positive-control.sh` proves this
empirically for both a direct-disk write and a pool-routed write (added this
attempt — see below).

## Restarts and anomalies, reported as they happened

CLAUDE.md and the orchestrate dispatch rules require restarts to be
explained, not silently redone; this section covers every attempt's
restarts and anomalies, oldest first.

### Attempt 1 (rejected; kept for the record)

1. **09:33Z run, killed ~09:42Z.** Misread a mid-task comment about the
   dispatch's real wall-clock budget as a hard ~30-minute cap on the whole
   executor and tore the lab down to avoid overrunning it. The comment did
   not mean that; the lab was rebuilt once this was clarified.
2. **09:45Z run, killed ~11:00Z, after ~75 minutes.** Rebuilding the lab
   after (1) reran `make lab-up`, which only reinstalls the base
   `scripts/devenv/Dockerfile` packages — it does **not** replay the
   `apt-get install` from the smoke-testing phase. `attr`, `fatrace`,
   `samba` and `smbclient` were silently missing for this second run.
   Every "FATRACE full log: (empty)" from that run was a false negative
   (fatrace was never running, not running-and-seeing-nothing), and the
   `smb` scenario's `smbd --daemon` step failed with "command not found"
   partway in and left the window's `run-window.sh` process wedged. Caught
   mid-run by directly checking live process state and package presence in
   the container rather than trusting output alone; the packages were
   reinstalled and the run restarted a third time. That third run produced
   the results attempt 1 committed — but with the broken attribution
   mechanism above, which a verifier caught and rejected.

### Attempt 2 (rejected; kept for the record)

No restart was needed for the acceptance matrix itself: the run started at
2026-09-15T14:29:10Z and finished at 2026-09-15T17:29:21Z in one pass — the
positive control, then all six windows, sequentially, each running its full
1800s.

One result **was** investigated rather than accepted at face value: the
first window, `idle-default`, showed a small delta on `disk2` with zero
`fatrace` events attributed to it. Attempt 2's write-up explained this as
deferred kernel writeback of the positive control's own probe — the right
mechanism — but misidentified which stat fields moved (`read_sectors`,
`read_ticks`, `io_ticks`) by parsing the raw snapshot line against a
`snapshot.sh` comment that listed only 11 of this kernel's 17 fields. A
verifier re-parsed the same committed raw lines field-by-field and found the
actual delta was `write_ios` (+8), `write_merges` (+1), `write_sectors`
(+59), `flush_ios` (+6) — a write-side, not read-side, effect — and that the
follow-up reproduction attempt 2 cited (`writeback-timing-check.log`, now
removed, superseded by attempt 3's characterization below) showed a smaller
magnitude than it claimed to reproduce. Rejected for these reasons, plus a
positive control that only covered a direct-disk write, not a pool-routed
one.

### Attempt 3 (this one)

No restart was needed. `run-all.sh` ran end to end in one pass, 2026-09-15
18:04:06Z-21:14:16Z (~3h10m): the trailing-write characterization (6 min),
the positive control (~16s, both probes), the settle gate (230s), then all
six windows sequentially, each running its full 1800s.

Before the full run, each new/changed script was smoke-tested individually
against the lab with short durations (10-90s) to catch scripting mistakes
before committing to the ~3-hour full run — none of those smoke tests are
part of the committed results; only the single `run-all.sh` invocation's
output is.

The full run needed no exception this time: every one of the six windows'
array-disk `/sys/block/<dev>/stat` lines was byte-for-byte identical between
`BEFORE` and `AFTER`, and every array-disk `fatrace` log stayed empty. See
`docs/internal/08-spike-findings.md` (Spike 1) for the corrected field
analysis, the settle gate's own timeline (two small deferred-write steps at
T+20s and T+50s after its `sync`, then genuinely quiet), and the
trailing-write characterization's findings (settles by ~65s after a probe on
this lab's XFS-on-loop-device setup, matching the corrected attempt-2 delta
in magnitude).

## Contents of `results/`

- Six `<scenario>-<mode>.log` files (raw `run-window.sh` output — mount
  line, before/after snapshots, a named per-field delta for each array disk,
  each array disk's fatrace log and stderr, and for `smb`, the
  smbclient/smbd logs) and matching `.err` files (all empty — `run-window.sh`
  only writes to its own stderr, redirected here, on a fatal tracer failure,
  which did not happen in this run).
- `positive-control.log` / `.err` — the positive control's own run (both the
  direct-disk and pool-routed probe), gating the matrix.
- `settle-gate.log` / `.err` — the settle gate's full sample timeline
  between the positive control and the matrix.
- `trailing-write-characterization.log` / `.err` — the standalone
  deferred-writeback timing/magnitude measurement on `disk1`, run before the
  positive control.
- `fatrace/` — every window's per-disk fatrace log/err pairs (three disks x
  six windows = 18 pairs, all empty this run), plus
  `fatrace/positive-control/` (the direct-disk probe's per-disk logs —
  `disk2.log` non-empty, `disk1.log`/`disk3.log` empty) and
  `fatrace/positive-control-pool/` (the pool-routed probe's per-disk logs —
  `disk3.log` non-empty this run since that is where `mspmfs` placed the
  probe, `disk1.log`/`disk2.log` empty).

`writeback-timing-check.log` (attempt 2) is removed: its follow-up
reproduction is superseded by attempt 3's
`trailing-write-characterization.log`, which measures the same phenomenon
with a longer, unforced sampling window and named per-field deltas.
