# Spike S7 — fanotify change journal, agent-run confirmation

Findings are in `docs/internal/08-spike-findings.md` (Spike 7 section). This
directory holds the experiment code (`cmd/journal`), the shell scripts that
drive it in the lab, and the raw output of the one run those findings are
based on.

## `cmd/journal` — the throwaway listener

Standard library only (`CLAUDE.md`: never touch `go.mod`/`go.sum` for a
spike). `golang.org/x/sys/unix` provides `Fanotify*`, `FAN_*`; this instead
hand-declares the two syscalls (`fanotify_init`=300, `fanotify_mark`=301 —
the x86-64 syscall table numbers, so this binary is amd64/linux-only, stated
here rather than generalised), the flags, and the on-wire
`fanotify_event_metadata`/`fanotify_event_info_*` structs straight from
`linux/fanotify.h`.

One process places one `FAN_MARK_FILESYSTEM` mark on one path (`-mark`), and
prints one TSV line per event to `-out`: `<ts_ns>\t<kind>\t<fid_hex>\t<name>`.
`<kind>` is `CREATE`/`DELETE`/`MODIFY`/`ATTRIB`/`CLOSE_WRITE`/`RENAME_OLD`/
`RENAME_NEW`/`OVERFLOW`/`STOPPED`. `<fid_hex>` is the directory's `fsid` +
file-handle bytes, hex-encoded — **not a path**: this lab's container has
`CAP_SYS_ADMIN` but not `CAP_DAC_READ_SEARCH`, so `open_by_handle_at(2)`
(needed to resolve a file handle back to a path) fails `EPERM` (established
by spikes S1/S6). `<fid_hex>` plus `<name>` is the nearest honest proxy for
"which file, in which directory" this lab can produce; the production
daemon runs as root with the resolving capability, but that is **untested
here**, not assumed.

`-unlimited` sets `FAN_UNLIMITED_QUEUE`. `-ready-file` writes once the mark
is installed (a driver script's synchronisation point). `-delay` sleeps
after signalling ready and before the first read — used to build an
undrained backlog for the overflow experiment. `-duration` exits
automatically. TSV lines are flushed immediately, not buffered to exit, since
this spike kills the listener by PID mid-workload and a buffered line lost to
`SIGTERM` would misreport what was actually captured.

**One real bug found and fixed while building this, worth recording since it
generalises beyond this spike:** a `fanotify_init` fd created *without*
`FAN_NONBLOCK` is, empirically, never treated by `os.NewFile` as pollable —
so a `file.Close()` called from a different goroutine (the signal handler)
does **not** interrupt an in-flight blocking `Read()` the way it does for a
socket or a pipe; the listener hung forever after `SIGTERM` (confirmed:
`/proc/<pid>/stat` still showed a live, non-zombie process 5s after the
signal). Fixed by adding `FAN_NONBLOCK` to the `fanotify_init` flags — this
makes `os.NewFile` integrate the fd with the runtime's netpoller, and
`file.Close()` from the signal/duration goroutine then correctly unblocks the
read with an error, letting the main loop exit and log a `STOPPED` line. See
the comment at the call site in `cmd/journal/main.go`.

## Scripts

Run in order by `scripts/run-all.sh` against a freshly built standing lab
array (`make lab-up`, default 1G/1G/512M geometry — plenty for the
many-small-files workloads here, so no custom `LAB_DATA_SIZE` was needed).
Each script assumes the specific state the one before it left, and is **not**
safe to re-run individually against an already-modified array — see "A
SnapRAID guard this spike's own re-runs tripped, and the fix" below for what
happens if you do.

- `scripts/lib.sh` — shared helpers. Reuses `scripts/devenv/lib.sh`'s
  `lab_require_id`/`lab_assert_own_loop` (never reimplements the
  loop-device-ownership discipline). `s7_create_disk` mirrors
  `create-array.sh`'s own `create_disk()` but parametrised by filesystem
  (needed for the ext4/btrfs fixtures). `s7_start_journal`/`s7_stop_journal`
  launch the binary as a backgrounded job (`cmd & pid=$!`) and kill exactly
  that PID, confirming via `/proc/<pid>/cmdline` first — never a name or
  pattern match. `s7_distinct_count` is the comparison primitive: distinct
  `(fid, name)` pairs in a TSV log, `awk`+`sort -u`+`wc -l`, nothing by eye.
  `s7_diff_changed_count` sums `snapraid diff`'s own summary block
  (`added+removed+updated+copied+restored`, excluding `moved` — S5 already
  found this lab's SnapRAID can never classify a move as `moved` at all, so
  it is always 0 here, and summing it anyway would silently hide a bug where
  it wasn't).
- `scripts/01-setup.sh` — installs `snapraid` (pinned to `12.4-1`, the same
  version S5 already validated in this lab — not a fresh, unvetted pin),
  `e2fsprogs`, `btrfs-progs`; writes `snapraid.conf` (content on `parity1`
  and `cache`, data `d1`-`d3` — same shape as S5's own config); seeds
  baseline files direct and through the pool; adds untouched filler files
  per disk (see below); runs the first `sync`.
- `scripts/02-workload-compare.sh` — the acceptance test itself: one journal
  per data disk, a workload covering every operation the issue names
  (create, overwrite, append, delete, rename within and across directories,
  copy, touch — each both direct and through the mergerfs pool), then
  `snapraid diff`, then the two totals side by side.
- `scripts/03-negative-control.sh` — the methodological control the dispatch
  asked for: one change made with no listener running, and an assertion that
  the comparison actually notices (`journal_count < diff_count`), not just
  that it happens to agree when nothing is wrong.
- `scripts/04-restart-remount.sh` — a listener restart mid-workload (part A)
  and a filesystem unmount+remount with the *same* listener process kept
  running throughout (part B), each measured against a `snapraid diff` of
  the whole window.
- `scripts/05-overflow.sh` — reads `/proc/sys/fs/fanotify/max_queued_events`
  (never lowered — `/proc/sys` is read-only in this lab), floods a burst
  comfortably larger than it while the listener is deliberately not yet
  reading (`-delay`), and a same-size `-unlimited` control run.
- `scripts/06-ext4-btrfs.sh` — ext4 and single-device btrfs data disks
  (Q23), plus a btrfs subvolume mounted separately from its root (the EXDEV
  question from the issue's own upstream research).
- `scripts/run-all.sh` — runs all six above in order, failing loudly on the
  first non-zero exit; cleans up this spike's own `$LAB/s7` working
  directory at the end (not touched by `destroy-array.sh`, which only ever
  removes `$LAB/img` and `$LAB/mnt`).

## Run from the host

```
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o spikes/s7/bin/journal ./spikes/s7/cmd/journal
make lab-up HOSERVA_LAB_ID=<id>
docker compose -f docker-compose.dev.yml -p hoserva-lab-<id> exec -T lab \
  bash /src/spikes/s7/scripts/run-all.sh
make lab-destroy HOSERVA_LAB_ID=<id>
```

The binary is built on the host, inside the workspace, and run inside the
container straight from the read-only `/src` bind mount (`/src/spikes/s7/bin/
journal`) — never copied under `/lab/<id>` (not needed: the read-only mount
already makes it executable in the container). It is a build artifact, not
committed to git.

## A SnapRAID guard this spike's own re-runs tripped, and the fix

While iterating on the journal's signal-handling bug (above), an earlier,
broken build of the listener was run against `02-workload-compare.sh` twice
in a row without resetting the lab array in between. The second attempt's own
`sync` refused: `WARNING! All the files previously present in disk 'd2' ...
and disk 'd3' ... are now missing or have been rewritten!` — SnapRAID's own
built-in safety check (`../reference/snapraid/cmdline/scan.c`,
`count_equal == 0`), separate from Hoserva's own threshold guard (doc 02 §2):
it refuses a sync where **not one single file** on a disk stayed exactly
unchanged. This spike's fixture disks originally carried only 2-5 tracked
files each; the workload's own operation list touches nearly all of them in
one pass by design, so at this tiny scale it looked, to SnapRAID, exactly
like a disk that had been swapped or restored from a stale backup. Fixed by
adding six never-touched filler files per disk in `01-setup.sh` — a few
untouched files is realistic for any real workload; a 2-5-file array is not,
and the filler exists so the fixture's own small scale doesn't trip a real
disk's genuine safety net. Recorded here because it is a real, general
SnapRAID behaviour worth knowing, not an artifact specific to fanotify or to
this journal.

Every result in `results/` is from one final, clean, end-to-end
`scripts/run-all.sh` invocation (`results/run-all.log`), against a freshly
rebuilt array, after the above and the `FAN_NONBLOCK` fix were both in
place — not a patchwork of the individual debugging runs above.

## Contents of `results/`

- `run-all.log` — the full, single `run-all.sh` invocation this spike's
  findings are based on: every step's own log lines, every `snapraid`
  command and its full output and exit code, and the raw per-disk journal
  TSV logs (`cat`ted inline by each script), for all six steps in order.
- `versions.log` — `snapraid`, `e2fsprogs`, `btrfs-progs`, `xfsprogs` dpkg
  versions and the kernel string, extracted from `run-all.log`'s own header.
