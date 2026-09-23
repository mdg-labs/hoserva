-- sqlc input (#19, Q60): typed Go query code for the jobs table, generated
-- into internal/store/db/ by `make gen`. Nothing hand-writes that package.
-- Deliberately no comment other than each "-- name:" line between queries
-- below: sqlc v1.31.1's SQLite engine mis-slices the raw source once any
-- extra comment line appears between two queries, silently truncating the
-- generated SQL text of every query that follows (confirmed while writing
-- this file). ListJobs, ListActiveJobs and InterruptActiveJobs carry doc
-- comments on their Go methods instead, in jobs.go, immediately above
-- each hand-written wrapper that calls them.

-- name: CreateJob :exec
INSERT INTO jobs (
    id, "type", class, "status", progress, resumable, cancellable,
    resource_ids, checkpoint, error_code, error_message,
    created_at, started_at, finished_at, params
) VALUES (
    ?, ?, ?, ?, ?, ?, ?,
    ?, ?, ?, ?,
    ?, ?, ?, ?
);

-- name: GetJob :one
SELECT
    id, "type", class, "status", progress, resumable, cancellable,
    resource_ids, checkpoint, error_code, error_message,
    created_at, started_at, finished_at, params
FROM jobs WHERE id = ?;

-- name: ListJobs :many
SELECT
    id, "type", class, "status", progress, resumable, cancellable,
    resource_ids, checkpoint, error_code, error_message,
    created_at, started_at, finished_at, params
FROM jobs
WHERE (sqlc.narg('class') IS NULL OR class = sqlc.narg('class'))
  AND (sqlc.narg('status') IS NULL OR "status" = sqlc.narg('status'))
ORDER BY created_at DESC
LIMIT sqlc.arg('row_limit');

-- name: ListActiveJobs :many
SELECT
    id, "type", class, "status", progress, resumable, cancellable,
    resource_ids, checkpoint, error_code, error_message,
    created_at, started_at, finished_at, params
FROM jobs WHERE "status" IN ('queued', 'running') ORDER BY created_at ASC;

-- name: UpdateJobStatus :exec
UPDATE jobs
SET "status" = ?, progress = ?, error_code = ?, error_message = ?,
    started_at = ?, finished_at = ?
WHERE id = ?;

-- name: UpdateJobProgress :exec
UPDATE jobs SET progress = ? WHERE id = ?;

-- name: SaveJobCheckpoint :exec
UPDATE jobs SET checkpoint = ? WHERE id = ?;

-- name: InterruptActiveJobs :exec
UPDATE jobs SET "status" = 'interrupted', finished_at = ?
WHERE "status" IN ('queued', 'running');

-- name: ListPendingJobsOfType :many
SELECT
    id, "type", class, "status", progress, resumable, cancellable,
    resource_ids, checkpoint, error_code, error_message,
    created_at, started_at, finished_at, params
FROM jobs
WHERE "type" = ? AND "status" IN ('queued', 'running', 'interrupted')
ORDER BY created_at ASC;

-- name: SetJobCancellable :exec
UPDATE jobs SET cancellable = ? WHERE id = ?;
