-- sqlc input (#71): the last container update check's result per image,
-- generated into internal/store/db/ by `make gen`. Doc comments live on the
-- Go wrapper in internal/store/updates.go.

-- name: UpsertImageUpdateCheck :exec
INSERT INTO image_update_checks (image, checked_at, status, kind, available_tag, message)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (image) DO UPDATE SET
    checked_at = excluded.checked_at,
    status = excluded.status,
    kind = excluded.kind,
    available_tag = excluded.available_tag,
    message = excluded.message;

-- name: ListImageUpdateChecks :many
SELECT image, checked_at, status, kind, available_tag, message
FROM image_update_checks ORDER BY image ASC;

-- name: DeleteImageUpdateCheck :exec
DELETE FROM image_update_checks WHERE image = ?;
