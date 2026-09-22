package job

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/parity"
)

// recordingEngine wraps FakeEngine to capture the last Sync/Scrub/Fix
// arguments a RunFunc actually passed. wroteParity is set only when Sync
// returns a progress channel without DryRun — matching SnapraidEngine,
// which never writes parity for a dry-run or a guard block.
type recordingEngine struct {
	*parity.FakeEngine

	mu           sync.Mutex
	lastSync     parity.SyncOpts
	wroteParity  bool
	lastScrubPct int
	lastScrubAge int
	lastFix      parity.FixOpts

	// relocationManifest, relocationRemovingDisks and relocationErr script
	// CurrentRelocationManifest — RunSync's own relocationManifestSource
	// type-assertion always finds this method on recordingEngine, so every
	// existing test above keeps its pre-#194 nil-manifest behaviour by
	// simply never scripting it.
	relocationManifest      []parity.ManifestEntry
	relocationRemovingDisks map[string]bool
	relocationErr           error
}

func newRecordingEngine() *recordingEngine {
	f := parity.NewFakeEngine()
	f.Sleep = func(time.Duration) {}
	return &recordingEngine{FakeEngine: f}
}

func (r *recordingEngine) Sync(ctx context.Context, opts parity.SyncOpts) (<-chan parity.Progress, error) {
	r.mu.Lock()
	r.lastSync = opts
	r.mu.Unlock()
	ch, err := r.FakeEngine.Sync(ctx, opts)
	if err == nil && !opts.DryRun {
		r.mu.Lock()
		r.wroteParity = true
		r.mu.Unlock()
	}
	return ch, err
}

func (r *recordingEngine) Scrub(ctx context.Context, pct, olderThanDays int) (<-chan parity.Progress, error) {
	r.mu.Lock()
	r.lastScrubPct = pct
	r.lastScrubAge = olderThanDays
	r.mu.Unlock()
	return r.FakeEngine.Scrub(ctx, pct, olderThanDays)
}

func (r *recordingEngine) Fix(ctx context.Context, opts parity.FixOpts) (<-chan parity.Progress, error) {
	r.mu.Lock()
	r.lastFix = opts
	r.mu.Unlock()
	return r.FakeEngine.Fix(ctx, opts)
}

func (r *recordingEngine) snapshot() (parity.SyncOpts, bool, int, parity.FixOpts) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastSync, r.wroteParity, r.lastScrubPct, r.lastFix
}

// ScriptRelocationManifest scripts CurrentRelocationManifest's next result.
func (r *recordingEngine) ScriptRelocationManifest(manifest []parity.ManifestEntry, removingDisks map[string]bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.relocationManifest = manifest
	r.relocationRemovingDisks = removingDisks
	r.relocationErr = err
}

func (r *recordingEngine) CurrentRelocationManifest(ctx context.Context) ([]parity.ManifestEntry, map[string]bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.relocationManifest, r.relocationRemovingDisks, r.relocationErr
}

func intPtr(n int) *int { return &n }

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

func await(t *testing.T, s *Scheduler, id string) *Job {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	j, err := s.Await(ctx, id)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	return j
}

func trippedGuard() parity.GuardResult {
	return parity.GuardResult{
		Blocked:      true,
		Triggers:     []parity.GuardTrigger{parity.TriggerRemovedCount},
		RemovedCount: 600,
	}
}

// TestRunSync_GuardBlockWithoutConfirmDoesNotWriteParity is the data-loss
// scenario: a sync that would otherwise proceed past a tripped threshold
// guard because confirm never reached SyncOpts.Confirm. The guard still
// evaluates (FakeEngine's scripted block); without Confirm, nothing syncs.
func TestRunSync_GuardBlockWithoutConfirmDoesNotWriteParity(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	eng := newRecordingEngine()
	eng.ScriptGuardBlock(trippedGuard())
	eng.ScriptSync([]parity.Progress{{Phase: "syncing", Percent: 100}}, nil)
	s.registry.Register(TypeSync, false, RunSync(eng))

	j, err := s.Submit(ctx, TypeSync, nil, mustJSON(t, SyncParams{Confirm: false}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s, want failed", finished.Status)
	}
	lastSync, wrote, _, _ := eng.snapshot()
	if lastSync.Confirm {
		t.Fatal("SyncOpts.Confirm was true even though the request did not set it")
	}
	if wrote {
		t.Fatal("sync proceeded past a tripped threshold guard without confirm — parity would have been written over the deletions")
	}
	if !strings.Contains(finished.ErrorMessage, "threshold guard blocked the sync") {
		t.Fatalf("ErrorMessage = %q, want a threshold-guard block", finished.ErrorMessage)
	}
}

func TestRunSync_ConfirmProceedsPastGuardBlock(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	eng := newRecordingEngine()
	eng.ScriptGuardBlock(trippedGuard())
	eng.ScriptSync([]parity.Progress{{Phase: "syncing", Percent: 100}}, nil)
	s.registry.Register(TypeSync, false, RunSync(eng))

	j, err := s.Submit(ctx, TypeSync, nil, mustJSON(t, SyncParams{Confirm: true}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	lastSync, wrote, _, _ := eng.snapshot()
	if !lastSync.Confirm {
		t.Fatal("SyncOpts.Confirm was false; confirm did not reach the engine")
	}
	if !wrote {
		t.Fatal("confirmed sync did not proceed after the guard block")
	}
}

func TestRunSync_DryRunDoesNotWriteParity(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	eng := newRecordingEngine()
	eng.ScriptSync([]parity.Progress{{Phase: "diff", Percent: 100}}, nil)
	s.registry.Register(TypeSync, false, RunSync(eng))

	j, err := s.Submit(ctx, TypeSync, nil, mustJSON(t, SyncParams{DryRun: true}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	lastSync, wrote, _, _ := eng.snapshot()
	if !lastSync.DryRun || lastSync.Confirm {
		t.Fatalf("SyncOpts = %+v, want DryRun true and Confirm false", lastSync)
	}
	if wrote {
		t.Fatal("dry-run sync wrote parity")
	}
}

func TestSubmit_ParamsSurviveRestartBetweenQueueAndRun(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	jobStore := NewStore(db)
	s := NewScheduler(jobStore, NewLogStore(t.TempDir()), NewHub(), NewRegistry())
	started, release := registerBlocking(s, TypeSync, false)
	syncJob, err := s.Submit(ctx, TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit running sync: %v", err)
	}
	<-started

	s.registry.Register(TypeScrub, false, func(context.Context, *RunContext) error { return nil })
	params := mustJSON(t, ScrubParams{Percent: intPtr(25)})
	queued, err := s.Submit(ctx, TypeScrub, nil, params)
	if err != nil {
		t.Fatalf("Submit queued scrub: %v", err)
	}
	if queued.Status != StatusQueued {
		t.Fatalf("status = %s, want queued", queued.Status)
	}
	// Release the blocking sync job and wait for it, and the scrub job
	// it unblocks, to finish recording their final status before
	// t.Cleanup closes db (registered earlier, so it runs after this
	// one) — otherwise those writes race the DB close.
	t.Cleanup(func() {
		close(release)
		await(t, s, syncJob.ID)
		await(t, s, queued.ID)
	})

	fresh := NewStore(db)
	got, err := fresh.Get(ctx, queued.ID)
	if err != nil {
		t.Fatalf("Get after restart: %v", err)
	}
	if string(got.Params) != string(params) {
		t.Fatalf("params after restart = %s, want %s", got.Params, params)
	}
}

func TestSubmit_QueuedJobRunFuncReadsPersistedParams(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	eng := newRecordingEngine()
	started, release := registerBlocking(s, TypeSync, false)
	s.registry.Register(TypeScrub, false, RunScrub(eng))

	if _, err := s.Submit(ctx, TypeSync, nil, nil); err != nil {
		t.Fatalf("Submit sync: %v", err)
	}
	<-started

	queued, err := s.Submit(ctx, TypeScrub, nil, mustJSON(t, ScrubParams{Percent: intPtr(40)}))
	if err != nil {
		t.Fatalf("Submit scrub: %v", err)
	}
	close(release)
	finished := await(t, s, queued.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	_, _, pct, _ := eng.snapshot()
	if pct != 40 {
		t.Fatalf("Scrub percent = %d, want 40 (params must be read from the job row, not the HTTP request)", pct)
	}
}

func TestRunScrub_OmittedPercentUsesDefault(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	eng := newRecordingEngine()
	s.registry.Register(TypeScrub, false, RunScrub(eng))

	j, err := s.Submit(ctx, TypeScrub, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	_, _, pct, _ := eng.snapshot()
	if pct != DefaultScrubPercent {
		t.Fatalf("Scrub percent = %d, want default %d", pct, DefaultScrubPercent)
	}
}

func TestRunFix_DiskReachesFixOpts(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	eng := newRecordingEngine()
	s.registry.Register(TypeFix, false, RunFix(eng))

	j, err := s.Submit(ctx, TypeFix, nil, mustJSON(t, FixParams{Confirm: true, Disk: intPtr(3)}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	_, _, _, lastFix := eng.snapshot()
	if lastFix.Disk != "d3" {
		t.Fatalf("FixOpts.Disk = %q, want d3", lastFix.Disk)
	}
}

func TestSubmit_RejectsParamsForTypesThatHaveNone(t *testing.T) {
	s := newTestScheduler(t)
	s.registry.Register(TypeMover, false, func(context.Context, *RunContext) error { return nil })
	_, err := s.Submit(context.Background(), TypeMover, nil, []byte(`{"dryRun":true}`))
	if err == nil {
		t.Fatal("Submit(mover, params) = nil error, want rejection")
	}
}

func TestSubmit_ACMEIssueAcceptsRenewParam(t *testing.T) {
	s := newTestScheduler(t)
	var gotRenew bool
	s.registry.Register(TypeACMEIssue, true, RunACMEIssue(func(_ context.Context, renew bool) error {
		gotRenew = renew
		return nil
	}))
	j, err := s.Submit(context.Background(), TypeACMEIssue, []string{"tls"}, mustJSON(t, ACMEIssueParams{Renew: true}))
	if err != nil {
		t.Fatal(err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("status = %s", finished.Status)
	}
	if !gotRenew {
		t.Fatal("expected renew=true")
	}
}

func TestSubmit_RejectsFixWithoutConfirm(t *testing.T) {
	s := newTestScheduler(t)
	s.registry.Register(TypeFix, false, RunFix(newRecordingEngine()))
	for _, params := range [][]byte{
		[]byte(`{"confirm":false,"disk":1}`),
		nil,
		[]byte("null"),
		[]byte(""),
	} {
		_, err := s.Submit(context.Background(), TypeFix, nil, params)
		if err == nil {
			t.Fatalf("Submit(fix, params=%q) = nil error, want rejection", params)
		}
	}
}

func TestValidateParams_RejectsTrailingJSON(t *testing.T) {
	for _, params := range [][]byte{
		[]byte(`{"dryRun":true}]`),
		[]byte(`{"dryRun":true}{"confirm":true}`),
	} {
		if err := ValidateParams(TypeSync, params); err == nil {
			t.Fatalf("ValidateParams(%q) = nil, want trailing-token rejection", params)
		}
	}
}

// TestRunSync_LoadsRelocationManifestAndRemovingDisksFromEngine confirms
// RunSync's production wiring boundary (#194): when eng implements
// relocationManifestSource, whatever it currently has persisted reaches
// SyncOpts.Manifest/RemovingDisks — the exact fields the real
// SnapraidEngine.Sync's own guard evaluation (Q15) reads.
func TestRunSync_LoadsRelocationManifestAndRemovingDisksFromEngine(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	eng := newRecordingEngine()
	manifest := []parity.ManifestEntry{{RelPath: "movies/a.mkv", SourceDisk: "/mnt/disk1", TargetDisk: "/mnt/disk2"}}
	removingDisks := map[string]bool{"/mnt/disk3": true}
	eng.ScriptRelocationManifest(manifest, removingDisks, nil)
	eng.ScriptSync([]parity.Progress{{Phase: "syncing", Percent: 100}}, nil)
	s.registry.Register(TypeSync, false, RunSync(eng))

	j, err := s.Submit(ctx, TypeSync, nil, mustJSON(t, SyncParams{}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	lastSync, _, _, _ := eng.snapshot()
	if len(lastSync.Manifest) != 1 || lastSync.Manifest[0].RelPath != "movies/a.mkv" {
		t.Fatalf("SyncOpts.Manifest = %+v, want the engine's persisted manifest", lastSync.Manifest)
	}
	if !lastSync.RemovingDisks["/mnt/disk3"] {
		t.Fatalf("SyncOpts.RemovingDisks = %+v, want /mnt/disk3", lastSync.RemovingDisks)
	}
}

// TestRunSync_RelocationManifestLoadErrorFailsClosed is the data-loss
// scenario this issue is safety-critical against: a transient failure
// reading the persisted manifest must never fall back to silently syncing
// with an incomplete picture of what the guard should account for —
// RunSync must fail the job before ever calling eng.Sync at all, so
// nothing writes parity on a guard evaluation this run could not actually
// trust.
func TestRunSync_RelocationManifestLoadErrorFailsClosed(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	eng := newRecordingEngine()
	eng.ScriptRelocationManifest(nil, nil, errors.New("relocation manifest store: boom"))
	eng.ScriptSync([]parity.Progress{{Phase: "syncing", Percent: 100}}, nil)
	s.registry.Register(TypeSync, false, RunSync(eng))

	j, err := s.Submit(ctx, TypeSync, nil, mustJSON(t, SyncParams{}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s, want failed", finished.Status)
	}
	if !strings.Contains(finished.ErrorMessage, "boom") {
		t.Fatalf("ErrorMessage = %q, want it to surface the relocation manifest load failure", finished.ErrorMessage)
	}
	_, wrote, _, _ := eng.snapshot()
	if wrote {
		t.Fatal("sync proceeded even though the relocation manifest could not be loaded — parity would have been written with an unknown accounting state")
	}
}

func TestSyncOptsFromParams_ConfirmDoesNotSkipGuardFields(t *testing.T) {
	opts, err := SyncOptsFromParams(mustJSON(t, SyncParams{Confirm: true}))
	if err != nil {
		t.Fatalf("SyncOptsFromParams: %v", err)
	}
	if !opts.Confirm {
		t.Fatal("Confirm = false, want true")
	}
	if opts.DryRun {
		t.Fatal("DryRun = true, want false")
	}
	if opts.Manifest != nil || opts.RemovingDisks != nil {
		t.Fatalf("Confirm must not skip evaluation via Manifest/RemovingDisks: %+v", opts)
	}
}
