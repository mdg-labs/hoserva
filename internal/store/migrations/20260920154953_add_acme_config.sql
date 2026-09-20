-- sqlite-migrate: checksum 24234bffbfaf21825076bb5db9b000752f08c19d3d3cf581274618281a41c144

CREATE TABLE acme_config (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    domain TEXT NOT NULL,
    provider TEXT NOT NULL CHECK (provider IN ('cloudflare', 'rfc2136')),
    provider_config TEXT NOT NULL,
    dns_secret BLOB NOT NULL,
    account_key BLOB NOT NULL,
    enabled INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    last_error TEXT,
    updated_at TEXT NOT NULL
) STRICT;
