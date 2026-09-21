-- sqlc input (#50, Q43, Q60): typed Go query code for the api_tokens
-- table, generated into internal/store/db/ by `make gen`.

-- name: CreateApiToken :exec
INSERT INTO api_tokens (token_hash, user_id, name, role, created_at) VALUES (?, ?, ?, ?, ?);

-- name: GetApiTokenByHash :one
SELECT token_hash, user_id, name, role, created_at FROM api_tokens WHERE token_hash = ?;

-- name: DeleteApiToken :execrows
DELETE FROM api_tokens WHERE token_hash = ?;

-- name: DeleteUserApiTokens :exec
DELETE FROM api_tokens WHERE user_id = ?;
