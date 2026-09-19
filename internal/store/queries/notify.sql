-- sqlc input (#35, Q60): typed Go query code for the notify_channels,
-- notify_routes, notify_event_severity, notify_quiet_hours and
-- notify_deliveries tables, generated into internal/store/db/ by
-- `make gen`. Deliberately no comment other than each "-- name:" line
-- between queries below (see jobs.sql for the same note about sqlc's
-- SQLite engine mis-slicing raw source around extra comment lines). Doc
-- comments live on the hand-written Go wrapper in internal/notify/store.go
-- instead.

-- name: CreateChannel :exec
INSERT INTO notify_channels (
    id, name, "type", enabled, config, secret, created_at, updated_at
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, ?
);

-- name: GetChannel :one
SELECT id, name, "type", enabled, config, secret, created_at, updated_at
FROM notify_channels WHERE id = ?;

-- name: ListChannels :many
SELECT id, name, "type", enabled, config, secret, created_at, updated_at
FROM notify_channels ORDER BY created_at ASC;

-- name: UpdateChannel :execrows
UPDATE notify_channels
SET name = ?, "type" = ?, enabled = ?, config = ?, secret = ?, updated_at = ?
WHERE id = ?;

-- name: DeleteChannel :execrows
DELETE FROM notify_channels WHERE id = ?;

-- name: DeleteRoutesForChannel :exec
DELETE FROM notify_routes WHERE channel_id = ?;

-- name: DeleteDeliveriesForChannel :exec
DELETE FROM notify_deliveries WHERE channel_id = ?;

-- name: HasEncryptedNotifySecrets :one
SELECT EXISTS(SELECT 1 FROM notify_channels WHERE secret IS NOT NULL);

-- name: UpsertEventSeverity :exec
INSERT INTO notify_event_severity (event_type, severity) VALUES (?, ?)
ON CONFLICT (event_type) DO UPDATE SET severity = excluded.severity;

-- name: ListEventSeverityOverrides :many
SELECT event_type, severity FROM notify_event_severity;

-- name: GetEventSeverityOverride :one
SELECT severity FROM notify_event_severity WHERE event_type = ?;

-- name: DeleteRoutesForEvent :exec
DELETE FROM notify_routes WHERE event_type = ?;

-- name: InsertRoute :exec
INSERT INTO notify_routes (event_type, channel_id) VALUES (?, ?);

-- name: ListAllRoutes :many
SELECT event_type, channel_id FROM notify_routes ORDER BY event_type ASC, channel_id ASC;

-- name: ListRoutesForEvent :many
SELECT channel_id FROM notify_routes WHERE event_type = ? ORDER BY channel_id ASC;

-- name: ListChannelsForEvent :many
SELECT c.id, c.name, c."type", c.enabled, c.config, c.secret, c.created_at, c.updated_at
FROM notify_channels c
JOIN notify_routes r ON r.channel_id = c.id
WHERE r.event_type = ? AND c.enabled = 1
ORDER BY c.created_at ASC;

-- name: GetQuietHours :one
SELECT id, enabled, start_time, end_time, updated_at FROM notify_quiet_hours WHERE id = 1;

-- name: SetQuietHours :exec
INSERT INTO notify_quiet_hours (id, enabled, start_time, end_time, updated_at)
VALUES (1, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    enabled = excluded.enabled,
    start_time = excluded.start_time,
    end_time = excluded.end_time,
    updated_at = excluded.updated_at;

-- name: CreateDelivery :exec
INSERT INTO notify_deliveries (
    id, channel_id, event_type, severity, title, message,
    "status", attempts, last_error, created_at, next_attempt_at, delivered_at
) VALUES (
    ?, ?, ?, ?, ?, ?,
    ?, ?, ?, ?, ?, ?
);

-- name: GetDelivery :one
SELECT id, channel_id, event_type, severity, title, message,
    "status", attempts, last_error, created_at, next_attempt_at, delivered_at
FROM notify_deliveries WHERE id = ?;

-- name: ListDueDeliveries :many
SELECT id, channel_id, event_type, severity, title, message,
    "status", attempts, last_error, created_at, next_attempt_at, delivered_at
FROM notify_deliveries
WHERE "status" = 'pending' AND next_attempt_at <= ?
ORDER BY next_attempt_at ASC
LIMIT sqlc.arg('row_limit');

-- name: UpdateDeliveryAttempt :exec
UPDATE notify_deliveries
SET "status" = ?, attempts = ?, last_error = ?, next_attempt_at = ?, delivered_at = ?
WHERE id = ?;

-- name: CreateAlert :exec
INSERT INTO notify_alerts (
    id, event_type, severity, title, message, created_at, read_at
) VALUES (
    ?, ?, ?, ?, ?, ?, ?
);

-- name: ListAlerts :many
SELECT id, event_type, severity, title, message, created_at, read_at
FROM notify_alerts
ORDER BY
    CASE WHEN read_at IS NULL THEN 0 ELSE 1 END ASC,
    created_at DESC;

-- name: CountUnreadAlerts :one
SELECT COUNT(*) FROM notify_alerts WHERE read_at IS NULL;

-- name: MarkAllAlertsRead :execrows
UPDATE notify_alerts
SET read_at = ?
WHERE read_at IS NULL;

-- name: MarkAlertsReadByIDs :execrows
UPDATE notify_alerts
SET read_at = ?
WHERE read_at IS NULL AND id IN (sqlc.slice('ids'));
