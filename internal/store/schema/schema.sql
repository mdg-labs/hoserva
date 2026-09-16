-- The central schema (D16, doc 01 §4). This is the only hand-edited
-- description of the database — every other schema artifact is generated
-- from it: `make db-migration` diffs this file against the schema the
-- existing migrations produce and writes the next file under
-- internal/store/migrations/, and `make gen` runs sqlc against this file
-- to produce internal/store/db/. Nothing else declares a table.
--
-- schema_info is deliberately the only table this issue adds. Issue #18 is
-- the schema/migration/query/runner pipeline itself, not a feature: the
-- tables real features need — jobs (#19), users and sessions (#22), disks,
-- shares — belong to the issues that design them, not to a foundation issue
-- guessing their shape. schema_info gives that pipeline one genuine,
-- permanent row to exercise end to end instead of an invented placeholder:
-- every install has exactly one row here, written once at first startup,
-- read by anything that needs to identify "this installation" (diagnostics
-- bundles, doc 03 §9.4; update/rollback, doc 01 §3).
CREATE TABLE schema_info (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    installation_id TEXT NOT NULL,
    created_at TEXT NOT NULL
);

-- Jobs (#19, doc 01 §4): the persisted record behind every long-running
-- operation. "type" and "status" are quoted (Q60: sqldef's SQLite parser
-- rejects "status" unquoted, and rejects "type" unquoted too). class and
-- resumable/cancellable are derived, in Go, from type — never trusted from
-- a caller — so the CHECK constraints here are a second, database-level
-- guard against a row that names an exclusion class or resumability the
-- type doesn't actually have.
-- resource_ids is a JSON array of the disk/container/VM ids a scoped job's
-- mutual-exclusion check compares (doc 01 §4's "same disks" / "same
-- container" / "same VM" rows); NULL for job types the scheduler never
-- scopes. checkpoint is the opaque, resumable-job-type checkpoint blob
-- (Q29); NULL until the first checkpoint is saved, and only ever written
-- for a resumable type. There is no log-path column: job stdout/stderr
-- lives in compressed files under a log directory the daemon passes in
-- (Q74), named by job id, so nothing here duplicates that path.
CREATE TABLE jobs (
    id TEXT PRIMARY KEY,
    "type" TEXT NOT NULL,
    class TEXT NOT NULL CHECK (class IN ('parity', 'array_write', 'topology', 'service', 'vm')),
    "status" TEXT NOT NULL CHECK ("status" IN ('queued', 'running', 'interrupted', 'succeeded', 'failed', 'cancelled')),
    progress INTEGER,
    resumable INTEGER NOT NULL CHECK (resumable IN (0, 1)),
    cancellable INTEGER NOT NULL CHECK (cancellable IN (0, 1)),
    resource_ids TEXT,
    checkpoint BLOB,
    error_code TEXT,
    error_message TEXT,
    created_at TEXT NOT NULL,
    started_at TEXT,
    finished_at TEXT
);

CREATE INDEX jobs_status_idx ON jobs ("status");
CREATE INDEX jobs_class_idx ON jobs (class);
