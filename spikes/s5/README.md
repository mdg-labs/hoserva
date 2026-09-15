# Spike S5 — SnapRAID fidelity on loop devices, agent-run confirmation

Findings are in `docs/internal/08-spike-findings.md` (Spike 5 section). This
directory holds the experiment scripts and the raw output of the run that
section is based on.

## What ran, and where

Lab `6-a1` (loop-device lab, doc 06 §3), built with **2 GiB data disks and a
3 GiB parity image** (`LAB_DATA_SIZE=2G LAB_PARITY_SIZE=3G`, the issue's own
recommended default) rather than the harness's usual 1 GiB — large enough
that SnapRAID's default 256 KiB blocksize produces a real multi-block, multi-
extent layout, small enough that the full six-step run (below) completes in
well under a minute. `LAB_DATA_DISKS` (3) and `LAB_CACHE_SIZE` (512 MiB) kept
their defaults.

Brought up with `docker compose … exec -e LAB_DATA_SIZE=2G -e
LAB_PARITY_SIZE=3G -e LAB_CACHE_SIZE=512M lab bash
/src/scripts/devenv/create-array.sh` directly, per the dispatch's own
instructions for a non-default geometry, rather than `make lab-up` — see
"A `make lab-up` / `.lab/` ownership gap found while tearing down" below for
the one consequence that had.

`snapraid` is not in the base lab image (`scripts/devenv/Dockerfile`, per
CLAUDE.md's Spikes rule — never baked in for a spike). Installed in the lab
container for this spike only:

```
apt-get update && apt-get install -y --no-install-recommends snapraid
```

Installed version, read from package metadata, not `--version` (doc 08 S9's
own lesson — Debian's build reports `vnone`): **`snapraid 12.4-1`**
(`dpkg-query -W -f '${Version}' snapraid`) — exactly the reference checkout's
own tag, `../reference/snapraid` at `v12.4`. `results/versions.log`.

A second parity disk (`parity2`, 3 GiB, matching `parity1`) and every
replacement disk the reconstruction steps need are created the same way
`scripts/devenv/create-array.sh`'s own `create_disk()` does — reused, not
reimplemented, via `spikes/s5/scripts/lib.sh`'s `s5_create_disk` — as images
under the *same* `$LAB/img/` and mounts under the *same* `$LAB/mnt/` the
standing array uses, per the orchestrator's own dispatch note. This spike's
own non-image, non-mount files (the config, every log, every manifest) live
under `$LAB/s5/`, which `destroy-array.sh` does **not** clean (it only
removes `$LAB/img` and `$LAB/mnt`) and which this spike removes itself,
from inside the container, before every teardown (see below).

## Scripts

Run in order by `scripts/run-all.sh`, each idempotent-enough for a single
clean pass but **not** safe to re-run individually against an
already-modified array (several steps assume specific pre-state — see each
script's own comments for why, including two things a first version of a
script got wrong and this final version fixes):

- `scripts/lib.sh` — shared helpers, sourced by everything else. Reuses
  `scripts/devenv/lib.sh`'s `lab_require_id`/`lab_assert_own_loop` (never
  reimplements the loop-device-ownership discipline). `s5_snapraid` runs
  `snapraid -c <this spike's conf>`, always printing the exact command and
  its exit code. `s5_snapraid_log` is the safe way to capture a `snapraid`
  invocation's output to a file *and* keep its real exit code, without a
  pipe — see the comment there for why a `snapraid diff | tee file` pattern
  under `set -euo pipefail` is broken (found empirically while writing
  `02-diff-and-sync.sh`, below). `s5_disk_manifest`/`s5_array_manifest`
  produce the sha256-per-file manifests every reconstruction claim in this
  spike is checked against.
- `scripts/01-seed-and-sync.sh` — writes the single-parity `snapraid.conf`
  (content files on `parity1` and `cache` — two distinct non-data devices,
  meeting the manual's "parity disks used (1) + 1" minimum, snapraid.txt
  §7.4), seeds 8 files directly onto `disk1`-`disk3`'s own mount points (not
  through the mergerfs pool — SnapRAID protects the data disks directly, and
  writing through the pool would hand placement to mergerfs's create policy
  instead of this script), and runs the first `sync`.
- `scripts/02-diff-and-sync.sh` — one change of each kind `diff` reports
  (add / update / remove / move / copy, snapraid.txt §5.5), one `diff` to
  observe the classification, one `sync` to commit it.
- `scripts/03-touch-and-copy.sh` — the sub-second-timestamp rule for
  "copied" detection and `snapraid touch` (§5.14), plus a negative control.
  A first version copied a file that was brand-new in the *same* diff pass
  as its source and got "add"/"add" instead of a "copy" pairing on both
  sides — restructured (final version) so every "copy" case's source is
  already committed by a prior `sync` before the copy is made.
- `scripts/04-scrub-and-fix.sh` — a plain sha256-single-byte-flip control
  (the primitive every sha256 comparison in this spike relies on), a clean
  baseline `scrub` (the negative control that scrub reports a genuinely
  uncorrupted array as clean), undeleting (`fix -f`, §4.3), `xfs_bmap`-
  targeted corruption injection (not the doc 06 §3 blind `dd
  bs=1M seek=100 count=10` — see "Loop-device-specific / general findings"
  below), scrub detection, `fix -e` repair, and a final rescrub to clear the
  bad mark.
- `scripts/05-reconstruct-single-parity.sh` — detaches `d2`'s loop device,
  attaches a brand-new empty replacement at the same mount point, then the
  manual's own four-step recovery procedure (§4.4): `fix -d d2`,
  `check -d d2 -a`, `sync`, and a full-array sha256 manifest diff against the
  pre-failure snapshot.
- `scripts/06-dual-parity-reconstruct.sh` — adds a second parity disk and
  content copy, `--force-full sync`s it in (plain `sync` refuses — see
  below), then fails **both** `d1` and `d3` at once (the case single parity
  cannot survive) and reconstructs both with `fix -d d1 -d d3` /
  `check -d d1 -d d3 -a` / `sync`.
- `scripts/run-all.sh` — runs all six above in order on a freshly built
  array, failing loudly on the first non-zero exit. Checks for `snapraid`
  first and fails with the exact install command if missing.

## Run from the host

```
docker compose -f docker-compose.dev.yml -p hoserva-lab-<id> exec -T -e LAB_DATA_SIZE=2G -e LAB_PARITY_SIZE=3G -e LAB_CACHE_SIZE=512M lab \
  bash /src/scripts/devenv/create-array.sh
docker compose -f docker-compose.dev.yml -p hoserva-lab-<id> exec -T lab \
  bash -c 'apt-get update && apt-get install -y --no-install-recommends snapraid'
docker compose -f docker-compose.dev.yml -p hoserva-lab-<id> exec -T -e HOSERVA_LAB_ID=<id> lab \
  bash /src/spikes/s5/scripts/run-all.sh
```

## Loop-device-specific / general findings

**The headline finding: SnapRAID cannot read any data disk's UUID in this
lab, at all, ever — a structural property of the narrowed container, not
something a different test could work around.** Every `sync`, `diff`,
`scrub` and `fix` in every run logged `WARNING! UUID is unsupported for
disks: 'd1', 'd2', 'd3'. Not using inodes to detect move operations.` (16
times across the one committed `run-all.log`; every occurrence).
`../reference/snapraid/cmdline/unix.c` (`devuuid`, `devuuid_dev`) shows
SnapRAID's *only* Linux UUID mechanism is enumerating `/dev/disk/by-uuid`
(with a `libblkid`-based fallback, `devuuid_blkid`, gated behind
`HAVE_BLKID`) — and this lab's Debian `snapraid 12.4-1` binary is not linked
against `libblkid` (`ldd`, `results/versions.log`: no `libblkid.so`
reference), so the fallback does not exist in this build. `/dev/disk/by-uuid`
does not exist in the lab container at all (`ls /dev/disk` → *No such file or
directory*) because nothing runs `udevd` here — the same root cause doc 08's
S2 section already found for `blkid`'s cache going stale, now shown to affect
SnapRAID's own UUID path too, unconditionally, not merely when a device
number is reused. This is not fixable by installing a package in the lab
container: `/dev/disk/by-uuid` is populated by a running udev daemon
reacting to device events, which a non-privileged, non-init container
fundamentally cannot provide.

**Measured consequence 1 — intra-disk moves are never detected as "moved".**
`02-diff-and-sync.sh`'s `mv` of an already-synced file to a different
directory *on the same disk*, preserving its inode and mtime exactly as
snapraid.txt §5.5 says a "moved" file should, was reported as `remove
movies/gattaca.bin` + `copy movies/gattaca.bin -> movies/archive/gattaca.bin`
(`results/diff-exercises.log`), not `moved`. Every diff in every one of this
spike's six scripts shows `0 moved` (`grep -c '0 moved'
results/run-all.log` → present at every diff/sync). Functionally this is
harmless here — `sync` still picks the change up correctly, and the
`copied` classification means the hash is reused rather than the whole file
being rehashed — but it is a real, measured difference from what a real disk
(with a working `/dev/disk/by-uuid`) would report, and it makes this lab
structurally unable to exercise the "moved, zero parity recompute" fast path
snapraid.txt §5.6 describes. **Covering layer: L3** — a real VM with a full
init system and a running `udevd` is required to observe true `moved`
classification and to exercise `-U`/`--force-uuid`'s actual purpose (below).

**Measured consequence 2 — the `-U`/`--force-uuid` disk-identity guard is
never actually exercised by this spike.** snapraid.txt §6 (`-U`) describes
it as the safety check that "detects when your disks are mounted in the
wrong mount points" by comparing each disk's tracked UUID against what is
currently mounted there, and explicitly allows one UUID change under single
parity / more under multiple parity "because it's the normal case of
replacing disks after a recovery" (§6). Because UUID is never tracked here at
all, `05-reconstruct-single-parity.sh` and `06-dual-parity-reconstruct.sh`
both swap a data disk's entire backing filesystem for a brand-new one at the
same mount point and `fix`/`sync` proceed with no complaint of any kind —
which is the *correct* outcome for this spike's single/dual-disk-replacement
case, but it is not evidence that the guard itself works, only that it never
ran. **Covering layer: L3** — the guard's actual trigger (a disk UUID
SnapRAID previously recorded no longer matching what's mounted at that
path, e.g. two data disks' mount points swapped) needs a real UUID source to
test at all.

**A related, non-loop-device-specific finding, still worth recording: doc 06
§3's own corruption-injection example is a filesystem-integrity risk on a
small disk, not just a SnapRAID one.** Its blind `dd bs=1M seek=100
count=10` was never actually run against a mounted, in-use filesystem by
this spike — `04-scrub-and-fix.sh` uses `xfs_bmap -v` to find the exact
physical block range of a specific 2.2 MB target file first, and corrupts
500,000 bytes at a computed offset safely inside that file's own extent
(`results/xfs_bmap-corrupt-target.log`: extent `[0..4303]` → device blocks
`192..4495`, 512-byte units per this filesystem's `sectsz=512`, confirmed via
`xfs_info`). On this run's 2 GiB data disks — far smaller than doc 06 §3's
own 4 TB example — a blind offset in the first 100-110 MB risks landing on
XFS's superblock or an early allocation-group header rather than inside a
file's data, which would produce a filesystem-corruption test, not the
silent-bit-rot test SnapRAID's `scrub` is actually for. This is a general
finding (any small-disk lab run, not specific to this container), recorded
in doc 06 §3.

**A related but separate lab-environment fact, found while corrupting: a
`dd` under a live mount can be served from a stale page-cache read
afterward.** `04-scrub-and-fix.sh` unmounts the target disk, corrupts its
loop device directly, then remounts before reading the file again —
otherwise the immediately-following `sha256sum` risks reading a still-cached
clean page instead of the now-corrupted on-disk bytes, silently turning the
corruption step into a no-op. This is standard Linux page-cache behaviour,
not loop-device-specific, but worth stating because doc 06 §3's own example
does not mention it.

## Two usage details found empirically, not loop-device-specific

- **`fix -f <path>` requires a leading `/`.** `snapraid fix -f
  documents/tax-2025.bin` fails outright (`Invalid filter specification …
  Filters using relative paths are not supported. Ensure to add an initial
  slash`); `snapraid fix -f /documents/tax-2025.bin` (the array-relative
  path) succeeds. snapraid.txt §4.3's own example (`snapraid fix -f FILE`)
  does not show this.
- **Adding a second parity level after data already exists needs
  `--force-full sync`.** A plain `sync` after adding `2-parity` to the
  config refuses (`WARNING! The 2-Parity parity has only 0 blocks instead of
  23. DANGER! …`) and names the fix itself in its own output: `snapraid
  --force-full sync`. `06-dual-parity-reconstruct.sh` uses it; snapraid.txt
  §6's own `-F`/`--force-full` entry does not mention this specific trigger.

## A `make lab-up` / `.lab/` ownership gap found while tearing down

This run's non-default geometry needed `docker compose … exec -e
LAB_DATA_SIZE=2G …`, which the dispatch's own instructions say to run
directly rather than through `make lab-up` (`make lab-up` has no way to pass
environment into the container it starts). `make lab-up`'s own recipe does
`mkdir -p -- ".lab/$HOSERVA_LAB_ID"` **before** starting the container,
specifically so the bind-mount target already exists and is owned by the
host user when Docker starts. Skipping straight to `docker compose … up -d`
(as this run did, and as any future non-default-geometry run following the
same instructions will) skips that `mkdir`, so Docker itself auto-creates
the missing host path — `.lab/6-a1`, and in this run's case `.lab` itself,
since neither existed yet — owned by root. `make lab-destroy`'s final `rm
-rf -- ".lab/$HOSERVA_LAB_ID"` then fails with *Permission denied*, and
`rmdir` fails too, because the *parent* `.lab` directory itself was also
root-owned and the host user has no write permission on it to remove the
entry. Worked around here with a one-off `docker run --rm -v
"$(pwd):/ws" debian:trixie-slim rm -rf /ws/.lab` (confirmed gone
afterward) — the same "remove root-owned bind-mount contents from inside a
container" pattern doc 08's S9 section already established, just needed one
level higher up the path this time. Not fixed here (outside this issue's
declared scope — `scripts/devenv/` and the `Makefile` are `area:devenv`, not
`spikes/s5/` or the two docs this issue's scope covers); flagged in this
run's report as a real gap for whoever picks it up: `make lab-up` should
`mkdir -p .lab/<id>` unconditionally reachable on its own, or the `lab-up`
target should grow a way to forward environment variables so this doesn't
need bypassing at all.

## Restarts and anomalies

Three scripts needed a fix after their first run, each found and fixed while
writing this spike, not after a "completed" run was later found wrong:

- `02-diff-and-sync.sh`: a `snapraid diff | tee file` pipeline under `set
  -euo pipefail` aborted the script immediately after printing `diff`'s
  output (its expected exit 2, under `pipefail`, trips `set -e` on the bare
  pipeline statement, before the following `rc=${PIPESTATUS[0]}` line could
  ever run) — the follow-up `sync` in that script silently never ran. Found
  because the *next* script's diff showed the same still-pending changes.
  Fixed by adding `s5_snapraid_log` (plain redirection, no pipe) to
  `lib.sh`, used from here on; confirmed empirically that `pipeline || true`
  also does not work around it (`f() { return 2; }; f | cat || true; echo
  $PIPESTATUS` prints `0`, not `2`).
- `03-touch-and-copy.sh`: as described above (copy-source-must-already-be-
  synced), and a `grep -qF "-> …"` control whose pattern starting with `-`
  was silently parsed as a `grep` option, so the negative control it
  guarded was never actually checked — fixed with `grep -qF --`.
- `05-reconstruct-single-parity.sh`: a first version's "no unrecoverable
  block" check was `grep -qF "unrecoverable:" fix.log`, which false-matched
  the log's own trailing `summary:error_unrecoverable:0` line (containing
  the substring `unrecoverable:0`) — the *summary*, reporting zero, not a
  failure. Fixed to parse `summary:error_unrecoverable:<N>` specifically and
  assert `N == 0`.

Every result in `results/` is from one final, clean, end-to-end
`scripts/run-all.sh` invocation (`results/run-all.log`), against a freshly
rebuilt array, after all of the above were fixed — not a patchwork of the
individual debugging runs above.

## Contents of `results/`

- `run-all.log` — the full, single `run-all.sh` invocation this spike's
  findings are based on: every `snapraid` command actually run, its full
  output, and its exit code, for all six steps in order.
- `versions.log` — `snapraid`'s dpkg version and `ldd` output (no
  `libblkid`), `xfsprogs`'s dpkg version.
- `snapraid.conf` — the final config (after step 6 adds the second parity
  disk).
- `diff-exercises.log`, `diff-touch-before.log`, `diff-touch-after.log` —
  the `diff` output each classification claim above is drawn from.
- `touch-mtimes-before.log`, `touch-mtimes-after.log` — one file's
  sub-second timestamp (nanosecond `stat` field) immediately before and
  after `snapraid touch`.
- `status-before-corruption.log`, `scrub-after-corruption.log`,
  `status-after-corruption.log`, `status-after-fix.log` — the full
  scrub/corruption/fix cycle's `status` and `scrub` output.
- `xfs_bmap-corrupt-target.log` — the corrupted file's physical extent, the
  raw material the corruption offset is computed from.
- `fix-d2.log`, `fix-dual.log` — SnapRAID's own structured `-l` log format
  for both reconstruction runs (one `entry:`/`error:`/`fixed:` triple per
  repaired block, one `summary:error_unrecoverable:0` trailer each).
- `manifest-after-initial-sync.sha256`, `manifest-after-diff-exercises.sha256`,
  `manifest-after-touch.sha256`, `manifest-after-scrub-fix.sha256`,
  `manifest-before-d2-failure.sha256`, `manifest-after-d2-reconstruction.sha256`,
  `manifest-before-dual-failure.sha256`, `manifest-after-dual-reconstruction.sha256`
  — sha256-per-file snapshots of the array at each checkpoint. Every file's
  content is freshly random per run, so the hash values themselves differ
  run to run; what is invariant, and is the thing being tested, is that a
  pre-failure and post-reconstruction manifest are byte-for-byte identical
  (`d2-reconstruction.diff`, `dual-reconstruction.diff` — both empty, i.e.
  `diff -u` found no difference).
