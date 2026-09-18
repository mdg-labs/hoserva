// Package metrics is metrics.db (#110, Q74, doc 02 §4): SMART attributes,
// temperatures, throughput, CPU and RAM history, kept in a database
// separate from the central store (D4) because losing it loses graphs,
// never configuration — an unbounded history table in the central
// database would break doc 10 §1's single-digit-MB config backup and wear
// the boot SSD, and metrics.db is therefore excluded from config backups
// entirely (doc 10 §1: "Not included: metrics.db and job logs — history,
// not configuration"). It uses its own minimal, disposable schema created
// with plain CREATE TABLE IF NOT EXISTS rather than the central store's
// generated migrations (internal/store/schema, D16): D16's migration
// discipline exists to protect data a schema mistake can't get back —
// metrics.db carries no such data, so a corrupt or lost file is simply
// recreated empty.
package metrics

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Resolution names one of the three retention tiers Q74 defines.
type Resolution string

const (
	Raw    Resolution = "raw"
	Hourly Resolution = "hourly"
	Daily  Resolution = "daily"
)

// Retention periods, per Q74's default: "raw samples for 48 hours, hourly
// for 90 days, daily for two years".
const (
	RawRetention    = 48 * time.Hour
	HourlyRetention = 90 * 24 * time.Hour
	DailyRetention  = 2 * 365 * 24 * time.Hour
)

const hourSeconds = int64(time.Hour / time.Second)
const daySeconds = int64(24 * time.Hour / time.Second)

const schemaSQL = `
CREATE TABLE IF NOT EXISTS samples (
    resolution TEXT NOT NULL,
    metric TEXT NOT NULL,
    subject TEXT NOT NULL,
    at INTEGER NOT NULL,
    value REAL NOT NULL,
    PRIMARY KEY (resolution, metric, subject, at)
) STRICT;
CREATE INDEX IF NOT EXISTS samples_resolution_at_idx ON samples (resolution, at);
`

// Store is a handle on one metrics.db.
type Store struct {
	db *sql.DB
}

// Open opens (creating and schema-initializing if needed) the metrics
// database at path. dsn mirrors cmd/hoservad's own production database:
// WAL mode plus a busy timeout, so a concurrent SMART poll and a
// Downsample run don't see SQLITE_BUSY the instant they overlap.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("metrics: opening %s: %w", path, err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("metrics: creating schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

// Sample is one raw observation. Subject is the device path for a
// per-disk metric (SMART attributes, temperature) and empty for a
// host-wide one (CPU, RAM, doc 02 §4).
type Sample struct {
	Metric  string
	Subject string
	At      time.Time
	Value   float64
}

// Insert records one raw sample. A second Insert for the same
// metric/subject/second overwrites the first rather than erroring —
// polls are idempotent by nature (doc 02 §4's periodic SMART poll), and
// the primary key already guarantees at most one raw row per second.
func (s *Store) Insert(ctx context.Context, sample Sample) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO samples (resolution, metric, subject, at, value) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (resolution, metric, subject, at) DO UPDATE SET value = excluded.value`,
		Raw, sample.Metric, sample.Subject, sample.At.UTC().Unix(), sample.Value,
	); err != nil {
		return fmt.Errorf("metrics: inserting %s/%s sample: %w", sample.Metric, sample.Subject, err)
	}
	return nil
}

// Downsample rolls up raw samples older than RawRetention into hourly
// averages, hourly samples older than HourlyRetention into daily
// averages, and deletes daily samples older than DailyRetention (Q74).
// It is idempotent and safe to call repeatedly — a maintenance step
// (doc 01 §4), never a job of its own, since it touches only this
// boot-SSD database and never a data disk.
func (s *Store) Downsample(ctx context.Context, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("metrics: beginning downsample transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := rollUp(ctx, tx, Raw, Hourly, hourSeconds, now.Add(-RawRetention).Unix()); err != nil {
		return err
	}
	if err := rollUp(ctx, tx, Hourly, Daily, daySeconds, now.Add(-HourlyRetention).Unix()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM samples WHERE resolution = ? AND at < ?`,
		Daily, now.Add(-DailyRetention).Unix()); err != nil {
		return fmt.Errorf("metrics: pruning daily samples: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("metrics: committing downsample: %w", err)
	}
	return nil
}

// rollUp averages every from-resolution bucket of bucketSeconds width
// that has fully ended by cutoff into one to-resolution sample, then
// deletes the from-resolution rows it just summarized. Gating on the
// bucket's own end (bucket+bucketSeconds <= cutoff), rather than each
// row's own age, guarantees a bucket is aggregated exactly once: new
// samples always land in the current, still-open bucket, never a closed
// one, so nothing arrives after a bucket has been rolled up and deleted.
func rollUp(ctx context.Context, tx *sql.Tx, from, to Resolution, bucketSeconds, cutoff int64) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT metric, subject, (at / ?) * ? AS bucket, AVG(value)
		FROM samples
		WHERE resolution = ?
		GROUP BY metric, subject, bucket
		HAVING bucket + ? <= ?`,
		bucketSeconds, bucketSeconds, from, bucketSeconds, cutoff)
	if err != nil {
		return fmt.Errorf("metrics: selecting %s buckets: %w", from, err)
	}

	type bucket struct {
		metric, subject string
		at              int64
		value           float64
	}
	var buckets []bucket
	for rows.Next() {
		var b bucket
		if err := rows.Scan(&b.metric, &b.subject, &b.at, &b.value); err != nil {
			_ = rows.Close()
			return fmt.Errorf("metrics: scanning %s bucket: %w", from, err)
		}
		buckets = append(buckets, b)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("metrics: reading %s buckets: %w", from, err)
	}
	_ = rows.Close()

	for _, b := range buckets {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO samples (resolution, metric, subject, at, value) VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT (resolution, metric, subject, at) DO UPDATE SET value = excluded.value`,
			to, b.metric, b.subject, b.at, b.value,
		); err != nil {
			return fmt.Errorf("metrics: writing %s sample: %w", to, err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM samples WHERE resolution = ? AND metric = ? AND subject = ? AND at >= ? AND at < ?`,
			from, b.metric, b.subject, b.at, b.at+bucketSeconds,
		); err != nil {
			return fmt.Errorf("metrics: pruning rolled-up %s samples: %w", from, err)
		}
	}
	return nil
}

// Count reports how many samples exist at resolution — a test seam for
// Downsample, not a query product code calls.
func (s *Store) Count(ctx context.Context, resolution Resolution) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM samples WHERE resolution = ?`, resolution).Scan(&n); err != nil {
		return 0, fmt.Errorf("metrics: counting %s samples: %w", resolution, err)
	}
	return n, nil
}

// Values returns metric/subject's samples at resolution, oldest first —
// used by tests to check rollup arithmetic, and by the eventual history
// API (doc 03 §3.4) to read a graph's data.
func (s *Store) Values(ctx context.Context, resolution Resolution, metric, subject string) ([]Sample, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT at, value FROM samples WHERE resolution = ? AND metric = ? AND subject = ? ORDER BY at`,
		resolution, metric, subject)
	if err != nil {
		return nil, fmt.Errorf("metrics: reading %s/%s at %s: %w", metric, subject, resolution, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Sample
	for rows.Next() {
		var at int64
		var value float64
		if err := rows.Scan(&at, &value); err != nil {
			return nil, fmt.Errorf("metrics: scanning %s/%s at %s: %w", metric, subject, resolution, err)
		}
		out = append(out, Sample{Metric: metric, Subject: subject, At: time.Unix(at, 0).UTC(), Value: value})
	}
	return out, rows.Err()
}
