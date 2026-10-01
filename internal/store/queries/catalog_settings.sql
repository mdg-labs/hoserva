-- name: GetCatalogSettings :one
SELECT catalog_refresh_interval, catalog_check_on_open FROM schema_info WHERE id = 1;

-- name: UpdateCatalogSettings :exec
UPDATE schema_info
SET catalog_refresh_interval = COALESCE(sqlc.narg('refresh_interval'), catalog_refresh_interval),
    catalog_check_on_open = COALESCE(sqlc.narg('check_on_open'), catalog_check_on_open)
WHERE id = 1;

-- name: EnsureSchemaMeta :exec
INSERT INTO schema_info (id, installation_id, created_at) VALUES (1, ?, ?)
ON CONFLICT (id) DO NOTHING;
