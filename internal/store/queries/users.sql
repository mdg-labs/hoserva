-- sqlc input (#22, Q60): typed Go query code for the users table,
-- generated into internal/store/db/ by `make gen`. Deliberately no
-- comment other than each "-- name:" line between queries below, and no
-- non-ASCII character anywhere in this file: sqlc v1.31.1's SQLite engine
-- mis-slices the raw source once an extra comment line appears between
-- two queries (see jobs.sql for that note) and, confirmed while writing
-- this file, also once any comment anywhere in the file contains a
-- multi-byte UTF-8 character (an em dash, in this case) before the last
-- query -- its byte-length/rune-length mismatch throws off every later
-- query's slice offset, corrupting the generated SQL text of every query
-- that follows it, not just the one after the offending comment.

-- name: CreateUser :exec
INSERT INTO users (
    id, username, password_hash, role, totp_secret, totp_confirmed_at, totp_last_step, created_at, totp_pending_secret
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, ?, ?
);

-- name: GetUserByUsername :one
SELECT id, username, password_hash, role, totp_secret, totp_confirmed_at, totp_last_step, created_at, totp_pending_secret
FROM users WHERE username = ?;

-- name: GetUserByID :one
SELECT id, username, password_hash, role, totp_secret, totp_confirmed_at, totp_last_step, created_at, totp_pending_secret
FROM users WHERE id = ?;

-- name: CountAdmins :one
SELECT COUNT(*) FROM users WHERE role = 'admin';

-- name: HasEncryptedTOTPSecrets :one
SELECT EXISTS(SELECT 1 FROM users WHERE totp_secret IS NOT NULL OR totp_pending_secret IS NOT NULL);

-- name: SetUserPendingTOTPSecret :exec
UPDATE users SET totp_pending_secret = sqlc.arg('pending_secret') WHERE id = sqlc.arg('id');

-- name: ActivateUserTOTP :execrows
UPDATE users
SET totp_secret = sqlc.arg('secret'), totp_confirmed_at = sqlc.arg('confirmed_at'),
    totp_last_step = sqlc.arg('step'), totp_pending_secret = NULL
WHERE id = sqlc.arg('id') AND totp_pending_secret = sqlc.arg('pending_secret');

-- name: UpdateUserTOTPLastStepIfNewer :execrows
UPDATE users SET totp_last_step = sqlc.arg('step')
WHERE id = sqlc.arg('id') AND totp_last_step < sqlc.arg('step');
