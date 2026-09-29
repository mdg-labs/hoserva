package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mdg-labs/hoserva/internal/backup"
	storedb "github.com/mdg-labs/hoserva/internal/store/db"
)

// DrillStore persists the last restore drill's result (#63, D4) through the
// sqlc-generated internal/store/db package, and implements
// backup.DrillStore.
type DrillStore struct {
	q *storedb.Queries
}

// NewDrillStore wraps db for drill result persistence.
func NewDrillStore(db storedb.DBTX) *DrillStore {
	return &DrillStore{q: storedb.New(db)}
}

var _ backup.DrillStore = (*DrillStore)(nil)

type storedDrillDestination struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Archive string `json:"archive,omitempty"`
	Passed  bool   `json:"passed"`
	Error   string `json:"error,omitempty"`
}

// RecordDrill implements backup.DrillStore.
func (s *DrillStore) RecordDrill(ctx context.Context, r backup.DrillResult) error {
	dests := make([]storedDrillDestination, len(r.Destinations))
	for i, d := range r.Destinations {
		dests[i] = storedDrillDestination{ID: d.DestinationID, Name: d.DestinationName, Archive: d.Archive, Passed: d.Passed, Error: d.Error}
	}
	raw, err := json.Marshal(dests)
	if err != nil {
		return fmt.Errorf("encoding restore drill destinations: %w", err)
	}
	if err := s.q.UpsertRestoreDrillResult(ctx, storedb.UpsertRestoreDrillResultParams{
		RanAt:        r.RanAt.UTC().Format(timeFormat),
		Passed:       boolToInt(r.Passed),
		Error:        sql.NullString{String: r.Error, Valid: r.Error != ""},
		Destinations: string(raw),
	}); err != nil {
		return fmt.Errorf("storing restore drill result: %w", err)
	}
	return nil
}

// LastDrill implements backup.DrillStore.
func (s *DrillStore) LastDrill(ctx context.Context) (*backup.DrillResult, error) {
	row, err := s.q.GetRestoreDrillResult(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("loading restore drill result: %w", err)
	}
	ranAt, err := time.Parse(timeFormat, row.RanAt)
	if err != nil {
		return nil, fmt.Errorf("parsing restore drill time %q: %w", row.RanAt, err)
	}
	var stored []storedDrillDestination
	if err := json.Unmarshal([]byte(row.Destinations), &stored); err != nil {
		return nil, fmt.Errorf("decoding restore drill destinations: %w", err)
	}
	out := &backup.DrillResult{RanAt: ranAt, Passed: row.Passed != 0, Error: row.Error.String}
	for _, d := range stored {
		out.Destinations = append(out.Destinations, backup.DrillDestination{
			DestinationID: d.ID, DestinationName: d.Name, Archive: d.Archive, Passed: d.Passed, Error: d.Error,
		})
	}
	return out, nil
}
