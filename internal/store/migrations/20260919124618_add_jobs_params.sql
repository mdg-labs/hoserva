-- sqlite-migrate: checksum 7c17764f9d134dbef9409d9036dfb93f1027f8c917ea660031a3c3dfff14e9d6

CREATE TABLE "jobs_sqlite_migrate_new" (
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
    finished_at TEXT,
    params TEXT
) STRICT;

INSERT INTO "jobs_sqlite_migrate_new" (rowid, "id", "type", "class", "status", "progress", "resumable", "cancellable", "resource_ids", "checkpoint", "error_code", "error_message", "created_at", "started_at", "finished_at")
SELECT rowid, "id", "type", "class", "status", "progress", "resumable", "cancellable", "resource_ids", "checkpoint", "error_code", "error_message", "created_at", "started_at", "finished_at"
FROM "jobs";

DROP TABLE "jobs";

ALTER TABLE "jobs_sqlite_migrate_new" RENAME TO "jobs";

CREATE INDEX jobs_class_idx ON jobs (class);

CREATE INDEX jobs_status_idx ON jobs ("status");
