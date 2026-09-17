-- sqlite-migrate: checksum 4acb2fb411f067665faaab6e1de79b4a9614e48fd1b724466ee5d0a2b9df50a4

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
) STRICT;

CREATE INDEX jobs_class_idx ON jobs (class);

CREATE INDEX jobs_status_idx ON jobs ("status");

CREATE TABLE machine_key_check (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    check_value BLOB NOT NULL,
    created_at TEXT NOT NULL
) STRICT;

CREATE TABLE schema_info (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    installation_id TEXT NOT NULL,
    created_at TEXT NOT NULL
) STRICT;

CREATE TABLE sessions (
    token_hash TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users (id),
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
) STRICT;

CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);

CREATE INDEX sessions_user_id_idx ON sessions (user_id);

CREATE TABLE users (
    id TEXT PRIMARY KEY,
    username TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('admin', 'viewer')),
    totp_secret BLOB,
    totp_confirmed_at TEXT,
    totp_last_step INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    totp_pending_secret BLOB
) STRICT;

CREATE UNIQUE INDEX users_one_admin_idx ON users (role) WHERE role = 'admin';
