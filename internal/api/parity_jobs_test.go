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

	mu           sync.Mutex
	lastSync     parity.SyncOpts
	wroteParity  bool
	lastScrub    int
	lastScrubAge int
	lastFix      parity.FixOpts
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
	r.lastScrubAge = olderThanDays
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

// awaitJob's own deadline has to clear the test database's own
// busy_timeout(5000) (handler_test.go's newTestHandler, store.DSN) with
// real headroom, not race it — a 2s budget could lose to a store write
// that legitimately retries for up to 5s under load, matching dcb18bf's
// own 10s Drain budget for the same store.
func awaitJob(t *testing.T, s *job.Scheduler, id string) *job.Job {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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

func TestHandler_StartScrub_AllBlocksReachesEngine(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  func() *apiv1.StartScrubRequest
		want int
	}{
		{"omitted", func() *apiv1.StartScrubRequest { return &apiv1.StartScrubRequest{} }, parity.DefaultScrubOlderThanDays},
		{"false", func() *apiv1.StartScrubRequest {
			req := &apiv1.StartScrubRequest{}
			req.SetAllBlocks(apiv1.NewOptBool(false))
			return req
		}, parity.DefaultScrubOlderThanDays},
		{"true", func() *apiv1.StartScrubRequest {
			req := &apiv1.StartScrubRequest{}
			req.SetPercent(apiv1.NewOptInt32(100))
			req.SetAllBlocks(apiv1.NewOptBool(true))
			return req
		}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			h, s, r := newTestHandler(t)
			eng := newRecordingParity()
			r.Register(job.TypeScrub, false, job.RunScrub(eng))

			got, err := h.StartScrub(ctx, tc.req())
			if err != nil {
				t.Fatalf("StartScrub: %v", err)
			}
			finished := awaitJob(t, s, got.ID.String())
			if finished.Status != job.StatusSucceeded {
				t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
			}
			eng.mu.Lock()
			age := eng.lastScrubAge
			eng.mu.Unlock()
			if age != tc.want {
				t.Fatalf("Scrub olderThanDays = %d, want %d", age, tc.want)
			}
			all, err := job.ScrubAllBlocksFromParams(finished.Params)
			if err != nil {
				t.Fatalf("ScrubAllBlocksFromParams(%s): %v", finished.Params, err)
			}
			if all != (tc.want == 0) {
				t.Fatalf("persisted params %s read allBlocks = %v", finished.Params, all)
			}
		})
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

func TestHandler_StartFix_PathReachesEngineAndIsStoredInTheJob(t *testing.T) {
	ctx := context.Background()
	h, s, r := newTestHandler(t)
	eng := newRecordingParity()
	r.Register(job.TypeFix, false, job.RunFix(eng))

	req := &apiv1.StartFixRequest{Confirm: true}
	req.SetPath(apiv1.NewOptString("/mnt/user/documents/tax return.pdf"))
	got, err := h.StartFix(ctx, req)
	if err != nil {
		t.Fatalf("StartFix: %v", err)
	}
	finished := awaitJob(t, s, got.ID.String())
	if finished.Status != job.StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	_, _, _, lastFix := eng.snapshot()
	if lastFix.Path != "/documents/tax return.pdf" || lastFix.Disk != "" {
		t.Fatalf("FixOpts = %+v, want Path /documents/tax return.pdf and no Disk", lastFix)
	}
	stored, err := job.FixPathFromParams(finished.Params)
	if err != nil || stored != "/mnt/user/documents/tax return.pdf" {
		t.Fatalf("persisted params %s read path %q, %v", finished.Params, stored, err)
	}
}

func TestHandler_StartFix_WithoutPathStaysUnfiltered(t *testing.T) {
	ctx := context.Background()
	h, s, r := newTestHandler(t)
	eng := newRecordingParity()
	r.Register(job.TypeFix, false, job.RunFix(eng))

	got, err := h.StartFix(ctx, &apiv1.StartFixRequest{Confirm: true})
	if err != nil {
		t.Fatalf("StartFix: %v", err)
	}
	finished := awaitJob(t, s, got.ID.String())
	if finished.Status != job.StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	if _, _, _, lastFix := eng.snapshot(); lastFix != (parity.FixOpts{}) {
		t.Fatalf("FixOpts = %+v, want the zero value (a whole-array fix)", lastFix)
	}
}

func TestHandler_StartFix_RefusesABadPathWith400AndQueuesNothing(t *testing.T) {
	ctx := context.Background()
	h, _, r := newTestHandler(t)
	eng := newRecordingParity()
	r.Register(job.TypeFix, false, job.RunFix(eng))

	for name, req := range map[string]*apiv1.StartFixRequest{
		"empty":            fixRequest("", 0),
		"outside the pool": fixRequest("/mnt/disk1/documents/tax.pdf", 0),
		"dot dot":          fixRequest("/mnt/user/documents/../../etc/shadow", 0),
		"a directory":      fixRequest("/mnt/user/documents/", 0),
		"nul":              fixRequest("/mnt/user/doc\x00uments/tax.pdf", 0),
		"a pattern":        fixRequest("/mnt/user/documents/*", 0),
		"with a disk":      fixRequest("/mnt/user/documents/tax.pdf", 2),
	} {
		_, err := h.StartFix(ctx, req)
		status := apiError(t, h, err)
		if status.StatusCode != 400 || status.Response.Code != "invalid_fix_path" {
			t.Errorf("%s: StartFix error = %+v, want 400 invalid_fix_path", name, status)
		}
	}
	jobs, err := h.ListJobs(ctx, apiv1.ListJobsParams{})
	if err != nil || len(jobs.Jobs) != 0 {
		t.Fatalf("ListJobs after the refusals = %+v, %v, want no job", jobs, err)
	}
	if _, _, _, lastFix := eng.snapshot(); lastFix != (parity.FixOpts{}) {
		t.Fatalf("a refused request reached the engine: %+v", lastFix)
	}
}

func fixRequest(path string, disk int32) *apiv1.StartFixRequest {
	req := &apiv1.StartFixRequest{Confirm: true}
	req.SetPath(apiv1.NewOptString(path))
	if disk != 0 {
		req.SetDisk(apiv1.NewOptInt32(disk))
	}
	return req
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
