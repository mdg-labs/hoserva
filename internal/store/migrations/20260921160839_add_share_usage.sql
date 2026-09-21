-- sqlite-migrate: checksum 1c2655ad2ea722fe1aa13d40a37c2114a915c0408f40305724c68d18463bfbaf

CREATE TABLE share_usage (
    share_name TEXT NOT NULL,
    disk_mountpoint TEXT NOT NULL,
    bytes INTEGER NOT NULL CHECK (bytes >= 0),
    PRIMARY KEY (share_name, disk_mountpoint)
) STRICT;

CREATE INDEX share_usage_share_idx ON share_usage (share_name);

CREATE TABLE share_usage_computed_at (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    computed_at TEXT NOT NULL
) STRICT;
