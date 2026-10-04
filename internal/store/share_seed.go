package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SeedUser is an account an Unraid import creates: share-only, with a random
// password hash nobody knows, so it has no working credential until a password
// is set (Q27).
type SeedUser struct {
	ID           string
	Username     string
	PasswordHash string
	CreatedAt    time.Time
}

// SeedGrant is one per-user access level on a seeded share.
type SeedGrant struct {
	Username string
	Access   string
}

// SeedShare is a share an Unraid import creates, with the access its read and
// write lists map to.
type SeedShare struct {
	Share  Share
	Grants []SeedGrant
}

// Seeded is what SeedMigration created and what it left alone, by name.
type Seeded struct {
	Users          []SeedUser
	ExistingUsers  []string
	Shares         []string
	ExistingShares []string
}

// SeedMigration creates the users and shares of an Unraid import and the
// per-user grants of the shares it creates, in one transaction: any failure
// leaves nothing of it. A share or user that already exists is left exactly as
// it is and reported in Existing*, and a grant is written only for a share this
// call created. A grant for a username that is neither created here nor already
// an account is an error, never skipped.
func (s *ShareStore) SeedMigration(ctx context.Context, users []SeedUser, shares []SeedShare) (Seeded, error) {
	var out Seeded
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Seeded{}, fmt.Errorf("store: beginning the migration seed: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	ids := map[string]string{}
	for _, u := range users {
		var id string
		err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE username = ?`, u.Username).Scan(&id)
		switch {
		case err == nil:
			ids[u.Username] = id
			out.ExistingUsers = append(out.ExistingUsers, u.Username)
			continue
		case !errors.Is(err, sql.ErrNoRows):
			return Seeded{}, fmt.Errorf("store: looking up user %s: %w", u.Username, err)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO users (id, username, password_hash, role, totp_last_step, created_at)
VALUES (?, ?, ?, 'share-only', 0, ?)`, u.ID, u.Username, u.PasswordHash, u.CreatedAt.UTC().Format(TimeFormat)); err != nil {
			return Seeded{}, fmt.Errorf("store: creating user %s: %w", u.Username, err)
		}
		ids[u.Username] = u.ID
		out.Users = append(out.Users, u)
	}

	q := s.q.WithTx(tx)
	for _, sh := range shares {
		rec := sh.Share
		if _, err := q.GetShare(ctx, rec.Name); err == nil {
			out.ExistingShares = append(out.ExistingShares, rec.Name)
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			return Seeded{}, fmt.Errorf("store: looking up share %s: %w", rec.Name, err)
		}
		if err := (&ShareStore{db: s.db, q: q}).Insert(ctx, rec); err != nil {
			return Seeded{}, err
		}
		for _, g := range sh.Grants {
			id, ok := ids[g.Username]
			if !ok {
				return Seeded{}, fmt.Errorf("store: share %s grants access to %s, who is not an account of this import", rec.Name, g.Username)
			}
			if _, err := tx.ExecContext(ctx, `
INSERT INTO share_user_permissions (share_name, user_id, access) VALUES (?, ?, ?)`, rec.Name, id, g.Access); err != nil {
				return Seeded{}, fmt.Errorf("store: granting %s access to share %s: %w", g.Username, rec.Name, err)
			}
		}
		out.Shares = append(out.Shares, rec.Name)
	}
	if err := tx.Commit(); err != nil {
		return Seeded{}, fmt.Errorf("store: committing the migration seed: %w", err)
	}
	return out, nil
}

// UnseedMigration removes what SeedMigration reported creating: the shares with
// their grants, then the users with theirs, in one transaction. A name that is
// already gone is not an error.
func (s *ShareStore) UnseedMigration(ctx context.Context, seeded Seeded) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: beginning the removal of a migration seed: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, name := range seeded.Shares {
		if _, err := tx.ExecContext(ctx, `DELETE FROM share_user_permissions WHERE share_name = ?`, name); err != nil {
			return fmt.Errorf("store: removing the grants of share %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM share_group_permissions WHERE share_name = ?`, name); err != nil {
			return fmt.Errorf("store: removing the group grants of share %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM shares WHERE name = ?`, name); err != nil {
			return fmt.Errorf("store: removing share %s: %w", name, err)
		}
	}
	for _, u := range seeded.Users {
		for _, stmt := range []string{
			`DELETE FROM share_user_permissions WHERE user_id = ?`,
			`DELETE FROM user_group_members WHERE user_id = ?`,
			`DELETE FROM users WHERE id = ?`,
		} {
			if _, err := tx.ExecContext(ctx, stmt, u.ID); err != nil {
				return fmt.Errorf("store: removing user %s: %w", u.Username, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: committing the removal of a migration seed: %w", err)
	}
	return nil
}
