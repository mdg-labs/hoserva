-- sqlc input (#180, Q60): typed Go query code for the array_settings and
-- array_disks tables, generated into internal/store/db/ by `make gen`.
-- Deliberately no comment other than each "-- name:" line between queries
-- below (see jobs.sql for the same note about sqlc's SQLite engine
-- mis-slicing raw source around extra comment lines). Doc comments live
-- on the hand-written Go wrapper in internal/store/array.go instead.

-- name: InsertArraySettings :exec
INSERT INTO array_settings (id, create_policy, min_free_space, created_at)
VALUES (1, ?, ?, ?);

-- name: GetArraySettings :one
SELECT id, create_policy, min_free_space, created_at
FROM array_settings WHERE id = 1;

-- name: CountArraySettings :one
SELECT COUNT(*) FROM array_settings;

-- name: InsertArrayDisk :exec
INSERT INTO array_disks (
    role, role_index, device, filesystem, fs_uuid, size_bytes,
    wwn, serial, by_id_name, weak_identity, mountpoint
) VALUES (
    ?, ?, ?, ?, ?, ?,
    ?, ?, ?, ?, ?
);

-- name: ListArrayDisks :many
SELECT
    id, role, role_index, device, filesystem, fs_uuid, size_bytes,
    wwn, serial, by_id_name, weak_identity, mountpoint, removal_state, removal_job_id
FROM array_disks
ORDER BY
    CASE role
        WHEN 'parity' THEN 1
        WHEN 'data' THEN 2
        WHEN 'cache' THEN 3
        ELSE 4
    END,
    role_index ASC;

-- name: GetArrayDataDiskByMountpoint :one
SELECT
    id, role, role_index, device, filesystem, fs_uuid, size_bytes,
    wwn, serial, by_id_name, weak_identity, mountpoint, removal_state, removal_job_id
FROM array_disks WHERE mountpoint = ? AND role = 'data';

-- name: ReplaceArrayDataDiskIdentity :execrows
UPDATE array_disks
SET device = ?, filesystem = ?, fs_uuid = ?, size_bytes = ?,
    wwn = ?, serial = ?, by_id_name = ?, weak_identity = ?
WHERE mountpoint = ? AND role = 'data';

-- name: ReplaceArrayDataDiskIdentityAbandoningRemoval :execrows
UPDATE array_disks
SET device = ?, filesystem = ?, fs_uuid = ?, size_bytes = ?,
    wwn = ?, serial = ?, by_id_name = ?, weak_identity = ?,
    removal_state = NULL, removal_job_id = NULL
WHERE mountpoint = ? AND role = 'data' AND removal_state IN ('evacuated', 'unpooled');

-- name: UpgradeArrayParityDiskSlot :execrows
UPDATE array_disks
SET device = ?, filesystem = ?, fs_uuid = ?, size_bytes = ?,
    wwn = ?, serial = ?, by_id_name = ?, weak_identity = ?,
    mountpoint = sqlc.arg(new_mountpoint)
WHERE mountpoint = sqlc.arg(old_mountpoint) AND role = 'parity';

-- name: GetRemovingArrayDisk :one
SELECT mountpoint, removal_state FROM array_disks WHERE removal_state IS NOT NULL LIMIT 1;

-- name: SetArrayDiskRemovalState :execrows
UPDATE array_disks SET removal_state = ?, removal_job_id = ? WHERE mountpoint = ? AND role = 'data';

-- name: ReleaseArrayDiskRemovalState :execrows
UPDATE array_disks SET removal_state = NULL, removal_job_id = NULL
WHERE mountpoint = ? AND role = 'data' AND removal_state = 'evacuating' AND removal_job_id = ?;

-- name: CancelArrayDiskRemovalState :execrows
UPDATE array_disks SET removal_state = NULL, removal_job_id = NULL
WHERE mountpoint = sqlc.arg(mountpoint) AND role = 'data'
    AND removal_state IN ('evacuating', 'evacuated')
    AND removal_state = sqlc.arg(expected_state)
    AND removal_job_id IS sqlc.arg(expected_job_id);

-- name: AdvanceArrayDiskRemovalState :execrows
UPDATE array_disks SET removal_state = sqlc.arg(to_state), removal_job_id = sqlc.arg(job_id)
WHERE mountpoint = sqlc.arg(mountpoint) AND role = 'data'
    AND removal_state IN (sqlc.arg(from_state), sqlc.arg(same_state));

-- name: DeleteUnlistedArrayDataDisk :execrows
DELETE FROM array_disks WHERE mountpoint = ? AND role = 'data' AND removal_state = 'unlisted';
