-- sqlite-migrate: checksum 84f4bea7d2062d15be2827eb0f2530bb21247fc7b8ab5df7ed66d8926dbeee00

CREATE TABLE "shares_sqlite_migrate_new" (
    name TEXT PRIMARY KEY,
    cache_mode TEXT NOT NULL CHECK (cache_mode IN ('cache-then-move', 'cache-only', 'array-only')),
    create_policy TEXT NOT NULL CHECK (create_policy IN ('mspmfs', 'mfs', 'lfs', 'ff')),
    smb_enabled INTEGER NOT NULL CHECK (smb_enabled IN (0, 1)),
    smb_guest INTEGER NOT NULL CHECK (smb_guest IN (0, 1)),
    smb_read_only INTEGER NOT NULL CHECK (smb_read_only IN (0, 1)),
    smb_browseable INTEGER NOT NULL CHECK (smb_browseable IN (0, 1)),
    smb_recycle INTEGER NOT NULL CHECK (smb_recycle IN (0, 1)),
    smb_time_machine INTEGER NOT NULL CHECK (smb_time_machine IN (0, 1)),
    smb_time_machine_max_size TEXT,
    nfs_enabled INTEGER NOT NULL DEFAULT 0 CHECK (nfs_enabled IN (0, 1)),
    nfs_hosts TEXT NOT NULL DEFAULT '[]',
    nfs_squash TEXT NOT NULL DEFAULT 'root_squash' CHECK (nfs_squash IN ('root_squash', 'no_root_squash', 'all_squash')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

INSERT INTO "shares_sqlite_migrate_new" (rowid, "name", "cache_mode", "create_policy", "smb_enabled", "smb_guest", "smb_read_only", "smb_browseable", "smb_recycle", "smb_time_machine", "smb_time_machine_max_size", "created_at", "updated_at")
SELECT rowid, "name", "cache_mode", "create_policy", "smb_enabled", "smb_guest", "smb_read_only", "smb_browseable", "smb_recycle", "smb_time_machine", "smb_time_machine_max_size", "created_at", "updated_at"
FROM "shares";

DROP TABLE "shares";

ALTER TABLE "shares_sqlite_migrate_new" RENAME TO "shares";
