-- sqlite-migrate: checksum 4a002264447d17694b44978a9cc19cc6705979c8911f62783b1655cfa4d481f6

CREATE TABLE ups_config (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    connection TEXT NOT NULL CHECK (connection IN ('usb', 'network')),
    driver TEXT NOT NULL,
    port TEXT NOT NULL,
    monitor_password BLOB NOT NULL,
    network_host TEXT NOT NULL,
    network_port INTEGER NOT NULL CHECK (network_port >= 0 AND network_port <= 65535),
    network_ups_name TEXT NOT NULL,
    network_username TEXT NOT NULL,
    network_password BLOB NOT NULL,
    low_battery_percent INTEGER NOT NULL CHECK (low_battery_percent >= 0 AND low_battery_percent <= 100),
    runtime_seconds INTEGER NOT NULL CHECK (runtime_seconds >= 0),
    updated_at TEXT NOT NULL
) STRICT;
