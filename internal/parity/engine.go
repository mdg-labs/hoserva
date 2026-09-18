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

// Progress is one update from a running sync, scrub, fix or check. Err is
// non-nil only on the final message, and marks the run as failed rather
// than completed — the shape doc 02 §6 needs for "a disk disappearing
// mid-sync: job must fail cleanly, not corrupt state". Output is the raw
// text SnapRAID printed for this update (one progress line, or — on the
// final message — nothing beyond what Percent/Err already say); a caller
// wiring this into a job's own RunContext.Output() (doc 01 §4) writes
// each Output through as it arrives to build that job's full captured
// log, without Engine depending on the job package to do it.
type Progress struct {
	Phase      string
	Percent    float64
	BytesDone  int64
	BytesTotal int64
	Output     string
	Err        error
}

// FixOpts selects what Fix reconstructs from parity (doc 02 §2, §4):
// exactly one of Disk (a whole failed disk, "Replacing a failed disk"
// step 4) or Path (undeleting one file by its array-relative path) is
// normally set for a guided fix; ErrorsOnly (SnapRAID's `-e`) restricts
// either to just the blocks a prior scrub already marked bad, the
// scrub-then-fix repair cycle confirmed in spike S5.
type FixOpts struct {
	Disk       string
	Path       string
	ErrorsOnly bool
}

// CheckOpts selects what Check verifies (doc 02 §2): Disk narrows to one
// disk (the "paranoid check" step of disk replacement, doc 02 §4), and
// AuditOnly (`-a`) checks file data only, without recomputing parity.
type CheckOpts struct {
	Disk      string
	AuditOnly bool
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
// of them shells out to snapraid. Sync, Scrub, Fix and Check are doc 01
// §4's four Parity-class job types — each streams Progress because each
// can run for real time against real data; Diff and Status are not job
// types at all (`internal/job`'s own Type table has no entry for either):
// diff runs synchronously immediately before every sync and on request,
// never on a timer, and status only reads the boot-device content file,
// so both return a single value once they finish rather than a channel.
// Touch is not part of this interface at all — Q17 makes it an automatic
// step Sync takes on its own before syncing, never something called
// independently.
type Engine interface {
	Sync(ctx context.Context, opts SyncOpts) (<-chan Progress, error)
	Diff(ctx context.Context) (DiffReport, error)
	// Scrub verifies pct percent of the array, restricted to blocks last
	// verified more than olderThanDays ago (doc 02 §2's own scrub
	// options; SnapRAID's `-p`/`-o`) — 0 forces literally everything,
	// ignoring recency, the override a guided "check now" action needs
	// rather than the scheduled default (DefaultScrubOlderThanDays).
	Scrub(ctx context.Context, pct, olderThanDays int) (<-chan Progress, error)
	Status(ctx context.Context) (ParityStatus, error)
	Fix(ctx context.Context, opts FixOpts) (<-chan Progress, error)
	Check(ctx context.Context, opts CheckOpts) (<-chan Progress, error)
}
