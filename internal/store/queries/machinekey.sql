-- sqlc input (#22, Q60): typed Go query code for the machine_key_check
-- table, generated into internal/store/db/ by `make gen`. Deliberately no
-- comment other than each "-- name:" line between queries below (see
-- jobs.sql for the same note about sqlc's SQLite engine).

-- name: GetMachineKeyCheck :one
SELECT check_value FROM machine_key_check WHERE id = 1;

-- name: SetMachineKeyCheck :exec
INSERT INTO machine_key_check (id, check_value, created_at) VALUES (1, ?, ?);
