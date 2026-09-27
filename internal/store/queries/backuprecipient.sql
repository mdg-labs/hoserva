-- sqlc input (#275, Q60): typed Go query code for the backup_recipient
-- table, generated into internal/store/db/ by `make gen`. Deliberately no
-- comment other than each "-- name:" line between queries below (see
-- jobs.sql for the same note about sqlc's SQLite engine).

-- name: GetBackupRecipient :one
SELECT public_recipient, wrapped_identity, check_value FROM backup_recipient WHERE id = 1;

-- name: SetBackupRecipient :exec
INSERT INTO backup_recipient (id, public_recipient, wrapped_identity, check_value, created_at) VALUES (1, ?, ?, ?, ?);
