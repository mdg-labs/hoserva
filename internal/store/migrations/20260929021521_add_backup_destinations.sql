-- sqlite-migrate: checksum c58320d47395802900684cbe93589a80d4c5e6a40b4b3dde4df225245bc22ee8

CREATE TABLE backup_destinations (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE COLLATE NOCASE,
    type TEXT NOT NULL CHECK (type IN ('local', 'smb', 's3', 'sftp', 'webdav', 'rclone')),
    path TEXT NOT NULL,
    options TEXT NOT NULL,
    secrets BLOB NOT NULL,
    enabled INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    encrypt INTEGER NOT NULL CHECK (encrypt IN (0, 1)),
    retention_daily INTEGER NOT NULL CHECK (retention_daily >= 0),
    retention_weekly INTEGER NOT NULL CHECK (retention_weekly >= 0),
    retention_monthly INTEGER NOT NULL CHECK (retention_monthly >= 0),
    last_successful_backup_at TEXT,
    stale_alerted_at TEXT,
    created_at TEXT NOT NULL
) STRICT;
