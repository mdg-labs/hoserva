package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	storedb "github.com/mdg-labs/hoserva/internal/store/db"
)

// The catalog's background refresh intervals (doc 04 §7, Q65).
const (
	CatalogIntervalOff = "off"
	CatalogInterval1h  = "1h"
	CatalogInterval6h  = "6h"
	CatalogInterval12h = "12h"
	CatalogInterval24h = "24h"
)

// ErrCatalogInterval is UpdateCatalogSettings's refusal of an interval that
// is not off, 1h, 6h, 12h or 24h.
var ErrCatalogInterval = errors.New("store: the catalog refresh interval must be one of off, 1h, 6h, 12h, 24h")

// CatalogSettings is how the catalog refreshes itself: a background check
// every RefreshInterval and, with CheckOnOpen, a check when the catalog is
// opened and the last one is stale.
type CatalogSettings struct {
	RefreshInterval string
	CheckOnOpen     bool
}

// DefaultCatalogSettings is what an install has until the settings are set.
var DefaultCatalogSettings = CatalogSettings{RefreshInterval: CatalogInterval24h, CheckOnOpen: true}

// CatalogSettingsUpdate is a partial change: a nil field stays as it is.
type CatalogSettingsUpdate struct {
	RefreshInterval *string
	CheckOnOpen     *bool
}

func validCatalogInterval(v string) bool {
	switch v {
	case CatalogIntervalOff, CatalogInterval1h, CatalogInterval6h, CatalogInterval12h, CatalogInterval24h:
		return true
	}
	return false
}

// CatalogSettingsStore persists the catalog refresh settings in
// schema_info's singleton row (D4).
type CatalogSettingsStore struct {
	q *storedb.Queries
}

// NewCatalogSettingsStore wraps db for catalog settings persistence.
func NewCatalogSettingsStore(db storedb.DBTX) *CatalogSettingsStore {
	return &CatalogSettingsStore{q: storedb.New(db)}
}

// CatalogSettings returns the saved settings, or the defaults while the
// installation row has not been written.
func (s *CatalogSettingsStore) CatalogSettings(ctx context.Context) (CatalogSettings, error) {
	row, err := s.q.GetCatalogSettings(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultCatalogSettings, nil
	}
	if err != nil {
		return CatalogSettings{}, fmt.Errorf("store: reading the catalog settings: %w", err)
	}
	return CatalogSettings{RefreshInterval: row.CatalogRefreshInterval, CheckOnOpen: row.CatalogCheckOnOpen != 0}, nil
}

// UpdateCatalogSettings applies u in one statement, so two partial updates
// never overwrite each other's field, and returns the settings now saved. An
// interval outside the allowed set is refused before anything is written.
// The installation row, which is otherwise written the first time a general
// setting is read, is created first when it does not exist yet.
func (s *CatalogSettingsStore) UpdateCatalogSettings(ctx context.Context, u CatalogSettingsUpdate) (CatalogSettings, error) {
	if u.RefreshInterval != nil && !validCatalogInterval(*u.RefreshInterval) {
		return CatalogSettings{}, ErrCatalogInterval
	}
	var params storedb.UpdateCatalogSettingsParams
	if u.RefreshInterval != nil {
		params.RefreshInterval = sql.NullString{String: *u.RefreshInterval, Valid: true}
	}
	if u.CheckOnOpen != nil {
		params.CheckOnOpen = sql.NullInt64{Int64: boolToInt(*u.CheckOnOpen), Valid: true}
	}
	if err := s.q.EnsureSchemaMeta(ctx, storedb.EnsureSchemaMetaParams{
		InstallationID: uuid.NewString(),
		CreatedAt:      time.Now().UTC().Format(TimeFormat),
	}); err != nil {
		return CatalogSettings{}, fmt.Errorf("store: creating the installation row for the catalog settings: %w", err)
	}
	if err := s.q.UpdateCatalogSettings(ctx, params); err != nil {
		return CatalogSettings{}, fmt.Errorf("store: saving the catalog settings: %w", err)
	}
	return s.CatalogSettings(ctx)
}
