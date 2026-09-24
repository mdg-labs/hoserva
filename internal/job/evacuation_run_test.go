package job

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mdg-labs/hoserva/internal/cache"
	"github.com/mdg-labs/hoserva/internal/parity"
)

// evacuationSyncFuncFromEngine adapts a parity.Engine into an
// EvacuationSyncFunc the way a real daemon's own evacuation wiring must
// (cmd/hoservad's own evacuationSyncFunc): unlike syncFuncFromEngine
// (share_relocation_run_test.go), it forwards removingDisks into
// SyncOpts.RemovingDisks rather than dropping it, since only evacuation's
// own trailing sync ever needs the guard's zero-files exemption (doc 09
// §4 step 2, Q15).
func evacuationSyncFuncFromEngine(e parity.Engine) EvacuationSyncFunc {
	return func(ctx context.Context, manifest []parity.ManifestEntry, removingDisks map[string]bool) error {
		ch, err := e.Sync(ctx, parity.SyncOpts{Manifest: manifest, RemovingDisks: removingDisks})
		return drainProgress(ch, err)
	}
}

// fakeRelocationManifestStore is a minimal, in-memory
// relocationManifestReplacer a test can inspect directly, standing in for
// *parity.RelocationManifestStore the way this package's own
// share-relocation tests already need one shape-compatible fake.
type fakeRelocationManifestStore struct {
	mu            sync.Mutex
	calls         int
	manifest      []parity.ManifestEntry
	removingDisks map[string]bool
}

func (f *fakeRelocationManifestStore) Replace(ctx context.Context, manifest []parity.ManifestEntry, removingDisks map[string]bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.manifest = manifest
	f.removingDisks = removingDisks
	return nil
}

func (f *fakeRelocationManifestStore) Current(ctx context.Context) ([]parity.ManifestEntry, map[string]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.manifest, f.removingDisks, nil
}

func (f *fakeRelocationManifestStore) snapshot() (int, []parity.ManifestEntry, map[string]bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.manifest, f.removingDisks
}

// TestRunEvacuation_MovesThroughScheduler_PostCheckPasses proves
// job.TypeEvacuation's own registration reaches cache.RunRebalance and
// cache.EvacuationPostCheck through a real Scheduler.Submit/Await round
// trip (registry wiring, doc 09 §4, #274) — not calling either directly,
// the way #56's own package-level tests do. A completed evacuation
// leaves its share branch on the evacuated disk empty.
func TestRunEvacuation_MovesThroughScheduler_PostCheckPasses(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	base := t.TempDir()
	diskMount := filepath.Join(base, "disk1")
	src := filepath.Join(diskMount, "media")
	dst := filepath.Join(base, "disk2", "media")
	plan := newRebalanceTestPlan(t, src, dst, "media", "movie.mkv", "movie bytes")

	eng := newRecordingEngine()
	eng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)

	share := cache.Share{Name: "media", Branches: []string{src}}
	s.registry.Register(TypeEvacuation, true, RunEvacuation(EvacuationDeps{
		Sync:             evacuationSyncFuncFromEngine(eng),
		TrackedFileCount: func(context.Context) (int, error) { return 1000, nil },
		Shares:           func(context.Context) ([]cache.Share, error) { return []cache.Share{share}, nil },
	}))

	j, err := s.Submit(ctx, TypeEvacuation, nil, mustJSON(t, EvacuationParams{Mountpoint: diskMount, Plan: plan}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	if _, err := os.Stat(filepath.Join(src, "movie.mkv")); !os.IsNotExist(err) {
		t.Fatalf("source should be gone after a completed evacuation: err=%v", err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("reading evacuated share branch: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("evacuated share branch still has entries = %+v, want empty (EvacuationPostCheck should have run and passed)", entries)
	}
}

// TestRunEvacuation_GuardBlocked_LeavesSourceUntouched is this issue's
// own safety-critical acceptance test: "an interrupted evacuation ...
// never deletes a source before the sync covering its copy", proven
// through the registered job — a blocked protecting sync must fail the
// job before cache.RunRebalance's own delete phase ever runs, leaving
// both the source and its already-verified target copy in place.
func TestRunEvacuation_GuardBlocked_LeavesSourceUntouched(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	base := t.TempDir()
	diskMount := filepath.Join(base, "disk1")
	src := filepath.Join(diskMount, "media")
	dst := filepath.Join(base, "disk2", "media")
	plan := newRebalanceTestPlan(t, src, dst, "media", "movie.mkv", "movie bytes")

	eng := newRecordingEngine()
	eng.ScriptGuardBlock(trippedGuard())

	share := cache.Share{Name: "media", Branches: []string{src}}
	s.registry.Register(TypeEvacuation, true, RunEvacuation(EvacuationDeps{
		Sync:             evacuationSyncFuncFromEngine(eng),
		TrackedFileCount: func(context.Context) (int, error) { return 1000, nil },
		Shares:           func(context.Context) ([]cache.Share, error) { return []cache.Share{share}, nil },
	}))

	j, err := s.Submit(ctx, TypeEvacuation, nil, mustJSON(t, EvacuationParams{Mountpoint: diskMount, Plan: plan}))
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
	if _, err := os.Stat(filepath.Join(src, "movie.mkv")); err != nil {
		t.Fatalf("source must survive a blocked sync — an evacuation must never delete a source before the sync covering its copy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "movie.mkv")); err != nil {
		t.Fatalf("verified target copy must survive a blocked sync: %v", err)
	}
}

// TestRunEvacuation_RequiresShares proves RunEvacuation fails closed —
// never silently skips the doc 09 §4 step 6 post-check — when its own
// Deps.Shares is left unwired, and does so before ever touching the
// source file.
func TestRunEvacuation_RequiresShares(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	base := t.TempDir()
	diskMount := filepath.Join(base, "disk1")
	src := filepath.Join(diskMount, "media")
	dst := filepath.Join(base, "disk2", "media")
	plan := newRebalanceTestPlan(t, src, dst, "media", "movie.mkv", "movie bytes")

	eng := newRecordingEngine()
	eng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)

	s.registry.Register(TypeEvacuation, true, RunEvacuation(EvacuationDeps{
		Sync:             evacuationSyncFuncFromEngine(eng),
		TrackedFileCount: func(context.Context) (int, error) { return 1000, nil },
	}))

	j, err := s.Submit(ctx, TypeEvacuation, nil, mustJSON(t, EvacuationParams{Mountpoint: diskMount, Plan: plan}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s, want failed (Deps.Shares is required for the post-check)", finished.Status)
	}
	if _, err := os.Stat(filepath.Join(src, "movie.mkv")); err != nil {
		t.Fatalf("source must survive when Deps.Shares is unwired: %v", err)
	}
}

// TestRunEvacuation_SyncCarriesRemovingDisks is this round's own
// regression test for the finding that blocked #274's first attempt:
// every sync RunEvacuation issues must carry SyncOpts.RemovingDisks =
// {mountpoint: true} (doc 09 §4 step 2, Q15) — without it, the guard's
// own zero-files rule blocks the evacuation's final, post-delete sync
// after its sources are already gone, and Q14's window stays open until
// a human confirms an ordinary sync by hand. recordingEngine's own
// lastSync field — not a scripted guard block, which FakeEngine does not
// derive from removingDisks at all — is what proves this: it records
// exactly what SyncOpts the job's own sync call last passed the engine.
func TestRunEvacuation_SyncCarriesRemovingDisks(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	base := t.TempDir()
	diskMount := filepath.Join(base, "disk1")
	src := filepath.Join(diskMount, "media")
	dst := filepath.Join(base, "disk2", "media")
	plan := newRebalanceTestPlan(t, src, dst, "media", "movie.mkv", "movie bytes")

	eng := newRecordingEngine()
	eng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)

	share := cache.Share{Name: "media", Branches: []string{src}}
	s.registry.Register(TypeEvacuation, true, RunEvacuation(EvacuationDeps{
		Sync:             evacuationSyncFuncFromEngine(eng),
		TrackedFileCount: func(context.Context) (int, error) { return 1000, nil },
		Shares:           func(context.Context) ([]cache.Share, error) { return []cache.Share{share}, nil },
	}))

	j, err := s.Submit(ctx, TypeEvacuation, nil, mustJSON(t, EvacuationParams{Mountpoint: diskMount, Plan: plan}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}

	lastSync, _, _, _ := eng.snapshot()
	if !lastSync.RemovingDisks[diskMount] {
		t.Fatalf("SyncOpts.RemovingDisks = %+v, want %q exempted — the evacuated disk's own zero-files trip must not block this sync", lastSync.RemovingDisks, diskMount)
	}
}

// TestRunEvacuation_PersistsAndClearsRelocationManifest proves
// RunEvacuation persists its own manifest and removing-disks set through
// Deps.Manifest as it runs — so a concurrent job.TypeSync run sees it
// through RunSync's own relocationManifestSource wiring (parity_run.go)
// — and clears it once the run finishes without being interrupted,
// mirroring RunShareRelocation's own persist-then-clear contract (#247).
func TestRunEvacuation_PersistsAndClearsRelocationManifest(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	base := t.TempDir()
	diskMount := filepath.Join(base, "disk1")
	src := filepath.Join(diskMount, "media")
	dst := filepath.Join(base, "disk2", "media")
	plan := newRebalanceTestPlan(t, src, dst, "media", "movie.mkv", "movie bytes")

	eng := newRecordingEngine()
	eng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)
	manifestStore := &fakeRelocationManifestStore{}

	share := cache.Share{Name: "media", Branches: []string{src}}
	s.registry.Register(TypeEvacuation, true, RunEvacuation(EvacuationDeps{
		Sync:             evacuationSyncFuncFromEngine(eng),
		TrackedFileCount: func(context.Context) (int, error) { return 1000, nil },
		Shares:           func(context.Context) ([]cache.Share, error) { return []cache.Share{share}, nil },
		Manifest:         manifestStore,
	}))

	j, err := s.Submit(ctx, TypeEvacuation, nil, mustJSON(t, EvacuationParams{Mountpoint: diskMount, Plan: plan}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}

	calls, manifest, removingDisks := manifestStore.snapshot()
	if calls < 2 {
		t.Fatalf("store.Replace calls = %d, want at least 2 (persist during the run, clear afterward)", calls)
	}
	if manifest != nil || removingDisks != nil {
		t.Fatalf("after a completed evacuation, store = (manifest=%+v, removingDisks=%+v), want both cleared", manifest, removingDisks)
	}
}

// TestRunEvacuation_GuardBlocked_ClearsRemovingDisks is this fix round's
// own regression test for the finding that blocked #274's second attempt,
// as narrowed by the finding that blocked the third: a job that ends
// failed can never be resumed (ErrJobNotInterrupted — "only an
// interrupted job can be resumed"), so a persisted manifest and
// removing-disks exemption that survived a guard block would permanently
// exempt this disk from the guard's own zero-files rule and could exempt
// a later, unrelated removal of the same path — silently covering a real,
// later wipe, exactly the lab scenario the finding reproduced. Both the
// manifest and the exemption are cleared together: parity.matchManifest
// (guard.go) accounts a manifest entry whenever its SourceDisk/RelPath
// pair shows up as *any* removal in a later diff, not only this run's own
// delete, so leaving a stale entry behind — as an earlier round of this
// fix did — is not inert.
func TestRunEvacuation_GuardBlocked_ClearsRemovingDisks(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	base := t.TempDir()
	diskMount := filepath.Join(base, "disk1")
	src := filepath.Join(diskMount, "media")
	dst := filepath.Join(base, "disk2", "media")
	plan := newRebalanceTestPlan(t, src, dst, "media", "movie.mkv", "movie bytes")

	eng := newRecordingEngine()
	eng.ScriptGuardBlock(trippedGuard())
	manifestStore := &fakeRelocationManifestStore{}

	share := cache.Share{Name: "media", Branches: []string{src}}
	s.registry.Register(TypeEvacuation, true, RunEvacuation(EvacuationDeps{
		Sync:             evacuationSyncFuncFromEngine(eng),
		TrackedFileCount: func(context.Context) (int, error) { return 1000, nil },
		Shares:           func(context.Context) ([]cache.Share, error) { return []cache.Share{share}, nil },
		Manifest:         manifestStore,
	}))

	j, err := s.Submit(ctx, TypeEvacuation, nil, mustJSON(t, EvacuationParams{Mountpoint: diskMount, Plan: plan}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s, want failed", finished.Status)
	}

	calls, manifest, removingDisks := manifestStore.snapshot()
	if calls < 2 {
		t.Fatalf("store.Replace calls = %d, want at least 2 (persist during the run, clear afterward)", calls)
	}
	if manifest != nil || removingDisks != nil {
		t.Fatalf("after a failed (non-resumable) run, store = (manifest=%+v, removingDisks=%+v), want both cleared — a stale manifest entry could exempt a later, unrelated removal of the same path (matchManifest, guard.go), and a stale exemption would permanently hide this disk's zero-files rule", manifest, removingDisks)
	}
}

// TestRunEvacuation_ContextCancelled_ClearsRemovingDisks proves the other
// non-resumable ending this fix round's finding calls out: a live
// Scheduler.Cancel of a running evacuation hard-cancels its context
// (Scheduler.Cancel's own doc comment) rather than closing
// rc.StopRequested() the way a graceful maintenance/battery stop does, so
// RunEvacuation must tell the two apart and clear the whole persisted
// relocation state for this one too — a cancelled job is exactly as
// unresumable as a failed one. The context is cancelled the instant the batch's
// manifest becomes durable, before its own guarded sync ever runs — the
// same window TestRunShareRelocation_ToCache_ManifestSurvivesInterruption_ThenClearsOnResume
// exercises for share relocation's graceful path — so this also proves
// the source survives: an evacuation must never delete a source before
// the sync covering its copy, even along the cancelled path.
func TestRunEvacuation_ContextCancelled_ClearsRemovingDisks(t *testing.T) {
	base := t.TempDir()
	diskMount := filepath.Join(base, "disk1")
	src := filepath.Join(diskMount, "media")
	dst := filepath.Join(base, "disk2", "media")
	plan := newRebalanceTestPlan(t, src, dst, "media", "movie.mkv", "movie bytes")

	eng := newRecordingEngine()
	eng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)
	manifestStore := &fakeRelocationManifestStore{}

	share := cache.Share{Name: "media", Branches: []string{src}}
	fn := RunEvacuation(EvacuationDeps{
		Sync:             evacuationSyncFuncFromEngine(eng),
		TrackedFileCount: func(context.Context) (int, error) { return 1000, nil },
		Shares:           func(context.Context) ([]cache.Share, error) { return []cache.Share{share}, nil },
		Manifest:         manifestStore,
	})

	runCtx, cancel := context.WithCancel(context.Background())
	saveCheckpoint := func(data []byte) error {
		var cp cache.RebalanceCheckpoint
		if err := json.Unmarshal(data, &cp); err == nil && len(cp.Manifest) > 0 {
			// Simulate Scheduler.Cancel landing the instant the manifest
			// is durable — the same race window a live Cancel can always
			// win against a checkpoint boundary.
			cancel()
		}
		return nil
	}
	rc := &RunContext{
		ctx:            runCtx,
		params:         mustJSON(t, EvacuationParams{Mountpoint: diskMount, Plan: plan}),
		stopRequested:  make(chan struct{}), // never closed: this is a hard Cancel, not a graceful stop
		out:            &bytes.Buffer{},
		saveCheckpoint: saveCheckpoint,
		setProgress:    func(int) {},
	}
	if err := fn(runCtx, rc); err != nil {
		t.Fatalf("cancelled run: %v", err)
	}

	if _, err := os.Stat(src); err != nil {
		t.Fatalf("source must survive a cancel that lands before the sync covering its copy: %v", err)
	}

	calls, manifest, removingDisks := manifestStore.snapshot()
	if calls < 2 {
		t.Fatalf("store.Replace calls = %d, want at least 2 (persist during the run, clear afterward)", calls)
	}
	if manifest != nil || removingDisks != nil {
		t.Fatalf("after a cancelled (non-resumable) run, store = (manifest=%+v, removingDisks=%+v), want both cleared", manifest, removingDisks)
	}
}

// TestRunEvacuation_FailedRun_LaterSyncSeesNoManifest is this fix round's
// own end-to-end proof of the effect
// TestRunEvacuation_GuardBlocked_ClearsRemovingDisks only checks at the
// store level: once a guard-blocked (non-resumable) evacuation has
// cleared, a later, independently scheduled job.TypeSync run — driven
// through the real Scheduler and RunSync's own relocationManifestSource
// wiring (parity_run.go), the same production path
// TestRunShareRelocation_ToCache_SyncDuringOutstandingRelocation_SeesManifest
// exercises for the opposite (still-outstanding) case — loads no manifest
// entries and no removing-disks exemption for this run at all, against a
// real *parity.RelocationManifestStore rather than the in-memory fake.
func TestRunEvacuation_FailedRun_LaterSyncSeesNoManifest(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	store := parity.NewRelocationManifestStore(db)
	base := t.TempDir()
	diskMount := filepath.Join(base, "disk1")
	src := filepath.Join(diskMount, "media")
	dst := filepath.Join(base, "disk2", "media")
	plan := newRebalanceTestPlan(t, src, dst, "media", "movie.mkv", "movie bytes")

	evacEng := newRecordingEngine()
	evacEng.ScriptGuardBlock(trippedGuard())

	share := cache.Share{Name: "media", Branches: []string{src}}
	fn := RunEvacuation(EvacuationDeps{
		Sync:             evacuationSyncFuncFromEngine(evacEng),
		TrackedFileCount: func(context.Context) (int, error) { return 1000, nil },
		Shares:           func(context.Context) ([]cache.Share, error) { return []cache.Share{share}, nil },
		Manifest:         store,
	})
	rc := &RunContext{
		ctx:            ctx,
		params:         mustJSON(t, EvacuationParams{Mountpoint: diskMount, Plan: plan}),
		stopRequested:  make(chan struct{}),
		out:            &bytes.Buffer{},
		saveCheckpoint: func([]byte) error { return nil },
		setProgress:    func(int) {},
	}
	if err := fn(ctx, rc); err == nil {
		t.Fatal("guard-blocked evacuation: want an error, got nil")
	}

	jobStore := NewStore(db)
	s := NewScheduler(jobStore, NewLogStore(t.TempDir()), NewHub(), NewRegistry())
	syncEng := &storeBackedEngine{recordingEngine: newRecordingEngine(), store: store}
	syncEng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)
	s.registry.Register(TypeSync, false, RunSync(syncEng))

	syncJob, err := s.Submit(ctx, TypeSync, nil, mustJSON(t, SyncParams{}))
	if err != nil {
		t.Fatalf("Submit(TypeSync): %v", err)
	}
	finished := await(t, s, syncJob.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("sync status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}

	lastSync, _, _, _ := syncEng.snapshot()
	if len(lastSync.Manifest) != 0 {
		t.Fatalf("sync's own SyncOpts.Manifest after the failed evacuation cleared = %+v, want none", lastSync.Manifest)
	}
	if len(lastSync.RemovingDisks) != 0 {
		t.Fatalf("sync's own SyncOpts.RemovingDisks after the failed evacuation cleared = %+v, want none", lastSync.RemovingDisks)
	}
}

// contextCheckingManifestStore wraps fakeRelocationManifestStore's own
// Replace to fail whenever called with an already-cancelled context, the
// way *parity.RelocationManifestStore's real SQL calls would — proving
// TestRunEvacuation_CleanFinish_ClearSurvivesLateCancel actually exercises
// the fix rather than passing by accident.
type contextCheckingManifestStore struct {
	*fakeRelocationManifestStore
}

func (c contextCheckingManifestStore) Replace(ctx context.Context, manifest []parity.ManifestEntry, removingDisks map[string]bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.fakeRelocationManifestStore.Replace(ctx, manifest, removingDisks)
}

// TestRunEvacuation_CleanFinish_ClearSurvivesLateCancel is this fix
// round's own regression test for finding 3: the clean-finish clear
// (RunEvacuation's own "err == nil && !report.Interrupted" case) must use
// a context Scheduler.Cancel racing in between cache.RunRebalance's own
// last ctx check and this call cannot poison — a run is never
// interrupted at this point (it already finished, un-resumably, as far
// as Resume is concerned), so a job that is not StatusInterrupted never
// re-enters RunEvacuation or reaches job.EvacuationAbort, and a clear
// that failed with context.Canceled here would leave the removing-disks
// exemption stranded forever. Uses a zero-move plan so
// cache.RunRebalance's own loop — which never runs when there is nothing
// to move — never itself observes ctx, letting this test cancel it
// up front and still reach the clean-finish branch deterministically,
// rather than relying on real goroutine timing to win a nanosecond race.
func TestRunEvacuation_CleanFinish_ClearSurvivesLateCancel(t *testing.T) {
	base := t.TempDir()
	diskMount := filepath.Join(base, "disk1")

	manifestStore := contextCheckingManifestStore{&fakeRelocationManifestStore{
		manifest:      []parity.ManifestEntry{{SourceDisk: diskMount, TargetDisk: filepath.Join(base, "disk2"), RelPath: "already-synced.bin"}},
		removingDisks: map[string]bool{diskMount: true},
	}}

	share := cache.Share{Name: "media", Branches: []string{filepath.Join(diskMount, "media")}}
	fn := RunEvacuation(EvacuationDeps{
		Sync: func(context.Context, []parity.ManifestEntry, map[string]bool) error {
			t.Fatal("Sync must not be called for a zero-move plan")
			return nil
		},
		TrackedFileCount: func(context.Context) (int, error) { return 1000, nil },
		Shares:           func(context.Context) ([]cache.Share, error) { return []cache.Share{share}, nil },
		Manifest:         manifestStore,
	})

	runCtx, cancel := context.WithCancel(context.Background())
	cancel() // simulate Cancel already racing in before RunEvacuation's own clean-finish clear runs
	rc := &RunContext{
		ctx:            runCtx,
		params:         mustJSON(t, EvacuationParams{Mountpoint: diskMount, Plan: cache.RebalancePlan{}}),
		stopRequested:  make(chan struct{}),
		out:            &bytes.Buffer{},
		saveCheckpoint: func([]byte) error { return nil },
		setProgress:    func(int) {},
	}
	if err := fn(runCtx, rc); err != nil {
		t.Fatalf("clean-finish run with a pre-cancelled ctx: %v", err)
	}

	_, manifest, removingDisks := manifestStore.snapshot()
	if manifest != nil || removingDisks != nil {
		t.Fatalf("after a clean finish, store = (manifest=%+v, removingDisks=%+v), want both cleared even though ctx was already cancelled", manifest, removingDisks)
	}
}

// TestRunEvacuation_MaintenanceInterrupt_KeepsRemovingDisks_ThenCancelClears
// proves the resumable ending this fix round's finding says must behave
// differently: a graceful maintenance-mode stop closes rc.StopRequested()
// without cancelling the context (EnterMaintenance's own doc comment), so
// the job ends interrupted and Resume can continue it — the
// removing-disks exemption must survive exactly that ending, unlike a
// failure or a Cancel. It then proves the other half of the same
// invariant: Scheduler.Cancel of that now-interrupted job never re-enters
// RunEvacuation at all (abortAndCancel goes straight from interrupted to
// cancelled), so job.EvacuationAbort — registered the same way
// hoservad registers it — must be the one to clear the exemption once the
// job is cancelled instead of resumed.
func TestRunEvacuation_MaintenanceInterrupt_KeepsRemovingDisks_ThenCancelClears(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	base := t.TempDir()
	diskMount := filepath.Join(base, "disk1")
	src := filepath.Join(diskMount, "media")
	dst := filepath.Join(base, "disk2", "media")
	plan := newRebalanceTestPlan(t, src, dst, "media", "movie.mkv", "movie bytes")

	eng := newRecordingEngine()
	eng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)
	manifestStore := &fakeRelocationManifestStore{}

	share := cache.Share{Name: "media", Branches: []string{src}}
	sync := func(ctx context.Context, manifest []parity.ManifestEntry, removingDisks map[string]bool) error {
		// Maintenance mode lands mid-sync, exactly as `hoserva array
		// stop` would while this job's own pre-delete sync is in flight.
		if err := s.EnterMaintenance(ctx); err != nil {
			t.Fatalf("EnterMaintenance: %v", err)
		}
		return evacuationSyncFuncFromEngine(eng)(ctx, manifest, removingDisks)
	}
	s.registry.Register(TypeEvacuation, true, RunEvacuation(EvacuationDeps{
		Sync:             sync,
		TrackedFileCount: func(context.Context) (int, error) { return 1000, nil },
		Shares:           func(context.Context) ([]cache.Share, error) { return []cache.Share{share}, nil },
		Manifest:         manifestStore,
	}))
	s.registry.RegisterAbort(TypeEvacuation, EvacuationAbort(manifestStore))

	j, err := s.Submit(ctx, TypeEvacuation, nil, mustJSON(t, EvacuationParams{Mountpoint: diskMount, Plan: plan}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusInterrupted {
		t.Fatalf("status = %s (%s), want interrupted", finished.Status, finished.ErrorMessage)
	}

	calls, manifest, removingDisks := manifestStore.snapshot()
	if calls == 0 {
		t.Fatal("store.Replace was never called — the batch's own manifest should have been persisted before the sync")
	}
	if len(manifest) == 0 || !removingDisks[diskMount] {
		t.Fatalf("after a graceful maintenance interrupt, store = (manifest=%+v, removingDisks=%+v), want both still persisted so Resume — and any concurrent sync — keeps exempting this disk", manifest, removingDisks)
	}

	if _, err := s.Cancel(ctx, j.ID); err != nil {
		t.Fatalf("Cancel(interrupted evacuation): %v", err)
	}
	cancelled := await(t, s, j.ID)
	if cancelled.Status != StatusCancelled {
		t.Fatalf("status after Cancel = %s (%s), want cancelled", cancelled.Status, cancelled.ErrorMessage)
	}

	_, _, removingDisksAfterCancel := manifestStore.snapshot()
	if removingDisksAfterCancel != nil {
		t.Fatalf("after cancelling the interrupted evacuation, removingDisks = %+v, want cleared by EvacuationAbort — Resume is no longer possible once cancelled", removingDisksAfterCancel)
	}
}

// TestEvacuationAbort_LeavesUnrelatedRemovingDisksAlone proves
// EvacuationAbort never clears blind: the store holds one shared slot, so
// if it named a different disk by the time Cancel runs — a different
// array-write job having since claimed it for an unrelated relocation
// while this one sat interrupted, doc 01 §4's mutual exclusion only ever
// excludes a *running* job of the same class — the abort must leave it
// untouched rather than clearing someone else's in-flight state.
func TestEvacuationAbort_LeavesUnrelatedRemovingDisksAlone(t *testing.T) {
	ctx := context.Background()
	store := &fakeRelocationManifestStore{
		manifest:      []parity.ManifestEntry{{RelPath: "docs/report.pdf", SourceDisk: "/mnt/disk9", TargetDisk: "/mnt/disk8"}},
		removingDisks: map[string]bool{"/mnt/disk9": true},
	}
	params := mustJSON(t, EvacuationParams{Mountpoint: "/mnt/disk1", Plan: cache.RebalancePlan{}})

	if err := EvacuationAbort(store)(ctx, params); err != nil {
		t.Fatalf("EvacuationAbort: %v", err)
	}

	_, _, removingDisks := store.snapshot()
	if !removingDisks["/mnt/disk9"] {
		t.Fatalf("removingDisks = %+v, want the unrelated disk's own exemption left alone", removingDisks)
	}
}

// TestRunEvacuation_BoundsManifestReplaceCalls proves finding 2's own
// closed measurement: a checkpoint per deleted file must not cost a
// store.Replace per deleted file. With RebalanceBatchLimit effectively 2
// (via a small TrackedFileCount, computed the same way rebalanceBatchSize
// always has) and 5 files, the plan runs as 3 batches; a bounded
// implementation calls store.Replace at most once per batch, plus one to
// clear at the end — 4 total, not one per file deleted (15, measured
// against cc2543a's own per-checkpoint persistence).
func TestRunEvacuation_BoundsManifestReplaceCalls(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	base := t.TempDir()
	diskMount := filepath.Join(base, "disk1")
	src := filepath.Join(diskMount, "media")
	dst := filepath.Join(base, "disk2", "media")
	const fileCount = 5
	plan := newEvacuationTestPlanMultiFile(t, src, dst, "media", fileCount)

	eng := newRecordingEngine()
	eng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)
	manifestStore := &fakeRelocationManifestStore{}

	share := cache.Share{Name: "media", Branches: []string{src}}
	s.registry.Register(TypeEvacuation, true, RunEvacuation(EvacuationDeps{
		Sync: evacuationSyncFuncFromEngine(eng),
		// 20 tracked files makes rebalanceBatchSize compute a 2-file
		// batch limit (10% of 20/(100-10) ≈ 2.2, truncated) — small
		// enough that fileCount needs more than one batch, without this
		// test depending on the 200-file default limit.
		TrackedFileCount: func(context.Context) (int, error) { return 20, nil },
		Shares:           func(context.Context) ([]cache.Share, error) { return []cache.Share{share}, nil },
		Manifest:         manifestStore,
	}))

	j, err := s.Submit(ctx, TypeEvacuation, nil, mustJSON(t, EvacuationParams{Mountpoint: diskMount, Plan: plan}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}

	const wantBatches = 3 // ceil(5/2)
	const wantCalls = wantBatches + 1
	calls, manifest, removingDisks := manifestStore.snapshot()
	if calls > wantCalls {
		t.Fatalf("store.Replace calls = %d, want at most %d (one per batch, plus the final clear) — not one per deleted file", calls, wantCalls)
	}
	if manifest != nil || removingDisks != nil {
		t.Fatalf("after a completed evacuation, store = (manifest=%+v, removingDisks=%+v), want both cleared", manifest, removingDisks)
	}
}

// newEvacuationTestPlanMultiFile writes n small files at srcDir and
// returns the cache.RebalancePlan a RunEvacuation job would be submitted
// with for moving all of them to dstDir — newRebalanceTestPlan's own
// single-move shape, extended to force RunRebalance into more than one
// batch.
func newEvacuationTestPlanMultiFile(t *testing.T, srcDir, dstDir, share string, n int) cache.RebalancePlan {
	t.Helper()
	moves := make([]cache.RebalanceMove, 0, n)
	for i := 0; i < n; i++ {
		rel := fmt.Sprintf("file%d.bin", i)
		content := fmt.Sprintf("bytes-%d", i)
		mustWriteFile(t, filepath.Join(srcDir, rel), content)
		moves = append(moves, cache.RebalanceMove{
			Share:        share,
			RelPath:      rel,
			SourceBranch: srcDir,
			TargetBranch: dstDir,
			Size:         int64(len(content)),
		})
	}
	return cache.RebalancePlan{Moves: moves}
}

func TestValidateParams_EvacuationRequiresMountpointAndPlan(t *testing.T) {
	for _, params := range [][]byte{
		nil,
		[]byte("null"),
		[]byte(""),
		mustJSON(t, EvacuationParams{Plan: cache.RebalancePlan{}}),
	} {
		if err := ValidateParams(TypeEvacuation, params); err == nil {
			t.Fatalf("ValidateParams(evacuation, %q) = nil, want rejection", params)
		}
	}
	// A mountpoint with an empty plan is legitimate — the disk already
	// holds nothing to evacuate — and must not be rejected.
	if err := ValidateParams(TypeEvacuation, mustJSON(t, EvacuationParams{Mountpoint: "/mnt/disk1"})); err != nil {
		t.Fatalf("ValidateParams(evacuation, mountpoint only) = %v, want nil", err)
	}
}
