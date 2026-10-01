-- sqlc input (#284): container update history, the bulk-update opt-out and
-- the update settings, generated into internal/store/db/ by `make gen`. Doc
-- comments live on the Go wrapper in internal/store/image_history.go.

-- name: InsertContainerImageHistory :one
INSERT INTO container_image_history (
    container, image, previous_image_id, snapshot_archive, snapshot_destination,
    updated_at, keep_until, reverted_at
) VALUES (?, ?, ?, ?, ?, ?, ?, '')
RETURNING id;

-- name: GetLatestContainerImageHistory :one
SELECT id, container, image, previous_image_id, snapshot_archive, snapshot_destination,
       updated_at, keep_until, reverted_at
FROM container_image_history WHERE container = ? ORDER BY id DESC LIMIT 1;

-- name: ListContainerImageHistory :many
SELECT id, container, image, previous_image_id, snapshot_archive, snapshot_destination,
       updated_at, keep_until, reverted_at
FROM container_image_history ORDER BY id DESC;

-- name: MarkContainerImageHistoryReverted :execrows
UPDATE container_image_history SET reverted_at = ? WHERE id = ? AND reverted_at = '';

-- name: DeleteContainerImageHistory :exec
DELETE FROM container_image_history WHERE id = ?;

-- name: SetContainerUpdatePolicy :exec
INSERT INTO container_update_policy (container, bulk_excluded) VALUES (?, ?)
ON CONFLICT (container) DO UPDATE SET bulk_excluded = excluded.bulk_excluded;

-- name: ListBulkExcludedContainers :many
SELECT container FROM container_update_policy WHERE bulk_excluded = 1 ORDER BY container ASC;

-- name: GetContainerUpdateSettings :one
SELECT image_keep_days FROM container_update_settings WHERE id = 1;

-- name: SetContainerUpdateSettings :exec
INSERT INTO container_update_settings (id, image_keep_days) VALUES (1, ?)
ON CONFLICT (id) DO UPDATE SET image_keep_days = excluded.image_keep_days;
