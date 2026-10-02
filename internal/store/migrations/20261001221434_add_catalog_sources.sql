-- sqlite-migrate: checksum 59f3a957c4ce161b76eeffb8e07938818dd6d130be61c088a61f5f7db796ba8b

CREATE TABLE catalog_sources (
    id TEXT PRIMARY KEY,
    url TEXT NOT NULL UNIQUE,
    kind TEXT NOT NULL CHECK (kind IN ('curated', 'user_added')),
    public_key TEXT NOT NULL,
    signature_verified INTEGER NOT NULL CHECK (signature_verified IN (0, 1)),
    last_refreshed_at TEXT NOT NULL,
    added_at TEXT NOT NULL,
    CHECK (kind = 'curated' OR signature_verified = 0 OR public_key != '')
) STRICT;
