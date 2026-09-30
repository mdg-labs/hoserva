package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/store"
	storedb "github.com/mdg-labs/hoserva/internal/store/db"
)

// BackupDestinationStore persists backup destinations (#60, D4) through
// the sqlc-generated internal/store/db package, and implements
// backup.DestinationStore. It has no business logic: backup.Service owns
// validation, sealing and staleness.
type BackupDestinationStore struct {
	db storedb.DBTX
	q  *storedb.Queries
}

// NewBackupDestinationStore wraps db for destination persistence.
func NewBackupDestinationStore(db storedb.DBTX) *BackupDestinationStore {
	return &BackupDestinationStore{db: db, q: storedb.New(db)}
}

var _ backup.DestinationStore = (*BackupDestinationStore)(nil)

func destinationFromRow(row *storedb.BackupDestination) (backup.Destination, error) {
	d := backup.Destination{
		ID:            row.ID,
		Name:          row.Name,
		Type:          backup.DestinationType(row.Type),
		Path:          row.Path,
		Enabled:       row.Enabled != 0,
		Encrypt:       row.Encrypt != 0,
		SealedSecrets: row.Secrets,
		Retention: backup.Retention{
			Daily:   int(row.RetentionDaily),
			Weekly:  int(row.RetentionWeekly),
			Monthly: int(row.RetentionMonthly),
		},
	}
	if err := json.Unmarshal([]byte(row.Options), &d.Options); err != nil {
		return backup.Destination{}, fmt.Errorf("decoding options of destination %q: %w", row.ID, err)
	}
	created, err := time.Parse(timeFormat, row.CreatedAt)
	if err != nil {
		return backup.Destination{}, fmt.Errorf("parsing creation time of destination %q: %w", row.ID, err)
	}
	d.CreatedAt = created
	if row.LastSuccessfulBackupAt.Valid {
		t, err := time.Parse(timeFormat, row.LastSuccessfulBackupAt.String)
		if err != nil {
			return backup.Destination{}, fmt.Errorf("parsing last backup time of destination %q: %w", row.ID, err)
		}
		d.LastSuccessfulBackupAt = &t
	}
	if row.StaleAlertedAt.Valid {
		t, err := time.Parse(timeFormat, row.StaleAlertedAt.String)
		if err != nil {
			return backup.Destination{}, fmt.Errorf("parsing stale-alert time of destination %q: %w", row.ID, err)
		}
		d.StaleAlertedAt = &t
	}
	return d, nil
}

// ListDestinations implements backup.DestinationStore.
func (s *BackupDestinationStore) ListDestinations(ctx context.Context) ([]backup.Destination, error) {
	rows, err := s.q.ListBackupDestinations(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing backup destinations: %w", err)
	}
	out := make([]backup.Destination, 0, len(rows))
	for _, row := range rows {
		d, err := destinationFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// GetDestination implements backup.DestinationStore.
func (s *BackupDestinationStore) GetDestination(ctx context.Context, id string) (backup.Destination, error) {
	row, err := s.q.GetBackupDestination(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return backup.Destination{}, backup.ErrDestinationNotFound
		}
		return backup.Destination{}, fmt.Errorf("reading backup destination %q: %w", id, err)
	}
	return destinationFromRow(row)
}

func createParams(d backup.Destination) (storedb.CreateBackupDestinationParams, error) {
	options, err := json.Marshal(d.Options)
	if err != nil {
		return storedb.CreateBackupDestinationParams{}, fmt.Errorf("encoding options: %w", err)
	}
	if d.Options == nil {
		options = []byte("{}")
	}
	secrets := d.SealedSecrets
	if secrets == nil {
		secrets = []byte{}
	}
	typ := d.Type
	if typ == "" {
		typ = backup.TypeLocal
	}
	return storedb.CreateBackupDestinationParams{
		ID:               d.ID,
		Name:             d.Name,
		Type:             string(typ),
		Path:             d.Path,
		Options:          string(options),
		Secrets:          secrets,
		Enabled:          boolToSQL(d.Enabled),
		Encrypt:          boolToSQL(d.Encrypt),
		RetentionDaily:   int64(d.Retention.Daily),
		RetentionWeekly:  int64(d.Retention.Weekly),
		RetentionMonthly: int64(d.Retention.Monthly),
		CreatedAt:        d.CreatedAt.UTC().Format(timeFormat),
	}, nil
}

// CreateDestination implements backup.DestinationStore.
func (s *BackupDestinationStore) CreateDestination(ctx context.Context, d backup.Destination) error {
	params, err := createParams(d)
	if err != nil {
		return err
	}
	if err := s.q.CreateBackupDestination(ctx, params); err != nil {
		return fmt.Errorf("persisting backup destination %q: %w", d.ID, err)
	}
	return nil
}

// SeedDestinations implements backup.DestinationStore: one transaction
// counts the rows and inserts every default, so a failure part way rolls
// them all back and the next start seeds the full set.
func (s *BackupDestinationStore) SeedDestinations(ctx context.Context, ds []backup.Destination) error {
	sqlDB, ok := s.db.(*sql.DB)
	if !ok {
		return errors.New("backup destination store: seeding requires *sql.DB")
	}
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning seed transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM backup_destinations`).Scan(&count); err != nil {
		return fmt.Errorf("counting backup destinations: %w", err)
	}
	if count > 0 {
		return nil
	}
	q := s.q.WithTx(tx)
	for _, d := range ds {
		params, err := createParams(d)
		if err != nil {
			return err
		}
		if err := q.CreateBackupDestination(ctx, params); err != nil {
			return fmt.Errorf("persisting backup destination %q: %w", d.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing seed transaction: %w", err)
	}
	return nil
}

// DeleteDestination implements backup.DestinationStore. An external
// disk's destination is deleted together with that disk's flag, in one
// transaction, so neither can be left claiming the other.
func (s *BackupDestinationStore) DeleteDestination(ctx context.Context, id string) error {
	if label, ok := strings.CutPrefix(id, externalDestinationIDPrefix); ok {
		return s.deleteExternalDestination(ctx, id, label)
	}
	n, err := s.q.DeleteBackupDestination(ctx, id)
	if err != nil {
		return fmt.Errorf("deleting backup destination %q: %w", id, err)
	}
	if n == 0 {
		return backup.ErrDestinationNotFound
	}
	return nil
}

const externalDestinationIDPrefix = "external:"

var _ backup.ExternalDestinationStore = (*BackupDestinationStore)(nil)

func (s *BackupDestinationStore) externalStore() (*store.ExternalStore, error) {
	sqlDB, ok := s.db.(*sql.DB)
	if !ok {
		return nil, errors.New("backup destination store: external disks require *sql.DB")
	}
	return store.NewExternalStore(sqlDB), nil
}

func (s *BackupDestinationStore) deleteExternalDestination(ctx context.Context, id, label string) error {
	ext, err := s.externalStore()
	if err != nil {
		return err
	}
	return ext.InTx(ctx, func(ext *store.ExternalStore, q *storedb.Queries) error {
		n, err := q.DeleteBackupDestination(ctx, id)
		if err != nil {
			return fmt.Errorf("deleting backup destination %q: %w", id, err)
		}
		if n == 0 {
			return backup.ErrDestinationNotFound
		}
		if err := ext.SetBackupDestination(ctx, label, false); err != nil && !errors.Is(err, store.ErrExternalNotFound) {
			return err
		}
		return nil
	})
}

// SetExternalDestination implements backup.ExternalDestinationStore.
func (s *BackupDestinationStore) SetExternalDestination(ctx context.Context, label string, dest *backup.Destination) error {
	ext, err := s.externalStore()
	if err != nil {
		return err
	}
	return ext.InTx(ctx, func(ext *store.ExternalStore, q *storedb.Queries) error {
		if err := ext.SetBackupDestination(ctx, label, dest != nil); err != nil {
			return err
		}
		if dest == nil {
			if _, err := q.DeleteBackupDestination(ctx, externalDestinationIDPrefix+label); err != nil {
				return fmt.Errorf("deleting backup destination of external disk %q: %w", label, err)
			}
			return nil
		}
		return createIfAbsent(ctx, q, *dest)
	})
}

// PutExternalDisk implements backup.ExternalDestinationStore.
func (s *BackupDestinationStore) PutExternalDisk(ctx context.Context, d store.ExternalDisk, dest *backup.Destination) error {
	ext, err := s.externalStore()
	if err != nil {
		return err
	}
	return ext.InTx(ctx, func(ext *store.ExternalStore, q *storedb.Queries) error {
		if err := ext.PutExternalDisk(ctx, d); err != nil {
			return err
		}
		if dest == nil {
			return nil
		}
		return createIfAbsent(ctx, q, *dest)
	})
}

// FlaggedExternalLabels implements backup.ExternalDestinationStore.
func (s *BackupDestinationStore) FlaggedExternalLabels(ctx context.Context) ([]string, error) {
	ext, err := s.externalStore()
	if err != nil {
		return nil, err
	}
	disks, err := ext.ListExternalDisks(ctx)
	if err != nil {
		return nil, err
	}
	var labels []string
	for _, d := range disks {
		if d.BackupDestination {
			labels = append(labels, d.Label)
		}
	}
	return labels, nil
}

func createIfAbsent(ctx context.Context, q *storedb.Queries, d backup.Destination) error {
	if _, err := q.GetBackupDestination(ctx, d.ID); err == nil {
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("reading backup destination %q: %w", d.ID, err)
	}
	params, err := createParams(d)
	if err != nil {
		return err
	}
	if err := q.CreateBackupDestination(ctx, params); err != nil {
		return fmt.Errorf("persisting backup destination %q: %w", d.ID, err)
	}
	return nil
}

// RecordBackupSuccess implements backup.DestinationStore.
func (s *BackupDestinationStore) RecordBackupSuccess(ctx context.Context, id string, at time.Time) error {
	n, err := s.q.RecordBackupDestinationSuccess(ctx, storedb.RecordBackupDestinationSuccessParams{
		LastSuccessfulBackupAt: sql.NullString{String: at.UTC().Format(timeFormat), Valid: true},
		ID:                     id,
	})
	if err != nil {
		return fmt.Errorf("recording backup success for destination %q: %w", id, err)
	}
	if n == 0 {
		return backup.ErrDestinationNotFound
	}
	return nil
}

// MarkStaleAlerted implements backup.DestinationStore.
func (s *BackupDestinationStore) MarkStaleAlerted(ctx context.Context, id string, observed *time.Time, at time.Time) error {
	params := storedb.MarkBackupDestinationStaleAlertedParams{
		StaleAlertedAt: sql.NullString{String: at.UTC().Format(timeFormat), Valid: true},
		ID:             id,
	}
	if observed != nil {
		params.ObservedLastSuccessfulBackupAt = sql.NullString{String: observed.UTC().Format(timeFormat), Valid: true}
	}
	n, err := s.q.MarkBackupDestinationStaleAlerted(ctx, params)
	if err != nil {
		return fmt.Errorf("marking destination %q alerted: %w", id, err)
	}
	if n == 0 {
		return backup.ErrDestinationNotFound
	}
	return nil
}

// BackupDestinationSecrets lists every destination's sealed credentials as
// the database secrets a config backup re-encrypts under the backup
// passphrase (Q28).
func (s *BackupDestinationStore) BackupDestinationSecrets(ctx context.Context) ([]backup.DatabaseSecret, error) {
	dests, err := s.ListDestinations(ctx)
	if err != nil {
		return nil, err
	}
	var out []backup.DatabaseSecret
	for _, d := range dests {
		if len(d.SealedSecrets) == 0 {
			continue
		}
		out = append(out, backup.DatabaseSecret{
			Table:      "backup_destinations",
			Column:     "secrets",
			RowID:      d.ID,
			Ciphertext: d.SealedSecrets,
		})
	}
	return out, nil
}
