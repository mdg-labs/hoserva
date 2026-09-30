-- sqlc input (#60, Q60): typed Go query code for the backup_destinations
-- table, generated into internal/store/db/ by `make gen`.

-- name: ListBackupDestinations :many
SELECT id, name, type, path, options, secrets, enabled, encrypt, retention_daily,
       retention_weekly, retention_monthly, last_successful_backup_at,
       stale_alerted_at, created_at
FROM backup_destinations ORDER BY created_at, id;

-- name: GetBackupDestination :one
SELECT id, name, type, path, options, secrets, enabled, encrypt, retention_daily,
       retention_weekly, retention_monthly, last_successful_backup_at,
       stale_alerted_at, created_at
FROM backup_destinations WHERE id = ?;

-- name: CreateBackupDestination :exec
INSERT INTO backup_destinations (
    id, name, type, path, options, secrets, enabled, encrypt, retention_daily,
    retention_weekly, retention_monthly, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: DeleteBackupDestination :execrows
DELETE FROM backup_destinations WHERE id = ?;

-- name: RecordBackupDestinationSuccess :execrows
UPDATE backup_destinations
SET last_successful_backup_at = ?, stale_alerted_at = NULL
WHERE id = ?;

-- name: MarkBackupDestinationStaleAlerted :execrows
UPDATE backup_destinations SET stale_alerted_at = ?
WHERE id = ? AND last_successful_backup_at IS sqlc.narg(observed_last_successful_backup_at);

-- name: RecordBackupDestinationReenabled :exec
INSERT INTO backup_destination_enabled (destination_id, enabled_at)
SELECT id, ? FROM backup_destinations WHERE id = ? AND enabled = 0
ON CONFLICT (destination_id) DO UPDATE SET enabled_at = excluded.enabled_at;

-- name: UpdateBackupDestination :execrows
UPDATE backup_destinations SET
    stale_alerted_at = CASE
        WHEN enabled = 0 AND CAST(sqlc.narg(enabled) AS INTEGER) = 1 THEN NULL
        ELSE stale_alerted_at
    END,
    enabled = COALESCE(CAST(sqlc.narg(enabled) AS INTEGER), enabled),
    retention_daily = COALESCE(CAST(sqlc.narg(retention_daily) AS INTEGER), retention_daily),
    retention_weekly = COALESCE(CAST(sqlc.narg(retention_weekly) AS INTEGER), retention_weekly),
    retention_monthly = COALESCE(CAST(sqlc.narg(retention_monthly) AS INTEGER), retention_monthly)
WHERE id = sqlc.arg(id);

-- name: ListBackupDestinationEnabledAt :many
SELECT destination_id, enabled_at FROM backup_destination_enabled;

-- name: GetBackupDestinationEnabledAt :one
SELECT enabled_at FROM backup_destination_enabled WHERE destination_id = ?;

-- name: DeleteBackupDestinationEnabledAt :exec
DELETE FROM backup_destination_enabled WHERE destination_id = ?;
