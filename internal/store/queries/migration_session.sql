-- sqlc input (#75): the one Unraid migration session, generated into
-- internal/store/db/ by `make gen`.

-- name: GetMigrationSession :one
SELECT source_file, source_size, source_received_at, report,
       scan_file, scan_size, scan_received_at, scan_unverified_layout, scan_error, scan_full_checksums, verify, checklist
FROM migration_session
WHERE id = 1;

-- name: UpsertMigrationSession :exec
INSERT INTO migration_session (
    id, source_file, source_size, source_received_at, report,
    scan_file, scan_size, scan_received_at, scan_unverified_layout, scan_error, scan_full_checksums, verify
) VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    source_file = excluded.source_file,
    source_size = excluded.source_size,
    source_received_at = excluded.source_received_at,
    report = excluded.report,
    scan_file = excluded.scan_file,
    scan_size = excluded.scan_size,
    scan_received_at = excluded.scan_received_at,
    scan_unverified_layout = excluded.scan_unverified_layout,
    scan_error = excluded.scan_error,
    scan_full_checksums = excluded.scan_full_checksums,
    verify = excluded.verify;

-- name: SetMigrationSessionChecklist :exec
INSERT INTO migration_session (id, checklist) VALUES (1, ?)
ON CONFLICT (id) DO UPDATE SET checklist = excluded.checklist;

-- name: ClearMigrationSession :exec
UPDATE migration_session SET
    source_file = '', source_size = 0, source_received_at = '', report = '',
    scan_file = '', scan_size = 0, scan_received_at = '', scan_unverified_layout = 0,
    scan_error = '', scan_full_checksums = 0, verify = ''
WHERE id = 1;

-- name: DeleteMigrationSession :exec
DELETE FROM migration_session WHERE id = 1 AND checklist = '';
