-- sqlc input (#22, Q60): typed Go query code for the sessions table,
-- generated into internal/store/db/ by `make gen`. Deliberately no
-- comment other than each "-- name:" line between queries below (see
-- jobs.sql for the same note about sqlc's SQLite engine).

-- name: CreateSession :exec
INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES (?, ?, ?, ?);

-- name: GetSession :one
SELECT token_hash, user_id, created_at, expires_at FROM sessions WHERE token_hash = ?;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE token_hash = ?;

-- name: DeleteExpiredSessions :exec
DELETE FROM sessions WHERE expires_at < ?;
