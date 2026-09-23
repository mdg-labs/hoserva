-- sqlite-migrate: checksum de19324bab6f44e71867f0ae1f4824be3bc54188e3146d46b3cb574eb7a7d4a4

CREATE TABLE cache_usage_breakdown (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    appdata_bytes INTEGER NOT NULL CHECK (appdata_bytes >= 0),
    pending_moves_bytes INTEGER NOT NULL CHECK (pending_moves_bytes >= 0),
    other_bytes INTEGER NOT NULL CHECK (other_bytes >= 0),
    computed_at TEXT NOT NULL
) STRICT;

CREATE TABLE mover_run_result (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    started_at TEXT NOT NULL,
    finished_at TEXT NOT NULL,
    duration_ms INTEGER NOT NULL CHECK (duration_ms >= 0),
    files_moved INTEGER NOT NULL CHECK (files_moved >= 0),
    bytes_moved INTEGER NOT NULL CHECK (bytes_moved >= 0),
    interrupted INTEGER NOT NULL CHECK (interrupted IN (0, 1)),
    skipped_json TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;
