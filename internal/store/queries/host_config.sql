-- sqlc input (Q76): typed Go query code for the host_config table,
-- generated into internal/store/db/ by `make gen`. Doc comments live on
-- the hand-written Go wrapper in internal/store/host.go instead.

-- name: UpsertHostConfig :exec
INSERT INTO host_config (kind, decision, facts, applied_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(kind) DO UPDATE SET
    decision = excluded.decision,
    facts = excluded.facts,
    applied_at = excluded.applied_at;

-- name: GetHostConfig :one
SELECT kind, decision, facts, applied_at
FROM host_config WHERE kind = ?;

-- name: ListHostConfig :many
SELECT kind, decision, facts, applied_at
FROM host_config
ORDER BY kind;
