-- sqlite-migrate: checksum 087eebc334ec26bb966b05e5ddfb63f6612a2024d62de63245139f0b92df4e81

CREATE TABLE container_image_history (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    container TEXT NOT NULL,
    image TEXT NOT NULL,
    previous_image_id TEXT NOT NULL,
    snapshot_archive TEXT NOT NULL,
    snapshot_destination TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    keep_until TEXT NOT NULL,
    reverted_at TEXT NOT NULL
) STRICT;

CREATE INDEX container_image_history_container ON container_image_history (container, id);

CREATE TABLE container_update_policy (
    container TEXT PRIMARY KEY,
    bulk_excluded INTEGER NOT NULL CHECK (bulk_excluded IN (0, 1))
) STRICT;

CREATE TABLE container_update_settings (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    image_keep_days INTEGER NOT NULL CHECK (image_keep_days >= 1 AND image_keep_days <= 365)
) STRICT;
