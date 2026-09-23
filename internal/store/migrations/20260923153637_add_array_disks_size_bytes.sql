-- sqlite-migrate: checksum 361fe04a72cd2911739ef716dde3e63eac9fb7a701b4ad006fba0c0c7185f6a0

CREATE TABLE "array_disks_sqlite_migrate_new" (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    role TEXT NOT NULL CHECK (role IN ('parity', 'data', 'cache')),
    role_index INTEGER NOT NULL CHECK (role_index >= 1),
    device TEXT NOT NULL,
    filesystem TEXT NOT NULL,
    fs_uuid TEXT NOT NULL,
    size_bytes INTEGER,
    wwn TEXT,
    serial TEXT,
    by_id_name TEXT,
    weak_identity INTEGER NOT NULL DEFAULT 0 CHECK (weak_identity IN (0, 1)),
    mountpoint TEXT NOT NULL,
    UNIQUE (role, role_index),
    UNIQUE (device),
    UNIQUE (fs_uuid),
    UNIQUE (mountpoint)
) STRICT;

INSERT INTO "array_disks_sqlite_migrate_new" ("id", "role", "role_index", "device", "filesystem", "fs_uuid", "wwn", "serial", "by_id_name", "weak_identity", "mountpoint")
SELECT "id", "role", "role_index", "device", "filesystem", "fs_uuid", "wwn", "serial", "by_id_name", "weak_identity", "mountpoint"
FROM "array_disks";

UPDATE sqlite_sequence SET seq = (SELECT MAX(seq) FROM sqlite_sequence WHERE name IN ('array_disks_sqlite_migrate_new', 'array_disks')) WHERE name = 'array_disks_sqlite_migrate_new';

INSERT INTO sqlite_sequence (name, seq)
SELECT 'array_disks_sqlite_migrate_new', seq FROM sqlite_sequence WHERE name = 'array_disks'
AND NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name = 'array_disks_sqlite_migrate_new');

DROP TABLE "array_disks";

ALTER TABLE "array_disks_sqlite_migrate_new" RENAME TO "array_disks";

CREATE INDEX array_disks_role_idx ON array_disks (role, role_index);
