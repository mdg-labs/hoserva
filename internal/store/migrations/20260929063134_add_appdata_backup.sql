-- sqlite-migrate: checksum cb62b58e1a5b0adc0093fb27be4f7b40ec534a44d58f5d42da76c82fe043357d

ALTER TABLE schedule_jobs ADD COLUMN last_run_at text;

CREATE TABLE appdata_backup_containers (
    container TEXT PRIMARY KEY,
    stop INTEGER NOT NULL CHECK (stop IN (0, 1)),
    included INTEGER NOT NULL CHECK (included IN (0, 1)),
    updated_at TEXT NOT NULL
) STRICT;
