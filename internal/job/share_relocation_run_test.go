package job

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mdg-labs/hoserva/internal/cache"
	"github.com/mdg-labs/hoserva/internal/parity"
)

// syncFuncFromEngine adapts a parity.Engine into a cache.SyncFunc the way
// a real daemon's own share-relocation wiring must (cache.SyncFunc's own
// doc comment): draining the returned progress channel with this
// package's own drainProgress (parity_run.go) so a blocked or failed
// sync surfaces as a plain error, exactly as RunSync's engine call does.
func syncFuncFromEngine(e parity.Engine) cache.SyncFunc {
	return func(ctx context.Context, manifest []parity.ManifestEntry) error {
		ch, err := e.Sync(ctx, parity.SyncOpts{Manifest: manifest})
		return drainProgress(ch, err)
	}
}

func newShareRelocationShare(t *testing.T, name string) cache.Share {
	t.Helper()
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	return cache.Share{
		Name:      name,
		CachePath: filepath.Join(base, "cache", name),
		Branches:  []string{filepath.Join(base, "disk1", name)},
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
}

// TestRunShareRelocation_ToCache_GuardBlocked_LeavesArrayUntouched is this
// issue's own central safety test (CLAUDE.md: "anything that can lose
// data gets its test before its implementation"): the job wiring this
// issue adds must route RelocateToCache's sync through the real
// threshold guard exactly as internal/cache's own #54 tests already
// prove RelocateToCache itself does — a blocked sync must fail the job
// before anything is deleted from the array, with the cache-side copy
// surviving so a later, confirmed run can still finish.
func TestRunShareRelocation_ToCache_GuardBlocked_LeavesArrayUntouched(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	share := newShareRelocationShare(t, "docs")
	src := filepath.Join(share.Branches[0], "report.pdf")
	mustWriteFile(t, src, "report bytes")

	eng := newRecordingEngine()
	eng.ScriptGuardBlock(trippedGuard())

	s.registry.Register(TypeShareRelocation, true, RunShareRelocation(ShareRelocationDeps{
		Open:  fakeOpen(),
		Share: func(context.Context, string) (cache.Share, error) { return share, nil },
		Sync:  syncFuncFromEngine(eng),
	}))

	j, err := s.Submit(ctx, TypeShareRelocation, nil, mustJSON(t, ShareRelocationParams{Share: "docs", To: "cache"}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s, want failed", finished.Status)
	}
	if !strings.Contains(finished.ErrorMessage, "threshold guard blocked the sync") {
		t.Fatalf("ErrorMessage = %q, want a threshold-guard block", finished.ErrorMessage)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("array original must survive a blocked sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(share.CachePath, "report.pdf")); err != nil {
		t.Fatalf("verified cache copy must survive a blocked sync: %v", err)
	}
	_, wrote, _, _ := eng.snapshot()
	if wrote {
		t.Fatal("sync proceeded past a tripped threshold guard — parity would have been written over the deletions")
	}
}

// TestRunShareRelocation_ToCache_MovesThroughScheduler proves the happy
// path reaches RelocateToCache through a real Scheduler.Submit/Await
// round trip: the array-side file is copied to cache, synced and its
// array original removed (Q14).
func TestRunShareRelocation_ToCache_MovesThroughScheduler(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	share := newShareRelocationShare(t, "docs")
	src := filepath.Join(share.Branches[0], "report.pdf")
	mustWriteFile(t, src, "report bytes")

	eng := newRecordingEngine()
	eng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)

	s.registry.Register(TypeShareRelocation, true, RunShareRelocation(ShareRelocationDeps{
		Open:  fakeOpen(),
		Share: func(context.Context, string) (cache.Share, error) { return share, nil },
		Sync:  syncFuncFromEngine(eng),
	}))

	j, err := s.Submit(ctx, TypeShareRelocation, nil, mustJSON(t, ShareRelocationParams{Share: "docs", To: "cache"}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	got, err := os.ReadFile(filepath.Join(share.CachePath, "report.pdf"))
	if err != nil || string(got) != "report bytes" {
		t.Fatalf("cache content = %q, %v, want %q", got, err, "report bytes")
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("array original should be gone after relocation: err=%v", err)
	}
}

// TestRunShareRelocation_ToArray_MovesThroughScheduler proves the
// cache-to-array direction reaches RelocateToArray through a real
// scheduler round trip, ignoring the grace period the way #54's own
// RelocateToArray always does.
func TestRunShareRelocation_ToArray_MovesThroughScheduler(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	base := t.TempDir()
	share := cache.Share{
		Name:      "docs",
		CachePath: filepath.Join(base, "cache", "docs"),
		ArrayPath: filepath.Join(base, "array", "docs"),
	}
	if err := os.MkdirAll(share.ArrayPath, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(share.CachePath, "report.pdf")
	mustWriteFile(t, src, "report bytes")

	s.registry.Register(TypeShareRelocation, true, RunShareRelocation(ShareRelocationDeps{
		Open:  fakeOpen(),
		Share: func(context.Context, string) (cache.Share, error) { return share, nil },
	}))

	j, err := s.Submit(ctx, TypeShareRelocation, nil, mustJSON(t, ShareRelocationParams{Share: "docs", To: "array"}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	dst := filepath.Join(share.ArrayPath, "report.pdf")
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "report bytes" {
		t.Fatalf("array content = %q, %v, want %q", got, err, "report bytes")
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("cache original should be gone after relocation: err=%v", err)
	}
}

// storeBackedEngine layers a real *parity.RelocationManifestStore's own
// Current onto recordingEngine's promoted, scripted
// CurrentRelocationManifest (params_test.go) — #247's own tests need
// RunSync's relocationManifestSource wiring to read the *real* store a
// concurrent share-relocation job wrote to, never a scripted stand-in, so
// this shadows that method instead of calling ScriptRelocationManifest.
type storeBackedEngine struct {
	*recordingEngine
	store *parity.RelocationManifestStore
}

func (e *storeBackedEngine) CurrentRelocationManifest(ctx context.Context) ([]parity.ManifestEntry, map[string]bool, error) {
	return e.store.Current(ctx)
}

// interruptOnceManifestDurable returns a stopRequested channel and a
// saveCheckpoint func for a directly-constructed RunContext: every
// checkpoint is recorded into *lastCheckpoint, and stopRequested is closed
// the first time this fires — simulating a daemon restart or
// maintenance-mode stop (doc 01 §4 Q70) landing right after
// RelocateToCache's own copy phase finishes. Because relocateCopyPhase
// only checks stopRequested at the *top* of its per-file loop
// (relocate.go), closing it from inside a checkpoint save never
// interrupts the copy phase itself mid-file — it takes effect only at the
// next checkpoint boundary, which for a share with everything already
// copied is the Phase-Syncing transition RelocateCheckpoint's own doc
// comment describes: the checkpoint that first carries the manifest, and
// the one saved immediately before RelocateToCache's first guarded sync
// call. The result is exactly the window this issue's own acceptance
// criteria call out: interrupted after the manifest is durable, before any
// sync ever ran.
func interruptOnceManifestDurable(lastCheckpoint *[]byte) (stopRequested chan struct{}, saveCheckpoint func(data []byte) error) {
	stopRequested = make(chan struct{})
	saveCheckpoint = func(data []byte) error {
		*lastCheckpoint = data
		select {
		case <-stopRequested:
		default:
			close(stopRequested)
		}
		return nil
	}
	return stopRequested, saveCheckpoint
}

// TestRunShareRelocation_ToCache_PersistsManifestBeforeFirstSync_ThenClears
// is #247's own write-path test: the manifest a copy phase built must be
// durable in the real RelocationManifestStore before RelocateToCache's
// first guarded sync ever runs, and the job's own successful completion
// (its trailing sync) must clear it again — proven against a real SQLite
// database (newTestDB), not a fake.
func TestRunShareRelocation_ToCache_PersistsManifestBeforeFirstSync_ThenClears(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	store := parity.NewRelocationManifestStore(db)

	s := newTestScheduler(t)
	share := newShareRelocationShare(t, "docs")
	src := filepath.Join(share.Branches[0], "report.pdf")
	mustWriteFile(t, src, "report bytes")

	eng := newRecordingEngine()
	eng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)

	var sawManifestBeforeSync []parity.ManifestEntry
	sync := func(ctx context.Context, manifest []parity.ManifestEntry) error {
		persisted, _, err := store.Current(ctx)
		if err != nil {
			t.Fatalf("store.Current inside the sync call: %v", err)
		}
		sawManifestBeforeSync = persisted
		return syncFuncFromEngine(eng)(ctx, manifest)
	}

	s.registry.Register(TypeShareRelocation, true, RunShareRelocation(ShareRelocationDeps{
		Open:     fakeOpen(),
		Share:    func(context.Context, string) (cache.Share, error) { return share, nil },
		Sync:     sync,
		Manifest: store,
	}))

	j, err := s.Submit(ctx, TypeShareRelocation, nil, mustJSON(t, ShareRelocationParams{Share: "docs", To: "cache"}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}

	if len(sawManifestBeforeSync) != 1 || sawManifestBeforeSync[0].RelPath != "docs/report.pdf" {
		t.Fatalf("manifest visible from inside the first guarded sync = %+v, want the copied file already durable", sawManifestBeforeSync)
	}

	manifest, removingDisks, err := store.Current(ctx)
	if err != nil {
		t.Fatalf("store.Current after completion: %v", err)
	}
	if manifest != nil || removingDisks != nil {
		t.Fatalf("manifest after a completed relocation = %+v/%+v, want cleared", manifest, removingDisks)
	}
}

// TestRunShareRelocation_ToCache_SyncDuringOutstandingRelocation_SeesManifest
// is #247's own central acceptance test. doc 01 §4's own exclusion table
// (internal/job/exclusion.go's conflicts) makes Parity and Array-write
// mutually exclusive while a job of either is actually *running* — so this
// models the real window the guard must cover: a relocation interrupted
// after its manifest became durable (a daemon restart, or maintenance
// mode, doc 01 §4 Q70) is no longer occupying the scheduler's Array-write
// slot, but its own manifest is still outstanding — exactly when a
// scheduled or manual sync can run next. This proves that sync, driven
// through the real Scheduler and RunSync's own relocationManifestSource
// wiring (parity_run.go), reads the relocation's own outstanding manifest
// from the real RelocationManifestStore rather than seeing nothing, and
// that once the relocation resumes and its trailing sync completes, a
// later sync no longer sees a stale one.
func TestRunShareRelocation_ToCache_SyncDuringOutstandingRelocation_SeesManifest(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	store := parity.NewRelocationManifestStore(db)

	share := newShareRelocationShare(t, "docs")
	src := filepath.Join(share.Branches[0], "report.pdf")
	mustWriteFile(t, src, "report bytes")

	relocEng := newRecordingEngine()
	relocEng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)
	fn := RunShareRelocation(ShareRelocationDeps{
		Open:     fakeOpen(),
		Share:    func(context.Context, string) (cache.Share, error) { return share, nil },
		Sync:     syncFuncFromEngine(relocEng),
		Manifest: store,
	})
	params := mustJSON(t, ShareRelocationParams{Share: "docs", To: "cache"})

	// Interrupt the relocation right after its manifest becomes durable —
	// before it ever calls Sync — the same way a daemon restart or
	// maintenance mode would (doc 01 §4 Q70). It no longer holds the
	// scheduler's Array-write slot from here on.
	var lastCheckpoint []byte
	stopRequested, saveCheckpoint := interruptOnceManifestDurable(&lastCheckpoint)
	rc1 := &RunContext{
		ctx:            ctx,
		out:            &bytes.Buffer{},
		params:         params,
		stopRequested:  stopRequested,
		saveCheckpoint: saveCheckpoint,
		setProgress:    func(int) {},
	}
	if err := fn(ctx, rc1); err != nil {
		t.Fatalf("interrupted run: %v", err)
	}

	// The relocation is now merely "outstanding" (durably persisted, not
	// running as a scheduler job at all) — a real, independently scheduled
	// TypeSync job runs through the real Scheduler and the production
	// RunSync wiring, sharing only the database with the relocation above.
	jobStore := NewStore(db)
	s := NewScheduler(jobStore, NewLogStore(t.TempDir()), NewHub(), NewRegistry())
	syncEng := &storeBackedEngine{recordingEngine: newRecordingEngine(), store: store}
	syncEng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)
	s.registry.Register(TypeSync, false, RunSync(syncEng))

	syncJob, err := s.Submit(ctx, TypeSync, nil, mustJSON(t, SyncParams{}))
	if err != nil {
		t.Fatalf("Submit sync during outstanding relocation: %v", err)
	}
	syncFinished := await(t, s, syncJob.ID)
	if syncFinished.Status != StatusSucceeded {
		t.Fatalf("sync status = %s (%s), want succeeded", syncFinished.Status, syncFinished.ErrorMessage)
	}
	lastSync, _, _, _ := syncEng.snapshot()
	if len(lastSync.Manifest) != 1 || lastSync.Manifest[0].RelPath != "docs/report.pdf" {
		t.Fatalf("sync's own SyncOpts.Manifest while the relocation is outstanding = %+v, want the relocation's own manifest, not empty", lastSync.Manifest)
	}

	// Resume the relocation to completion — its own trailing sync clears
	// the manifest.
	rc2 := &RunContext{
		ctx:           ctx,
		out:           &bytes.Buffer{},
		params:        params,
		checkpoint:    lastCheckpoint,
		stopRequested: make(chan struct{}),
		saveCheckpoint: func(data []byte) error {
			lastCheckpoint = data
			return nil
		},
		setProgress: func(int) {},
	}
	if err := fn(ctx, rc2); err != nil {
		t.Fatalf("resumed run: %v", err)
	}

	laterSyncJob, err := s.Submit(ctx, TypeSync, nil, mustJSON(t, SyncParams{}))
	if err != nil {
		t.Fatalf("Submit later sync: %v", err)
	}
	laterFinished := await(t, s, laterSyncJob.ID)
	if laterFinished.Status != StatusSucceeded {
		t.Fatalf("later sync status = %s (%s), want succeeded", laterFinished.Status, laterFinished.ErrorMessage)
	}
	laterSync, _, _, _ := syncEng.snapshot()
	if len(laterSync.Manifest) != 0 {
		t.Fatalf("a sync after the relocation completed still saw a manifest = %+v, want it cleared so an unrelated later removal at the same disk+path is not wrongly accounted", laterSync.Manifest)
	}
}

// TestRunShareRelocation_ToCache_FinalSyncFailure_ClearsManifest_FollowingSyncCountsRemovals
// replaces #247's TestRunShareRelocation_ToCache_FinalSyncFailure_LeavesManifestPersisted,
// whose expectation is inverted on purpose (#732). A relocation whose
// trailing sync failed ends failed, and a failed job is never resumed, so
// nothing is left that still needs its manifest; keeping it would exempt
// removals at those disk+paths from every later sync's guard for as long as
// the row survived. With it cleared, the next sync counts the same removals
// toward RemovedFilesMax and may block until the user confirms: the guard is
// stricter, not weaker, and nothing is lost because the copies are on the
// cache.
func TestRunShareRelocation_ToCache_FinalSyncFailure_ClearsManifest_FollowingSyncCountsRemovals(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	store := parity.NewRelocationManifestStore(db)

	s := newTestScheduler(t)
	share := newShareRelocationShare(t, "docs")
	src := filepath.Join(share.Branches[0], "report.pdf")
	mustWriteFile(t, src, "report bytes")

	eng := newRecordingEngine()
	eng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)

	var calls int
	failFinal := errors.New("threshold guard blocked the trailing sync")
	sync := func(ctx context.Context, manifest []parity.ManifestEntry) error {
		calls++
		if calls == 2 {
			return failFinal
		}
		return syncFuncFromEngine(eng)(ctx, manifest)
	}

	s.registry.Register(TypeShareRelocation, true, RunShareRelocation(ShareRelocationDeps{
		Open:     fakeOpen(),
		Share:    func(context.Context, string) (cache.Share, error) { return share, nil },
		Sync:     sync,
		Manifest: store,
	}))
	syncEng := &storeBackedEngine{recordingEngine: newRecordingEngine(), store: store}
	syncEng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)
	s.registry.Register(TypeSync, false, RunSync(syncEng))

	j, err := s.Submit(ctx, TypeShareRelocation, nil, mustJSON(t, ShareRelocationParams{Share: "docs", To: "cache"}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s, want failed", finished.Status)
	}
	if !strings.Contains(finished.ErrorMessage, failFinal.Error()) {
		t.Fatalf("ErrorMessage = %q, want it to surface the trailing sync's own failure", finished.ErrorMessage)
	}
	if calls != 2 {
		t.Fatalf("expected exactly 2 sync calls (pre-delete, trailing), got %d", calls)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("array original should already be gone before the trailing sync failed: err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(share.CachePath, "report.pdf")); err != nil {
		t.Fatalf("the cache copy must survive a failed trailing sync: %v", err)
	}

	manifest, removingDisks, err := store.Current(ctx)
	if err != nil {
		t.Fatalf("store.Current after a failed trailing sync: %v", err)
	}
	if len(manifest) != 0 || len(removingDisks) != 0 {
		t.Fatalf("manifest after a failed trailing sync = %+v/%+v, want it cleared", manifest, removingDisks)
	}

	syncJob, err := s.Submit(ctx, TypeSync, nil, mustJSON(t, SyncParams{}))
	if err != nil {
		t.Fatalf("Submit following sync: %v", err)
	}
	if got := await(t, s, syncJob.ID); got.Status != StatusSucceeded {
		t.Fatalf("following sync status = %s (%s), want succeeded", got.Status, got.ErrorMessage)
	}
	lastSync, _, _, _ := syncEng.snapshot()
	if len(lastSync.Manifest) != 0 {
		t.Fatalf("the following sync's SyncOpts.Manifest = %+v, want empty so the guard counts the relocation's removals", lastSync.Manifest)
	}
}

// TestRunShareRelocation_ToCache_UnreadableLeftoverAfterFinalSync_ClearsManifestAndEndsIncomplete
// covers the end-of-job check of what is left on the array: once the
// trailing sync has succeeded the manifest is spent, so a directory the
// check cannot read must neither keep the manifest persisted nor turn a
// finished relocation into a plain failure. It ends failed with
// relocation_incomplete, naming the path and the error.
func TestRunShareRelocation_ToCache_UnreadableLeftoverAfterFinalSync_ClearsManifestAndEndsIncomplete(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory without permission")
	}
	ctx := context.Background()
	db := newTestDB(t)
	store := parity.NewRelocationManifestStore(db)

	s := newTestScheduler(t)
	share := newShareRelocationShare(t, "docs")
	mustWriteFile(t, filepath.Join(share.Branches[0], "report.pdf"), "report bytes")
	locked := filepath.Join(share.Branches[0], "locked")

	eng := newRecordingEngine()
	eng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)

	var calls int
	sync := func(ctx context.Context, manifest []parity.ManifestEntry) error {
		calls++
		if err := syncFuncFromEngine(eng)(ctx, manifest); err != nil {
			return err
		}
		if calls == 2 {
			mustWriteFile(t, filepath.Join(locked, "hidden.db"), "x")
			if err := os.Chmod(locked, 0o000); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
		}
		return nil
	}

	s.registry.Register(TypeShareRelocation, true, RunShareRelocation(ShareRelocationDeps{
		Open:     fakeOpen(),
		Share:    func(context.Context, string) (cache.Share, error) { return share, nil },
		Sync:     sync,
		Manifest: store,
	}))

	j, err := s.Submit(ctx, TypeShareRelocation, nil, mustJSON(t, ShareRelocationParams{Share: "docs", To: "cache"}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed || finished.ErrorCode != "relocation_incomplete" {
		t.Fatalf("status = %s, code = %q (%s), want failed / relocation_incomplete", finished.Status, finished.ErrorCode, finished.ErrorMessage)
	}
	if !strings.Contains(finished.ErrorMessage, "locked") || !strings.Contains(finished.ErrorMessage, "permission denied") {
		t.Fatalf("ErrorMessage = %q, want it to name the unreadable path and its error", finished.ErrorMessage)
	}
	if calls != 2 {
		t.Fatalf("expected exactly 2 sync calls (pre-delete, trailing), got %d", calls)
	}

	manifest, _, err := store.Current(ctx)
	if err != nil {
		t.Fatalf("store.Current: %v", err)
	}
	if len(manifest) != 0 {
		t.Fatalf("manifest after the trailing sync succeeded = %+v, want it cleared", manifest)
	}
}

// TestRunShareRelocation_ToCache_ManifestSurvivesInterruption_ThenClearsOnResume
// proves the resumed/checkpoint-continuation path this issue's own
// acceptance criteria call out explicitly: a run interrupted right after
// the copy phase makes its manifest durable (before ever calling Sync)
// must leave that manifest persisted across the interruption, and the
// resumed run that actually completes the relocation must still clear it.
func TestRunShareRelocation_ToCache_ManifestSurvivesInterruption_ThenClearsOnResume(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	store := parity.NewRelocationManifestStore(db)

	share := newShareRelocationShare(t, "docs")
	src := filepath.Join(share.Branches[0], "report.pdf")
	mustWriteFile(t, src, "report bytes")

	eng := newRecordingEngine()
	eng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)

	fn := RunShareRelocation(ShareRelocationDeps{
		Open:     fakeOpen(),
		Share:    func(context.Context, string) (cache.Share, error) { return share, nil },
		Sync:     syncFuncFromEngine(eng),
		Manifest: store,
	})
	params := mustJSON(t, ShareRelocationParams{Share: "docs", To: "cache"})

	// Run 1: interrupted right after the copy phase's own checkpoint save
	// makes the manifest durable, before RelocateToCache ever calls Sync.
	var lastCheckpoint []byte
	stopRequested, saveCheckpoint := interruptOnceManifestDurable(&lastCheckpoint)
	rc1 := &RunContext{
		ctx:            ctx,
		out:            &bytes.Buffer{},
		params:         params,
		stopRequested:  stopRequested,
		saveCheckpoint: saveCheckpoint,
		setProgress:    func(int) {},
	}
	if err := fn(ctx, rc1); err != nil {
		t.Fatalf("interrupted run: %v", err)
	}
	if lastCheckpoint == nil {
		t.Fatal("interrupted run never saved a checkpoint — nothing to resume from")
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("array original must survive an interrupted run: %v", err)
	}

	manifest, _, err := store.Current(ctx)
	if err != nil {
		t.Fatalf("store.Current after an interrupted run: %v", err)
	}
	if len(manifest) != 1 || manifest[0].RelPath != "docs/report.pdf" {
		t.Fatalf("manifest after an interrupted run = %+v, want the copy phase's own manifest durably persisted", manifest)
	}

	// Run 2: resumed from run 1's own checkpoint, run to completion.
	rc2 := &RunContext{
		ctx:           ctx,
		out:           &bytes.Buffer{},
		params:        params,
		checkpoint:    lastCheckpoint,
		stopRequested: make(chan struct{}),
		saveCheckpoint: func(data []byte) error {
			lastCheckpoint = data
			return nil
		},
		setProgress: func(int) {},
	}
	if err := fn(ctx, rc2); err != nil {
		t.Fatalf("resumed run: %v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("array original should be gone once the resumed run completes: err=%v", err)
	}

	manifest, removingDisks, err := store.Current(ctx)
	if err != nil {
		t.Fatalf("store.Current after the resumed run completes: %v", err)
	}
	if manifest != nil || removingDisks != nil {
		t.Fatalf("manifest after the resumed run completes = %+v/%+v, want cleared", manifest, removingDisks)
	}
}

// countingManifestReplacer wraps a real *parity.RelocationManifestStore,
// counting Replace calls while still writing through to it — #247's own
// bound-pinning test needs a real store (so the other manifest tests'
// store.Current assertions keep meaning something) but also needs to
// observe how many DELETE+INSERT transactions a run actually issues.
type countingManifestReplacer struct {
	store *parity.RelocationManifestStore
	calls int
}

func (c *countingManifestReplacer) Replace(ctx context.Context, manifest []parity.ManifestEntry, removingDisks map[string]bool) error {
	c.calls++
	return c.store.Replace(ctx, manifest, removingDisks)
}

func (c *countingManifestReplacer) Current(ctx context.Context) ([]parity.ManifestEntry, map[string]bool, error) {
	return c.store.Current(ctx)
}

// TestRunShareRelocation_ToCache_PersistsManifestOnce_NotPerCheckpoint pins
// this issue's own fix: relocateDeletePhase (internal/cache/relocate.go)
// checkpoints after every deleted file, and RelocateToCache checkpoints
// again at each phase transition, but every one of those checkpoints after
// the first carries the identical manifest the copy phase already built
// (RelocateCheckpoint's own doc comment) — so persisting it again each
// time is pure write amplification, not new information for a concurrent
// sync to see. With several files (several delete-phase checkpoints),
// store.Replace — a DELETE-then-N-INSERT transaction — must still fire
// exactly once for the whole run, not once per checkpoint/deleted file.
func TestRunShareRelocation_ToCache_PersistsManifestOnce_NotPerCheckpoint(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	replacer := &countingManifestReplacer{store: parity.NewRelocationManifestStore(db)}

	s := newTestScheduler(t)
	share := newShareRelocationShare(t, "docs")
	const fileCount = 5
	for i := 0; i < fileCount; i++ {
		mustWriteFile(t, filepath.Join(share.Branches[0], fmt.Sprintf("f%d.txt", i)), "content")
	}

	eng := newRecordingEngine()
	eng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)

	s.registry.Register(TypeShareRelocation, true, RunShareRelocation(ShareRelocationDeps{
		Open:     fakeOpen(),
		Share:    func(context.Context, string) (cache.Share, error) { return share, nil },
		Sync:     syncFuncFromEngine(eng),
		Manifest: replacer,
	}))

	j, err := s.Submit(ctx, TypeShareRelocation, nil, mustJSON(t, ShareRelocationParams{Share: "docs", To: "cache"}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}

	// The write path's own bound: one persist for the whole run, whatever
	// happened to the delete phase's own checkpoint. The trailing clear
	// (Replace(ctx, nil, nil) once the relocation's own final sync
	// succeeds) is the run's second and last call.
	if replacer.calls != 2 {
		t.Fatalf("store.Replace called %d times for a %d-file relocation, want exactly 2 (one persist, one clear) — not one per checkpoint/deleted file", replacer.calls, fileCount)
	}

	manifest, removingDisks, err := replacer.store.Current(ctx)
	if err != nil {
		t.Fatalf("store.Current after completion: %v", err)
	}
	if manifest != nil || removingDisks != nil {
		t.Fatalf("manifest after a completed relocation = %+v/%+v, want cleared", manifest, removingDisks)
	}
}

// TestRunShareRelocation_ToCache_CancelBetweenFinalSyncAndClear_ClearSurvives
// is #381's own regression test: a Cancel landing the instant the
// relocation's own trailing sync succeeds — before RunShareRelocation's own
// clear runs — must not leave the clear itself failing with
// context.Canceled, since cache.RelocateToCache never checks ctx again
// after that sync call returns (internal/cache/relocate.go). A clear that
// failed that way would be indistinguishable from the RunFunc's own
// reaction to the cancel (isCancellationDerived, scheduler.go) and silently
// dropped, stranding the manifest so it could wrongly exempt a later,
// unrelated removal at the same disk+path from the guard (doc 02 §4, Q15).
// Reuses contextCheckingManifestStore (evacuation_run_test.go), which fails
// Replace exactly the way the real store's BeginTx(ctx) would on an
// already-cancelled context, so this fails on the pre-fix code — which
// clears on the job's own cancellable ctx and leaves the manifest
// persisted — and passes once the clear runs on context.WithoutCancel, the
// same fix #378 made for evacuation.
func TestRunShareRelocation_ToCache_CancelBetweenFinalSyncAndClear_ClearSurvives(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	share := newShareRelocationShare(t, "docs")
	src := filepath.Join(share.Branches[0], "report.pdf")
	mustWriteFile(t, src, "report bytes")

	manifestStore := contextCheckingManifestStore{&fakeRelocationManifestStore{}}

	var calls int
	sync := func(context.Context, []parity.ManifestEntry) error {
		calls++
		if calls == 2 {
			// The second call is the trailing sync (RelocatePhaseFinalSync,
			// after the delete phase). RelocateToCache never checks ctx
			// again once this call returns, so a Cancel landing exactly
			// here reaches RunShareRelocation's own clear with err == nil,
			// report.Interrupted == false — precisely the race #381
			// describes.
			cancel()
		}
		return nil
	}

	fn := RunShareRelocation(ShareRelocationDeps{
		Open:     fakeOpen(),
		Share:    func(context.Context, string) (cache.Share, error) { return share, nil },
		Sync:     sync,
		Manifest: manifestStore,
	})

	rc := &RunContext{
		ctx:            ctx,
		out:            &bytes.Buffer{},
		params:         mustJSON(t, ShareRelocationParams{Share: "docs", To: "cache"}),
		stopRequested:  make(chan struct{}),
		saveCheckpoint: func([]byte) error { return nil },
		setProgress:    func(int) {},
	}

	if err := fn(ctx, rc); err != nil {
		t.Fatalf("relocation with a cancel landing right after its trailing sync succeeded: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected exactly 2 sync calls (pre-delete, trailing), got %d", calls)
	}

	_, manifest, removingDisks := manifestStore.snapshot()
	if manifest != nil || removingDisks != nil {
		t.Fatalf("manifest after a cancel landing between the trailing sync and the clear = %+v/%+v, want cleared despite the cancel", manifest, removingDisks)
	}
}

// TestRunShareRelocation_ToCache_ClearFailure_SurfacesAsPlainFailure proves
// #381's acceptance criterion that a clear failing for a genuine, unrelated
// reason (no cancel involved at all) is still recorded on the job rather
// than dropped: moving the clear onto context.WithoutCancel(ctx) must not
// stop a plain clear failure from surfacing — runJob's own plain-failure
// path (reason != reasonCancel, "job_failed") keeps this error's message
// unchanged, exactly as TestScheduler_ShareRelocationCancelRace_BeforeReturn_ManifestClearFailureIsNotErased
// (scheduler_cancel_race_test.go, #379) already proves for the raced-Cancel
// case via runJob's own identity-based isCancellationDerived classification
// — this fix leaves that plain-error shape unchanged.
func TestRunShareRelocation_ToCache_ClearFailure_SurfacesAsPlainFailure(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	share := newShareRelocationShare(t, "docs")
	src := filepath.Join(share.Branches[0], "report.pdf")
	mustWriteFile(t, src, "report bytes")

	eng := newRecordingEngine()
	eng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)

	manifestStore := failClearManifestStore{
		fakeRelocationManifestStore: &fakeRelocationManifestStore{},
		err:                         errors.New("disk full"),
	}

	s.registry.Register(TypeShareRelocation, true, RunShareRelocation(ShareRelocationDeps{
		Open:     fakeOpen(),
		Share:    func(context.Context, string) (cache.Share, error) { return share, nil },
		Sync:     syncFuncFromEngine(eng),
		Manifest: manifestStore,
	}))

	j, err := s.Submit(ctx, TypeShareRelocation, nil, mustJSON(t, ShareRelocationParams{Share: "docs", To: "cache"}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s, want failed — the relocation itself succeeded, only its own clear failed", finished.Status)
	}
	if finished.ErrorCode != "job_failed" {
		t.Fatalf("ErrorCode = %q, want %q", finished.ErrorCode, "job_failed")
	}
	if !strings.Contains(finished.ErrorMessage, "disk full") {
		t.Fatalf("ErrorMessage = %q, want it to surface the failed clear rather than dropping it", finished.ErrorMessage)
	}
}

// TestRunShareRelocation_UnknownShareFailsTheJob proves a share the
// caller cannot resolve fails the job rather than relocating nothing
// silently — mirroring TestRunMover_SharesErrorFailsTheJob.
func TestRunShareRelocation_UnknownShareFailsTheJob(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	wantErr := errors.New("no such share")

	s.registry.Register(TypeShareRelocation, true, RunShareRelocation(ShareRelocationDeps{
		Open:  fakeOpen(),
		Share: func(context.Context, string) (cache.Share, error) { return cache.Share{}, wantErr },
	}))

	j, err := s.Submit(ctx, TypeShareRelocation, nil, mustJSON(t, ShareRelocationParams{Share: "ghost", To: "array"}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s, want failed", finished.Status)
	}
	if !strings.Contains(finished.ErrorMessage, "no such share") {
		t.Fatalf("ErrorMessage = %q, want it to surface the share-resolution failure", finished.ErrorMessage)
	}
}

func TestValidateParams_ShareRelocationRequiresShareAndValidDirection(t *testing.T) {
	for _, params := range [][]byte{
		nil,
		[]byte("null"),
		[]byte(""),
		[]byte(`{"to":"array"}`),
		[]byte(`{"share":"docs"}`),
		[]byte(`{"share":"docs","to":"elsewhere"}`),
	} {
		if err := ValidateParams(TypeShareRelocation, params); err == nil {
			t.Fatalf("ValidateParams(share_relocation, %q) = nil, want rejection", params)
		}
	}
	if err := ValidateParams(TypeShareRelocation, mustJSON(t, ShareRelocationParams{Share: "docs", To: "cache"})); err != nil {
		t.Fatalf("ValidateParams(share_relocation, valid) = %v, want nil", err)
	}
}

// TestRunShareRelocation_ToCache_LeftBehindEntryFailsTheJobAndClearsTheManifest
// proves a relocation that finished but could not move every entry ends
// failed with its own code, names the entry left behind and why, and still
// clears the persisted relocation manifest, so nothing stale is left to
// exempt a later removal from the guard.
func TestRunShareRelocation_ToCache_LeftBehindEntryFailsTheJobAndClearsTheManifest(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	share := newShareRelocationShare(t, "docs")
	moved := filepath.Join(share.Branches[0], "report.pdf")
	held := filepath.Join(share.Branches[0], "held.db")
	mustWriteFile(t, moved, "report bytes")
	mustWriteFile(t, held, "database bytes")

	open := fakeOpen()
	open.SetOpen(held, true)
	eng := newRecordingEngine()
	eng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)
	manifestStore := &fakeRelocationManifestStore{}

	s.registry.Register(TypeShareRelocation, true, RunShareRelocation(ShareRelocationDeps{
		Open:     open,
		Share:    func(context.Context, string) (cache.Share, error) { return share, nil },
		Sync:     syncFuncFromEngine(eng),
		Manifest: manifestStore,
	}))

	j, err := s.Submit(ctx, TypeShareRelocation, nil, mustJSON(t, ShareRelocationParams{Share: "docs", To: "cache"}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed || finished.ErrorCode != "relocation_incomplete" {
		t.Fatalf("status = %s, code = %q (%s), want failed / relocation_incomplete", finished.Status, finished.ErrorCode, finished.ErrorMessage)
	}
	if !strings.Contains(finished.ErrorMessage, "held.db") || !strings.Contains(finished.ErrorMessage, "skipped_open") {
		t.Fatalf("ErrorMessage = %q, want the entry left behind and why", finished.ErrorMessage)
	}
	if strings.Contains(finished.ErrorMessage, "report.pdf") {
		t.Fatalf("ErrorMessage = %q names an entry that was moved", finished.ErrorMessage)
	}
	if _, err := os.Stat(held); err != nil {
		t.Fatalf("the held file must stay on the array: %v", err)
	}
	calls, manifest, _ := manifestStore.snapshot()
	if calls < 2 || manifest != nil {
		t.Fatalf("manifest store: %d Replace calls, final manifest %+v, want it persisted then cleared", calls, manifest)
	}
}

func TestRunShareRelocation_ToArray_LeftBehindEntryFailsTheJob(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	base := t.TempDir()
	share := cache.Share{
		Name:      "docs",
		CachePath: filepath.Join(base, "cache", "docs"),
		ArrayPath: filepath.Join(base, "array", "docs"),
	}
	if err := os.MkdirAll(share.ArrayPath, 0o755); err != nil {
		t.Fatal(err)
	}
	held := filepath.Join(share.CachePath, "held.db")
	mustWriteFile(t, held, "database bytes")
	open := fakeOpen()
	open.SetOpen(held, true)

	s.registry.Register(TypeShareRelocation, true, RunShareRelocation(ShareRelocationDeps{
		Open:  open,
		Share: func(context.Context, string) (cache.Share, error) { return share, nil },
	}))
	j, err := s.Submit(ctx, TypeShareRelocation, nil, mustJSON(t, ShareRelocationParams{Share: "docs", To: "array"}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed || finished.ErrorCode != "relocation_incomplete" || !strings.Contains(finished.ErrorMessage, "held.db") {
		t.Fatalf("status = %s, code = %q (%s), want failed / relocation_incomplete naming held.db", finished.Status, finished.ErrorCode, finished.ErrorMessage)
	}
}

// TestRunShareRelocation_ToArray_OnlyASocketLeftStillSucceeds: a socket is
// runtime-only, so leaving it behind does not make the relocation incomplete.
func TestRunShareRelocation_ToArray_OnlyASocketLeftStillSucceeds(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	base := t.TempDir()
	share := cache.Share{
		Name:      "docs",
		CachePath: filepath.Join(base, "cache", "docs"),
		ArrayPath: filepath.Join(base, "array", "docs"),
	}
	if err := os.MkdirAll(share.ArrayPath, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(share.CachePath, "report.pdf"), "report bytes")
	dir, err := os.Open(share.CachePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: fmt.Sprintf("/proc/self/fd/%d/app.sock", dir.Fd()), Net: "unix"})
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	l.SetUnlinkOnClose(false)
	_ = l.Close()

	s.registry.Register(TypeShareRelocation, true, RunShareRelocation(ShareRelocationDeps{
		Open:  fakeOpen(),
		Share: func(context.Context, string) (cache.Share, error) { return share, nil },
	}))
	j, err := s.Submit(ctx, TypeShareRelocation, nil, mustJSON(t, ShareRelocationParams{Share: "docs", To: "array"}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if finished := await(t, s, j.ID); finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	if _, err := os.Lstat(filepath.Join(share.CachePath, "app.sock")); err != nil {
		t.Fatalf("the socket must be left in place: %v", err)
	}
}

// TestRunShareRelocation_ToCache_ResumedRunStillFailsOnWhatAnEarlierRunLeftBehind:
// held.db is skipped as open in the first run, which is interrupted once the
// copy phase is done. The resumed run starts after the copy phase, so only
// the end-of-job check of the array side can still name held.db.
func TestRunShareRelocation_ToCache_ResumedRunStillFailsOnWhatAnEarlierRunLeftBehind(t *testing.T) {
	ctx := context.Background()
	share := newShareRelocationShare(t, "docs")
	held := filepath.Join(share.Branches[0], "held.db")
	mustWriteFile(t, filepath.Join(share.Branches[0], "report.pdf"), "report bytes")
	mustWriteFile(t, held, "database bytes")
	open := fakeOpen()
	open.SetOpen(held, true)
	eng := newRecordingEngine()
	eng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)

	fn := RunShareRelocation(ShareRelocationDeps{
		Open:  open,
		Share: func(context.Context, string) (cache.Share, error) { return share, nil },
		Sync:  syncFuncFromEngine(eng),
	})
	params := mustJSON(t, ShareRelocationParams{Share: "docs", To: "cache"})

	stop := make(chan struct{})
	var last []byte
	rc1 := &RunContext{
		ctx:           ctx,
		out:           &bytes.Buffer{},
		params:        params,
		stopRequested: stop,
		saveCheckpoint: func(data []byte) error {
			last = data
			var cp cache.RelocateCheckpoint
			if err := json.Unmarshal(data, &cp); err != nil {
				return err
			}
			if cp.Phase == cache.RelocatePhaseSyncing {
				close(stop)
			}
			return nil
		},
		setProgress: func(int) {},
	}
	if err := fn(ctx, rc1); err != nil {
		t.Fatalf("interrupted run: %v", err)
	}
	if last == nil {
		t.Fatal("interrupted run never saved a checkpoint")
	}

	rc2 := &RunContext{
		ctx:            ctx,
		out:            &bytes.Buffer{},
		params:         params,
		checkpoint:     last,
		stopRequested:  make(chan struct{}),
		saveCheckpoint: func([]byte) error { return nil },
		setProgress:    func(int) {},
	}
	err := fn(ctx, rc2)
	var outcome *OutcomeError
	if !errors.As(err, &outcome) || outcome.Status != StatusFailed || outcome.Code != relocationIncompleteCode {
		t.Fatalf("resumed run error = %v, want failed / %s", err, relocationIncompleteCode)
	}
	if !strings.Contains(outcome.Error(), "held.db") {
		t.Fatalf("error = %q, want it to name held.db", outcome.Error())
	}
	if _, err := os.Stat(held); err != nil {
		t.Fatalf("the held file must stay on the array: %v", err)
	}
}

// foreignManifests are persisted manifests that some other job owns while a
// "docs" relocation to the cache fails or is cancelled: a rebalance of the
// same share (array target), another share's cache relocation, and an
// evacuation's (removing disks set). The relocation must leave each alone.
func foreignManifests(share cache.Share) map[string]struct {
	manifest      []parity.ManifestEntry
	removingDisks map[string]bool
} {
	type m = struct {
		manifest      []parity.ManifestEntry
		removingDisks map[string]bool
	}
	return map[string]m{
		"rebalance of the same share": {manifest: []parity.ManifestEntry{
			{RelPath: "docs/x.txt", SourceDisk: "/mnt/disk1", TargetDisk: "/mnt/disk2"},
		}},
		"relocation of another share to the cache": {manifest: []parity.ManifestEntry{
			{RelPath: "photos/x.jpg", SourceDisk: "/mnt/disk1", TargetDisk: filepath.Dir(share.CachePath)},
		}},
		"evacuation": {
			manifest:      []parity.ManifestEntry{{RelPath: "docs/x.txt", SourceDisk: "/mnt/disk1", TargetDisk: "/mnt/disk2"}},
			removingDisks: map[string]bool{"/mnt/disk1": true},
		},
	}
}

func requireManifestUntouched(t *testing.T, store *parity.RelocationManifestStore, wantManifest []parity.ManifestEntry, wantRemoving map[string]bool) {
	t.Helper()
	gotManifest, gotRemoving, err := store.Current(context.Background())
	if err != nil {
		t.Fatalf("store.Current: %v", err)
	}
	if len(gotManifest) != len(wantManifest) || len(gotRemoving) != len(wantRemoving) {
		t.Fatalf("store = %+v/%+v, want another job's manifest %+v/%+v left alone", gotManifest, gotRemoving, wantManifest, wantRemoving)
	}
	for i := range wantManifest {
		if gotManifest[i].RelPath != wantManifest[i].RelPath || gotManifest[i].SourceDisk != wantManifest[i].SourceDisk || gotManifest[i].TargetDisk != wantManifest[i].TargetDisk {
			t.Fatalf("store manifest = %+v, want %+v", gotManifest, wantManifest)
		}
	}
	for k := range wantRemoving {
		if !gotRemoving[k] {
			t.Fatalf("store removing disks = %+v, want %+v", gotRemoving, wantRemoving)
		}
	}
}

// TestRunShareRelocation_ToCache_GuardBlockedFirstSync_ClearsManifest is
// #732's central case: the first guarded sync is blocked, the job fails, and
// a failed job is never resumed, so the manifest persisted right before that
// sync must not outlive it. Without the clear the stored array→cache entries
// would keep exempting later removals at those paths from the guard.
func TestRunShareRelocation_ToCache_GuardBlockedFirstSync_ClearsManifest(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	store := parity.NewRelocationManifestStore(db)

	s := newTestScheduler(t)
	share := newShareRelocationShare(t, "docs")
	src := filepath.Join(share.Branches[0], "report.pdf")
	mustWriteFile(t, src, "report bytes")

	var sawPersisted int
	eng := newRecordingEngine()
	eng.ScriptGuardBlock(trippedGuard())
	sync := func(ctx context.Context, manifest []parity.ManifestEntry) error {
		persisted, _, err := store.Current(ctx)
		if err != nil {
			t.Fatalf("store.Current inside the sync call: %v", err)
		}
		sawPersisted = len(persisted)
		return syncFuncFromEngine(eng)(ctx, manifest)
	}
	s.registry.Register(TypeShareRelocation, true, RunShareRelocation(ShareRelocationDeps{
		Open:     fakeOpen(),
		Share:    func(context.Context, string) (cache.Share, error) { return share, nil },
		Sync:     sync,
		Manifest: store,
	}))

	j, err := s.Submit(ctx, TypeShareRelocation, nil, mustJSON(t, ShareRelocationParams{Share: "docs", To: "cache"}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed || !strings.Contains(finished.ErrorMessage, "threshold guard blocked the sync") {
		t.Fatalf("status = %s (%s), want failed on the guard block", finished.Status, finished.ErrorMessage)
	}
	if sawPersisted != 1 {
		t.Fatalf("manifest entries durable during the blocked sync = %d, want 1 (the case this test is about)", sawPersisted)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("array original must survive a blocked sync: %v", err)
	}

	manifest, removingDisks, err := store.Current(ctx)
	if err != nil {
		t.Fatalf("store.Current: %v", err)
	}
	if len(manifest) != 0 || len(removingDisks) != 0 {
		t.Fatalf("manifest after a guard-blocked relocation = %+v/%+v, want cleared", manifest, removingDisks)
	}
}

// TestRunShareRelocation_ToCache_FailureLeavesAnotherJobsManifestAlone is
// #732's ownership check on the run path: by the time a failed relocation
// reaches its clear, the single stored slot may hold a manifest some other
// job wrote; that one must survive.
func TestRunShareRelocation_ToCache_FailureLeavesAnotherJobsManifestAlone(t *testing.T) {
	probe := newShareRelocationShare(t, "docs")
	for name := range foreignManifests(probe) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := parity.NewRelocationManifestStore(newTestDB(t))
			s := newTestScheduler(t)
			share := newShareRelocationShare(t, "docs")
			mustWriteFile(t, filepath.Join(share.Branches[0], "report.pdf"), "report bytes")
			foreign := foreignManifests(share)[name]

			sync := func(ctx context.Context, _ []parity.ManifestEntry) error {
				if err := store.Replace(ctx, foreign.manifest, foreign.removingDisks); err != nil {
					t.Fatalf("another job replacing the manifest: %v", err)
				}
				return errors.New("sync failed")
			}
			s.registry.Register(TypeShareRelocation, true, RunShareRelocation(ShareRelocationDeps{
				Open:     fakeOpen(),
				Share:    func(context.Context, string) (cache.Share, error) { return share, nil },
				Sync:     sync,
				Manifest: store,
			}))

			j, err := s.Submit(ctx, TypeShareRelocation, nil, mustJSON(t, ShareRelocationParams{Share: "docs", To: "cache"}))
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}
			if finished := await(t, s, j.ID); finished.Status != StatusFailed {
				t.Fatalf("status = %s (%s), want failed", finished.Status, finished.ErrorMessage)
			}
			requireManifestUntouched(t, store, foreign.manifest, foreign.removingDisks)
		})
	}
}

// interruptedRelocationHarness drives a to-cache relocation of "docs"
// through a real Scheduler to a graceful maintenance interrupt right after
// its manifest became durable, with the abort registered the way hoservad
// registers it, and returns the interrupted job and a switch that makes the
// share lookup fail from then on.
func interruptedRelocationHarness(t *testing.T, store *parity.RelocationManifestStore) (*Scheduler, cache.Share, *Job, *atomic.Bool) {
	t.Helper()
	ctx := context.Background()
	s := newTestScheduler(t)
	share := newShareRelocationShare(t, "docs")
	mustWriteFile(t, filepath.Join(share.Branches[0], "report.pdf"), "report bytes")

	eng := newRecordingEngine()
	eng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)
	sync := func(ctx context.Context, manifest []parity.ManifestEntry) error {
		if err := s.EnterMaintenance(ctx); err != nil {
			t.Fatalf("EnterMaintenance: %v", err)
		}
		return syncFuncFromEngine(eng)(ctx, manifest)
	}
	lookupFails := &atomic.Bool{}
	deps := ShareRelocationDeps{
		Open: fakeOpen(),
		Share: func(context.Context, string) (cache.Share, error) {
			if lookupFails.Load() {
				return cache.Share{}, errors.New("share not found")
			}
			return share, nil
		},
		Sync:     sync,
		Manifest: store,
	}
	s.registry.Register(TypeShareRelocation, true, RunShareRelocation(deps))
	s.registry.RegisterAbort(TypeShareRelocation, ShareRelocationAbort(deps))

	j, err := s.Submit(ctx, TypeShareRelocation, nil, mustJSON(t, ShareRelocationParams{Share: "docs", To: "cache"}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	interrupted := await(t, s, j.ID)
	if interrupted.Status != StatusInterrupted {
		t.Fatalf("status = %s (%s), want interrupted", interrupted.Status, interrupted.ErrorMessage)
	}
	manifest, _, err := store.Current(ctx)
	if err != nil {
		t.Fatalf("store.Current: %v", err)
	}
	if len(manifest) != 1 {
		t.Fatalf("manifest after a graceful interrupt = %+v, want it kept for Resume", manifest)
	}
	return s, share, interrupted, lookupFails
}

// TestScheduler_CancelInterruptedShareRelocation_AbortClearsManifest is
// #732's cancel path: Scheduler.Cancel of an interrupted relocation never
// re-enters RunShareRelocation, so only the registered abort can clear the
// manifest the interrupted run kept for Resume. The abort is registered here
// exactly as cmd/hoservad/main.go registers it
// (job.ShareRelocationAbort with the relocation's own deps).
func TestScheduler_CancelInterruptedShareRelocation_AbortClearsManifest(t *testing.T) {
	ctx := context.Background()
	store := parity.NewRelocationManifestStore(newTestDB(t))
	s, _, interrupted, _ := interruptedRelocationHarness(t, store)

	if _, err := s.Cancel(ctx, interrupted.ID); err != nil {
		t.Fatalf("Cancel(interrupted relocation): %v", err)
	}
	if got := await(t, s, interrupted.ID); got.Status != StatusCancelled {
		t.Fatalf("status after Cancel = %s (%s), want cancelled", got.Status, got.ErrorMessage)
	}
	manifest, removingDisks, err := store.Current(ctx)
	if err != nil {
		t.Fatalf("store.Current: %v", err)
	}
	if len(manifest) != 0 || len(removingDisks) != 0 {
		t.Fatalf("manifest after cancelling the interrupted relocation = %+v/%+v, want cleared by the abort", manifest, removingDisks)
	}
}

// TestScheduler_CancelInterruptedShareRelocation_AbortLeavesAnotherJobsManifestAlone
// is the ownership half of the abort: the slot was taken over by another job
// after this relocation was interrupted, and cancelling must not wipe it.
func TestScheduler_CancelInterruptedShareRelocation_AbortLeavesAnotherJobsManifestAlone(t *testing.T) {
	probe := newShareRelocationShare(t, "docs")
	for name := range foreignManifests(probe) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := parity.NewRelocationManifestStore(newTestDB(t))
			s, share, interrupted, _ := interruptedRelocationHarness(t, store)
			foreign := foreignManifests(share)[name]
			if err := store.Replace(ctx, foreign.manifest, foreign.removingDisks); err != nil {
				t.Fatalf("another job replacing the manifest: %v", err)
			}

			if _, err := s.Cancel(ctx, interrupted.ID); err != nil {
				t.Fatalf("Cancel(interrupted relocation): %v", err)
			}
			if got := await(t, s, interrupted.ID); got.Status != StatusCancelled {
				t.Fatalf("status after Cancel = %s (%s), want cancelled", got.Status, got.ErrorMessage)
			}
			requireManifestUntouched(t, store, foreign.manifest, foreign.removingDisks)
		})
	}
}

// erroringCurrentManifestStore fails the ownership read, never touching the
// wrapped store's contents.
type erroringCurrentManifestStore struct {
	*fakeRelocationManifestStore
	err error
}

func (e erroringCurrentManifestStore) Current(context.Context) ([]parity.ManifestEntry, map[string]bool, error) {
	return nil, nil, e.err
}

// TestShareRelocationAbort_UnreadableManifest_FailsAndClearsNothing pins the
// fail direction for an unreadable stored manifest: the abort neither clears
// blind (it could be another job's) nor reports success while a possibly
// stale one remains. It errors, so Cancel leaves the job interrupted and the
// user can retry once the store is readable.
func TestShareRelocationAbort_UnreadableManifest_FailsAndClearsNothing(t *testing.T) {
	ctx := context.Background()
	share := newShareRelocationShare(t, "docs")
	fake := &fakeRelocationManifestStore{manifest: []parity.ManifestEntry{
		{RelPath: "docs/report.pdf", SourceDisk: "/mnt/disk1", TargetDisk: filepath.Dir(share.CachePath)},
	}}
	store := erroringCurrentManifestStore{fakeRelocationManifestStore: fake, err: errors.New("database is locked")}

	abort := ShareRelocationAbort(ShareRelocationDeps{
		Share:    func(context.Context, string) (cache.Share, error) { return share, nil },
		Manifest: store,
	})
	err := abort(ctx, "job-1", mustJSON(t, ShareRelocationParams{Share: "docs", To: "cache"}))
	if err == nil || !strings.Contains(err.Error(), "database is locked") {
		t.Fatalf("abort error = %v, want the unreadable manifest reported", err)
	}
	if calls, _, _ := fake.snapshot(); calls != 0 {
		t.Fatalf("Replace called %d time(s) despite an unreadable manifest, want none", calls)
	}
}

// TestShareRelocationAbort_ToArrayLeavesManifestAlone: only the array→cache
// direction ever writes a manifest, so cancelling a to-array relocation must
// not read or clear the shared slot.
func TestShareRelocationAbort_ToArrayLeavesManifestAlone(t *testing.T) {
	share := newShareRelocationShare(t, "docs")
	fake := &fakeRelocationManifestStore{manifest: []parity.ManifestEntry{
		{RelPath: "docs/report.pdf", SourceDisk: "/mnt/disk1", TargetDisk: filepath.Dir(share.CachePath)},
	}}
	abort := ShareRelocationAbort(ShareRelocationDeps{
		Share:    func(context.Context, string) (cache.Share, error) { return share, nil },
		Manifest: fake,
	})
	if err := abort(context.Background(), "job-1", mustJSON(t, ShareRelocationParams{Share: "docs", To: "array"})); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if calls, manifest, _ := fake.snapshot(); calls != 0 || len(manifest) != 1 {
		t.Fatalf("store = calls %d, manifest %+v, want untouched", calls, manifest)
	}
}

// TestRunShareRelocation_ToCache_RunningCancel_ClearsManifest: a Cancel of a
// running relocation cancels its context, the delete phase reports the run
// interrupted with no error, and ctx.Err() != nil makes that ending not
// resumable (only a graceful stop is), so the manifest must go.
func TestRunShareRelocation_ToCache_RunningCancel_ClearsManifest(t *testing.T) {
	ctx := context.Background()
	store := parity.NewRelocationManifestStore(newTestDB(t))
	s := newTestScheduler(t)
	share := newShareRelocationShare(t, "docs")
	mustWriteFile(t, filepath.Join(share.Branches[0], "report.pdf"), "report bytes")

	started := make(chan struct{})
	release := make(chan struct{})
	sync := func(context.Context, []parity.ManifestEntry) error {
		close(started)
		<-release
		return nil
	}
	s.registry.Register(TypeShareRelocation, true, RunShareRelocation(ShareRelocationDeps{
		Open:     fakeOpen(),
		Share:    func(context.Context, string) (cache.Share, error) { return share, nil },
		Sync:     sync,
		Manifest: store,
	}))

	j, err := s.Submit(ctx, TypeShareRelocation, nil, mustJSON(t, ShareRelocationParams{Share: "docs", To: "cache"}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	<-started
	if _, err := s.Cancel(ctx, j.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	close(release)

	if finished := await(t, s, j.ID); finished.Status != StatusCancelled {
		t.Fatalf("status = %s (%s), want cancelled", finished.Status, finished.ErrorMessage)
	}
	manifest, _, err := store.Current(ctx)
	if err != nil {
		t.Fatalf("store.Current: %v", err)
	}
	if len(manifest) != 0 {
		t.Fatalf("manifest after cancelling a running relocation = %+v, want cleared", manifest)
	}
}

// TestRunShareRelocation_ToCache_KeepForResumeRefused_ClearsManifest: a
// graceful stop is only kept for Resume if rc.KeepForResume() commits; when a
// Cancel has already been accepted it refuses, and the manifest must be
// cleared like any other non-resumable ending.
func TestRunShareRelocation_ToCache_KeepForResumeRefused_ClearsManifest(t *testing.T) {
	for _, keep := range []bool{true, false} {
		t.Run(fmt.Sprintf("keep=%v", keep), func(t *testing.T) {
			ctx := context.Background()
			store := parity.NewRelocationManifestStore(newTestDB(t))
			share := newShareRelocationShare(t, "docs")
			mustWriteFile(t, filepath.Join(share.Branches[0], "report.pdf"), "report bytes")

			var lastCheckpoint []byte
			stopRequested, saveCheckpoint := interruptOnceManifestDurable(&lastCheckpoint)
			rc := &RunContext{
				ctx:            ctx,
				out:            &bytes.Buffer{},
				params:         mustJSON(t, ShareRelocationParams{Share: "docs", To: "cache"}),
				stopRequested:  stopRequested,
				saveCheckpoint: saveCheckpoint,
				setProgress:    func(int) {},
				keepForResume:  func() bool { return keep },
			}
			fn := RunShareRelocation(ShareRelocationDeps{
				Open:     fakeOpen(),
				Share:    func(context.Context, string) (cache.Share, error) { return share, nil },
				Sync:     func(context.Context, []parity.ManifestEntry) error { return nil },
				Manifest: store,
			})
			if err := fn(ctx, rc); err != nil {
				t.Fatalf("interrupted run: %v", err)
			}
			manifest, _, err := store.Current(ctx)
			if err != nil {
				t.Fatalf("store.Current: %v", err)
			}
			if keep && len(manifest) != 1 {
				t.Fatalf("manifest = %+v, want it kept when KeepForResume commits", manifest)
			}
			if !keep && len(manifest) != 0 {
				t.Fatalf("manifest = %+v, want it cleared when KeepForResume refuses", manifest)
			}
		})
	}
}

// TestRunShareRelocation_ToCache_FailureAndClearFailure_ReportsBoth: the
// relocation's own failure stays the primary error and a clear that also
// fails is added to it, not substituted for it and not dropped.
func TestRunShareRelocation_ToCache_FailureAndClearFailure_ReportsBoth(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	share := newShareRelocationShare(t, "docs")
	mustWriteFile(t, filepath.Join(share.Branches[0], "report.pdf"), "report bytes")

	eng := newRecordingEngine()
	eng.ScriptGuardBlock(trippedGuard())
	s.registry.Register(TypeShareRelocation, true, RunShareRelocation(ShareRelocationDeps{
		Open:  fakeOpen(),
		Share: func(context.Context, string) (cache.Share, error) { return share, nil },
		Sync:  syncFuncFromEngine(eng),
		Manifest: failClearManifestStore{
			fakeRelocationManifestStore: &fakeRelocationManifestStore{},
			err:                         errors.New("disk full"),
		},
	}))

	j, err := s.Submit(ctx, TypeShareRelocation, nil, mustJSON(t, ShareRelocationParams{Share: "docs", To: "cache"}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s (%s), want failed", finished.Status, finished.ErrorMessage)
	}
	if !strings.Contains(finished.ErrorMessage, "threshold guard blocked the sync") || !strings.Contains(finished.ErrorMessage, "disk full") {
		t.Fatalf("ErrorMessage = %q, want the relocation's own failure and the failed clear", finished.ErrorMessage)
	}
}

// TestRunShareRelocation_ToCache_FailureWithUnreadableManifest_ReportsBoth:
// the ownership read failing is neither a blind clear nor a silent keep; it
// is reported next to the relocation's own failure.
func TestRunShareRelocation_ToCache_FailureWithUnreadableManifest_ReportsBoth(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	share := newShareRelocationShare(t, "docs")
	mustWriteFile(t, filepath.Join(share.Branches[0], "report.pdf"), "report bytes")

	eng := newRecordingEngine()
	eng.ScriptGuardBlock(trippedGuard())
	fake := &fakeRelocationManifestStore{}
	s.registry.Register(TypeShareRelocation, true, RunShareRelocation(ShareRelocationDeps{
		Open:     fakeOpen(),
		Share:    func(context.Context, string) (cache.Share, error) { return share, nil },
		Sync:     syncFuncFromEngine(eng),
		Manifest: erroringCurrentManifestStore{fakeRelocationManifestStore: fake, err: errors.New("database is locked")},
	}))

	j, err := s.Submit(ctx, TypeShareRelocation, nil, mustJSON(t, ShareRelocationParams{Share: "docs", To: "cache"}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s (%s), want failed", finished.Status, finished.ErrorMessage)
	}
	if !strings.Contains(finished.ErrorMessage, "threshold guard blocked the sync") || !strings.Contains(finished.ErrorMessage, "database is locked") {
		t.Fatalf("ErrorMessage = %q, want the relocation's own failure and the unreadable manifest", finished.ErrorMessage)
	}
	if calls, manifest, _ := fake.snapshot(); len(manifest) != 1 || calls != 1 {
		t.Fatalf("store = calls %d, manifest %+v, want only the persist call and the manifest untouched by a clear", calls, manifest)
	}
}

// TestShareRelocationAbort_UnresolvableShare_StillCancelsAndChecksShape: a
// share that can no longer be resolved must not make the job impossible to
// cancel (Resume would fail on the same lookup). The abort then judges
// ownership by the manifest's own shape: it clears one that lies under the
// share's name and targets a single disk, and still leaves an evacuation's
// (removing disks) or another share's alone.
func TestShareRelocationAbort_UnresolvableShare_StillCancelsAndChecksShape(t *testing.T) {
	for _, c := range []struct {
		name          string
		manifest      []parity.ManifestEntry
		removingDisks map[string]bool
		wantCleared   bool
	}{
		{"own shape", []parity.ManifestEntry{{RelPath: "docs/a.txt", SourceDisk: "/mnt/disk1", TargetDisk: "/mnt/cache"}}, nil, true},
		{"another share", []parity.ManifestEntry{{RelPath: "photos/a.jpg", SourceDisk: "/mnt/disk1", TargetDisk: "/mnt/cache"}}, nil, false},
		{"evacuation", []parity.ManifestEntry{{RelPath: "docs/a.txt", SourceDisk: "/mnt/disk1", TargetDisk: "/mnt/disk2"}}, map[string]bool{"/mnt/disk1": true}, false},
		{"mixed targets", []parity.ManifestEntry{
			{RelPath: "docs/a.txt", SourceDisk: "/mnt/disk1", TargetDisk: "/mnt/cache"},
			{RelPath: "docs/b.txt", SourceDisk: "/mnt/disk1", TargetDisk: "/mnt/disk2"},
		}, nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := &fakeRelocationManifestStore{manifest: c.manifest, removingDisks: c.removingDisks}
			abort := ShareRelocationAbort(ShareRelocationDeps{
				Share:    func(context.Context, string) (cache.Share, error) { return cache.Share{}, errors.New("no such share") },
				Manifest: fake,
			})
			if err := abort(context.Background(), "job-1", mustJSON(t, ShareRelocationParams{Share: "docs", To: "cache"})); err != nil {
				t.Fatalf("abort: %v", err)
			}
			calls, _, _ := fake.snapshot()
			if cleared := calls == 1; cleared != c.wantCleared {
				t.Fatalf("Replace calls = %d, want cleared = %v", calls, c.wantCleared)
			}
		})
	}
}

// TestRunShareRelocation_ToCache_ResumeWithUnresolvableShare_ClearsManifest:
// a relocation interrupted after its manifest became durable is resumed
// after the share can no longer be looked up (deleted, its cache disk
// unassigned, a store read failing). The job ends failed, and a failed job
// can be neither resumed nor cancelled, so the abort never runs; the run
// itself must clear the manifest it kept, or later syncs keep exempting
// removals at those array paths from the guard.
func TestRunShareRelocation_ToCache_ResumeWithUnresolvableShare_ClearsManifest(t *testing.T) {
	ctx := context.Background()
	store := parity.NewRelocationManifestStore(newTestDB(t))
	s, _, interrupted, lookupFails := interruptedRelocationHarness(t, store)

	lookupFails.Store(true)
	s.ExitMaintenance()
	if _, err := s.Resume(ctx, interrupted.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	finished := await(t, s, interrupted.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s (%s), want failed", finished.Status, finished.ErrorMessage)
	}
	if !strings.Contains(finished.ErrorMessage, "share not found") {
		t.Fatalf("ErrorMessage = %q, want the lookup failure reported", finished.ErrorMessage)
	}
	manifest, removingDisks, err := store.Current(ctx)
	if err != nil {
		t.Fatalf("store.Current: %v", err)
	}
	if len(manifest) != 0 || len(removingDisks) != 0 {
		t.Fatalf("manifest after the failed resume = %+v/%+v, want cleared", manifest, removingDisks)
	}
}

func unresolvableShareRun(t *testing.T, store relocationManifestStore, checkpoint []byte, stopRequested <-chan struct{}) error {
	t.Helper()
	ctx := context.Background()
	rc := &RunContext{
		ctx:           ctx,
		out:           &bytes.Buffer{},
		params:        mustJSON(t, ShareRelocationParams{Share: "docs", To: "cache"}),
		checkpoint:    checkpoint,
		stopRequested: stopRequested,
		setProgress:   func(int) {},
	}
	return RunShareRelocation(ShareRelocationDeps{
		Share: func(context.Context, string) (cache.Share, error) {
			return cache.Share{}, errors.New("share not found")
		},
		Manifest: store,
	})(ctx, rc)
}

var ownShapeManifest = []parity.ManifestEntry{{RelPath: "docs/a.txt", SourceDisk: "/mnt/disk1", TargetDisk: "/mnt/cache"}}

// TestRunShareRelocation_ToCache_UnresolvableShare_ReportsFailedClearToo: a
// clear that fails on the lookup-failure path is reported next to the lookup
// error, neither replacing it nor dropped.
func TestRunShareRelocation_ToCache_UnresolvableShare_ReportsFailedClearToo(t *testing.T) {
	store := failClearManifestStore{
		fakeRelocationManifestStore: &fakeRelocationManifestStore{manifest: ownShapeManifest},
		err:                         errors.New("disk full"),
	}
	err := unresolvableShareRun(t, store, []byte(`{}`), nil)
	var cleanup *CancelCleanupError
	if !errors.As(err, &cleanup) {
		t.Fatalf("error = %v, want a *CancelCleanupError carrying both failures", err)
	}
	if !strings.Contains(err.Error(), "share not found") || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("error = %q, want the lookup failure and the failed clear", err)
	}
}

// TestRunShareRelocation_ToCache_UnresolvableShare_FirstRunLeavesManifestAlone:
// a run with no checkpoint has persisted nothing, so whatever the shared slot
// holds is another job's, even if its shape matches this share's (a
// rebalance of the same share does).
func TestRunShareRelocation_ToCache_UnresolvableShare_FirstRunLeavesManifestAlone(t *testing.T) {
	fake := &fakeRelocationManifestStore{manifest: ownShapeManifest}
	err := unresolvableShareRun(t, fake, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "share not found") {
		t.Fatalf("error = %v, want the lookup failure", err)
	}
	if calls, manifest, _ := fake.snapshot(); calls != 0 || len(manifest) != 1 {
		t.Fatalf("store = calls %d, manifest %+v, want it untouched", calls, manifest)
	}
}

// TestRunShareRelocation_ToCache_UnresolvableShare_LeavesAnotherJobsManifestAlone:
// on a resumed run the shape check still protects an evacuation's manifest
// and another share's.
func TestRunShareRelocation_ToCache_UnresolvableShare_LeavesAnotherJobsManifestAlone(t *testing.T) {
	for name, c := range map[string]struct {
		manifest      []parity.ManifestEntry
		removingDisks map[string]bool
	}{
		"another share": {manifest: []parity.ManifestEntry{{RelPath: "photos/a.jpg", SourceDisk: "/mnt/disk1", TargetDisk: "/mnt/cache"}}},
		"evacuation": {
			manifest:      []parity.ManifestEntry{{RelPath: "docs/a.txt", SourceDisk: "/mnt/disk1", TargetDisk: "/mnt/disk2"}},
			removingDisks: map[string]bool{"/mnt/disk1": true},
		},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeRelocationManifestStore{manifest: c.manifest, removingDisks: c.removingDisks}
			if err := unresolvableShareRun(t, fake, []byte(`{}`), nil); err == nil {
				t.Fatal("want the lookup failure")
			}
			if calls, _, _ := fake.snapshot(); calls != 0 {
				t.Fatalf("Replace called %d time(s), want none", calls)
			}
		})
	}
}

// TestRunShareRelocation_ToCache_UnresolvableShare_StopRequestedKeepsManifest:
// a lookup that fails while maintenance mode asked the job to stop ends
// interrupted, not failed (runJob maps any error under a maintenance stop to
// interrupted), so Resume can still continue from the manifest.
func TestRunShareRelocation_ToCache_UnresolvableShare_StopRequestedKeepsManifest(t *testing.T) {
	stop := make(chan struct{})
	close(stop)
	fake := &fakeRelocationManifestStore{manifest: ownShapeManifest}
	if err := unresolvableShareRun(t, fake, []byte(`{}`), stop); err == nil {
		t.Fatal("want the lookup failure")
	}
	if calls, manifest, _ := fake.snapshot(); calls != 0 || len(manifest) != 1 {
		t.Fatalf("store = calls %d, manifest %+v, want it kept for Resume", calls, manifest)
	}
}
