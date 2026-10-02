-- sqlc input (#283): the catalog_sources table, generated into
-- internal/store/db/ by `make gen`. Doc comments live on the Go wrapper in
-- internal/store/catalog_sources.go.

-- name: InsertCatalogSource :exec
INSERT INTO catalog_sources (id, url, kind, public_key, signature_verified, last_refreshed_at, added_at)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: EnsureCuratedCatalogSource :exec
INSERT INTO catalog_sources (id, url, kind, public_key, signature_verified, last_refreshed_at, added_at)
VALUES (?, ?, 'curated', '', 1, '', ?)
ON CONFLICT (id) DO NOTHING;

-- name: GetCatalogSource :one
SELECT id, url, kind, public_key, signature_verified, last_refreshed_at, added_at
FROM catalog_sources WHERE id = ?;

-- name: ListCatalogSources :many
SELECT id, url, kind, public_key, signature_verified, last_refreshed_at, added_at
FROM catalog_sources
ORDER BY CASE kind WHEN 'curated' THEN 0 ELSE 1 END ASC, added_at ASC, rowid ASC;

-- name: RecordCatalogSourceRefresh :execrows
UPDATE catalog_sources SET last_refreshed_at = ?, signature_verified = ? WHERE id = ?;

-- name: DeleteUserAddedCatalogSource :execrows
DELETE FROM catalog_sources WHERE id = ? AND kind = 'user_added';
