-- sqlc input (#488): the credentials the container update check logs in to a
-- registry with, generated into internal/store/db/ by `make gen`. Doc
-- comments live on the Go wrapper in internal/store/registry_credentials.go.

-- name: UpsertRegistryCredential :exec
INSERT INTO registry_credentials (registry, credential, updated_at)
VALUES (?, ?, ?)
ON CONFLICT (registry) DO UPDATE SET
    credential = excluded.credential,
    updated_at = excluded.updated_at;

-- name: GetRegistryCredential :one
SELECT registry, credential, updated_at FROM registry_credentials WHERE registry = ?;

-- name: ListRegistryCredentials :many
SELECT registry, credential, updated_at FROM registry_credentials ORDER BY registry ASC;

-- name: DeleteRegistryCredential :execrows
DELETE FROM registry_credentials WHERE registry = ?;
