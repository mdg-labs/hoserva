-- sqlite-migrate: checksum 82ae18c2a3b347049d1051afb4c2f3d6727d958fa32a896be34c834c86e72064

CREATE TABLE backup_destination_enabled (
    destination_id TEXT PRIMARY KEY REFERENCES backup_destinations (id) ON DELETE CASCADE,
    enabled_at TEXT NOT NULL
) STRICT;
