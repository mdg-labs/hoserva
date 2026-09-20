-- sqlite-migrate: checksum b864860dc517c323ec1ed32687e5d6d8511e1207b09f124696985bef395c9346

CREATE TABLE shares (
    name TEXT PRIMARY KEY,
    cache_mode TEXT NOT NULL CHECK (cache_mode IN ('cache-then-move', 'cache-only', 'array-only')),
    create_policy TEXT NOT NULL CHECK (create_policy IN ('mspmfs', 'mfs', 'lfs', 'ff')),
    smb_enabled INTEGER NOT NULL CHECK (smb_enabled IN (0, 1)),
    smb_guest INTEGER NOT NULL CHECK (smb_guest IN (0, 1)),
    smb_read_only INTEGER NOT NULL CHECK (smb_read_only IN (0, 1)),
    smb_browseable INTEGER NOT NULL CHECK (smb_browseable IN (0, 1)),
    smb_recycle INTEGER NOT NULL CHECK (smb_recycle IN (0, 1)),
    smb_time_machine INTEGER NOT NULL CHECK (smb_time_machine IN (0, 1)),
    smb_time_machine_max_size TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;
