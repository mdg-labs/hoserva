-- sqlc input (#63): the last restore drill's result, generated into
-- internal/store/db/ by `make gen`.

-- name: GetRestoreDrillResult :one
SELECT ran_at, passed, error, destinations
FROM restore_drill_result
WHERE id = 1;

-- name: UpsertRestoreDrillResult :exec
INSERT INTO restore_drill_result (id, ran_at, passed, error, destinations)
VALUES (1, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    ran_at = excluded.ran_at,
    passed = excluded.passed,
    error = excluded.error,
    destinations = excluded.destinations;
