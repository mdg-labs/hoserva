-- sqlite-migrate: checksum 0ab736f75cdc360c86d22717a73ee2aca05f59618a101f6f5e81a953b8fd5b2f

CREATE TABLE file_metadata (
    generation INTEGER NOT NULL,
    disk_mountpoint TEXT NOT NULL,
    rel_path TEXT NOT NULL,
    is_dir INTEGER NOT NULL CHECK (is_dir IN (0, 1)),
    uid INTEGER NOT NULL CHECK (uid >= 0),
    gid INTEGER NOT NULL CHECK (gid >= 0),
    mode INTEGER NOT NULL CHECK (mode >= 0 AND mode <= 4095),
    PRIMARY KEY (generation, disk_mountpoint, rel_path)
) STRICT, WITHOUT ROWID;

CREATE TABLE file_metadata_current (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    generation INTEGER NOT NULL
) STRICT;
