# Known escapes

Defect patterns that passed a `task-verifier` PASS and were later confirmed
real by CodeRabbit on a `dev → main` pull request. `task-executor` checks its
change against this list before committing; `task-verifier` checks each
commit against it under layers 6 and 7. `cr-review` appends a line
whenever it fixes a confirmed finding whose pattern is not here yet.

One line per pattern: **category** — what goes wrong — where it was seen.
Keep it to patterns, not individual bugs; merge a new instance into an
existing line by adding its PR number.

## Wiring and missing scope
- **wiring** — service, store or engine built but never constructed or assigned in `cmd/hoservad/main.go`, so the handler field stays nil and every operation `501`s or returns empty data — PR 195 (metrics), 236 (share usage), 246 (relocation manifest), #261 (shares)
- **wiring** — job type implemented but never registered with the scheduler, so `Submit` rejects it — PR 177 (sync/scrub/fix), #244, #245
- **wiring** — a dependency captured once at startup from a value a live reconfiguration later supplies (parity engine, usage reader after a live array creation), so jobs and services wired from the snapshot keep nil until a restart — PR 370
- **wiring** — publisher or alert function with no production call site — PR 236 (space alerts)
- **wiring** — a runtime path hard-coded to the production location instead of derived from `--state-dir`/`--config-root`, so a `--dev` daemon writes outside its workspace — PR 395
- **wiring** — settings persisted but nothing reads them at runtime (schedules, create policy) — PR 182, 199
- **wiring** — a second, independent path bypasses the one Hoserva owns (NUT `SHUTDOWNCMD` skipping the clean array stop) — PR 254
- **wiring** — test or check script that no `make` target or CI job runs — #42, PR 221
- **wiring** — a CI step calls a `make` target whose prerequisites redo the job's own earlier install and build, doubling the job's cost — PR 474
- **wiring** — a build a deploy workflow now runs gets its toolchain setup (`setup-node` from `.nvmrc`, cache) only in the CI job that tests it, so the deploy builds on the runner's default version — PR 590
- **scope** — API/CLI option accepted and silently ignored (`--follow`, `--json`, `dryRun`/`confirm`/`percent` payload) — PR 174
- **scope** — stub or sample data presented as real (sample diff rows, hard-coded "Confirmed", `Mounted` from disk count) — PR 174, 187, 210
- **scope** — feature covers only the first or common case (first NIC only, common timezones only, cache disks missing from a step) — PR 182, 213
- **scope** — an operation that replaces or deletes a directory stops and job-scopes only the container that owns it, not every container whose bind mount lies inside it or holds it, so another keeps writing into the removed tree — PR 453

## Partial failure and atomicity
- **partial-failure** — DB row committed before a later side effect (generated file, mount, Samba account, audit row, notification row) that can fail; error returned over a half-applied change, or retry blocked by the leftover row — PR 174, 206, 213, 216, 218, 228
- **partial-failure** — a compensating delete, or a live apply that must follow an already-committed row, uses the request context, so a disconnect cancels it and leaves DB and live state disagreeing — PR 344, 382
- **partial-failure** — a compensating undo run after any failure of a multi-step operation without confirming the failure came before the change took effect (a swap that fails after the replacement took over), so the undo drops the record and kept state the recovery path needs — PR 510
- **partial-failure** — validation interleaved with writes, so a refused 400 still leaves the fields before the failing one applied — PR 382
- **partial-failure** — a regenerate step deletes the committed output before the replacement has been fetched, so a failed copy leaves it missing or partial — PR 567
- **partial-failure** — rollback restores the row and files but not the live state (mounts) — PR 218
- **partial-failure** — rollback restores a snapshot read before an unserialized write window, so a concurrent save that succeeded in between is silently reverted — PR 357
- **live-state** — change persisted and written to config but never applied to what is running (an idempotency early return keyed on a name that never changes; units written but the live mount left on its old branches) — PR 338
- **live-state** — a share-list mutation that bypasses Create/Update/Delete never runs PostCommit, so the array sequence keeps the previous share list — PR 344
- **resume** — a resumed run looks its target up by the key an earlier invocation already moved, or re-checks an identity field the run itself changed (filesystem UUID after its own format), so every resume fails — PR 338
- **resume** — a resumed job runs a plan persisted before a state change (a disk entering removal) without re-validating it against current state, so it writes where the plan is no longer allowed to — PR 370
- **reconcile** — regenerate-and-reconcile from a partial state (disks but no shares) deletes files another subsystem owns — PR 338
- **durability** — `rename` without an fsync of the directory; truncate-then-write of a settings file or certificate; archive written in place with `O_TRUNC`; a certificate and key replaced as two renames with no recovery if the process stops between them — PR 150, 174, 213, 236, 344
- **atomicity** — read-modify-write of a whole row lets concurrent partial updates overwrite each other — PR 182, 199
- **atomicity** — check-then-act on a path (validate, then re-resolve by name) — PR 228, 236
- **atomicity** — a clear or mark keyed only on the row, not on the state and holder the caller checked, so a transition that lands between read and write is wiped (a stale-alert mark over a success recorded after the check read the row) — PR 382, 433
- **atomicity** — a maintenance check that returns before the mutation, so array stop can unmount while the mutation is still writing under the mountpoint — PR 344
- **atomicity** — a cancel or stop flag read in one lock hold and the start done in a later one, so a request that lands in the gap is accepted and then ignored — PR 412
- **atomicity** — an admission gate checked only when a job is submitted, not again when a queued job is dispatched or an interrupted one resumed, so a state change while it waited (a migration recording its pending array) is bypassed — PR 592
- **ordering** — a side effect that takes a bounded resource (a pre-change archive's retention slot) runs before the admission check that can refuse the operation, so refused retries use up what real changes rely on — PR 412
- **atomicity** — two paths that each rebuild and publish the same live object (degraded acknowledge vs. array-sequence rebuild) under no shared lock, so one publishes state computed before the other's change landed and silently undoes it — PR 394
- **atomicity** — a mutex held across slow I/O that only needs a value read under it (a client's request body streaming to disk, a whole-file read that validates a large baseline), so every other operation on that lock (status, delete, a running job's commit) stalls for as long as the I/O takes — PR 575, 589

## Fail-open and error handling
- **fail-open** — a safety or readiness check that continues on error (boot-disk detection with an unreadable mount table, identity-less format fallback) — PR 150, 159
- **fail-open** — `|| true` or a swallowed error inside a gate, so the gate reports PASS after a failure — PR 163, 210
- **fail-open** — a skip meant for one step applied to every step (unregistered mover skip also skipping sync/scrub) — PR 201
- **fail-open** — an input that matches nothing turns a protective change into a silent no-op (a removing disk not in the data-disk list leaves every branch RW) — PR 337
- **fail-open** — a destructive call treats a missing path as success while the disks are unmounted, so the data is still on disk — PR 344
- **fail-open** — a cleanup step skipped because a status signal still reads good from an earlier successful run (stale freshness/lastSyncAt), not from the run that just failed — PR 357
- **fail-open** — a paired stop-then-start recovery step reads live status to decide whether the start is still owed, but live status can't distinguish "never touched" from "an earlier attempt's stop succeeded and its start didn't", so a retry after a failed start silently skips finishing it and reports success — PR 421
- **fail-open** — a teardown discards each step's error and returns the run's own status, so a mount or device it could not remove stays behind while the run exits 0 — PR 579
- **fail-open** — a config edit that matches only the exact expected line (`sed s/^Components: main$/…/`) silently changes nothing when the line carries more values, so the step it enables fails later with an unrelated error — PR 581
- **errors** — state advanced before the operation succeeded, so a transient failure is never retried (alert state, spin-event cursor, a completed-stop flag cleared before the start's fallible checks, and not put back by a rollback that did complete the stop) — PR 199, 246, 338, 395
- **errors** — a secondary failure (a usage breakdown, a cancelled job context) discards a result that was already produced — PR 344
- **errors** — `os.IsNotExist` on a `%w`-wrapped error; use `errors.Is(err, fs.ErrNotExist)` — PR 201
- **errors** — infrastructure failure mapped to HTTP 400 with raw internal text — PR 216
- **errors** — a refusal of the user's input returned as an unnamed `errors.New`, so the API's sentinel classifier cannot match it and a bad request answers 500 — PR 592
- **errors** — a catch-all default maps every unclassified error to 502, so a local database or filesystem failure is blamed on an upstream; reserve 502 for errors wrapped as coming from the external process — PR 491
- **errors** — a per-group result (one row per image) failed by one member that cannot be evaluated, hiding the result its comparable siblings produced — PR 491
- **efficiency** — a per-item lookup that resolves its item by listing every item, called once per item on a request path, so one request costs N full listings — PR 510
- **errors** — external command without `CommandContext` or a timeout, able to block a request forever — PR 174, 206
- **errors** — one deadline shared across a multi-step sequence, so a slow but successful early step leaves a later step too little time and it fails into a needless rollback or a leftover — PR 430
- **errors** — a fixed deadline sized for the small case applied to a transfer whose size is unbounded (a multi-gigabyte archive over rclone), so large inputs fail on size alone — PR 453
- **errors** — a caller accepts a helper's exit 0 as a result while the helper exits 0 with empty output when its input is missing (`dev-diff.sh --list` without a local `dev`), so "unavailable" reads as "empty" — PR 575

## Web UI
- **ui-states** — `openapi-fetch` returns `{ error }` instead of throwing, and can return `error: undefined` on an empty non-OK body; ignoring either turns a failed request into empty, "no array" or success state — PR 187, 193, 199, 216, 228, 344
- **ui-states** — unhandled rejection or abort from a request inside an effect, a `void`-called handler with only `try/finally`, or a detached `Promise.all`, including a fetch queued in a microtask that cleanup does not cancel — PR 187, 199, 228, 344, 357
- **ui-states** — `loading` stays true for a background refetch, so a page that treats it as "no data yet" unmounts dialogs on every poll — PR 344
- **ui-states** — an error replaces the confirmation text the operator needs in order to retry — PR 344
- **ui-states** — a dialog derives its options from state its own first step already changed, so a failure in the second step removes the retry (save mode, then relocate) — PR 357
- **ui-states** — a "touched" flag sends a cleared field as an empty value the schema rejects, instead of omitting it to keep the stored secret — PR 357
- **drift** — a domain rule (which mode change relocates where, which removal states leave the pool) copied between pages or packages instead of shared from one definition — PR 357, 370
- **drift** — a check enforces an invariant that a documented maintenance procedure breaks (layout check requires every `versions.json` entry built; docs drop old versions with `onlyIncludeVersions`), so following the docs fails the build — PR 590
- **ui-states** — stale response overwrites the current selection (open A, open B, A's response lands) — PR 195, 228
- **ui-states** — error rendered behind an open dialog or overlay — PR 216, 228, 382
- **ui-states** — a dialog, overlay or panel dismissable (Escape, backdrop, Cancel) while its request runs, so the later failure lands on a closed surface — PR 382
- **drift** — a UI rule derived from one flow's backend contract (schema versions differ → no change groups) applied unchanged to a second flow whose backend does send that data, so the page hides what the server returned — PR 474
- **drift** — a hand-kept web list of an API enum (notification event types) not extended when the spec gains a value, so the new value gets no settings row or label — PR 531
- **ui-copy** — help text implies an operation leaves the system ready for a physical step (pull the disk) when a further required step remains — PR 370
- **ui-states** — unknown value rendered as zero (`?? 0`), so missing data reads as an empty disk or 0% — PR 337
- **ui-states** — a download's object URL revoked in the same task as `anchor.click()`, so a browser that resolves the download asynchronously finds the blob gone and saves nothing, with no error shown — PR 589
- **ui-states** — a reload that settles an unanswered save replaces the editor's unsent text with the server's copy and keeps nothing to restore it from, including edits made between a failed reload and its retry — PR 555
- **ui-states** — a second load path (a reload after a save) calls the raw operation instead of the loader that maps terminal answers (not found, no template), so a permanent state renders as a failure whose retry fails forever — PR 555
- **i18n** — raw API enum shown instead of a catalog label for every value but the one the author tested — PR 337
- **i18n** — a user-visible fallback or formatted value (duration units, separators) written as an English literal instead of a catalog key — PR 344, 357, 527, 531
- **i18n** — a count-bearing catalog key with no `_one`/`_other` forms, so a count of one reads "1 files" — PR 370
- **i18n** — a catalog key built from an API value (`reasons.${reason}`) with no `defaultValue`, so a value the catalog lacks shows the raw key path — PR 542
- **a11y** — controls without an accessible name; focus indicator removed with no replacement — PR 187, 199

## Validation and contracts
- **validation** — duplicate entries accepted (same device in two roles, repeated mount path, duplicate grant ids, a second cache that silently replaces the first when only some branches of a resolver check) — PR 150, 221, 592
- **validation** — a file accepted because it parses, without the structure its format requires (a compose.yaml that is empty, comment-only or has no `services` map), so it is reported and counted as read — PR 579
- **validation** — an "exact duplicate" rule compares only some fields, so entries that differ in access mode or bind address count as identical and one is silently dropped — PR 491
- **validation** — missing map key read as zero; integer overflow after parsing; empty payload skipping a required `confirm` — PR 150, 177, 236
- **validation** — a helper carrying a single-value side effect (a "given more than once, the last is used" note) reused for a field that accumulates a list, so the operator is told kept values were dropped — PR 542
- **contract** — a size limit set on a decoded value (48 KiB of template text) under a transport limit (64 KiB request body) that the encoding can inflate past, so a valid maximum input is refused before the handler sees it — PR 542
- **mock-drift** — `cmd/mockapi` accepts what the production handler rejects, or defaults differently — PR 166, 182, 213, 228, 382
- **spec-drift** — handler requires a field the OpenAPI schema marks optional — PR 213
- **doc-drift** — a design doc names a state or identifier the code never persists — PR 370
- **doc-drift** — a command example in a skill or prompt drops a required operand (`issue-edit --body-file` with no issue number or file), so an agent following it literally fails — PR 412
- **doc-drift** — a design doc states an external source's conditions more broadly than the source does (an advisory's exploit trigger), so a reader misjudges the exposure — PR 433
- **doc-drift** — a dispatch prompt tells an agent to do what its agent definition forbids (run scripts outside its workspace), so the agent cannot obey both — PR 491, 546
- **doc-drift** — a skill adopts another skill's rule (hold back a commit on a fresh `blockedBy`) without the check that makes it hold (`git log origin/dev..dev` before every push), so the next push publishes what was held — PR 546
- **doc-drift** — a code comment still describes behaviour a later fix removed, inviting the next change to put it back — PR 394
- **doc-drift** — a function's doc promises a cost bound its loop does not keep (a status query "only while caught up" run on every chunk), so a large stream pays a database read per buffer — PR 474
- **doc-drift** — a design doc's command table lists only one of a command's alternative forms (`--flash-backup` without `--flash-device`) — PR 575
- **mirror-drift** — a client-side mirror of backend rendering applies a looser check than the Go code for an edge input (an IPv4-mapped address bracketed as IPv6) — PR 357
- **validation** — mode selected by a flag's non-empty value rather than its presence, so an empty value falls through to the default path (`-ups-notify ""` starting a second daemon) — PR 337
- **validation** — a required phrase checked anywhere in a document instead of inside the section it must appear in — PR 337
- **identity** — first match taken when several candidates match (a weak-identity disk and its clone), or a stale path reported beside the disk that now holds it, so one record appears twice — PR 337
- **accounting** — capacity tracked per consumer (per share) instead of per filesystem, or a negative headroom summed into a total, so a plan overcommits or wrongly refuses — PR 337
- **planning** — planner and post-check disagree on which entries count (the planner skips symlinks or all of lost+found, the post-check rejects them), so the refusal comes only after all the work, on every retry — PR 337, 394
- **validation** — a list input split in a way that silently drops entries (bash `read` stops at the first newline and drops a trailing empty field) instead of refusing the malformed input — PR 403

## Security
- **security** — host or URL checked by substring instead of parsed host (including allowlist entries left unanchored beside anchored ones); redirects not validated — PR 201, 228, 474
- **security** — an allowlist exemption decided on a truncated capture (a URL cut short inside a `${…}` interpolation), so a fixed host after the cut passes — PR 491
- **security** — a "safe location" rule admits a whole root by prefix, including a sensitive subtree it holds (the cache and Docker's data-root on it), so a mount of that subtree is classified as harmless — PR 510
- **security** — destructive CLI command that sends `confirm: true` itself — PR 193, 201
- **security** — secret-bearing URL or credential echoed into a persisted error string — PR 166
- **security** — a CI job that runs pull-request code checks out with the default `persist-credentials`, leaving `GITHUB_TOKEN` in `.git/config` for the code under test to read — PR 546
- **security** — user or state values written into a config format without escaping control characters — PR 254
- **security** — a file the CLI saves for the user that names accounts, shares or containers is left world-readable (0644) where the daemon keeps the same data 0600 — PR 575

## Tests
- **tests** — test passes vacuously (placeholder absence as success, `|| true` on the poll, assertion against an unintended path, `.first()` matching an older record, a tool exit code shared by "blank" and "could not open", a precondition gate refusing before the injected failure is reached, any non-zero exit accepted as the expected refusal without its diagnostic, a fixture key spelled differently from the one the parser reads so the scenario is never built) — PR 159, 163, 231, 337, 403, 421, 430, 567, 589, 592
- **tests** — a short real deadline also bounds setup I/O ahead of the code under test (the SQLite write entering maintenance), so on a loaded runner the error comes from the setup step and an `errors.Is` check still passes; trip the deadline once the step under test is reached and assert its own error text — PR 433
- **tests** — an end-to-end failure detector defined as "any banner but this list of informational ones", not extended when the change adds a new informational note, so the expected note fails the journey — PR 474
- **tests** — unsynchronized read of state written by another goroutine — PR 166, 246
- **tests** — exact equality between two separately sampled system values — PR 236
- **tests** — parallel labs compile test binaries into one directory, so one lab replaces another's binary — PR 344
- **tests** — a restart check treats systemd active as API-ready, so the single login races the listener — PR 344
- **tests** — a process probe matches any process in `/proc` instead of this test's own child, so an unrelated one triggers the next step early — PR 357
- **tests** — a fixture or capture script clears every resource on a shared daemon (all Docker containers and networks) instead of refusing a populated one and removing only what it created — PR 567
- **tests** — a fixture generator shared by several variants hard-codes a value only some of them support (a cache-pool path), so a variant without it records a state its own spec rules out — PR 568
- **tests** — a readiness gate waits on more than the acceptance criterion measures (the cache disk in an array-disk settle), so unrelated activity fails it — PR 357
- **tests** — nested mounts torn down in mount-table order (parent before child), so the parent stays busy — PR 357
- **tests** — a check's cleanup runs after a later step shadows what it must remove (a mount over the directory holding a stray probe), so the leftover survives into later steps — PR 395
- **tests** — a test swaps process-global state (the `log` output) and its cleanup restores a hard-coded default rather than the value it saved, clobbering whatever an earlier caller set — PR 527
- **docs** — a design doc or spike verdict says a behaviour is verified (by fixtures, the lab or the scan) when the check that would verify it has not been built yet — PR 562
- **docs** — a fixture or spec comment states a property only one build tier produces (an L2-only partition layout) as if every build had it — PR 568

## External tool semantics
- **platform** — systemd unit names need `systemd-escape` (`-` → `\x2d`); `x-systemd.*` options are ignored in a native `.mount` unit — PR 150, 156
- **platform** — Samba `create mask` caps bits; `force create mode` adds them — PR 221
- **platform** — Debian: depend on `adduser` when using `addgroup`; AGPL text is not in `/usr/share/common-licenses` — PR 159
- **platform** — `mkfs.ext4` creates `lost+found`; smartctl reports NVMe health in a different section than ATA — PR 150, 254
- **platform** — a daemon that drops privileges reads its config as its own group; a root-only generated file locks it out (upsd.users needs root:nut 0640) — PR 337
- **platform** — systemd: a masked unit cannot start, and a disabled one was switched off on purpose — never start either — PR 338
- **platform** — exports(5) default-options field (`-opts`) stored as a client host — PR 344
- **platform** — `[[ -e path ]]` is false for a dangling symlink, so a script that then creates the path fails on it; test `-L` too — PR 382
- **platform** — systemd `systemctl stop` of a busy mount reports a failed job without EBUSY text, so a retry that matches only strerror never runs — PR 344
- **platform** — a filesystem path concatenated into a URI or DSN (SQLite `file:`) unescaped, so a `?`, `#` or `%` in it opens a different file and drops the query options — PR 403
- **platform** — Docker `--entrypoint` is one executable, never an argument list; splitting it on spaces runs a different program — PR 491
- **platform** — `git merge --ff-only origin/<b>` as a sync check also succeeds when the local branch is ahead; compare `HEAD` to the remote ref — PR 491
- **platform** — `git check-ignore` skips tracked paths unless given `--no-index`, so a "this source file is not ignored" check always passes even when a rule hides the directory — PR 527
- **platform** — GitHub Actions: a job `timeout-minutes` at or below a step timeout it contains, so the job backstop cancels a step still inside its own bound — PR 403
- **platform** — a command's stdout parsed as data while captured with `2>&1`, so a warning the tool prints on stderr with exit 0 becomes a bogus record — PR 562
- **platform** — Unraid mover direction comes from each share's Primary/Secondary storage and Mover action: a `prefer` share moves onto the cache and an `only` share never moves, so "run the mover" does not empty the cache — PR 562
