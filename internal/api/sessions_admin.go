package api

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrSessionNotFound is RevokeSession naming an id no active session has.
var ErrSessionNotFound = errors.New("no such session")

// SessionWithUser is one sessions row joined with its account's username,
// for the admin-facing session list (doc 03 §7).
type SessionWithUser struct {
	TokenHash string
	UserID    string
	Username  string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// ListSessions returns every active session across every account, most
// recently created first.
func (s *AuthService) ListSessions(ctx context.Context) ([]SessionWithUser, error) {
	return s.Store.ListSessionsWithUsernames(ctx)
}

// RevokeSession ends one session immediately, server-side.
func (s *AuthService) RevokeSession(ctx context.Context, id string) error {
	return s.Store.RevokeSessionByID(ctx, id)
}

// ListSessionsWithUsernames joins sessions with users for the admin
// session list.
func (s *AuthStore) ListSessionsWithUsernames(ctx context.Context) ([]SessionWithUser, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT s.token_hash, s.user_id, u.username, s.created_at, s.expires_at
FROM sessions s
JOIN users u ON u.id = s.user_id
ORDER BY s.created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SessionWithUser
	for rows.Next() {
		var sess SessionWithUser
		var createdAt, expiresAt string
		if err := rows.Scan(&sess.TokenHash, &sess.UserID, &sess.Username, &createdAt, &expiresAt); err != nil {
			return nil, fmt.Errorf("scanning session row: %w", err)
		}
		ct, err := time.Parse(timeFormat, createdAt)
		if err != nil {
			return nil, fmt.Errorf("parsing session created_at: %w", err)
		}
		et, err := time.Parse(timeFormat, expiresAt)
		if err != nil {
			return nil, fmt.Errorf("parsing session expires_at: %w", err)
		}
		sess.CreatedAt, sess.ExpiresAt = ct, et
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
	}
	return out, nil
}

// RevokeSessionByID deletes one session by its token hash (its public
// id — never the raw token). Reports ErrSessionNotFound when no session
// has that id.
func (s *AuthStore) RevokeSessionByID(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, id)
	if err != nil {
		return fmt.Errorf("revoking session: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("revoking session: %w", err)
	}
	if n == 0 {
		return ErrSessionNotFound
	}
	return nil
}
