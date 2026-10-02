-- sqlc input (#278): the stacks table, generated into internal/store/db/ by
-- `make gen`. Doc comments live on the Go wrapper in internal/store/stack.go.

-- name: InsertStack :exec
INSERT INTO stacks (name, template_source, template_id, template_revision, compose, env, installed_at, manually_edited)
VALUES (?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetStack :one
SELECT name, template_source, template_id, template_revision, compose, env, installed_at, manually_edited
FROM stacks WHERE name = ?;

-- name: ListStacks :many
SELECT name, template_source, template_id, template_revision, compose, env, installed_at, manually_edited
FROM stacks ORDER BY name ASC;

-- name: UpdateStackCompose :execrows
UPDATE stacks SET compose = ?, manually_edited = ? WHERE name = ?;

-- name: UpdateStackEnv :execrows
UPDATE stacks SET env = ? WHERE name = ?;

-- name: DeleteStack :execrows
DELETE FROM stacks WHERE name = ?;
