-- sqlite-migrate: checksum c486bb8c643f1147c64fb6424a686fe9b29eeac5846018a109d70e214733284f

CREATE TABLE registry_credentials (
    registry TEXT PRIMARY KEY,
    credential BLOB NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;
