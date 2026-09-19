-- sqlite-migrate: checksum b81cb68bc49ac0012debb8a99ae6d1e3f241934506f476fdc02e13a2517754b0

CREATE TABLE notify_alerts (
    id TEXT PRIMARY KEY,
    event_type TEXT NOT NULL,
    severity TEXT NOT NULL CHECK (severity IN ('info', 'warning', 'error', 'critical')),
    title TEXT NOT NULL,
    message TEXT NOT NULL,
    created_at TEXT NOT NULL,
    read_at TEXT
) STRICT;

CREATE INDEX notify_alerts_created_at_idx ON notify_alerts (created_at);

CREATE INDEX notify_alerts_read_at_idx ON notify_alerts (read_at);
