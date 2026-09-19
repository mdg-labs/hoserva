-- sqlite-migrate: checksum 10dde882ba7efd97c8a227080e6fed78d2113c490016fbf4cf894595105a0597

CREATE TABLE notify_channels (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    "type" TEXT NOT NULL CHECK ("type" IN ('email', 'gotify', 'ntfy', 'discord', 'webhook')),
    enabled INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    config TEXT NOT NULL,
    secret BLOB,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

CREATE TABLE notify_deliveries (
    id TEXT PRIMARY KEY,
    channel_id TEXT NOT NULL REFERENCES notify_channels (id) ON DELETE CASCADE,
    event_type TEXT NOT NULL,
    severity TEXT NOT NULL CHECK (severity IN ('info', 'warning', 'error', 'critical')),
    title TEXT NOT NULL,
    message TEXT NOT NULL,
    "status" TEXT NOT NULL CHECK ("status" IN ('pending', 'delivered', 'failed', 'suppressed')),
    attempts INTEGER NOT NULL,
    last_error TEXT,
    created_at TEXT NOT NULL,
    next_attempt_at TEXT NOT NULL,
    delivered_at TEXT
) STRICT;

CREATE INDEX notify_deliveries_channel_id_idx ON notify_deliveries (channel_id);

CREATE INDEX notify_deliveries_status_idx ON notify_deliveries ("status", next_attempt_at);

CREATE TABLE notify_event_severity (
    event_type TEXT PRIMARY KEY,
    severity TEXT NOT NULL CHECK (severity IN ('info', 'warning', 'error', 'critical'))
) STRICT;

CREATE TABLE notify_quiet_hours (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    enabled INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    start_time TEXT NOT NULL,
    end_time TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

CREATE TABLE notify_routes (
    event_type TEXT NOT NULL,
    channel_id TEXT NOT NULL REFERENCES notify_channels (id) ON DELETE CASCADE,
    PRIMARY KEY (event_type, channel_id)
) STRICT;

CREATE INDEX notify_routes_channel_id_idx ON notify_routes (channel_id);
