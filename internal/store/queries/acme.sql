-- sqlc input (#211, Q60): typed Go query code for the acme_config table,
-- generated into internal/store/db/ by `make gen`. Deliberately no
-- comment other than each "-- name:" line between queries below (see
-- jobs.sql for the same note about sqlc's SQLite engine mis-slicing raw
-- source around extra comment lines).

-- name: GetACMEConfig :one
SELECT id, domain, provider, provider_config, dns_secret, account_key, enabled, last_error, updated_at
FROM acme_config WHERE id = 1;

-- name: UpsertACMEConfig :exec
INSERT INTO acme_config (
    id, domain, provider, provider_config, dns_secret, account_key, enabled, last_error, updated_at
) VALUES (
    1, ?, ?, ?, ?, ?, ?, ?, ?
)
ON CONFLICT (id) DO UPDATE SET
    domain = excluded.domain,
    provider = excluded.provider,
    provider_config = excluded.provider_config,
    dns_secret = excluded.dns_secret,
    account_key = excluded.account_key,
    enabled = excluded.enabled,
    last_error = excluded.last_error,
    updated_at = excluded.updated_at;

-- name: SetACMEEnabled :execrows
UPDATE acme_config SET enabled = ?, last_error = ?, updated_at = ? WHERE id = 1;

-- name: SetACMELastError :exec
UPDATE acme_config SET last_error = ?, updated_at = ? WHERE id = 1;

-- name: SetACMEAccountKey :exec
UPDATE acme_config SET account_key = ?, updated_at = ? WHERE id = 1;

-- name: HasEncryptedACMESecrets :one
SELECT EXISTS(
    SELECT 1 FROM acme_config
    WHERE length(dns_secret) > 0 OR length(account_key) > 0
);

-- name: ListACMESecrets :many
SELECT 'dns_secret' AS col, dns_secret AS ciphertext FROM acme_config WHERE id = 1 AND length(dns_secret) > 0
UNION ALL
SELECT 'account_key' AS col, account_key AS ciphertext FROM acme_config WHERE id = 1 AND length(account_key) > 0;

-- name: DeleteACMEConfig :exec
DELETE FROM acme_config WHERE id = 1;
