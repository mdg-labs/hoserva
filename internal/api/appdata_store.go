package api

import (
	"context"
	"fmt"
	"time"

	"github.com/mdg-labs/hoserva/internal/backup"
	storedb "github.com/mdg-labs/hoserva/internal/store/db"
)

// AppdataPolicyStore persists the per-container appdata backup policy
// (#61, D4) through the sqlc-generated internal/store/db package, and
// implements backup.AppdataPolicyStore.
type AppdataPolicyStore struct {
	q *storedb.Queries
}

// NewAppdataPolicyStore wraps db for policy persistence.
func NewAppdataPolicyStore(db storedb.DBTX) *AppdataPolicyStore {
	return &AppdataPolicyStore{q: storedb.New(db)}
}

var _ backup.AppdataPolicyStore = (*AppdataPolicyStore)(nil)

// ListAppdataPolicies implements backup.AppdataPolicyStore.
func (s *AppdataPolicyStore) ListAppdataPolicies(ctx context.Context) ([]backup.AppdataPolicy, error) {
	rows, err := s.q.ListAppdataBackupContainers(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing appdata backup policies: %w", err)
	}
	out := make([]backup.AppdataPolicy, len(rows))
	for i, r := range rows {
		out[i] = backup.AppdataPolicy{Container: r.Container, Stop: r.Stop != 0, Included: r.Included != 0}
	}
	return out, nil
}

// SetAppdataPolicy implements backup.AppdataPolicyStore.
func (s *AppdataPolicyStore) SetAppdataPolicy(ctx context.Context, p backup.AppdataPolicy, at time.Time) error {
	if err := s.q.UpsertAppdataBackupContainer(ctx, storedb.UpsertAppdataBackupContainerParams{
		Container: p.Container,
		Stop:      boolToInt(p.Stop),
		Included:  boolToInt(p.Included),
		UpdatedAt: at.UTC().Format(timeFormat),
	}); err != nil {
		return fmt.Errorf("storing appdata backup policy: %w", err)
	}
	return nil
}
