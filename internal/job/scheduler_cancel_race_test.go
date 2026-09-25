package job

import (
	"context"
	"errors"
	"fmt"
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

// TestScheduler_CancelRace_BeforeFinished_GenuineFailureIsNotErased is
// #379's own reproduction of the report: a Cancel landing after a RunFunc
// has already made its last cancellation check and committed to a genuine,
// non-cancellation failure — but before runJob has taken rj.mu to set
// rj.finished — is still accepted (rj.finished is not yet set, so
// Scheduler.Cancel's own finished check does not refuse it). Before the
// fix, runJob's own reasonCancel branch recorded this as a bare,
// errorless Cancelled job, discarding the real failure. s.beforeFinishedHook
// lands the Cancel deterministically in that exact window — a window no
// other hook in this package reaches, and the one the original report
// says nothing could reproduce — never a sleep.
func TestScheduler_CancelRace_BeforeFinished_GenuineFailureIsNotErased(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	wantErr := errors.New("disk: write failed")
	s.registry.Register(TypeSync, true, func(ctx context.Context, rc *RunContext) error {
		return wantErr
	})

	var hookCalls int32
	s.beforeFinishedHook = func(jobID string) {
		atomic.AddInt32(&hookCalls, 1)
		if _, err := s.Cancel(context.Background(), jobID); err != nil {
			t.Errorf("Cancel between the RunFunc's return and runJob setting finished: %v", err)
		}
	}

	j, err := s.Submit(ctx, TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	finished := await(t, s, j.ID)
	if atomic.LoadInt32(&hookCalls) == 0 {
		t.Fatal("beforeFinishedHook was never called — the test did not land the Cancel in the window it claims to")
	}
	if finished.Status != StatusCancelled {
		t.Fatalf("status = %s (%s), want cancelled — the cancel itself is still honoured", finished.Status, finished.ErrorMessage)
	}
	if finished.ErrorCode == "" || finished.ErrorMessage == "" {
		t.Fatalf("ErrorCode/ErrorMessage = %q/%q, want the RunFunc's real failure recorded, not erased by the raced cancel", finished.ErrorCode, finished.ErrorMessage)
	}
	if !strings.Contains(finished.ErrorMessage, wantErr.Error()) {
		t.Fatalf("ErrorMessage = %q, want it to name the actual failure %q", finished.ErrorMessage, wantErr.Error())
	}
}

// TestScheduler_CancelRace_BeforeReturn_OutcomeErrorIsNotSilenced is the
// #379 attempt-1 verifier's own second observation on the same gap: a
// RunFunc's explicit *OutcomeError already names its own status and code
// deliberately, the same way a *CancelCleanupError does, so it must never
// be silenced by runJob's own identity classification (isCancellationDerived,
// scheduler.go) either. The RunFunc here calls s.Cancel itself, directly,
// right after its own last ctx check and before it returns — landing the
// Cancel inside rj.run, narrower than s.beforeFinishedHook's window, so
// ctx.Err() already reads true by the time runJob's switch runs. Before the
// attempt-1 fix, runJob's own switch checked a ctx.Err() sample ahead of
// hasOutcome, so this reached that sample first and the OutcomeError's own
// code was dropped.
func TestScheduler_CancelRace_BeforeReturn_OutcomeErrorIsNotSilenced(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	wantErr := &OutcomeError{Status: StatusFailed, Code: "test_outcome_code", Err: errors.New("disk: write failed")}
	s.registry.Register(TypeSync, true, func(ctx context.Context, rc *RunContext) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err := s.Cancel(context.Background(), rc.JobID()); err != nil {
			t.Errorf("Cancel between the RunFunc's own last ctx check and its return: %v", err)
		}
		return wantErr
	})

	j, err := s.Submit(ctx, TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	finished := await(t, s, j.ID)
	if finished.Status != StatusCancelled {
		t.Fatalf("status = %s (%s), want cancelled — the cancel itself is still honoured", finished.Status, finished.ErrorMessage)
	}
	if finished.ErrorCode != "test_outcome_code" {
		t.Fatalf("ErrorCode = %q, want %q — an explicit *OutcomeError must never be silenced by the identity classification", finished.ErrorCode, "test_outcome_code")
	}
	if !strings.Contains(finished.ErrorMessage, wantErr.Error()) {
		t.Fatalf("ErrorMessage = %q, want it to name the actual failure %q", finished.ErrorMessage, wantErr.Error())
	}
}

// TestScheduler_CancelRace_BeforeReturn_PlainErrorIsNotErased is the
// attempt-3 verifier's own generic reproduction (a): a RunFunc that returns
// a plain, non-cancellation error after its own last ctx check, with the
// Cancel landing inside the RunFunc itself, narrower than
// s.beforeFinishedHook's window — the same shape as the sibling
// *OutcomeError test above, but for a plain error with no explicit
// marking. Before the attempt-2 fix (identity classification), this ended
// cancelled with no error recorded at all.
func TestScheduler_CancelRace_BeforeReturn_PlainErrorIsNotErased(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	wantErr := errors.New("disk: write failed")
	s.registry.Register(TypeSync, true, func(ctx context.Context, rc *RunContext) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err := s.Cancel(context.Background(), rc.JobID()); err != nil {
			t.Errorf("Cancel between the RunFunc's own last ctx check and its return: %v", err)
		}
		return wantErr
	})

	j, err := s.Submit(ctx, TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	finished := await(t, s, j.ID)
	if finished.Status != StatusCancelled {
		t.Fatalf("status = %s (%s), want cancelled — the cancel itself is still honoured", finished.Status, finished.ErrorMessage)
	}
	if finished.ErrorCode != "job_failed_before_cancel" {
		t.Fatalf("ErrorCode = %q, want %q — a genuine failure that survived its own last cancellation check must not be erased", finished.ErrorCode, "job_failed_before_cancel")
	}
	if !strings.Contains(finished.ErrorMessage, wantErr.Error()) {
		t.Fatalf("ErrorMessage = %q, want it to name the actual failure %q", finished.ErrorMessage, wantErr.Error())
	}
}

// TestScheduler_ShareRelocationCancelRace_BeforeReturn_ManifestClearFailureIsNotErased
// is the attempt-3 verifier's own reproduction (b): RunShareRelocation's
// own post-final-sync manifest clear (share_relocation_run.go) fails for a
// reason unrelated to the Cancel that lands while the fake store's own
// Replace call is blocked, the same started/release shape the sibling
// evacuation reapply/clear-failure tests above use to land a Cancel
// deterministically inside a RunFunc still in flight, before it ever
// returns. Before the attempt-2 fix, this ended cancelled with no error
// recorded and the stale manifest still persisted — the same fail-open
// AC2 forbids for evacuation, in the one other RunFunc that persists a
// relocation manifest. Closing it at runJob's own classification layer
// (this test) does not by itself fix share_relocation_run.go clearing its
// manifest on a cancellable context — that is a separate, already-filed
// defect — but it does mean a plain failure of that clear, however it is
// timed against a Cancel, is recorded rather than silently dropped.
func TestScheduler_ShareRelocationCancelRace_BeforeReturn_ManifestClearFailureIsNotErased(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	share := newShareRelocationShare(t, "docs")
	src := filepath.Join(share.Branches[0], "report.pdf")
	mustWriteFile(t, src, "report bytes")

	eng := newRecordingEngine()
	eng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)

	wantErr := errors.New("database is locked")
	started := make(chan struct{})
	release := make(chan struct{})
	manifest := &blockingClearManifestReplacer{started: started, release: release, err: wantErr}

	s.registry.Register(TypeShareRelocation, true, RunShareRelocation(ShareRelocationDeps{
		Share:    func(context.Context, string) (cache.Share, error) { return share, nil },
		Sync:     syncFuncFromEngine(eng),
		Manifest: manifest,
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

	finished := await(t, s, j.ID)
	if finished.Status != StatusCancelled {
		t.Fatalf("status = %s (%s), want cancelled — the cancel itself is still honoured", finished.Status, finished.ErrorMessage)
	}
	if finished.ErrorCode != "job_failed_before_cancel" {
		t.Fatalf("ErrorCode = %q, want %q — the failed manifest clear must not be erased by the raced cancel", finished.ErrorCode, "job_failed_before_cancel")
	}
	if !strings.Contains(finished.ErrorMessage, wantErr.Error()) {
		t.Fatalf("ErrorMessage = %q, want it to name the failed clear %q", finished.ErrorMessage, wantErr.Error())
	}
}

// blockingClearManifestReplacer's own Replace blocks on release once
// called to clear the manifest (a nil manifest and nil removingDisks, the
// exact signature every relocation-manifest clear uses), so a test can
// land a Cancel deterministically while the RunFunc is still inside this
// call, then let it fail with a reason unrelated to the cancel itself (a
// locked database, never context.Canceled) — proving the failure survives
// as genuine under the identity classification.
type blockingClearManifestReplacer struct {
	started chan struct{}
	release chan struct{}
	err     error
}

func (m *blockingClearManifestReplacer) Replace(ctx context.Context, manifest []parity.ManifestEntry, removingDisks map[string]bool) error {
	if manifest != nil || removingDisks != nil {
		return nil
	}
	close(m.started)
	<-m.release
	return m.err
}

// TestScheduler_EvacuationCancelRace_BeforeFinished_FailedManifestClearIsNotErased
// is #379's own acceptance test for the evacuation-specific scenario the
// report names: RunEvacuation's own non-resumable, post-copy inline clear
// (evacuation_run.go, d.run's own "every non-resumable ending... clears
// the whole persisted relocation state" branch) runs with no Cancel yet —
// this run finishes normally — and the clear itself fails, so d.run
// returns a *CancelCleanupError unconditionally (never gated on ctx.Err(),
// #379). RunEvacuation's own outer wrapper, seeing ctx.Err() == nil,
// returns it unchanged. Only after RunEvacuation has fully returned does a
// Cancel land, in the window
// TestScheduler_CancelRace_BeforeFinished_GenuineFailureIsNotErased
// exercises directly. Before #379's fix, this produced exactly the
// fail-open the issue reports: a Cancelled job with no recorded error, and
// the removing-disks exemption left persisted with nothing to explain why.
// TestScheduler_EvacuationCancelRace_BeforeManifestClearReturn_FailedClearIsNotErased,
// below, proves the same failure survives a Cancel landing even closer to
// d.run's own return, inside the call that already produced it.
func TestScheduler_EvacuationCancelRace_BeforeFinished_FailedManifestClearIsNotErased(t *testing.T) {
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

	share := cache.Share{Name: "media", Branches: []string{src}}
	s.registry.Register(TypeEvacuation, true, RunEvacuation(EvacuationDeps{
		Sync:             evacuationSyncFuncFromEngine(eng),
		TrackedFileCount: func(context.Context) (int, error) { return 1000, nil },
		Shares:           func(context.Context) ([]cache.Share, error) { return []cache.Share{share}, nil },
		Manifest:         manifestStore,
		Store:            removalStore,
		ArrayReady:       arrayReady.hook,
	}))

	var hookCalls int32
	s.beforeFinishedHook = func(jobID string) {
		atomic.AddInt32(&hookCalls, 1)
		if _, err := s.Cancel(context.Background(), jobID); err != nil {
			t.Errorf("Cancel between RunEvacuation's return and runJob setting finished: %v", err)
		}
	}

	j, err := s.Submit(ctx, TypeEvacuation, nil, mustJSON(t, EvacuationParams{Mountpoint: diskMount, Plan: plan}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	finished := await(t, s, j.ID)
	if atomic.LoadInt32(&hookCalls) == 0 {
		t.Fatal("beforeFinishedHook was never called — the test did not land the Cancel in the window it claims to")
	}
	if finished.Status != StatusCancelled {
		t.Fatalf("status = %s (%s), want cancelled — the cancel itself is still honoured", finished.Status, finished.ErrorMessage)
	}
	if finished.ErrorCode != "evacuation_cancel_manifest_clear_failed" {
		t.Fatalf("ErrorCode = %q, want %q — the failed manifest clear recorded, not erased by the raced cancel", finished.ErrorCode, "evacuation_cancel_manifest_clear_failed")
	}
	if !strings.Contains(finished.ErrorMessage, "clearing relocation manifest") {
		t.Fatalf("ErrorMessage = %q, want it to name the failed clear", finished.ErrorMessage)
	}

	_, manifest, removingDisks := manifestStore.snapshot()
	if len(manifest) == 0 || !removingDisks[diskMount] {
		t.Fatalf("after the raced cancel, store = (manifest=%+v, removingDisks=%+v), want the exemption still persisted — the failed clear never released it, exactly as a plain (unraced) failure of the same clear would leave it", manifest, removingDisks)
	}
}

// TestScheduler_EvacuationCancelRace_BeforeManifestClearReturn_FailedClearIsNotErased
// closes the gap the #379 attempt-1 verifier found in
// TestScheduler_EvacuationCancelRace_BeforeFinished_FailedManifestClearIsNotErased:
// that test lands its Cancel only after RunEvacuation has fully returned
// to runJob. A Cancel landing narrower still — inside d.run's own
// manifest-clear branch, after its combined error is already built but
// before that branch returns — used to reach a ctx.Err() sample no matter
// how close to the return it was taken, because Cancel's own cancellation
// of ctx completes synchronously and there is no instant before a return
// statement that a racing goroutine cannot land in first.
// EvacuationDeps.beforeManifestClearReturnHook lands the Cancel exactly
// there, deterministically. The fix does not try to sample any closer: it
// stops consulting ctx.Err() at this point at all, wrapping the failed
// clear in a *CancelCleanupError unconditionally, so this test's outcome
// no longer depends on when, relative to the return, the Cancel lands.
func TestScheduler_EvacuationCancelRace_BeforeManifestClearReturn_FailedClearIsNotErased(t *testing.T) {
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

	share := cache.Share{Name: "media", Branches: []string{src}}
	deps := EvacuationDeps{
		Sync:             evacuationSyncFuncFromEngine(eng),
		TrackedFileCount: func(context.Context) (int, error) { return 1000, nil },
		Shares:           func(context.Context) ([]cache.Share, error) { return []cache.Share{share}, nil },
		Manifest:         manifestStore,
		Store:            removalStore,
		ArrayReady:       arrayReady.hook,
	}
	var hookCalls int32
	deps.beforeManifestClearReturnHook = func(jobID string) {
		atomic.AddInt32(&hookCalls, 1)
		if _, err := s.Cancel(context.Background(), jobID); err != nil {
			t.Errorf("Cancel between the manifest clear's own failure and d.run's return: %v", err)
		}
	}
	s.registry.Register(TypeEvacuation, true, RunEvacuation(deps))

	j, err := s.Submit(ctx, TypeEvacuation, nil, mustJSON(t, EvacuationParams{Mountpoint: diskMount, Plan: plan}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	finished := await(t, s, j.ID)
	if atomic.LoadInt32(&hookCalls) == 0 {
		t.Fatal("beforeManifestClearReturnHook was never called — the test did not land the Cancel in the window it claims to")
	}
	if finished.Status != StatusCancelled {
		t.Fatalf("status = %s (%s), want cancelled — the cancel itself is still honoured", finished.Status, finished.ErrorMessage)
	}
	if finished.ErrorCode != "evacuation_cancel_manifest_clear_failed" {
		t.Fatalf("ErrorCode = %q, want %q — the failed manifest clear recorded, not erased by a cancel landing inside d.run's own manifest-clear branch", finished.ErrorCode, "evacuation_cancel_manifest_clear_failed")
	}
	if !strings.Contains(finished.ErrorMessage, "clearing relocation manifest") {
		t.Fatalf("ErrorMessage = %q, want it to name the failed clear", finished.ErrorMessage)
	}

	_, manifest, removingDisks := manifestStore.snapshot()
	if len(manifest) == 0 || !removingDisks[diskMount] {
		t.Fatalf("after the raced cancel, store = (manifest=%+v, removingDisks=%+v), want the exemption still persisted", manifest, removingDisks)
	}
}

// TestScheduler_CancelRunningJob_KilledProcessStyleErrorStaysSilentCancelled
// is the negative case #379's fix must not regress: an ordinary Cancel of a
// running job — no race, Cancel lands and its context.CancelFunc runs while
// the RunFunc is still blocked, well before it returns — whose RunFunc
// reacts with a killed process's own real, non-nil exit error. A real killed
// SnapRAID process is exactly this shape: internal/parity/snapraid_engine.go's
// own runStream now always folds ctx.Err() into that error when ctx is
// cancelled (its own doc comment on why), so the fake here models the same
// wrap rather than an unwrapped exit error — proving runJob's identity
// classification (isCancellationDerived, scheduler.go) still recognizes it
// as the RunFunc's own reaction to the cancellation it was asked to stop
// for, and this stays silently Cancelled exactly as it did before #379.
func TestScheduler_CancelRunningJob_KilledProcessStyleErrorStaysSilentCancelled(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	started := make(chan struct{})
	s.registry.Register(TypeSync, true, func(ctx context.Context, rc *RunContext) error {
		close(started)
		<-ctx.Done()
		return fmt.Errorf("%w: %v", ctx.Err(), errors.New(`parity: snapraid sync: exit "": signal: killed`))
	})

	j, err := s.Submit(ctx, TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	<-started

	if _, err := s.Cancel(ctx, j.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	finished := await(t, s, j.ID)
	if finished.Status != StatusCancelled {
		t.Fatalf("status = %s (%s), want cancelled", finished.Status, finished.ErrorMessage)
	}
	if finished.ErrorCode != "" || finished.ErrorMessage != "" {
		t.Fatalf("ErrorCode/ErrorMessage = %q/%q, want none — an ordinary cancel's own reaction from the RunFunc must stay silent even when it does not wrap context.Canceled", finished.ErrorCode, finished.ErrorMessage)
	}
}
