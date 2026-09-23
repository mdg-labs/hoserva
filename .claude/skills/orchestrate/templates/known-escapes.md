# Known escapes

Defect patterns that passed a `task-verifier` PASS and were later confirmed
real by CodeRabbit on a `dev → main` pull request. `task-executor` checks its
change against this list before committing; `task-verifier` checks each
commit against it under layers 6 and 7. `coderabbit-review` appends a line
whenever it fixes a confirmed finding whose pattern is not here yet.

One line per pattern: **category** — what goes wrong — where it was seen.
Keep it to patterns, not individual bugs; merge a new instance into an
existing line by adding its PR number.

## Wiring and missing scope
- **wiring** — service, store or engine built but never constructed or assigned in `cmd/hoservad/main.go`, so the handler field stays nil and every operation `501`s or returns empty data — PR 195 (metrics), 236 (share usage), 246 (relocation manifest), #261 (shares)
- **wiring** — job type implemented but never registered with the scheduler, so `Submit` rejects it — PR 177 (sync/scrub/fix), #244, #245
- **wiring** — publisher or alert function with no production call site — PR 236 (space alerts)
- **wiring** — settings persisted but nothing reads them at runtime (schedules, create policy) — PR 182, 199
- **wiring** — a second, independent path bypasses the one Hoserva owns (NUT `SHUTDOWNCMD` skipping the clean array stop) — PR 254
- **wiring** — test or check script that no `make` target or CI job runs — #42, PR 221
- **scope** — API/CLI option accepted and silently ignored (`--follow`, `--json`, `dryRun`/`confirm`/`percent` payload) — PR 174
- **scope** — stub or sample data presented as real (sample diff rows, hard-coded "Confirmed", `Mounted` from disk count) — PR 174, 187, 210
- **scope** — feature covers only the first or common case (first NIC only, common timezones only, cache disks missing from a step) — PR 182, 213

## Partial failure and atomicity
- **partial-failure** — DB row committed before a later side effect (generated file, mount, Samba account, audit row, notification row) that can fail; error returned over a half-applied change, or retry blocked by the leftover row — PR 174, 206, 213, 216, 218, 228
- **partial-failure** — a compensating delete uses the request context, so a disconnect cancels the rollback and leaves the rows it was supposed to remove — PR 344
- **partial-failure** — rollback restores the row and files but not the live state (mounts) — PR 218
- **live-state** — change persisted and written to config but never applied to what is running (an idempotency early return keyed on a name that never changes; units written but the live mount left on its old branches) — PR 338
- **live-state** — a share-list mutation that bypasses Create/Update/Delete never runs PostCommit, so the array sequence keeps the previous share list — PR 344
- **resume** — a resumed run looks its target up by the key an earlier invocation already moved, or re-checks an identity field the run itself changed (filesystem UUID after its own format), so every resume fails — PR 338
- **reconcile** — regenerate-and-reconcile from a partial state (disks but no shares) deletes files another subsystem owns — PR 338
- **durability** — `rename` without an fsync of the directory; truncate-then-write of a settings file or certificate; archive written in place with `O_TRUNC`; a certificate and key replaced as two renames with no recovery if the process stops between them — PR 150, 174, 213, 236, 344
- **atomicity** — read-modify-write of a whole row lets concurrent partial updates overwrite each other — PR 182, 199
- **atomicity** — check-then-act on a path (validate, then re-resolve by name) — PR 228, 236
- **atomicity** — a maintenance check that returns before the mutation, so array stop can unmount while the mutation is still writing under the mountpoint — PR 344

## Fail-open and error handling
- **fail-open** — a safety or readiness check that continues on error (boot-disk detection with an unreadable mount table, identity-less format fallback) — PR 150, 159
- **fail-open** — `|| true` or a swallowed error inside a gate, so the gate reports PASS after a failure — PR 163, 210
- **fail-open** — a skip meant for one step applied to every step (unregistered mover skip also skipping sync/scrub) — PR 201
- **fail-open** — an input that matches nothing turns a protective change into a silent no-op (a removing disk not in the data-disk list leaves every branch RW) — PR 337
- **fail-open** — a destructive call treats a missing path as success while the disks are unmounted, so the data is still on disk — PR 344
- **errors** — state advanced before the operation succeeded, so a transient failure is never retried (alert state, spin-event cursor, a completed-stop flag cleared before the start's fallible checks) — PR 199, 246, 338
- **errors** — a secondary failure (a usage breakdown, a cancelled job context) discards a result that was already produced — PR 344
- **errors** — `os.IsNotExist` on a `%w`-wrapped error; use `errors.Is(err, fs.ErrNotExist)` — PR 201
- **errors** — infrastructure failure mapped to HTTP 400 with raw internal text — PR 216
- **errors** — external command without `CommandContext` or a timeout, able to block a request forever — PR 174, 206

## Web UI
- **ui-states** — `openapi-fetch` returns `{ error }` instead of throwing, and can return `error: undefined` on an empty non-OK body; ignoring either turns a failed request into empty, "no array" or success state — PR 187, 193, 199, 216, 228, 344
- **ui-states** — unhandled rejection or abort from a request inside an effect or a detached `Promise.all`, including a fetch queued in a microtask that cleanup does not cancel — PR 187, 199, 228, 344
- **ui-states** — `loading` stays true for a background refetch, so a page that treats it as "no data yet" unmounts dialogs on every poll — PR 344
- **ui-states** — an error replaces the confirmation text the operator needs in order to retry — PR 344
- **ui-states** — stale response overwrites the current selection (open A, open B, A's response lands) — PR 195, 228
- **ui-states** — error rendered behind an open dialog or overlay — PR 216, 228
- **ui-states** — unknown value rendered as zero (`?? 0`), so missing data reads as an empty disk or 0% — PR 337
- **i18n** — raw API enum shown instead of a catalog label for every value but the one the author tested — PR 337
- **i18n** — a user-visible fallback written as an English literal instead of a catalog key — PR 344
- **a11y** — controls without an accessible name; focus indicator removed with no replacement — PR 187, 199

## Validation and contracts
- **validation** — duplicate entries accepted (same device in two roles, repeated mount path, duplicate grant ids) — PR 150, 221
- **validation** — missing map key read as zero; integer overflow after parsing; empty payload skipping a required `confirm` — PR 150, 177, 236
- **mock-drift** — `cmd/mockapi` accepts what the production handler rejects, or defaults differently — PR 166, 182, 213, 228
- **spec-drift** — handler requires a field the OpenAPI schema marks optional — PR 213
- **validation** — mode selected by a flag's non-empty value rather than its presence, so an empty value falls through to the default path (`-ups-notify ""` starting a second daemon) — PR 337
- **validation** — a required phrase checked anywhere in a document instead of inside the section it must appear in — PR 337
- **identity** — first match taken when several candidates match (a weak-identity disk and its clone), or a stale path reported beside the disk that now holds it, so one record appears twice — PR 337
- **accounting** — capacity tracked per consumer (per share) instead of per filesystem, or a negative headroom summed into a total, so a plan overcommits or wrongly refuses — PR 337
- **planning** — planner and post-check disagree on which entries count (the planner skips symlinks, the post-check rejects them), so the refusal comes only after all the work, on every retry — PR 337

## Security
- **security** — host or URL checked by substring instead of parsed host; redirects not validated — PR 201, 228
- **security** — destructive CLI command that sends `confirm: true` itself — PR 193, 201
- **security** — secret-bearing URL or credential echoed into a persisted error string — PR 166
- **security** — user or state values written into a config format without escaping control characters — PR 254

## Tests
- **tests** — test passes vacuously (placeholder absence as success, `|| true` on the poll, assertion against an unintended path, `.first()` matching an older record) — PR 159, 163, 231, 337
- **tests** — unsynchronized read of state written by another goroutine — PR 166, 246
- **tests** — exact equality between two separately sampled system values — PR 236
- **tests** — parallel labs compile test binaries into one directory, so one lab replaces another's binary — PR 344
- **tests** — a restart check treats systemd active as API-ready, so the single login races the listener — PR 344

## External tool semantics
- **platform** — systemd unit names need `systemd-escape` (`-` → `\x2d`); `x-systemd.*` options are ignored in a native `.mount` unit — PR 150, 156
- **platform** — Samba `create mask` caps bits; `force create mode` adds them — PR 221
- **platform** — Debian: depend on `adduser` when using `addgroup`; AGPL text is not in `/usr/share/common-licenses` — PR 159
- **platform** — `mkfs.ext4` creates `lost+found`; smartctl reports NVMe health in a different section than ATA — PR 150, 254
- **platform** — a daemon that drops privileges reads its config as its own group; a root-only generated file locks it out (upsd.users needs root:nut 0640) — PR 337
- **platform** — systemd: a masked unit cannot start, and a disabled one was switched off on purpose — never start either — PR 338
- **platform** — exports(5) default-options field (`-opts`) stored as a client host — PR 344
- **platform** — systemd `systemctl stop` of a busy mount reports a failed job without EBUSY text, so a retry that matches only strerror never runs — PR 344
