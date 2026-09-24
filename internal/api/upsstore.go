package api

import (
	"context"
	"database/sql"
	"errors"

	storedb "github.com/mdg-labs/hoserva/internal/store/db"
)

// UPSConfigRow is ups_config's singleton (doc 03 §8.1, Q77, Q28).
type UPSConfigRow struct {
	Connection        string
	Driver            string
	Port              string
	MonitorPassword   []byte
	NetworkHost       string
	NetworkPort       int64
	NetworkUPSName    string
	NetworkUsername   string
	NetworkPassword   []byte
	LowBatteryPercent int64
	RuntimeSeconds    int64
	UpdatedAt         string
}

// UPSStore reads and writes ups_config through the singleton row (D4).
type UPSStore struct {
	q *storedb.Queries
}

// NewUPSStore wraps db for UPS settings persistence.
func NewUPSStore(db storedb.DBTX) *UPSStore {
	return &UPSStore{q: storedb.New(db)}
}

// Get returns the singleton row, or sql.ErrNoRows when none is configured.
func (s *UPSStore) Get(ctx context.Context) (*UPSConfigRow, error) {
	row, err := s.q.GetUPSConfig(ctx)
	if err != nil {
		return nil, err
	}
	return &UPSConfigRow{
		Connection:        row.Connection,
		Driver:            row.Driver,
		Port:              row.Port,
		MonitorPassword:   row.MonitorPassword,
		NetworkHost:       row.NetworkHost,
		NetworkPort:       row.NetworkPort,
		NetworkUPSName:    row.NetworkUpsName,
		NetworkUsername:   row.NetworkUsername,
		NetworkPassword:   row.NetworkPassword,
		LowBatteryPercent: row.LowBatteryPercent,
		RuntimeSeconds:    row.RuntimeSeconds,
		UpdatedAt:         row.UpdatedAt,
	}, nil
}

// Upsert replaces the singleton row.
func (s *UPSStore) Upsert(ctx context.Context, row UPSConfigRow) error {
	return s.q.UpsertUPSConfig(ctx, storedb.UpsertUPSConfigParams{
		Connection:        row.Connection,
		Driver:            row.Driver,
		Port:              row.Port,
		MonitorPassword:   row.MonitorPassword,
		NetworkHost:       row.NetworkHost,
		NetworkPort:       row.NetworkPort,
		NetworkUpsName:    row.NetworkUPSName,
		NetworkUsername:   row.NetworkUsername,
		NetworkPassword:   row.NetworkPassword,
		LowBatteryPercent: row.LowBatteryPercent,
		RuntimeSeconds:    row.RuntimeSeconds,
		UpdatedAt:         row.UpdatedAt,
	})
}

// Delete removes the singleton row (used to roll back a first configure).
func (s *UPSStore) Delete(ctx context.Context) error {
	return s.q.DeleteUPSConfig(ctx)
}

// HasEncryptedSecrets reports whether any UPS password ciphertext exists.
func (s *UPSStore) HasEncryptedSecrets(ctx context.Context) (bool, error) {
	return s.q.HasEncryptedUPSSecrets(ctx)
}

// ListSecrets returns encrypted UPS password columns for config backup (Q28).
func (s *UPSStore) ListSecrets(ctx context.Context) ([]struct {
	Column     string
	Ciphertext []byte
}, error) {
	rows, err := s.q.ListUPSSecrets(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]struct {
		Column     string
		Ciphertext []byte
	}, 0, len(rows))
	for _, row := range rows {
		out = append(out, struct {
			Column     string
			Ciphertext []byte
		}{Column: row.Col, Ciphertext: row.Ciphertext})
	}
	return out, nil
}

// restoreUPSRow puts previous back, or deletes the row when previous is nil.
// It deliberately uses its own context so a cancelled request cannot leave
// the half-applied row behind (known-escapes: compensating delete).
func restoreUPSRow(store *UPSStore, previous *UPSConfigRow) error {
	ctx := context.Background()
	if previous == nil {
		if err := store.Delete(ctx); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return nil
	}
	row := *previous
	// SQLite scans an empty BLOB as nil; ups_config password columns are
	// NOT NULL, so a round-trip restore must write []byte{} not nil.
	if row.MonitorPassword == nil {
		row.MonitorPassword = []byte{}
	}
	if row.NetworkPassword == nil {
		row.NetworkPassword = []byte{}
	}
	return store.Upsert(ctx, row)
}
