// Package parity defines the subsystem abstraction over SnapRAID (doc 01
// §4, doc 02 §2). Engine orchestrates SnapRAID's own sync/diff/scrub/status
// operations — it computes nothing itself (D1) — so the threshold guard and
// every caller above it can be tested against a scripted simulator instead
// of a real parity disk (doc 06 §2).
package parity

import (
	"context"
	"path/filepath"
	"time"
)

// SyncOpts controls one sync run (doc 01 §3: `hoserva sync [--dry-run]
// [--force]`).
type SyncOpts struct {
	// DryRun runs the diff SnapRAID would sync against without writing
	// parity.
	DryRun bool
	// Confirm is the human decision behind the threshold guard's (doc 02
	// §2) "Review the diff and sync anyway" action: it authorizes Sync to
	// proceed past a diff its own guard evaluation just blocked. It has no
	// effect when the guard does not block, and it can never skip that
	// evaluation itself — Sync always runs it fresh, for every non-dry-run
	// call, before this field is even consulted (CLAUDE.md: "no code path
	// syncs without passing the guard"). This is the "minimum is a
	// confirmation prompt" doc 02 §2 and Q16 require: there is no field
	// anywhere in this package that disables the guard outright.
	Confirm bool
	// Manifest is this sync's relocation manifest (Q15): entries a
	// mover, rebalance, evacuation or share-relocation job recorded for
	// files it moved since the last sync. The guard excludes a removal
	// from its thresholds when it matches an entry here and the same
	// relative path either reappears as added or copied on that entry's
	// target disk in this sync's own fresh diff, or was already
	// confirmed there by an earlier sync — Q14's mandated two-phase
	// order (copy+verify, sync, delete, sync) means a relocation's own
	// addition and removal are structurally never in the same diff, so
	// Sync itself checks a real `snapraid list` for exactly the entries
	// that need it before evaluating the guard (ManifestEntry.
	// TargetConfirmed, #248).
	Manifest []ManifestEntry
	// RemovingDisks is the set of mount points currently being evacuated
	// (doc 09 §4, keyed the same way DiffReport.PerDisk is) — exempt from
	// the guard's zero-files rule. Independently of the guard, SnapRAID's
	// own native "would empty a disk" refusal (syncArgv's own doc
	// comment) still needs `-E` whenever any disk — evacuating or not —
	// is emptied by this diff, so Sync sets it for exactly those disks'
	// syncs regardless of RemovingDisks' contents.
	RemovingDisks map[string]bool
}

// ManifestEntry is one file a relocation job (the mover, rebalance,
// evacuation or share relocation — doc 09 §2-§4) recorded moving since the
// last sync (doc 02 §2, Q15). Whichever package drives that job owns
// writing and persisting these; this package never persists a change to
// one — it only ever reads a slice of them, handed in through
// SyncOpts.Manifest for the one sync that must account for them, and may
// set TargetConfirmed on its own in-memory copy before evaluating the
// guard (#248) — a derived fact recomputed fresh from a real `snapraid
// list` every time, never written back to whatever store the caller
// loaded the manifest from.
type ManifestEntry struct {
	// RelPath is the file's path relative to its disk (matching
	// DiffFile.RelPath's own shape — what the guard's manifest matching
	// actually compares it against), identical on both SourceDisk and
	// TargetDisk — a relocation moves a file, it never renames it.
	RelPath    string
	Size       int64
	MTime      time.Time
	SourceDisk string // mount point, matching DiffReport.PerDisk's own keys.
	TargetDisk string
	// TargetConfirmed is true once this entry's file is already tracked
	// on TargetDisk in SnapRAID's own content file — i.e. an earlier
	// sync's own diff already recorded the addition this manifest entry
	// promises (Q14's two-phase order: copy+verify, sync, delete, sync —
	// the addition and this entry's eventual removal are structurally
	// never in the same diff, #248). A caller building a manifest never
	// sets this itself; it always starts false, and SnapraidEngine.Sync
	// is the only place that ever sets it, from a real `snapraid list`,
	// never from the manifest's own say-so.
	TargetConfirmed bool
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

	// RemovedFiles and AddedFiles are Removed and Added+Copied broken out
	// per file, RelPath relative to each entry's own Disk (a mount point,
	// matching PerDisk's keys) — what the guard's manifest accounting
	// (Q15) matches a ManifestEntry against. AddedFiles combines both
	// freshly added files and copy destinations, since Q15's own rule is
	// "appears as added or copied on the manifest's target disk".
	RemovedFiles []DiffFile
	AddedFiles   []DiffFile
}

// DiffFile is one file a diff reported as removed, or added/copied-in, on
// one data disk.
type DiffFile struct {
	Disk    string // mount point, matching DiffReport.PerDisk's own keys.
	RelPath string
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
	// DataMounts maps SnapRAID's own disk id ("d1") to the mount path the
	// running config assigns it, straight from the config `status` itself
	// just echoed back — the mapping FixOpts.Disk and CheckOpts.Disk need
	// to target one disk by the label snapraid uses (doc 02 §4
	// "Replacing a failed disk" step 4, `snapraid fix -d dN`).
	DataMounts map[string]string
}

// DataDiskLabel returns the SnapRAID disk id ("d1", "d2", ...) whose
// DataMounts entry is mount, for building a FixOpts or CheckOpts targeting
// one disk by mount point rather than by SnapRAID's own internal label
// (doc 02 §4 "Replacing a failed disk" step 4). Paths are compared after
// filepath.Clean, so an equivalent spelling (a trailing slash, say) still
// matches.
func (s ParityStatus) DataDiskLabel(mount string) (string, bool) {
	clean := filepath.Clean(mount)
	for id, m := range s.DataMounts {
		if filepath.Clean(m) == clean {
			return id, true
		}
	}
	return "", false
}

// Engine is the interface every subsystem touching SnapRAID sits behind
// (doc 01 §4). context.Context is first on every method because every one
// of them shells out to snapraid. Sync, Scrub, Fix and Check are doc 01
// §4's four Parity-class job types — each streams Progress because each
// can run for real time against real data; Diff, Status and List are not
// job types at all (`internal/job`'s own Type table has no entry for any
// of them): diff runs synchronously immediately before every sync and on
// request, never on a timer; status only reads the boot-device content
// file; and List reads that same tracked state to report every file
// SnapRAID currently tracks, each with its owning disk and size — the
// per-disk directory breakdown doc 02 §1 line 78 describes, computed only
// as a step of the sync job (RunSync, #223), never live and never on its
// own timer. All three return a single value once they finish rather than
// a channel. Touch is not part of this interface at all — Q17 makes it an
// automatic step Sync takes on its own before syncing, never something
// called independently.
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
	List(ctx context.Context) (ListReport, error)
}
