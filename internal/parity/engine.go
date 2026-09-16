// Package parity defines the subsystem abstraction over SnapRAID (doc 01
// §4, doc 02 §2). Engine orchestrates SnapRAID's own sync/diff/scrub/status
// operations — it computes nothing itself (D1) — so the threshold guard and
// every caller above it can be tested against a scripted simulator instead
// of a real parity disk (doc 06 §2).
package parity

import (
	"context"
	"time"
)

// SyncOpts controls one sync run (doc 01 §3: `hoserva sync [--dry-run]
// [--force]`).
type SyncOpts struct {
	// DryRun runs the diff SnapRAID would sync against without writing
	// parity.
	DryRun bool
	// Force syncs even though the threshold guard would otherwise hold it
	// (doc 02 §2's "Review the diff and sync anyway"). The guard itself
	// lives above Engine; Engine only carries the caller's decision through.
	Force bool
}

// DiffReport is what `snapraid diff` reports before a sync: exactly what
// changed since the last one (doc 02 §2). It is what the threshold guard
// decides on.
type DiffReport struct {
	Added   int
	Removed int
	Updated int
	Moved   int
	Copied  int

	// MovedByHoserva is the subset of Removed that a relocation manifest
	// matches to a reappearance on its recorded target disk (Q15) — the
	// guard excludes these from its thresholds; every other removal counts.
	MovedByHoserva int

	// PerDisk is keyed by mount point, and is how the guard's zero-files
	// rule ("any data disk reports zero files where it previously had
	// files") is evaluated.
	PerDisk map[string]DiskDiff
}

// DiskDiff is one data disk's file count before and after the change this
// diff reports.
type DiskDiff struct {
	FilesBefore int
	FilesAfter  int
}

// Progress is one update from a running sync or scrub. Err is non-nil only
// on the final message, and marks the run as failed rather than completed —
// the shape doc 02 §6 needs for "a disk disappearing mid-sync: job must fail
// cleanly, not corrupt state".
type Progress struct {
	Phase      string
	Percent    float64
	BytesDone  int64
	BytesTotal int64
	Err        error
}

// FreshnessLevel is the dashboard's permanent parity indicator (doc 02 §2).
type FreshnessLevel int

const (
	FreshnessGreen FreshnessLevel = iota
	FreshnessAmber
	FreshnessRed
)

// ParityStatus is `snapraid status` distilled to what the dashboard and the
// freshness indicator need.
type ParityStatus struct {
	Freshness        FreshnessLevel
	LastSyncAt       time.Time
	ChangedSinceSync int
	DataDisks        int
	ParityDisks      int
}

// Engine is the interface every subsystem touching SnapRAID sits behind
// (doc 01 §4). context.Context is first on every method because every one
// of them shells out to snapraid.
type Engine interface {
	Sync(ctx context.Context, opts SyncOpts) (<-chan Progress, error)
	Diff(ctx context.Context) (DiffReport, error)
	Scrub(ctx context.Context, pct int) (<-chan Progress, error)
	Status(ctx context.Context) (ParityStatus, error)
}
