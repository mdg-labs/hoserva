# Hoserva — Open Questions and Recommended Defaults

The single register of everything not yet settled. **No question here is a bare question**: each carries a recommended default and a one-line rationale, and the rest of the document set is written *as if the default were adopted*. Overriding a default means editing its entry here and the sections it lists under **Affects** — nothing else should need to change.

A default is not a decision. Decisions live in the decision log (doc 00 §5) and need a new reason to reopen; defaults here need only a better idea. Promote a default to the decision log once it has survived contact with real code.

### Status legend

| Status | Meaning |
|---|---|
| **Default** | Docs are written to this. Override freely before the phase it gates. |
| **Spike** | Default adopted provisionally; a Phase 0 spike (doc 07 §1) confirms or replaces it. |
| **External** | Depends on someone outside the project (a maintainer, a lawyer, a registry). Default is what we do while waiting. |
| **Maintainer** | Default is recommended, but only the maintainer can adopt it (legal or irreversible). |
| **Settled → Dn** | Promoted to the decision log; the entry stays only so its number keeps resolving. |

### Where open questions lived before this doc

Consolidated from: doc 00 §6 (license), doc 02 §1 (spindown "open risk"), doc 04 §4 (catalog licensing posture), doc 05 §2 (variant table "Test"/"Verify" rows), doc 07 §3 (the former open-questions section), doc 08 ("Remaining hands-on work"), plus gaps and contradictions found in a full cross-read of docs 00–12. Doc 07 §3 now points here.

---

## Index by deadline

| Gate | Questions |
|---|---|
| **Now** (repo is public) | Q2 (Q1 settled → D17) |
| **Before Phase 1** | Q3–Q21, Q28–Q32, Q40, Q42, Q44–Q46, Q48, Q49, Q59, Q60, Q63, Q66–Q70, Q74, Q76, Q78, Q79, Q84, Q85, Q86, Q87 |
| **Before Phase 2** | Q26, Q27, Q41, Q43, Q61, Q71–Q73, Q75, Q77, Q80 |
| **Before Phase 3** | Q22–Q25, Q36–Q39, Q62, Q64, Q65, Q81–Q83, Q88, Q89, Q90 (Q33–Q35 settled → D19) |
| **Before Phase 3.5** | Q51–Q58 |
| **Before 1.0** | Q47, Q50 |

---

## Governance

### Q1 — License
**Status:** Settled → D17 · **Affects:** doc 00 §6, repo `LICENSE`

**Settled: AGPL-3.0**, promoted to the decision log as D17, with `LICENSE` added at the repository root. The reasoning stays in doc 00 §6.

### Q2 — Contribution terms
**Status:** Default · **Gate:** now · **Affects:** `CONTRIBUTING.md`, `scripts/devenv/hooks/`, `.github/workflows/ci.yml`

**Default: DCO sign-off (`Signed-off-by:`), no CLA.**
A CLA signals an intent to relicense, which undercuts AGPL's trust signal exactly where the project needs it. A hosted monitoring service can be its own codebase and needs no relicensing of this one.

Enforced, not just documented: `make hooks-install` (a repo-tracked `prepare-commit-msg` hook) signs off every commit automatically for a human clone or an `orchestrate` scratch clone, and CI's `dco` job rejects a push or pull request carrying a commit without one. Every pre-existing commit on `main` and `beta` was retroactively signed off the same way (a one-time history rewrite, done directly rather than through the issue tracker).

### Q3 — Where the public docs site lives
**Status:** Default · **Gate:** Phase 1 · **Affects:** doc 05 §7, doc 06 §9, doc 12 §2, §7, Q90

**Default: `site/` at the repo root (Docusaurus); design docs stay in `docs/internal/`.**
`docs/` already holds these internal design docs, so the site cannot live there: a Node project mixed into the design-doc folder makes both harder to split out later (doc 12 §7). Docusaurus is chosen over Astro Starlight because versioned docs are a requirement (Q90) and Docusaurus has them natively, and its React matches `web/`'s stack.

### Q90 — Versioned documentation
**Status:** Default · **Gate:** Phase 3 · **Affects:** doc 05 §7, doc 12 §2, Q3, Q63, Q66

**Default: every stable minor release gets one docs snapshot, kept in the repo, and the current docs are published beside them.**
- **One snapshot per stable minor.** `npx docusaurus docs:version X.Y`, run in the release-prep commit on `dev` for the first stable `vX.Y.0`, writes `site/versioned_docs/version-X.Y/`, `site/versioned_sidebars/version-X.Y-sidebars.json` and `site/versions.json`, and reaches `main` through the normal `dev` to `main` pull request before the tag. A stable patch release (`vX.Y.Z`, Z > 0) gets no new snapshot; it updates `version-X.Y` in place if something user-facing changed. Beta pre-release tags are never snapshotted: their users read the current docs.
- **Before the first stable release** there are no versions. The current docs are the whole site, at the root, with the "unreleased" banner.
- **Once a version exists** the root serves the latest stable version. The current docs (`site/docs/`, that is `main`) are served at `/next/` with the "unreleased" banner and `noindex`. Older versions carry the "unmaintained" banner, and the navbar has a version dropdown.
- **Docs fixes** land in `site/docs/`, and also in the latest stable snapshot when they correct something wrong there. Older snapshots are not maintained.
- **All versions stay published** until build time or the 900 MiB Pages canary (`scripts/release/assemble-pages-site.sh`) forces the oldest to be dropped: it is removed from `site/versions.json` and its `versioned_docs/` and `versioned_sidebars/` files are deleted (git history keeps them). Not with `onlyIncludeVersions`, which keeps the version in `versions.json` unbuilt and so fails the layout check `make site-build` runs.
- **Pages needs no versioning logic.** One build of `main` contains every version, and `pages.yml` always assembles from `main`'s tree (`assemble-pages-site.sh` runs `make site-build` and publishes `site/dist`), so an edit to an old snapshot goes live on the next push to `main`.

Fix old versions with a commit, not a release: snapshots kept in the repo can be corrected by an ordinary commit, where building each version's docs from its release tag would need a new Hoserva release to change them. `make site-build` proves the layout above, including the "a version exists" layout on a throwaway snapshot made in a temporary copy of `site/`. The check that refuses a stable tag without its snapshot belongs to the release workflow and is tracked as its own issue.

---

## Platform

### Q4 — Supported base OS
**Status:** Default · **Gate:** Phase 1 · **Affects:** doc 00 D2, doc 01 §1, doc 06 §4, §7

**Default: Debian 13 (trixie) only for 1.0. Debian 14 is added when it releases. Ubuntu is community best-effort, not tested.**
Debian 12 is already oldstable. Trixie's 6.12 kernel gives FUSE passthrough and fanotify FID reporting (Q13), both of which the storage design leans on. Supporting Ubuntu doubles the L3 matrix for an audience that can usually cope.

### Q5 — CPU architectures
**Status:** Default · **Gate:** Phase 1 · **Affects:** doc 01 §1, doc 07 §1

**Default: amd64 is supported. arm64 is built in CI from the first commit and labelled unsupported until the public beta covers arm64 boards (doc 06 §6).**
Go cross-compilation is free as long as the SQLite driver is pure Go (Q6). A support claim needs real boards' storage controllers, which emulation can't show. That resolves the tension between doc 01 ("arm64 matters") and doc 07 ("ARM builds post-1.0").

### Q6 — SQLite driver
**Status:** Default · **Gate:** Phase 1 · **Affects:** doc 01 §1

**Default: `modernc.org/sqlite` (pure Go, no CGO).**
D3 promises a single static binary. `mattn/go-sqlite3` needs CGO, which breaks static linking and cross-compiling. Its speed advantage doesn't matter at a NAS's config-database scale.

### Q60 — Schema, schema-migration and query tooling
**Status:** Default — **`sqlite-migrate` (MIT, `github.com/mdg-labs/sqlite-migrate`) replaces sqldef-as-a-library plus Hoserva's own ~1,650-line checksum/drift/allow-list stack, for generation, checksum, drift and (via a Hoserva-owned apply loop built on its exported building blocks) transactional apply. The four gaps this issue was asked to verify (`ADD COLUMN ... REFERENCES` #133, a contract step's own `ON CONFLICT DO UPDATE` #134, a trigger with a DML body #135, `CREATE VIRTUAL TABLE`) were checked hands-on against the published module and decided by the maintainer on 2026-09-17 — see each paragraph below. `internal/store/tools/dbmigration`, `internal/store/tools/dbcheck`, `internal/store/{checksum,drift,safety,allowlist,columnrefs}.go` and `internal/store/migrations/contracts` are retired outright, not ported. Hoserva's own code narrows to the `transforms/` data-copying layer, now bound to a migration's checksum, and the apply loop that runs a transform inside the same transaction as its migration — `sqlite-migrate`'s own library has no hook for that. The newer-than-binary refusal and the doc 10 §1 snapshot path/retention need no bespoke code at all: both are already covered by the library's own exported API** · **Gate:** Phase 1 (foundation) · **Affects:** doc 00 D16, doc 01 §4, doc 06 §2, doc 12 §2, §3

**Default:**

| Job | Tool |
|---|---|
| Generate the next schema migration from `schema.sql`, checksum and drift check | `sqlite-migrate` (MIT), used both as an embedded Go library (`github.com/mdg-labs/sqlite-migrate`) from `hoservad`, and as a CLI (`sqlite-migrate generate` / `check`) via `make db-migration` / `make db-check` |
| Data-copying transforms a schema diff can't express (`size_mb` → `size_bytes`) | Hoserva's own Go code in `internal/store/transforms/`, bound to a migration's checksum, run by Hoserva's own apply loop, not `sqlite-migrate`'s |
| Typed queries | sqlc (MIT), `engine: sqlite`, reading `schema.sql`, run via `make gen` — unchanged |
| Applying migrations on the user's machine | Hoserva's own thin apply loop in `internal/store` (package `store`), built on `sqlite-migrate`'s exported `LoadDir`/`PendingMigrations`/`Checksum`/`Snapshot`, migrations embedded via `go:embed` — no migration tool ships in the `.deb` |

**Confirmed against a source build of `github.com/mdg-labs/sqlite-migrate` (commit `00c65f1`, wrapping `github.com/sqldef/sqldef/v3` v3.11.23 and `modernc.org/sqlite` v1.59.0), generating into a scratch copy of `schema.sql` — never against the tracked `internal/store/migrations/`:**

- **The published module could not initially be fetched the way its own README said to install it — found during this investigation, now fixed.** `go install`/`go run github.com/mdg-labs/sqlite-migrate/cmd/sqlite-migrate@latest` failed outright: `go: ... create zip: docs/internal/sqlite-migrate — Spec MVP Doc.md: malformed file path ... invalid char '—'` — a doc filename in the published repository contained an em dash, which Go's module zip format rejects before the tool ever runs. Every confirmation below used a plain `git clone` of the repository and a local `go build` to work around it at the time. This was the maintainer's own module; it was reported separately rather than folded into this issue's scope, as `mdg-labs/sqlite-migrate#42`, and is fixed as of the published `v0.1.1` — confirmed directly: `go get github.com/mdg-labs/sqlite-migrate/cmd/sqlite-migrate@latest` now resolves to `v0.1.1` and fetches and builds cleanly, no workaround needed. #139 can use the ordinary documented install path.
- **Every table must be declared `STRICT`; `sqlite-migrate` refuses to parse a schema that isn't** (stated in its own README) — confirmed: `generate` against Hoserva's actual `schema.sql` fails outright, `parse schema.sql: table "jobs" is not STRICT: schema.sql tables must be declared STRICT`. None of Hoserva's five current tables are STRICT. Adding `STRICT` to every table is therefore a required, one-time `schema.sql` authoring change for #139, not optional — confirmed safe once added: a STRICT-only copy of the real `schema.sql` generates its baseline migration cleanly, with every `CREATE TABLE` body byte-for-byte unchanged apart from the trailing `) STRICT;`.
- **A new column's own `REFERENCES` clause is preserved, not dropped — gap #133 does not reproduce.** Adding `parent_id TEXT REFERENCES users (id)` to an existing table generates `ALTER TABLE machine_key_check ADD COLUMN parent_id text REFERENCES users (id);` intact, and `sqlite-migrate check` reports no drift against it. `internal/store/columnrefs.go`'s `SchemaColumnReferences`/`restoreAddColumnReferences` post-processing step (#133) has nothing to restore under this tool and is retired with the rest of `internal/store/tools/dbmigration`.
- **Every rebuild a schema diff needs — including one plain `ALTER TABLE` can't express — is generated automatically, never hand-written.** Confirmed with a column type change (`jobs.progress INTEGER` → `REAL`) and, separately, a destructive column drop (with `--allow-destructive`): `generate` writes the complete rebuild sequence itself (`CREATE TABLE "..._sqlite_migrate_new"`, a column-by-column `INSERT ... SELECT` copy by `rowid`, `DROP TABLE`, `RENAME TO`, re-creating every index) with no hand-editing and no registration step of any kind. There is no `contracts/`-style mechanism to build, because there is no longer a class of migration a human writes by hand for the tool to police. **This is why gap #134 doesn't reproduce either, for a more basic reason than "fixed": the scenario needs a hand-authored contract-step file, and under `sqlite-migrate` no migration file is ever hand-authored** — every rebuild's own copy step is a plain `INSERT ... SELECT` with no `ON CONFLICT` clause, confirmed absent from the tool's generator and rebuild source entirely (`ON CONFLICT`/`OnConflict` appears in neither, only in its own tests).
- **There is no allow-list, and no vocabulary vetting of any kind, of what a migration file may contain — a deliberate design difference, not a residual gap to work around.** `sqlite-migrate`'s checksum is a pure self-consistency check (a header comment's hash must match a fresh hash of the body below it), never a content classifier: a migration file hand-edited to append `DELETE FROM t WHERE val < 0;`, with its header checksum recomputed to match, passes `sqlite-migrate check` cleanly — confirmed directly. `check`'s drift comparison only compares the *resulting schema structure* after replay against `schema.sql`, never the DML a migration executes along the way, so an injected `DELETE`/`UPDATE`/`INSERT` is exactly as invisible to it with an `ON CONFLICT` clause as without one. The safety property `internal/store/allowlist.go` enforced at runtime — every migration statement is one of a known-safe shape — has no equivalent here; it is replaced by the same process discipline `api/gen/` already relies on: a migration file is committed only as `sqlite-migrate generate`'s literal, reviewed output, never hand-edited afterward.
- **`CREATE TRIGGER` and `CREATE VIEW` are invisible to `sqlite-migrate` entirely — a finding beyond the four named gaps, and more severe than any of them.** Adding a brand-new trigger to `schema.sql` (a plain `SELECT`-only body and a DML body were both tried) makes `generate` report `no changes detected`, and `check` reports `ok` even though `schema.sql` now declares a trigger no migration in the journal ever created. Confirmed at the source: `internal/schemadiff.Schema` — the package both `generate` and `check` diff through — has exactly one field, `Tables map[string]*Table`, and its own introspection query is `SELECT ... FROM sqlite_master ... WHERE sqlite_master.type = 'table'`; triggers and views are never read, diffed or drift-checked in this version. Hoserva's `schema.sql` has no trigger or view today, so this blocks nothing now, but gap #135's question (classifying a DML-bodied trigger as contract-only) isn't answered so much as made moot for a different reason than #134's: adopting a trigger or view under the current tool would need hand-verification against a live database on every change, with no generation or drift-check support at all. Per this issue's own constraint, that stays the same documented, deliberate posture Q60 already held for `CREATE VIRTUAL TABLE` below — not silently unblocked, and not silently assumed safe either — now extended to triggers and views for a tool-shaped reason rather than a classifier gap.
- **`CREATE VIRTUAL TABLE` (FTS5) is fully and deliberately supported, both ways — gap #4 is resolved, not merely unreproduced.** A `docs`/`docs_fts` pair (`content='docs', content_rowid='id'`) generates and drift-checks cleanly, and the library's own test suite (`TestParse_VirtualTableFTS5ExemptFromSTRICT`, `TestParse_VirtualTableFTS5ShadowTablesAreHidden`, `TestDiff_VirtualTable_AddedRemovedChanged`) shows this is deliberate: a virtual table is exempted from the STRICT requirement above, and FTS5's own shadow tables are hidden from the diff so they never surface as unexpected tables. Per this issue's constraint, this resolves the question honestly instead of silently unblocking a feature: `CREATE VIRTUAL TABLE` is confirmed data-safe under the adopted tool, but Hoserva still has no search feature that needs it, so **whether to use it stays a future, feature-scoped product decision** — unlike triggers/views above, where the limitation is the tool's, not a scoping choice.
- **The library exposes exactly the daemon-startup hooks Hoserva needs, except one.** `sqlitemigrate.Runner` (the embeddable Go API `apply`'s CLI wraps) exports `DBPath`, `SnapshotDir` and `RetainSnapshots` directly — pointing `SnapshotDir` at `/var/lib/hoserva/backups/pre-migration/` with `RetainSnapshots: 3` reproduces doc 10 §1's convention with no Hoserva-side snapshot code at all (confirmed by reading `Runner.Snapshot`, and by a live `apply --yes` run, which wrote its snapshot next to the target db file — the library's own default for an empty `SnapshotDir`). Refusing to start against a database newer than the binary needs no bespoke detection either: `PendingMigrations` returns a `MissingMigrationError`, and `apply`/`Runner.Apply` fail outright (exit 1, "recorded as applied but its file is missing"), the moment the database has an applied migration this binary's embedded `migrations/` doesn't include — confirmed directly by pointing `apply`/`status` at a migrations directory missing an already-applied file. The one real gap: `Runner.Apply` runs its whole batch — snapshot, open, suspend `PRAGMA foreign_keys`, begin, execute every pending migration's SQL, record its checksum, `foreign_key_check`, `integrity_check`, commit — as one closed function with no callback of any kind (confirmed by reading `applyInTransaction`), and its own `checkForeignKeys`/`checkIntegrity` are unexported, so they can't be reused piecemeal either. There is no way to run a Hoserva transform between one migration's SQL and the next inside `Runner.Apply`'s own transaction. Hoserva's own thin apply loop (issue #139) therefore still has to exist, built from the library's *other* exported, directly reusable pieces (`LoadDir`, `PendingMigrations`, `Checksum`, `Snapshot`) plus Hoserva's own transaction loop — run a migration's SQL, then any transform registered against that migration's checksum, before the next migration; re-issue the same two `PRAGMA` checks by hand before commit; and write the same `schema_migrations` bookkeeping row shape `sqlite-migrate`'s own `Runner` does, so `sqlite-migrate status`/`check`/`verify` keep working unmodified against the resulting database.
- **Several of the old parser-rejection authoring rules no longer reproduce**, spot-checked against the bundled `sqldef/v3` v3.11.23 (a materially newer parser than the v1.0.7 the previous version of this entry tested): `NOT NULL COLLATE ...` (the previously-rejected clause order), `DEFAULT (datetime('now'))`, and an unquoted `status` column all generate cleanly now. This issue did not re-run the old entry's full list (`CHECK`, `UNIQUE`, foreign-key actions, `COLLATE` inside a `CHECK`, generated columns, `WITHOUT ROWID`, an unquoted `Key`/`value`) — #139 should treat that list as unconfirmed rather than still-required, and verify only what `schema.sql` actually ends up needing.

**What this does, and doesn't, change about the design:** D16's expand (new structure) → transform (copy, converting) → contract (remove old structure, later release) order is unchanged. `sqlite-migrate` only changes how the expand and contract *schema* steps are generated — fully automatically, rebuild included, with no hand-written or registered step of any kind — and how the transform step is bound: to a migration's checksum rather than a sequential version number, run by Hoserva's own apply loop rather than `sqlite-migrate`'s `Runner.Apply`. `internal/store/transforms/` keeps its existing job and its existing test discipline (every fixture database, doc 06 §2) unchanged; only what each transform is bound to changes.

**Decided by the maintainer (2026-09-17):** adopt `sqlite-migrate` for generation, checksum and drift, and as the library basis (not the whole of) a Hoserva-owned apply loop; retire `internal/store/tools/dbmigration`, `internal/store/tools/dbcheck`, `internal/store/{checksum,drift,safety,allowlist,columnrefs}.go` and `internal/store/migrations/contracts` outright; regenerate `internal/store/migrations/` from scratch against a STRICT-ified `schema.sql` under `sqlite-migrate`'s own naming and checksum format, since no release has shipped and nothing pins the old filenames. Issue #139 implements all of this.

**A `queries.sql` authoring note (sqlc v1.31.1):** two pitfalls confirmed while writing `internal/store/queries/jobs.sql` and `users.sql` (#19, #22) — a comment placed between a query's `-- name:` line and its SQL body, and any non-ASCII character appearing anywhere earlier in a query file than the file's last query, both silently corrupt or truncate the *generated* SQL for the query (or queries) after the affected one, with no build error to flag it. Neither is fixed upstream as of sqlc v1.31.1. The authoring rule: put a query's only comment, if it needs one, above its `-- name:` line, never between that line and the SQL it introduces; and keep query files under `internal/store/queries/` ASCII-only, or, if non-ASCII content (a comment, a string literal) is unavoidable, put it only in the file's last query.

### Q63 — API contract tooling
**Status:** Default — **ogen confirmed for jobs and errors; its SSE support is client-only, confirmed against ogen v1.24.0; the `Event` schema and a Go SSE-frame reader are still generated, via ogen's `jschemagen`, so the CLI's "generated client only" rule (D18, doc 01 §5) holds for `/api/v1/events` too (issue #17)** · **Gate:** Phase 1 (foundation) · **Affects:** doc 00 D18, doc 01 §5, doc 05 §7, doc 06 §7, §8, doc 12 §2, §3

**Default:**

| Job | Tool |
|---|---|
| Spec | OpenAPI 3.1, hand-written in `api/openapi.yaml` |
| Go server interfaces, request validation, the CLI's Go client for every operation except `/api/v1/events` | ogen (Apache-2.0) `cmd/ogen`, pinned `v1.24.0` via a `go.mod` `tool` directive |
| Go types and JSON decoding for `/api/v1/events`'s `Event` schema, plus the Go SSE-frame reader that decodes a stream of them | ogen's `cmd/jschemagen` (same module, same `v1.24.0` pin), against `api/event-schema-root.yaml`; the reader is boilerplate `make gen` writes deterministically, the same technique as `api/gen/ts/client.ts` |
| The `/api/v1/events` SSE endpoint's server implementation | Hand-written in `internal/api` (`events.go`, issue #19), against the schemas `openapi.yaml` declares for it, alongside the job system's own progress/state hub |
| TypeScript client for the web UI | openapi-typescript `7.13.0` (types) with openapi-fetch `0.17.0` (runtime wrapper), pinned in `api/package.json` |
| Spec lint, including "every operation has an `operationId` and an `x-hoserva-role`" | Spectral `6.16.3` (Apache-2.0) with a Hoserva ruleset (`api/.spectral.yaml`) |
| Breaking-change check against the last release | oasdiff (Apache-2.0) `v1.32.1`, pinned via a `go.mod` `tool` directive; skipped cleanly until a `v*` release tag exists |
| API reference on the docs site | docusaurus-openapi-docs (MIT) |

**Confirmed, not assumed (issue #17):** generating from the real spec (jobs, cancel, resume, the job log download and the shared `Error` schema) with ogen v1.24.0 produces a `Handler` interface and Go client that compile clean, with a handler missing a method — or one whose signature no longer matches the spec — failing `go build`, not a test (demonstrated in `internal/api`, doc 01 §5). ogen's own docs say only SSE *client* generation is supported; generating a spec with a `text/event-stream` response confirmed this the hard way — generation for the *entire* spec fails outright ("sse server response encoding not implemented") unless that one operation is named in `ogen.yml`'s `generator.ignore_not_implemented`, and doing so drops the operation, and every schema only it reaches, from **both** the generated server and the generated client, not just the server half (confirmed: `api/gen/go/oas_schemas_gen.go` has no `Event` type when `streamEvents` is the only reference to it, even though `generateResponses` runs, and registers the schema, before the operation is discarded — a scratch spec with a schema unused by any surviving operation confirmed the same pruning independently of SSE). There is no ogen option to generate a client for one operation independently of its server, and no option to force an otherwise-unreferenced component schema into the output.

That pruning is what the first attempt at this issue missed: it left `/api/v1/events` fully documented but with **no generated Go type at all**, on either side, which is a real, unacknowledged departure from doc 01 §5's "typed in the generated clients" (plural). The fix keeps ogen as the generator for every other operation (confirmed working) and adds a second, narrow generation step for this one schema: ogen ships a separate tool, `cmd/jschemagen`, that generates a Go type plus JSON `Encode`/`Decode` from a single JSON Schema document, independent of any operation. Pointed at `api/event-schema-root.yaml` (a one-line `$ref` into `openapi.yaml#/components/schemas/Event`, needed only because `jschemagen` takes a schema document, not an OpenAPI document plus a pointer into it), it produces the full `Event` sum type and every variant it reaches (`JobProgressEvent`, `DiskStateEvent`, `ContainerStateEvent`, `NotificationEvent`, and the `Job`/`DiskState`/etc. schemas they embed) into its own package, `api/gen/go/events`, deterministically and idempotently (confirmed across two runs). `make gen` then writes a small, fixed SSE-frame reader alongside it (`api/gen/go/events/reader.go`) — the same boilerplate-around-generated-types technique `api/gen/ts/client.ts` already uses for openapi-fetch — so a Go client decodes `/api/v1/events` only by calling generated code (`events.NewReader(body).Next()`), never a hand-rolled parser. `internal/api/events_test.go` confirms the reader decodes one framed event of each of the four types. One spec change was needed to make this work at all: each event variant's discriminator property (`event: enum: [job_progress]`, etc.) is now `const: job_progress` rather than a single-value `enum` — ogen names a dedicated enum type after schema-plus-property (`DiskStateEvent`'s `event` becomes `DiskStateEventEvent`), which is exactly the name ogen's own sum-type codegen picks for `Event`'s discriminator tag on `DiskStateEvent`, so generating `Event` in isolation surfaced a real duplicate-declaration collision between the two; `const` inlines the field as a plain validated string with nothing left to collide, and it changes nothing about the wire format or the TS client's types (`schema.d.ts` still types each variant's `event` field as the literal string, confirmed after regenerating).

The `/api/v1/events` server write loop itself — framing an `Event` over `text/event-stream` — is still not generated by any tool checked here; no tool here generates that half from a schema, since it is business logic (when to write, when to flush, how to end the connection) regardless of generator. **Issue #19 implements it**: `internal/api.EventsHandler` subscribes to the job system's own `job.Hub` (a plain Go pub-sub of `*job.Job`, published on every state and progress change) and frames each one as a `job_progress` `events.Event` — the same generated types this section's reader decodes — with a keep-alive comment while idle and clean disconnect handling on the request context. It is mounted beside, not inside, the generated router (ogen's own `Handler`/`SecurityHandler` still don't cover this route at all — Q63's pruning is unchanged), and is gated by an `Authenticate func(*http.Request) error` field rather than a real check: #22 owns session/token validation, so until it lands, a nil or refusing `Authenticate` refuses every request, matching `SecurityHandler`'s own default for every other operation.

oapi-codegen (`github.com/oapi-codegen/oapi-codegen/v2` `v2.5.0`) was run, not just reasoned about, against both the real spec and an isolated events-only spec (a `mktemp -d` scratch module, deleted after). Against the real spec it fails outright before reaching the SSE question at all: `error generating Go types for component schemas: error converting Schema Job to Go type: ... error resolving primitive type: unhandled Schema type: &[null]` — it does not support OpenAPI 3.1's `type: [string, "null"]` nullable form (a documented, open limitation, oapi-codegen#373; the tool itself warns "you are using an OpenAPI 3.1.x specification, which is not yet supported"), which this spec uses throughout for every nullable field, not only in `/api/v1/events`. Against the isolated events-only spec (downgraded to 3.0.3 to get past that limitation and isolate the SSE behaviour specifically), generation succeeds but produces something worse than ogen's explicit refusal: the server side (`std-http-server`) reduces the operation to a bare `StreamEvents(w http.ResponseWriter, r *http.Request)` — no typed response encoding of any kind, `text/event-stream` or otherwise. The client side is worse than a gap — it is actively wrong: `ParseStreamEventsResponse` calls `io.ReadAll(rsp.Body)` and returns a single `[]byte`, which would block forever against a real `/api/v1/events` connection (an intentionally unbounded stream) rather than return per-frame typed events, with nothing in the generated code or its comments to warn a caller. oapi-codegen was not adopted: it does not reach ogen's already-working generation for jobs and errors, does not support this spec's OpenAPI 3.1 constructs, and its own SSE output is worse than absent. Authoring the spec in TypeSpec instead of YAML is not adopted unless the hand-written YAML proves painful in practice.

### Q7 — How mergerfs and SnapRAID are sourced
**Status:** Default (S8 confirmed, agent-run in the lab, doc 08 §8) · **Gate:** Phase 1 · **Affects:** doc 00 D1, doc 01 §1, doc 03 §8.6, doc 06 §3, §7

**Default: depend on Debian 13's own packages — `mergerfs (>= 2.40.2)` and `snapraid (>= 12.4)` in `Depends`, no upper bound. The lab and CI install the same Debian packages. `hoserva doctor` and the updates page (doc 03 §8.6) warn, not block, when the installed version is below this tested floor — an upper bound in `Depends` would block Debian's own security updates. Hoserva ships an upstream build only for a concrete, named need it lacks; none has been found.** Installed versions are always read from package metadata, never from `--version` (Debian's mergerfs reports `vunknown`).

Checked hands-on (doc 08 §8, S8, confirmed — no longer partial): every doc 02 §1 mergerfs option, and all four named create policies (`mspmfs`, `mfs`, `lfs`, `ff`), are accepted by 2.40.2 and reported back correctly through the mount's own runtime control file (S6 already confirmed `mspmfs`'s fallback behaviour vs. `epmfs`'s ENOSPC — this run adds the other three policies and the "accepted and reported" layer). SnapRAID 12.4's `-Z`/`--force-zero` and `-E`/`--force-empty` guards refuse and then, with the flag, proceed exactly as `snapraid.txt` describes (S5 already confirmed `sync`/`diff`/`scrub`/`fix`/`touch`/`2-parity`). A changelog review from each pinned version to its current upstream release (mergerfs 2.40.2→2.42.0, SnapRAID 12.4→v14.9) found two real mergerfs bugs worth naming — a crash when `moveonenospc`'s relocation policy returns no destination branch, and a multi-branch `rmdir` that could report success while a branch's files remain — but neither is a fix to Hoserva's documented default configuration's everyday behaviour, and both are bounded by other layers of the design (the threshold guard's own array-fill visibility; doc 09 §2's own mover algorithm, which never calls `rmdir` on a share directory or trusts a directory-emptiness signal — its delete step enumerates tracked files and unlinks them by name — and whose always-run size verification, doc 09 §2 line 84, would catch a short or miscounted copy before any delete regardless of whether checksum verification, which doc 09 makes optional, is also enabled). SnapRAID's own changelog turned up nothing present in 12.4 that affects a behaviour doc 02/09 relies on. Using the distribution's packages removes a packaging and security-update burden from a solo maintainer (R7), and the maintenance cost of a private build isn't justified by either finding. Golden files and create-policy behaviour (Q11) still depend on exact versions, which is why the version range is explicit and the lab uses the same packages.

### Q8 — Frontend framework
**Status:** Default · **Gate:** Phase 1 · **Affects:** doc 01 §1, doc 06 §8, doc 12 §2

**Default: a React SPA built with Vite, using a client-side router. Next.js static export is not used.**
Next's `output: 'export'` needs every dynamic segment known at build time (`generateStaticParams`). The spec's core routes can't meet that: `/storage/disks/[id]`, `/shares/[name]` and `/apps/[name]` only exist at runtime. Static export also discards everything Next adds (SSR, server components, route handlers). A Vite SPA is the plain form of what doc 01 actually describes: static assets plus a REST API, embedded with `go:embed`.

### Q59 — What coss ui doesn't cover
**Status:** Default · **Gate:** Phase 1 · **Affects:** doc 00 D15, doc 03 (Component system), doc 12 §2

**The gap:** coss has no chart, code editor, diff view, terminal, remote-console viewer, virtualised list or stepper, and doc 03 needs every one of them.

**Default: one library per gap, each wrapped once under `web/src/components/` and styled from coss tokens, so no page imports a library directly:**

| Need | Library |
|---|---|
| Charts: throughput, temperature, SMART history, wake timeline, stats, stacked capacity | Recharts |
| Code views, the Compose editor, raw option fields (YAML, XML, INI) | CodeMirror 6 |
| Config drift diff | CodeMirror merge view |
| Browser terminal | xterm.js |
| VM console | noVNC (doc 14 §4) |
| Long log lists | TanStack Virtual inside the coss ScrollArea |
| Wizard steps | None — composed from coss as doc 03's `wizard` pattern |
| Fonts | Inter and Geist Mono (coss's defaults), bundled from Fontsource |

Recharts is declarative React and covers both time series and stacked bars; if live throughput graphs perform poorly on low-end hardware, the library changes inside the `chart` wrapper without touching a page. CodeMirror rather than Monaco because Monaco is large and expects web workers. Fonts are bundled because Hoserva makes no outbound requests of its own (Q49) and often runs on a LAN without internet access.

### Q9 — Web UI port and TLS
**Status:** Default · **Gate:** Phase 1 · **Affects:** doc 01 §5, §7, doc 03 §8.2

**Default: one TCP port, `:8008`, TLS-only from first boot (self-signed). A plain-HTTP request on that port gets a short "use https://" response. Ports 80/443 are never claimed.**
Doc 01 had HTTP on `:8008` and also "HTTPS by default, HTTP redirects" with no HTTPS port. The curated catalog seeds Nginx Proxy Manager (doc 04 §7), which needs 80/443. Hoserva taking those ports would break the most common homelab reverse-proxy setup.

### Q10 — What "bind to LAN by default" means
**Status:** Default · **Gate:** Phase 1 · **Affects:** doc 01 §7, doc 03 §8.2

**Default: listen on all interfaces, but accept connections only from loopback, RFC 1918, link-local, IPv6 ULA and CGNAT `100.64.0.0/10` (Tailscale) source addresses. One warned toggle allows all sources.**
Binding to a specific address breaks the first time DHCP hands out a new lease. Filtering on source address expresses the actual intent ("not reachable from the internet") and survives address changes. Including CGNAT keeps Tailscale, the safe remote-access path, working out of the box.

### Q66 — Where releases and the catalog are published
**Status:** Default · **Gate:** Phase 1 · **Affects:** doc 04 §7, doc 05 §7, doc 07 §1, doc 12 §6, Q3, Q39, Q50, Q65, Q67

**Default: until 1.0, every tag attaches the amd64 and arm64 `.deb` to a GitHub Release — stable versions as releases, beta versions as pre-releases — with a `SHA256SUMS` file carrying a detached Ed25519 signature. One static project site at `hoserva.dev`, deployed from GitHub Pages by a single workflow, holds the docs at the root and a signed release index under `/releases/` listing each channel's versions, asset URLs and checksums. At 1.0 the signed apt repository joins the site under `/apt/`: built with aptly, signed with a key held only as a CI secret, trusted through a `hoserva-archive-keyring` package and a `signed-by` source entry, each channel keeping the last five releases per architecture. The catalog is not on this site: it is built and published from its own repository at `catalog.hoserva.dev` (Q39, Q65).**
Before 1.0 only opt-in beta users run Hoserva, and GitHub Releases keeps every version at no operating cost; an apt repository is worth its key management once stable users expect `apt upgrade` to work. A release installs with `apt install ./hoserva_<version>_<arch>.deb`, which still resolves mergerfs and SnapRAID from Debian. A repository gets one GitHub Pages site, so this repository's site shares it by path between the docs, the release index and later the apt repository (Q3). The catalog is built and reviewed in its own repository (Q39) and so gets its own Pages site on its own subdomain, `catalog.hoserva.dev`, rather than a path here; `apps.hoserva.dev` stays free for a future browsable app listing. The index and catalog URLs compiled into `hoservad` must never change, which is why they live on a domain the project owns, registered before the first public release (Q50).

### Q67 — How Hoserva updates and rolls back itself
**Status:** Default · **Gate:** Phase 1 · **Affects:** doc 01 §3, §7, doc 03 §8.6, doc 12 §6, Q49, Q66

**Default: the update check reads only the signed release index on the project site (Q66) for the configured channel — never the GitHub API and never a system-wide `apt update`. An update downloads the release's `.deb` from GitHub Releases, verifies it against the signed checksums, and installs it in a transient systemd unit after a config backup; it is refused while a Parity, Array-write or Topology job runs. `hoserva rollback` downloads and verifies the previous release's `.deb`, installs it and restores that version's pre-migration database snapshot (doc 01 §4). The update that installs 1.0 adds the apt source and keyring, and later updates come from the apt repository with the same checks.**
`apt update` refreshes every source on the host — an outbound request per source the user never asked Hoserva to make — and changes what the next unrelated upgrade does. One static index keeps the GitHub API's unauthenticated rate limit out of the picture and describes both channels in one file. There are no down migrations (D16), so rollback is only safe as "previous package plus its snapshot", which GitHub Releases keeping every version makes possible.

### Q68 — Debian updates and reboots
**Status:** Default · **Gate:** Phase 1 · **Affects:** doc 01 §6, doc 03 §8.6, doc 02 §4

**Default: the `.deb` recommends `unattended-upgrades`, configured for Debian security updates only. Hoserva never reboots on its own: `/settings/updates` shows pending Debian updates and whether a reboot is required, and a reboot the user starts waits for a running Parity, Array-write or Topology job — except a data-disk upgrade (doc 02 §4), which the clean shutdown sequence (Q70) stops at its next checkpoint instead of the reboot waiting for it — then runs that sequence.**
A home server that falls behind on security updates is a real risk for the target user, and unattended security updates are Debian's own mechanism for it. An unplanned reboot mid-sync or mid-evacuation is worse than a delayed kernel update, so the reboot stays the user's action. A data-disk upgrade is the one exception: it is resumable and the array is already stopped for it, so waiting hours for the whole copy buys nothing a checkpoint stop doesn't already give for free.

### Q74 — Metrics, job logs and history retention
**Status:** Default · **Gate:** Phase 1 · **Affects:** doc 01 §4, §6, doc 03 §2, §3.4, doc 10 §1

**Default: time series — SMART attributes, temperatures, throughput, CPU and RAM — live in a separate `metrics.db`, excluded from config backups and downsampled: raw samples for 48 hours, hourly for 90 days, daily for two years. Spin-state events and the audit log stay in the main database for two years. Job stdout/stderr goes to compressed files under `/var/lib/hoserva/jobs/`, kept 90 days and capped at 1 GB, oldest first; each job's summary row stays in the database.**
Unbounded history in the main database would break doc 10 §1's single-digit-MB config backup and wear the boot SSD. Losing `metrics.db` loses graphs, never configuration, so it doesn't belong in the backup.

### Q75 — Host network configuration
**Status:** Default · **Gate:** Phase 2 · **Affects:** doc 03 §8.2, doc 14 §4, Q54

**Default: Hoserva changes host networking only when the host uses ifupdown, writing one managed file under `/etc/network/interfaces.d/`; on NetworkManager or systemd-networkd hosts the network page is read-only in v1 and says why. Every network change — address, DNS, gateway, the `vmbr0` bridge — applies with a 60-second confirm-or-revert: unless the browser confirms over the new configuration, the previous one is restored. The ISO (Phase 4) installs ifupdown.**
A wrong address on a headless box means carrying a monitor to it; confirm-or-revert makes the mistake recoverable. One backend done well beats three done badly. **A Debian 13 generic cloud image (this repo's L3 base, `debian-13-generic-amd64`) uses netplan → systemd-networkd, not ifupdown** — confirmed in L3 (issue #114): `NetworkManager=inactive systemd-networkd=active`, `ifup` and `/etc/network/interfaces` both missing, netplan `50-cloud-init.yaml` driving `ens20`. v1 still only edits ifupdown; the ISO installs ifupdown so a Hoserva appliance is editable. A debian-installer "minimal server" image was not the L3 guest.

### Q76 — Installing onto a Debian system that is already in use
**Status:** Default · **Gate:** Phase 1 · **Affects:** doc 01 §2, doc 03 §1, doc 04 §3, Q62

**Default: installing never overwrites existing configuration. Onboarding's system check lists what it finds — Samba shares, NFS exports, fstab mounts, Docker containers, images, named volumes, user-defined networks and installed plugins — and offers each managed file for import into the database or to be left unmanaged under the drift model (doc 01 §2). Docker's data-root moves to the cache (Q62) only when the user accepts it and Docker holds no containers, images, named volumes, user-defined networks or plugins; with any existing Docker state, or with no cache disk, it stays at `/var/lib/docker`. A named volume, network or plugin has no import/leave decision of its own — Hoserva does not migrate it (that stays out of scope) — it only blocks the move, since moving the data-root out from under Docker without moving that state strands it invisibly under the old root (#413, #416). Swarm state is not counted and is not reported: Hoserva does not support swarm (D6).**
The `.deb` installs onto a user's own Debian (D9), so a host with Samba shares or running containers is the normal case, not an edge. Silently replacing `smb.conf` or moving Docker's data-root would make shares, containers, networks and plugins vanish — the loss of trust doc 01 §2 exists to prevent.

### Q77 — UPS support
**Status:** Default · **Gate:** Phase 2 · **Affects:** doc 01 §1, doc 02 §6, doc 03 §8.1, §8.3

**Default: NUT (`Recommends: nut`), with its configuration generated from the database, for a USB-attached UPS or a network NUT server. On battery: notify, pause the mover and hold scheduled syncs. At low battery, or after a configurable runtime on battery: bring running Array-write jobs to their next checkpoint, mark a running sync interrupted, and run the shutdown sequence (Q70).**
A power cut mid-sync is recoverable (doc 02 §6), but a clean shutdown is better, and a home server without UPS integration sends users back to hand-edited NUT configuration — exactly what Hoserva exists to remove. NUT is Debian's packaged standard.

---

## Storage

### Q11 — Create policy defaults
**Status:** Default — **`mspmfs`'s parent-path fallback confirmed, agent-run in the lab** (S6, doc 08) · **Gate:** Phase 1 · **Affects:** doc 02 §1, doc 03 §3.1, §4.2, doc 05 §4, doc 09 §1

**Default: the policy is per share (Q12 makes that possible). Plain-language options:**

| UI label | mergerfs policy | Default for |
|---|---|---|
| **Keep folders together** | `mspmfs` (fallback: `epmfs`) | New shares |
| **Balance across disks** | `mfs` | — |
| **Quiet disks** | `lfs` | The "Quiet mode" preset (doc 08) |
| **Fill disks in order** | `ff` | Unraid shares imported with *Fill-up* allocation |

`mspmfs` ("most shared path") keeps `epmfs`'s folder locality. When no branch holding the full path has room, it retries with the parent directory, and so on up the tree. That directly addresses the ENOSPC-instead-of-fallback edge in doc 08 and doc 09 §1. **S6 confirmed this on the loop harness with Debian 13's mergerfs 2.40.2 (Q7), 2026-09-16**: on a dedicated three-disk array with one branch filled below `minfreespace`, a write through an `mspmfs` pool succeeded by falling back to the parent path (landing on an unfilled branch), while the identical write through an `epmfs` pool — same branches, same fill state, only the create policy different — returned `No space left on device` (doc 08 §6). The policy name was already known to be accepted (doc 08, S9); this run confirms its fallback behaviour too, so the default stands and does **not** revert to `epmfs`. The imported Unraid allocation methods map as *Fill-up* → `ff` and *Most-free* → `mfs`. *High-water* has no exact equivalent: it maps to `mfs`, and the import review screen says so.

### Q12 — Per-share cache modes vs. a single mergerfs mount *(architectural gap)*
**Status:** Default — **the per-share topology confirmed, agent-run in the lab** (S6, doc 08); the two-mount fallback below is not adopted · **Gate:** Phase 1 · **Affects:** doc 01 §6, doc 02 §1, §3, doc 09 §2, doc 06 §3

**The gap:** doc 02 §3 promises a per-share cache mode (cache-then-move / cache-only / array-only), and doc 05 imports per-share allocation settings. But a mergerfs mount has exactly one create policy and one branch list for the whole mount. One `/mnt/user` mount unioning cache and array can't send share A's writes to cache and share B's writes to the array.

**Default topology:**

```
/mnt/user                   mergerfs: /mnt/disk*=RW  (catch-all; default policy)
/mnt/user/<share>           mergerfs, one mount per share, branches by cache mode:
                              cache-then-move → /mnt/cache/<share>=RW : /mnt/disk*/<share>=NC
                              cache-only      → /mnt/cache/<share>=RW
                              array-only      → /mnt/disk*/<share>=RW
                            create policy = the share's own (Q11)
/run/hoserva/array/<share>  mergerfs: /mnt/disk*/<share>=RW, same policy — the mover's write target
```

- Paths stay identical to Unraid (D10): `/mnt/user/<share>` is still where every share lives.
- The catch-all mount means `ls /mnt/user` works, and a stray top-level directory written by a container lands on the array, not on the boot device.
- `NC` (no-create) array branches in cache-then-move shares are still readable, so files already moved remain visible.
- **The mover writes through `/run/hoserva/array/<share>`**, so mergerfs itself places the file. That satisfies doc 09 §2's "never a second placement algorithm" by construction, not by reimplementation.
- mount ordering (catch-all before per-share children) is expressed with systemd `RequiresMountsFor=`.

**S6 validated the costs on the loop harness, 2026-09-16 (doc 08 §6): confirmed.** A catch-all plus a dozen per-share mounts (four of each cache mode) mounted and unmounted cleanly in dependency order; mounting a share before its catch-all made it silently unreachable rather than corrupted, and unmounting the catch-all re-exposed it byte-identical, the concrete mechanism `RequiresMountsFor=` exists to prevent; the catch-all itself refused to unmount while shares were nested inside it; `NC` branches stayed readable while new files landed on cache; a stray top-level write landed on a data disk, never the boot device; twelve share mounts used ≈87 MiB RSS combined (well under this entry's own 200 MiB "too high" bar); killing one share's own mergerfs process failed safely (`ENOTCONN`) rather than exposing the array beneath it, and a remount recovered cleanly. Boot-time `RequiresMountsFor=` enforcement itself still needs L3 (no init system in the loop-device lab), tracked as residual risk, not as an open question about the topology.

**Throughput's own cost figure needed a second pass.** An earlier measurement ran every twelve-shares repetition before any single-mount repetition and attributed a depressed first twelve-shares rep to mount count; an independent verifier reproduced this as a run-order artifact instead (reversing which scenario ran first moved the depression to the other scenario). The corrected measurement counterbalances scenario order (ABBA: pair 1 single-mount-then-twelve-shares, pair 2 the reverse, and so on) and discards four warm-up reads per state before counting — a warm-up count reached by iterating in the lab (single generic warm-up, then one per state, then four) until a fresh run's own warm-up log stopped showing a residual climb — because the climb spans several repetitions after a mount/unmount transition, not just the first read. Five reps per scenario: relative loss by median (twelve-shares vs. single-mount) **−0.01%** in the one committed run (twelve-shares marginally faster, within noise), comfortably under this entry's own 10% "too high" bar. An order-effect check (a Pearson correlation between read position and throughput, pooled across scenarios: this run's own **r = −0.090**, close to zero) is reported alongside it regardless of what it shows — a measurement limitation of this lab named as an open caveat (doc 08 §6, "Residual risk"), not resolved by a single run's reading, near-zero or not, or by a bigger warm-up.

**The two-mount fallback is not adopted.** Per-share cache modes stay as designed: set per share, at write time, exactly as doc 02 §3 promises. Doc 07 R12 is retired for the topology itself; its own row still names the throughput measurement's residual caveat rather than claiming a clean "no cost" result.

### Q13 — Counting "files changed since last sync" without waking every disk *(contradiction)*
**Status:** Default — **confirmed, agent-run in the lab, 2026-09-16** (S7, doc 08): a `FAN_MARK_FILESYSTEM` mark per data disk, counted by distinct `(directory-FID, name)` pair, matched `snapraid diff`'s own changed-file total exactly (16/16) across create, overwrite, append, delete, rename (same- and cross-directory), copy and touch, both direct and through the mergerfs pool; queue overflow was produced and correctly detected against a same-size control; a listener restart and a filesystem remount were both measured to lose events silently, confirming the "approximate, unknown until the next sync" UI behaviour below is load-bearing, not decorative; ext4 and single-device btrfs data disks behave identically to XFS. · **Gate:** Phase 1 · **Affects:** doc 02 §1, §2, §4, doc 03 top bar, §3.5, doc 08

**The contradiction:** the permanent amber indicator ("412 files unprotected", live) implies polling `snapraid diff`. That command stats every file on every data disk, spinning all of them up. Meanwhile the spindown acceptance test requires 30 idle minutes.

**Default: a change journal.** `hoservad` places a `fanotify` mark (`FAN_MARK_FILESYSTEM`, FID reporting) on each data-disk filesystem and records create/modify/delete/rename events per disk since the last sync. Events are only generated by writes that already woke the disk, so the journal never wakes anything. The UI shows the count as approximate ("≈412 files changed"). An exact `snapraid diff` runs only immediately before a sync (the disks are spinning anyway) and on explicit request, and the UI says it will wake the disks. The same journal supplies "files pending at failure, by name" for a dead disk (doc 02 §4), and it reuses the fanotify machinery that wake attribution (doc 08) needs anyway.

**One addition from S7, not a change of default:** the lab this spike ran in has `CAP_SYS_ADMIN` but not `CAP_DAC_READ_SEARCH`, so a fanotify file handle cannot be resolved to a real path there (`open_by_handle_at` fails `EPERM`, same as S1/S6) — the spike counted distinct `(directory-FID, name)` pairs instead, and confirmed that proxy tracks `snapraid diff`'s own count exactly in this lab. The production daemon runs as root with the resolving capability and should be able to record real paths (needed for "files pending at failure, by name" above); that path is unverified by this spike and needs an L3 or public-beta confirmation, not assumed from the lab result. Two further additions, both consequences of the mechanism rather than surprises: a `hoservad` restart or a data disk's remount **silently** stops that disk's event delivery — the daemon must re-arm each disk's mark after either, and until it does, the UI's "approximate" count for that disk should read as stale, not merely rounded. A btrfs data disk laid out as a subvolume distinct from its filesystem's root needs its own mark per subvolume (`EXDEV` on a single root-level mark, confirmed); a plain whole-device btrfs disk (the only layout Q23 currently plans to support) needs no special handling.

### Q14 — Rebalance and evacuation must respect parity ordering *(data-safety gap)*
**Status:** Default · **Gate:** Phase 1 · **Affects:** doc 02 §4, doc 09 §3, §4, §6

**The gap:** doc 09 uses the mover's copy-verify-delete for rebalance and evacuation. Between two data disks that ordering is unsafe under SnapRAID. Parity reconstructs disk B's blocks using disk A's blocks *as of the last sync*. Deleting the source file on A before a sync means a failure of B in that window can't be fully recovered.

**Default: array-to-array relocations are two-phase.** Copy and verify everything, run `sync` (through the guard, Q15), delete the sources, then `sync` again. Cache-to-array moves (the mover) keep single-phase copy-verify-delete, because cache is outside parity and nothing depends on its old blocks.

### Q15 — The threshold guard vs. Hoserva's own relocations *(gap)*
**Status:** Default · **Gate:** Phase 1 · **Affects:** doc 02 §2, doc 09 §3, §4

**The gap:** an evacuation of a full 8 TB disk produces a diff with tens of thousands of removals, plus a data disk whose file count drops to zero. That is exactly the pattern the guard exists to block. As specified, every evacuation and rebalance would trip it.

**Default:** every relocation job writes a manifest (relative path, size, mtime, source disk, target disk). The guard counts a removal as *accounted* when it matches a manifest entry whose matching file either appears as added or copied on the manifest's target disk in the same diff, or was already confirmed there (below). Accounted removals appear in the diff view as their own group and don't count toward thresholds. Unaccounted removals count exactly as before. A disk in removal (doc 09 §4) is exempt from the zero-files rule, and only that disk gets `--force-empty`. **Every sync, whatever triggered it (schedule, disk add, evacuation, manual), goes through the guard.**

Same-diff co-occurrence alone cannot cover Q14's mandated two-phase array-to-array order (copy+verify, sync, delete, sync, #248): the addition is committed by the *first* sync's own diff, and the removal only shows up in the *trailing* sync's diff, once the source is deleted — the two are structurally never the same diff. Before `SnapraidEngine.Sync` evaluates the guard, it checks whether the manifest has any entry whose source-side removal this diff shows but whose target-side addition it does not; only then does it pay for a real `snapraid list` (tracked state only, never a live walk) and mark that entry's own in-memory copy `TargetConfirmed` when the file is genuinely already tracked on its target disk. `matchManifest` treats `TargetConfirmed` exactly like a same-diff addition. This costs nothing for an ordinary, same-diff relocation (the ordinary mover): the `snapraid list` call only ever runs for the shape a two-phase trailing sync actually produces. The confirmation is never taken on the manifest's own say-so — it is grounded in the same real, observed SnapRAID state the same-diff check already was — and it is never persisted back to whatever store supplied the manifest; it is recomputed fresh, in memory, for that one `Sync` call.

Neither half of the data-disk rule above can ever be satisfied for a manifest entry whose target is not a SnapRAID-tracked disk at all — an array→cache share relocation's (doc 09 §2) manifest entry names the cache mount as its target, which can never appear in a SnapRAID diff's added files and is never something `snapraid list` tracks, regardless of sync ordering (#240). `matchManifest` recognizes this from `diff.PerDisk`, the diff's own "data:" config echo — the real, structurally grounded set of disks SnapRAID currently tracks, which the manifest has no say over. When `diff.PerDisk` is populated (always true for a real diff; only a hand-built test fixture ever leaves it empty) and a manifest entry's target disk is not one of its keys, the removal is accounted as soon as its source-side removal appears in the diff, without needing a same-diff reappearance or `TargetConfirmed`. A manifest entry whose target *is* a tracked data disk, or a diff that never says which disks it tracks, still gets the strict same-diff-or-confirmed rule above unchanged — an entry cannot borrow the looser cache rule by pointing at an unrelated data disk, and an empty `diff.PerDisk` is never read as "the target is non-data."

### Q16 — Guard threshold values
**Status:** Default · **Gate:** Phase 1 end · **Affects:** doc 02 §2

**Default: keep 500 removed files / 10% removed+updated.** Confirmed by the L3 soak (issue #44, `scripts/vm/soak-history.jsonl`, `scripts/vm/soak-report.md`): 25 routine nights of seeded add/edit/rename/delete churn peaked at 16 snapraid-removed files (a night that also carried the previous yanked-disk night's unsynced churn) and well under 10% removed+updated; two mass-delete nights (600 removals, 35.6% and 48.2% removed+updated) tripped both count and percent triggers. Raising either number would let a ransomware-scale deletion through; lowering them would block ordinary churn the soak never produced. Re-run as lab `210-a1` on current `beta` (catch-all mkdir in `Mounter.Mount`, config backup wired into the chain, fail-closed recovery) reproduced the same figures.

### Q17 — `snapraid touch` before syncs
**Status:** Default · **Gate:** Phase 1 · **Affects:** doc 02 §2

**Default: run `snapraid touch` automatically before a sync only when `snapraid status` reports files with a zero sub-second timestamp. Log the count.**
SnapRAID prints exactly that warning when touch is needed. Running touch conditionally fixes the move-detection problem the community scripts address, without cargo-culting it into every run.

### Q18 — Content file placement
**Status:** Default · **Gate:** Phase 1 · **Affects:** doc 02 §2, doc 05 §4

**Default: one on the boot device (`/var/lib/hoserva/snapraid.content`, listed first), one on cache if present, then data disks with the most free space, until the count reaches at least `parity disks + 2` on at least three distinct physical devices.**
Listing the boot-device copy first means `snapraid status` polling reads a disk that is always awake (Q13). Requiring distinct physical devices turns doc 02's "3 separate disks, one not a data disk" into a rule the config generator can check. A cache that is a partition of the boot disk (doc 02 §4) is not a distinct device: it gets no copy of its own and the data disks supply the rest.

### Q19 — Number of parity disks in v1
**Status:** Default · **Gate:** Phase 1 · **Affects:** doc 00 §4, doc 03 §3.1, doc 05 §2, §3, doc 07 §1

**Default: 1 or 2 parity disks supported in v1, for new pools and for migration. 3 or more are out of scope.**
Dual parity was excluded because its "migration path [is] unclear", but it isn't. Unraid parity is never reused in either case: both Unraid parity disks are fully rewritten as SnapRAID parity, exactly like single parity. The extra cost is a `2-parity` line in the config generator, one golden file and one loop-harness case. Refusing it would force the users with the largest arrays, who most need dual parity, to downgrade their protection in order to migrate.

### Q20 — Parity disk filesystem
**Status:** Default · **Gate:** Phase 1 · **Affects:** doc 02 §2, §5, doc 03 §3.1

**Default: XFS for parity disks, formatted fresh (a migrated parity disk's contents are discarded anyway).**
The parity file is a single file roughly as large as the largest data disk. ext4 with 4 KiB blocks caps files at 16 TiB, which today's 20 TB+ disks exceed. XFS has no practical file size limit and no reserved-block overhead. The data disks' `minfreespace` (default 50G) keeps parity headroom even when parity and data disks are the same nominal size.

### Q21 — Disk identity
**Status:** Default (mount-by-filesystem-UUID confirmed in S2, agent-run in the lab, doc 08 §2; L3 virtual-disk identity confirmed, agent-run in an L3 VM against a real, unmodified `disk.LinuxProvider.List()` path — `ResolveIdentity` recognizes the guest's `virtio-<serial>` by-id link and resolves every array disk to a non-weak `Identity` that `Matches` correctly, via a truncation-safe `<serial>` field order in `scripts/vm/create-vm.sh`, not via a `<wwn>` device attribute, which libvirt/qemu refuse on a virtio-blk-bus disk on this host, issue #162. What remains open: real hardware's own `ata-`/`scsi-`/`nvme-`/`usb-` WWN by-id links, and enclosures/HBAs that report identity inconsistently — neither tier below real hardware can produce either, so both stay a documented residual risk under D20, not an actionable next step (doc 06 §6)) · **Gate:** Phase 1 · **Affects:** doc 02 §4, doc 05 §4, doc 10 §1

**Default: a disk's identity is its `/dev/disk/by-id` WWN, falling back to serial. Mounts use filesystem UUID. When a USB enclosure hides the serial, the disk is marked "weak identity": allowed as a data disk (matched on FS UUID + size, with size recorded on the `array_disks` row at create/add/replace time — rows from before that column existed keep a NULL size and fall back to FS UUID alone), refused as parity, and warned about in the setup wizard and migration scan.**
Serial matching is validated by Unraid's own model (doc 08). Enclosures that mask serials are the known exception, and a wrong parity-disk match is the most expensive mistake that exception could cause.

**Identity-bound formatting.** Confirming identity is not enough on its own if the destructive call that follows still runs against a `/dev/sdX` path: that path can be reassigned between the confirmation and the call. Formatting and adopt-checking (doc 02 §4) always run against the confirmed disk's own `/dev/disk/by-id` path when one is known — the exact by-id link basename WWN/Serial were resolved from, retained end to end from discovery rather than reconstructed only from WWN — so the kernel resolves it to whichever physical disk currently carries that identity at the moment the call actually opens it, regardless of `/dev/sdX` renumbering in between. A disk with no by-id link at all (every disk in the loop-device lab, doc 06 §3) has nothing to bind to and keeps using its plain device path, exactly as before.

### Q22 — Encrypted (LUKS) arrays
**Status:** Default · **Gate:** Phase 3 · **Affects:** doc 00 §4, doc 05 §2, §7, doc 06 §5, doc 07 §1

**Default: v1 detects encrypted Unraid arrays and refuses to migrate them, with a clear message and a docs page on manual options. New encrypted pools are not offered. Post-1.0 design: a keyfile on the boot device, stated plainly as convenience rather than protection against physical theft.**
Doc 08's recovery evidence and R2 outweigh the convenience. Detect-and-refuse matches the ZFS stance, keeps the fixture (it tests the refusal), and costs no support burden. Implementation note from S2 (doc 08 §2, sourced from `unraid/webgui`'s `DiskSettings.page`): Unraid's own `defaultFsType` setting encodes encryption as a `luks:<fs>` value (e.g. `luks:xfs`) rather than a separate flag — detection must parse that prefix, not assume a distinct boolean field.

### Q23 — Non-XFS Unraid data disks
**Status:** Default · **Gate:** Phase 3 · **Affects:** doc 05 §2, §3, doc 06 §5, doc 08 §2

**Default: adopt any single-device XFS, btrfs or ext4 data disk, each after its own read-only check (`xfs_repair -n`, `btrfs check --readonly` on the unmounted device, `e2fsck -n`). A disk that fails its check is refused. ZFS-formatted array disks and multi-device btrfs members are refused.**
mergerfs and SnapRAID are filesystem-agnostic, so a single-device btrfs or ext4 disk costs one fixture each. ZFS needs OpenZFS as a dependency, which is out of scope (doc 00 §4). Doc 08's "refuse a filesystem that reports errors" extends to every filesystem, not just XFS.

**Reading the disks (doc 05 §3).** A disk that passes its check is mounted read-only to record the verify baseline: XFS with `ro,norecovery`, ext4 with `ro,noload`, btrfs with `ro,rescue=nologreplay`, never a plain `-o ro` mount, which rewrites the XFS superblock and log (S2) and replays a btrfs tree log. A dirty XFS log fails `xfs_repair -n` and is refused, as is a btrfs disk with a pending tree log (`log_root` not zero) and an ext4 disk with `needs_recovery`; replaying it with explicit consent stays a product decision nobody has asked for. The baseline's content hashes cover **every file of 1 MiB or less plus a deterministic 1 in 100 (at least 200) of the larger files per disk**, chosen by a stable hash of the path and stored with the baseline, and `--full-checksums` hashes everything. Sizes and counts are recorded for every file whatever is hashed.

**Unraid 7.3 internal boot devices are not data disks (doc 08 §2, "Unraid 7.3 internal boot").** The ZFS refusal above is for array and pool data disks. A device whose GPT partitions 1 to 4 carry Unraid's internal-boot names and types (`BIOS Boot Partition`, `EFI System Partition`, `Unraid Boot Partition`, and a Linux data partition) is classed as Unraid's boot device before any filesystem check, read from the partition table in the udev database without opening a partition. Its partition 3 is a `zfs_member` by design, so such a device is **never refused as a ZFS data disk and never adopted**, whether it is a dedicated device, one half of a mirrored pair, or a boot + data device; partition 4 of a boot + data device is Unraid's cache, re-created like every cache. A device that matches only part of the layout is not classed this way, and its ZFS partition is refused here like any other. After migration the device is offered like an unassigned disk, labelled as holding Unraid's boot pool: a dedicated or mirrored boot device needs the usual whole-device wipe confirmation, and for a boot + data device step 17 formats partition 4 only, leaving partitions 1 to 3 until the user asks to erase them. Hoserva never installs or writes its own boot device (doc 01 §6), so the recommended default's third clause, "offered as a target for Hoserva's boot device", does not apply; the rest is confirmed. The rule is Unraid's own detector (`unraid/api`, `disks.service.ts`), checked in the lab against synthetic images including two negative controls; no real internal-boot server was available (D20).

### Q24 — Supported Unraid versions for migration
**Status:** Default (confirmed in S2 — agent-run in the lab against a synthetic XFS fixture, plus a source diff of `unraid/webgui` across its `6.12.15` and `7.3.2` tags found no version difference in the data-disk partition-format default or the `/boot/config/` flash layout; doc 08 §2. The 7.3 internal-boot layout is documented from public source and a lab check on synthetic images, not yet fixture-verified — see below) · **Gate:** Phase 3 · **Affects:** doc 05 §2, §3, doc 06 §5, doc 07 R5

**Default: Unraid 6.12.x and 7.x, each backed by a fixture. The scan refuses any version or config layout it doesn't recognise. `--unverified-layout` overrides that refusal with a full-screen warning and records the override in the report.**
R5's "fail loudly on unknown layouts" needs a concrete allowlist to fail against. Residual: only the ≤2TB MBR/4K-aligned partition layout and the primary XFS path are fixture-verified so far. For GPT (>2TB) disks, a real 7.3.2 server showed one partition starting at sector 64 and ending 33 sectors before the end of the disk (doc 08 §2); that is calibration, and a GPT fixture with those values is still needed, as are ext4/btrfs disks and a VM/`libvirt.img` fixture.

**Unraid 7.3 internal boot is its own layout and needs its own fixture-verified entry.** Its flash tree lives on the ZFS dataset `flash/boot` of a GPT device (partitions: BIOS boot, FAT EFI system partition, ZFS boot partition, data partition), the Flash Backup zip has the same root as a USB stick's (`config/` and `bzimage` inside it) but derived differences (`grub/` in place of `syslinux/` and `EFI/`), and no stick is involved (doc 08 §2). The allowlist therefore recognises the zip by `config/` and `bzimage`, not by `syslinux/` or `EFI/`, and the device by its partition fingerprint (Q23). The layout counts as supported-and-verified only once `unraid-internal-boot` and `unraid-internal-boot-shared` are built and the scan passes against them; until then it is recognised from public source only, and doc 05 §2 says so.

### Q25 — Where migration reads Unraid config from *(contradiction)*
**Status:** Default (flash config tree confirmed via `unraid/webgui` source, doc 08 §2 — every path Q25 depends on lives under `/boot/config/`, which is `config/` inside the Flash Backup zip, whose root is `/boot`) · **Gate:** Phase 3 · **Affects:** doc 05 §3, §4, §6

**The contradiction:** Phase B step 11 removes the Unraid USB stick (for a server that boots from one). Step 15 then seeds shares and users from config "exported in step 3", but the doc never says where that export is stored or how Hoserva reads it. Doc 05 §3 also offers to run the scan "from a live environment", which doesn't exist until the Phase 4 ISO.

**Default: the migrator reads Unraid configuration from the Flash Backup zip that step 1 already produces, uploaded through the UI or given as a path. Alternatively it reads from the stick itself, mounted read-only.** It never writes to the stick. Phase A's prepare script adds its capture files under `config/hoserva/` of the same flash, so they are part of this single source (Q89). The scan runs on the freshly installed Hoserva, before any import. That is still well before the point of no return (step 17), so it keeps the rollback guarantee without needing a live environment. Unraid-side checks that need a running Unraid (the final parity check) become a printable pre-cutover checklist.

**The import needs nothing from the flash beyond what the scan stored (settled with the import's adoption, doc 05 §4 steps 14-16).** Its inputs are the disk-role mapping the user confirms, this machine's disks, and the scan's report in the session (its disk table and verdict). The later steps that seed shares and users read what the scan stored too (`Report.Import`, doc 05 §3). The adoption therefore reads no zip and mounts no stick, a session whose source is a stick that has since been removed imports without it, and nothing is copied from the stick, so no copy of its secrets lands on the new machine. A later step that does need a file from the flash reads it from the session's recorded source and, for a `device:<path>` session whose stick is no longer attached, refuses with a distinct error that suggests the Flash Backup zip, never from a copy of the stick.

**For Unraid 7.3 internal boot the zip is the only source (doc 08 §2).** The boot pool is ZFS, and reading it would need OpenZFS, which is out of scope (doc 00 §4). A USB stick that may still be attached for licensing is a copy Unraid no longer writes once it mounts the internal pool on `/boot`, so the scan never reads configuration from it. The zip's root is the same as a stick's (`config/` inside it), because Unraid's Flash Backup and the prepare script both zip `/boot`.

### Q83 — Unraid User Scripts *(gap)*
**Status:** Default · **Gate:** Phase 3 · **Affects:** doc 05 §3, §4

**The gap:** a migrating user can carry dozens of Unraid User Scripts plugin entries encoding real operational behaviour — backups, cleanups, notifications — and doc 05 had no stated position on them: not migrate, not report, not out of scope.

**Default: inventory and report, never execute or auto-translate.** The pre-flight scan lists every User Scripts entry it finds — name, schedule and enabled state — in the go/no-go report, and the user decides what to do with each one. Hoserva never runs a migrated script and never translates one into a job automatically.
This fits the scan's existing role: it already reports things it does not migrate (appdata location, UID/GID distribution, disk serial mapping). Executing or auto-translating arbitrary third-party shell would violate `CLAUDE.md`'s "never interpolate user or template input into a shell" and has unbounded scope. It mirrors the converter's "never silently drop" principle (doc 04 §5) — the user is told what existed rather than discovering the absence later.

**Confirmed (2026-10-02, against a real 7.3.2 config tree):** the plugin stores each script at `config/plugins/user.scripts/scripts/<name>/{script,name,description}` and the schedules in `config/plugins/user.scripts/customSchedule.cron`, both under the flash's `config/` and so inside the Flash Backup zip the scan already reads (Q25). The scan reads those two paths. The `user.scripts.enhanced` plugin keeps only `categories.json` beside its package, nothing the scan needs. State a plugin keeps anywhere else was not part of the confirmation, so the report lists each script with the schedule these files give, and an enabled state only where they show one: a script with a line in `customSchedule.cron` (five fields or an `@` shorthand; the folder name may hold spaces) is reported as scheduled, only the plugin's own `startCustom.php` line counts, and any other line is a warning that gives its line number and never its text (the file may hold a credential), and a script without a line is reported as having no schedule there, with the caveat that the plugin may run it another way (at array start or stop, or on demand). When the plugin is installed and no entry is found, the row says the entries could not be found and never that there are none.

### Q89 — What Phase A captures from the running Unraid server
**Status:** Default · **Gate:** Phase 3 · **Affects:** doc 05 §3, §4, doc 08 §2, Q25, Q37

**Default: Phase A starts with a read-only prepare script, run on the still-running Unraid server before the Flash Backup, from the user's own computer over SSH. It writes its capture to `/boot/config/hoserva/`, prints the Phase A report, and with `--zip` streams the Flash Backup zip to the user's machine. The scan reads the capture from the zip or the read-only stick, like the rest of Unraid's config.**
- **Written to the flash, not the array.** `/boot/config/hoserva/` lands in both of Q25's sources and keeps one config source. A path under `/mnt/user/…` can land on the cache when a share prefers it, the cache is re-created during migration, and Unraid 7 allows servers without an array. The script writes there while Unraid runs; Hoserva itself still never writes to the stick.
- **Captured files:**
  - `containers.json` — `docker inspect` over every container (`docker ps -aq`, exited ones included); `[]` when there are none
  - `networks.json` — `docker network inspect` over every network
  - `disks.ini` — a copy of `/var/local/emhttp/disks.ini`, the slot → disk identity table
  - `autostart` — a copy of `/var/lib/docker/unraid-autostart` when it exists
  - `var.ini` subset — the parity-check fields (`sbSynced`, `sbSyncExit`, `sbSyncErrs`, `mdResync`) and no other key
  - `smart/` — a copy of `/var/local/emhttp/smart/`
  - `capture.json` — capture time, Unraid version (`/etc/unraid-version`), script version, boot mode, boot device identities, whether the boot pool is mirrored or shares its device with a data pool, the location class (cache, array or boot pool) of the Docker directory and of `libvirt.img`, and the writable-layer size per container (`docker ps -as`)
- **Docker's own inspect format, unwrapped,** so fixtures can be produced in the lab without Unraid (D20).
- **The zip comes from the script,** streamed to stdout after the capture is written. Its root is `/boot`, like Unraid's own Flash Backup, and top-level `previous/` is left out. Unraid's own Flash Backup behaves differently between versions (it writes into RAM or onto a user share, or streams; newer releases add a "save to server" option that picks a cache pool first), so the script makes the zip itself. The GUI Flash Backup stays a documented alternative, with a warning against "save to server".
- **Container origin:** dockerMan (template matched on its `<Name>`), Compose Manager (its `compose.yaml`), or created by hand (flagged; nothing to convert).
- **The capture holds the containers' environment variables, secrets included,** the same as the templates on the same flash.

**Evidence** (the maintainer's own measurement on their server, and a read-only calibration session they opened on the same 7.3.2 server on 2026-10-02; general facts and counts only, no values):
- 367 templates in `config/plugins/dockerMan/templates-user/`, but only 35 containers (`docker ps -a`), 32 running and 3 exited: about 90% of the templates had no container behind them. `templates-user/` records every template ever installed, not the containers that exist.
- `config/plugins/dockerMan/userprefs.cfg` is not usable: of its 40 container names, 11 had no container and 6 real containers were missing; it also holds `folder-*` entries from a folder-grouping plugin.
- The join key is the template's `<Name>` element, not its file name: a template file and its `<Name>` can differ, and `userprefs.cfg` can hold a third spelling. Docker names are case-sensitive. Matched on `<Name>`, all 35 containers had a template.
- Every dockerMan container carries the label `net.unraid.docker.managed=dockerman` (35 of 35). A Compose Manager container carries `com.docker.compose.project` instead (its files are `config/plugins/compose.manager/projects/<name>/compose.yaml`), and a container made with `docker run` carries neither.
- Running is not autostart: 31 of 35 containers have restart policy `no`; Unraid starts containers itself from `/var/lib/docker/unraid-autostart` (one name per line, an optional second field the wait in seconds; 28 entries). That file sits in the Docker directory, usually on the cache, and is lost with it.
- The array's slot → disk identity is not in a text file on the flash: `config/disk.cfg` carries `diskIdSlot.N="-"` for every slot and the assignment is in `config/super.dat` (4 KiB, Unraid's own binary format). `/var/local/emhttp/disks.ini` has it as text, one section per slot (`id`, `device`, `type`, `status`, `fsType`, `rotational` and more). Pools differ: `config/pools/<name>.cfg` has a text `diskId`.
- The boot mode is visible only at runtime: `findmnt -no FSTYPE /boot` is `vfat` for a USB stick and `zfs` for Unraid 7.3's internal boot pool, while `diskBootSize="0"` also appears on an ordinary pool of a USB-boot server.
- The parity-check result is in `/var/local/emhttp/var.ini`, and Unraid's cached SMART output per slot in `/var/local/emhttp/smart/<slot>`.

**Rejected sources:**
- *`userprefs.cfg`* — incomplete in one direction and stale in the other (evidence above).
- *Reading `docker.img` or the Docker directory after adoption* — its layout differs between servers (btrfs image, XFS image or directory mode), it is Docker's internal on-disk format, not an API, and on the cache it is gone once the cache is re-created (doc 05 §2).
- *Parsing `config/super.dat`* — an undocumented binary format; `disks.ini` gives the same table as text.

**Deferred:** a per-container `docker diff` capture (it would let doc 04 §5's writable-layer warning name paths), and drift detection between a container's inspect output and its template. On the calibration server image, network, ports and variables matched their templates for all 35 containers.

### Q26 — Share ownership and UID/GID model *(gap)*
**Status:** Default (verify on fixture) · **Gate:** Phase 2 · **Affects:** doc 03 §4.2, §7, doc 04 §5, §7, doc 05 §4

**The gap:** none of the docs define file ownership. Unraid data is conventionally owned `nobody:users` (99:100), and Unraid templates pass `PUID=99 PGID=100`. On Debian, `nobody` is UID 65534; GID 100 (`users`) is the same on both.

**Default:** GID 100 (`users`) is the shared data group. Share directories are `2775` (setgid); Samba create/directory masks are `0664`/`2775`; SMB users are members of `users`. Migrated files keep their numeric UID 99 untouched. Hoserva creates a `hoserva-apps` system user pinned to UID 99 when that UID is free on the host (the pre-flight checks), and curated templates default to `PUID=99 PGID=100`. Converted Unraid templates keep working with no ownership rewrite.

**The system account behind an SMB login (#596).** Samba's default `tdbsam` passdb refuses `smbpasswd -a` for a name that no Unix account holds, so setting a user's password also provisions one, one per Hoserva user — a shared account would make every user the same Unix identity, and file ownership and `write list` could not tell them apart. The account is named exactly as the user, has no home directory and the shell `/usr/sbin/nologin`, is locked (no Unix password, so it cannot log in to the host, only to Samba), has `users` (GID 100) as its primary group, and takes a UID from the reserved range **30000–39999**, which `useradd` picks itself (`-K UID_MIN`/`UID_MAX`). Hoserva treats an account as its own only when its UID is in that range and its shell is `nologin`: a username that an account outside that already holds (`root`, `daemon`, a person's login) is refused with a conflict rather than given an SMB password, and deleting such a user never removes that account. Deleting a user runs `smbpasswd -x`, then `userdel`: once the Samba entry is gone nothing can log in, so a failure of the second step leaves a locked account with no password and a retry finishes it; a `smbpasswd` failure after the account was created removes it again. The account is created lazily, at the first password, so a user who never gets a password has none, including an account the Unraid import seeds, which gets its system account when its password is set. Usernames are lower case (`^[a-z_][a-z0-9_.-]{0,63}$`), because Samba and the system account match names without regard to case: create folds the name to lower case and refuses one outside that form, and a unique index on `lower(username)` makes two names differing only in case impossible. Upgrading a database that already holds such a pair (only a hand-edited one can) is refused with a message naming the accounts, before anything is changed, since which one keeps its name is the owner's decision; no row is lost.

### Q27 — User roles *(gap)*
**Status:** Default · **Gate:** Phase 2 · **Affects:** doc 03 §7

**Default: three roles.** *Admin*: full UI. *Viewer*: read-only UI. *Share-only*: SMB/NFS access, no UI login at all, and the default for newly created users. Setting a password writes both the UI hash and the Samba passdb entry in one action.
Doc 03 §7 gives every user both UI login and SMB access. That means every family member with a share login can reach a UI that formats disks, which is the wrong default on a box that runs as root.

**Share access is closed by default (maintainer decision 2026-10-04, #594).** A non-guest share that no user or group has been granted renders `available = no` in the generated `smb.conf`, so no account reaches it until access is granted on purpose (doc 03 §4.2). Least privilege is the reason: an account that exists for one share never gains another by default, and an empty `valid users` list would otherwise mean every account.

### Q28 — Secrets at rest *(contradiction)*
**Status:** Default · **Gate:** Phase 1 · **Affects:** doc 10 §1, doc 11 §2, doc 03 §1

**The contradiction:** doc 10 §1 encrypts secrets "with a key derived from a passphrase the user sets". But `hoservad` must use those secrets unattended, at 03:00, to deliver the alert that a disk died. A passphrase-derived key has no one to type the passphrase.

**Default:** at runtime, secret columns are encrypted with a machine key in `/etc/hoserva/secret.key` (root, `0600`). That protects against the database file leaking, via a diagnostics bundle or a copied backup. **Backups** re-encrypt the secrets section with a *backup passphrase* set during onboarding (doc 03 §1). A restore without the passphrase restores everything except secrets, and says so, as doc 10 already describes.

**In-place reading (#443):** an archive of this installation carries its database secret columns sealed under this installation's own machine key, which the box already holds, so an in-place restore brings them back with the database and the passphrase protects nothing there. Only the passphrase-only section (the stacks' `.env` files, Q80) depends on the passphrase: without one, or with a wrong configured one, the import restores everything else and its report says the `.env` files were not restored; a passphrase given explicitly that does not open `secrets.age` is refused before anything is written. "Everything except secrets" protects the archive off the box, which is the bare-metal case (#276, #448): it re-seals the database secrets from `secrets.age` under the new machine key, through the reader `backup.ReadSecrets`.

**Bare-metal reading (#276, maintainer decision 2026-09-29; secrets: #448):** a fresh install, for the import, is one with **no array configured**, not one with no admin, so the restore also works from the Restore section after onboarding has created an admin. A restore onto a fresh install always keeps this box's own `machine_key_check` row, so `hoservad` starts. What happens to the secrets sealed under the archive's machine key depends on the backup passphrase (the request's, or else the configured one) and on which files it opens:

- **With a passphrase that opens `secrets.age`**, each database secret in it (ACME, UPS, backup-destination and notification-channel credentials) is sealed under this box's machine key and written back into its own table, column and row, and the stacks' `.env` files are restored (`0600`); the passphrase becomes this box's backup passphrase, since the setting is otherwise unreadable after the restore and remote destinations refuse to write without one.
- **With one that also opens `identity.age`**, the archive's backup recipient replaces this box's own (Q80): its identity is wrapped under this box's machine key, the recipient check value is recomputed, and the running daemon encrypts to it from then on, so future archives keep the same recipient and installation id. Every restored value is sealed before the first write, and the writes are one transaction on the staged database.
- **Without one**, everything else is restored, every secret column is cleared, this box keeps its own `backup_recipient` row, and the report lists each cleared secret (`database_secret`) and the kept recipient (`backup_recipient`). A passphrase given explicitly that does not open `secrets.age` (or, for an archive with an `identity.age` and no `secrets.age`, `identity.age`) is refused (400 `backup_passphrase_incorrect`) before anything is written; one that opens `secrets.age` but not `identity.age` is restored and the report says the recipient was kept.
- **Never carried, always cleared**: TOTP secrets (`totp_secret`, `totp_pending_secret`), with `totp_confirmed_at` and `totp_last_step`, so the next login enrols again, and any credential a pre-#449 archive does not hold. Each is reported.

In every case the database is one the next start accepts (machine key check, recipient check). A test lists the schema's `BLOB` columns and fails on one that is neither cleared nor classified, so a new sealed column cannot slip past the restore.

**Implementation note (#22):** the key is generated at first start, not at install — a fresh install has nowhere else a key could have come from, and doc 01 §7 requires no default credential of any kind to exist before setup. It is generated exactly once per installation, atomically (a temp file, fsynced, then linked — never renamed — into place, and the temp name removed once the link succeeds, so exactly one directory entry with link count 1 exists afterward, not two hard links to the same key material), and never regenerated afterward: a check value (an HMAC of a fixed constant under the key) is written to the database alongside it and verified on every later start, so a missing key file with an existing check value or existing encrypted secrets, or a present key file that doesn't match the recorded check value, is a fatal startup error naming this doc, not a silent replacement that would permanently strand every secret the real key could still have decrypted. The key file's owner is checked too, not only its mode: root can read a `0600` file regardless of who owns it, so a key file owned by neither root nor the daemon's own uid (a dev run's uid stands in for root there) is refused outright, even though its permissions alone would pass.

**Recovery:** every one of the fatal-mismatch errors above names this entry ("Q28 recovery") directly. In both cases below, stop `hoservad` and back up the database file before touching it.

- **No encrypted secret exists yet** — a fresh install: no TOTP enrolled, no other secret column ever written. Run `DELETE FROM machine_key_check;` against the database, then restart `hoservad`. `LoadOrGenerateMachineKey` then sees a fresh install: if a key file is still present at `/etc/hoserva/secret.key`, it adopts that file as the installation's own key and records a fresh check value for it — it does not generate a new one. Only if the file is also absent does it generate a brand new key. Either way, the procedure closes the mismatch and hoservad starts.
- **An encrypted secret already exists** — in practice, TOTP. That secret is permanently unreadable under a key that no longer exists; regenerating a key over it is never the fix, since that would just as permanently strand whatever the *next* key encrypts the moment it too goes missing. Clear it, not only the check value: for the affected account(s), set `users.totp_secret`, `users.totp_pending_secret` and `users.totp_confirmed_at` to `NULL` and `users.totp_last_step` to `0`; then run `DELETE FROM machine_key_check;`; then restart `hoservad`. Clearing `totp_secret` alone is not enough: `totp_confirmed_at` staying set leaves `TOTPEnrolled()` (internal/api/authstore.go) reporting true with no secret left to decrypt, so every login for that account would fail with an internal error instead of prompting to re-enrol. Once the daemon is back up, re-enrol TOTP (and re-enter any other affected credential) exactly as at first setup.

A key file owned by the wrong account is not a lost-key situation at all — restore the file's ownership (root in production) rather than replacing its contents or touching the database.

### Q29 — Resuming interrupted jobs *(contradiction)*
**Status:** Default · **Gate:** Phase 1 · **Affects:** doc 01 §4, doc 02 §4, doc 09 §2, §4

**The contradiction:** doc 01 §4 says interrupted jobs are "never silently resumed". Doc 09 §4 says an evacuation "must survive a daemon restart and pick up where it left off".

**Default: jobs are never resumed automatically. Resumable job types (mover, rebalance, evacuation, share relocation, and the data- and parity-disk upgrades) persist a checkpoint and resume from it, never restarting from zero, when the user clicks Resume or, for the mover only, at its next scheduled run.** Sync, scrub and fix are not resumable; they are re-run. Both documents' intents survive: no surprise background work after a crash, and no repeating a day of copying.

Finishing a disk's removal (doc 09 §4 steps 7–9, `disk_remove`, #358) is re-run too, not resumed: it is not in the resumable set and keeps no checkpoint. Its progress is the disk's persisted removal state (`unpooled`, `unlisted`), which every run starts from, so a re-run after a failure or restart skips the steps already done and never repeats the step-8 sync once the disk is `unlisted`.

### Q30 — Nightly schedule ordering
**Status:** Default · **Gate:** Phase 1 · **Affects:** doc 02 §2, §3, doc 03 §8.4, doc 09 §2, §6

**Default: one chained nightly maintenance run, starting 02:00: mover → diff + guard → (touch, Q17) → sync → config backup. On the weekly day, scrub runs after the sync.** Each step starts when the previous one finishes, not at a clock time.
With fixed clock times (mover "before" a 03:00 sync), a mover run longer than an hour silently breaks the ordering. Chaining makes the order structural. The schedule page's conflict detection then only matters for jobs the user schedules separately.
The daemon wakes the schedule loop once a minute (or coarser) and compares the persisted start time against now in the installation timezone. A missed night is not backfilled. Last-run is recorded when a window is claimed, so a restart inside the window cannot start a second overlapping chain. Separately scheduled jobs share this loop once their RunFuncs are registered; unregistered job IDs are skipped.

### Q31 — Spindown acceptance criterion
**Status:** Default (confirmed in the lab, S1, 2026-09-15 — L3 confirmation still open) · **Gate:** Phase 1 · **Affects:** doc 02 §1, doc 06 §6

**Default: array disks stay in standby for ≥ 30 minutes with no SMB/NFS clients connected, no containers holding pool paths open, and appdata on cache. The measurement window must start only after array disks have settled (below) — not immediately after a prior probe or workload has touched them.** The measured result is published with the release, including what breaks it.
This is doc 08's refinement. It is measured by doc 06 §6's zero-IO proxy in the lab and L3; firmware-level wakes are stated residual risk. **Confirmed in the lab, with no exceptions**: six 30-minute windows (idle, appdata, and an idle-connected-SMB-client scenario, each at a short and a long mergerfs cache-timeout setting) all showed **zero** movement in any of the 17 `/sys/block/<dev>/stat` fields on any array disk, and zero `fatrace` events on any array disk, with per-array-disk `fatrace -c` attribution verified by a positive control covering both a direct-disk write and a write routed through the mergerfs pool mount (doc 08 §1). A first pass at this confirmation found one window with a small write-side delta and explained it with the wrong stat fields, parsed by eye; a corrected run added a settle gate (an explicit `sync`, then wait until every array disk is unchanged for 3 minutes) between the positive control and the acceptance matrix, and a standalone characterization that measured deferred XFS writeback settling within ~65 seconds of a probe write on this lab's loop devices (`xfssyncd_centisecs=30s`) — with the settle gate and a per-window `sync` in place, the re-run matrix is flat throughout; see doc 08 §1 for the full timeline and the corrected field-by-field deltas. The lab run also found that raising the cache-timeout setting made no observable difference in any of the three scenarios tested — not evidence the setting is unnecessary, since none of the tested scenarios re-walks the pool tree the way a real client's directory browsing, an indexer or `updatedb` would; see doc 08 §1 for the residual risk this leaves open (no real Docker container, no NFS client, no browsing/indexing client, no `updatedb`, no SMART polling, no L3/firmware coverage, and fanotify-based attribution's blind spot for writeback-only IO, now measured at up to ~65s on loop devices but not established on real disks).

### Q32 — Wake attribution scope
**Status:** Default · **Gate:** Phase 1 / Phase 4 · **Affects:** doc 07 §1, doc 03 §3.3

**Default: a spin-state event log (per-disk transitions with timestamps, polled without waking disks) ships in Phase 1. Process and container attribution via fanotify is targeted for Phase 4 and may slip past 1.0 without blocking the release.**
The event log is cheap and makes R1 diagnosable from day one. Attribution is the differentiator doc 08 calls out, but it has no prior art, and 1.0 shouldn't wait on it.

### Q69 — Startup order and a disk missing at boot
**Status:** Default · **Gate:** Phase 1 · **Affects:** doc 02 §1, §4, §6, doc 04 §3, Q12, Q21, Q71

**Default: data, parity and cache mounts are `nofail`, so a dead disk never hangs boot — `nofail` alone does this by dropping the mount from `local-fs.target`'s required ordering; `x-systemd.device-timeout=` is not part of this, since that option only applies to an `/etc/fstab` entry and is silently ignored in a native `.mount` unit's own `Options=`, so Hoserva's generated units never emit it. Every mountpoint directory is made immutable while empty, so a write to an unmounted path fails instead of landing on the boot device — for the array slots (data, parity, cache) when their mountpoint is assigned, and for those slots and the catch-all `/mnt/user` on every startup and array rebuild pass, with each slot and `/mnt/user` also guarded right after `array stop` unmounts it and again just before `array start` mounts it; a path whose directory already holds files or whose filesystem refuses the immutable bit is logged as a warning and left as it was (doc 02 §1). Samba, NFS, Docker and libvirt start after `hoserva-storage.target` through managed systemd drop-ins; `hoservad` reaches that target only when every expected disk is present by identity (Q21), or once the user acknowledges the degraded state — and never while a larger-data-disk upgrade is pending (doc 02 §4, UR2). A disk present by identity but positively carrying a different filesystem than its slot's own record is `wrong_filesystem` (#388), not `active`; one present by identity whose own mount attempt still failed or timed out — most commonly a genuinely blank same-serial replacement, whose filesystem UUID was never positively read either way — is `mount_failed` (#398), a separate `GET /pool` slot state recorded from the daemon's own last bounded mount attempt, never from a device probe on this read path. Both offer `hoserva disk replace`, restricted to that slot's own device; for `mount_failed`, the replace path runs a one-off, bounded probe against exactly that device before allowing it — `blkid -p` first, then, only once it reports no signature, a read-only readback of the device's own first and last MiB (since `blkid`'s own exit 2 also covers a device it simply could not read, not only a genuinely blank one) — positively confirming no filesystem or partition-table signature at all and that the device reads cleanly, never on `List()` or any other poll path. `mount_failed` is recorded from both `Startup` (boot) and `Update` (a live SIGHUP arrival while `hoservad` is already running), each bounded the same way, so the state is current whichever path a same-serial-blank disk actually arrives on.**
Without this, a container that starts before `/mnt/user` is mounted writes its data onto the boot device and shows the user an empty app — a quiet, common homelab failure. A missing disk is also exactly when the guard's zero-files rule has to hold (doc 02 §2), so nothing that writes to the pool runs before a human has seen the degraded state.

### Q70 — Stopping the array and shutting down
**Status:** Default · **Gate:** Phase 1 · **Affects:** doc 01 §3, §4, doc 02 §4, doc 03 §3.2, Q29, Q69, Q71

**Default: `hoserva array stop` — *Stop array* on `/storage` — puts the system in maintenance mode: new jobs are refused, running resumable jobs stop at their next checkpoint and the rest are marked interrupted, VMs shut down (gracefully, then forced after a timeout), containers stop, Samba and NFS stop, Docker and libvirt stop with them, the storage-target gate itself closes, then the per-share mounts, the catch-all and the disks unmount. `hoserva array start` reverses it. System shutdown and reboot run the same sequence through `hoserva-storage.target`. The replace and upgrade flows require maintenance mode, or a powered-off box, before a disk is physically touched. A larger-data-disk upgrade (Q71) is the one job maintenance mode admits, and only once the stop sequence has completed. From submit until it succeeds, fails or is cancelled, it holds the array stopped, across daemon restarts and reboots too. While it is pending, `hoserva array start` and boot's readiness gate (Q69) are refused, and so is `hoserva array stop`; system shutdown, reboot and the UPS path still run the stop sequence. That upgrade's state machine in doc 02 §4 is the authority on what each command does meanwhile. A user-requested `array stop` persists maintenance mode and the "stop sequence completed" state in SQLite (#387, D16), so a crash or a package-upgrade restart of `hoservad` alone while the array is stopped this way comes back up still stopped, never silently back to normal operation. System shutdown, a reboot and the UPS low-battery path run the identical stop sequence but never themselves persist a new stopped state, and never create or clear the durable array-stopped condition flag every generated mount unit's `ConditionPathExists=` checks (doc 02 §4) — none of the three is a user asking the array to stay stopped once the box comes back — so a plain reboot or UPS shutdown of a running array comes back up on the next ordinary boot, while a persisted user `array stop` already in force, and its own durable flag, survives either one exactly as it was. On every daemon start `hoservad` reconciles that flag from the persisted row, regenerates every managed disk and pool mount unit from SQLite, and — once the row says the array is not stopped — explicitly (re)mounts every disk and the pool itself, so a boot where the flag was stale, or an array predating the flag's own condition, can never come back with something silently left unmounted. Docker and libvirt stop on `array stop` because they read the pool the same way Samba and NFS do — the same `ServiceUnitController` enabled/masked-aware rule already used to bring them back up, applied to stopping them too.**
Every NAS owner eventually needs "stop everything so I can swap a disk". Without a defined order, a container holding a file open blocks the unmount, or a disk is pulled mid-write.

### Q71 — Replacing a healthy disk with a larger one
**Status:** Default · **Gate:** Phase 2 · **Affects:** doc 02 §4, doc 03 §3.2, Q14, Q20, Q29, Q70

**Default: two guided flows, each keeping the old disk untouched until the new one verifies.**
- **Larger parity disk:** copy the parity file to the new disk, verify it byte for byte, switch the configuration, and pass `snapraid check` before the old parity disk is released — the array stays protected throughout. When a new data disk would be larger than the current parity, the flow offers this first and then reuses the old parity disk as a data disk.
- **Larger data disk:** with the array stopped (Q70), and `snapraid diff` showing nothing to sync, copy the old disk's files to the new one, preserving ownership, xattrs and timestamps. Verify the copy, then mount the new disk at the same `/mnt/diskN` by its filesystem UUID. Require `snapraid diff` to show no removed or updated files before the old disk is released.
  - The job mounts every array disk itself, and unmounts all of them whenever it stops.
  - The pool and every service stay down until the upgrade succeeds, fails or is cancelled.
  - Cancelling is the abort back to the old disk. It is allowed until the job reaches its release step.
  - Once the copy has verified, nothing goes back to copying.
  - Doc 02 §4 ("Larger data disk: the upgrade's state machine") specifies every checkpoint against cancel, interruption, restart, `array start`, `array stop`, resume and a second upgrade. The implementation follows it exactly.

Both follow SnapRAID's documented replacement procedures, and the exact diff expectations are confirmed against SnapRAID 12.4 in the lab before this ships. Rebuilding from parity (`snapraid fix`) stays reserved for failed disks: it leaves the array without redundancy for the rebuild's duration, which is needless while a healthy source disk exists.

### Q72 — Disks outside the array
**Status:** Default · **Gate:** Phase 2 · **Affects:** doc 00 §4, doc 01 §3, §6, doc 03 §3.3, doc 10 §1

**Default: a narrow "external disks" feature. A disk with the Ignore role, or a USB disk plugged in later, can be mounted by filesystem UUID at `/mnt/disks/<label>` and ejected safely (unmount, then spin down). External disks are never in the pool or parity, are ignored by the threshold guard and the change journal, and can be a backup destination (doc 10) or a container path. Nothing mounts automatically on plug-in, and formatting one takes the same typed confirmation as an array disk.**
Doc 10 already names an unassigned disk as a backup destination, and the same `/mnt/disks/` convention Unraid users know keeps migrated container paths meaningful.

### Q73 — Share size limits
**Status:** Default · **Gate:** Phase 2 · **Affects:** doc 00 §4, doc 03 §4.2

**Default: no per-share quotas in v1. The one exception is Time Machine: a Time Machine share has a maximum size, enforced by Samba's `fruit:time machine max size`.**
mergerfs has no quota across branches, and per-disk XFS project quotas can't express a share-wide limit. Time Machine grows until its destination is full, which on a pool means until every other share stops accepting writes; everything else is covered by per-disk free-space alerts (doc 09 §5).

---

### Q61 — iSCSI
**Status:** Default · **Gate:** Phase 2 · **Affects:** doc 00 §4, doc 03 §4

**Default: no iSCSI target in v1, and not a planned post-1.0 feature either — revisit only if real demand shows up.**
A recurring complaint about Unraid is the lack of iSCSI without a plugin, most often for a datastore backing a separate hypervisor host. Hoserva's own VM manager (doc 14) already covers that case with local qcow2 vdisks, so there is no gap to fill for Hoserva users specifically. Running an iSCSI target (LIO/`targetcli`) is its own security surface — raw block devices exposed over the network — and its own orchestration surface, for a narrow slice of the target user (doc 00 §3). Chasing feature parity with general-purpose NAS platforms before the core is solid is a named failure mode (doc 07 §4); this is exactly that temptation, stated and declined rather than left open by omission. If it does get built later, it is scoped the way containers were (D6): a handful of guided cases, not a general SAN feature set.

---

### Q85 — Periodic statfs(2) polling and spindown safety
**Status:** Default · **Gate:** Phase 1 · **Affects:** doc 09 §5, `CLAUDE.md`

**Default: a per-disk `statfs(2)` call on the existing minute schedule-tick cadence does not violate "nothing on a timer walks a data disk."** `statfs(2)` reads a mounted filesystem's own cached VFS/superblock free-space counters — the same call `df` issues — never a directory walk or a data read, so it needs no I/O to a spun-down disk's platters to answer. Doc 09 §5 already treats how often this is recomputed as a caching tunable (`cache.statfs`) rather than a spindown hazard, and `internal/pool.ComputePoolSpace` already read every data disk this way for `GetPool`'s on-demand path (#57) before the periodic disk-near-minfreespace/rebalance-suggested check (#237) added a second, ticking caller. This is confirmed from `statfs(2)`'s own semantics and doc 09 §5's existing framing, not from an instrumented lab measurement — doc 08's existing spike runs did not specifically instrument `statfs` wake behaviour, so a follow-up measurement stays open if that assumption ever needs harder evidence.

### Q87 — Cache usage breakdown without a live directory walk
**Status:** Default · **Gate:** Phase 1 · **Affects:** doc 03 §3.6, doc 09 §2, `#273`

**Default: the cache page's appdata / pending-moves / other breakdown is computed as a by-product of each mover run, persisted in SQLite, and read by `GET /cache/usage` — never recomputed on a timer or on each page load.** After `cache.Run` finishes (including interrupted or failed runs that still produced a started report), the mover job walks every share's cache-side directory once to sum bytes by cache mode (`cache-only` → appdata, `cache-then-move` → pending moves), then takes a single `statfs(2)` of the cache mount (Q85) for total used and sets `other = max(0, used − appdata − pending)`. That walk is part of an Array-write job the user already asked for (schedule, threshold, or manual), not a polled live `du`. Until the first mover run that can resolve a cache disk has finished, the API returns null — the same honest "not yet computed" shape share usage uses before the first sync (#223).

A live request-time walk would wake the cache for every UI poll and would also be the wrong place to invent a second accounting path beside the mover's own enumeration. Recomputing only when the mover itself already touches those trees keeps Q13 intact and keeps the figures "as of the last mover run", which is exactly what doc 03 §3.6's last-run panel shows next to the breakdown.

---

## Containers

### Q33 — Default catalog source on a fresh install
**Status:** Settled → D19 · **Affects:** doc 04 §4, §7

**The curated Hoserva catalog is the only built-in source; users may add their own catalog source URLs. The Unraid XML converter is always available for local templates.**

### Q34 — Third-party catalog feeds
**Status:** Settled → D19 · **Affects:** doc 04 §4, doc 07 R4

**No third-party catalog feed is built in.** The catalog is Hoserva's own (doc 04 §7).

### Q35 — Licensing of catalog templates
**Status:** Settled → D19 · **Affects:** doc 04 §7, Q39

**Every curated template is written by the project from the application's upstream documentation, so the catalog carries no third-party template license.** Each packaged application keeps its own upstream license.

The catalog repository (`mdg-labs/hoserva-catalog`, Q39) is licensed **MIT**, not this repository's AGPL-3.0 (D17): templates are configuration that users copy and change for their own servers, and third-party catalog authors should be able to reuse them without taking on copyleft obligations. Contributions to it carry the same DCO sign-off as this repository (Q2).

### Q36 — What counts as a "clean" template conversion
**Status:** Default · **Gate:** Phase 3 · **Affects:** doc 04 §5, doc 06 §2

**Default: "clean" means the generated Compose file needs no manual action. Informational warnings (`:latest` tag, a dropped `<Shell>`) are allowed; untranslated `ExtraParams`, unresolved networks, or paths flagged for review are not.** The converter's clean-conversion release metric, which must not regress, uses this definition.

### Q37 — Container networks
**Status:** Default · **Gate:** Phase 3 · **Affects:** doc 03 §5.4, doc 04 §1, §5, §7

**Default: the install flow lists the *existing* Docker networks and offers bridge, host, or any of them (macvlan/ipvlan included); it never creates one. v1 has no network-creation UI. When a template needs a network that doesn't exist, the converter's warning includes the exact `docker network create` command. A network chosen at install that doesn't exist is reported in the install plan with the same exact command, and the install is refused until the network exists.**
Doc 03 said "custom", doc 04 said "beyond bridge/host/macvlan selection", and doc 04 §5 said "requires a pre-existing network". This default reconciles the three while staying inside D6. The command appears on both paths, in the converter's warning and in the install plan, so the user can create the network themselves and come back to the same install; refusing until it exists keeps a missing network from becoming a failed start, and nothing is ever created on the user's behalf.
When the migration capture's `networks.json` is present (Q89), the command for a macvlan, ipvlan or custom bridge network can be given exactly (driver, subnet, gateway, parent interface). Without it, the converter gives the command with placeholders and says so.

### Q38 — Minimum Docker version
**Status:** Default · **Gate:** Phase 3 · **Affects:** doc 04 §3

**Default: negotiate the Engine API version at runtime rather than hard-coding "Engine 24+". Require the Compose v2 plugin. `hoserva doctor` warns when the installed Engine is older than a tested floor: the newer of the oldest release upstream still supports and the first release without a published container-escape advisory.** Documentation points to Docker's apt repository.
A version number frozen into a 2026 spec is already stale by the time Phase 3 starts.

The floor is `dockerEngineMinVersion` in `internal/api/doctor.go`, raised over time. It is currently Engine 29.5.1, the first release that fixes GHSA-x86f-5xw2-fm2r, GHSA-vp62-88p7-qqf5, GHSA-rg2x-37c3-w2rh and GHSA-pxq6-2prw-chj9 (container-to-host escapes reachable when a container from a malicious image receives a copy or archive upload — `docker cp` or `PUT /containers/{id}/archive` — and a plugin-privilege check bypassed by installing a malicious plugin). The floor stays a warning, never a refusal: Apps keep working on an older Engine, and Docker remains the user's own install (D8). The Engine client is `github.com/moby/moby/client`, whose API version negotiation is on by default and supports Engines from API 1.40 (Docker 19.03) up.

### Q62 — Docker container storage backend
**Status:** Default · **Gate:** Phase 3 · **Affects:** doc 04 §3

**Default: standard Docker Engine directory-based storage — `overlay2`, data-root a plain directory on cache (`/mnt/cache/docker`) — never a fixed-size loopback image.**
A loopback image that must be manually resized when it fills is one of the most common Docker complaints on Unraid, and it is self-inflicted: standard Docker Engine already defaults to directory-based `overlay2` storage, and the loopback image is an Unraid-specific choice to keep Docker's storage in one movable file. Hoserva has no reason to reproduce it — the Engine is a normal prerequisite (D8) pointed at a normal directory, sized by the cache device itself, which already has its own capacity monitoring (doc 02 §3). One less way to run out of space by surprise.

### Q39 — Where curated templates live
**Status:** Default · **Gate:** Phase 3 · **Affects:** doc 04 §7, doc 12 §1, §2, §7, Q35, Q65, Q66

**Default: the curated catalog lives in its own repository, `mdg-labs/hoserva-catalog`, from the start — before any template exists. It holds `<id>/compose.yaml` plus an icon per template (Q64), its own CI, its own issue tracker for template requests and PRs, and its own MIT license (Q35). This monorepo keeps the `x-hoserva` schema and its validator (`internal/template/`); the catalog repository's CI runs Hoserva's own checker from a pinned Hoserva version instead of copying its rules. The schema is published as a versioned JSON Schema, its `schema:` number is the compatibility boundary, and a newer Hoserva keeps reading older schema versions. This repository embeds a pinned catalog archive (Q65), and tests that use curated templates read that pinned snapshot, never a live fetch.**
This replaces the earlier default of `templates/` in the monorepo until the first external template PR, and the reasons to split now rather than then:
- Nothing exists yet. Splitting now costs a docs change; splitting at the first external PR means moving history, CI, issues and labels while contributors already use the old layout.
- The issue tracker here is the project plan. Template requests and template PRs are a different kind of work with a different review bar, and they belong in their own tracker.
- Catalog CI depends on outside services (registry image and tag existence, `docker compose config` per template) and should not gate a Hoserva release (doc 12 §7).
- Our catalog becomes a normal instance of the user-source format (doc 04 §4): built in its own repository exactly as a third party's would be.

Issue tracking follows the split: Hoserva-side catalog engineering (schema, validator, lint command, fetch and verify, UI) stays on `mdg-labs/hoserva`, and template content work is tracked in the catalog repository. The Unraid corpus in `testdata/unraid-templates/` stays here (doc 06 §2).

**Revised (2026-10-01, #505):** the catalog repository keeps its templates in a `templates/` folder, `templates/<id>/compose.yaml` plus its icon, so the repository's first page stays short as the catalog grows; its tooling and fixtures live under `.ci/`. The published archive is unchanged: `index.json` and `<id>/` at its root. Doc 04 §7.

---

### Q64 — Template format
**Status:** Default · **Gate:** Phase 3 · **Affects:** doc 04 §7, Q39

**Default: a template is `<id>/compose.yaml` plus an icon in the catalog repository (Q39) — a valid Compose file with an `x-hoserva` extension block holding its inputs (kind, path role, default), metadata and a revision. The privilege summary is computed from the Compose content, never declared by the template.**
A custom YAML schema would need its own converter to Compose and its own validator. A Compose file with an extension block is checkable with `docker compose config` and runnable as-is, and it is the format contributors already know.

**Revised (2026-10-01, #505):** in the catalog repository a template is `templates/<id>/compose.yaml` plus an icon, inside its `templates/` folder (Q39); the published archive still holds `<id>/` at its root. Doc 04 §7.

**Revised (2026-10-01, #502):** a `string` input may declare `optional: true`, so a template can offer a value the upstream image treats as optional, such as a one-time claim token. An optional input may resolve to the empty string, which is written to `.env` as an empty value, and it has no `default`; `optional` is refused on every other kind. Doc 04 §7.

### Q65 — How the catalog reaches installations
**Status:** Default · **Gate:** Phase 3 · **Affects:** doc 03 §5.2, §8.4, doc 04 §4, §7, doc 01 §7, Q39, Q49, Q66

**Default: the catalog repository's CI (Q39) publishes one signed `catalog.tar.zst` and its detached Ed25519 signature as static files from that repository's own GitHub Pages site at `catalog.hoserva.dev`; the compiled-in default catalog URL points there. The catalog has its own Ed25519 key, separate from the release-signing key: the private half is the GitHub Actions secret `HOSERVA_CATALOG_SIGNING_KEY` on `mdg-labs/hoserva-catalog` and is held nowhere else, the public half is `signing-key.pub.pem` at that repository's root (both OpenSSL PEM), and before signing CI checks that the secret's public half matches the published one, the same integrity check `release.yml` runs for the release key. `hoservad` compiles in two public keys, one for updates (Q67) and one for the catalog, and checks each file only against its own. `hoservad` embeds a snapshot at build time: this repository commits one published archive's serial and SHA-256, and the build fetches that archive, checks the pin and the signature, and embeds it, so an update to the snapshot is a normal commit that bumps the pin. The refreshed copy lives in `/var/lib/hoserva/catalog/`, and every refresh is one conditional request for the archive and, after a `200`, one for its signature, with the same signature and serial checks; a signature that fails verification makes the check fetch the pair once more before it reports the failure. The triggers are a background interval the user sets — off, hourly, every 6 hours, every 12 hours or daily, daily by default, each with random jitter; a check when the catalog is read through the API and the last check is older than 15 minutes, on by default and never waiting on the network; and a manual *Check for updates* that always works and always reports a failure, where the automatic checks do not repeat a notification for the same failure. With the interval off and check-on-open off, Hoserva contacts the catalog host only when the user presses the button. A new archive is used only if its signature verifies against the compiled-in catalog key and its serial is higher; an installed app never changes — a newer template revision is offered as a diff.**
One static, conditional request stays clear of any rate limit, works behind a CDN and degrades to the on-disk copy offline, where per-template fetches through the GitHub API would hit the unauthenticated limit. Nothing comes from `api.github.com`, and an unchanged catalog answers `304 Not Modified` and downloads nothing, which is why checking when the catalog is opened costs little: a template published during the day shows up on the next open, where a once-a-day check would hide it until the next morning. Templates can request privileged access, so an unsigned or replayed catalog must never be trusted. The key is separate because the catalog repository has more contributors and CI that signs on every merge: a leaked catalog key can at worst get templates signed, never a Hoserva release, a signature made for one kind of file can never pass as another, and each key can be replaced without touching the other. The archive is trusted through its signature, not its host, so the host can move later without a trust change.

The interval and the check-on-open switch are stored as additive columns on the `schema_info` settings row next to `update_check_enabled`, not as a `schedule_jobs` row: widening that table's `job_id` `CHECK` would need a table rebuild (D16). The check-on-open trigger lives in `hoservad`, so the CLI and the UI behave the same (D5), and completion is announced on `/api/v1/events` so the UI refreshes.

**Revised (2026-10-01, #495):** the signed archive has two sources, each with one job. Installations refresh from `catalog.hoserva.dev`, which always serves the latest archive. Hoserva's build-time snapshot fetches its pinned serial from the catalog repository's GitHub Release `serial-<serial>`, whose assets are `catalog.tar.zst` and `catalog.tar.zst.sig`, and checks the pin's SHA-256 and the signature exactly as before. The reason is that each Pages deploy replaces the whole site, so Pages only ever serves the latest archive: once the catalog is promoted past the pinned serial, CI and release builds would fail the pin check until the pin is bumped, and old tags could not be rebuilt. A release is immutable and gives the pin a permanent home. The refresh still never uses the GitHub API; a release download happens only at build time. Doc 04 §7.


### Q81 — Checking containers for updates
**Status:** Default · **Gate:** Phase 3 · **Affects:** doc 01 §7, doc 04 §6, Q49

**Default: at most once a day, with random jitter, Hoserva compares each managed container's image digest with the registry's by requesting only the manifest — never pulling. Credentials can be added per registry and are stored as secrets (Q28). A registry that answers with a rate limit is skipped until the next day, and the UI says the check was skipped. The check can be disabled, and it counts as an outbound request under Q49.**
Pulling to compare would count against registries' limits on anonymous pulls and waste bandwidth; one manifest request per image per day is cheap. Docker Hub's current limit policy is re-checked when this is built, since it has changed before.

**Revised (2026-10-01, #487):** telling a new version tag from a new build on the same tag (doc 04 §6) needs more than the manifest. The check requests each image's manifest and, only when the image's tag looks like a version (`16.4`, `v1.2.3-alpine`; never `latest`), the repository's tag list (`GET /v2/<repository>/tags/list`, in pages when the registry paginates). Both are metadata requests: the check never pulls an image or fetches a blob. A registry that answers either request with a rate limit is skipped until the next day, and the UI says the check was skipped. Where the manifest request already showed a new build, a rate limit on the tag list only means newer version tags were not looked for, and the new build is still reported.

### Q88 — How long the previous image is kept for a revert
**Status:** Default · **Gate:** Phase 3 · **Affects:** doc 04 §6, doc 10 §2

**Default: an update keeps the image the container ran for 7 days, and the period is a setting of 1 to 365 days (`imageKeepDays`, `GET`/`PUT /settings/apps`). A change applies to updates made afterwards; an update already recorded keeps the deadline it was made with. The image is held under a `hoserva-previous:<id>` tag that the Engine's own cleanup leaves alone, and Hoserva removes the tag when the period ends, unless a container still runs the image.**
A revert needs both halves of the pairing, the previous image and the appdata snapshot taken just before the update, so the image is kept about as long as an operator is likely to notice a bad update. A week covers a weekend and the next working days; longer holds disk space for images that are unlikely to be wanted again. The snapshot is bounded separately, by the 5 pre-change archives per container a destination keeps (doc 10 §2): a revert whose snapshot has been pruned is refused with `revert_unavailable` and changes nothing.

### Q82 — GPUs for containers
**Status:** Default · **Gate:** Phase 3 · **Affects:** doc 04 §7, doc 14 §3, Q53

**Default: an Intel or AMD GPU is offered to containers through `/dev/dri`, as a template input of kind `device` with role `gpu`, and the stack gets the host's `render` group. NVIDIA GPUs need the proprietary driver and the NVIDIA container toolkit on the host — a prerequisite like Docker (D8): `hoserva doctor` reports whether both are present and working, and the docs give the install steps. A GPU bound to `vfio-pci` for a VM is never offered to containers, and a GPU in use by a container is flagged in the passthrough check.**
Hardware transcoding for Plex and Jellyfin is one of the most common reasons to put a GPU in a home server. Installing a proprietary kernel driver needs root and ties the host to NVIDIA's release cadence — the same reasoning that keeps Docker external.

## Virtual machines

### Q51 — VM disk image placement and format
**Status:** Default · **Gate:** Phase 3.5 · **Affects:** doc 02 §3, doc 09, doc 14 §2

**Default: vdisks are qcow2, sparse, under `/mnt/user/domains/<vm-name>/` — a share like any other, using the same cache-mode choices as Q12.** A running VM's vdisk is never touched by the mover; relocating between cache and array is a stop-VM, relocate, restart operation.
The `domains` share path matches Unraid's own exactly (D10-style path compatibility), so a migrated VM's domain XML disk paths need no rewriting at all.

### Q52 — The threshold guard vs. large VM disk images
**Status:** Default · **Gate:** Phase 3.5 · **Affects:** doc 02 §2, Q13, Q15, Q16, doc 14 §2

**The gap:** a running VM can dirty gigabytes inside one qcow2 file between syncs. That's one file rewriting, not many files deleted, so it doesn't trip the guard by count — but the guard's ransomware-detection value (doc 01 §7's threat-model note) doesn't reach *inside* a VM's own filesystem, and a nightly sync can move a large amount of parity data for what looks like a single, unremarkable file change.

**Default:** vdisk shares are **not** given special guard exemptions — the existing count/percentage logic already doesn't trip on one large file rewrite — but the UI marks VM disk shares as "not diff-protected against changes inside the VM," so the guard's silence there isn't mistaken for a stronger guarantee than it is.

### Q53 — PCI/USB passthrough and VFIO binding
**Status:** Default · **Gate:** Phase 3.5 · **Affects:** doc 01 §7, doc 06 §6, doc 14 §3

**Default: IOMMU groups are detected and shown read-only at any time; a device is bound to `vfio-pci` only on explicit user assignment, applied at boot (IOMMU kernel parameter where needed, generated `vfio-pci` device list) and requiring a reboot** — static binding at boot, as Unraid also does, never a live unbind. A device the host itself depends on (boot controller, sole console GPU) is never offered as assignable. `hoserva vm passthrough check` reports IOMMU/ACS group isolation before the user commits to the reboot.
Live, in-session device unbinding is the single most common way passthrough bricks a box's own boot storage or networking; the reboot-applied model avoids that class of failure entirely.

### Q54 — VM networking
**Status:** Default · **Gate:** Phase 3.5 · **Affects:** doc 03, Q37, doc 14 §4

**Default: VMs default to a bridged interface (`vmbr0`) over the host's physical NIC, giving a real LAN-visible DHCP address** — the model Unraid uses and what homelab users expect from a VM that should act as its own network host (a router VM, a game server). An isolated/NAT network is offered as the alternative.
This is a separate layer from container networks (Q37), sharing only the narrowing discipline of offering the handful of options that cover real use cases, not a general network-topology editor.

### Q55 — Unraid VM migration mechanics
**Status:** Default (`libvirt.img`/`domains`/`isos` default paths confirmed via `unraid/webgui` source, doc 08 §2 — not yet by an adoption run against a VM fixture, which is spike S11's job) · **Gate:** Phase 3.5 (feeds doc 05) · **Affects:** doc 05, doc 14 §5

**The finding:** Unraid's VM Manager is libvirt underneath, so an exported domain is already libvirt domain XML — the format Hoserva itself generates. This is closer to doc 05's array-adoption problem than to doc 04's format-conversion problem.

**Default:** after disk adoption, the migrator attaches Unraid's `libvirt.img` (default `/mnt/user/system/libvirt/`, on the array — not in the Flash Backup) read-only, parses each domain's XML, rewrites only the fields that diverge (network bridge name, OVMF firmware path) and **re-validates passthrough device addresses against the target machine's own IOMMU scan rather than trusting the source** — those addresses are hardware-specific and the target is not guaranteed to be the same box. Vdisks under `/mnt/user/domains` are adopted in place with the rest of the array and verified by checksum like other adopted data, not copied. Nothing autostarts on import; rewritten XML is reviewed side by side with the source first, mirroring doc 04 §5.

### Q56 — Where VMs sit in the job system
**Status:** Default · **Gate:** Phase 3.5 · **Affects:** doc 01 §4, doc 14 §2

**Default: a new VM job class** (start, stop, create, delete, snapshot, clone, migration-import), mutually exclusive with other VM jobs on the *same* VM, independent of the Parity/Array-write/Topology/Service classes otherwise. Relocating a VM's disk between cache and array is instead an **Array-write**-class job, same as any mover/relocation action, and requires the VM to be stopped first.

### Q57 — VM console access
**Status:** Default · **Gate:** Phase 3.5 · **Affects:** doc 01 §5, doc 03, doc 14 §4

**Default: libvirt's VNC/SPICE graphics device is never exposed as a raw port.** The API proxies it over the existing authenticated TCP/TLS connection via a WebSocket to an embedded noVNC client in the web UI — same session auth as everything else, no separate credential or port.
Matches doc 01 §7's small-attack-surface posture; a raw VNC port is exactly the kind of second, usually-unauthenticated protocol that posture exists to avoid.

### Q58 — libvirt/QEMU dependency sourcing
**Status:** Default · **Gate:** Phase 3.5 · **Affects:** doc 00 D13, doc 14 §7

**Default: depend on Debian 13's own `libvirt-daemon-system` and `qemu-system-x86` packages directly**, the same sourcing posture as mergerfs/SnapRAID (Q7) rather than Docker's external-prerequisite model (D8) — these are stable, Debian-maintained packages without the fast-release version-churn problem that keeps Docker external.

---

## Backup

### Q40 — Default config backup destinations
**Status:** Default · **Gate:** Phase 1 · **Affects:** doc 10 §1, §4

**Default: two local destinations out of the box, `/var/lib/hoserva/backups` on the boot device and a path on the pool, plus the existing prompt to add an off-box destination.**
Doc 02 §6 lists "boot device fails" and "array lost" as separate failure domains. One copy on each covers both at zero cost. Doc 10's own point, that a backup stored only on the array it describes is not a backup, applies to the boot device just as much.

### Q41 — rclone
**Status:** Default · **Gate:** Phase 2 · **Affects:** doc 10 §1

**Default: rclone is an optional dependency (`Recommends:`). Local destinations work without it. The UI offers the install command the first time a remote destination is configured.**

### Q80 — Encryption of backup archives
**Status:** Default · **Gate:** Phase 2 · **Affects:** doc 10 §1, §2, Q28

**Default: every archive written to a destination other than a local path is encrypted with age (`filippo.io/age`) before it leaves the box; local destinations can opt in. Archives are encrypted to an age recipient generated once at onboarding. The box keeps that public recipient in the clear and the matching private identity wrapped under the machine key, never in the clear; the identity also travels with every archive, wrapped under the backup passphrase (age's scrypt mode) — as its own embedded `identity.age`, and, for a destination that encrypts, as a small sidecar alongside the archive itself, which is encrypted to the public recipient alone — so encryption runs unattended and a restore needs only the passphrase. A remote destination can't be added until a backup passphrase is set. Inside config backups, stacks' `.env` files — which hold template-generated secrets (Q64) — go in the passphrase-protected secrets section, never in plain text.**
Appdata archives hold application databases and credentials in plain files, and remote destinations are someone else's storage. age is small, audited and pure Go, and encrypting before rclone keeps the key out of rclone's configuration. Without the `.env` rule, Q28's protection of database secrets would be bypassed by the files sitting next to them in the same archive.

**Implementation note (#275):** age's own spec refuses to combine a scrypt recipient with any other recipient in one encrypted file ("an scrypt recipient must be the only one"), so "a restore needs only the passphrase" can't be a single age.Encrypt call to both the recipient and the passphrase. A destination that encrypts instead receives two files: the archive itself, encrypted to the recipient's public key alone, and a small sidecar carrying the private identity, encrypted to the passphrase alone (`identitySidecarSuffix`, doc 10 §1) — recover the identity from the sidecar with the passphrase, then use it to open the archive. The onboarding recipient (`backup_recipient` table) is generated exactly once, at first `hoservad` start, the same "never regenerated, a mismatch is fatal" contract as Q28's own machine key.

**Bare-metal restore (#448):** `identity.age` is what lets a restore keep protecting future archives with the archive's recipient rather than starting a fresh one. A bare-metal restore recovers the identity from `identity.age` with the backup passphrase, wraps it under the new box's machine key, and replaces the box's own `backup_recipient` row; the installation id in archive names and the retention that keys off it follow, since they derive from the recipient. Without a passphrase that opens it the box keeps its own recipient, and the restore report says so. An archive can hold `identity.age` and no `secrets.age` (no database secret and no `.env` to protect), so the passphrase is checked against whichever of the two the archive has.

**Pre-import safety archive (#458):** when an in-place import restores stack `.env` files, the pre-import archive seals its own `secrets.age` with the passphrase that opened the imported archive's (the request's, or the configured one when that opened it), so it holds every `.env` the import replaces even when no backup passphrase is configured. Otherwise the import would delete the only copy of each replaced `.env`. An import that restores no `.env` takes the archive as before, with no `secrets.age` when none is configured.

**Divergence (#275): the box persists the private identity too, wrapped under the machine key (`backup_recipient.wrapped_identity`) — not only the public recipient this default originally specified.** The identity is never regenerated (Q28-style), so it has to survive a daemon restart the same way the machine key itself does; and because every archive build re-wraps it under whatever the backup passphrase currently is (`buildIdentityAge`, run fresh each time, not only once at onboarding), the box needs the identity on hand at every nightly run and on-demand export, not just at the moment it was first generated — a passphrase change picked up on the very next archive, with no separate re-wrap step. Wrapping it under the machine key adds no new exposure: the machine key already protects `secrets.age`'s contents at rest the same way, under the same threat model.

---

## Security, CI and workflow

### Q42 — CI runners for a public repository *(security gap)*
**Status:** Spike (S9) — **closed: loop devices, FUSE and a hosted SnapRAID sync confirmed on the pinned hosted runner image; `/dev/kvm` CONFIRMED on a hosted runner; `apparmor=unconfined` CONFIRMED required for the lab container's own `mount(2)` on a hosted runner** (issue #10, doc 08 §9 "Hosted CI runners", run 35076920766) — so L3's hosted path is open, pending S10 (nested KVM, a separate, still-open question) · **Gate:** Phase 1 · **Affects:** doc 06 §7, doc 07 R11

**The gap:** doc 06 §7 runs privileged, nested-virtualisation jobs on self-hosted runners "every PR". On a public repository, a pull request from a fork can run arbitrary code on those runners, which here means a privileged host with loop devices.

**Default:** everything that executes pull-request code runs on GitHub-hosted runners: L1, L2 loop devices via the runner's `sudo`, `.deb` build, and L3 (a single non-nested guest). There are no self-hosted runners (Q79). Workflows from first-time contributors require approval (a repository setting). **What's confirmed, not assumed, as of S9's close (issue #10, doc 08 §9):** `ci.yml`'s own `lab` job has run loop devices, XFS and a mergerfs pool mount successfully on a hosted runner in production (run 34950031773, 2026-09-15, on `ubuntu-latest`), and the pinned-`ubuntu-24.04` probe has since confirmed all three of S9's questions on a hosted runner — a SnapRAID sync completing hosted (`Everything OK`), a QEMU guest booting with KVM acceleration confirmed host-side via QMP `query-kvm`, and `apparmor=unconfined` confirmed **required** for the lab container's own `mount(2)` on such a runner (run 35076920766: a control arm on `unconfined` succeeded, a variant arm on Docker's default profile failed with EBUSY at the same `mount(2)` call, and the standing lab under `unconfined` succeeded again immediately after — an A-B-A result that rules out leftover state or ordering as the cause). The errno being EBUSY rather than the more conventional EACCES for an AppArmor denial is recorded as an open, unexplained detail, not a blocker to the necessity conclusion. Getting there took six voided attempts (runs 35049304081, 35056076616, 35057453620, 35068617498, 35071011877, 35075054897) from a hand-rolled probe that reimplemented the lab recipe instead of exercising it — every void traced to a bug in that replica, never to AppArmor; one of those runs (35049304081) printed a "required" verdict from a broken experiment that was retracted at the time and is not validated by the later, independent result. `/dev/kvm` support and AppArmor necessity are no longer open — **S9 is closed**. S10 (nested KVM, VM-in-VM, a different, still-open question) is not answered by any of this.

### Q43 — API tokens
**Status:** Default · **Gate:** Phase 2 · **Affects:** doc 01 §5, doc 03 §7

**Default: personal API tokens, scoped to a role (admin/viewer), created and revoked on `/users`. They are used for scripting and for running the CLI against a remote host over TCP.**
Doc 01 calls the API public and doc 12 anticipates third-party consumers, but the only credentials specified were browser sessions and the local root socket.

### Q44 — Who may use the Unix socket
**Status:** Default · **Gate:** Phase 1 · **Affects:** doc 01 §5, §7

**Default: a `hoserva` group, created empty. Documentation states plainly that membership is root-equivalent (the API formats disks), exactly like the `docker` group.**

**Implementation note (#22):** the peer-credential check (`SO_PEERCRED`) also authorizes a connection whose uid equals `hoservad`'s own uid, in addition to uid 0 and `hoserva`-group membership. In production `hoservad` runs as root, so this adds nothing there; it exists because a non-root dev run (CLAUDE.md: never `sudo`, never create a system group from a dev/agent session) would otherwise have a Unix socket nothing but a coincidental root shell could ever open at all — the account that started the daemon is the one identity a dev run can always vouch for by construction. Packaging's own production socket permissions are unaffected.

**Setup-gate exemption (#276, maintainer decision 2026-09-29):** while no admin exists the setup gate refuses every operation but `/setup/status` and `/setup/admin`, except that on the **Unix socket only** it also passes `importConfig` and `previewConfigImport`, the entry of the bare-metal restore from the root shell of a box nobody has set up. The TCP listener keeps refusing both with `setup_required` until an admin exists, so an unauthenticated LAN client can never restore an archive onto an unclaimed box; the socket's peer is credential-checked, as above.

**Divergence (#340): the ups control socket (`/run/hoserva/ups-control.sock`) does not use this default.** upsmon's own unprivileged child (Debian's `RUN_AS_USER` default, `nut`) needs it, but this default's own "membership is root-equivalent" is exactly why `nut` must never join `hoserva` — a parsing bug in that child's network upsd client, reachable from a remote NUT server, would otherwise translate into `hoserva.sock` admission (disk-formatting API access), not just this one socket. So this one socket instead names the `nut` group directly (`root:nut 0660`; `nut` is always a member of its own group, no packaging or admin step needed), and `authorizeUnixPeer` checks each socket's peer against the group *that socket* names — `hoservaGroup` for `hoserva.sock`, `nut` for the ups control socket — never the same group for both. `applySocketGroupPermissions` (`cmd/hoservad/main.go`) applies this at daemon start for both sockets, and `UPSService.Update` (`internal/api/upsservice.go`) re-applies the ups control socket's `nut` group on every settings-ups save, covering `nut` being installed after the daemon already started (Recommends:, not Depends:) without a timer walking anything. `internal/config.Generator.CanWriteUPS`/`WriteUPS` refuse before any row or file changes if the `nut` group does not exist on the host at all.

### Q45 — How development agents run the storage lab on a dev machine
**Status:** Default — **validated on the primary dev host and confirmed on GitHub-hosted `ubuntu-24.04` runners** (S9, doc 08 §9) and **SnapRAID's own behaviour on the loop-device lab confirmed, with one named structural gap** (S5, doc 08) · **Gate:** Phase 1 (foundation) · **Affects:** doc 06 §3, §6, doc 12 §5, `CLAUDE.md`

**Default: the loop-device lab (L2) runs in a Docker container started only through `make lab-up`, with *narrowed* device access instead of `--privileged`: `CAP_SYS_ADMIN`, `--device /dev/fuse`, `--device /dev/loop-control`, and `--device-cgroup-rule 'b 7:* rmw'` (loop block devices only, major 7). There is no `/dev` bind mount, and loop nodes are `mknod`-ed inside the container. Agents never run `losetup`, `mkfs`, `mount` or `wipefs` on the host itself; to confirm their own teardown left nothing attached, they run `find /sys/devices/virtual/block -maxdepth 3 -path '*/loop/backing_file' -exec cat {} +` instead — the real path, not the `/sys/block` symlinks plain `find` won't follow — which prints nothing and exits 0 when clean (a shell glob over the same path errors when nothing matches, and `-L` over `/sys/block` instead exits non-zero even when clean because of its own self-referential `subsystem` symlink, so neither is used for this). Every lab is namespaced by `HOSERVA_LAB_ID` (image directory, mount root, container name), so parallel agents never share loop devices or mount points. L3 uses libvirt VMs.**
doc 06 §3's `privileged: true` plus `/dev:/dev` gives the container every host block device: one mistyped path formats the developer's real NVMe. Allowing only loop devices and FUSE makes that mistake impossible rather than merely unlikely. doc 06 §3's fixed `LAB=/tmp/hoserva-lab` path would also collide the moment the orchestrate skill runs two lanes at once. S9 confirmed the recipe on the dev host: loop devices, XFS and mergerfs work, and opening the host NVMe's device node fails with *Operation not permitted*. It also showed teardown must delete `.lab/<id>` from inside the container, since everything the lab creates is root-owned (doc 08). S5 confirmed SnapRAID itself behaves as documented on this lab for sync, diff, scrub, fix, undeleting, touch, and single- and dual-parity whole-disk reconstruction — with one structural gap: the lab's lack of a running `udevd` means SnapRAID can never read a data disk's UUID, so it always misclassifies an intra-disk move as remove+copy and never exercises the `-U`/`--force-uuid` disk-identity guard (doc 06 §6, doc 08 §5) — a real, if narrow, limit on this recipe's fidelity, covered by L3 rather than by anything a lab change could fix. **The recipe is now confirmed on hosted runners, not only the dev host: run `35076920766`'s `lab` job (`104731428284`, issue #10) ran `lab-up`, `lab-seed`, `lab-verify-refusal` (both host-device refusals returning EPERM) and a SnapRAID sync (`Everything OK`), then `lab-destroy` — all green on the pinned `ubuntu-24.04` (run `35057453620` showed the same earlier).** On that runner image, `apparmor=unconfined` is a required part of the recipe, not an incidental setting — confirmed by an A-B-A hosted comparison of the real recipe's own `security_opt`, not assumed (doc 08 §9; not restated here). The libvirt-lab-VM fallback this entry named is accordingly moot for hosted runners, since it was contingent on "if the recipe doesn't hold somewhere," and on GitHub-hosted `ubuntu-24.04` it holds; nothing here says anything about other CI providers or other runner images, where the fallback would still apply if the recipe were ever tried and failed there.

**The isolation is one-directional, and the other direction needs its own guard.** The container cannot reach host disks, but a loop device is host-global, so the developer's own desktop sees every lab filesystem the moment it is created: `udisks2` mounts it read-write under `/run/media/<user>/` and raises a polkit dialog to do so. That is a concurrent second read-write mount of a filesystem the lab is using, it puts host IO into the counters the spikes measure, and repeated dialogs lock the developer's account via `pam_faillock` (issue #119). `scripts/devenv/99-hoserva-lab-loop.rules` sets `UDISKS_IGNORE=1` on loop devices and is installed once per dev machine; doc 06 §3 carries the instructions. On a headless dev host nothing is watching, which is why this went unnoticed until a desktop ran the lab.

### Q46 — Branching and review with agent-driven development
**Status:** Default · **Gate:** Phase 1 · **Affects:** doc 12 §5, §6

**Default: work is landed by the `orchestrate` skill as independently verified local commits on `main`. The maintainer reads and pushes; nothing agent-made is pushed automatically. CI runs on push. Pull requests are the path for external contributors. Commits touching `safety-critical` paths (threshold guard, mover/relocation delete path, migration import, schema migrations and data transforms — D16, `packaging/`, PCI/USB passthrough's VFIO/bootloader changes — doc 14 §3) are listed separately in every orchestrate report, for a line-by-line read before pushing.**
Doc 12 §6 prescribed "feature branches, squash-merged", and doc 12 §5 a "protected list of files requiring explicit human review". This default keeps both intents inside the issue-driven agent workflow, whose unit of review is the verified commit.

**Revised (maintainer, 2026-09-17), surfaced by #18's nine-attempt verification loop and the #120→#122→#128→#129 lab-teardown cascade (one commit in that chain bypassed `orchestrate` entirely; another was a skill bug that told agents to run `losetup` on the host):** local verification alone is not a sufficient gate for `main` — it shares the executor's environment and blind spots, and has already been bypassed once. `main` gets a second, independent gate: a `beta` working branch, promoted to `main` only through a pull request that GitHub's required status checks (the full `ci.yml` suite) must pass. `orchestrate` now lands on local `beta`, not `main`, and pushes there immediately after a PASS — except for `safety-critical` commits (unchanged: listed for the maintainer to read and push) and any commit whose issue picked up an open `blockedBy` during the same run. `main` stays the GitHub default branch, so `Fixes #n` still closes an issue only once it reaches `main` — that's for the maintainer's visibility only; agents key off `status:*` labels (`status:implemented`/`status:closed` both count as unblocking), never GitHub's open/closed state. External contributors fork the repo, branch from `beta`, and PR into `beta`; `main` is reachable only through the promotion PR. Full detail in doc 12 §6.

**Revised again (maintainer, 2026-09-18):** `safety-critical` no longer holds a commit back from `beta` either. `beta` is a working branch, not a release branch — the gate that actually protects `main` is the pull request's required status checks, not a manual pre-push read of something already independently verified. `orchestrate` now pushes every landed commit to `beta` immediately after its PASS, `safety-critical` ones included; a commit is held back from pushing only when its own issue picked up a fresh open `blockedBy` during the same run, because a new dependency — not a label — is what limits trust in the fix. `safety-critical` commits are still called out on their own in every orchestrate report, so the maintainer knows which ones deserve a closer read even though they already reached `origin/beta`.

**Revised again (maintainer, 2026-09-21, #229):** the working branch is renamed `beta` → `dev`. The model from the two revisions above is otherwise unchanged — a working branch that CI runs on every push to, promoted to `main` only through a pull request gated by required status checks — only the branch's name changes. The rename removes a naming collision: `beta` was both "the branch everything lands on first" and "a pre-release build" (`vX.Y.Z-beta.N` tags, GitHub pre-releases, and eventually the apt beta channel). The release channel names `stable`/`beta` are unaffected — `scripts/release/lib.sh`'s `hoserva_channel_from_tag()` derives them purely from the tag string, not from which branch the tag sits on; only `hoserva_verify_tag_ancestry()`'s `beta`-channel → ancestor-of-branch mapping changes, from `beta` to `dev`. `dev` was cut fresh from `main` (not by renaming/rebasing the old `beta`, which would have dragged its superseded history along) once #228 — the last promotion PR still targeting the old branch — merged. The old `origin/beta` branch is deleted only after `dev` is confirmed working end to end and the maintainer gives explicit go-ahead. Full detail in doc 12 §6.

**Revised (maintainer, 2026-09-30, #484):** the model extends to the catalog repository, `mdg-labs/hoserva-catalog` (Q39). It has a `dev` working branch and keeps `main` as its release-only GitHub default branch. `orchestrate` lands a verified commit for an issue with a `Lands in: mdg-labs/hoserva-catalog` line on the catalog's `dev` and pushes `origin dev`; `main` moves only via a `dev → main` pull request. The issue's `Fixes mdg-labs/hoserva#<n>` trailer therefore closes it on reaching the catalog's `main`, the same as here. Doc 12 §1, §6.

### Q78 — Recovering the admin account
**Status:** Default · **Gate:** Phase 1 · **Affects:** doc 01 §3, §7, doc 03 §7, Q44

**Default: `hoserva user reset-password <name>` and `hoserva user disable-totp <name>`, accepted only from root over the Unix socket — checked by the caller's peer credentials, so the `hoserva` group and TCP can't use them — each audit-logged and announced through every notification channel. There is no email or security-question reset.**
Anyone with a root shell already controls the box, so root is the right authority for recovery, and it adds no secret to lose. Announcing the reset means a recovery nobody asked for doesn't go unnoticed.

**Decided (maintainer, 2026-09-17), surfaced by #22's login rate limiting:** any LAN client can keep the admin account backed off indefinitely by feeding it wrong passwords (doc 01 §7) — accepted as a trade-off rather than tightened further, on condition that this same root-only path also clears that lockout, not only resets credentials: `hoserva user unlock <name>`, over the Unix socket, checked the same way and audit-logged the same way as the two commands above (#37). All three recovery commands gate on the caller's peer credential being **uid 0 specifically** — the Unix socket also accepts connections at `hoservad`'s own uid (Q44's implementation note), which is enough to use the daemon in development but not enough to authorize recovering another account.

### Q84 — Revoking sessions on a credential change
**Status:** Default · **Gate:** Phase 1 · **Affects:** doc 01 §7

**Default: changing an account's TOTP enrollment — and, later, its password — revokes every other session already issued to that account, leaving only the session that made the change.**
Surfaced by #22's review (doc 01 §7): sessions last 30 days without re-authentication, and today changing TOTP touches no other session, so a stolen cookie from before the change stays valid for the rest of its 30 days even after the credential it was issued under is gone. Revoking every other session closes that window at no cost to the user making the change — their own current session is unaffected. **Built in #137**: `ConfirmTOTP` activates the pending secret and revokes every other session of the account in one transaction, keeping the caller's session hash; a future password-change path will call the same store helper.

### Q79 — Where the long-running test suites run
**Status:** Default — **S9 is closed: its lab half (loop devices, FUSE, a SnapRAID sync), its KVM half (a QEMU guest boots with KVM acceleration on a standard `ubuntu-24.04` hosted runner, host-side confirmed via QMP), and its AppArmor-necessity half (`apparmor=unconfined` confirmed required for the lab container's own `mount(2)`, from an A-B-A hosted comparison, after six earlier voided attempts) are all CONFIRMED hosted** (run 35076920766, issue #10, doc 08 §9); **S10 (nested KVM, VM-in-VM) is a different question and remains entirely untouched, out of scope here** · **Gate:** Phase 1 · **Affects:** doc 06 §4, §7, doc 14 §8, Q42, D20

**Default: no self-hosted runners. L3, the migration suite, Playwright and the VM-management suite run nightly on GitHub-hosted runners wherever S9 (and S10, for nested KVM) confirm support. Whatever hosted runners can't run, agents run on the development host — in the lab and user-session VMs (D20) — as a required step before every release, recorded in the release checklist with the commit it ran against.**
A self-hosted runner is a machine the maintainer owns and exposes to CI, which D20 rules out. A mandatory pre-release agent run keeps every suite required without any new infrastructure. **S9 is now closed and a full pass: a single, non-nested QEMU guest boots with KVM acceleration on a hosted runner, L2 (loop devices, FUSE, a SnapRAID sync) runs hosted, and `apparmor=unconfined` is confirmed required for the lab container there too.** That opens the hosted path for L3 workloads that need exactly a single accelerated guest, but not for anything requiring nested virtualisation (a VM inside the L3 VM) — S10 is a distinct, still-open question, and until it resolves, any suite that needs nested KVM stays on the "agents run it on the dev host before every release" path. Which specific suites this unblocks for the hosted path is a follow-up scoping decision, not settled by this entry alone.

---

## Product

### Q47 — AI assistant phasing *(contradiction)*
**Status:** Default · **Gate:** 1.0 · **Affects:** doc 00 §4, doc 07 §1, doc 11 §8

**The contradiction:** doc 11 §8 says "Not v1" and also "Phase 3 or 4", but 1.0 ships at the end of Phase 4.

**Default: post-1.0.** Step 1 of doc 11 §8 (local model, docs-only chat) may land in Phase 4 only if everything else in Phase 4 is done. R7 (solo-maintainer burnout) and doc 07 §4's failure modes favour cutting it first.

### Q48 — Internationalisation *(gap)*
**Status:** Default · **Gate:** Phase 1 · **Affects:** doc 03 §8.1

**Default: English only for 1.0, but every UI string goes through an i18n message catalog from the first component. Community translations come post-1.0.** The "UI language" setting stays hidden until a second language exists.
Extracting strings later is a rewrite of every component. Doing it from day one costs almost nothing.

### Q49 — Telemetry *(gap)*
**Status:** Default · **Gate:** Phase 1 · **Affects:** doc 01 §7, doc 03 §8.6

**Default: none. The only outbound requests Hoserva makes on its own are its update check against its own release index (Q67), the catalog refresh (Q65) and the daily container update check (Q81); none sends anything beyond a plain HTTP request, and each automatic check can be disabled (the catalog host is then contacted only when the user presses *Check for updates*). Any future opt-in usage statistics require a new entry here.**
A home server that phones home by default undermines the trust an open project depends on.

### Q50 — Name clearance
**Status:** External · **Gate:** before public 1.0 announcement · **Affects:** doc 00 §6

**Default: search EUIPO/TMview and secure `.io`/`.com` if cheap, before the 1.0 announcement rather than before development.** The name is settled; the check exists so the announcement doesn't have to be reversed.

### Q86 — mergerfs never accepts `RENAME_NOREPLACE` through its FUSE mount
**Status:** Default · **Gate:** Before Phase 1 · **Affects:** doc 09 §2, `internal/cache/rename_linux.go`

**Confirmed (#243): not a lab-only gap, and not a version or mount-option gap — a genuine, permanent FUSE limitation.** mergerfs's FUSE-facing rename handler (`FUSE::rename`, `src/fuse_rename.cpp` upstream) takes no flags parameter at all; checked against mergerfs's current upstream `master` source, not just the lab's Debian 13 2.40.2 package (Q7), so a version bump changes nothing, and `mergerfs -h`'s full mount-option list has no rename-flags/rename2 option to enable. Reproduced directly in the lab with a `renameat2(..., RENAME_NOREPLACE)` probe: the same call succeeds against a branch's own XFS filesystem but fails against the mergerfs union mount above it. The failure mode is more specific than "always EINVAL", and this matters for anyone else touching this path: the kernel's own dentry-existence check still catches an *existing* target and returns `EEXIST` without ever asking mergerfs (a generic VFS-level check, independent of the filesystem's flag support) — so **conflict detection through mergerfs already worked correctly before this issue**. Only the non-conflicting case fails: once the kernel confirms no target exists, completing the rename itself needs flag support mergerfs's FUSE handler doesn't have, and the syscall fails outright with `EINVAL`, leaving both paths untouched (no partial rename, no data loss — a clean failure of what should have been a successful move). That is what the three named lab tests were hitting, since a mover run practically always has more non-conflicting moves than conflicting ones.

`renameNoReplace` (`internal/cache/rename_linux.go`) now falls back, on `EINVAL`, to `link(2)`-then-`unlink(2)`: `link(2)` is itself an atomic create-if-absent primitive, so it fails closed into `ResultConflict` for a target that exists (`renameNoReplaceFallback`, unit-tested directly) without ever opening the TOCTOU window a separate `Lstat`-then-`rename(2)` would — `oldpath` is only removed once `link(2)` has already claimed `newpath`. This is the same pattern Q28 already documents for a synced temp file published atomically without a replace-capable rename. copyMoveFile's tmp file and its destination are always same-directory siblings, so this `EINVAL` can only mean "the destination filesystem can't carry the flag through" here, never rename(2)'s unrelated "directory into its own subdirectory" `EINVAL` case — the fallback is safe to take unconditionally on `EINVAL` at this call site. Anyone adding a new call through a mergerfs mount that needs an atomic no-replace rename hits the same gap and needs the same fallback, not a different one.
