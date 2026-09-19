-- sqlc input (#197): typed Go query code for schedule_chain and
-- schedule_jobs, generated into internal/store/db/ by `make gen`.

-- name: GetScheduleChain :one
SELECT
    id,
    start_time,
    weekly_scrub_day,
    mover_enabled,
    diff_guard_enabled,
    sync_enabled,
    scrub_enabled,
    config_backup_enabled,
    last_run_at,
    updated_at
FROM schedule_chain
WHERE id = 1;

-- name: UpsertScheduleChain :exec
INSERT INTO schedule_chain (
    id,
    start_time,
    weekly_scrub_day,
    mover_enabled,
    diff_guard_enabled,
    sync_enabled,
    scrub_enabled,
    config_backup_enabled,
    updated_at
) VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    start_time = excluded.start_time,
    weekly_scrub_day = excluded.weekly_scrub_day,
    mover_enabled = excluded.mover_enabled,
    diff_guard_enabled = excluded.diff_guard_enabled,
    sync_enabled = excluded.sync_enabled,
    scrub_enabled = excluded.scrub_enabled,
    config_backup_enabled = excluded.config_backup_enabled,
    updated_at = excluded.updated_at;

-- name: SetScheduleChainLastRun :exec
UPDATE schedule_chain SET last_run_at = ? WHERE id = 1;

-- name: ListScheduleJobs :many
SELECT job_id, enabled, frequency, start_time, updated_at
FROM schedule_jobs
ORDER BY job_id ASC;

-- name: GetScheduleJob :one
SELECT job_id, enabled, frequency, start_time, updated_at
FROM schedule_jobs
WHERE job_id = ?;

-- name: UpsertScheduleJob :exec
INSERT INTO schedule_jobs (job_id, enabled, frequency, start_time, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (job_id) DO UPDATE SET
    enabled = excluded.enabled,
    frequency = excluded.frequency,
    start_time = excluded.start_time,
    updated_at = excluded.updated_at;
