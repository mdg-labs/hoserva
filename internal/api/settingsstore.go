package api

import (
	"context"
	"database/sql"

	storedb "github.com/mdg-labs/hoserva/internal/store/db"
)

// GeneralSettingsRow is schema_info's installation-wide settings (doc 03
// §1, §8.1, D4): hostname, timezone and the machine-key-encrypted backup
// passphrase (Q28).
type GeneralSettingsRow struct {
	Hostname         sql.NullString
	Timezone         sql.NullString
	BackupPassphrase []byte
}

// SettingsStore reads and writes general settings through schema_info's
// singleton row (D4). It has no business logic of its own — SettingsService
// owns validation and encryption; SettingsStore only reads and writes rows.
type SettingsStore struct {
	q *storedb.Queries
}

// NewSettingsStore wraps db for general-settings persistence.
func NewSettingsStore(db storedb.DBTX) *SettingsStore {
	return &SettingsStore{q: storedb.New(db)}
}

// Get returns schema_info's row, or sql.ErrNoRows when the singleton has
// not been written yet.
func (s *SettingsStore) Get(ctx context.Context) (*GeneralSettingsRow, error) {
	row, err := s.q.GetSchemaMeta(ctx)
	if err != nil {
		return nil, err
	}
	return &GeneralSettingsRow{
		Hostname:         row.Hostname,
		Timezone:         row.Timezone,
		BackupPassphrase: row.BackupPassphrase,
	}, nil
}

// InsertMeta writes schema_info's one row with installationID and
// createdAt. Callers use this only when the singleton does not exist yet.
func (s *SettingsStore) InsertMeta(ctx context.Context, installationID, createdAt string) error {
	return s.q.InsertSchemaMeta(ctx, storedb.InsertSchemaMetaParams{
		InstallationID: installationID,
		CreatedAt:      createdAt,
	})
}

// Update replaces hostname, timezone and backup_passphrase on the
// singleton row. Every column is written — callers merge partial updates
// against the current row first.
func (s *SettingsStore) Update(ctx context.Context, row GeneralSettingsRow) error {
	return s.q.UpdateGeneralSettings(ctx, storedb.UpdateGeneralSettingsParams{
		Hostname:         row.Hostname,
		Timezone:         row.Timezone,
		BackupPassphrase: row.BackupPassphrase,
	})
}
