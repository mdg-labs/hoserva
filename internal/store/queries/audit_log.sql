-- sqlc input (#110, Q60): typed Go query code for the audit_log table,
-- generated into internal/store/db/ by `make gen`. Deliberately no
-- comment other than each "-- name:" line between queries below (see
-- jobs.sql for the same note about sqlc's SQLite engine).

-- name: InsertAuditLogEntry :exec
INSERT INTO audit_log (actor, action, detail, at) VALUES (?, ?, ?, ?);

-- name: CountAuditLog :one
SELECT COUNT(*) FROM audit_log;

-- name: PruneAuditLog :exec
DELETE FROM audit_log WHERE at < ?;
