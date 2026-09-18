-- sqlite-migrate: checksum 376a6625bcb5c7c64306aa5ddd8a84a27d2364243c3066f492177d13860536ec

CREATE TABLE audit_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    actor TEXT NOT NULL,
    action TEXT NOT NULL,
    detail TEXT,
    at TEXT NOT NULL
) STRICT;

CREATE INDEX audit_log_at_idx ON audit_log (at);

CREATE TABLE spin_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    device TEXT NOT NULL,
    from_state TEXT NOT NULL CHECK (from_state IN ('active', 'standby')),
    to_state TEXT NOT NULL CHECK (to_state IN ('active', 'standby')),
    at TEXT NOT NULL
) STRICT;

CREATE INDEX spin_events_at_idx ON spin_events (at);

CREATE INDEX spin_events_device_idx ON spin_events (device);
