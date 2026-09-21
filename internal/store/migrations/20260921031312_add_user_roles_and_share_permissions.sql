-- sqlite-migrate: checksum 3c787b7f95e6cae75c2bec9a42bd53c3a3eb69ad48f85dbcb08abb90b9722425

CREATE TABLE share_group_permissions (
    share_name TEXT NOT NULL REFERENCES shares (name) ON DELETE CASCADE,
    group_id TEXT NOT NULL REFERENCES user_groups (id) ON DELETE CASCADE,
    access TEXT NOT NULL CHECK (access IN ('none', 'read-only', 'read-write')),
    PRIMARY KEY (share_name, group_id)
) STRICT;

CREATE INDEX share_group_permissions_group_id_idx ON share_group_permissions (group_id);

CREATE TABLE share_user_permissions (
    share_name TEXT NOT NULL REFERENCES shares (name) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    access TEXT NOT NULL CHECK (access IN ('none', 'read-only', 'read-write')),
    PRIMARY KEY (share_name, user_id)
) STRICT;

CREATE INDEX share_user_permissions_user_id_idx ON share_user_permissions (user_id);

CREATE TABLE user_group_members (
    group_id TEXT NOT NULL REFERENCES user_groups (id) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    PRIMARY KEY (group_id, user_id)
) STRICT;

CREATE INDEX user_group_members_user_id_idx ON user_group_members (user_id);

CREATE TABLE user_groups (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
) STRICT;

CREATE TABLE "users_sqlite_migrate_new" (
    id TEXT PRIMARY KEY,
    username TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('admin', 'viewer', 'share-only')),
    totp_secret BLOB,
    totp_confirmed_at TEXT,
    totp_last_step INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    totp_pending_secret BLOB
) STRICT;

INSERT INTO "users_sqlite_migrate_new" (rowid, "id", "username", "password_hash", "role", "totp_secret", "totp_confirmed_at", "totp_last_step", "created_at", "totp_pending_secret")
SELECT rowid, "id", "username", "password_hash", "role", "totp_secret", "totp_confirmed_at", "totp_last_step", "created_at", "totp_pending_secret"
FROM "users";

DROP TABLE "users";

ALTER TABLE "users_sqlite_migrate_new" RENAME TO "users";

CREATE UNIQUE INDEX users_one_admin_idx ON users (role) WHERE role = 'admin';
