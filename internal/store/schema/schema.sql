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
