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
	"sync/atomic"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/cache"
	"github.com/mdg-labs/hoserva/internal/parity"
	hoservastore "github.com/mdg-labs/hoserva/internal/store"
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

// fakeRemovalStateStore is a minimal, in-memory removalStateStore a test
// can inspect directly, standing in for *store.ArrayStore the way
// fakeRelocationManifestStore stands in for
// *parity.RelocationManifestStore. It reproduces store.ArrayStore's own
// rules: SetRemovalState refuses while a different mountpoint holds a
// state and records the job holding it, and ReleaseRemovalState clears
// only an "evacuating" state held by the named job.
type fakeRemovalStateStore struct {
	mu     sync.Mutex
	state  map[string]string
	holder map[string]string
}

func newFakeRemovalStateStore() *fakeRemovalStateStore {
	return &fakeRemovalStateStore{state: map[string]string{}, holder: map[string]string{}}
}

func (f *fakeRemovalStateStore) SetRemovalState(ctx context.Context, mountpoint, state, jobID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if jobID == "" {
		return fmt.Errorf("fake: no job id")
	}
	for mp, st := range f.state {
		if mp != mountpoint && st != "" {
			return fmt.Errorf("fake: %s is already removing", mp)
		}
	}
	f.state[mountpoint] = state
	f.holder[mountpoint] = jobID
	return nil
}

func (f *fakeRemovalStateStore) ReleaseRemovalState(ctx context.Context, mountpoint, jobID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state[mountpoint] != hoservastore.RemovalStateEvacuating || f.holder[mountpoint] != jobID {
		return false, nil
	}
	delete(f.state, mountpoint)
	delete(f.holder, mountpoint)
	return true, nil
}

func (f *fakeRemovalStateStore) get(mountpoint string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state[mountpoint]
}

// fakeArrayReadyHook is a scriptable ArrayReady dependency (job.
// EvacuationDeps.ArrayReady): CLAUDE.md's "every system-touching
// subsystem sits behind a package interface with a scriptable fake"
// applied to the live-mount reapplication hook itself, so a test can
// prove RunEvacuation fails before any copy when it fails, and count how
// many times it actually ran.
type fakeArrayReadyHook struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (f *fakeArrayReadyHook) hook(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.err
}

func (f *fakeArrayReadyHook) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
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
		Store:            newFakeRemovalStateStore(),
		ArrayReady:       func(context.Context) error { return nil },
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
		Store:            newFakeRemovalStateStore(),
		ArrayReady:       func(context.Context) error { return nil },
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

// TestRunEvacuation_RequiresStoreAndArrayReady proves RunEvacuation fails
// closed — never silently skips doc 09 §4 step 2's own no-create switch —
// when either Deps.Store or Deps.ArrayReady is left unwired, the same
// fail-closed shape TestRunEvacuation_RequiresShares proves for Deps.Shares.
func TestRunEvacuation_RequiresStoreAndArrayReady(t *testing.T) {
	base := t.TempDir()
	diskMount := filepath.Join(base, "disk1")
	src := filepath.Join(diskMount, "media")
	dst := filepath.Join(base, "disk2", "media")
	plan := newRebalanceTestPlan(t, src, dst, "media", "movie.mkv", "movie bytes")
	share := cache.Share{Name: "media", Branches: []string{src}}
	eng := newRecordingEngine()
	eng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)

	base1 := func() EvacuationDeps {
		return EvacuationDeps{
			Sync:             evacuationSyncFuncFromEngine(eng),
			TrackedFileCount: func(context.Context) (int, error) { return 1000, nil },
			Shares:           func(context.Context) ([]cache.Share, error) { return []cache.Share{share}, nil },
		}
	}
	rc := func() *RunContext {
		return &RunContext{
			id:             "job-1",
			ctx:            context.Background(),
			params:         mustJSON(t, EvacuationParams{Mountpoint: diskMount, Plan: plan}),
			stopRequested:  make(chan struct{}),
			out:            &bytes.Buffer{},
			saveCheckpoint: func([]byte) error { return nil },
			setProgress:    func(int) {},
		}
	}

	noStore := base1()
	noStore.ArrayReady = func(context.Context) error { return nil }
	if err := RunEvacuation(noStore)(context.Background(), rc()); err == nil {
		t.Fatal("RunEvacuation with no Deps.Store: want an error, got nil")
	}

	noArrayReady := base1()
	noArrayReady.Store = newFakeRemovalStateStore()
	if err := RunEvacuation(noArrayReady)(context.Background(), rc()); err == nil {
		t.Fatal("RunEvacuation with no Deps.ArrayReady: want an error, got nil")
	}
}

// TestRunEvacuation_MarksRemovingAndAppliesLive_BeforeAnyCopy proves doc
// 09 §4 step 2's own ordering: Deps.Store.SetRemovalState and
// Deps.ArrayReady both run — and the disk is already marked "evacuating"
// — before Deps.Sync is ever called, i.e. before this run's first copy.
// order records which of the three happens first; without the fix, Sync
// could run before the disk was ever marked no-create.
func TestRunEvacuation_MarksRemovingAndAppliesLive_BeforeAnyCopy(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	base := t.TempDir()
	diskMount := filepath.Join(base, "disk1")
	src := filepath.Join(diskMount, "media")
	dst := filepath.Join(base, "disk2", "media")
	plan := newRebalanceTestPlan(t, src, dst, "media", "movie.mkv", "movie bytes")

	eng := newRecordingEngine()
	eng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)
	removalStore := newFakeRemovalStateStore()

	var mu sync.Mutex
	var order []string
	arrayReady := func(context.Context) error {
		mu.Lock()
		order = append(order, "array-ready:"+removalStore.get(diskMount))
		mu.Unlock()
		return nil
	}
	sync := func(ctx context.Context, manifest []parity.ManifestEntry, removingDisks map[string]bool) error {
		mu.Lock()
		order = append(order, "sync:"+removalStore.get(diskMount))
		mu.Unlock()
		return evacuationSyncFuncFromEngine(eng)(ctx, manifest, removingDisks)
	}

	share := cache.Share{Name: "media", Branches: []string{src}}
	s.registry.Register(TypeEvacuation, true, RunEvacuation(EvacuationDeps{
		Sync:             sync,
		TrackedFileCount: func(context.Context) (int, error) { return 1000, nil },
		Shares:           func(context.Context) ([]cache.Share, error) { return []cache.Share{share}, nil },
		Store:            removalStore,
		ArrayReady:       arrayReady,
	}))

	j, err := s.Submit(ctx, TypeEvacuation, nil, mustJSON(t, EvacuationParams{Mountpoint: diskMount, Plan: plan}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}

	mu.Lock()
	got := append([]string(nil), order...)
	mu.Unlock()
	if len(got) < 2 || got[0] != "array-ready:evacuating" {
		t.Fatalf("call order = %v, want ArrayReady to run first, with the disk already marked evacuating", got)
	}
	if got[1] != "sync:evacuating" {
		t.Fatalf("call order = %v, want Sync's first call to see the disk already marked evacuating", got)
	}
}

// TestRunEvacuation_ArrayReadyFailure_FailsBeforeAnyCopy is this issue's
// own acceptance test for "if the live NC update fails, the evacuation
// job fails before copying anything": a fake ArrayReady standing in for a
// failed live mounter (CLAUDE.md's own scriptable-fake rule) must fail
// the job before Sync — and so before any copy — ever runs, and the
// source file must survive untouched. Without the fix, a failed live
// remount would only be logged (main.go's own topologyChanged, before
// this issue) and the evacuation would proceed to copy files off a disk
// that is not actually no-create yet.
func TestRunEvacuation_ArrayReadyFailure_FailsBeforeAnyCopy(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	base := t.TempDir()
	diskMount := filepath.Join(base, "disk1")
	src := filepath.Join(diskMount, "media")
	dst := filepath.Join(base, "disk2", "media")
	plan := newRebalanceTestPlan(t, src, dst, "media", "movie.mkv", "movie bytes")

	eng := newRecordingEngine()
	eng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)
	removalStore := newFakeRemovalStateStore()

	var syncCalls int32
	sync := func(ctx context.Context, manifest []parity.ManifestEntry, removingDisks map[string]bool) error {
		atomic.AddInt32(&syncCalls, 1)
		return evacuationSyncFuncFromEngine(eng)(ctx, manifest, removingDisks)
	}
	failingArrayReady := &fakeArrayReadyHook{err: fmt.Errorf("simulated failure applying no-create to the live mount")}

	share := cache.Share{Name: "media", Branches: []string{src}}
	s.registry.Register(TypeEvacuation, true, RunEvacuation(EvacuationDeps{
		Sync:             sync,
		TrackedFileCount: func(context.Context) (int, error) { return 1000, nil },
		Shares:           func(context.Context) ([]cache.Share, error) { return []cache.Share{share}, nil },
		Store:            removalStore,
		ArrayReady:       failingArrayReady.hook,
	}))

	j, err := s.Submit(ctx, TypeEvacuation, nil, mustJSON(t, EvacuationParams{Mountpoint: diskMount, Plan: plan}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s (%s), want failed", finished.Status, finished.ErrorMessage)
	}
	if atomic.LoadInt32(&syncCalls) != 0 {
		t.Fatalf("Sync was called %d time(s), want 0 — a failed live no-create application must fail the job before any copy", syncCalls)
	}
	if _, err := os.Stat(filepath.Join(src, "movie.mkv")); err != nil {
		t.Fatalf("source must survive when the live no-create application fails: %v", err)
	}
	if got := removalStore.get(diskMount); got != "evacuating" {
		t.Fatalf("removal_state after a failed live application = %q, want %q — the store write itself already succeeded", got, "evacuating")
	}
}

// TestRunEvacuation_Success_MarksEvacuated proves doc 09 §4 step 2's own
// closing transition: once the copy and its post-check both pass, the
// disk moves from "evacuating" to "evacuated" — still no-create (steps
// 7-9 have not run), but no longer mid-copy.
func TestRunEvacuation_Success_MarksEvacuated(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	base := t.TempDir()
	diskMount := filepath.Join(base, "disk1")
	src := filepath.Join(diskMount, "media")
	dst := filepath.Join(base, "disk2", "media")
	plan := newRebalanceTestPlan(t, src, dst, "media", "movie.mkv", "movie bytes")

	eng := newRecordingEngine()
	eng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)
	removalStore := newFakeRemovalStateStore()

	share := cache.Share{Name: "media", Branches: []string{src}}
	s.registry.Register(TypeEvacuation, true, RunEvacuation(EvacuationDeps{
		Sync:             evacuationSyncFuncFromEngine(eng),
		TrackedFileCount: func(context.Context) (int, error) { return 1000, nil },
		Shares:           func(context.Context) ([]cache.Share, error) { return []cache.Share{share}, nil },
		Store:            removalStore,
		ArrayReady:       func(context.Context) error { return nil },
	}))

	j, err := s.Submit(ctx, TypeEvacuation, nil, mustJSON(t, EvacuationParams{Mountpoint: diskMount, Plan: plan}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	if got := removalStore.get(diskMount); got != "evacuated" {
		t.Fatalf("removal_state after a successful evacuation = %q, want %q", got, "evacuated")
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
		Store:            newFakeRemovalStateStore(),
		ArrayReady:       func(context.Context) error { return nil },
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
		Store:            newFakeRemovalStateStore(),
		ArrayReady:       func(context.Context) error { return nil },
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
	removalStore := newFakeRemovalStateStore()
	arrayReady := &fakeArrayReadyHook{}

	share := cache.Share{Name: "media", Branches: []string{src}}
	s.registry.Register(TypeEvacuation, true, RunEvacuation(EvacuationDeps{
		Sync:             evacuationSyncFuncFromEngine(eng),
		TrackedFileCount: func(context.Context) (int, error) { return 1000, nil },
		Shares:           func(context.Context) ([]cache.Share, error) { return []cache.Share{share}, nil },
		Manifest:         manifestStore,
		Store:            removalStore,
		ArrayReady:       arrayReady.hook,
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
	// Unlike the guard's own manifest/exemption table (cleared above), a
	// genuine failure — as opposed to an explicit Cancel — must leave the
	// disk's own removal_state exactly as it was: "evacuating", so a later
	// resume or retry still finds it no-create and cannot land a new write
	// on it in the meantime (doc 09 §4 step 2).
	if got := removalStore.get(diskMount); got != "evacuating" {
		t.Fatalf("removal_state after a failed (non-cancelled) run = %q, want still %q", got, "evacuating")
	}
	if arrayReady.callCount() != 1 {
		t.Fatalf("ArrayReady was called %d time(s), want exactly 1 (only the pre-copy application — a plain failure never re-applies RW branches)", arrayReady.callCount())
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
	removalStore := newFakeRemovalStateStore()
	arrayReady := &fakeArrayReadyHook{}

	share := cache.Share{Name: "media", Branches: []string{src}}
	fn := RunEvacuation(EvacuationDeps{
		Sync:             evacuationSyncFuncFromEngine(eng),
		TrackedFileCount: func(context.Context) (int, error) { return 1000, nil },
		Shares:           func(context.Context) ([]cache.Share, error) { return []cache.Share{share}, nil },
		Manifest:         manifestStore,
		Store:            removalStore,
		ArrayReady:       arrayReady.hook,
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
		id:             "job-1",
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
	if got := removalStore.get(diskMount); got != "" {
		t.Fatalf("removal_state after a cancelled run = %q, want cleared so the disk takes writes again (doc 09 §4 step 2)", got)
	}
	if arrayReady.callCount() < 2 {
		t.Fatalf("ArrayReady was called %d time(s), want at least 2 (before the first copy, and again to re-apply RW branches once cancelled)", arrayReady.callCount())
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
		Store:            newFakeRemovalStateStore(),
		ArrayReady:       func(context.Context) error { return nil },
	})
	rc := &RunContext{
		id:             "job-1",
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
		Store:            newFakeRemovalStateStore(),
		ArrayReady:       func(context.Context) error { return nil },
	})

	runCtx, cancel := context.WithCancel(context.Background())
	cancel() // simulate Cancel already racing in before RunEvacuation's own clean-finish clear runs
	rc := &RunContext{
		id:             "job-1",
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
	removalStore := newFakeRemovalStateStore()
	arrayReady := &fakeArrayReadyHook{}

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
		Store:            removalStore,
		ArrayReady:       arrayReady.hook,
	}))
	s.registry.RegisterAbort(TypeEvacuation, EvacuationAbort(manifestStore, removalStore, arrayReady.hook))

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
	if got := removalStore.get(diskMount); got != "evacuating" {
		t.Fatalf("removal_state after a graceful maintenance interrupt = %q, want %q — Resume must still find the disk no-create", got, "evacuating")
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
	if got := removalStore.get(diskMount); got != "" {
		t.Fatalf("removal_state after cancelling the interrupted evacuation = %q, want cleared so the disk takes writes again", got)
	}
	if arrayReady.callCount() < 2 {
		t.Fatalf("ArrayReady was called %d time(s), want at least 2 (before the first copy, and again to re-apply RW branches once EvacuationAbort clears the state)", arrayReady.callCount())
	}
}

// TestRunEvacuation_InterruptedThenResumed_ReappliesRemovingState is
// #359's own "restart mid-evacuation" acceptance test: an interrupted run
// (the same graceful-stop shape TestRunShareRelocation_ToCache_
// ManifestSurvivesInterruption_ThenClearsOnResume exercises for share
// relocation) leaves the disk's own removal_state persisted — exactly
// what a restarted daemon's own newArraySequence would read fresh, since
// removalStore here stands in for the same *store.ArrayStore a real
// restart reopens unchanged — and Resume (fn called again with the first
// run's own checkpoint, a fresh RunContext standing in for the process
// restart) re-applies it through Deps.Store/ArrayReady again before
// continuing, ending the run "evacuated".
func TestRunEvacuation_InterruptedThenResumed_ReappliesRemovingState(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	diskMount := filepath.Join(base, "disk1")
	src := filepath.Join(diskMount, "media")
	dst := filepath.Join(base, "disk2", "media")
	plan := newRebalanceTestPlan(t, src, dst, "media", "movie.mkv", "movie bytes")

	eng := newRecordingEngine()
	eng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)
	manifestStore := &fakeRelocationManifestStore{}
	// removalStore is never rebuilt between the two runs below — the same
	// persistence a real *store.ArrayStore backed by SQLite gives a
	// restarted daemon (D4): its state is what a restart reads back, not
	// something the process itself has to carry across the restart.
	removalStore := newFakeRemovalStateStore()
	arrayReady := &fakeArrayReadyHook{}
	share := cache.Share{Name: "media", Branches: []string{src}}

	fn := RunEvacuation(EvacuationDeps{
		Sync:             evacuationSyncFuncFromEngine(eng),
		TrackedFileCount: func(context.Context) (int, error) { return 1000, nil },
		Shares:           func(context.Context) ([]cache.Share, error) { return []cache.Share{share}, nil },
		Manifest:         manifestStore,
		Store:            removalStore,
		ArrayReady:       arrayReady.hook,
	})
	params := mustJSON(t, EvacuationParams{Mountpoint: diskMount, Plan: plan})

	var lastCheckpoint []byte
	stopRequested, saveCheckpoint := interruptOnceManifestDurable(&lastCheckpoint)
	rc1 := &RunContext{
		id:             "job-1",
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
	if got := removalStore.get(diskMount); got != "evacuating" {
		t.Fatalf("removal_state after an interrupted (resumable) run = %q, want %q", got, "evacuating")
	}
	callsBeforeResume := arrayReady.callCount()
	if callsBeforeResume == 0 {
		t.Fatal("ArrayReady was never called before the interrupt")
	}

	// "Restart": a fresh RunContext, the same way a real daemon restart
	// followed by a user's Resume call re-enters this same RunFunc — never
	// a special restart-only code path (job.RunEvacuation has none).
	rc2 := &RunContext{
		id:            "job-1",
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
	if arrayReady.callCount() <= callsBeforeResume {
		t.Fatalf("ArrayReady call count after resume = %d, want more than %d — a resume must re-apply no-create, not assume it is still live", arrayReady.callCount(), callsBeforeResume)
	}
	if got := removalStore.get(diskMount); got != "evacuated" {
		t.Fatalf("removal_state after the resumed run completes = %q, want %q", got, "evacuated")
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
	manifestStore := &fakeRelocationManifestStore{
		manifest:      []parity.ManifestEntry{{RelPath: "docs/report.pdf", SourceDisk: "/mnt/disk9", TargetDisk: "/mnt/disk8"}},
		removingDisks: map[string]bool{"/mnt/disk9": true},
	}
	removalStore := newFakeRemovalStateStore()
	if err := removalStore.SetRemovalState(ctx, "/mnt/disk9", "evacuating", "other-job"); err != nil {
		t.Fatalf("seeding removal state: %v", err)
	}
	arrayReady := &fakeArrayReadyHook{}
	params := mustJSON(t, EvacuationParams{Mountpoint: "/mnt/disk1", Plan: cache.RebalancePlan{}})

	if err := EvacuationAbort(manifestStore, removalStore, arrayReady.hook)(ctx, "this-job", params); err != nil {
		t.Fatalf("EvacuationAbort: %v", err)
	}

	_, _, removingDisks := manifestStore.snapshot()
	if !removingDisks["/mnt/disk9"] {
		t.Fatalf("removingDisks = %+v, want the unrelated disk's own exemption left alone", removingDisks)
	}
	if got := removalStore.get("/mnt/disk9"); got != "evacuating" {
		t.Fatalf("removal_state for the unrelated disk = %q, want left alone (%q) — this abort names a different mountpoint", got, "evacuating")
	}
	if arrayReady.callCount() != 0 {
		t.Fatalf("ArrayReady was called %d time(s), want 0 — this abort is a no-op for an unrelated disk", arrayReady.callCount())
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
		Store:            newFakeRemovalStateStore(),
		ArrayReady:       func(context.Context) error { return nil },
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

// TestArrayRemovalState_SurvivesRelocationManifestClear is this issue's
// own regression test for "a share relocation or mover run after the
// evacuation does not clear the state": RunShareRelocation and
// job.RunRebalance both end a clean run by calling
// *parity.RelocationManifestStore.Replace(ctx, nil, nil) to clear their
// own guard-exemption bookkeeping (share_relocation_run.go,
// rebalance_run.go) — an entirely different table from array_disks.
// removal_state (#359). This proves the two stay independent against a
// real, migrated database, not only by code inspection.
func TestArrayRemovalState_SurvivesRelocationManifestClear(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	arrayStore := hoservastore.NewArrayStore(db)
	settings := hoservastore.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "10G", CreatedAt: time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)}
	disks := []hoservastore.ArrayDisk{
		{Role: hoservastore.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d1", Mountpoint: "/mnt/disk1"},
		{Role: hoservastore.ArrayRoleData, RoleIndex: 2, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "uuid-d2", Mountpoint: "/mnt/disk2"},
	}
	if err := arrayStore.PutArray(ctx, settings, disks); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	if err := arrayStore.SetRemovalState(ctx, "/mnt/disk1", hoservastore.RemovalStateEvacuating, "job-1"); err != nil {
		t.Fatalf("SetRemovalState: %v", err)
	}

	manifestStore := parity.NewRelocationManifestStore(db)
	if err := manifestStore.Replace(ctx, []parity.ManifestEntry{{SourceDisk: "/mnt/disk2", TargetDisk: "/mnt/cache", RelPath: "unrelated.bin"}}, nil); err != nil {
		t.Fatalf("Replace (persist): %v", err)
	}
	// The exact call RunShareRelocation and job.RunRebalance both make on
	// a clean finish.
	if err := manifestStore.Replace(ctx, nil, nil); err != nil {
		t.Fatalf("Replace(nil, nil): %v", err)
	}

	mountpoint, state, err := arrayStore.RemovingDisk(ctx)
	if err != nil {
		t.Fatalf("RemovingDisk: %v", err)
	}
	if mountpoint != "/mnt/disk1" || state != hoservastore.RemovalStateEvacuating {
		t.Fatalf("array_disks removal state after clearing the relocation manifest = (%q, %q), want unchanged (/mnt/disk1, %q) — the two tables must stay independent", mountpoint, state, hoservastore.RemovalStateEvacuating)
	}
}
