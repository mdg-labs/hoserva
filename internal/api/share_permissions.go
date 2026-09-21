package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/mdg-labs/hoserva/internal/store"
)

// ErrInvalidAccessLevel is a share-permission write naming anything but
// none, read-only or read-write.
var ErrInvalidAccessLevel = errors.New("access must be none, read-only or read-write")

func validateAccessLevel(access string) error {
	switch access {
	case "none", "read-only", "read-write":
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrInvalidAccessLevel, access)
	}
}

// PermissionGrant is one {id, access} pair in a share-permission replace
// request — the same shape for a user id or a group id.
type PermissionGrant struct {
	ID     string
	Access string
}

// SharePermissionUser is one row of a share's per-user access (the
// share-side read).
type SharePermissionUser struct {
	UserID   string
	Username string
	Access   string
}

// SharePermissionGroup is one row of a share's per-group access (the
// share-side read).
type SharePermissionGroup struct {
	GroupID string
	Name    string
	Access  string
}

// UserSharePermission is one row of an account's per-share access (the
// user-side read).
type UserSharePermission struct {
	ShareName string
	Access    string
}

// GetSharePermissions returns shareName's explicit per-user and per-group
// access (Q27, doc 03 §7). A user or group with no row is not returned —
// none of the three levels is assumed.
func (s *AuthService) GetSharePermissions(ctx context.Context, shareName string) ([]SharePermissionUser, []SharePermissionGroup, error) {
	return s.Store.GetSharePermissions(ctx, shareName)
}

// SetSharePermissions replaces shareName's per-user and per-group access
// with exactly users and groups (doc 03 §7: the share-side editor).
func (s *AuthService) SetSharePermissions(ctx context.Context, shareName string, users, groups []PermissionGrant) error {
	return s.Store.SetSharePermissions(ctx, shareName, users, groups)
}

// GetUserSharePermissions returns userID's explicit per-share access
// across every share (the user-side read).
func (s *AuthService) GetUserSharePermissions(ctx context.Context, userID string) ([]UserSharePermission, error) {
	return s.Store.GetUserSharePermissions(ctx, userID)
}

// SetUserSharePermissions replaces userID's per-share access with exactly
// entries (doc 03 §7: the user-side editor).
func (s *AuthService) SetUserSharePermissions(ctx context.Context, userID string, entries []UserSharePermission) error {
	return s.Store.SetUserSharePermissions(ctx, userID, entries)
}

// GetSharePermissions joins share_user_permissions and
// share_group_permissions with their subjects' names.
func (s *AuthStore) GetSharePermissions(ctx context.Context, shareName string) ([]SharePermissionUser, []SharePermissionGroup, error) {
	userRows, err := s.db.QueryContext(ctx, `
SELECT sup.user_id, u.username, sup.access
FROM share_user_permissions sup
JOIN users u ON u.id = sup.user_id
WHERE sup.share_name = ?
ORDER BY u.username`, shareName)
	if err != nil {
		return nil, nil, fmt.Errorf("listing share user permissions: %w", err)
	}
	defer func() { _ = userRows.Close() }()
	var users []SharePermissionUser
	for userRows.Next() {
		var p SharePermissionUser
		if err := userRows.Scan(&p.UserID, &p.Username, &p.Access); err != nil {
			return nil, nil, fmt.Errorf("scanning share user permission row: %w", err)
		}
		users = append(users, p)
	}
	if err := userRows.Err(); err != nil {
		return nil, nil, fmt.Errorf("listing share user permissions: %w", err)
	}

	groupRows, err := s.db.QueryContext(ctx, `
SELECT sgp.group_id, g.name, sgp.access
FROM share_group_permissions sgp
JOIN user_groups g ON g.id = sgp.group_id
WHERE sgp.share_name = ?
ORDER BY g.name`, shareName)
	if err != nil {
		return nil, nil, fmt.Errorf("listing share group permissions: %w", err)
	}
	defer func() { _ = groupRows.Close() }()
	var groups []SharePermissionGroup
	for groupRows.Next() {
		var p SharePermissionGroup
		if err := groupRows.Scan(&p.GroupID, &p.Name, &p.Access); err != nil {
			return nil, nil, fmt.Errorf("scanning share group permission row: %w", err)
		}
		groups = append(groups, p)
	}
	if err := groupRows.Err(); err != nil {
		return nil, nil, fmt.Errorf("listing share group permissions: %w", err)
	}
	return users, groups, nil
}

// SetSharePermissions replaces shareName's rows in both permission tables
// with exactly users and groups, inside one transaction. Every id and
// access level is validated before anything is written: an unknown user
// or group id (ErrUserNotFound/ErrGroupNotFound) or an invalid access
// level (ErrInvalidAccessLevel) leaves the share's existing permissions
// untouched.
func (s *AuthStore) SetSharePermissions(ctx context.Context, shareName string, users, groups []PermissionGrant) error {
	sqlDB, ok := s.db.(*sql.DB)
	if !ok {
		return fmt.Errorf("auth store: set share permissions requires *sql.DB")
	}
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning set share permissions transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, g := range users {
		if err := validateAccessLevel(g.Access); err != nil {
			return err
		}
		if err := existsInTx(ctx, tx, "users", g.ID); err != nil {
			return err
		}
	}
	for _, g := range groups {
		if err := validateAccessLevel(g.Access); err != nil {
			return err
		}
		if err := existsInTx(ctx, tx, "user_groups", g.ID); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM share_user_permissions WHERE share_name = ?`, shareName); err != nil {
		return fmt.Errorf("clearing share user permissions: %w", err)
	}
	for _, g := range users {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO share_user_permissions (share_name, user_id, access) VALUES (?, ?, ?)`,
			shareName, g.ID, g.Access); err != nil {
			return fmt.Errorf("setting share user permission for %s: %w", g.ID, err)
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM share_group_permissions WHERE share_name = ?`, shareName); err != nil {
		return fmt.Errorf("clearing share group permissions: %w", err)
	}
	for _, g := range groups {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO share_group_permissions (share_name, group_id, access) VALUES (?, ?, ?)`,
			shareName, g.ID, g.Access); err != nil {
			return fmt.Errorf("setting share group permission for %s: %w", g.ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing set share permissions transaction: %w", err)
	}
	return nil
}

// GetUserSharePermissions returns userID's explicit per-share access,
// sorted by share name. Reports ErrUserNotFound for an unknown userID
// rather than an empty list, matching the mock's behaviour.
func (s *AuthStore) GetUserSharePermissions(ctx context.Context, userID string) ([]UserSharePermission, error) {
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM users WHERE id = ?`, userID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s", ErrUserNotFound, userID)
		}
		return nil, fmt.Errorf("checking user %s: %w", userID, err)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT share_name, access FROM share_user_permissions WHERE user_id = ? ORDER BY share_name`, userID)
	if err != nil {
		return nil, fmt.Errorf("listing user share permissions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []UserSharePermission
	for rows.Next() {
		var p UserSharePermission
		if err := rows.Scan(&p.ShareName, &p.Access); err != nil {
			return nil, fmt.Errorf("scanning user share permission row: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing user share permissions: %w", err)
	}
	return out, nil
}

// SetUserSharePermissions replaces userID's rows in share_user_permissions
// with exactly entries, inside one transaction. userID, every share name
// and every access level is validated before anything is written.
func (s *AuthStore) SetUserSharePermissions(ctx context.Context, userID string, entries []UserSharePermission) error {
	sqlDB, ok := s.db.(*sql.DB)
	if !ok {
		return fmt.Errorf("auth store: set user share permissions requires *sql.DB")
	}
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning set user share permissions transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := existsInTx(ctx, tx, "users", userID); err != nil {
		return err
	}
	for _, e := range entries {
		if err := validateAccessLevel(e.Access); err != nil {
			return err
		}
		if err := existsInTx(ctx, tx, "shares", e.ShareName); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM share_user_permissions WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("clearing user share permissions: %w", err)
	}
	for _, e := range entries {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO share_user_permissions (share_name, user_id, access) VALUES (?, ?, ?)`,
			e.ShareName, userID, e.Access); err != nil {
			return fmt.Errorf("setting user share permission for %s: %w", e.ShareName, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing set user share permissions transaction: %w", err)
	}
	return nil
}

// existsInTx reports ErrUserNotFound, ErrGroupNotFound or
// store.ErrShareNotFound when id names no row in table — table is always
// one of this file's own literal strings, never caller input, so this
// never interpolates anything untrusted into SQL.
func existsInTx(ctx context.Context, tx *sql.Tx, table, id string) error {
	var exists int
	var query string
	switch table {
	case "users":
		query = `SELECT 1 FROM users WHERE id = ?`
	case "user_groups":
		query = `SELECT 1 FROM user_groups WHERE id = ?`
	case "shares":
		query = `SELECT 1 FROM shares WHERE name = ?`
	default:
		return fmt.Errorf("existsInTx: unknown table %q", table)
	}
	if err := tx.QueryRowContext(ctx, query, id).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			switch table {
			case "users":
				return fmt.Errorf("%w: %s", ErrUserNotFound, id)
			case "user_groups":
				return fmt.Errorf("%w: %s", ErrGroupNotFound, id)
			case "shares":
				return fmt.Errorf("%w: %s", store.ErrShareNotFound, id)
			}
		}
		return fmt.Errorf("checking %s %s: %w", table, id, err)
	}
	return nil
}
