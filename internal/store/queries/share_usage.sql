-- sqlc input (#223): typed Go query code for the share_usage and
-- share_usage_computed_at tables, generated into internal/store/db/ by
-- `make gen`. Doc comments live on the hand-written Go wrapper in
-- internal/parity/usage_store.go instead.

-- name: DeleteShareUsage :exec
DELETE FROM share_usage;

-- name: InsertShareUsage :exec
INSERT INTO share_usage (share_name, disk_mountpoint, bytes)
VALUES (?, ?, ?);

-- name: ListShareUsageByShare :many
SELECT share_name, disk_mountpoint, bytes
FROM share_usage
WHERE share_name = ?
ORDER BY disk_mountpoint ASC;

-- name: ListAllShareUsage :many
SELECT share_name, disk_mountpoint, bytes
FROM share_usage
ORDER BY share_name ASC, disk_mountpoint ASC;

-- name: UpsertShareUsageComputedAt :exec
INSERT INTO share_usage_computed_at (id, computed_at) VALUES (1, ?)
ON CONFLICT (id) DO UPDATE SET computed_at = excluded.computed_at;

-- name: GetShareUsageComputedAt :one
SELECT computed_at FROM share_usage_computed_at WHERE id = 1;
