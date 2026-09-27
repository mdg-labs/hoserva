-- sqlite-migrate: checksum c8819d7eb5675207d828ccc4096623dac1e4a72b267a12d2a3bc3cb1536825de

CREATE TABLE backup_recipient (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    public_recipient TEXT NOT NULL,
    wrapped_identity BLOB NOT NULL,
    check_value BLOB NOT NULL,
    created_at TEXT NOT NULL
) STRICT;
