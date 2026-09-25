package job

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mdg-labs/hoserva/internal/cache"
	"github.com/mdg-labs/hoserva/internal/parity"
)

// TestScheduler_CancelRace_AfterRunFuncReturns_IsRefused is #364's own
// deterministic reproduction of the race the issue reports: a Cancel call
// that lands after a job's RunFunc has already returned — and settled its
// real, final outcome — but before runJob has removed the job from
// s.running. It uses Scheduler.settleHook, a synchronization hook runJob
// calls in exactly that window, to land the Cancel there every time,
// never a sleep or a retry loop. Without the fix (Cancel refusing once
// rj.finished is set), this Cancel would instead mutate reason to
// reasonCancel and the job would be misrecorded StatusCancelled, even
// though its RunFunc actually succeeded.
func TestScheduler_CancelRace_AfterRunFuncReturns_IsRefused(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	s.registry.Register(TypeSync, true, func(ctx context.Context, rc *RunContext) error {
		return nil
	})

	settled := make(chan struct{})
	proceed := make(chan struct{})
	s.settleHook = func(jobID string) {
		close(settled)
		<-proceed
	}

	j, err := s.Submit(ctx, TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	<-settled // RunFunc has returned and rj.finished is set, but the job
	// is still in s.running — exactly the window the issue describes.

	if _, err := s.Cancel(ctx, j.ID); !errors.Is(err, ErrJobNotRunning) {
		t.Fatalf("Cancel in the settle window = %v, want ErrJobNotRunning (the job is already terminal)", err)
	}
	close(proceed)

	finished := await(t, s, j.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded — the race must never relabel the run's real outcome cancelled", finished.Status, finished.ErrorMessage)
	}
}

// TestScheduler_EvacuationCancelRace_AfterGracefulStop_DoesNotOrphanExemption
// is #364's own regression test for the #274/#359 scenario the report
// describes: a graceful maintenance-mode stop leaves RunEvacuation's
// relocation manifest and removing-disks exemption persisted for Resume
// (report.Interrupted, err == nil) — and, in the same settle window
// TestScheduler_CancelRace_AfterRunFuncReturns_IsRefused exercises
// directly, a Cancel call races in before runJob has removed the job from
// s.running. Without the fix, that Cancel would relabel the job
// StatusCancelled without ever invoking EvacuationAbort, so the manifest's
// removing-disks exemption would survive forever with no job left able to
// clear it — exactly the fail-open the issue reports. With the fix, the
// raced Cancel is refused, the job is correctly recorded interrupted (as
// its own graceful stop decided), and a second, legitimate Cancel of that
// now-actually-interrupted job reaches EvacuationAbort and clears both the
// manifest and the removal state, proving nothing was orphaned.
func TestScheduler_EvacuationCancelRace_AfterGracefulStop_DoesNotOrphanExemption(t *testing.T) {
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
		// Maintenance mode lands mid-sync, exactly as `hoserva array stop`
		// would while this job's own pre-delete sync is in flight — the
		// same graceful-stop shape
		// TestRunEvacuation_MaintenanceInterrupt_KeepsRemovingDisks_ThenCancelClears
		// exercises without the race.
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

	settled := make(chan struct{})
	proceed := make(chan struct{})
	s.settleHook = func(jobID string) {
		close(settled)
		<-proceed
	}

	j, err := s.Submit(ctx, TypeEvacuation, nil, mustJSON(t, EvacuationParams{Mountpoint: diskMount, Plan: plan}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	<-settled // RunEvacuation has already returned nil (graceful stop,
	// keeping the manifest and exemption for Resume), but the job is
	// still in s.running.

	if _, err := s.Cancel(ctx, j.ID); !errors.Is(err, ErrJobNotRunning) {
		t.Fatalf("Cancel in the settle window = %v, want ErrJobNotRunning", err)
	}
	close(proceed)

	finished := await(t, s, j.ID)
	if finished.Status != StatusInterrupted {
		t.Fatalf("status = %s (%s), want interrupted — the raced Cancel must not relabel a genuinely resumable stop cancelled", finished.Status, finished.ErrorMessage)
	}

	calls, manifest, removingDisks := manifestStore.snapshot()
	if calls == 0 {
		t.Fatal("store.Replace was never called — the batch's own manifest should have been persisted before the sync")
	}
	if len(manifest) == 0 || !removingDisks[diskMount] {
		t.Fatalf("after the raced Cancel, store = (manifest=%+v, removingDisks=%+v), want both still persisted, exactly as an unraced graceful stop leaves them", manifest, removingDisks)
	}
	if got := removalStore.get(diskMount); got != "evacuating" {
		t.Fatalf("removal_state after the raced Cancel = %q, want %q — Resume must still find the disk no-create", got, "evacuating")
	}

	// Nothing is orphaned: a legitimate Cancel of the now-actually-
	// interrupted job reaches EvacuationAbort and clears both.
	if _, err := s.Cancel(ctx, j.ID); err != nil {
		t.Fatalf("Cancel(interrupted evacuation): %v", err)
	}
	_, manifest, removingDisks = manifestStore.snapshot()
	if manifest != nil || removingDisks != nil {
		t.Fatalf("after cancelling the now-interrupted job, store = (manifest=%+v, removingDisks=%+v), want both cleared", manifest, removingDisks)
	}
	if got := removalStore.get(diskMount); got != "" {
		t.Fatalf("removal_state after cancelling the interrupted job = %q, want released", got)
	}
}

// TestScheduler_EvacuationCancel_ReapplyFailure_SurfacesCancelCleanupError
// is the ghotso review comment's own acceptance point on #364: the same
// runJob branch that used to let reasonCancel silently win over a run's
// real outcome also used to discard the error from a failed live re-apply
// after a cancel (RunEvacuation's own releaseRemovalState/ArrayReady call)
// — the database ends up saying RW while the live mounts stay no-create,
// and nothing ever recorded that. Driven through the real Scheduler (the
// same POST /jobs/{id}/cancel path cancelJob serves), a Cancel of a
// running evacuation whose live re-apply fails still ends the job
// StatusCancelled — the cancel itself is honoured — but must carry the
// re-apply failure's own code and message rather than silently dropping
// it.
func TestScheduler_EvacuationCancel_ReapplyFailure_SurfacesCancelCleanupError(t *testing.T) {
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
	// The first call is the pre-copy no-create application (must succeed
	// so the run actually starts); the second is the post-cancel re-apply
	// this test fails on purpose.
	arrayReady := &countingHook{onCall: func(ctx context.Context, n int32) error {
		if n == 2 {
			return errors.New("dial tcp: connection refused")
		}
		return nil
	}}

	started := make(chan struct{})
	release := make(chan struct{})
	share := cache.Share{Name: "media", Branches: []string{src}}
	sync := func(ctx context.Context, manifest []parity.ManifestEntry, removingDisks map[string]bool) error {
		close(started)
		<-release
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

	j, err := s.Submit(ctx, TypeEvacuation, nil, mustJSON(t, EvacuationParams{Mountpoint: diskMount, Plan: plan}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	<-started

	if _, err := s.Cancel(ctx, j.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	close(release)

	finished := await(t, s, j.ID)
	if finished.Status != StatusCancelled {
		t.Fatalf("status = %s (%s), want cancelled — the cancel itself must still be honoured even though its own cleanup failed", finished.Status, finished.ErrorMessage)
	}
	if finished.ErrorCode != "evacuation_cancel_reapply_failed" {
		t.Fatalf("ErrorCode = %q, want %q", finished.ErrorCode, "evacuation_cancel_reapply_failed")
	}
	if !strings.Contains(finished.ErrorMessage, "reapplying the live pool") {
		t.Fatalf("ErrorMessage = %q, want it to name the failed live re-apply rather than being silently dropped", finished.ErrorMessage)
	}
}

// failClearManifestStore wraps fakeRelocationManifestStore so Replace
// fails only when called to clear the manifest — a nil manifest and nil
// removingDisks, the exact signature every relocation-manifest clear uses
// (d.run's own non-resumable branch, clearOwnedEvacuationManifest) —
// letting the run's own per-batch persist calls succeed normally, so a
// test proves specifically that a failed *clear* is surfaced rather than
// silently dropped (#378).
type failClearManifestStore struct {
	*fakeRelocationManifestStore
	err error
}

func (f failClearManifestStore) Replace(ctx context.Context, manifest []parity.ManifestEntry, removingDisks map[string]bool) error {
	if manifest == nil && removingDisks == nil {
		return f.err
	}
	return f.fakeRelocationManifestStore.Replace(ctx, manifest, removingDisks)
}

// TestScheduler_EvacuationCancel_ManifestClearFailure_SurfacesCancelCleanupError
// is #378's own acceptance test for the dropped manifest-clear failure the
// #364 executor found: a Cancel of a running (not yet interrupted)
// evacuation always makes d.run take its non-resumable branch (ctx.Err()
// != nil there rules out resumable regardless of report.Interrupted), and
// that branch clears the persisted relocation manifest and removing-disks
// exemption before returning. Before the fix, a failure of that clear was
// wrapped only in a plain error, and runJob's own reasonCancel branch keeps
// nothing but a *CancelCleanupError under a cancelled outcome — the
// failure, and the exemption it could not release, were silently dropped.
// Driven through the real Scheduler, exactly like the sibling reapply-
// failure test above, this proves the job's recorded outcome now carries
// the failed clear's own code and message instead.
func TestScheduler_EvacuationCancel_ManifestClearFailure_SurfacesCancelCleanupError(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	base := t.TempDir()
	diskMount := filepath.Join(base, "disk1")
	src := filepath.Join(diskMount, "media")
	dst := filepath.Join(base, "disk2", "media")
	plan := newRebalanceTestPlan(t, src, dst, "media", "movie.mkv", "movie bytes")

	eng := newRecordingEngine()
	eng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)
	manifestStore := failClearManifestStore{
		fakeRelocationManifestStore: &fakeRelocationManifestStore{},
		err:                         errors.New("disk full"),
	}
	removalStore := newFakeRemovalStateStore()
	arrayReady := &fakeArrayReadyHook{}

	started := make(chan struct{})
	release := make(chan struct{})
	share := cache.Share{Name: "media", Branches: []string{src}}
	sync := func(ctx context.Context, manifest []parity.ManifestEntry, removingDisks map[string]bool) error {
		close(started)
		<-release
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

	j, err := s.Submit(ctx, TypeEvacuation, nil, mustJSON(t, EvacuationParams{Mountpoint: diskMount, Plan: plan}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	<-started

	if _, err := s.Cancel(ctx, j.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	close(release)

	finished := await(t, s, j.ID)
	if finished.Status != StatusCancelled {
		t.Fatalf("status = %s (%s), want cancelled — the cancel itself must still be honoured even though its own cleanup failed", finished.Status, finished.ErrorMessage)
	}
	if finished.ErrorCode != "evacuation_cancel_manifest_clear_failed" {
		t.Fatalf("ErrorCode = %q, want %q", finished.ErrorCode, "evacuation_cancel_manifest_clear_failed")
	}
	if !strings.Contains(finished.ErrorMessage, "clearing relocation manifest") {
		t.Fatalf("ErrorMessage = %q, want it to name the failed clear rather than being silently dropped", finished.ErrorMessage)
	}
}

// TestScheduler_EvacuationCancel_BetweenResumableDecisionAndReturn_ClearsExemption
// is #378's own acceptance test for the second window the #364 verifier
// found: a graceful maintenance-mode stop leaves d.run's own resumable
// check reading ctx.Err() == nil, so it decides the stop is resumable and
// returns without clearing the persisted relocation manifest or
// removing-disks exemption — exactly right for a genuine Resume. But a
// Cancel that lands in the narrow window between that decision and
// RunEvacuation's own outer ctx.Err() check is still accepted (the job is
// still running as far as Scheduler.Cancel is concerned), and before the
// fix nothing cleared the manifest or the exemption it left behind: the
// job ended cancelled, EvacuationAbort never ran (a cancel of a still-
// running job never reaches it), and the exemption survived every later
// scheduled sync. s.resumableDecisionHook lands the Cancel call exactly in
// that window, deterministically, instead of racing the real clock.
func TestScheduler_EvacuationCancel_BetweenResumableDecisionAndReturn_ClearsExemption(t *testing.T) {
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
		// Maintenance mode lands mid-sync, exactly as `hoserva array stop`
		// would: this is a graceful stop, never a hard ctx cancel, so
		// d.run's own resumable check reads ctx.Err() == nil.
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

	var hookCalls int32
	s.resumableDecisionHook = func(jobID string) {
		atomic.AddInt32(&hookCalls, 1)
		if _, err := s.Cancel(context.Background(), jobID); err != nil {
			t.Errorf("Cancel between the resumable decision and RunEvacuation's return: %v", err)
		}
	}

	j, err := s.Submit(ctx, TypeEvacuation, nil, mustJSON(t, EvacuationParams{Mountpoint: diskMount, Plan: plan}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	finished := await(t, s, j.ID)
	if atomic.LoadInt32(&hookCalls) == 0 {
		t.Fatal("resumableDecisionHook was never called — the test did not land the Cancel in the window it claims to")
	}
	if finished.Status != StatusCancelled {
		t.Fatalf("status = %s (%s), want cancelled", finished.Status, finished.ErrorMessage)
	}
	if finished.ErrorCode != "" || finished.ErrorMessage != "" {
		t.Fatalf("ErrorCode/ErrorMessage = %q/%q, want none — the clear succeeded, so the cancel must be silent", finished.ErrorCode, finished.ErrorMessage)
	}

	_, manifest, removingDisks := manifestStore.snapshot()
	if manifest != nil || removingDisks != nil {
		t.Fatalf("after a cancel landing between the resumable decision and RunEvacuation's return, store = (manifest=%+v, removingDisks=%+v), want both cleared — a resumable stop's exemption must never survive a cancel that preempts its own Resume", manifest, removingDisks)
	}
	if got := removalStore.get(diskMount); got != "" {
		t.Fatalf("removal_state after that cancel = %q, want released so the disk takes writes again", got)
	}
}

// TestScheduler_EvacuationCancelRace_AfterRunEvacuationReturns_DoesNotOrphanExemption
// reproduces the #378 fix-round verifier's own finding: a Cancel landing
// after RunEvacuation's own inner logic has already returned — including
// its outer ctx.Err() check — but before runJob has taken rj.mu to set
// rj.finished (scheduler.go's own runJob, right after `runErr := rj.run(ctx,
// rc)`). A best-effort second cleanup pass inside RunEvacuation's own outer
// function can never close this window, since it too has already returned
// by the time the race lands. It is closed instead by
// RunContext.KeepForResume committing rj.finished the instant d.run's own
// preliminary resumable check holds, long before RunEvacuation itself
// returns — so this Cancel, landing still later, must always find the job
// already unreachable and be refused, exactly like the #364 settle-window
// race it extends. The test wraps the real RunEvacuation in a RunFunc that
// calls Scheduler.Cancel synchronously the instant the real one returns,
// the same reproduction the verifier used against the rejected commit.
func TestScheduler_EvacuationCancelRace_AfterRunEvacuationReturns_DoesNotOrphanExemption(t *testing.T) {
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
		// A graceful maintenance-mode stop, never a hard ctx cancel, so
		// d.run's own preliminary resumable check reads ctx.Err() == nil.
		if err := s.EnterMaintenance(ctx); err != nil {
			t.Fatalf("EnterMaintenance: %v", err)
		}
		return evacuationSyncFuncFromEngine(eng)(ctx, manifest, removingDisks)
	}
	inner := RunEvacuation(EvacuationDeps{
		Sync:             sync,
		TrackedFileCount: func(context.Context) (int, error) { return 1000, nil },
		Shares:           func(context.Context) ([]cache.Share, error) { return []cache.Share{share}, nil },
		Manifest:         manifestStore,
		Store:            removalStore,
		ArrayReady:       arrayReady.hook,
	})

	cancelResult := make(chan error, 1)
	s.registry.Register(TypeEvacuation, true, func(ctx context.Context, rc *RunContext) error {
		err := inner(ctx, rc)
		_, cerr := s.Cancel(context.Background(), rc.JobID())
		cancelResult <- cerr
		return err
	})
	s.registry.RegisterAbort(TypeEvacuation, EvacuationAbort(manifestStore, removalStore, arrayReady.hook))

	j, err := s.Submit(ctx, TypeEvacuation, nil, mustJSON(t, EvacuationParams{Mountpoint: diskMount, Plan: plan}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	finished := await(t, s, j.ID)

	var cancelErr error
	select {
	case cancelErr = <-cancelResult:
	default:
		t.Fatal("the wrapped RunFunc never reached its own Cancel call")
	}
	if !errors.Is(cancelErr, ErrJobNotRunning) {
		t.Fatalf("Cancel immediately after RunEvacuation returned = %v, want ErrJobNotRunning — KeepForResume must already have closed this window", cancelErr)
	}
	if finished.Status != StatusInterrupted {
		t.Fatalf("status = %s (%s), want interrupted — a Cancel that could never take effect must not relabel a genuinely resumable stop cancelled", finished.Status, finished.ErrorMessage)
	}

	calls, manifest, removingDisks := manifestStore.snapshot()
	if calls == 0 {
		t.Fatal("store.Replace was never called — the batch's own manifest should have been persisted before the sync")
	}
	if len(manifest) == 0 || !removingDisks[diskMount] {
		t.Fatalf("after the raced Cancel, store = (manifest=%+v, removingDisks=%+v), want both still persisted for Resume, exactly as an unraced graceful stop leaves them", manifest, removingDisks)
	}
	if got := removalStore.get(diskMount); got != "evacuating" {
		t.Fatalf("removal_state after the raced Cancel = %q, want %q — Resume must still find the disk no-create", got, "evacuating")
	}

	// Nothing is orphaned: a legitimate Cancel of the now-actually-
	// interrupted job reaches EvacuationAbort and clears both.
	if _, err := s.Cancel(ctx, j.ID); err != nil {
		t.Fatalf("Cancel(interrupted evacuation): %v", err)
	}
	_, manifest, removingDisks = manifestStore.snapshot()
	if manifest != nil || removingDisks != nil {
		t.Fatalf("after cancelling the now-interrupted job, store = (manifest=%+v, removingDisks=%+v), want both cleared", manifest, removingDisks)
	}
	if got := removalStore.get(diskMount); got != "" {
		t.Fatalf("removal_state after cancelling the interrupted job = %q, want released", got)
	}
}

// TestScheduler_EvacuationCancel_ResumedRun_DuringPreCopyArrayReady_ClearsExemption
// is the maintainer's added #378 scope: a resumed evacuation re-enters
// d.run, whose two pre-copy steps (Deps.Store.SetRemovalState, the
// pre-copy Deps.ArrayReady) run again before RunRebalance is ever
// re-entered — before the resumable check further down d.run that the
// sibling tests above exercise. A Cancel landing during that resumed
// pre-copy ArrayReady used to be accepted with the manifest and
// removing-disks exemption an earlier graceful stop had committed for
// Resume left exactly as that stop persisted them: EvacuationAbort never
// runs for a job that never became interrupted, so every later sync would
// exempt the disk from the guard's zero-files rule forever (fail-open).
// The first run is interrupted by a real, unraced maintenance-mode stop;
// the countingHook lands the Cancel deterministically inside the second
// (resumed) ArrayReady call, no sleep.
func TestScheduler_EvacuationCancel_ResumedRun_DuringPreCopyArrayReady_ClearsExemption(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	base := t.TempDir()
	diskMount := filepath.Join(base, "disk1")
	src := filepath.Join(diskMount, "media")
	dst := filepath.Join(base, "disk2", "media")
	plan := newRebalanceTestPlan(t, src, dst, "media", "movie.mkv", "movie bytes")

	eng := newRecordingEngine()
	eng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)
	eng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)
	manifestStore := &fakeRelocationManifestStore{}
	removalStore := newFakeRemovalStateStore()
	inResumeArrayReady := make(chan struct{})
	arrayReady := &countingHook{onCall: func(ctx context.Context, n int32) error {
		if n == 2 {
			close(inResumeArrayReady)
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}}

	share := cache.Share{Name: "media", Branches: []string{src}}
	sync := func(ctx context.Context, manifest []parity.ManifestEntry, removingDisks map[string]bool) error {
		if err := s.EnterMaintenance(ctx); err != nil {
			t.Errorf("EnterMaintenance: %v", err)
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
	first := await(t, s, j.ID)
	if first.Status != StatusInterrupted {
		t.Fatalf("first run status = %s (%s), want interrupted", first.Status, first.ErrorMessage)
	}
	s.ExitMaintenance()

	if _, err := s.Resume(ctx, j.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	<-inResumeArrayReady
	if _, err := s.Cancel(ctx, j.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	finished := await(t, s, j.ID)
	if finished.Status != StatusCancelled {
		t.Fatalf("status = %s (%s), want cancelled", finished.Status, finished.ErrorMessage)
	}
	_, manifest, removingDisks := manifestStore.snapshot()
	if manifest != nil || removingDisks != nil {
		t.Fatalf("FAIL-OPEN: cancelled with manifest/exemption still persisted: manifest=%+v removingDisks=%+v", manifest, removingDisks)
	}
	if got := removalStore.get(diskMount); got != "" {
		t.Fatalf("removal_state after that cancel = %q, want released so the disk takes writes again", got)
	}
}

// TestScheduler_EvacuationFailure_ResumedRun_AtPreCopyArrayReady_ClearsExemption
// is the maintainer's added #378 scope's other half: the same resumed
// pre-copy ArrayReady window, but ending in an ordinary failure rather
// than a Cancel. EvacuationAbort never runs for a job that ends
// StatusFailed either, so before the fix the manifest and removing-disks
// exemption an earlier graceful stop had committed for Resume survived a
// plain failure here too — exempting the disk from the guard's
// zero-files rule on every later sync, and leaving a stale manifest entry
// that could exempt an unrelated later removal of the same path
// (matchManifest, internal/parity/guard.go).
func TestScheduler_EvacuationFailure_ResumedRun_AtPreCopyArrayReady_ClearsExemption(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	base := t.TempDir()
	diskMount := filepath.Join(base, "disk1")
	src := filepath.Join(diskMount, "media")
	dst := filepath.Join(base, "disk2", "media")
	plan := newRebalanceTestPlan(t, src, dst, "media", "movie.mkv", "movie bytes")

	eng := newRecordingEngine()
	eng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)
	eng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)
	manifestStore := &fakeRelocationManifestStore{}
	removalStore := newFakeRemovalStateStore()
	arrayReady := &countingHook{onCall: func(ctx context.Context, n int32) error {
		if n == 2 {
			return errors.New("dial tcp: connection refused")
		}
		return nil
	}}

	share := cache.Share{Name: "media", Branches: []string{src}}
	sync := func(ctx context.Context, manifest []parity.ManifestEntry, removingDisks map[string]bool) error {
		if err := s.EnterMaintenance(ctx); err != nil {
			t.Errorf("EnterMaintenance: %v", err)
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
	first := await(t, s, j.ID)
	if first.Status != StatusInterrupted {
		t.Fatalf("first run status = %s (%s), want interrupted", first.Status, first.ErrorMessage)
	}
	s.ExitMaintenance()

	if _, err := s.Resume(ctx, j.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s (%s), want failed", finished.Status, finished.ErrorMessage)
	}
	if !strings.Contains(finished.ErrorMessage, "applying no-create to the live pool") {
		t.Fatalf("ErrorMessage = %q, want it to name the failed live re-apply", finished.ErrorMessage)
	}
	_, manifest, removingDisks := manifestStore.snapshot()
	if manifest != nil || removingDisks != nil {
		t.Fatalf("FAIL-OPEN: failed with manifest/exemption still persisted: manifest=%+v removingDisks=%+v", manifest, removingDisks)
	}
	// A plain failure still keeps the disk "evacuating" (EvacuationDeps.Store's
	// own doc comment) — only the manifest and its exemption are this fix's
	// concern; the removal state is released by a later Cancel or a
	// successful re-evacuation, exactly as any other plain failure leaves it.
	if got := removalStore.get(diskMount); got != "evacuating" {
		t.Fatalf("removal_state after that failure = %q, want %q", got, "evacuating")
	}
}
