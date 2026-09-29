-- sqlite-migrate: checksum d7283a480d0bb0f0a4550a518ff2ca09cc2c25ab1ec585a3804fe64f20ea8a77

CREATE TABLE restore_drill_result (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    ran_at TEXT NOT NULL,
    passed INTEGER NOT NULL CHECK (passed IN (0, 1)),
    error TEXT,
    destinations TEXT NOT NULL
) STRICT;
