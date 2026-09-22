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
- **partial-failure** — rollback restores the row and files but not the live state (mounts) — PR 218
- **durability** — `rename` without an fsync of the directory; truncate-then-write of a settings file; archive written in place with `O_TRUNC` — PR 150, 174, 213, 236
- **atomicity** — read-modify-write of a whole row lets concurrent partial updates overwrite each other — PR 182, 199
- **atomicity** — check-then-act on a path (validate, then re-resolve by name) — PR 228, 236

## Fail-open and error handling
- **fail-open** — a safety or readiness check that continues on error (boot-disk detection with an unreadable mount table, identity-less format fallback) — PR 150, 159
- **fail-open** — `|| true` or a swallowed error inside a gate, so the gate reports PASS after a failure — PR 163, 210
- **fail-open** — a skip meant for one step applied to every step (unregistered mover skip also skipping sync/scrub) — PR 201
- **errors** — state advanced before the operation succeeded, so a transient failure is never retried (alert state, spin-event cursor) — PR 199, 246
- **errors** — `os.IsNotExist` on a `%w`-wrapped error; use `errors.Is(err, fs.ErrNotExist)` — PR 201
- **errors** — infrastructure failure mapped to HTTP 400 with raw internal text — PR 216
- **errors** — external command without `CommandContext` or a timeout, able to block a request forever — PR 174, 206

## Web UI
- **ui-states** — `openapi-fetch` returns `{ error }` instead of throwing; ignoring it turns a failed request into empty, "no array" or success state — PR 187, 193, 199, 216, 228
- **ui-states** — unhandled rejection or abort from a request inside an effect or a detached `Promise.all` — PR 187, 199, 228
- **ui-states** — stale response overwrites the current selection (open A, open B, A's response lands) — PR 195, 228
- **ui-states** — error rendered behind an open dialog or overlay — PR 216, 228
- **a11y** — controls without an accessible name; focus indicator removed with no replacement — PR 187, 199

## Validation and contracts
- **validation** — duplicate entries accepted (same device in two roles, repeated mount path, duplicate grant ids) — PR 150, 221
- **validation** — missing map key read as zero; integer overflow after parsing; empty payload skipping a required `confirm` — PR 150, 177, 236
- **mock-drift** — `cmd/mockapi` accepts what the production handler rejects, or defaults differently — PR 166, 182, 213, 228
- **spec-drift** — handler requires a field the OpenAPI schema marks optional — PR 213

## Security
- **security** — host or URL checked by substring instead of parsed host; redirects not validated — PR 201, 228
- **security** — destructive CLI command that sends `confirm: true` itself — PR 193, 201
- **security** — secret-bearing URL or credential echoed into a persisted error string — PR 166
- **security** — user or state values written into a config format without escaping control characters — PR 254

## Tests
- **tests** — test passes vacuously (placeholder absence as success, `|| true` on the poll, assertion against an unintended path) — PR 159, 163, 231
- **tests** — unsynchronized read of state written by another goroutine — PR 166, 246
- **tests** — exact equality between two separately sampled system values — PR 236

## External tool semantics
- **platform** — systemd unit names need `systemd-escape` (`-` → `\x2d`); `x-systemd.*` options are ignored in a native `.mount` unit — PR 150, 156
- **platform** — Samba `create mask` caps bits; `force create mode` adds them — PR 221
- **platform** — Debian: depend on `adduser` when using `addgroup`; AGPL text is not in `/usr/share/common-licenses` — PR 159
- **platform** — `mkfs.ext4` creates `lost+found`; smartctl reports NVMe health in a different section than ATA — PR 150, 254
