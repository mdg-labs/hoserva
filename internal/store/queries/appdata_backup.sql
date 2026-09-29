-- sqlc input (#61): the per-container appdata backup policy, generated
-- into internal/store/db/ by `make gen`.

-- name: ListAppdataBackupContainers :many
SELECT container, stop, included, updated_at
FROM appdata_backup_containers
ORDER BY container ASC;

-- name: UpsertAppdataBackupContainer :exec
INSERT INTO appdata_backup_containers (container, stop, included, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT (container) DO UPDATE SET
    stop = excluded.stop,
    included = excluded.included,
    updated_at = excluded.updated_at;
