package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	storedb "github.com/mdg-labs/hoserva/internal/store/db"
)

// HostConfig is one Q76 onboarding choice persisted so a later generate
// can take ownership (import) or so Generator never writes the file
// (leave). facts is JSON of the parsed host file or Docker inventory.
type HostConfig struct {
	Kind      string
	Decision  string
	Facts     string
	AppliedAt time.Time
}

// HostConfigStore persists Q76 apply choices in the central database.
type HostConfigStore struct {
	q *storedb.Queries
}

// NewHostConfigStore wraps db for host-config persistence.
func NewHostConfigStore(db *sql.DB) *HostConfigStore {
	return &HostConfigStore{q: storedb.New(db)}
}

// Put upserts one category's decision and parsed facts.
func (s *HostConfigStore) Put(ctx context.Context, rec HostConfig) error {
	if rec.AppliedAt.IsZero() {
		rec.AppliedAt = time.Now().UTC()
	}
	if err := s.q.UpsertHostConfig(ctx, storedb.UpsertHostConfigParams{
		Kind:      rec.Kind,
		Decision:  rec.Decision,
		Facts:     rec.Facts,
		AppliedAt: rec.AppliedAt.UTC().Format(TimeFormat),
	}); err != nil {
		return fmt.Errorf("store: upserting host_config %s: %w", rec.Kind, err)
	}
	return nil
}

// Get returns the persisted row for kind, or sql.ErrNoRows.
func (s *HostConfigStore) Get(ctx context.Context, kind string) (HostConfig, error) {
	row, err := s.q.GetHostConfig(ctx, kind)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return HostConfig{}, err
		}
		return HostConfig{}, fmt.Errorf("store: getting host_config %s: %w", kind, err)
	}
	return hostConfigFromRow(row)
}

// List returns every persisted Q76 choice, ordered by kind.
func (s *HostConfigStore) List(ctx context.Context) ([]HostConfig, error) {
	rows, err := s.q.ListHostConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: listing host_config: %w", err)
	}
	out := make([]HostConfig, 0, len(rows))
	for _, row := range rows {
		rec, err := hostConfigFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

func hostConfigFromRow(row *storedb.HostConfig) (HostConfig, error) {
	applied, err := time.Parse(TimeFormat, row.AppliedAt)
	if err != nil {
		return HostConfig{}, fmt.Errorf("store: parsing host_config applied_at: %w", err)
	}
	return HostConfig{
		Kind:      row.Kind,
		Decision:  row.Decision,
		Facts:     row.Facts,
		AppliedAt: applied,
	}, nil
}
