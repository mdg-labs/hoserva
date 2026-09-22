-- sqlc input (#194): typed Go query code for the relocation_manifest and
-- relocation_removing_disks tables, generated into internal/store/db/ by
-- `make gen`. Doc comments live on the hand-written Go wrapper in
-- internal/parity/relocation_store.go instead.

-- name: DeleteRelocationManifest :exec
DELETE FROM relocation_manifest;

-- name: InsertRelocationManifestEntry :exec
INSERT INTO relocation_manifest (rel_path, size, mtime, source_disk, target_disk)
VALUES (?, ?, ?, ?, ?);

-- name: ListRelocationManifest :many
SELECT rel_path, size, mtime, source_disk, target_disk
FROM relocation_manifest
ORDER BY id ASC;

-- name: DeleteRelocationRemovingDisks :exec
DELETE FROM relocation_removing_disks;

-- name: InsertRelocationRemovingDisk :exec
INSERT INTO relocation_removing_disks (mountpoint) VALUES (?);

-- name: ListRelocationRemovingDisks :many
SELECT mountpoint FROM relocation_removing_disks ORDER BY mountpoint ASC;
