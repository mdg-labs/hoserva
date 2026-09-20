-- sqlite-migrate: checksum e779fc5bb17511fe0d1f94857c009f307e35e7d85b5bdcb642d0b9c9a13ff457

CREATE TABLE external_disks (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    label TEXT NOT NULL CHECK (length(label) >= 1),
    device TEXT NOT NULL,
    filesystem TEXT NOT NULL,
    fs_uuid TEXT NOT NULL,
    wwn TEXT,
    serial TEXT,
    by_id_name TEXT,
    weak_identity INTEGER NOT NULL CHECK (weak_identity IN (0, 1)),
    mountpoint TEXT NOT NULL CHECK (mountpoint GLOB '/mnt/disks/*'),
    backup_destination INTEGER NOT NULL CHECK (backup_destination IN (0, 1)),
    UNIQUE (label),
    UNIQUE (device),
    UNIQUE (fs_uuid),
    UNIQUE (mountpoint)
) STRICT;

CREATE INDEX external_disks_device_idx ON external_disks (device);
