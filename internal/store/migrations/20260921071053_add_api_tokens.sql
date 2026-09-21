-- sqlite-migrate: checksum e7a1b45818e290b8428d45bf01a089019ea42a204a75ecf40f9ae022386b3551

CREATE TABLE api_tokens (
    token_hash TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('admin', 'viewer')),
    created_at TEXT NOT NULL
) STRICT;

CREATE INDEX api_tokens_user_id_idx ON api_tokens (user_id);
