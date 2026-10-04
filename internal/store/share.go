package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	storedb "github.com/mdg-labs/hoserva/internal/store/db"
)

var (
	// ErrShareNotFound is GetShare/UpdateShare/DeleteShare when no row
	// has that name.
	ErrShareNotFound = errors.New("store: share not found")
	// ErrShareExists is InsertShare's refusal of a duplicate name.
	ErrShareExists = errors.New("store: share already exists")
)

// Share is one row of the shares table (#46).
type Share struct {
	Name                  string
	CacheMode             string
	CreatePolicy          string
	SMBEnabled            bool
	SMBGuest              bool
	SMBReadOnly           bool
	SMBBrowseable         bool
	SMBRecycle            bool
	SMBTimeMachine        bool
	SMBTimeMachineMaxSize string
	NFSEnabled            bool
	NFSHosts              []string
	NFSSquash             string
	CreatedAt             time.Time
	UpdatedAt             time.Time
	// MinFreeSpace is the share's own mergerfs minfreespace; empty keeps the
	// array's.
	MinFreeSpace string
	// TargetCacheMode is the cache mode an Unraid import wants once the cache
	// exists, while CacheMode is array-only; empty when there is none.
	TargetCacheMode string
	// MigrationNotes is what the Unraid import could not map exactly.
	MigrationNotes []string
}

// ShareStore persists shares in the central SQLite database (D4).
type ShareStore struct {
	db *sql.DB
	q  *storedb.Queries
}

// NewShareStore wraps db for share persistence.
func NewShareStore(db *sql.DB) *ShareStore {
	return &ShareStore{db: db, q: storedb.New(db)}
}

// Insert creates a share row. It refuses (ErrShareExists) a duplicate name.
func (s *ShareStore) Insert(ctx context.Context, rec Share) error {
	return insertShare(ctx, s.q, rec)
}

func insertShare(ctx context.Context, q *storedb.Queries, rec Share) error {
	err := q.InsertShare(ctx, storedb.InsertShareParams{
		Name:                  rec.Name,
		CacheMode:             rec.CacheMode,
		CreatePolicy:          rec.CreatePolicy,
		SmbEnabled:            boolToInt(rec.SMBEnabled),
		SmbGuest:              boolToInt(rec.SMBGuest),
		SmbReadOnly:           boolToInt(rec.SMBReadOnly),
		SmbBrowseable:         boolToInt(rec.SMBBrowseable),
		SmbRecycle:            boolToInt(rec.SMBRecycle),
		SmbTimeMachine:        boolToInt(rec.SMBTimeMachine),
		SmbTimeMachineMaxSize: nullString(rec.SMBTimeMachineMaxSize),
		NfsEnabled:            boolToInt(rec.NFSEnabled),
		NfsHosts:              marshalNFSHosts(rec.NFSHosts),
		NfsSquash:             nfsSquashOrDefault(rec.NFSSquash),
		CreatedAt:             rec.CreatedAt.UTC().Format(TimeFormat),
		UpdatedAt:             rec.UpdatedAt.UTC().Format(TimeFormat),
		MinFreeSpace:          rec.MinFreeSpace,
		TargetCacheMode:       rec.TargetCacheMode,
		MigrationNotes:        marshalNotes(rec.MigrationNotes),
	})
	if isUniqueConstraint(err) {
		return fmt.Errorf("%w: %s", ErrShareExists, rec.Name)
	}
	if err != nil {
		return fmt.Errorf("store: inserting share %s: %w", rec.Name, err)
	}
	return nil
}

// Get returns the share named name, or ErrShareNotFound.
func (s *ShareStore) Get(ctx context.Context, name string) (Share, error) {
	row, err := s.q.GetShare(ctx, name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Share{}, fmt.Errorf("%w: %s", ErrShareNotFound, name)
		}
		return Share{}, fmt.Errorf("store: getting share %s: %w", name, err)
	}
	return shareFromRow(row)
}

// List returns every share, sorted by name.
func (s *ShareStore) List(ctx context.Context) ([]Share, error) {
	rows, err := s.q.ListShares(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: listing shares: %w", err)
	}
	out := make([]Share, 0, len(rows))
	for _, row := range rows {
		rec, err := shareFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// Update replaces mutable columns of the named share. It refuses
// (ErrShareNotFound) a missing name.
func (s *ShareStore) Update(ctx context.Context, rec Share) error {
	n, err := s.q.UpdateShare(ctx, storedb.UpdateShareParams{
		CacheMode:             rec.CacheMode,
		CreatePolicy:          rec.CreatePolicy,
		SmbEnabled:            boolToInt(rec.SMBEnabled),
		SmbGuest:              boolToInt(rec.SMBGuest),
		SmbReadOnly:           boolToInt(rec.SMBReadOnly),
		SmbBrowseable:         boolToInt(rec.SMBBrowseable),
		SmbRecycle:            boolToInt(rec.SMBRecycle),
		SmbTimeMachine:        boolToInt(rec.SMBTimeMachine),
		SmbTimeMachineMaxSize: nullString(rec.SMBTimeMachineMaxSize),
		NfsEnabled:            boolToInt(rec.NFSEnabled),
		NfsHosts:              marshalNFSHosts(rec.NFSHosts),
		NfsSquash:             nfsSquashOrDefault(rec.NFSSquash),
		UpdatedAt:             rec.UpdatedAt.UTC().Format(TimeFormat),
		MinFreeSpace:          rec.MinFreeSpace,
		TargetCacheMode:       rec.TargetCacheMode,
		MigrationNotes:        marshalNotes(rec.MigrationNotes),
		Name:                  rec.Name,
	})
	if err != nil {
		return fmt.Errorf("store: updating share %s: %w", rec.Name, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrShareNotFound, rec.Name)
	}
	return nil
}

// ShareGrant is one per-user or per-group access row of a share: the user or
// group id and its level.
type ShareGrant struct {
	ID     string
	Access string
}

// ShareGrants is what a share's two permission tables held.
type ShareGrants struct {
	Users  []ShareGrant
	Groups []ShareGrant
}

// Delete removes the share definition with its per-user and per-group access
// grants, in one transaction, so a share created later under the same name
// inherits none of them. It refuses (ErrShareNotFound) a missing name and
// then removes nothing. It never touches files on disk.
func (s *ShareStore) Delete(ctx context.Context, name string) error {
	_, err := s.Remove(ctx, name)
	return err
}

// Remove is Delete that also returns the grants it removed, read in the same
// transaction, so Restore can put the share back exactly as it was.
func (s *ShareStore) Remove(ctx context.Context, name string) (ShareGrants, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ShareGrants{}, fmt.Errorf("store: beginning the deletion of share %s: %w", name, err)
	}
	defer func() { _ = tx.Rollback() }()
	var grants ShareGrants
	for _, t := range []struct {
		table, col string
		into       *[]ShareGrant
	}{
		{"share_user_permissions", "user_id", &grants.Users},
		{"share_group_permissions", "group_id", &grants.Groups},
	} {
		rows, err := tx.QueryContext(ctx, `SELECT `+t.col+`, access FROM `+t.table+` WHERE share_name = ? ORDER BY `+t.col, name)
		if err != nil {
			return ShareGrants{}, fmt.Errorf("store: reading the grants of share %s: %w", name, err)
		}
		for rows.Next() {
			var g ShareGrant
			if err := rows.Scan(&g.ID, &g.Access); err != nil {
				_ = rows.Close()
				return ShareGrants{}, fmt.Errorf("store: reading the grants of share %s: %w", name, err)
			}
			*t.into = append(*t.into, g)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return ShareGrants{}, fmt.Errorf("store: reading the grants of share %s: %w", name, err)
		}
		if err := rows.Close(); err != nil {
			return ShareGrants{}, fmt.Errorf("store: reading the grants of share %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+t.table+` WHERE share_name = ?`, name); err != nil {
			return ShareGrants{}, fmt.Errorf("store: removing the grants of share %s: %w", name, err)
		}
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM shares WHERE name = ?`, name)
	if err != nil {
		return ShareGrants{}, fmt.Errorf("store: deleting share %s: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return ShareGrants{}, fmt.Errorf("store: deleting share %s: %w", name, err)
	}
	if n == 0 {
		return ShareGrants{}, fmt.Errorf("%w: %s", ErrShareNotFound, name)
	}
	if err := tx.Commit(); err != nil {
		return ShareGrants{}, fmt.Errorf("store: committing the deletion of share %s: %w", name, err)
	}
	return grants, nil
}

// Restore inserts a share removed by Remove together with its grants, in one
// transaction: either all of it is back or none of it is.
func (s *ShareStore) Restore(ctx context.Context, rec Share, grants ShareGrants) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: beginning the restore of share %s: %w", rec.Name, err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertShare(ctx, s.q.WithTx(tx), rec); err != nil {
		return err
	}
	for _, t := range []struct {
		table, col string
		rows       []ShareGrant
	}{
		{"share_user_permissions", "user_id", grants.Users},
		{"share_group_permissions", "group_id", grants.Groups},
	} {
		for _, g := range t.rows {
			if _, err := tx.ExecContext(ctx, `INSERT INTO `+t.table+` (share_name, `+t.col+`, access) VALUES (?, ?, ?)`, rec.Name, g.ID, g.Access); err != nil {
				return fmt.Errorf("store: restoring a grant of share %s: %w", rec.Name, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: committing the restore of share %s: %w", rec.Name, err)
	}
	return nil
}

func shareFromRow(row *storedb.Share) (Share, error) {
	createdAt, err := time.Parse(TimeFormat, row.CreatedAt)
	if err != nil {
		return Share{}, fmt.Errorf("store: parsing share %s created_at: %w", row.Name, err)
	}
	updatedAt, err := time.Parse(TimeFormat, row.UpdatedAt)
	if err != nil {
		return Share{}, fmt.Errorf("store: parsing share %s updated_at: %w", row.Name, err)
	}
	hosts, err := unmarshalNFSHosts(row.NfsHosts)
	if err != nil {
		return Share{}, fmt.Errorf("store: parsing share %s nfs_hosts: %w", row.Name, err)
	}
	notes, err := unmarshalNotes(row.MigrationNotes)
	if err != nil {
		return Share{}, fmt.Errorf("store: parsing share %s migration_notes: %w", row.Name, err)
	}
	return Share{
		Name:                  row.Name,
		CacheMode:             row.CacheMode,
		CreatePolicy:          row.CreatePolicy,
		SMBEnabled:            row.SmbEnabled != 0,
		SMBGuest:              row.SmbGuest != 0,
		SMBReadOnly:           row.SmbReadOnly != 0,
		SMBBrowseable:         row.SmbBrowseable != 0,
		SMBRecycle:            row.SmbRecycle != 0,
		SMBTimeMachine:        row.SmbTimeMachine != 0,
		SMBTimeMachineMaxSize: row.SmbTimeMachineMaxSize.String,
		NFSEnabled:            row.NfsEnabled != 0,
		NFSHosts:              hosts,
		NFSSquash:             nfsSquashOrDefault(row.NfsSquash),
		CreatedAt:             createdAt,
		UpdatedAt:             updatedAt,
		MinFreeSpace:          row.MinFreeSpace,
		TargetCacheMode:       row.TargetCacheMode,
		MigrationNotes:        notes,
	}, nil
}

func marshalNotes(notes []string) string {
	if notes == nil {
		notes = []string{}
	}
	b, err := json.Marshal(notes)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func unmarshalNotes(raw string) ([]string, error) {
	if raw == "" {
		return []string{}, nil
	}
	var notes []string
	if err := json.Unmarshal([]byte(raw), &notes); err != nil {
		return nil, err
	}
	if notes == nil {
		notes = []string{}
	}
	return notes, nil
}

func marshalNFSHosts(hosts []string) string {
	if hosts == nil {
		hosts = []string{}
	}
	b, err := json.Marshal(hosts)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func unmarshalNFSHosts(raw string) ([]string, error) {
	if raw == "" {
		return []string{}, nil
	}
	var hosts []string
	if err := json.Unmarshal([]byte(raw), &hosts); err != nil {
		return nil, err
	}
	if hosts == nil {
		hosts = []string{}
	}
	return hosts, nil
}

func nfsSquashOrDefault(s string) string {
	if s == "" {
		return "root_squash"
	}
	return s
}

func isUniqueConstraint(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint")
}
