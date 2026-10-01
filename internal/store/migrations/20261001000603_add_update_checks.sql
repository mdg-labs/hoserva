-- sqlite-migrate: checksum bc0ff9eb6dfe0dee8f1a234025dc1636d6ef1797ff9e8f97c2c1e36608728c1f

CREATE TABLE image_update_checks (
    image TEXT PRIMARY KEY,
    checked_at TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('up_to_date', 'update_available', 'skipped', 'failed', 'not_checked')),
    kind TEXT NOT NULL CHECK (kind IN ('', 'new_build', 'new_version')),
    available_tag TEXT NOT NULL,
    message TEXT NOT NULL
) STRICT;
