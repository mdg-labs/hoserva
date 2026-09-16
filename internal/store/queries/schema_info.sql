-- sqlc input (Q60): typed Go query code for schema.sql's tables, generated
-- into internal/store/db/ by `make gen`. Nothing hand-writes that package.

-- name: GetSchemaMeta :one
SELECT id, installation_id, created_at FROM schema_info WHERE id = 1;

-- name: InsertSchemaMeta :exec
INSERT INTO schema_info (id, installation_id, created_at) VALUES (1, ?, ?);
