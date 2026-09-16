# Spike S9 — hosted CI runners, issue #10

Findings are in `docs/internal/08-spike-findings.md` ("Spike 9 — Lab
container (dev host) and Spike 8" section, "Hosted CI runners" subsection).
This directory holds the probe workflow's scripts and the raw evidence this
run's findings are checked against.

**This issue could not be fully closed in one pass.** A `workflow_dispatch`
workflow can only be run on GitHub once it exists on the default branch, and
no agent pushes (`CLAUDE.md`). What follows is split accordingly:

- **Confirmed now**, from a real hosted-runner CI run that already
  happened, plus one gap closed by local dev-host validation.
- **Left for the maintainer**, with the exact command to run and what a
  pass looks like.

## What's confirmed, and from what

### The lab half — loop devices, XFS, the mergerfs pool, teardown

`gh run view 34950031773 --repo mdg-labs/hoserva --job 104318547816 --log`
— the `Loop-device lab (L2)` job of the push-triggered `ci.yml` run on
`main`, 2026-09-15, `conclusion: success`. Full job log fetched read-only
via `gh run view ... --log` (never re-run by this spike — an agent never
triggers a workflow); the runner-image banner and the lab-specific steps
are excerpted in `results/ci-run-34950031773-lab-job.log`.

That job ran `make lab-up` (which builds the lab image, brings up the
container, and runs `scripts/devenv/create-array.sh`), `make lab-seed`,
`make lab-verify-refusal`, then `make lab-destroy` — on the **hosted**
runner `ubuntu-24.04` image `20260907.300.1` (`Ubuntu 24.04.5 LTS`,
Azure `westus3`), with `docker-compose.dev.yml`'s narrowed device access
(`SYS_ADMIN`, `/dev/fuse`, `/dev/loop-control`, `device_cgroup_rules: "b
7:* rmw"`, `security_opt: apparmor=unconfined` — no `--privileged`, no
`/dev` bind mount). Result: the array came up (`parity: .../parity1`,
`data: .../disk1 disk2 disk3`, `cache: .../cache`, `pool: .../user` — a
mergerfs pool mount, confirmed by its own presence in `create-array.sh`'s
success output), seeded, the host-device-refusal checks all passed
(`ok: host NVMe class (major 259) — open refused with EPERM`, `ok: host
SCSI/SATA class (major 8) — open refused with EPERM`, plus both
`lab_assert_own_loop` negative checks), and `lab-destroy` reported the lab
destroyed and removed `.lab/$HOSERVA_LAB_ID` — a clean run, no manual
intervention, no privileged container.

**What this run does *not* prove, stated rather than glossed over (the
orchestrator's own finding, confirmed by reading the log):**

1. **It ran on `ubuntu-latest`, which resolved to `ubuntu-24.04` image
   `20260907.300.1` at that moment** — not the pinned `ubuntu-24.04` this
   issue prefers for reproducibility (Q42, "pinned `ubuntu-24.04`, not
   `ubuntu-latest`"). `ubuntu-latest` can move to a new default image at
   any time GitHub chooses; a re-run next month is not guaranteed to see
   the same image. `ci.yml` itself is out of this issue's scope to change
   (it already works and isn't broken), so this is recorded as a named gap
   rather than fixed here — `.github/workflows/s9-hosted-probe.yml` (this
   issue's own new workflow) pins `ubuntu-24.04` explicitly, closing the
   gap for future runs of *that* workflow.
2. **No SnapRAID sync ran.** `scripts/devenv/Dockerfile` (out of this
   spike's declared scope) does not install `snapraid` — only
   `util-linux`, `xfsprogs`, `mergerfs`, `fuse3`, confirmed directly in the
   job's own `apt-get install` log line. `make lab-up`/`lab-seed`/
   `lab-verify-refusal` never touch SnapRAID. The issue's own proposed
   approach names a SnapRAID sync as part of the lab check — closed below.

### The SnapRAID-sync gap — closed, but only on the dev host, not yet on a hosted runner

`spikes/s9/scripts/hosted-snapraid-check.sh` (host-side orchestration) and
`spikes/s9/scripts/snapraid-sync-in-container.sh` (runs inside the lab
container) install `snapraid` for this spike only — the same "not baked
into the standing image" pattern `spikes/s5` and `spikes/s7` already
use — build a two-content-file config against the standing array's own
`disk1`/`disk2`/`disk3`/`parity1` mounts, and run one `sync`.

Run against the **local dev host's** lab, lab id `10-a1`, 2026-09-16 (this
issue's own dispatch lab id, on the primary dev host — CachyOS, Docker
29.8, no AppArmor, matching doc 08's existing S9 dev-host section, **not**
a hosted runner): `snapraid 12.4-1` installed, `sync` exited 0, printed
`Everything OK`, and showed the same `WARNING! UUID is unsupported for
disks: 'd1', 'd2', 'd3'. Not using inodes to detect move operations.` line
spike S5 already found and explained (no `udevd` in a non-privileged
container — a property of the lab recipe itself, not of this host or a
hosted runner). Full output: `results/local-snapraid-sync-check.log`
(captured via `tee` while the script ran, not reconstructed after the
fact); package versions: `results/local-versions.log`. The lab was
destroyed and confirmed gone afterward (`docker ps -a` empty for the lab's
container name, `.lab/10-a1` removed, `ls -A /run/media/$USER` empty — the
host's automounter hazard this dispatch names did not trigger).

**This closes the mechanism gap (the script works, the commands are
correct) but not the hosted-runner claim** — nothing above ran on a GitHub
runner. `.github/workflows/s9-hosted-probe.yml`'s `lab` job runs the exact
same two scripts on `ubuntu-24.04`; until the maintainer triggers it, "does
a SnapRAID sync succeed on a hosted runner" stays open, even though "does
this exact script work at all" is now closed.

### AppArmor — still not closed to "necessary", only to "present and working"

Doc 08's existing S9 dev-host finding says `apparmor=unconfined` "is still
needed on Debian/Ubuntu hosts (unverified here)" because the dev host
(CachyOS) has no AppArmor at all, so the dev-host run couldn't test it
either way. Run 34950031773 ran on Ubuntu 24.04 (which does have AppArmor)
**with** `apparmor=unconfined` already set unconditionally in
`docker-compose.dev.yml`, and succeeded — that confirms the setting is
compatible with a real Ubuntu-with-AppArmor host, but that run never
varied the flag, so it cannot show whether removing it would have failed
too. `.github/workflows/s9-hosted-probe.yml`'s `lab` job adds an explicit
with/without comparison (a disposable, non-lab `docker run`, mirroring the
raw-command style doc 08's own dev-host S9 check already used) specifically
to close this — result pending the maintainer's run.

## What's left open, and exactly how to close it

**KVM was never touched by this spike.** No workflow existed on GitHub to
run one, and no agent runs `gh workflow run` or pushes (`CLAUDE.md`, this
issue's own dispatch). `.github/workflows/s9-hosted-probe.yml`'s `kvm` job
is written, `shellcheck`-clean (`spikes/s9/scripts/kvm-boot-check.sh`, via
the read-only `koalaman/shellcheck:stable` container) and `actionlint`-clean,
but **has never executed** — its correctness rests on design and public
sources (GitHub's 2024-04-02 changelog entry for the udev rule; this
issue's own upstream research on the late-trigger caveat), not on an
observed run.

After this commit is reviewed and pushed to `main`, the maintainer runs,
in fish:

```fish
gh workflow run s9-hosted-probe.yml --repo mdg-labs/hoserva --ref main
```

then watches it (`gh run watch --repo mdg-labs/hoserva`, or the Actions
tab) and reads both jobs' logs. A pass looks like:

- **`lab` job**: `lab-up`/`lab-seed`/`lab-verify-refusal` succeed exactly
  as run 34950031773 already showed, the "AppArmor necessity check" step
  prints a result for both the with- and without-`apparmor=unconfined`
  cases, and the SnapRAID step ends with `snapraid sync: confirmed (exit
  0, 'Everything OK')`.
- **`kvm` job**: `/dev/kvm` is absent (or present but wrong-permissioned)
  in the "before any udev rule" step, present and `crw-rw-rw-` (or
  group-writable to `kvm`) after the rule is applied and settled, and
  `kvm-boot-check.sh` ends with a line starting `PASS: guest booted to its
  own init, /proc/cpuinfo confirmed a hypervisor`.

If either job fails, its own log names which specific check failed (every
assertion in both scripts prints what it expected before exiting non-zero)
— update `docs/internal/08-spike-findings.md`'s S9 section and Q42/Q79
(doc 13) with the actual result, whichever way it goes, rather than leaving
the "still to check" framing standing.

## Files

- `scripts/hosted-snapraid-check.sh` — host-side orchestration (runs on the
  CI runner or a dev host)
- `scripts/snapraid-sync-in-container.sh` — runs inside the lab container,
  invoked by the above
- `scripts/kvm-boot-check.sh` — the guest-boot check the `kvm` job runs;
  **written, checked with `shellcheck`/`bash -n`, never executed** — no
  `/dev/kvm` access is exercised by this spike (this issue's own dispatch:
  "don't start local VMs for this issue")
- `results/ci-run-34950031773-lab-job.log` — excerpt of the existing hosted
  CI run this spike cites for the lab half
- `results/local-snapraid-sync-check.log`, `results/local-versions.log` —
  this spike's own dev-host validation of the SnapRAID-sync scripts
