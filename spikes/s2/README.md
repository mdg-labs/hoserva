# Spike S2 — Unraid disk adoption, agent-run confirmation

Findings are in `docs/internal/08-spike-findings.md` (Spike 2 section, "Agent-run
confirmation, 2026-09-16"). This directory holds the experiment scripts and
the raw output of the run that section is based on.

## What ran, and where

Lab `3-a1` (loop-device lab, doc 06 §3). Everything here runs under its own
`$LAB/s2/` subtree — never `$LAB/img` or `$LAB/mnt`, which belong to the
standing array `make lab-up` builds and which `destroy-array.sh` removes on
`make lab-destroy`. `$LAB/s2/` is unmounted, its loop devices detached, and
deleted from inside the container before the lab is torn down (see the
executor's own report; nothing was left under `.lab/3-a1/s2/`).

Packages added to the lab container for this spike only (not baked into
`scripts/devenv/Dockerfile`, per the Spikes rule in `CLAUDE.md`): `fdisk
2.41.5-0+deb13u1`, `gdisk 1.0.10-2`. `fdisk` is the package that actually
ships `sfdisk` on Debian — confirmed absent from a stock lab image built
from the base `Dockerfile`; `util-linux` alone does **not** provide it. In
the end the fixtures built here use only the MBR path (`sfdisk`/`mkfs.xfs`/
`xfs_repair`/`blkid`/`xfs_io`), of which `mkfs.xfs`/`xfs_repair` come from
`xfsprogs` and `blkid`/`xfs_io` from `util-linux`, both already in the base
lab image — only `sfdisk` needs `fdisk` installed. `gdisk` was installed
anticipating GPT fixture work that this run did not reach (see "Residual
risk" in doc 08 §2) and is recorded here for completeness, not because
anything in this directory calls it.

`spikes/s2/scripts/run-all.sh` checks for `sfdisk` before doing anything
else and fails with the exact install command if it is missing, rather than
letting `build-fixture.sh` fail later with an unexplained `sfdisk: command
not found` — see "Run from the host" below.

## Scripts

- `scripts/lib.sh` — shared helpers, sourced by every other script here.
  `s2_partition_disk` builds Unraid's documented "MBR: 4K-aligned" layout
  (single partition starting at sector 64) on an image file and exposes the
  partition as its own loop device via `losetup --offset/--sizelimit` (major
  7 — no partition-scanned device node is ever involved, per the
  orchestrator's own note about the lab's device-cgroup restriction).
  `s2_fresh_blkid_cache` deletes this container's `blkid` cache before every
  UUID resolution — see "A lab-environment pitfall" below.
- `scripts/build-fixture.sh` — builds three data disks (`disk1`-`disk3`),
  seeds Unraid-style share directories that span multiple disks (`media`,
  `documents`) and single-disk shares (`backup`, `isos`), records every
  file's sha256 in `manifest.sha256` keyed by its eventual mergerfs-union
  path, then cleanly unmounts and detaches each disk — "unplugged" until
  adoption.
- `scripts/check-fixture.sh` — the migration-scan proxy. Per disk: an
  `xfs_repair -n` dry run, a comparison of a plain `-o ro` mount against a
  `-o ro,norecovery` mount (each bracketed by a whole-partition byte
  snapshot taken immediately before and after — this is what found that
  plain `ro` isn't byte-for-byte silent even on a clean log, doc 08 §2).
  Where the two snapshots differ, `cmp -l` counts and locates every
  differing byte (`lib.sh`'s `s2_byte_diff_ranges`) and each contiguous
  range is classified against the disk's own superblock and internal-log
  location, read once read-only via `xfs_db -r` (`s2_xfs_log_geometry`,
  `s2_classify_ranges`) — so "N bytes changed, in the superblock and the
  log" is always a claim checkable against this run's own committed output,
  never asserted free-hand. Also: a write-attempt refusal check, and a
  second `xfs_repair -n` after the mount. Then unions all three disks
  read-only with mergerfs (`ro,norecovery`, `mspmfs`) and diffs a fresh
  sha256 walk of every file in the union against `manifest.sha256`.
- `scripts/negative-controls.sh` — three controls proving the checking
  mechanisms themselves can fail, not just that they happened to pass: a
  manifest with one deliberately wrong sha256 character must fail the diff;
  mounting a UUID that does not exist must fail outright; a disposable
  scratch disk (never one of the three fixture disks) must mount `rw` and
  accept a write with no restriction.
- `scripts/dirty-log.sh` — the case `build-fixture.sh`'s disks don't cover:
  a disposable disk is written to, `sync`ed, written to again (left
  unsynced), then crashed with `xfs_io -x -c shutdown` while still mounted
  — the same mechanism `xfstests` uses to simulate an unclean shutdown
  without touching a real device. `xfs_repair -n`, a plain `-o ro` mount and
  a `-o ro,norecovery` mount are then each tried against their own **fresh
  copy** of the crashed image (necessary because the first finding below
  showed a plain `ro` mount silently repairs the disk it mounts, which would
  have contaminated a shared image across variants). Uses the same
  `s2_byte_diff_ranges`/`s2_classify_ranges` instrumentation as
  `check-fixture.sh`.
- `scripts/run-all.sh` — runs the four scripts above in order, failing
  loudly (`set -euo pipefail`) on the first non-zero exit. Checks for
  `sfdisk` before doing anything else and fails with the exact fix if it is
  missing (see "Prerequisite" below).

## Prerequisite: `sfdisk`

`sfdisk` ships in Debian's *`fdisk`* package, not `util-linux` — it is not
in the base lab image (`scripts/devenv/Dockerfile`) and must be installed
in the lab container before running anything here, once per lab (spike-only
per-container install, per CLAUDE.md's Spikes rule — never baked into the
base image):

```
docker compose -f docker-compose.dev.yml -p hoserva-lab-<id> exec -T lab \
  bash -c 'apt-get update && apt-get install -y --no-install-recommends fdisk'
```

If this step is skipped, `run-all.sh` fails immediately with the same
install command rather than an unexplained `sfdisk: command not found`
partway through `build-fixture.sh`.

## Run from the host

Against your own lab id, once it is up (`make lab-up HOSERVA_LAB_ID=<id>`)
and the prerequisite above is installed:

```
docker compose -f docker-compose.dev.yml -p hoserva-lab-<id> exec -T -e HOSERVA_LAB_ID=<id> lab \
  bash /src/spikes/s2/scripts/run-all.sh
```

## A lab-environment pitfall found and fixed while building this

This container has no `udevd`, so nothing invalidates `blkid`'s persistent
cache (`/run/blkid/blkid.tab`) when a recycled `/dev/loopN`'s backing image
changes. The cache is keyed by device number: a UUID that resolved and
mounted correctly in one round of `check-fixture.sh` resolved to a stale
value from a *different* image in a later round against the same disk, once
a loop number had been detached and reattached to something else in
between. Confirmed by inspecting `/run/blkid/blkid.tab` directly mid-run.
Fixed in `lib.sh`'s `s2_fresh_blkid_cache`, called after every fresh
`losetup` attach and before any UUID resolution or `UUID=` mount. Not
Unraid-specific — a caveat for any future test environment without a real
init system, including a real `hoserva migrate scan` implementation's own
test suite.

## The central finding: plain `-o ro` is not byte-for-byte silent

`check-fixture.sh` found that `mount -t xfs -o ro` — with nothing written
through it, on a disk that had already been unmounted cleanly, and with
`xfs_repair -n` reporting clean both before and after — rewrote 4390, 4395
and 4387 bytes on `disk1`, `disk2` and `disk3` respectively, and every one
of those bytes falls inside the primary superblock's first sector or the
internal log region (offset 167784448-234893311 on every fixture disk, all
three built identically) — never elsewhere, per the disk-by-disk `cmp -l`
ranges in `results/check-fixture.log`. These exact counts are a property of
this run, this kernel and this 320 MiB fixture layout, not a fixed
constant — re-running this script reproduces the qualitative result
(superblock + log only, on a cleanly-unmounted disk) but not necessarily
these exact numbers. `mount -t xfs -o ro,norecovery` left every disk's raw
bytes byte-for-byte identical (0 differing bytes) across a mount plus a
full read of every file. `dirty-log.sh` reproduces the same asymmetry on a
disk with a genuinely dirty log (4399 differing bytes replaying it via a
plain `ro` mount — this time *not* confined to the superblock and log,
since a real transaction replay also writes its committed changes to their
permanent on-disk locations), where `norecovery` also matters for a
different reason: it can hide files that a `sync` had already made durable
before a crash, not only guard against ones that were genuinely lost — see
doc 08 §2 for the full account. Every mount in this spike, from the point
this was found onward, uses `norecovery`.

## Restarts and anomalies

`build-fixture.sh` needed one correction before its first successful run
(xfsprogs 6.13 refuses to format below ~300 MB; the image size was raised
from 256 MiB to 320 MiB) and `check-fixture.sh`/`dirty-log.sh` each went
through iteration while being written (the blkid-cache bug above, and
restructuring `dirty-log.sh` to use a fresh image copy per mount variant
once the first variant was found to mutate the disk it mounted) — normal
script development, not a restart of a completed run.

The committed `results/` are from a fix-round re-run, 2026-09-16, done to
close two verification findings against the prior run: the byte-diff
instrumentation above did not exist yet (only a whole-partition sha256
before/after, with an unsubstantiated specific byte count and location
asserted in prose), and `run-all.sh` had no `sfdisk` prerequisite check.
Both fixes were confirmed from a fresh `make lab-up`: first, running
`run-all.sh` with no packages installed, which failed immediately with the
prerequisite message above (not a bare `command not found`); then,
installing `fdisk`/`gdisk` and running `run-all.sh` once, end to end, which
is the single run `results/` is from (all four steps —
`build-fixture.sh` → `negative-controls.sh` → `check-fixture.sh` →
`dirty-log.sh` — via one `run-all.sh` invocation, output split into the
files below on the host side afterward), all passing, with the lab left
with no stray loop devices or mounts afterward (`losetup -a` /
`mount | grep s2` checked directly) and `$LAB/s2/` removed from inside the
container before `make lab-destroy`.

## Contents of `results/`

- `build-fixture.log` — the fixture build (three disks, partition/mkfs/seed
  lines).
- `manifest.sha256` / `found.sha256` — the fixture's expected sha256 per
  file (keyed by mergerfs-union path) and what `check-fixture.sh` actually
  found in the union; byte-identical, which is the acceptance result itself
  (`diff manifest.sha256 found.sha256` — empty). Every file's content is
  freshly random per run (`build-fixture.sh` seeds from `/dev/urandom`), so
  these two files' actual hash values differ run to run; what stays
  invariant, and is the thing being tested, is that the two files always
  match each other.
- `negative-controls.log` — all three controls, each asserted to fail (or
  succeed) as expected.
- `check-fixture.log` — every disk's `xfs_repair -n` output (before and
  after), the plain-`ro`-vs-`ro,norecovery` byte-level comparison — the
  `cmp -l` differing-byte count and every contiguous offset range,
  classified against that disk's own superblock/log location — and the
  final union/verify pass.
- `dirty-log.log` — the crashed disk's `xfs_repair -n` output, and each
  mount variant's exit status, directory listing and byte-level comparison
  (same `cmp -l`-derived ranges as `check-fixture.log`) against its own
  fresh copy of the crash.
