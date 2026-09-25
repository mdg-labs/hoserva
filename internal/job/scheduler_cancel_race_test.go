package job

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
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
