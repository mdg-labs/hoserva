-- sqlite-migrate: checksum 64fd02fba566e238c78c59e7a652d2626b07135d0e14d7f4e4799760a8ae3905

CREATE TABLE relocation_manifest (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    rel_path TEXT NOT NULL,
    size INTEGER NOT NULL CHECK (size >= 0),
    mtime TEXT NOT NULL,
    source_disk TEXT NOT NULL,
    target_disk TEXT NOT NULL
) STRICT;

CREATE TABLE relocation_removing_disks (
    mountpoint TEXT PRIMARY KEY
) STRICT;
