-- sqlite-migrate: checksum ef9a42a417ef527cbb1d40760fafbe887068df03570796ca7421d34ff5a6b932

CREATE TABLE "users_sqlite_migrate_new" (
    id TEXT PRIMARY KEY,
    username TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('admin', 'viewer', 'share-only')),
    totp_secret BLOB,
    totp_confirmed_at TEXT,
    totp_last_step INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    totp_pending_secret BLOB,
    last_login_at TEXT,
    smb_credential_set_at TEXT
) STRICT;

INSERT INTO "users_sqlite_migrate_new" (rowid, "id", "username", "password_hash", "role", "totp_secret", "totp_confirmed_at", "totp_last_step", "created_at", "totp_pending_secret")
SELECT rowid, "id", "username", "password_hash", "role", "totp_secret", "totp_confirmed_at", "totp_last_step", "created_at", "totp_pending_secret"
FROM "users";

DROP TABLE "users";

ALTER TABLE "users_sqlite_migrate_new" RENAME TO "users";

CREATE UNIQUE INDEX users_one_admin_idx ON users (role) WHERE role = 'admin';
