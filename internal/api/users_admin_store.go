package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// beforeCommitTimeout bounds DeleteUser's beforeCommit hook (the Samba
// account removal). The transaction's write lock is held for the hook's
// whole duration (its rollback guarantee needs that ordering — see
// DeleteUser below), so a hung smbpasswd process must not be able to hold
// it indefinitely.
const beforeCommitTimeout = 10 * time.Second

// ListUsers returns every account, sorted by username. It reads the
// users table directly (not through sqlc's generated single-row queries)
// since this is the only caller that ever needs every row at once.
func (s *AuthStore) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, username, password_hash, role, totp_confirmed_at, created_at, last_login_at, smb_credential_set_at
FROM users ORDER BY username`)
	if err != nil {
		return nil, fmt.Errorf("listing users: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*User
	for rows.Next() {
		var u User
		var totpConfirmedAt, lastLoginAt, smbCredentialSetAt sql.NullString
		var createdAt string
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &totpConfirmedAt, &createdAt, &lastLoginAt, &smbCredentialSetAt); err != nil {
			return nil, fmt.Errorf("scanning user row: %w", err)
		}
		t, err := time.Parse(timeFormat, createdAt)
		if err != nil {
			return nil, fmt.Errorf("parsing user %s created_at: %w", u.ID, err)
		}
		u.CreatedAt = t
		if totpConfirmedAt.Valid {
			ct, err := time.Parse(timeFormat, totpConfirmedAt.String)
			if err != nil {
				return nil, fmt.Errorf("parsing user %s totp_confirmed_at: %w", u.ID, err)
			}
			u.TOTPConfirmedAt = &ct
		}
		if lastLoginAt.Valid {
			lt, err := time.Parse(timeFormat, lastLoginAt.String)
			if err != nil {
				return nil, fmt.Errorf("parsing user %s last_login_at: %w", u.ID, err)
			}
			u.LastLoginAt = &lt
		}
		if smbCredentialSetAt.Valid {
			st, err := time.Parse(timeFormat, smbCredentialSetAt.String)
			if err != nil {
				return nil, fmt.Errorf("parsing user %s smb_credential_set_at: %w", u.ID, err)
			}
			u.SMBCredentialSetAt = &st
		}
		out = append(out, &u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing users: %w", err)
	}
	return out, nil
}

// UpdateUserRole sets userID's role. It refuses (ErrCannotModifyAdmin) an
// admin-role target, and reports ErrUserNotFound for an unknown id. The
// preceding lookup protects the sole admin from a role update.
func (s *AuthStore) UpdateUserRole(ctx context.Context, userID, role string) error {
	u, err := s.GetUserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUserNotFound
		}
		return fmt.Errorf("looking up user: %w", err)
	}
	if u.Role == roleAdmin {
		return ErrCannotModifyAdmin
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE users SET role = ? WHERE id = ?`, role, userID); err != nil {
		return fmt.Errorf("updating role: %w", err)
	}
	return nil
}

// DeleteUser removes userID along with its sessions, personal API tokens
// (#50, Q43), group memberships and per-share permissions, all inside one
// transaction, then invokes
// beforeCommit with the deleted user's username before committing. It
// refuses (ErrCannotModifyAdmin) an admin-role target, and reports
// ErrUserNotFound for an unknown id.
//
// beforeCommit runs the (comparatively failure-prone) Samba account
// removal: if it returns an error, the transaction is rolled back and no
// row is removed at all, so a failed Samba delete never leaves a
// half-deleted user — DB row gone, Samba account orphaned — or vice
// versa. It runs under beforeCommitTimeout, since the write transaction
// (and the SQLite write lock with it) stays open for its entire duration.
func (s *AuthStore) DeleteUser(ctx context.Context, userID string, beforeCommit func(ctx context.Context, username string) error) error {
	u, err := s.GetUserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUserNotFound
		}
		return fmt.Errorf("looking up user: %w", err)
	}
	if u.Role == roleAdmin {
		return ErrCannotModifyAdmin
	}

	sqlDB, ok := s.db.(*sql.DB)
	if !ok {
		return fmt.Errorf("auth store: delete user requires *sql.DB")
	}
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning delete user transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("deleting user sessions: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM api_tokens WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("deleting user api tokens: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_group_members WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("deleting user group memberships: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM share_user_permissions WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("deleting user share permissions: %w", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, userID)
	if err != nil {
		return fmt.Errorf("deleting user: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("deleting user: %w", err)
	} else if n == 0 {
		return ErrUserNotFound
	}

	if beforeCommit != nil {
		bcCtx, cancel := context.WithTimeout(ctx, beforeCommitTimeout)
		err := beforeCommit(bcCtx, u.Username)
		cancel()
		if err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing delete user transaction: %w", err)
	}
	return nil
}
