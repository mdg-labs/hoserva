package store

import (
	"context"
	"database/sql"
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
	CreatedAt             time.Time
	UpdatedAt             time.Time
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
	err := s.q.InsertShare(ctx, storedb.InsertShareParams{
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
		CreatedAt:             rec.CreatedAt.UTC().Format(TimeFormat),
		UpdatedAt:             rec.UpdatedAt.UTC().Format(TimeFormat),
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
		UpdatedAt:             rec.UpdatedAt.UTC().Format(TimeFormat),
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

// Delete removes the share definition. It refuses (ErrShareNotFound) a
// missing name. It never touches files on disk.
func (s *ShareStore) Delete(ctx context.Context, name string) error {
	n, err := s.q.DeleteShare(ctx, name)
	if err != nil {
		return fmt.Errorf("store: deleting share %s: %w", name, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrShareNotFound, name)
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
		CreatedAt:             createdAt,
		UpdatedAt:             updatedAt,
	}, nil
}

func isUniqueConstraint(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint")
}
