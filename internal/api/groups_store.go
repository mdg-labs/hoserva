package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CreateGroup inserts a new user_groups row.
func (s *AuthStore) CreateGroup(ctx context.Context, id, name string, createdAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO user_groups (id, name, created_at) VALUES (?, ?, ?)`,
		id, name, createdAt.UTC().Format(timeFormat))
	if err != nil {
		return fmt.Errorf("creating group: %w", err)
	}
	return nil
}

// ListGroups returns every group, sorted by name, each with its member
// user ids. Membership is read as one extra query rather than a join, so
// a group with zero members is still returned with an empty (not
// missing) MemberIDs slice.
func (s *AuthStore) ListGroups(ctx context.Context) ([]*Group, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, created_at FROM user_groups ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("listing groups: %w", err)
	}
	defer func() { _ = rows.Close() }()

	byID := make(map[string]*Group)
	var order []string
	for rows.Next() {
		var g Group
		var createdAt string
		if err := rows.Scan(&g.ID, &g.Name, &createdAt); err != nil {
			return nil, fmt.Errorf("scanning group row: %w", err)
		}
		t, err := time.Parse(timeFormat, createdAt)
		if err != nil {
			return nil, fmt.Errorf("parsing group %s created_at: %w", g.ID, err)
		}
		g.CreatedAt = t
		g.MemberIDs = []string{}
		byID[g.ID] = &g
		order = append(order, g.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing groups: %w", err)
	}

	memberRows, err := s.db.QueryContext(ctx, `SELECT group_id, user_id FROM user_group_members`)
	if err != nil {
		return nil, fmt.Errorf("listing group members: %w", err)
	}
	defer func() { _ = memberRows.Close() }()
	for memberRows.Next() {
		var groupID, userID string
		if err := memberRows.Scan(&groupID, &userID); err != nil {
			return nil, fmt.Errorf("scanning group member row: %w", err)
		}
		if g, ok := byID[groupID]; ok {
			g.MemberIDs = append(g.MemberIDs, userID)
		}
	}
	if err := memberRows.Err(); err != nil {
		return nil, fmt.Errorf("listing group members: %w", err)
	}

	out := make([]*Group, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out, nil
}

// GetGroupByID returns one group with its member user ids, or
// ErrGroupNotFound.
func (s *AuthStore) GetGroupByID(ctx context.Context, id string) (*Group, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, name, created_at FROM user_groups WHERE id = ?`, id)
	var g Group
	var createdAt string
	if err := row.Scan(&g.ID, &g.Name, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrGroupNotFound
		}
		return nil, fmt.Errorf("getting group: %w", err)
	}
	t, err := time.Parse(timeFormat, createdAt)
	if err != nil {
		return nil, fmt.Errorf("parsing group %s created_at: %w", g.ID, err)
	}
	g.CreatedAt = t

	memberRows, err := s.db.QueryContext(ctx, `SELECT user_id FROM user_group_members WHERE group_id = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("listing group members: %w", err)
	}
	defer func() { _ = memberRows.Close() }()
	g.MemberIDs = []string{}
	for memberRows.Next() {
		var userID string
		if err := memberRows.Scan(&userID); err != nil {
			return nil, fmt.Errorf("scanning group member row: %w", err)
		}
		g.MemberIDs = append(g.MemberIDs, userID)
	}
	if err := memberRows.Err(); err != nil {
		return nil, fmt.Errorf("listing group members: %w", err)
	}
	return &g, nil
}

// DeleteGroup removes id along with its memberships and its per-share
// permissions, all inside one transaction. Reports ErrGroupNotFound for
// an unknown id.
func (s *AuthStore) DeleteGroup(ctx context.Context, id string) error {
	sqlDB, ok := s.db.(*sql.DB)
	if !ok {
		return fmt.Errorf("auth store: delete group requires *sql.DB")
	}
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning delete group transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM share_group_permissions WHERE group_id = ?`, id); err != nil {
		return fmt.Errorf("deleting group share permissions: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_group_members WHERE group_id = ?`, id); err != nil {
		return fmt.Errorf("deleting group memberships: %w", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM user_groups WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting group: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("deleting group: %w", err)
	} else if n == 0 {
		return ErrGroupNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing delete group transaction: %w", err)
	}
	return nil
}

// SetGroupMembers replaces group id's membership with exactly userIDs,
// inside one transaction. Refuses (ErrGroupNotFound) an unknown group,
// and (ErrUserNotFound) any userIDs entry that names no account — checked
// before anything is written, so a bad id never partially replaces a
// group's membership.
func (s *AuthStore) SetGroupMembers(ctx context.Context, groupID string, userIDs []string) error {
	sqlDB, ok := s.db.(*sql.DB)
	if !ok {
		return fmt.Errorf("auth store: set group members requires *sql.DB")
	}
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning set group members transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM user_groups WHERE id = ?`, groupID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrGroupNotFound
		}
		return fmt.Errorf("checking group: %w", err)
	}

	seen := make(map[string]struct{}, len(userIDs))
	for _, userID := range userIDs {
		if _, dup := seen[userID]; dup {
			return fmt.Errorf("%w: %s", ErrDuplicateGrant, userID)
		}
		seen[userID] = struct{}{}
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM users WHERE id = ?`, userID).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: %s", ErrUserNotFound, userID)
			}
			return fmt.Errorf("checking user %s: %w", userID, err)
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM user_group_members WHERE group_id = ?`, groupID); err != nil {
		return fmt.Errorf("clearing group members: %w", err)
	}
	for _, userID := range userIDs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO user_group_members (group_id, user_id) VALUES (?, ?)`,
			groupID, userID); err != nil {
			return fmt.Errorf("adding group member %s: %w", userID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing set group members transaction: %w", err)
	}
	return nil
}
