package cache

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// timeFormat matches internal/store.TimeFormat (RFC3339) — kept local so
// this package does not depend on store just for a timestamp layout.
const timeFormat = time.RFC3339

// ErrNoMoverRun is LastRun's (and CacheUsage's) result before any mover
// job has persisted a report — GET /mover/last-run and GET /cache/usage
// then return null (#273).
var ErrNoMoverRun = errors.New("cache: no mover run has been persisted yet")

// SkippedEntry is one file the mover chose not to move (or could not),
// with why — the API's MoverSkippedEntry shape.
type SkippedEntry struct {
	Share  string `json:"share"`
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes,omitempty"`
	Result Result `json:"result"`
	Reason string `json:"reason,omitempty"`
}

// PersistedRun is one finished mover run's structured result (#273).
type PersistedRun struct {
	StartedAt   time.Time
	FinishedAt  time.Time
	DurationMs  int64
	FilesMoved  int
	BytesMoved  int64
	Interrupted bool
	Skipped     []SkippedEntry
}

// UsageBreakdown is the cache page's appdata / pending / other totals
// (doc 03 §3.6), computed as a by-product of a mover run (Q87).
type UsageBreakdown struct {
	AppdataBytes      int64
	PendingMovesBytes int64
	OtherBytes        int64
	ComputedAt        time.Time
}

// UsageShare is one share's cache-side directory and mode, for the
// post-run usage breakdown.
type UsageShare struct {
	Name string
	Path string
	Mode string // pool.CacheThenMove / CacheOnly / ArrayOnly string values
}

// ResultStore persists mover_run_result and cache_usage_breakdown in the
// central SQLite database (D4). Queries are hand-written here rather
// than through sqlc so this package stays the sole owner of the write
// path the mover job uses (#273).
type ResultStore struct {
	db *sql.DB
}

// NewResultStore wraps db for mover-run and cache-usage persistence.
func NewResultStore(db *sql.DB) *ResultStore {
	return &ResultStore{db: db}
}

// SaveFromReport upserts the structured run result from report, and —
// when cacheMount is non-empty — recomputes and upserts the cache usage
// breakdown from shares (Q87). A zero StartedAt is a no-op: the mover
// never started, so there is nothing honest to persist.
func (s *ResultStore) SaveFromReport(ctx context.Context, report Report, cacheMount string, shares []UsageShare) error {
	if report.StartedAt.IsZero() {
		return nil
	}
	run := PersistedRunFromReport(report)
	usage, err := ComputeUsageBreakdown(cacheMount, shares, report.FinishedAt)
	if err != nil {
		return fmt.Errorf("cache: computing usage breakdown: %w", err)
	}
	return s.save(ctx, run, usage)
}

// PersistedRunFromReport maps a cache.Report onto the API/persistence
// shape: moved counts, duration, and every skipped entry with reason.
func PersistedRunFromReport(report Report) PersistedRun {
	finished := report.FinishedAt
	if finished.IsZero() {
		finished = report.StartedAt
	}
	dur := finished.Sub(report.StartedAt)
	if dur < 0 {
		dur = 0
	}
	skipped := make([]SkippedEntry, 0, len(report.Skipped()))
	for _, e := range report.Skipped() {
		reason := e.Reason
		if reason == "" {
			reason = e.Err
		}
		skipped = append(skipped, SkippedEntry{
			Share:  e.Share,
			Path:   e.Path,
			Bytes:  e.Bytes,
			Result: e.Result,
			Reason: reason,
		})
	}
	return PersistedRun{
		StartedAt:   report.StartedAt.UTC(),
		FinishedAt:  finished.UTC(),
		DurationMs:  dur.Milliseconds(),
		FilesMoved:  len(report.Moved()),
		BytesMoved:  report.MovedBytes(),
		Interrupted: report.Interrupted,
		Skipped:     skipped,
	}
}

func (s *ResultStore) save(ctx context.Context, run PersistedRun, usage *UsageBreakdown) error {
	skippedJSON, err := json.Marshal(run.Skipped)
	if err != nil {
		return fmt.Errorf("cache: encoding skipped entries: %w", err)
	}
	if skippedJSON == nil {
		skippedJSON = []byte("[]")
	}
	interrupted := int64(0)
	if run.Interrupted {
		interrupted = 1
	}
	now := time.Now().UTC().Format(timeFormat)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("cache: beginning mover result transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
INSERT INTO mover_run_result (
    id, started_at, finished_at, duration_ms, files_moved, bytes_moved,
    interrupted, skipped_json, updated_at
) VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    started_at = excluded.started_at,
    finished_at = excluded.finished_at,
    duration_ms = excluded.duration_ms,
    files_moved = excluded.files_moved,
    bytes_moved = excluded.bytes_moved,
    interrupted = excluded.interrupted,
    skipped_json = excluded.skipped_json,
    updated_at = excluded.updated_at
`, run.StartedAt.Format(timeFormat), run.FinishedAt.Format(timeFormat),
		run.DurationMs, run.FilesMoved, run.BytesMoved, interrupted,
		string(skippedJSON), now)
	if err != nil {
		return fmt.Errorf("cache: upserting mover run result: %w", err)
	}

	if usage != nil {
		_, err = tx.ExecContext(ctx, `
INSERT INTO cache_usage_breakdown (
    id, appdata_bytes, pending_moves_bytes, other_bytes, computed_at
) VALUES (1, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    appdata_bytes = excluded.appdata_bytes,
    pending_moves_bytes = excluded.pending_moves_bytes,
    other_bytes = excluded.other_bytes,
    computed_at = excluded.computed_at
`, usage.AppdataBytes, usage.PendingMovesBytes, usage.OtherBytes,
			usage.ComputedAt.UTC().Format(timeFormat))
		if err != nil {
			return fmt.Errorf("cache: upserting cache usage breakdown: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("cache: committing mover result: %w", err)
	}
	return nil
}

// LastRun returns the most recent persisted mover run, or ErrNoMoverRun
// when none has been written yet.
func (s *ResultStore) LastRun(ctx context.Context) (PersistedRun, error) {
	var startedAt, finishedAt, skippedJSON string
	var durationMs, filesMoved, bytesMoved, interrupted int64
	err := s.db.QueryRowContext(ctx, `
SELECT started_at, finished_at, duration_ms, files_moved, bytes_moved,
       interrupted, skipped_json
FROM mover_run_result WHERE id = 1
`).Scan(&startedAt, &finishedAt, &durationMs, &filesMoved, &bytesMoved,
		&interrupted, &skippedJSON)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PersistedRun{}, ErrNoMoverRun
		}
		return PersistedRun{}, fmt.Errorf("cache: reading mover run result: %w", err)
	}
	started, err := time.Parse(timeFormat, startedAt)
	if err != nil {
		return PersistedRun{}, fmt.Errorf("cache: parsing mover run started_at: %w", err)
	}
	finished, err := time.Parse(timeFormat, finishedAt)
	if err != nil {
		return PersistedRun{}, fmt.Errorf("cache: parsing mover run finished_at: %w", err)
	}
	var skipped []SkippedEntry
	if err := json.Unmarshal([]byte(skippedJSON), &skipped); err != nil {
		return PersistedRun{}, fmt.Errorf("cache: decoding skipped entries: %w", err)
	}
	if skipped == nil {
		skipped = []SkippedEntry{}
	}
	return PersistedRun{
		StartedAt:   started,
		FinishedAt:  finished,
		DurationMs:  durationMs,
		FilesMoved:  int(filesMoved),
		BytesMoved:  bytesMoved,
		Interrupted: interrupted != 0,
		Skipped:     skipped,
	}, nil
}

// CacheUsage returns the latest persisted breakdown, or ErrNoMoverRun
// when none has been written yet.
func (s *ResultStore) CacheUsage(ctx context.Context) (UsageBreakdown, error) {
	var appdata, pending, other int64
	var computedAt string
	err := s.db.QueryRowContext(ctx, `
SELECT appdata_bytes, pending_moves_bytes, other_bytes, computed_at
FROM cache_usage_breakdown WHERE id = 1
`).Scan(&appdata, &pending, &other, &computedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return UsageBreakdown{}, ErrNoMoverRun
		}
		return UsageBreakdown{}, fmt.Errorf("cache: reading cache usage breakdown: %w", err)
	}
	at, err := time.Parse(timeFormat, computedAt)
	if err != nil {
		return UsageBreakdown{}, fmt.Errorf("cache: parsing cache usage computed_at: %w", err)
	}
	return UsageBreakdown{
		AppdataBytes:      appdata,
		PendingMovesBytes: pending,
		OtherBytes:        other,
		ComputedAt:        at,
	}, nil
}
