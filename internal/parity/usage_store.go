package parity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/mdg-labs/hoserva/internal/store"
	storedb "github.com/mdg-labs/hoserva/internal/store/db"
)

// ErrUsageNeverComputed is UsageStore.ComputedAt's (and Get's) result
// before ComputeShareUsage has ever completed once — no sync has run
// since this daemon's database was created, so share_usage has never been
// written and there is nothing honest to report but "not yet synced"
// (doc 03 §4.2).
var ErrUsageNeverComputed = errors.New("parity: share usage has never been computed")

// UsageSnapshot is one share's persisted per-disk bytes, as of the sync
// that computed them.
type UsageSnapshot struct {
	Disks      map[string]int64 // mount point -> bytes.
	ComputedAt time.Time
}

// UsageStore persists share_usage and share_usage_computed_at in the
// central SQLite database (D4) through the sqlc-generated
// internal/store/db package. Replace is its only write, run once per
// successful sync by SnapraidEngine.ComputeShareUsage — never per
// request (doc 02 §1 line 78).
type UsageStore struct {
	db *sql.DB
	q  *storedb.Queries
}

// NewUsageStore wraps db for share-usage persistence.
func NewUsageStore(db *sql.DB) *UsageStore {
	return &UsageStore{db: db, q: storedb.New(db)}
}

// Replace atomically clears share_usage and writes rows, then records
// computedAt as the timestamp the whole table — including every share
// that now has no row at all — is "as of" (doc 03 §4.2). Clearing first
// means a disk a share no longer occupies has no row after this call,
// rather than a stale one left over from a previous sync.
func (s *UsageStore) Replace(ctx context.Context, rows []ShareUsage, computedAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("parity: beginning share usage transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	q := s.q.WithTx(tx)
	if err := q.DeleteShareUsage(ctx); err != nil {
		return fmt.Errorf("parity: clearing share usage: %w", err)
	}
	for _, r := range rows {
		if err := q.InsertShareUsage(ctx, storedb.InsertShareUsageParams{
			ShareName:      r.Share,
			DiskMountpoint: r.Disk,
			Bytes:          r.Bytes,
		}); err != nil {
			return fmt.Errorf("parity: inserting share usage for %s: %w", r.Share, err)
		}
	}
	if err := q.UpsertShareUsageComputedAt(ctx, computedAt.UTC().Format(store.TimeFormat)); err != nil {
		return fmt.Errorf("parity: recording share usage computed_at: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("parity: committing share usage: %w", err)
	}
	return nil
}

// ComputedAt returns the timestamp the current share_usage table
// reflects, or ErrUsageNeverComputed if ComputeShareUsage has never run.
func (s *UsageStore) ComputedAt(ctx context.Context) (time.Time, error) {
	return computedAt(ctx, s.q)
}

// computedAt is ComputedAt's own query, taking q so Get and ListAll can
// run it against a tx-bound *storedb.Queries and read the same snapshot
// their row query sees (below).
func computedAt(ctx context.Context, q *storedb.Queries) (time.Time, error) {
	row, err := q.GetShareUsageComputedAt(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, ErrUsageNeverComputed
		}
		return time.Time{}, err
	}
	t, err := time.Parse(store.TimeFormat, row)
	if err != nil {
		return time.Time{}, fmt.Errorf("parity: parsing share usage computed_at: %w", err)
	}
	return t, nil
}

// Get returns share's per-disk bytes as of the last computation. ok is
// false only when no computation has ever run (ErrUsageNeverComputed) —
// the caller's own "not yet synced" signal, distinct from a share that
// has been through a sync and genuinely holds zero bytes (an ok=true
// result with an empty Disks map).
func (s *UsageStore) Get(ctx context.Context, share string) (UsageSnapshot, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return UsageSnapshot{}, false, fmt.Errorf("parity: beginning share usage read transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := s.q.WithTx(tx)

	at, err := computedAt(ctx, q)
	if err != nil {
		if errors.Is(err, ErrUsageNeverComputed) {
			return UsageSnapshot{}, false, nil
		}
		return UsageSnapshot{}, false, err
	}
	rows, err := q.ListShareUsageByShare(ctx, share)
	if err != nil {
		return UsageSnapshot{}, false, fmt.Errorf("parity: listing share usage for %s: %w", share, err)
	}
	disks := make(map[string]int64, len(rows))
	for _, r := range rows {
		disks[r.DiskMountpoint] = r.Bytes
	}
	return UsageSnapshot{Disks: disks, ComputedAt: at}, true, nil
}

// ListAll returns the computed-at timestamp the whole table reflects,
// plus every currently persisted share's per-disk bytes in one query —
// what a share list (doc 03 §4.1) needs without one query per share. A
// share with no entry in byShare has never had a nonzero-byte file on any
// disk; it is still "as of computedAt", not "not yet synced" — the
// caller (internal/share.Service) decides that only by comparing
// computedAt against its own share's CreatedAt. ok is false only when no
// computation has ever run, and computedAt/byShare are meaningless then.
func (s *UsageStore) ListAll(ctx context.Context) (at time.Time, byShare map[string]map[string]int64, ok bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, nil, false, fmt.Errorf("parity: beginning share usage read transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := s.q.WithTx(tx)

	at, err = computedAt(ctx, q)
	if err != nil {
		if errors.Is(err, ErrUsageNeverComputed) {
			return time.Time{}, nil, false, nil
		}
		return time.Time{}, nil, false, err
	}
	rows, err := q.ListAllShareUsage(ctx)
	if err != nil {
		return time.Time{}, nil, false, fmt.Errorf("parity: listing share usage: %w", err)
	}
	out := map[string]map[string]int64{}
	for _, r := range rows {
		disks, ok := out[r.ShareName]
		if !ok {
			disks = map[string]int64{}
			out[r.ShareName] = disks
		}
		disks[r.DiskMountpoint] = r.Bytes
	}
	return at, out, true, nil
}
