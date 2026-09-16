# Spike S9 — hosted CI runners, issue #10

Findings are in `docs/internal/08-spike-findings.md` ("Hosted CI runners"
subsection under Spike 9). This directory holds the probe workflow's
scripts and the raw evidence this run's findings are checked against.

**This is fix attempt 9 — a change of approach, not another patch.**
Attempts 1-4 (see git history for the full blow-by-blow; not repeated here)
landed the workflow, fixed a `mknod` bug and an undersized-image bug, and
**confirmed the KVM half hosted for real** (run `35056076616`: QMP
`query-kvm` → `{"enabled": true, "present": true}`) and **confirmed the lab
half hosted for real** (run `35057453620`: `lab-up`, `lab-seed`,
`lab-verify-refusal`, a SnapRAID sync and `lab-destroy` all green, with
`apparmor=unconfined` set). Both of those results stand, unchanged by this
attempt.

**AppArmor necessity was still open after six straight voids** (runs
`35049304081`, `35056076616`, `35057453620`, `35068617498`, `35071011877`,
`35075054897`) from attempts 4-8, which built an increasingly elaborate
hand-rolled replica of the lab recipe — raw `docker run`, manual `mknod`,
`losetup`, `mkfs.xfs`, `mount`, a pure-text failure classifier, a
loop-free tmpfs mediation probe, an order-reversal consistency check, and
finally a host-side kernel-audit capture — to test one property of the
**real lab**. Every single void traced to a bug in that replica: a missing
`mknod`, an undersized image, an EBUSY misread as a denial, a `set -e`
abort swallowing a verdict, a stale result file read as success, and
finally a correct-but-inconclusive EBUSY. The host-audit capture added in
attempt 7 also rested on a false premise: it looked for `apparmor="DENIED"`
records to corroborate a denial, but Docker's default AppArmor profile
denies with a plain `deny mount,` rule, and in AppArmor a plain `deny`
**suppresses the audit record** — only `audit deny` logs. Finding "288
records, zero denials" in that capture proved nothing about whether a
denial happened.

**This attempt retires that entire apparatus** (`apparmor-loop-probe.sh`,
`apparmor-tmpfs-probe.sh`, `apparmor-classify.sh`, `run-apparmor-check.sh`,
`test-apparmor-classify.sh`, and the host `sudo` audit capture — deleted,
not superseded-but-kept) and replaces it with the one thing CLAUDE.md's
first architecture rule ("orchestrate, never reimplement") already implied
should have been used from the start: **run the real lab twice, through the
real `make lab-up`/`make lab-destroy` targets, with exactly one variable.**
`docker-compose.dev.yml` (the artifact under test) sets
`security_opt: [apparmor=unconfined]` and is never edited for this
experiment — the control arm runs it completely unmodified (byte-for-byte
the already-confirmed recipe from run `35057453620`); the variant arm adds
`scripts/devenv/docker-compose.apparmor-default.yml` as a second compose
file (the Makefile's new `LAB_COMPOSE_EXTRA` hook), which puts the lab
container back on Docker's default AppArmor profile instead. The success
criterion is the one the real recipe already has:
`scripts/devenv/create-array.sh` runs under `set -euo pipefail`, calls
`die` on any failure, and prints `lab <id> ready:` only on success — so
`make lab-up`'s own exit status is the whole verdict; no stderr
classification, no audit-log corroboration, no probe apparatus of any kind.
As a direct consequence, this removes the `sudo` risk surface the host-audit
capture introduced entirely — the new experiment needs no `sudo` at all.

Verdict discipline is unchanged from every prior attempt, because it was
never the problem — only the mechanics producing the inputs to it were:

- **control fails → VOID.** The control step doesn't vary AppArmor at all;
  it's today's already-confirmed recipe. A failure there means the recipe
  itself is broken on this runner, and the job fails loudly at that step —
  never read as an AppArmor result.
- **variant fails, control succeeded → REQUIRED.** Reported from the
  variant step's own real `make lab-up` output (visible directly in that
  step's log — `continue-on-error: true` keeps the job green while
  preserving the step's true outcome for the verdict step to read), never
  assumed or re-classified from a stderr string.
- **both succeed → NOT-REQUIRED.**

## What run 35057453620 and 35056076616 established (still standing, unchanged by this attempt)

### KVM: CONFIRMED on a hosted runner (run 35056076616)

`gh run view 35056076616 --repo mdg-labs/hoserva --job 104666440639 --log`,
excerpted in `results/ci-run-35056076616-kvm-job.log`. On the pinned
`ubuntu-24.04` (image `20260907.300.1`, `nproc: 4`, Azure `eastus2`), the
`kvm` job succeeded end to end: `/dev/kvm` went `crw-rw----` →
`crw-rw-rw-` after the udev rule, the kernel-acquisition fix worked on a
real runner, and the guest booted with KVM acceleration confirmed **by the
host process** (QMP `query-kvm`), not only inferred from the guest. **This
closes S9's KVM acceptance criterion**: "A QEMU guest boots with KVM
acceleration on a standard hosted runner." This says nothing about S10
(nested KVM, VM-in-VM) — a different, still-open question, out of this
spike's scope.

### Lab half: CONFIRMED on a hosted runner (run 35057453620)

`lab-up`, `lab-seed`, `lab-verify-refusal`, a SnapRAID sync and
`lab-destroy` all ran green in the same run, with `apparmor=unconfined`
set — the same steps this workflow's job still runs today, after its own
AppArmor step. No log excerpt for run `35057453620` itself is committed in
this directory. `results/ci-run-35049304081-lab-job.log` is the earliest
committed evidence of the same pattern (`lab-up`/`lab-seed`/
`lab-verify-refusal`/SnapRAID sync all succeeding in one run, on the pinned
`ubuntu-24.04` image) — that run's own inline AppArmor verdict was
separately retracted (both branches failed on an unrelated `mknod` bug),
but the lab steps after it were real passes, not affected by that bug.
`results/ci-run-35068617498-lab-job.log` does **not** show this: in that
run the job never got past the (now-retired) AppArmor probe step, so
`lab-up` onward never ran — it is kept here only as historical context for
the retired probe's own bug history, not as lab-half evidence.

## AppArmor necessity: the new experiment (this attempt)

The workflow's `lab` job now runs, in order, after deriving this run's base
lab id (`s9-<run>-<attempt>`):

1. **Control arm** — `HOSERVA_LAB_ID=<base>-apparmor-ctrl make lab-up`
   (`docker-compose.dev.yml` alone, `apparmor=unconfined`), then
   `make lab-destroy` for that same id, `if: always()`.
2. **Variant arm** (only reached if the control succeeded) —
   `HOSERVA_LAB_ID=<base>-apparmor-var LAB_COMPOSE_EXTRA=scripts/devenv/docker-compose.apparmor-default.yml make lab-up`
   (Docker's default AppArmor profile), `continue-on-error: true`, then
   `make lab-destroy` for that same id and extra file, `if: always()`.
3. **Verdict step** — reads `steps.apparmor_variant.outcome` and prints
   `NOT-REQUIRED` or `REQUIRED` as plain step output. Always exits 0 (a
   concluded finding is not a workflow failure); a VOID has already failed
   the job at step 1, before this step is ever reached.
4. The job's own standing `lab-up`/`lab-seed`/`lab-verify-refusal`/SnapRAID
   sync then runs as before, under the base id itself — a third, distinct
   lab id, so none of the three (`-apparmor-ctrl`, `-apparmor-var`, and the
   bare base id) can ever collide with one another.

Compose's list-merge behaviour for `security_opt` across `-f` files was
**rendered and read, not assumed**: a first draft of
`docker-compose.apparmor-default.yml` used a plain
`security_opt: [apparmor=docker-default]`, and `docker compose ... config`
showed both entries present (`[apparmor=unconfined, apparmor=docker-default]`)
— Compose unions `security_opt` across files by default, it does not
replace it. The committed file uses the `!override` YAML merge tag instead,
which does replace the list wholesale — confirmed by re-rendering both arms:
`results/compose-config-control.txt` (docker-compose.dev.yml alone,
`security_opt: [apparmor=unconfined]`, unchanged from today) and
`results/compose-config-variant.txt` (with the override file added,
`security_opt: [apparmor=docker-default]` alone).

After this commit is reviewed and pushed to `main`, the maintainer runs, in
fish:

```fish
gh workflow run s9-hosted-probe.yml --repo mdg-labs/hoserva --ref main
```

then reads the `lab` job's five new "AppArmor necessity check: ..." steps.
What each outcome looks like in the log:

- **VOID** — the "control arm" step itself is red; its own `make lab-up`
  output (in that step's log) is the actual failure to investigate. The
  job fails there; none of "variant arm", "verdict", or the standing
  `lab-up`/`lab-seed`/`lab-verify-refusal`/SnapRAID-sync steps run (except
  the final `lab-destroy`, which always runs and no-ops safely against an
  id that was never brought up).
- **REQUIRED** — "control arm" is green, "variant arm" shows its own
  failure (marked non-fatal by `continue-on-error`, so the job stays
  green), and "verdict" prints
  `AppArmor necessity check: REQUIRED — ...` naming the variant's own
  error as the evidence.
- **NOT-REQUIRED** — "control arm" and "variant arm" are both green, and
  "verdict" prints
  `AppArmor necessity check: NOT-REQUIRED — ...`.

Whatever the result, update `docs/internal/08-spike-findings.md`'s S9
section and Q42/Q79 (doc 13) with the actual verdict, and record the run's
id and log excerpts under `spikes/s9/results/` the same way prior runs are
recorded above.

## Files

- `scripts/hosted-snapraid-check.sh` — host-side orchestration (runs on the
  CI runner or a dev host); unchanged, already confirmed on a hosted runner
  (run `35049304081`)
- `scripts/snapraid-sync-in-container.sh` — runs inside the lab container,
  invoked by the above; unchanged
- `scripts/kvm-boot-check.sh` — the guest-boot check the `kvm` job runs;
  unchanged, confirmed against a real `/dev/kvm` by run `35056076616`
- `../../scripts/devenv/docker-compose.apparmor-default.yml` — new this
  attempt: the variant arm's compose overlay. Lives under `scripts/devenv/`
  (not `spikes/s9/`) because it is a `-f` argument to the real
  `docker-compose.dev.yml`/`make lab-up` recipe, not a spike-only artifact
- `../../Makefile` — new this attempt: the optional `LAB_COMPOSE_EXTRA`
  variable, appended as a second `-f` to `COMPOSE_DEV` only when set; empty
  by default, so the standing lab's behaviour is unchanged
- `results/compose-config-control.txt`, `results/compose-config-variant.txt`
  — new this attempt: verbatim `docker compose ... config` renderings for
  both arms, proving the control rendering is unchanged from today and the
  variant carries exactly one different `security_opt` value
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
