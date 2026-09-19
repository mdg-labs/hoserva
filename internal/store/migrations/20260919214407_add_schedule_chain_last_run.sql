-- sqlite-migrate: checksum 4938b5d3363a1c43449e783eaf82f232a5e870e1d525b369d245f1d4aa707b50

CREATE TABLE "schedule_chain_sqlite_migrate_new" (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    start_time TEXT NOT NULL,
    weekly_scrub_day INTEGER NOT NULL CHECK (weekly_scrub_day BETWEEN 0 AND 6),
    mover_enabled INTEGER NOT NULL CHECK (mover_enabled IN (0, 1)),
    diff_guard_enabled INTEGER NOT NULL CHECK (diff_guard_enabled IN (0, 1)),
    sync_enabled INTEGER NOT NULL CHECK (sync_enabled IN (0, 1)),
    scrub_enabled INTEGER NOT NULL CHECK (scrub_enabled IN (0, 1)),
    config_backup_enabled INTEGER NOT NULL CHECK (config_backup_enabled IN (0, 1)),
    last_run_at TEXT,
    updated_at TEXT NOT NULL
) STRICT;

INSERT INTO "schedule_chain_sqlite_migrate_new" ("id", "start_time", "weekly_scrub_day", "mover_enabled", "diff_guard_enabled", "sync_enabled", "scrub_enabled", "config_backup_enabled", "updated_at")
SELECT "id", "start_time", "weekly_scrub_day", "mover_enabled", "diff_guard_enabled", "sync_enabled", "scrub_enabled", "config_backup_enabled", "updated_at"
FROM "schedule_chain";

DROP TABLE "schedule_chain";

ALTER TABLE "schedule_chain_sqlite_migrate_new" RENAME TO "schedule_chain";
