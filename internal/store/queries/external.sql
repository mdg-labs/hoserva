-- sqlc input (#115, Q72): typed Go query code for the external_disks
-- table, generated into internal/store/db/ by `make gen`. Deliberately no
-- comment other than each "-- name:" line between queries below (see
-- jobs.sql). Doc comments live on the hand-written Go wrapper in
-- internal/store/external.go instead.

-- name: InsertExternalDisk :exec
INSERT INTO external_disks (
    label, device, filesystem, fs_uuid,
    wwn, serial, by_id_name, weak_identity, mountpoint, backup_destination
) VALUES (
    ?, ?, ?, ?,
    ?, ?, ?, ?, ?, ?
);

-- name: GetExternalDiskByLabel :one
SELECT
    id, label, device, filesystem, fs_uuid,
    wwn, serial, by_id_name, weak_identity, mountpoint, backup_destination
FROM external_disks WHERE label = ?;

-- name: GetExternalDiskByDevice :one
SELECT
    id, label, device, filesystem, fs_uuid,
    wwn, serial, by_id_name, weak_identity, mountpoint, backup_destination
FROM external_disks WHERE device = ?;

-- name: ListExternalDisks :many
SELECT
    id, label, device, filesystem, fs_uuid,
    wwn, serial, by_id_name, weak_identity, mountpoint, backup_destination
FROM external_disks
ORDER BY label ASC;

-- name: UpdateExternalDisk :execrows
UPDATE external_disks
SET device = ?, filesystem = ?, fs_uuid = ?,
    wwn = ?, serial = ?, by_id_name = ?, weak_identity = ?,
    mountpoint = ?, backup_destination = ?
WHERE label = ?;

-- name: SetExternalBackupDestination :execrows
UPDATE external_disks
SET backup_destination = ?
WHERE label = ?;

-- name: DeleteExternalDisk :execrows
DELETE FROM external_disks WHERE label = ?;
