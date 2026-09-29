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
