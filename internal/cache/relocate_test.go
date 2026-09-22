package cache

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/parity"
)

// relocateShare builds a Share with branchCount per-disk array
// directories, each named "<base>/diskN/<name>", and its own cache
// directory — the shape RelocateToCache's own doc comment requires:
// each Branches entry is "<disk>/<share>".
func relocateShare(t *testing.T, name string, branchCount int) Share {
	t.Helper()
	base := t.TempDir()
	s := Share{
		Name:      name,
		CachePath: filepath.Join(base, "cache", name),
	}
	for i := 0; i < branchCount; i++ {
		s.Branches = append(s.Branches, filepath.Join(base, "disks", "disk"+string(rune('1'+i)), name))
	}
	return s
}

// syncFuncFromEngine adapts a parity.Engine into a cache.SyncFunc the
// same way a future internal/job wiring would (doc.go's own precedent
// for keeping this package decoupled from what it calls) — draining the
// returned progress channel and mapping a blocked or failed sync to a
// plain error.
func syncFuncFromEngine(e parity.Engine) SyncFunc {
	return func(ctx context.Context, manifest []parity.ManifestEntry) error {
		ch, err := e.Sync(ctx, parity.SyncOpts{Manifest: manifest})
		if err != nil {
			return err
		}
		var last parity.Progress
		for p := range ch {
			last = p
		}
		return last.Err
	}
}

func TestRelocateToArray_IgnoresGracePeriod(t *testing.T) {
	s := newShare(t, "movies")
	src := filepath.Join(s.CachePath, "movie.mkv")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	// Written "now" — well inside the default grace period, unlike
	// mustWrite's own hour-old files.
	if err := os.WriteFile(src, []byte("movie bytes"), 0o640); err != nil {
		t.Fatal(err)
	}

	report, err := RelocateToArray(context.Background(), s, Config{}, testDeps(NewFakeOpenChecker()), RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RelocateToArray: %v", err)
	}
	if len(report.Moved()) != 1 {
		t.Fatalf("expected the freshly-written file to move despite the grace period, got %+v", report.Entries)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("source should be gone: err=%v", err)
	}
}

// TestRelocateToCache_CopiesVerifiesSyncsDeletes is the two-phase happy
// path (doc 09 §2, Q14, Q15): the array original is copied to cache,
// verified, a sync runs carrying the file's own manifest entry, and only
// then is the array original removed.
func TestRelocateToCache_CopiesVerifiesSyncsDeletes(t *testing.T) {
	s := relocateShare(t, "docs", 1)
	src := filepath.Join(s.Branches[0], "report.pdf")
	mustWrite(t, src, "report bytes")

	engine := parity.NewFakeEngine()
	engine.Sleep = func(_ time.Duration) {}
	engine.ScriptSync([]parity.Progress{{Percent: 100}}, nil)

	deps := testDeps(NewFakeOpenChecker())
	deps.Sync = syncFuncFromEngine(engine)

	report, err := RelocateToCache(context.Background(), s, Config{}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RelocateToCache: %v", err)
	}
	if len(report.Moved()) != 1 {
		t.Fatalf("expected one moved entry, got %+v", report.Entries)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("array original should be gone: err=%v", err)
	}
	dst := filepath.Join(s.CachePath, "report.pdf")
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "report bytes" {
		t.Fatalf("cache copy = %q, %v, want %q", got, err, "report bytes")
	}
}

// TestRelocateToCache_ManifestUsesDiskRelativePaths proves each
// ManifestEntry the copy phase hands to Sync matches DiffFile.RelPath's
// own shape (ManifestEntry's doc comment, Q15): RelPath carries the
// share prefix and TargetDisk is the cache mount point itself, not the
// share's own cache subdirectory. Getting this wrong means the guard's
// matchManifest can never recognize the removal this relocation causes
// as accounted (internal/parity/guard.go), leaving the trailing sync
// blocked behind an unnecessary confirm.
func TestRelocateToCache_ManifestUsesDiskRelativePaths(t *testing.T) {
	s := relocateShare(t, "docs", 1)
	src := filepath.Join(s.Branches[0], "sub", "report.pdf")
	mustWrite(t, src, "report bytes")

	engine := parity.NewFakeEngine()
	engine.Sleep = func(_ time.Duration) {}
	engine.ScriptSync([]parity.Progress{{Percent: 100}}, nil)

	var gotManifest []parity.ManifestEntry
	deps := testDeps(NewFakeOpenChecker())
	deps.Sync = func(ctx context.Context, manifest []parity.ManifestEntry) error {
		if gotManifest == nil {
			gotManifest = manifest
		}
		return syncFuncFromEngine(engine)(ctx, manifest)
	}

	if _, err := RelocateToCache(context.Background(), s, Config{}, deps, RunHooks{}, nil); err != nil {
		t.Fatalf("RelocateToCache: %v", err)
	}

	if len(gotManifest) != 1 {
		t.Fatalf("expected one manifest entry, got %+v", gotManifest)
	}
	me := gotManifest[0]
	wantRelPath := filepath.Join("docs", "sub", "report.pdf")
	if me.RelPath != wantRelPath {
		t.Errorf("RelPath = %q, want %q (share-prefixed, disk-relative)", me.RelPath, wantRelPath)
	}
	wantSourceDisk := filepath.Dir(s.Branches[0])
	if me.SourceDisk != wantSourceDisk {
		t.Errorf("SourceDisk = %q, want %q (the data disk mount)", me.SourceDisk, wantSourceDisk)
	}
	wantTargetDisk := filepath.Dir(s.CachePath)
	if me.TargetDisk != wantTargetDisk {
		t.Errorf("TargetDisk = %q, want %q (the cache disk mount, not the share's cache subdirectory)", me.TargetDisk, wantTargetDisk)
	}
}

// TestRelocateToCache_SyncsAgainAfterDelete proves Q14's trailing sync:
// "copy and verify everything, run sync, delete the sources, then sync
// again". Parity must never be left stale against a deletion this run
// itself just made, so a second guarded sync — carrying the same
// manifest — has to run once the array originals are gone.
func TestRelocateToCache_SyncsAgainAfterDelete(t *testing.T) {
	s := relocateShare(t, "docs", 1)
	src := filepath.Join(s.Branches[0], "report.pdf")
	mustWrite(t, src, "report bytes")

	engine := parity.NewFakeEngine()
	engine.Sleep = func(_ time.Duration) {}
	engine.ScriptSync([]parity.Progress{{Percent: 100}}, nil)

	var calls int
	deps := testDeps(NewFakeOpenChecker())
	deps.Sync = func(ctx context.Context, manifest []parity.ManifestEntry) error {
		calls++
		switch calls {
		case 1:
			if _, err := os.Stat(src); err != nil {
				t.Fatalf("array original must still exist before the first sync: %v", err)
			}
		case 2:
			if _, err := os.Stat(src); !os.IsNotExist(err) {
				t.Fatalf("array original must already be gone before the trailing sync: err=%v", err)
			}
		}
		return syncFuncFromEngine(engine)(ctx, manifest)
	}

	report, err := RelocateToCache(context.Background(), s, Config{}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RelocateToCache: %v", err)
	}
	if len(report.Moved()) != 1 {
		t.Fatalf("expected one moved entry, got %+v", report.Entries)
	}
	if calls != 2 {
		t.Fatalf("expected sync to run twice (Q14: sync, delete, sync again), got %d calls", calls)
	}
}

// TestRelocateToCache_GuardBlocked_LeavesArrayUntouched is this issue's
// own central safety test (CLAUDE.md: "anything that can lose data gets
// its test before its implementation" — a threshold-guard block must
// never be bypassed and must never be followed by a delete): a blocked
// sync must stop the relocation before anything is deleted from the
// array, and the cache-side copy — already safely duplicated — must
// survive so a later, confirmed run can still finish.
func TestRelocateToCache_GuardBlocked_LeavesArrayUntouched(t *testing.T) {
	s := relocateShare(t, "docs", 1)
	src := filepath.Join(s.Branches[0], "report.pdf")
	mustWrite(t, src, "report bytes")

	engine := parity.NewFakeEngine()
	engine.Sleep = func(_ time.Duration) {}
	engine.ScriptGuardBlock(parity.GuardResult{Blocked: true, Triggers: []parity.GuardTrigger{parity.TriggerRemovedCount}})

	deps := testDeps(NewFakeOpenChecker())
	deps.Sync = syncFuncFromEngine(engine)

	report, err := RelocateToCache(context.Background(), s, Config{}, deps, RunHooks{}, nil)
	if err == nil {
		t.Fatal("expected an error from a blocked sync, got nil")
	}
	if !errors.Is(err, parity.ErrGuardBlocked) {
		t.Fatalf("err = %v, want errors.Is(err, parity.ErrGuardBlocked)", err)
	}
	if len(report.Moved()) != 0 {
		t.Fatalf("nothing should be reported moved when the sync is blocked, got %+v", report.Entries)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("array original must survive a blocked sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.CachePath, "report.pdf")); err != nil {
		t.Fatalf("verified cache copy must survive a blocked sync: %v", err)
	}
}

// TestRelocateToCache_OpenAtDeleteTime_LeavesSourceInPlace proves the
// re-check-immediately-before-unlink step (doc 09 §2): a file that
// became open again between the copy phase and the delete phase is left
// on the array — both copies are already correct, so nothing is lost.
func TestRelocateToCache_OpenAtDeleteTime_LeavesSourceInPlace(t *testing.T) {
	s := relocateShare(t, "docs", 1)
	src := filepath.Join(s.Branches[0], "report.pdf")
	mustWrite(t, src, "report bytes")

	open := NewFakeOpenChecker()
	engine := parity.NewFakeEngine()
	engine.Sleep = func(_ time.Duration) {}
	engine.ScriptSync([]parity.Progress{{Percent: 100}}, nil)

	deps := testDeps(open)
	deps.Sync = func(ctx context.Context, manifest []parity.ManifestEntry) error {
		// The file becomes open exactly as the sync completes — after
		// the copy phase's own open check already passed, but before
		// the delete phase's re-check runs.
		open.SetOpen(src, true)
		return syncFuncFromEngine(engine)(ctx, manifest)
	}

	report, err := RelocateToCache(context.Background(), s, Config{}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RelocateToCache: %v", err)
	}
	if len(report.Moved()) != 0 {
		t.Fatalf("expected no completed move, got %+v", report.Entries)
	}
	pending := report.byResult(ResultMovedPendingDelete)
	if len(pending) != 1 {
		t.Fatalf("expected one moved_pending_delete entry, got %+v", report.Entries)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("source held open at delete time must survive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.CachePath, "report.pdf")); err != nil {
		t.Fatalf("verified cache copy must still exist: %v", err)
	}
}

// TestRelocateToCache_Conflict_LeavesBothCopiesUntouched proves a
// cache-side file that already exists with a different size is never
// auto-resolved (doc 09 §2, mirroring the mover's own conflict rule):
// the array original is left exactly as it is.
func TestRelocateToCache_Conflict_LeavesBothCopiesUntouched(t *testing.T) {
	s := relocateShare(t, "docs", 1)
	src := filepath.Join(s.Branches[0], "report.pdf")
	mustWrite(t, src, "report bytes")
	mustWrite(t, filepath.Join(s.CachePath, "report.pdf"), "a different, unrelated file")

	deps := testDeps(NewFakeOpenChecker())
	deps.Sync = func(context.Context, []parity.ManifestEntry) error {
		t.Fatal("sync must not run when nothing was copied")
		return nil
	}

	report, err := RelocateToCache(context.Background(), s, Config{}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RelocateToCache: %v", err)
	}
	conflicts := report.byResult(ResultConflict)
	if len(conflicts) != 1 {
		t.Fatalf("expected one conflict entry, got %+v", report.Entries)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("array original must survive a conflict: %v", err)
	}
}

// TestRelocateToCache_InterruptedBeforeSync_ThenResumes proves the
// two-phase order end to end: interrupting between the copy phase and
// the sync leaves the array completely untouched (nothing was ever at
// risk), and a later resume — from exactly the checkpoint the first run
// saved — completes the relocation without re-copying.
func TestRelocateToCache_InterruptedBeforeSync_ThenResumes(t *testing.T) {
	s := relocateShare(t, "docs", 1)
	src := filepath.Join(s.Branches[0], "report.pdf")
	mustWrite(t, src, "report bytes")

	engine := parity.NewFakeEngine()
	engine.Sleep = func(_ time.Duration) {}
	engine.ScriptSync([]parity.Progress{{Percent: 100}}, nil)

	syncCalled := false
	deps := testDeps(NewFakeOpenChecker())
	deps.Sync = func(ctx context.Context, manifest []parity.ManifestEntry) error {
		syncCalled = true
		return syncFuncFromEngine(engine)(ctx, manifest)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var lastCheckpoint []byte
	hooks := RunHooks{
		SaveCheckpoint: func(data []byte) error {
			lastCheckpoint = append([]byte(nil), data...)
			var cp RelocateCheckpoint
			if err := json.Unmarshal(data, &cp); err != nil {
				t.Fatalf("unmarshal checkpoint: %v", err)
			}
			if cp.Phase == RelocatePhaseSyncing {
				// Interrupt right at the copy/sync boundary — the array
				// must still be completely untouched at this point.
				cancel()
			}
			return nil
		},
	}

	report, err := RelocateToCache(ctx, s, Config{}, deps, hooks, nil)
	if err != nil {
		t.Fatalf("RelocateToCache: %v", err)
	}
	if !report.Interrupted {
		t.Fatal("expected the run to report Interrupted")
	}
	if syncCalled {
		t.Fatal("sync must never run once the context was cancelled before it")
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("array original must be untouched after an interrupt before sync: %v", err)
	}
	if len(report.Moved()) != 0 {
		t.Fatalf("nothing should be reported moved yet, got %+v", report.Entries)
	}

	// Resume from the exact checkpoint the interrupted run left behind.
	report2, err := RelocateToCache(context.Background(), s, Config{}, deps, RunHooks{}, lastCheckpoint)
	if err != nil {
		t.Fatalf("resumed RelocateToCache: %v", err)
	}
	if !syncCalled {
		t.Fatal("expected the resumed run to call sync")
	}
	if len(report2.Moved()) != 1 {
		t.Fatalf("expected the resumed run to complete the move, got %+v", report2.Entries)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("array original should be gone after the resumed run: err=%v", err)
	}
}

// TestRelocateToCache_InterruptedMidCopy_ThenResumesCompletesAllFiles
// proves an interrupt *during* the copy phase — after some files are
// already copied and verified onto cache, before the phase itself
// finishes — never drops them: the copy-phase checkpoint carries no
// manifest of its own (RelocateCheckpoint's own doc comment), so a
// resumed run must fold every already-copied file back into the
// manifest itself, not just the ones the checkpoint hadn't yet reached,
// or the sync and delete phases would never learn those files exist.
func TestRelocateToCache_InterruptedMidCopy_ThenResumesCompletesAllFiles(t *testing.T) {
	s := relocateShare(t, "docs", 1)
	srcA := filepath.Join(s.Branches[0], "a.txt")
	srcB := filepath.Join(s.Branches[0], "b.txt")
	srcC := filepath.Join(s.Branches[0], "c.txt")
	mustWrite(t, srcA, "a bytes")
	mustWrite(t, srcB, "b bytes")
	mustWrite(t, srcC, "c bytes")

	engine := parity.NewFakeEngine()
	engine.Sleep = func(_ time.Duration) {}
	engine.ScriptSync([]parity.Progress{{Percent: 100}}, nil)

	var syncedManifests [][]parity.ManifestEntry
	deps := testDeps(NewFakeOpenChecker())
	deps.Sync = func(ctx context.Context, manifest []parity.ManifestEntry) error {
		syncedManifests = append(syncedManifests, manifest)
		return syncFuncFromEngine(engine)(ctx, manifest)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var lastCheckpoint []byte
	hooks := RunHooks{
		SaveCheckpoint: func(data []byte) error {
			lastCheckpoint = append([]byte(nil), data...)
			var cp RelocateCheckpoint
			if err := json.Unmarshal(data, &cp); err != nil {
				t.Fatalf("unmarshal checkpoint: %v", err)
			}
			if cp.Phase == RelocatePhaseCopying && cp.LastPath == "a.txt" {
				// Interrupt mid copy phase — right after a.txt is copied
				// and verified onto cache, before b.txt or c.txt are ever
				// reached.
				cancel()
			}
			return nil
		},
	}

	report, err := RelocateToCache(ctx, s, Config{}, deps, hooks, nil)
	if err != nil {
		t.Fatalf("RelocateToCache: %v", err)
	}
	if !report.Interrupted {
		t.Fatal("expected the run to report Interrupted")
	}
	if len(syncedManifests) != 0 {
		t.Fatal("sync must never run before the copy phase finishes")
	}
	if _, err := os.Stat(srcA); err != nil {
		t.Fatalf("a.txt must still be on the array before any sync: %v", err)
	}

	// Resume from exactly the checkpoint the interrupted run left behind.
	report2, err := RelocateToCache(context.Background(), s, Config{}, deps, RunHooks{}, lastCheckpoint)
	if err != nil {
		t.Fatalf("resumed RelocateToCache: %v", err)
	}
	if report2.Interrupted {
		t.Fatal("resumed run should complete, not interrupt again")
	}
	if moved := report2.Moved(); len(moved) != 3 {
		t.Fatalf("expected all three files moved after resume, got %+v", report2.Entries)
	}
	for _, src := range []string{srcA, srcB, srcC} {
		if _, err := os.Stat(src); !os.IsNotExist(err) {
			t.Fatalf("array original %s should be gone after the resumed run: err=%v", src, err)
		}
	}
	if len(syncedManifests) == 0 {
		t.Fatal("expected the resumed run to call sync")
	}
	if len(syncedManifests[0]) != 3 {
		t.Fatalf("expected the sync manifest to account for all three files (including a.txt, copied before the interrupt), got %d entries: %+v", len(syncedManifests[0]), syncedManifests[0])
	}
}

func TestRelocateToCache_RequiresSync(t *testing.T) {
	s := relocateShare(t, "docs", 1)
	_, err := RelocateToCache(context.Background(), s, Config{}, testDeps(NewFakeOpenChecker()), RunHooks{}, nil)
	if err == nil {
		t.Fatal("expected an error when Deps.Sync is nil")
	}
}

// TestRelocateToCache_ReusesOneSnapshotForCopyPhase proves the copy phase's
// pre-copy open checks take exactly one /proc walk for a share with
// several eligible files, not one per file — mirroring #238's own
// TestRun_ReusesOneSnapshotPerShare, using the same countingSnapshotter
// (mover_test.go).
func TestRelocateToCache_ReusesOneSnapshotForCopyPhase(t *testing.T) {
	s := relocateShare(t, "docs", 1)
	mustWrite(t, filepath.Join(s.Branches[0], "a.txt"), "a bytes")
	mustWrite(t, filepath.Join(s.Branches[0], "b.txt"), "b bytes")
	mustWrite(t, filepath.Join(s.Branches[0], "c.txt"), "c bytes")

	engine := parity.NewFakeEngine()
	engine.Sleep = func(_ time.Duration) {}
	engine.ScriptSync([]parity.Progress{{Percent: 100}}, nil)

	snapshotter := newCountingSnapshotter()
	deps := testDeps(NewFakeOpenChecker())
	deps.Open = snapshotter
	deps.Sync = syncFuncFromEngine(engine)

	report, err := RelocateToCache(context.Background(), s, Config{}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RelocateToCache: %v", err)
	}
	if len(report.Moved()) != 3 {
		t.Fatalf("expected all three files to move, got %+v", report.Entries)
	}
	if snapshotter.snapshots != 1 {
		t.Fatalf("Snapshot called %d times, want exactly 1 for a three-file copy phase", snapshotter.snapshots)
	}
}

// TestRelocateToCache_PreUnlinkRecheckIgnoresStalePreCopySnapshot proves
// finishRelocateDelete's pre-unlink check is answered by the checker's own
// live IsOpen, never by the copy phase's snapshot — mirroring #238's own
// TestRun_PreUnlinkRecheckIgnoresStalePreCopySnapshot: scripting the file
// closed in the snapshot (so the copy proceeds) but open on the checker's
// own live IsOpen must still leave the array original in place afterward.
func TestRelocateToCache_PreUnlinkRecheckIgnoresStalePreCopySnapshot(t *testing.T) {
	s := relocateShare(t, "docs", 1)
	src := filepath.Join(s.Branches[0], "report.pdf")
	mustWrite(t, src, "report bytes")

	snapshotter := newCountingSnapshotter()
	snapshotter.SetOpen(src, true) // live IsOpen reports open throughout; the snapshot never does

	engine := parity.NewFakeEngine()
	engine.Sleep = func(_ time.Duration) {}
	engine.ScriptSync([]parity.Progress{{Percent: 100}}, nil)

	deps := testDeps(NewFakeOpenChecker())
	deps.Open = snapshotter
	deps.Sync = syncFuncFromEngine(engine)

	report, err := RelocateToCache(context.Background(), s, Config{}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RelocateToCache: %v", err)
	}
	if len(report.Moved()) != 0 {
		t.Fatalf("expected no completed move, got %+v", report.Entries)
	}
	pending := report.byResult(ResultMovedPendingDelete)
	if len(pending) != 1 {
		t.Fatalf("expected one moved_pending_delete entry — the copy must have proceeded (the snapshot reported it closed) but the pre-unlink recheck must have caught the live open state, got %+v", report.Entries)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("source held open at delete time must survive: %v", err)
	}
}

func TestPrecheck_ReportsOpenFilesOnCacheAndArray(t *testing.T) {
	s := relocateShare(t, "docs", 2)
	cacheFile := filepath.Join(s.CachePath, "open-on-cache.txt")
	arrayFile := filepath.Join(s.Branches[1], "open-on-array.txt")
	quietFile := filepath.Join(s.Branches[0], "quiet.txt")
	mustWrite(t, cacheFile, "a")
	mustWrite(t, arrayFile, "b")
	mustWrite(t, quietFile, "c")

	open := NewFakeOpenChecker()
	open.SetOpen(cacheFile, true)
	open.SetOpen(arrayFile, true)

	result, err := Precheck(context.Background(), s, testDeps(open))
	if err != nil {
		t.Fatalf("Precheck: %v", err)
	}
	if len(result.OpenPaths) != 2 {
		t.Fatalf("OpenPaths = %+v, want 2 entries", result.OpenPaths)
	}
}

// TestPrecheck_ReusesOneSnapshotAcrossAllRoots proves Precheck takes
// exactly one /proc walk across the cache path and every array branch
// combined, not one per file.
func TestPrecheck_ReusesOneSnapshotAcrossAllRoots(t *testing.T) {
	s := relocateShare(t, "docs", 2)
	mustWrite(t, filepath.Join(s.CachePath, "cache.txt"), "a")
	mustWrite(t, filepath.Join(s.Branches[0], "disk1.txt"), "b")
	mustWrite(t, filepath.Join(s.Branches[1], "disk2.txt"), "c")

	snapshotter := newCountingSnapshotter()
	deps := testDeps(NewFakeOpenChecker())
	deps.Open = snapshotter

	if _, err := Precheck(context.Background(), s, deps); err != nil {
		t.Fatalf("Precheck: %v", err)
	}
	if snapshotter.snapshots != 1 {
		t.Fatalf("Snapshot called %d times, want exactly 1 across the cache path and every branch", snapshotter.snapshots)
	}
}
