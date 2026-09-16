# Spike S9 — hosted CI runners, issue #10

Findings are in `docs/internal/08-spike-findings.md` ("Hosted CI runners"
subsection under Spike 9). This directory holds the probe workflow's
scripts and the raw evidence this run's findings are checked against.

**This is fix attempt 7.** Attempt 1 landed (`.github/workflows/s9-hosted-probe.yml`
+ this directory) before the workflow had ever run. The maintainer then
triggered it once (run `35049304081`, `workflow_dispatch`, `main`,
2026-09-16) — its real evidence is recorded here for the first time; a
rejected attempt 2 recorded it but its own fix for the KVM half was never
actually validated under the privilege boundary it needed, so it never
landed. Attempt 3 rewrote and validated the fix for both bugs run
`35049304081` exposed and landed it (`1388f83`); the maintainer then
triggered it a second time (run `35056076616`, `workflow_dispatch`, `main`,
2026-09-16) — **the KVM half is now confirmed hosted, for real**, and the
lab job exposed two more, narrower bugs, fixed by attempt 4 (`9519d0e`).
The maintainer triggered it a third time (run `35057453620`) after that
landed: the control (apparmor=unconfined) branch succeeded, but the
variant's `mount` failed with libmount's EBUSY message ("already mounted
or mount point busy") and the step's own `else` branch called that
"apparmor=unconfined IS required" — a verdict the evidence actually
contradicts, since an EBUSY means AppArmor's mount mediation already let
the call through (issue #10's own analysis, not repeated here — see the
issue). Attempt 5 landed the two-probe design
(`spikes/s9/scripts/run-apparmor-check.sh` and the scripts it calls):
extracted, testable failure classification
(`apparmor-classify.sh`, exercised directly against the exact strings that
fooled all three prior runs — `results/apparmor-classify-test.log`),
device attribution reusing `scripts/devenv/lib.sh`'s own
`lab_assert_own_loop()`, a verified detach barrier between branches, a
second loop-free `mount -t tmpfs` mediation probe run alongside the
faithful loop/XFS one, and an order-reversal consistency check on the
cheap probe. The maintainer triggered it (run `35068617498`): the loop
probe ran, correctly voided (an EBUSY, not a denial — see below), and the
step then exited immediately, before the tmpfs probe — the one designed
to answer the AppArmor question without any loop/XFS machinery — ever
ran. Attempt 6 fixed the control-flow bug that stopped attempt 5's second probe
from ever running (`classify_verdict`/`classify_branch_pair` signalled
VOID through their own return status as well as their echoed string, and
`run-apparmor-check.sh` captured that status with a bare assignment under
`set -euo pipefail`, so a VOID at probe 1 killed the script before probe 2
started). The maintainer triggered it a fifth time (run `35071011877`):
all three probes finally ran, in two orders, with and without a loop
device — and all three came back VOID, on the identical EBUSY, which rules
out loop contention, leftover state and branch order as the cause (see
below). Every branch's diagnostics also showed an empty kernel/audit
capture and "not available in this container" for the profiles listing —
on every branch, including the controls, where AppArmor could not
possibly have fired. **Attempt 7 (this round) does not answer the
AppArmor question either** — nothing in this round could have, since none
of run `35071011877`'s failures were denials — it fixes that diagnostic
path: the audit/profile capture was reading `dmesg`/`journalctl -k`/
`/sys/kernel/security/apparmor/*` **inside the unprivileged probe
container**, which could never see the runner host's own kernel log, and
silently produced an empty result indistinguishable from "captured fine,
found nothing" on failure.
`spikes/s9/scripts/run-apparmor-check.sh` (which runs directly on the
runner host) now also captures the HOST's own kernel log and AppArmor
state after every branch, and reports a capture failure distinctly from
an absence of records, so a future triggered run's diagnostics can
actually be trusted. What follows is the current, cumulative state: what
is settled, what each attempt fixed, and what is still open.

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
Five verdicts are now void, each for its own reason: run `35049304081`'s
(both branches failed identically on the `mknod` bug), `35056076616`'s
(both branches would have failed identically on the size bug — the run
recorded a `FAIL` on the control before the variant branch ever
executed), `35057453620`'s (the variant's `mount` failed with an EBUSY
the step wrongly read as a denial), `35068617498`'s (the loop probe
correctly voided on the same class of EBUSY, but the script exited right
there — the tmpfs probe, the one probe never affected by any of the
loop/XFS bugs above, has still never executed), and `35071011877`'s (all
three probes finally ran, and all three hit the identical EBUSY — never a
denial — see below). Attempt 5 landed the classification, attribution,
detach-barrier fixes and the second, independent tmpfs probe; attempt 6
fixed the control-flow bug that kept that second probe from ever running
(see above, and `spikes/s9/scripts/apparmor-classify.sh`'s comments on
`classify_verdict` and `decide_final_verdict`); attempt 7 (this commit)
fixes the audit/profile capture so a future run's diagnostics are
trustworthy — but, like every fix round before it, has not yet run
hosted itself.

### A new observation from run 35068617498 — rules out one cause, establishes no other

`gh run view 35068617498 --repo mdg-labs/hoserva --job 104704596000 --log`
(excerpted in `results/ci-run-35068617498-lab-job.log`). The loop probe's
two branches got their own, distinct backing images (control inode
`8939735`, variant inode `8939751` — `losetup -a after attach` in each
branch's diagnostics), attribution passed for both, and the detach
barrier passed for both. The control's `losetup -a at end` is empty (its
device was cleanly released) before the variant branch's container even
started. The variant then still hit the same EBUSY
(`mount: /probe/mnt: /dev/loop0 already mounted or mount point busy.`)
run `35057453620` also hit. **This rules out inter-branch loop-device
contention** — the failure mode the detach barrier was added to
address — as the EBUSY's cause: the variant had its own image, its own
successful attribution, and started after the control's device was
already gone. It does not establish what the actual cause is, and it says
nothing about whether `apparmor=unconfined` is required — the tmpfs
probe, which isolates `mount(2)` from every loop/XFS concern including
this one, has still never run. The same inode-pair signature is
reproducible across runs `35057453620` and `35068617498`.

Also worth recording as a positive: the step-ordering fix from attempt 4
held — "Derive this run's lab id" ran before the AppArmor step (its
output, `id=s9-35068617498-1`, is set before the AppArmor step starts),
and `lab-destroy` used that real id and reported `lab s9-35068617498-1:
container does not exist, nothing to tear down` — a clean no-op, not the
`lab-require-id` failure on an empty id that run `35056076616` hit.
`lab-up`/`lab-seed`/`lab-verify-refusal`/the SnapRAID sync were **skipped**
in run `35068617498` (the job never got past the AppArmor step), so this
run does not re-confirm the lab half; that still rests on run
`35057453620`, where every lab step ran and passed in the same run as the
(subsequently-corrected) AppArmor verdict.

### Run 35071011877 — all three probes finally ran; still VOID, all three on the same EBUSY; found why the audit log always came up empty

`gh run view 35071011877 --repo mdg-labs/hoserva --job 104712259394 --log`
(excerpted in `results/ci-run-35071011877-lab-job.log`). Attempt 6's fix
held: the loop probe ran, then the tmpfs probe (forward order), then the
tmpfs probe reversed — all three, for the first time. All three came back
VOID, and all three for the identical reason: the variant's `mount` hit
`mount: /probe/mnt: /dev/loop0 already mounted or mount point busy.` (the
loop probe) or the tmpfs probe's own EBUSY equivalent — never a
permission-denied/operation-not-permitted string. Running the variant
*first* in the reversed tmpfs pass still produced the same EBUSY, and the
control run *after* it still succeeded, which rules out loop contention,
leftover state, and branch order as the cause (see also the
`35068617498`/`35057453620` inode-pair analysis above, which already ruled
out inter-branch loop-device contention specifically). The combined
verdict was VOID because every individual probe was VOID (`decide_final_verdict`'s
"at least one probe was individually VOID" path) — the two/three probes
never got the chance to actually disagree or agree with each other.

Every branch's diagnostics also showed `apparmor profiles listing: not
available in this container` and `kernel/audit apparmor lines: (empty)` —
on every branch, including the controls, where mediation could not
possibly have fired. That is the actual bug this round (issue #10
host-audit fix round) fixes: `apparmor-loop-probe.sh`/
`apparmor-tmpfs-probe.sh` were reading `dmesg`/`journalctl -k` **inside
the unprivileged probe container**, which was never going to see the
runner host's own kernel ring buffer, and silently fell back to an empty
file on failure — so a capture that could never work looked identical to
one that ran cleanly and found nothing. `run-apparmor-check.sh` (which
runs directly on the runner host, not in a container) now also captures
the HOST's `dmesg`/`journalctl -k` after every branch, using the runner's
own passwordless `sudo`, merges any AppArmor lines into the same
`audit-apparmor-lines.log` the classifier already reads, and records the
capture's own success/failure distinctly (`describe_audit_capture`,
`spikes/s9/scripts/apparmor-classify.sh`) so a runner where the kernel log
truly is unreadable is never again indistinguishable from "captured fine,
found nothing." It also captures the host's own AppArmor enablement state
and whether the `docker-default` profile is present, once, ahead of both
probes — the same root cause as the per-branch profiles-listing line.

This does not answer the AppArmor question either — it could not: none of
these EBUSYs were denials, so there was never a denial to corroborate. Its
value is fixing the diagnostic path a real denial (or the current EBUSY,
if an audit record turns out to explain it) would need to be seen at all.
A future triggered run's diagnostics should show either real AppArmor
audit lines or a "failed: could not read host dmesg/journalctl -k" status
line — never silent emptiness again. See this round's own commit for the
deliberate decision on what happens if a future run's audit log shows a
DENIED record alongside an EBUSY-classified stderr (still VOID, not
REQUIRED — `apparmor-classify.sh`'s `classify_verdict`/
`other_failure_with_audit_denial`).

After this commit is reviewed and pushed to `main`, the maintainer runs,
in fish:

```fish
gh workflow run s9-hosted-probe.yml --repo mdg-labs/hoserva --ref main
```

then reads the `lab` job's "AppArmor necessity check" step log — the `kvm`
job's question is already closed and does not need re-triggering for S9's
sake, though it will run again as part of the same workflow dispatch. The
step now runs `spikes/s9/scripts/run-apparmor-check.sh`, which prints its
own combined verdict as the very last thing it does, on stderr:

- **`NOT-REQUIRED`** (step exits 0) — both the faithful loop/XFS probe and
  the loop-free tmpfs probe report the variant (default AppArmor profile)
  succeeded, and the tmpfs probe agrees with itself run in both orders.
  `apparmor=unconfined` is not needed for this recipe on this runner.
- **`REQUIRED`** (step exits 0) — both probes report the variant failed
  with a confirmed denial (permission-denied/operation-not-permitted at
  the mediated operation). `apparmor=unconfined` is needed.
- **`VOID`** (step exits non-zero, and the `lab` job fails at this step) —
  either probe's own control failed, an attribution or detach-barrier
  check failed inside the loop probe, the two probes disagreed with each
  other, or the tmpfs probe disagreed with itself across the two run
  orders. Each of these prints which check failed and why, in the step's
  own log (`run-apparmor-check.sh`'s diagnostics, per-branch device,
  `losetup -a`, stderr, and any kernel/audit AppArmor lines the runner
  exposed) — the next update to this section should be written directly
  from that log, not re-derived.

`lab-up`/`lab-seed`/`lab-verify-refusal`/the SnapRAID sync only run if the
AppArmor step reaches `NOT-REQUIRED` or `REQUIRED` (exit 0); on `VOID` the
job stops there, same as the original design's "abort loudly instead of
asserting anything" — `lab-destroy` (`if: always()`) still runs either
way, because the lab id is derived before this step.

Whatever the result, update `docs/internal/08-spike-findings.md`'s S9
section and Q42/Q79 (doc 13) with the actual verdict — rather than leaving
this section's "still open" framing standing — and record the run's id
and log excerpts under `spikes/s9/results/` the same way runs
`35049304081` and `35056076616` are recorded above.

### This fix round: a real runtime guard against `sudo` on a dev host (issue #10 fix round)

The host-audit fix round above added `run-apparmor-check.sh` calls to
`sudo dmesg`, `sudo journalctl -k` and
`sudo cat /sys/kernel/security/apparmor/profiles`, guarded only by a
comment on `capture_host_audit` saying the function "must only ever run
on that hosted runner — never on a maintainer's dev host". A comment is
not a check: nothing in the script actually stopped it from running, and
`sudo`-ing, on a developer's own machine. A `sudo` invocation there raises
a polkit/PAM prompt on the maintainer's *desktop session*, and three
failed authentications trip `pam_faillock`'s `deny=3` and lock them out of
their own machine, including out of `sudo` itself — `CLAUDE.md`'s absolute
rule on never triggering such a prompt outranks finishing any task.

`run-apparmor-check.sh` now refuses to run at all unless `GITHUB_ACTIONS`
is exactly `"true"` — the literal value GitHub Actions sets in every
workflow job's environment — checked in a real `if` immediately after the
only two things needed to reach it (`set -euo pipefail` and the `source`
that pulls in the check itself); nothing else in the script, including
`REPO_ROOT` resolution, runs before it. The decision itself
(`hosted_runner_guard`, `apparmor-classify.sh`) is a pure function — no
`sudo`, no `docker`, no side effect — so it is unit-tested directly
(`test-apparmor-classify.sh`) without ever running `run-apparmor-check.sh`:
running that script, even expecting it to refuse, would itself be the
hazard a faulty guard could cause. The same test file also asserts, by
grep and line number against `run-apparmor-check.sh`'s actual text
(comment lines and message-printing `echo` lines excluded, so prose
mentioning "sudo"/"docker" can't produce a false match), that the guard's
call site precedes both the first real `sudo` call and the first real
`docker run` call — the ordering property is proven mechanically, not
eyeballed. No override variable (`FORCE=1` or similar) is accepted: an
escape hatch is exactly how a guard like this ends up bypassed on a dev
host anyway. See `results/apparmor-classify-test-issue10-preguard.log`
(39/47 — the 8 new guard cases fail cleanly, `hosted_runner_guard` not
yet existing in that pre-fix code) vs. `results/apparmor-classify-test.log`
(47/47, current).

## Files

- `scripts/hosted-snapraid-check.sh` — host-side orchestration (runs on
  the CI runner or a dev host); unchanged this round, already confirmed
  on a hosted runner (run 35049304081)
- `scripts/snapraid-sync-in-container.sh` — runs inside the lab
  container, invoked by the above; unchanged this round
- `scripts/kvm-boot-check.sh` — the guest-boot check the `kvm` job runs;
  unchanged since attempt 3, now **confirmed against a real `/dev/kvm`**
  by run `35056076616`
- `scripts/apparmor-classify.sh` — pure-text failure classification
  (`classify_mount_failure`) and control+variant verdict combination
  (`classify_verdict`), extracted so it can be exercised without Docker or
  a loop device; never infers a denial from exit status alone
  (requirement B). This round (attempt 6): `classify_verdict`/
  `classify_branch_pair` now always `return 0`, even on VOID — the old
  `return 1` was what let run `35068617498`'s VOID at probe 1 kill
  `run-apparmor-check.sh` under `set -euo pipefail` before probe 2 ever
  started. Also new this round: `decide_final_verdict`, the orchestrator's
  combined-decision step (requirements F/G), moved here from
  `run-apparmor-check.sh` so it is directly testable; unlike the two
  functions above, its own nonzero-on-VOID return status is deliberate —
  it is the terminal decision, never captured with a bare assignment.
  This round (attempt 7, issue #10 host-audit fix round): two new
  functions, both pure text/exit-status, no Docker or privilege needed —
  `other_failure_with_audit_denial` (the explicit, separately-named path
  for "the variant's stderr didn't classify as a denial, but the host's
  kernel audit log has a DENIED record for it anyway": stays VOID, but
  with distinct reasoning text, never folded into DENIED by widening
  `classify_mount_failure`'s stderr match) and `describe_audit_capture`
  (pure formatting for `run-apparmor-check.sh`'s new host-side capture,
  decoupled from actually running `dmesg`/`journalctl -k` so it's
  testable without `sudo`). `classify_verdict`'s OTHER-FAILURE case now
  calls the former; no other classification behaviour changed. This
  fix round (attempt 8, issue #10 hosted-runner-guard fix round): one new
  function, `hosted_runner_guard` — a pure allow/refuse decision (no
  `sudo`, no `docker`, no side effect) that `run-apparmor-check.sh` now
  calls before its first `sudo`/`docker` invocation, closing the actual
  defect this round exists to fix (see the README section above `## Files`
  for the full reasoning). No other function in this file changed —
  `classify_mount_failure` in particular is byte-identical to attempt 7's
  committed version.
- `scripts/test-apparmor-classify.sh` — exercises `apparmor-classify.sh`
  against real strings, including run `35057453620`'s own verbatim EBUSY
  message — output committed at `results/apparmor-classify-test.log`.
  This round (attempt 6): two new cases reproducing run `35068617498`'s
  exact failure (a VOID captured under `set -euo pipefail` must not abort
  the caller — proven to fail against the pre-fix idiom and pass against
  the fixed one, see `results/apparmor-classify-test-issue10-preunfixed.log`
  vs. `results/apparmor-classify-test.log`) and five new
  `decide_final_verdict` cases, including one proving a combined VOID
  still exits non-zero. This round (attempt 7): 16 new cases — four
  `classify_verdict` end-to-end cases covering every combination of
  {EBUSY, genuine denial} × {with, without a corroborating audit record}
  (with `check_reason_contains` assertions proving the audit-conflict case
  gets genuinely distinct reasoning text, not just the plain OTHER-FAILURE
  text with the same verdict word), plus 12 pure unit cases for
  `other_failure_with_audit_denial`/`describe_audit_capture` — 39/39
  passing, up from 23; see
  `results/apparmor-classify-test-issue10-preaudit.log` for the same suite
  run against this round's PRE-FIX code (`e9ad714`) — 30/39, the 9 new
  cases either printing `FAIL` or aborting the run on "command not found"
  (both functions are new this round). This fix round (attempt 8): 8 new
  cases — 7 pure `hosted_runner_guard` allow/refuse cases (the
  hosted-runner signal present, absent, empty, `"false"`, wrong case,
  an unrelated truthy-looking string, and no argument at all — all but
  the first refuse) plus one static ordering check that greps
  `run-apparmor-check.sh` by line number to assert the guard's call site
  precedes the first real `sudo` call and the first real `docker run`
  call, with comment lines and message-printing `echo` lines stripped
  first so prose mentioning "sudo"/"docker" can't produce a false match —
  47/47 passing, up from 39; see
  `results/apparmor-classify-test-issue10-preguard.log` for the same
  suite run against this round's own PRE-FIX code (`main@e40300d`)
  instead — 39/47, the 8 new cases failing cleanly (`hosted_runner_guard`
  does not exist yet in that code, and the guard's call site is absent
  from that `run-apparmor-check.sh`). Per this round's dispatch,
  `run-apparmor-check.sh` itself is never executed to prove any of
  this — not even expecting it to refuse.
- `scripts/apparmor-loop-probe.sh` — runs inside one branch's container
  for the faithful loop/XFS probe; reuses `scripts/devenv/lib.sh`'s
  `lab_assert_own_loop()` for device attribution (requirement C) and adds
  a verified detach barrier before the branch's container exits
  (requirement D); unchanged this round. Its own in-container best-effort
  `dmesg`/`journalctl -k` capture (requirement E) is also unchanged and
  kept — harmless when empty, and it's what run `35071011877` showed was
  never going to work from inside an unprivileged container; the fix is
  entirely on the reader side now (`run-apparmor-check.sh`, below).
- `scripts/apparmor-tmpfs-probe.sh` — the loop-free mediation probe
  (requirement F) — a bare `mount -t tmpfs`, isolating the one syscall in
  question from every loop-device/XFS concern; unchanged this round (same
  in-container-capture-kept reasoning as the loop probe, above) — **now
  confirmed to actually run in CI**, by run `35071011877`.
- `scripts/run-apparmor-check.sh` — host-side orchestrator: runs both
  probes' control+variant pairs, an order-reversal consistency check on
  the cheap tmpfs probe (requirement G), classifies each with
  `apparmor-classify.sh`, and refuses to let either probe claim the
  other's result if they disagree (requirement F). This round (attempt
  6): the combined-decision block is now a call to
  `decide_final_verdict` instead of inline logic — behaviour unchanged,
  moved for testability. This round (attempt 7, issue #10 host-audit fix
  round): two new functions, `capture_host_audit` (runs after every
  branch; captures the runner HOST's own `dmesg`/`journalctl -k` with its
  own passwordless `sudo`, merges any AppArmor lines into the same
  `audit-apparmor-lines.log` the classifier already reads, and writes a
  distinct `audit-capture-status` field via `describe_audit_capture` so a
  runner where the log is genuinely unreadable never again looks like
  "captured fine, found nothing") and `capture_host_apparmor_state`
  (captured once, ahead of both probes: whether the kernel has AppArmor
  enabled, and whether the `docker-default` profile is present on the
  host). `print_branch_diagnostics` now prints the capture status line
  distinctly from the merged audit-lines content; `print_host_apparmor_state`
  is a new, once-only diagnostics block ahead of "Probe 1/2". No
  classification logic changed here — only what feeds
  `audit-apparmor-lines.log` and what gets printed. This fix round
  (attempt 8): the hosted-runner guard call (`hosted_runner_guard`,
  `apparmor-classify.sh`) is now the first thing this script does after
  `set -euo pipefail` and the `source` that reaches it — before
  `REPO_ROOT` is even resolved, let alone the lib.sh check, `mktemp`,
  `docker`, or the host-side `sudo` calls further down. Nothing else in
  this file changed; `capture_host_audit`, `capture_host_apparmor_state`,
  `print_host_apparmor_state`, `print_branch_diagnostics` and every probe
  call below are unchanged.
- `results/apparmor-classify-test.log` — verbatim output of
  `test-apparmor-classify.sh` against the current (fixed) code — now
  47/47 passing (was 39/39 before this round; see above)
- `results/apparmor-classify-test-issue10-preunfixed.log` — the same
  current test suite (as of attempt 6) run against the PRE-FIX
  `apparmor-classify.sh` (`main@437cb25`) instead — 16/23, the two
  set-e-survival cases failing with empty stdout (the caller died before
  its own first `echo`) and the five `decide_final_verdict` cases failing
  with "command not found" (that function is new in attempt 6) — the
  required before/after demonstration that the new cases actually test
  something
- `results/apparmor-classify-test-issue10-preaudit.log` — new this round
  (attempt 7): the CURRENT test suite (39 cases) run against this round's
  own PRE-FIX code (`main@e9ad714`, attempt 6's committed state) instead —
  30/39, proving the 9 new audit-conflict/capture-status cases actually
  exercise something this round added
- `results/apparmor-classify-test-issue10-preguard.log` — new this fix
  round (attempt 8): the CURRENT test suite (47 cases) run against this
  round's own PRE-FIX code (`main@e40300d`, attempt 7's committed state)
  instead — 39/47, proving the 8 new hosted-runner-guard cases actually
  exercise something this round added
- `results/ci-run-35071011877-lab-job.log` — new this round: the fourth
  `workflow_dispatch` run of this issue's own workflow (on `e9ad714`,
  attempt 6): all three probes finally ran, in two orders, with and
  without a loop device; all three came back VOID on the identical EBUSY
  (never a denial); every branch's audit/profiles diagnostics came up
  empty/"not available" — the run that exposed this round's actual bug
  (see the README section above this one for the full analysis)
- `results/ci-run-35068617498-lab-job.log` — the third `workflow_dispatch`
  run of this issue's own workflow (on `437cb25`, attempt 5): "Derive this
  run's lab id" now correctly precedes the AppArmor step, the loop probe
  ran and correctly voided on an EBUSY, the tmpfs probe never appeared
  (the bug attempt 6 fixed), and `lab-destroy` used the real derived id
  and no-op'd cleanly
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
