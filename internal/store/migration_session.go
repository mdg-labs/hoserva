package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	storedb "github.com/mdg-labs/hoserva/internal/store/db"
)

// MigrationSession is the migration_session row (#75): the source of the latest
// finished scan with its report, and the scan that has not finished, if any.
// Report is the report's JSON, which the migrate package owns. A time that does
// not apply is the zero time.
type MigrationSession struct {
	SourceFile       string
	SourceSize       int64
	SourceReceivedAt time.Time
	Report           []byte

	ScanFile             string
	ScanSize             int64
	ScanReceivedAt       time.Time
	ScanUnverifiedLayout bool
	ScanFullChecksums    bool
	ScanError            string

	// Verify is the verify phase's result as JSON, which the migrate package
	// owns. Empty when no verify has run against the current baseline.
	Verify []byte
}

// MigrationSessionStore persists the one migration session in the central
// SQLite database (D4).
type MigrationSessionStore struct {
	q *storedb.Queries
}

// NewMigrationSessionStore wraps db for migration session persistence.
func NewMigrationSessionStore(db storedb.DBTX) *MigrationSessionStore {
	return &MigrationSessionStore{q: storedb.New(db)}
}

func formatOptionalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(TimeFormat)
}

func parseOptionalTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(TimeFormat, s)
}

// Get returns the session, and false when there is none.
func (s *MigrationSessionStore) Get(ctx context.Context) (MigrationSession, bool, error) {
	row, err := s.q.GetMigrationSession(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return MigrationSession{}, false, nil
	}
	if err != nil {
		return MigrationSession{}, false, fmt.Errorf("store: reading the migration session: %w", err)
	}
	received, err := parseOptionalTime(row.SourceReceivedAt)
	if err != nil {
		return MigrationSession{}, false, fmt.Errorf("store: parsing the migration source time %q: %w", row.SourceReceivedAt, err)
	}
	scanReceived, err := parseOptionalTime(row.ScanReceivedAt)
	if err != nil {
		return MigrationSession{}, false, fmt.Errorf("store: parsing the migration scan time %q: %w", row.ScanReceivedAt, err)
	}
	m := MigrationSession{
		SourceFile: row.SourceFile, SourceSize: row.SourceSize, SourceReceivedAt: received,
		ScanFile: row.ScanFile, ScanSize: row.ScanSize, ScanReceivedAt: scanReceived,
		ScanUnverifiedLayout: row.ScanUnverifiedLayout != 0, ScanFullChecksums: row.ScanFullChecksums != 0, ScanError: row.ScanError,
	}
	if row.Report != "" {
		m.Report = []byte(row.Report)
	}
	if row.Verify != "" {
		m.Verify = []byte(row.Verify)
	}
	return m, true, nil
}

// Put replaces the session with m, in one statement.
func (s *MigrationSessionStore) Put(ctx context.Context, m MigrationSession) error {
	err := s.q.UpsertMigrationSession(ctx, storedb.UpsertMigrationSessionParams{
		SourceFile:           m.SourceFile,
		SourceSize:           m.SourceSize,
		SourceReceivedAt:     formatOptionalTime(m.SourceReceivedAt),
		Report:               string(m.Report),
		ScanFile:             m.ScanFile,
		ScanSize:             m.ScanSize,
		ScanReceivedAt:       formatOptionalTime(m.ScanReceivedAt),
		ScanUnverifiedLayout: boolInt(m.ScanUnverifiedLayout),
		ScanError:            m.ScanError,
		ScanFullChecksums:    boolInt(m.ScanFullChecksums),
		Verify:               string(m.Verify),
	})
	if err != nil {
		return fmt.Errorf("store: saving the migration session: %w", err)
	}
	return nil
}

// Delete removes the session. Deleting nothing succeeds.
func (s *MigrationSessionStore) Delete(ctx context.Context) error {
	if err := s.q.DeleteMigrationSession(ctx); err != nil {
		return fmt.Errorf("store: deleting the migration session: %w", err)
	}
	return nil
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
