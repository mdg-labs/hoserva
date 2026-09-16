# Spike S9 — hosted CI runners, issue #10

Findings are in `docs/internal/08-spike-findings.md` ("Hosted CI runners"
subsection under Spike 9). This directory holds the probe workflow's
scripts and the raw evidence this run's findings are checked against.

**This is fix attempt 4.** Attempt 1 landed (`.github/workflows/s9-hosted-probe.yml`
+ this directory) before the workflow had ever run. The maintainer then
triggered it once (run `35049304081`, `workflow_dispatch`, `main`,
2026-09-16) — its real evidence is recorded here for the first time; a
rejected attempt 2 recorded it but its own fix for the KVM half was never
actually validated under the privilege boundary it needed, so it never
landed. Attempt 3 rewrote and validated the fix for both bugs run
`35049304081` exposed and landed it (`1388f83`); the maintainer then
triggered it a second time (run `35056076616`, `workflow_dispatch`, `main`,
2026-09-16) — **the KVM half is now confirmed hosted, for real**, and the
lab job exposed two more, narrower bugs, both fixed by this attempt. What
follows is the current, cumulative state: what is settled, what this
attempt fixed, and what is still open.

## KVM: CONFIRMED on a hosted runner (run 35056076616)

`gh run view 35056076616 --repo mdg-labs/hoserva --job 104666440639 --log`,
excerpted in `results/ci-run-35056076616-kvm-job.log`. On the pinned
`ubuntu-24.04` (image `20260907.300.1`, `nproc: 4`, Azure `eastus2`), the
`kvm` job succeeded end to end: `/dev/kvm` went `crw-rw----` →
`crw-rw-rw-` after the udev rule (as run `35049304081` already showed), the
kernel-acquisition fix worked on a real runner (`## Kernel acquired:
/boot/vmlinuz-6.17.0-1022-azure -> ... (runner:runner 644)` — no "no
readable kernel" failure this time), and the guest booted with KVM
acceleration confirmed **by the host process**, not only inferred from the
guest: `## QMP query-kvm reply: {"return": {"enabled": true, "present":
true}}`, followed by `PASS: guest booted to its own init in 1s; ... KVM
acceleration confirmed by the host (QMP), not only inferred from the
guest`. **This closes S9's KVM acceptance criterion**: "A QEMU guest boots
with KVM acceleration on a standard hosted runner."

This says nothing about S10 (nested KVM, VM-in-VM) — that is a different,
still-open question, out of this spike's scope.

## The lab job failed at the AppArmor step — two more bugs, now fixed

The same run's `lab` job (`gh run view 35056076616 ... --job 104666440441
--log`, excerpted in `results/ci-run-35056076616-lab-job.log`) failed
before ever reaching `lab-up`. With the previous attempt's `mknod` fix
confirmed working (`losetup` no longer failed), the control branch reached
`mkfs.xfs` for the first time and hit the next latent bug:

**Bug A — the probe images were too small.** `truncate -s 64M`
(previously lines 87 and 97), but `mkfs.xfs` refuses anything under
~300MB: `Filesystem must be larger than 300MB.` The step's own control
guard worked exactly as designed — it caught the broken recipe and voided
the AppArmor verdict instead of asserting one:

```
FAIL: the control itself failed with apparmor=unconfined set — the probe recipe (mknod/loop/xfs mechanics) is broken, not an AppArmor result. This step's AppArmor verdict is VOID.
```

Fixed by sizing both images at 512M instead of 64M — matching the lab's
own `LAB_CACHE_SIZE` convention (`scripts/devenv/create-array.sh`) and
validated directly against xfsprogs in a disposable container, no loop
device involved (`results/mkfs-xfs-size-check.log`): 256M still refuses,
300M is the first size that succeeds, 512M passes with margin.

**Bug B — teardown ran with an empty lab id.** `lab-destroy` (`if:
always()`) uses `steps.lab.outputs.id`, but "Derive this run's lab id" sat
*after* the AppArmor step. When the AppArmor step failed, the id was never
set, and `lab-require-id` correctly refused the empty value — a second,
unrelated failure on top of the first:

```
HOSERVA_LAB_ID:
set HOSERVA_LAB_ID (e.g. HOSERVA_LAB_ID=dev make lab-up)
make: *** [Makefile:90: lab-require-id] Error 1
```

Fixed by moving "Derive this run's lab id" to before the AppArmor step, so
`lab-destroy` always has a real id — `make lab-destroy` already treats "no
such container" as a no-op (see the Makefile's `lab-destroy` target), so
this is a safe no-op on a run where `lab-up` never happened.

**Neither bug is an AppArmor result.** AppArmor necessity on a hosted
Ubuntu runner is still unanswered; the lab job never reached
`lab-up`/`lab-seed`/`lab-verify-refusal`/the SnapRAID sync in this run —
those results still stand on run `35049304081`, not this one.

## What run 35049304081 established (still standing)

### The lab job — success, with one previously-claimed result now retracted

`gh run view 35049304081 --repo mdg-labs/hoserva --job 104645999596 --log`,
excerpted in `results/ci-run-35049304081-lab-job.log`. On the **pinned**
`ubuntu-24.04` (image `20260907.300.1`) — closing the "ran on
`ubuntu-latest`, not the pinned image" gap the previous dev-host-only
finding (doc 08, "Spike 9") had left open — `lab-up`/`lab-seed`/
`lab-verify-refusal` succeeded exactly as the earlier push-triggered run
already showed (mergerfs pool at `.../mnt/user`, both host-device-refusal
checks passing with EPERM), **and the SnapRAID-sync step this issue added
succeeded on a hosted runner for the first time**: `snapraid 12.4-1`
installed, `sync` exited 0, `Everything OK`, the same `WARNING! UUID is
unsupported` line S5 already explained (no `udevd` in a non-privileged
container). `lab-destroy` reported the lab destroyed. **This closes two
of doc 08's named gaps for real** — pinned image, hosted SnapRAID sync —
not just on the dev host.

**The AppArmor step's own verdict from that run is retracted, not
carried forward.** Its log reads:

```
losetup: /probe.img: failed to set up loop device: No such file or directory
losetup: device node /dev/loop0 (7:0) is lost. You may use mknod(1) to recover it.
without-unconfined: FAILED — apparmor=unconfined is required on this runner image
```

— but the **with**-unconfined branch (the control) failed with the
identical message. The probe never `mknod`-ed the container's own
`/dev/loopN` nodes the way the real lab's `scripts/devenv/entrypoint.sh`
does, so both branches failed for a reason that has nothing to do with
AppArmor, and the step still printed a confident "required" verdict
anyway. **A control that fails cannot support a conclusion about the
thing being varied — this run answers nothing about AppArmor necessity,
and the doc 08/doc 13 entries below say so plainly instead of repeating
the old wording.** The workflow's AppArmor step is fixed (`mknod` inline,
a fresh image per branch, the with-unconfined branch now the experiment's
control — if the control itself fails, the step aborts loudly instead of
asserting a verdict) but, like the rest of this commit's fixes, has not
yet run on a hosted runner.

### The kvm job — failure, and the actual bug is now understood and fixed

`gh run view 35049304081 --repo mdg-labs/hoserva --job 104645999427 --log`,
excerpted in `results/ci-run-35049304081-kvm-job.log`. It established,
before failing:

- `/dev/kvm` exists, group `kvm`, and goes `crw-rw----` → `crw-rw-rw-`
  after GitHub's documented udev rule, reload, trigger, settle — **the
  udev mechanism works on this runner image.**
- The CPU reports `svm` — the underlying hardware exposes virtualization
  extensions.
- `nproc: 4`, `15Gi` RAM.

Then it failed with `no readable kernel at /boot/vmlinuz-6.17.0-1022-azure
— cannot boot a guest`, **before `qemu-system-x86_64` was ever invoked.**
The KVM acceleration question was never actually tested. The cause: that
file is the runner's own real, currently-running kernel — it exists, but
at mode `0600`, owned `root:root`, unreadable by the plain runner user.
A now-rejected attempt 2 tried installing a *fresh* kernel package
instead of fixing the read; that would have failed identically, since
`linux-image-generic`'s own postinst leaves its `vmlinuz` at the same
0600 root:root (reproduced independently — see the finding attempt 2 was
rejected for, in the issue thread).

## The kernel-acquisition fix, and how it was validated (attempt 3, now confirmed hosted by run 35056076616 above)

`spikes/s9/scripts/kvm-boot-check.sh` now acquires the kernel by copying
the **running** kernel out with `sudo cp` and `chown`-ing the copy to the
calling user — the runner's own passwordless, job-scoped `sudo`, already
proven to work earlier in the same job (the udev-rule step used it
successfully in run 35049304081 itself). No extra kernel package, no
version mismatch, and the read happens under the exact non-root boundary
the guest-boot step uses.

**Validated under that boundary, not assumed** — `results/kernel-acquisition-nonroot.log`:
a disposable `ubuntu:24.04` container, a real unprivileged user with
passwordless `sudo` (mirroring the runner), a root-owned 0600 file
standing in for the runner's real vmlinuz, and the readability check run
*as that user, without sudo*, both before the fix (`UNREADABLE`, matching
the real failure) and after it (`sudo cp` + `sudo chown` + `chmod`,
`READABLE-AFTER-FIX`). This is the exact test attempt 2 was rejected for
skipping — its own validation ran the whole sequence as root, which can
read anything regardless of permission bits and so could not have caught
the bug.

Acceleration evidence no longer rests solely on the guest's own
`/proc/cpuinfo` self-report. `-accel kvm` (a single value, no `,tcg`
fallback list) makes QEMU refuse to start at all if KVM is unavailable,
and the script also queries the QEMU process directly over QMP
(`query-kvm`), asserting `{"enabled": true, "present": true}` — evidence
that comes from the host process, not only from what the guest reports.
Both checks run; either failing fails the script loudly, by name.

**Optionally, and clearly labeled as such**, the boot/QMP *mechanics*
were validated end to end under pure TCG emulation in a disposable
container, no `/dev/kvm` mapped, `HOSERVA_S9_ACCEL=tcg` (an override the
real workflow never sets) — `results/tcg-plumbing-check.log`. This proved
the kernel-acquisition, initramfs, and QMP-handshake code all work
together and **caught a real bug in the process**: the first version
computed the QMP result with `jq -r '.return.enabled // "missing"'`,
and jq's `//` treats a literal `false` the same as null/absent, so a
genuine "not accelerated" answer printed as `"missing"`. Fixed and
re-validated; the log shows the corrected output. **This TCG run proves
the plumbing works. It proves nothing whatsoever about KVM acceleration
on any real host** — that still requires an actual hosted run with
`/dev/kvm`.

## What remains open, and exactly how to close it

**KVM acceleration on a hosted runner is now established** — run
`35056076616`'s `kvm` job, above. This is settled; no further run is
needed for this specific question. It says nothing about S10 (nested KVM,
VM-in-VM), which is a different, still-open question outside this spike.

**AppArmor necessity on a hosted Ubuntu runner is still not established.**
Both prior verdicts are void: run `35049304081`'s (both branches failed
identically on the `mknod` bug) and run `35056076616`'s (both branches
would have failed identically on the size bug — the run recorded a `FAIL`
on the control before the variant branch ever executed). The fixed step
(512M images, id derived first) has not yet run hosted.

After this commit is reviewed and pushed to `main`, the maintainer runs,
in fish:

```fish
gh workflow run s9-hosted-probe.yml --repo mdg-labs/hoserva --ref main
```

then reads the `lab` job's log — the `kvm` job's question is already
closed and does not need re-triggering for S9's sake, though it will run
again as part of the same workflow dispatch. A pass looks like:

- the AppArmor step's control (`with-unconfined`) succeeds against a 512M
  image — if it doesn't, the step itself says the verdict is void and the
  run needs investigating before trusting either branch's result; the
  variant (`without-unconfined`) then reports genuinely whether the flag
  is needed;
- `lab-up`/`lab-seed`/`lab-verify-refusal`/the SnapRAID sync succeed
  exactly as run `35049304081` already showed;
- `lab-destroy` succeeds regardless of how the AppArmor step went, because
  the lab id is now derived before it.

If the AppArmor step still fails for a reason unrelated to AppArmor, its
own log names which specific check failed — update
`docs/internal/08-spike-findings.md`'s S9 section and Q42/Q79 (doc 13)
with the actual result, whichever way it goes, rather than leaving this
section's "still open" framing standing. If it produces a real verdict,
record it there and close out this spike's one remaining open question.

## Files

- `scripts/hosted-snapraid-check.sh` — host-side orchestration (runs on
  the CI runner or a dev host); unchanged this round, already confirmed
  on a hosted runner (run 35049304081)
- `scripts/snapraid-sync-in-container.sh` — runs inside the lab
  container, invoked by the above; unchanged this round
- `scripts/kvm-boot-check.sh` — the guest-boot check the `kvm` job runs;
  unchanged since attempt 3, now **confirmed against a real `/dev/kvm`**
  by run `35056076616`
- `results/ci-run-34950031773-lab-job.log` — the earlier push-triggered
  `ci.yml` run this spike originally cited for the lab half (pre-dates
  this issue's own workflow)
- `results/ci-run-35049304081-lab-job.log`,
  `results/ci-run-35049304081-kvm-job.log` — the first `workflow_dispatch`
  run of this issue's own workflow, both jobs, the AppArmor verdict it
  produced and why that verdict is retracted, and exactly where the KVM
  job died (the kernel-read bug, since fixed and confirmed)
- `results/ci-run-35056076616-lab-job.log`,
  `results/ci-run-35056076616-kvm-job.log` — the second `workflow_dispatch`
  run, on `1388f83`: the `kvm` job's real PASS with QMP evidence, and the
  `lab` job's AppArmor step failing on the `mkfs.xfs` size bug plus the
  resulting empty-lab-id teardown failure, both fixed by this commit
- `results/mkfs-xfs-size-check.log` — the xfsprogs minimum-size threshold,
  validated against a plain file in a disposable container, no loop
  device involved; the basis for the 512M fix
- `results/kernel-acquisition-nonroot.log` — the non-root privilege
  boundary validation for the kernel-acquisition fix (attempt 3, now
  confirmed hosted)
- `results/tcg-plumbing-check.log` — the optional pure-TCG plumbing
  validation, and the jq bug it caught
- `results/local-snapraid-sync-check.log`, `results/local-versions.log` —
  this spike's earlier dev-host validation of the SnapRAID-sync scripts
