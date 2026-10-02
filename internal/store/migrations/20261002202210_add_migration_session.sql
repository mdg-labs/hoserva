-- sqlite-migrate: checksum e325c69a61eb1d55d223cd4e8fee50709a48fba5e5aa8b19ca7f45f60fb6eeb2

CREATE TABLE migration_session (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    source_file TEXT NOT NULL DEFAULT '',
    source_size INTEGER NOT NULL DEFAULT 0,
    source_received_at TEXT NOT NULL DEFAULT '',
    report TEXT NOT NULL DEFAULT '',
    scan_file TEXT NOT NULL DEFAULT '',
    scan_size INTEGER NOT NULL DEFAULT 0,
    scan_received_at TEXT NOT NULL DEFAULT '',
    scan_unverified_layout INTEGER NOT NULL DEFAULT 0 CHECK (scan_unverified_layout IN (0, 1)),
    scan_error TEXT NOT NULL DEFAULT ''
) STRICT;
