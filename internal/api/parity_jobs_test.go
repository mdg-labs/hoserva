package api_test

import (
	"context"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/parity"
)

type recordingParity struct {
	*parity.FakeEngine

	mu          sync.Mutex
	lastSync    parity.SyncOpts
	wroteParity bool
	lastScrub   int
	lastFix     parity.FixOpts
}

func newRecordingParity() *recordingParity {
	f := parity.NewFakeEngine()
	f.Sleep = func(time.Duration) {}
	return &recordingParity{FakeEngine: f}
}

func (r *recordingParity) Sync(ctx context.Context, opts parity.SyncOpts) (<-chan parity.Progress, error) {
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

func (r *recordingParity) Scrub(ctx context.Context, pct, olderThanDays int) (<-chan parity.Progress, error) {
	r.mu.Lock()
	r.lastScrub = pct
	r.mu.Unlock()
	return r.FakeEngine.Scrub(ctx, pct, olderThanDays)
}

func (r *recordingParity) Fix(ctx context.Context, opts parity.FixOpts) (<-chan parity.Progress, error) {
	r.mu.Lock()
	r.lastFix = opts
	r.mu.Unlock()
	return r.FakeEngine.Fix(ctx, opts)
}

func (r *recordingParity) snapshot() (parity.SyncOpts, bool, int, parity.FixOpts) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastSync, r.wroteParity, r.lastScrub, r.lastFix
}

func awaitJob(t *testing.T, s *job.Scheduler, id string) *job.Job {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	j, err := s.Await(ctx, id)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	return j
}

func apiTrippedGuard() parity.GuardResult {
	return parity.GuardResult{
		Blocked:      true,
		Triggers:     []parity.GuardTrigger{parity.TriggerRemovedCount},
		RemovedCount: 600,
	}
}

// TestHandler_StartSync_GuardBlockWithoutConfirmDoesNotWriteParity is the
// data-loss scenario at the HTTP boundary: startSync without confirm must
// not let a tripped threshold guard proceed to write parity.
func TestHandler_StartSync_GuardBlockWithoutConfirmDoesNotWriteParity(t *testing.T) {
	ctx := context.Background()
	h, s, r := newTestHandler(t)
	eng := newRecordingParity()
	eng.ScriptGuardBlock(apiTrippedGuard())
	eng.ScriptSync([]parity.Progress{{Phase: "syncing", Percent: 100}}, nil)
	r.Register(job.TypeSync, false, job.RunSync(eng))

	got, err := h.StartSync(ctx, &apiv1.StartSyncRequest{})
	if err != nil {
		t.Fatalf("StartSync: %v", err)
	}
	finished := awaitJob(t, s, got.ID.String())
	if finished.Status != job.StatusFailed {
		t.Fatalf("status = %s, want failed", finished.Status)
	}
	lastSync, wrote, _, _ := eng.snapshot()
	if lastSync.Confirm {
		t.Fatal("confirm reached SyncOpts when startSync did not set it")
	}
	if wrote {
		t.Fatal("sync proceeded past a tripped threshold guard without confirm — parity would have been written over the deletions")
	}
}

func TestHandler_StartSync_ConfirmReachesEngine(t *testing.T) {
	ctx := context.Background()
	h, s, r := newTestHandler(t)
	eng := newRecordingParity()
	eng.ScriptGuardBlock(apiTrippedGuard())
	eng.ScriptSync([]parity.Progress{{Phase: "syncing", Percent: 100}}, nil)
	r.Register(job.TypeSync, false, job.RunSync(eng))

	req := &apiv1.StartSyncRequest{}
	req.SetConfirm(apiv1.NewOptBool(true))
	got, err := h.StartSync(ctx, req)
	if err != nil {
		t.Fatalf("StartSync: %v", err)
	}
	finished := awaitJob(t, s, got.ID.String())
	if finished.Status != job.StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	lastSync, wrote, _, _ := eng.snapshot()
	if !lastSync.Confirm {
		t.Fatal("confirm did not reach SyncOpts")
	}
	if !wrote {
		t.Fatal("confirmed sync did not proceed after the guard block")
	}
}

func TestHandler_StartSync_DryRunDoesNotWriteParity(t *testing.T) {
	ctx := context.Background()
	h, s, r := newTestHandler(t)
	eng := newRecordingParity()
	eng.ScriptSync([]parity.Progress{{Phase: "diff", Percent: 100}}, nil)
	r.Register(job.TypeSync, false, job.RunSync(eng))

	req := &apiv1.StartSyncRequest{}
	req.SetDryRun(apiv1.NewOptBool(true))
	got, err := h.StartSync(ctx, req)
	if err != nil {
		t.Fatalf("StartSync: %v", err)
	}
	finished := awaitJob(t, s, got.ID.String())
	if finished.Status != job.StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	lastSync, wrote, _, _ := eng.snapshot()
	if !lastSync.DryRun {
		t.Fatal("dryRun did not reach SyncOpts")
	}
	if wrote {
		t.Fatal("dry-run sync wrote parity")
	}
}

func TestHandler_StartScrub_PercentReachesEngine(t *testing.T) {
	ctx := context.Background()
	h, s, r := newTestHandler(t)
	eng := newRecordingParity()
	r.Register(job.TypeScrub, false, job.RunScrub(eng))

	req := &apiv1.StartScrubRequest{}
	req.SetPercent(apiv1.NewOptInt32(25))
	got, err := h.StartScrub(ctx, req)
	if err != nil {
		t.Fatalf("StartScrub: %v", err)
	}
	finished := awaitJob(t, s, got.ID.String())
	if finished.Status != job.StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	_, _, pct, _ := eng.snapshot()
	if pct != 25 {
		t.Fatalf("Scrub percent = %d, want 25", pct)
	}
}

func TestHandler_StartScrub_OmittedPercentUsesDefault(t *testing.T) {
	ctx := context.Background()
	h, s, r := newTestHandler(t)
	eng := newRecordingParity()
	r.Register(job.TypeScrub, false, job.RunScrub(eng))

	got, err := h.StartScrub(ctx, &apiv1.StartScrubRequest{})
	if err != nil {
		t.Fatalf("StartScrub: %v", err)
	}
	finished := awaitJob(t, s, got.ID.String())
	if finished.Status != job.StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	_, _, pct, _ := eng.snapshot()
	if pct != job.DefaultScrubPercent {
		t.Fatalf("Scrub percent = %d, want default %d", pct, job.DefaultScrubPercent)
	}
}

func TestHandler_StartFix_RequiresConfirm(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newTestHandler(t)

	_, err := h.StartFix(ctx, &apiv1.StartFixRequest{Confirm: false})
	status := apiError(t, h, err)
	if status.StatusCode != 409 || status.Response.Code != "confirmation_required" {
		t.Fatalf("StartFix(confirm=false) error = %+v, want 409 confirmation_required", status)
	}
}

func TestHandler_StartFix_DiskReachesEngine(t *testing.T) {
	ctx := context.Background()
	h, s, r := newTestHandler(t)
	eng := newRecordingParity()
	r.Register(job.TypeFix, false, job.RunFix(eng))

	req := &apiv1.StartFixRequest{Confirm: true}
	req.SetDisk(apiv1.NewOptInt32(3))
	got, err := h.StartFix(ctx, req)
	if err != nil {
		t.Fatalf("StartFix: %v", err)
	}
	finished := awaitJob(t, s, got.ID.String())
	if finished.Status != job.StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	_, _, _, lastFix := eng.snapshot()
	if lastFix.Disk != "d3" {
		t.Fatalf("FixOpts.Disk = %q, want d3", lastFix.Disk)
	}
}
