-- sqlc input (#387): typed Go query code for array_maintenance, generated
-- into internal/store/db/ by `make gen`.

-- name: GetArrayMaintenance :one
SELECT id, maintenance, array_stopped, updated_at
FROM array_maintenance
WHERE id = 1;

-- name: UpsertArrayMaintenance :exec
INSERT INTO array_maintenance (id, maintenance, array_stopped, updated_at)
VALUES (1, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    maintenance = excluded.maintenance,
    array_stopped = excluded.array_stopped,
    updated_at = excluded.updated_at;
