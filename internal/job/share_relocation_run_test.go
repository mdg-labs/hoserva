package job

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	src := filepath.Join(share.CachePath, "report.pdf")
	mustWriteFile(t, src, "report bytes")

	s.registry.Register(TypeShareRelocation, true, RunShareRelocation(ShareRelocationDeps{
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

// TestRunShareRelocation_ToCache_FinalSyncFailure_LeavesManifestPersisted
// is #247's own clear-path safety test (its own issue text: "Get the
// clear-path wrong and this issue itself becomes a guard-masking bug"):
// when the relocation's own *trailing* sync fails — after the delete phase
// already removed the array originals — the manifest must NOT be cleared,
// since a concurrent or later sync still needs it to correctly exempt
// those already-accounted removals until the relocation itself resolves.
func TestRunShareRelocation_ToCache_FinalSyncFailure_LeavesManifestPersisted(t *testing.T) {
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
		Share:    func(context.Context, string) (cache.Share, error) { return share, nil },
		Sync:     sync,
		Manifest: store,
	}))

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

	manifest, _, err := store.Current(ctx)
	if err != nil {
		t.Fatalf("store.Current after a failed trailing sync: %v", err)
	}
	if len(manifest) != 1 || manifest[0].RelPath != "docs/report.pdf" {
		t.Fatalf("manifest after a failed trailing sync = %+v, want it still persisted, not cleared", manifest)
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
