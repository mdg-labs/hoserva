-- sqlite-migrate: checksum 42882a5064ffe4bafd9cff0868752a1a9a4036f3c1601e4e4c1766612f037c8a

CREATE TABLE array_maintenance (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    maintenance INTEGER NOT NULL CHECK (maintenance IN (0, 1)),
    array_stopped INTEGER NOT NULL CHECK (array_stopped IN (0, 1)),
    updated_at TEXT NOT NULL
) STRICT;
