-- sqlc input (#110, Q60): typed Go query code for the spin_events table,
-- generated into internal/store/db/ by `make gen`. Deliberately no
-- comment other than each "-- name:" line between queries below (see
-- jobs.sql for the same note about sqlc's SQLite engine).

-- name: InsertSpinEvent :exec
INSERT INTO spin_events (device, from_state, to_state, at) VALUES (?, ?, ?, ?);

-- name: CountSpinEvents :one
SELECT COUNT(*) FROM spin_events;

-- name: PruneSpinEvents :exec
DELETE FROM spin_events WHERE at < ?;

-- name: ListSpinEvents :many
SELECT id, device, from_state, to_state, at
FROM spin_events
ORDER BY at DESC;

-- name: ListDailyWakeCounts :many
SELECT device, date(at) AS day, COUNT(*) AS count
FROM spin_events
WHERE from_state = 'standby' AND to_state = 'active'
GROUP BY device, date(at)
ORDER BY day DESC, device;
