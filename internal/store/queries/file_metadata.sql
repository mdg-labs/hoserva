-- sqlc input (#626): typed Go query code for the file_metadata and
-- file_metadata_current tables, generated into internal/store/db/ by
-- `make gen`. Doc comments live on the hand-written Go wrapper in
-- internal/parity/filemeta_store.go instead.

-- name: InsertFileMetadata :exec
INSERT INTO file_metadata (generation, disk_mountpoint, rel_path, is_dir, uid, gid, mode)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: GetFileMetadata :one
SELECT m.is_dir, m.uid, m.gid, m.mode
FROM file_metadata m
JOIN file_metadata_current c ON c.generation = m.generation
WHERE c.id = 1 AND m.disk_mountpoint = ? AND m.rel_path = ?;

-- name: GetFileMetadataGeneration :one
SELECT generation FROM file_metadata_current WHERE id = 1;

-- name: MaxFileMetadataGeneration :one
SELECT CAST(COALESCE(MAX(generation), 0) AS INTEGER) FROM file_metadata;

-- name: SetFileMetadataGeneration :exec
INSERT INTO file_metadata_current (id, generation) VALUES (1, ?)
ON CONFLICT (id) DO UPDATE SET generation = excluded.generation;

-- name: DeleteFileMetadataOtherGenerations :execrows
DELETE FROM file_metadata
WHERE (file_metadata.generation, file_metadata.disk_mountpoint, file_metadata.rel_path) IN (
    SELECT s.generation, s.disk_mountpoint, s.rel_path
    FROM file_metadata s
    WHERE s.generation < ?1 OR s.generation > ?1
    LIMIT ?2
);
