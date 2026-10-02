-- sqlc input (#75): the one Unraid migration session, generated into
-- internal/store/db/ by `make gen`.

-- name: GetMigrationSession :one
SELECT source_file, source_size, source_received_at, report,
       scan_file, scan_size, scan_received_at, scan_unverified_layout, scan_error
FROM migration_session
WHERE id = 1;

-- name: UpsertMigrationSession :exec
INSERT INTO migration_session (
    id, source_file, source_size, source_received_at, report,
    scan_file, scan_size, scan_received_at, scan_unverified_layout, scan_error
) VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    source_file = excluded.source_file,
    source_size = excluded.source_size,
    source_received_at = excluded.source_received_at,
    report = excluded.report,
    scan_file = excluded.scan_file,
    scan_size = excluded.scan_size,
    scan_received_at = excluded.scan_received_at,
    scan_unverified_layout = excluded.scan_unverified_layout,
    scan_error = excluded.scan_error;

-- name: DeleteMigrationSession :exec
DELETE FROM migration_session WHERE id = 1;
