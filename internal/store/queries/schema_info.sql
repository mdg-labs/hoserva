-- sqlc input (Q60): typed Go query code for schema.sql's tables, generated
-- into internal/store/db/ by `make gen`. Nothing hand-writes that package.

-- name: GetSchemaMeta :one
SELECT id, installation_id, created_at, hostname, timezone, backup_passphrase, update_channel, update_check_enabled, previous_version
FROM schema_info WHERE id = 1;

-- name: InsertSchemaMeta :exec
INSERT INTO schema_info (id, installation_id, created_at) VALUES (1, ?, ?);

-- name: UpdateGeneralSettings :exec
UPDATE schema_info
SET hostname = ?, timezone = ?, backup_passphrase = ?
WHERE id = 1;

-- name: UpdateUpdateSettings :exec
UPDATE schema_info
SET update_channel = ?, update_check_enabled = ?
WHERE id = 1;

-- name: UpdatePreviousVersion :exec
UPDATE schema_info
SET previous_version = ?
WHERE id = 1;

-- name: HasEncryptedBackupPassphrase :one
SELECT EXISTS(
    SELECT 1 FROM schema_info WHERE id = 1 AND backup_passphrase IS NOT NULL AND length(backup_passphrase) > 0
) AS has_encrypted_backup_passphrase;
