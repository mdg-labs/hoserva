# Spike S9 — hosted CI runners, issue #10 (closed)

Findings are in `docs/internal/08-spike-findings.md` ("Hosted CI runners"
subsection under Spike 9), which is the authoritative, closed verdict. This
directory holds the probe workflow's scripts and the raw evidence the
findings are checked against.

**S9 is fully answered: L2 (loop devices, FUSE, a SnapRAID sync), KVM
acceleration for a single non-nested guest, and AppArmor necessity for the
lab's own `mount(2)` are all confirmed hosted.** S10 (nested KVM, VM-in-VM,
issue #87, Phase 3.5) is a separate, still-open question — nothing here
answers it.

## What was established, and from which runs

### KVM: CONFIRMED on a hosted runner

First confirmed in run `35056076616` (`gh run view 35056076616 --repo
mdg-labs/hoserva --job 104666440639 --log`, excerpted in
`results/ci-run-35056076616-kvm-job.log`) and reconfirmed in five further
runs since, most recently run `35076920766` (job `104731427908`,
2026-09-16, head `e97c469`). On the pinned `ubuntu-24.04` image, the `kvm`
job succeeds end to end: `/dev/kvm` goes `crw-rw----` → `crw-rw-rw-` after
the udev rule, the runner's own running kernel is acquired readably, and
the guest boots with KVM acceleration confirmed **by the host process**
(QMP `query-kvm` → `{"enabled": true, "present": true}`), not only
inferred from the guest. **This closes S9's KVM acceptance criterion**: "a
QEMU guest boots with KVM acceleration on a standard hosted runner." It
says nothing about S10 (nested KVM, VM-in-VM) — a different, still-open
question, out of this spike's scope.

### Lab (L2): CONFIRMED on a hosted runner

First confirmed in run `35049304081` and reconfirmed since, most recently
in run `35076920766`'s own `lab` job (`104731428284`): `lab-up`,
`lab-seed`, `lab-verify-refusal` (both host-device-refusal checks,
correctly `EPERM`), a SnapRAID sync against the standing array
(`Everything OK`) and `lab-destroy` all ran green in the same run as the
KVM confirmation above, on the pinned `ubuntu-24.04` image.
`results/ci-run-35049304081-lab-job.log` is that first run's committed
evidence; its own inline AppArmor verdict was separately retracted (see
"The void history" below), but the lab steps around it were real passes,
unaffected by that bug.

### AppArmor necessity: ANSWERED — `apparmor=unconfined` is REQUIRED

Run `35076920766`, job `104731428284`, ran the real lab recipe three times
on one runner, one image, under three distinct `HOSERVA_LAB_ID`s, differing
only in the container's AppArmor profile:

1. **Control arm** — `HOSERVA_LAB_ID=<base>-apparmor-ctrl make lab-up`
   (`docker-compose.dev.yml` alone, `apparmor=unconfined`) — succeeded:
   `lab … ready: parity1, disk1-3, cache, pool` at 09:00:43 UTC — then
   `make lab-destroy` for that same id.
2. **Variant arm** — `HOSERVA_LAB_ID=<base>-apparmor-var
   LAB_COMPOSE_EXTRA=scripts/devenv/docker-compose.apparmor-default.yml
   make lab-up` (Docker's default AppArmor profile) — failed at 09:00:55
   UTC: `mount: /lab/…/mnt/parity1: /dev/loop0 already mounted or mount
   point busy.` / `make: *** [Makefile:110: lab-up] Error 32` — then
   `make lab-destroy` for that same id and extra file.
3. **Verdict step** — read `steps.apparmor_variant.outcome` directly
   (`if [ "failure" = "success" ]`, never a stderr re-classification) and
   printed `AppArmor necessity check: REQUIRED — …`.
4. The job's own standing `lab-up`/`lab-seed`/`lab-verify-refusal`/SnapRAID
   sync then ran under the base id at 09:01:06 UTC — a third, distinct
   `unconfined` lab, green end to end.

**The A-B-A ordering is what makes this conclusive**: `unconfined`
succeeded both immediately before and immediately after the
`docker-default` failure, one second after the failing variant's own
teardown, on the same runner, against the same loop devices — ruling out
leftover state, loop contention and ordering artifacts from inside this
run itself. **Arm 4 was not designed as a control for this experiment** —
it is the job's own standing lab run, which happens to supply the second
`unconfined` success; it is reported as corroboration, not as a
purpose-built third arm.

Compose's list-merge behaviour for `security_opt` across `-f` files was
**rendered and read, not assumed** — see `results/compose-config-control.txt`
and `results/compose-config-variant.txt`; the committed
`docker-compose.apparmor-default.yml` uses the `!override` YAML merge tag
so the variant carries exactly one `security_opt` value
(`apparmor=docker-default`), never a union of both.

### What remains open: the errno

The variant's failure is **EBUSY** ("already mounted or mount point
busy"), where a mount denied by AppArmor would conventionally surface as
**EACCES**. It arrives *after* `losetup --find --show`,
`lab_assert_own_loop` and `mkfs.xfs -q -L parity1` all succeeded against
the same `/dev/loop0` — only the `mount(2)` call itself failed. This is
left as an open, unexplained observation, not a resolved mechanism: the
established claim is necessity (removing the flag breaks the real lab
recipe at its first mount), not an explanation of the kernel's exact error
code. Moby's `docker-default` AppArmor template (`moby/profiles`,
`apparmor/template.go`) denies `mount` with a plain, unaudited `deny` rule
— consistent with a silently denied mount, cited only as upstream
corroboration, not as the primary evidence.

## The void history

**AppArmor necessity was voided six times before the result above** — runs
`35049304081`, `35056076616`, `35057453620`, `35068617498`, `35071011877`,
`35075054897` — across attempts 1-8, which built an increasingly elaborate
hand-rolled replica of the lab recipe: raw `docker run`, manual `mknod`,
`losetup`, `mkfs.xfs`, `mount`, a pure-text failure classifier, a loop-free
tmpfs mediation probe, an order-reversal consistency check, and finally a
host-side kernel-audit capture. Every single void traced to a bug in that
replica, never to AppArmor itself: a missing `mknod`, an undersized image,
an EBUSY misread as a denial, a `set -e` abort swallowing a verdict, a
stale result file read as success, and finally a correct-but-inconclusive
EBUSY the replica had no way to interpret past. The host-audit capture
added in attempt 7 also rested on a false premise: it looked for
`apparmor="DENIED"` records to corroborate a denial, but Docker's default
AppArmor profile denies with a plain `deny mount,` rule, and in AppArmor a
plain `deny` **suppresses the audit record** — only `audit deny` logs.
Finding "288 records, zero denials" in that capture proved nothing about
whether a denial happened.

**Two of those runs printed "apparmor=unconfined is required" from broken
experiments, and both verdicts were retracted at the time** — run
`35049304081`, where both branches failed identically from the
missing-`mknod` bug, and run `35057453620`, where the probe misread an
EBUSY as a denial. Neither failure had anything to do with AppArmor.
**The confirmed result above does not retroactively validate either
retracted verdict.** These are two separate facts: an earlier claim
was unsupported and withdrawn; a later, independent, methodologically
sound experiment has since established the same conclusion on its own
evidence.

**Attempt 9 (this attempt) retired that entire apparatus**
(`apparmor-loop-probe.sh`, `apparmor-tmpfs-probe.sh`,
`apparmor-classify.sh`, `run-apparmor-check.sh`,
`test-apparmor-classify.sh`, and the host `sudo` audit capture — deleted,
not superseded-but-kept) and replaced it with the one thing `CLAUDE.md`'s
first architecture rule ("orchestrate, never reimplement") already implied
should have been used from the start: **run the real lab twice, through
the real `make lab-up`/`make lab-destroy` targets, with exactly one
variable.** `docker-compose.dev.yml` (the artifact under test) sets
`security_opt: [apparmor=unconfined]` and is never edited for this
experiment — the control arm runs it completely unmodified; the variant
arm adds `scripts/devenv/docker-compose.apparmor-default.yml` as a second
compose file (the Makefile's `LAB_COMPOSE_EXTRA` hook), which puts the lab
container back on Docker's default AppArmor profile instead. The success
criterion is the one the real recipe already has:
`scripts/devenv/create-array.sh` runs under `set -euo pipefail`, calls
`die` on any failure, and prints `lab <id> ready:` only on success — so
`make lab-up`'s own exit status is the whole verdict; no stderr
classification, no audit-log corroboration, no probe apparatus of any
kind. This also removed the `sudo` risk surface the host-audit capture had
introduced entirely — the new experiment needs no `sudo` at all. This
attempt ran hosted as run `35076920766` and produced the conclusive REQUIRED
result recorded above.

**The lesson worth carrying forward** (also recorded in
`docs/internal/08-spike-findings.md` §9): every one of the six voids came
from reimplementing the lab recipe instead of exercising it. The fix was to
run the real lab through the real `make` targets with one variable — the
most transferable finding of this spike.

## S9 / S10 boundary

S9's three questions (L2 hosted, KVM hosted, AppArmor necessity) are all
answered above. **S10 (nested KVM, VM-in-VM, issue #87, Phase 3.5) is a
different question and remains open** — whether the L3 test VM itself can
run a nested KVM guest, on the dev host and on hosted runners. Nothing in
this spike implies an answer to S10 either way.

## Files

- `scripts/hosted-snapraid-check.sh` — host-side orchestration (runs on the
  CI runner or a dev host); confirmed on a hosted runner (run `35049304081`
  and every run since)
- `scripts/snapraid-sync-in-container.sh` — runs inside the lab container,
  invoked by the above
- `scripts/kvm-boot-check.sh` — the guest-boot check the `kvm` job runs;
  confirmed against a real `/dev/kvm` by run `35056076616` and every run
  since, most recently `35076920766`
- `../../scripts/devenv/docker-compose.apparmor-default.yml` — the
  variant arm's compose overlay, confirmed to produce the REQUIRED result
  in run `35076920766`. Lives under `scripts/devenv/` (not `spikes/s9/`)
  because it is a `-f` argument to the real `docker-compose.dev.yml`/
  `make lab-up` recipe, not a spike-only artifact
- `../../Makefile` — the `LAB_COMPOSE_EXTRA` variable, appended as a second
  `-f` to `COMPOSE_DEV` only when set; empty by default, so the standing
  lab's behaviour is unchanged
- `results/compose-config-control.txt`, `results/compose-config-variant.txt`
  — verbatim `docker compose ... config` renderings for both arms, proving
  the control rendering is unchanged from today and the variant carries
  exactly one different `security_opt` value
- `results/ci-run-34950031773-lab-job.log` — the earlier push-triggered
  `ci.yml` run this spike originally cited for the lab half (pre-dates this
  issue's own workflow)
- `results/ci-run-35049304081-lab-job.log`,
  `results/ci-run-35049304081-kvm-job.log` — the first `workflow_dispatch`
  run of this issue's own workflow: `lab-up`/`lab-seed`/
  `lab-verify-refusal`/SnapRAID sync/`lab-destroy` all succeeded (the first
  hosted confirmation of the lab half), while that run's own then-inline
  AppArmor probe (a different, now-deleted piece of code) failed on a
  `mknod` bug and had its verdict retracted; the `kvm` job hit the
  kernel-read bug, since fixed and confirmed
- `results/ci-run-35056076616-lab-job.log`,
  `results/ci-run-35056076616-kvm-job.log` — the second run: the `kvm`
  job's real PASS with QMP evidence, and the `lab` job's then-inline
  AppArmor probe hitting an undersized-image bug plus the resulting
  empty-lab-id teardown failure, both since fixed (the lab half's own
  `lab-up` onward was never reached in this particular run)
- `results/ci-run-35068617498-lab-job.log` — the third run: a (now-retired)
  AppArmor probe attempt's own void; the lab half's `lab-up` onward was not
  reached in this run either (the job stopped at the probe step)
- `results/ci-run-35071011877-lab-job.log` — the fourth run: further
  evidence the old probe's EBUSY was not loop contention, branch order or
  leftover state (still informative context; the probe itself is retired)
- `results/kernel-acquisition-nonroot.log` — the non-root privilege
  boundary validation for the KVM kernel-acquisition fix, confirmed hosted
- `results/tcg-plumbing-check.log` — the optional pure-TCG plumbing
  validation for the KVM boot-check script, and a jq bug it caught
- `results/local-snapraid-sync-check.log`, `results/local-versions.log` —
  this spike's earlier dev-host validation of the SnapRAID-sync scripts
- `results/mkfs-xfs-size-check.log` — the xfsprogs minimum-size threshold
  that fixed the (now-retired) AppArmor probe's undersized images, by
  matching the standing lab's own `LAB_CACHE_SIZE` convention
  (`scripts/devenv/create-array.sh`) instead of a smaller ad hoc size

The final, conclusive AppArmor-necessity run (`35076920766`) is fetched
read-only and cited by run id and job id throughout this document and in
`docs/internal/08-spike-findings.md` §9; no new log excerpt file for it is
committed here — reproduce with `gh run view 35076920766 --repo
mdg-labs/hoserva --job 104731428284 --log`.
