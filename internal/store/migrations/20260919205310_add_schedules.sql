-- sqlite-migrate: checksum aeab84207a0d351ea79fd93f90d931196ff43cfda5d473df5b1b60331c8abc5b

CREATE TABLE schedule_chain (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    start_time TEXT NOT NULL,
    weekly_scrub_day INTEGER NOT NULL CHECK (weekly_scrub_day BETWEEN 0 AND 6),
    mover_enabled INTEGER NOT NULL CHECK (mover_enabled IN (0, 1)),
    diff_guard_enabled INTEGER NOT NULL CHECK (diff_guard_enabled IN (0, 1)),
    sync_enabled INTEGER NOT NULL CHECK (sync_enabled IN (0, 1)),
    scrub_enabled INTEGER NOT NULL CHECK (scrub_enabled IN (0, 1)),
    config_backup_enabled INTEGER NOT NULL CHECK (config_backup_enabled IN (0, 1)),
    updated_at TEXT NOT NULL
) STRICT;

CREATE TABLE schedule_jobs (
    job_id TEXT PRIMARY KEY CHECK (job_id IN ('smart_self_test', 'appdata_backup', 'restore_drill', 'container_update_check')),
    enabled INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    frequency TEXT NOT NULL CHECK (frequency IN ('daily', 'weekly', 'monthly')),
    start_time TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;
