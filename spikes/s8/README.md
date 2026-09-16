Spike S8 — Debian 13 dependency coverage. Finishes what doc 08's Spike 9
section recorded as "partial S8" (both packages present in trixie): whether
2.40.2/12.4 actually behave as doc 02 needs, and whether an upstream build
should be carried. Full findings, verdict and the Q7 update: doc 08 §8
("Spike 8 — Debian 13 dependency coverage") and doc 13 Q7.

Lab id `9-a1`. Packages installed for this spike only, beyond the standing
lab image (`scripts/devenv/Dockerfile` ships `mergerfs`; `snapraid` and
`attr` are not in the base image, matching doc 08 S5/S7's own pattern):
`attr 1:2.5.2-3`, `snapraid 12.4-1` (`spikes/s8/scripts/00-install-deps.sh`,
`spikes/s8/results/versions.log` — every version read with
`dpkg-query -W`, never `--version`, doc 08 S9).

## What ran

- `spikes/s8/scripts/01-mergerfs-options.sh` — mounts doc 02 §1's full
  mergerfs option table over the standing lab's `disk1..3` branches (a
  second mount alongside `make lab-up`'s own `user` pool, same pattern S6
  used for its `mspmfs`/`epmfs` comparison) and reads every option's
  *effective* value back through the mount's own runtime control file
  (`getfattr -d <mount>/.mergerfs`, man/mergerfs.1 "RUNTIME INTERFACES"), plus
  `findmnt`/`/proc/self/mountinfo` for `fsname`. Also round-trips all four
  create policies doc 02 §1 names (`mspmfs`, `mfs`, `lfs`, `ff`) through
  `setfattr`/`getfattr`, plus `epmfs` for comparison with S6.
- `spikes/s8/scripts/02-snapraid-force-guards.sh` — builds a minimal
  single-parity SnapRAID 12.4 array over `disk1..3` and deliberately
  triggers the two guards S5 did not exercise: `-Z`/`--force-zero`
  (truncating a previously-non-zero tracked file to zero bytes) and
  `-E`/`--force-empty` (removing every originally-present file on one
  whole disk) — confirming the plain-`sync` refusal, its exact message,
  and that the named override flag succeeds.
- A changelog review of mergerfs 2.40.2 → 2.42.0 (current) and SnapRAID
  12.4 → v14.9 (current), read from each project's own GitHub releases and,
  where a release's own notes were too terse to judge, the actual commit
  diff — `spikes/s8/results/changelog/`.

Raw output for every command: `spikes/s8/results/` (`mergerfs-options.log`,
`getfattr-full.log`, `policy-roundtrip.log`, `findmnt.log`,
`mountinfo.log`, `snapraid-guards.log`, `snapraid.conf` and the individual
`*-sync.log`/`final-status.log` files, `changelog/`).

## Scope note

`minfreespace=50M` (not doc 02 §1's own production default of `50G`) is
used in `01-mergerfs-options.sh` to fit inside this lab's 1 GiB loop-device
disks — the same convention S6 used for the identical reason. The mount's
own reported value (`52428800`, i.e. exactly 50×1024×1024 bytes) confirms
the unit conversion is exact; nothing about the production default value is
tested here (or anywhere in this lab, per doc 06 §3's own scale
disclaimer).
