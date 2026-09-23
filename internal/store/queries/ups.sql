-- sqlc input (#249, Q60): typed Go query code for the ups_config table,
-- generated into internal/store/db/ by `make gen`.

-- name: GetUPSConfig :one
SELECT id, connection, driver, port, monitor_password, network_host, network_port,
       network_ups_name, network_username, network_password, low_battery_percent,
       runtime_seconds, updated_at
FROM ups_config WHERE id = 1;

-- name: UpsertUPSConfig :exec
INSERT INTO ups_config (
    id, connection, driver, port, monitor_password, network_host, network_port,
    network_ups_name, network_username, network_password, low_battery_percent,
    runtime_seconds, updated_at
) VALUES (
    1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
)
ON CONFLICT (id) DO UPDATE SET
    connection = excluded.connection,
    driver = excluded.driver,
    port = excluded.port,
    monitor_password = excluded.monitor_password,
    network_host = excluded.network_host,
    network_port = excluded.network_port,
    network_ups_name = excluded.network_ups_name,
    network_username = excluded.network_username,
    network_password = excluded.network_password,
    low_battery_percent = excluded.low_battery_percent,
    runtime_seconds = excluded.runtime_seconds,
    updated_at = excluded.updated_at;

-- name: DeleteUPSConfig :exec
DELETE FROM ups_config WHERE id = 1;

-- name: HasEncryptedUPSSecrets :one
SELECT EXISTS(
    SELECT 1 FROM ups_config
    WHERE length(monitor_password) > 0 OR length(network_password) > 0
);

-- name: ListUPSSecrets :many
SELECT 'monitor_password' AS col, monitor_password AS ciphertext FROM ups_config WHERE id = 1 AND length(monitor_password) > 0
UNION ALL
SELECT 'network_password' AS col, network_password AS ciphertext FROM ups_config WHERE id = 1 AND length(network_password) > 0;
