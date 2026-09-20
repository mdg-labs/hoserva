-- sqlc input (#46): typed Go query code for the shares table, generated
-- into internal/store/db/ by `make gen`. Deliberately no comment other
-- than each "-- name:" line between queries below (see jobs.sql). Doc
-- comments live on the hand-written Go wrapper in internal/store/share.go.

-- name: InsertShare :exec
INSERT INTO shares (
    name, cache_mode, create_policy,
    smb_enabled, smb_guest, smb_read_only, smb_browseable,
    smb_recycle, smb_time_machine, smb_time_machine_max_size,
    nfs_enabled, nfs_hosts, nfs_squash,
    created_at, updated_at
) VALUES (
    ?, ?, ?,
    ?, ?, ?, ?,
    ?, ?, ?,
    ?, ?, ?,
    ?, ?
);

-- name: GetShare :one
SELECT
    name, cache_mode, create_policy,
    smb_enabled, smb_guest, smb_read_only, smb_browseable,
    smb_recycle, smb_time_machine, smb_time_machine_max_size,
    nfs_enabled, nfs_hosts, nfs_squash,
    created_at, updated_at
FROM shares WHERE name = ?;

-- name: ListShares :many
SELECT
    name, cache_mode, create_policy,
    smb_enabled, smb_guest, smb_read_only, smb_browseable,
    smb_recycle, smb_time_machine, smb_time_machine_max_size,
    nfs_enabled, nfs_hosts, nfs_squash,
    created_at, updated_at
FROM shares
ORDER BY name ASC;

-- name: UpdateShare :execrows
UPDATE shares
SET cache_mode = ?, create_policy = ?,
    smb_enabled = ?, smb_guest = ?, smb_read_only = ?, smb_browseable = ?,
    smb_recycle = ?, smb_time_machine = ?, smb_time_machine_max_size = ?,
    nfs_enabled = ?, nfs_hosts = ?, nfs_squash = ?,
    updated_at = ?
WHERE name = ?;

-- name: DeleteShare :execrows
DELETE FROM shares WHERE name = ?;
