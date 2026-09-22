package parity

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/mdg-labs/hoserva/internal/store"
	storedb "github.com/mdg-labs/hoserva/internal/store/db"
)

// RelocationManifestStore persists Q15's own current relocation manifest
// and doc 09 §4's removing-disks set in the central SQLite database (D4)
// through the sqlc-generated internal/store/db package. It has no
// business logic of its own — deciding what belongs in the manifest is a
// relocation job's job (the mover, rebalance, evacuation or share
// relocation, doc 09 §2-§4); this store only persists what one of them
// already decided, and lets RunParityDiff and RunSync
// (internal/job/parity_run.go) read it back at the wiring boundary. Empty
// (both tables, the state right after NewRelocationManifestStore against
// a fresh database, or after Replace(ctx, nil, nil)) means no relocation
// is currently in progress — Current then returns nil, nil, matching
// Guard.Evaluate's own pre-#194 nil-manifest behaviour exactly.
type RelocationManifestStore struct {
	db *sql.DB
	q  *storedb.Queries
}

// NewRelocationManifestStore wraps db for relocation-manifest persistence.
func NewRelocationManifestStore(db *sql.DB) *RelocationManifestStore {
	return &RelocationManifestStore{db: db, q: storedb.New(db)}
}

// Replace atomically clears both relocation_manifest and
// relocation_removing_disks and writes manifest and removingDisks in
// their place — the write side a relocation job calls as it copies and
// verifies files (doc 09 §3-4), or clears entirely (both nil/empty) once
// its own trailing sync has finished accounting for them.
func (s *RelocationManifestStore) Replace(ctx context.Context, manifest []ManifestEntry, removingDisks map[string]bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("parity: beginning relocation manifest transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	q := s.q.WithTx(tx)
	if err := q.DeleteRelocationManifest(ctx); err != nil {
		return fmt.Errorf("parity: clearing relocation manifest: %w", err)
	}
	for _, m := range manifest {
		if err := q.InsertRelocationManifestEntry(ctx, storedb.InsertRelocationManifestEntryParams{
			RelPath:    m.RelPath,
			Size:       m.Size,
			Mtime:      m.MTime.UTC().Format(store.TimeFormat),
			SourceDisk: m.SourceDisk,
			TargetDisk: m.TargetDisk,
		}); err != nil {
			return fmt.Errorf("parity: inserting relocation manifest entry for %s: %w", m.RelPath, err)
		}
	}

	if err := q.DeleteRelocationRemovingDisks(ctx); err != nil {
		return fmt.Errorf("parity: clearing relocation removing disks: %w", err)
	}
	for disk, removing := range removingDisks {
		if !removing {
			continue
		}
		if err := q.InsertRelocationRemovingDisk(ctx, disk); err != nil {
			return fmt.Errorf("parity: inserting relocation removing disk %s: %w", disk, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("parity: committing relocation manifest: %w", err)
	}
	return nil
}

// Current returns the persisted relocation manifest and removing-disks
// set, exactly as Replace last wrote them — nil, nil when no relocation
// is in progress. RunParityDiff and RunSync (internal/job/parity_run.go)
// pass these straight into Guard.Evaluate/SyncOpts without altering them;
// matching a manifest against a diff is the guard's job alone (D1).
func (s *RelocationManifestStore) Current(ctx context.Context) ([]ManifestEntry, map[string]bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("parity: beginning relocation manifest read transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := s.q.WithTx(tx)

	rows, err := q.ListRelocationManifest(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("parity: listing relocation manifest: %w", err)
	}
	var manifest []ManifestEntry
	for _, r := range rows {
		mtime, err := time.Parse(store.TimeFormat, r.Mtime)
		if err != nil {
			return nil, nil, fmt.Errorf("parity: parsing relocation manifest mtime for %s: %w", r.RelPath, err)
		}
		manifest = append(manifest, ManifestEntry{
			RelPath:    r.RelPath,
			Size:       r.Size,
			MTime:      mtime,
			SourceDisk: r.SourceDisk,
			TargetDisk: r.TargetDisk,
		})
	}

	diskRows, err := q.ListRelocationRemovingDisks(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("parity: listing relocation removing disks: %w", err)
	}
	var removingDisks map[string]bool
	if len(diskRows) > 0 {
		removingDisks = make(map[string]bool, len(diskRows))
		for _, mount := range diskRows {
			removingDisks[mount] = true
		}
	}

	return manifest, removingDisks, nil
}
