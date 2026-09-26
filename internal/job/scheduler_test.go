package job

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/disk"
	storedb "github.com/mdg-labs/hoserva/internal/store/db"
)

// blockingRun returns a RunFunc that signals started, then blocks until
// release is closed (or its context is cancelled), then returns retErr.
// It never touches time.Sleep — every ordering guarantee in this file
// comes from a channel, not a race against the clock.
func blockingRun(started chan<- struct{}, release <-chan struct{}, retErr error) RunFunc {
	return func(ctx context.Context, rc *RunContext) error {
		close(started)
		select {
		case <-release:
			return retErr
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func newTestScheduler(t *testing.T) *Scheduler {
	t.Helper()
	db := newTestDB(t)
	store := NewStore(db)
	logs := NewLogStore(t.TempDir())
	return NewScheduler(store, logs, NewHub(), NewRegistry())
}

// registerBlocking registers t on s's registry with a blockingRun, and
// returns the started/release channels the caller uses to drive it.
// Every test below uses a distinct Type per job it submits — exactly as a
// real registry is used, one binding per type for the process's lifetime
// — so this never collides with Register's own duplicate-registration
// panic.
func registerBlocking(s *Scheduler, typ Type, cancellable bool) (started chan struct{}, release chan struct{}) {
	started = make(chan struct{})
	release = make(chan struct{})
	s.registry.Register(typ, cancellable, blockingRun(started, release, nil))
	return started, release
}

// waitFor polls fn until it returns true or the timeout elapses, failing
// the test on timeout — used only to observe an asynchronous side effect
// (dispatch() running after a goroutine returns), never as a substitute
// for the channel-based synchronization the running job itself uses.
func waitFor(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !fn() {
		t.Fatal("condition not met before timeout")
	}
}

func waitSucceeded(t *testing.T, s *Scheduler, id string) {
	t.Helper()
	waitFor(t, time.Second, func() bool {
		got, err := s.store.Get(context.Background(), id)
		return err == nil && got.Status == StatusSucceeded
	})
}

// TestScheduler_RecoverFromRestartMarksActiveJobsInterrupted exercises the
// actual daemon-restart shape (doc 01 §4's first acceptance criterion):
// rows written by a previous process instance — never a live goroutine of
// this one — must come back as interrupted, and only those rows, once a
// brand new Scheduler with no in-memory state calls RecoverFromRestart.
func TestScheduler_RecoverFromRestartMarksActiveJobsInterrupted(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	store := NewStore(db)

	seed := []*Job{
		{ID: "was-running", Type: TypeSync, Class: ClassParity, Status: StatusRunning, CreatedAt: time.Now().UTC()},
		{ID: "was-queued", Type: TypeMover, Class: ClassArrayWrite, Status: StatusQueued, CreatedAt: time.Now().UTC()},
		{ID: "already-done", Type: TypeScrub, Class: ClassParity, Status: StatusSucceeded, CreatedAt: time.Now().UTC()},
	}
	for _, j := range seed {
		if err := store.Create(ctx, j); err != nil {
			t.Fatalf("seeding job %s: %v", j.ID, err)
		}
	}

	s := NewScheduler(store, NewLogStore(t.TempDir()), NewHub(), NewRegistry())
	if err := s.RecoverFromRestart(ctx); err != nil {
		t.Fatalf("RecoverFromRestart: %v", err)
	}

	for _, id := range []string{"was-running", "was-queued"} {
		got, err := store.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status != StatusInterrupted {
			t.Errorf("job %s status = %s, want interrupted", id, got.Status)
		}
	}
	done, err := store.Get(ctx, "already-done")
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != StatusSucceeded {
		t.Errorf("already-finished job's status = %s, want unchanged succeeded", done.Status)
	}

	// Doc 01 §4: never resumed automatically. A fresh Scheduler starts
	// with nothing running or queued in memory — recovery only rewrites
	// database rows, it does not resurrect any job's execution.
	active, err := store.ListActive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("ListActive after recovery = %v, want empty", active)
	}
}

func TestScheduler_SubmitFailsForUnregisteredType(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	if _, err := s.Submit(ctx, TypeSync, nil, nil); !errors.Is(err, ErrJobTypeNotRegistered) {
		t.Fatalf("Submit(unregistered type) = %v, want ErrJobTypeNotRegistered", err)
	}
}

func TestScheduler_SubmitRunsImmediatelyWhenNoConflict(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	started, release := registerBlocking(s, TypeSync, false)
	j, err := s.Submit(ctx, TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if j.Status != StatusRunning {
		t.Fatalf("Status = %s, want running", j.Status)
	}
	<-started
	close(release)
	waitSucceeded(t, s, j.ID)
}

func TestScheduler_SubmitQueuesOnParityConflictThenDispatchesWhenFreed(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	aStarted, aRelease := registerBlocking(s, TypeSync, false)
	a, err := s.Submit(ctx, TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit a: %v", err)
	}
	<-aStarted

	bStarted, bRelease := registerBlocking(s, TypeScrub, false)
	b, err := s.Submit(ctx, TypeScrub, nil, nil)
	if err != nil {
		t.Fatalf("Submit b: %v", err)
	}
	if b.Status != StatusQueued {
		t.Fatalf("second parity-class job Status = %s, want queued (doc 01 §4: Parity excludes Parity)", b.Status)
	}

	close(aRelease)
	<-bStarted // only reachable once dispatch() started b after a finished

	got, err := s.store.Get(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusRunning {
		t.Fatalf("b Status = %s, want running once a freed the parity class", got.Status)
	}
	close(bRelease)

	waitSucceeded(t, s, a.ID)
	waitSucceeded(t, s, b.ID)
}

func TestScheduler_ArrayWriteDifferentDisksRunConcurrently(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	aStarted, aRelease := registerBlocking(s, TypeMover, false)
	a, err := s.Submit(ctx, TypeMover, []string{"disk-1"}, nil)
	if err != nil {
		t.Fatalf("Submit a: %v", err)
	}
	<-aStarted

	bStarted, bRelease := registerBlocking(s, TypeVMDiskRelocation, false)
	b, err := s.Submit(ctx, TypeVMDiskRelocation, []string{"disk-2"}, nil)
	if err != nil {
		t.Fatalf("Submit b: %v", err)
	}
	if b.Status != StatusRunning {
		t.Fatalf("array_write job on a disjoint disk set Status = %s, want running immediately", b.Status)
	}
	<-bStarted
	close(aRelease)
	close(bRelease)
	waitSucceeded(t, s, a.ID)
	waitSucceeded(t, s, b.ID)
}

func TestScheduler_ArrayWriteSameDiskQueues(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	aStarted, aRelease := registerBlocking(s, TypeMover, false)
	a, err := s.Submit(ctx, TypeMover, []string{"disk-1"}, nil)
	if err != nil {
		t.Fatalf("Submit a: %v", err)
	}
	<-aStarted

	bStarted, bRelease := registerBlocking(s, TypeVMDiskRelocation, false)
	b, err := s.Submit(ctx, TypeVMDiskRelocation, []string{"disk-1"}, nil)
	if err != nil {
		t.Fatalf("Submit b: %v", err)
	}
	if b.Status != StatusQueued {
		t.Fatalf("array_write job on the same disk Status = %s, want queued", b.Status)
	}
	close(aRelease)
	<-bStarted
	close(bRelease)
	waitSucceeded(t, s, a.ID)
	waitSucceeded(t, s, b.ID)
}

func TestScheduler_ServiceDifferentContainersRunConcurrently(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	aStarted, aRelease := registerBlocking(s, TypeContainerUpdate, false)
	a, err := s.Submit(ctx, TypeContainerUpdate, []string{"jellyfin"}, nil)
	if err != nil {
		t.Fatalf("Submit a: %v", err)
	}
	<-aStarted

	bStarted, bRelease := registerBlocking(s, TypeAppdataBackup, false)
	b, err := s.Submit(ctx, TypeAppdataBackup, []string{"radarr"}, nil)
	if err != nil {
		t.Fatalf("Submit b: %v", err)
	}
	if b.Status != StatusRunning {
		t.Fatalf("service job on a different container Status = %s, want running", b.Status)
	}
	<-bStarted
	close(aRelease)
	close(bRelease)
	waitSucceeded(t, s, a.ID)
	waitSucceeded(t, s, b.ID)
}

func TestScheduler_ServiceSameContainerQueues(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	aStarted, aRelease := registerBlocking(s, TypeContainerUpdate, false)
	a, err := s.Submit(ctx, TypeContainerUpdate, []string{"jellyfin"}, nil)
	if err != nil {
		t.Fatalf("Submit a: %v", err)
	}
	<-aStarted

	bStarted, bRelease := registerBlocking(s, TypeAppdataBackup, false)
	b, err := s.Submit(ctx, TypeAppdataBackup, []string{"jellyfin"}, nil)
	if err != nil {
		t.Fatalf("Submit b: %v", err)
	}
	if b.Status != StatusQueued {
		t.Fatalf("service job on the same container Status = %s, want queued", b.Status)
	}

	close(aRelease)
	<-bStarted
	close(bRelease)
	waitSucceeded(t, s, a.ID)
	waitSucceeded(t, s, b.ID)
}

func TestScheduler_TopologyExcludesEveryStorageClassButNotServiceOrVM(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	topStarted, topRelease := registerBlocking(s, TypeDiskAdd, false)
	// registerBlocking's own fake RunFunc never decodes params — this is
	// only a Topology-class placeholder for the class-exclusion behaviour
	// below — but Submit still runs ValidateParams first, so disk_add's
	// own confirmation/device requirement (#288) needs a well-formed
	// payload here, not nil.
	topologyParams := mustJSON(t, DiskAddParams{Confirmation: "ERASE /dev/sdx", Disk: disk.AssignedDisk{Device: "/dev/sdx"}})
	_, err := s.Submit(ctx, TypeDiskAdd, nil, topologyParams)
	if err != nil {
		t.Fatalf("Submit topology job: %v", err)
	}
	<-topStarted

	syncStarted, syncRelease := registerBlocking(s, TypeSync, false)
	sync, err := s.Submit(ctx, TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit sync: %v", err)
	}
	if sync.Status != StatusQueued {
		t.Fatalf("parity job during a topology job Status = %s, want queued", sync.Status)
	}

	vmStarted, vmRelease := registerBlocking(s, TypeVMStart, false)
	vmJob, err := s.Submit(ctx, TypeVMStart, []string{"vm-1"}, nil)
	if err != nil {
		t.Fatalf("Submit vm job: %v", err)
	}
	if vmJob.Status != StatusRunning {
		t.Fatalf("vm job during a topology job Status = %s, want running (Topology only excludes the storage classes)", vmJob.Status)
	}
	<-vmStarted
	close(vmRelease)
	waitSucceeded(t, s, vmJob.ID)

	close(topRelease)
	<-syncStarted
	close(syncRelease)
	waitSucceeded(t, s, sync.ID)
}

func TestScheduler_CancelRunningCancellableJob(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	started := make(chan struct{})
	s.registry.Register(TypeSync, true, func(ctx context.Context, rc *RunContext) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	j, err := s.Submit(ctx, TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	<-started

	if _, err := s.Cancel(ctx, j.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	waitFor(t, time.Second, func() bool {
		got, err := s.store.Get(ctx, j.ID)
		return err == nil && got.Status == StatusCancelled
	})
}

func TestScheduler_CancelRunningNonCancellableJobRefuses(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	started, release := registerBlocking(s, TypeSync, false)
	j, err := s.Submit(ctx, TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	<-started

	_, err = s.Cancel(ctx, j.ID)
	if !errors.Is(err, ErrJobNotCancellable) {
		t.Fatalf("Cancel(non-cancellable job) = %v, want ErrJobNotCancellable", err)
	}

	got, err := s.store.Get(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusRunning {
		t.Fatalf("job status after a refused cancel = %s, want unchanged running", got.Status)
	}

	close(release)
	waitSucceeded(t, s, j.ID)
}

func TestScheduler_CancelQueuedJobRemovesItRegardlessOfCancellable(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	aStarted, aRelease := registerBlocking(s, TypeSync, false)
	a, err := s.Submit(ctx, TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit a: %v", err)
	}
	<-aStarted

	s.registry.Register(TypeScrub, false, blockingRun(make(chan struct{}), make(chan struct{}), nil))
	b, err := s.Submit(ctx, TypeScrub, nil, nil)
	if err != nil {
		t.Fatalf("Submit b: %v", err)
	}
	if b.Status != StatusQueued {
		t.Fatalf("b.Status = %s, want queued", b.Status)
	}

	cancelled, err := s.Cancel(ctx, b.ID)
	if err != nil {
		t.Fatalf("Cancel(queued job): %v", err)
	}
	if cancelled.Status != StatusCancelled {
		t.Fatalf("Cancel(queued job) returned status %s, want cancelled", cancelled.Status)
	}

	close(aRelease)
	waitSucceeded(t, s, a.ID)
}

func TestScheduler_CancelJobNotRunningReturnsError(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	started, release := registerBlocking(s, TypeSync, false)
	j, err := s.Submit(ctx, TypeSync, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	close(release)
	waitSucceeded(t, s, j.ID)

	if _, err := s.Cancel(ctx, j.ID); !errors.Is(err, ErrJobNotRunning) {
		t.Fatalf("Cancel(already-finished job) = %v, want ErrJobNotRunning", err)
	}
}

// TestScheduler_CancelEndsAnInterruptedCancellableJobCancelled: Cancel
// accepts an interrupted, cancellable job and ends it cancelled — for a
// type with no AbortFunc, a plain status change.
func TestScheduler_CancelEndsAnInterruptedCancellableJobCancelled(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	stopSeen := make(chan struct{})
	s.registry.Register(TypeMover, true, func(ctx context.Context, rc *RunContext) error {
		<-rc.StopRequested()
		close(stopSeen)
		return nil
	})
	j, err := s.Submit(ctx, TypeMover, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := s.EnterMaintenance(ctx); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}
	<-stopSeen
	waitFor(t, time.Second, func() bool {
		got, err := s.store.Get(ctx, j.ID)
		return err == nil && got.Status == StatusInterrupted
	})

	cancelled, err := s.Cancel(ctx, j.ID)
	if err != nil {
		t.Fatalf("Cancel(interrupted job): %v", err)
	}
	if cancelled.Status != StatusCancelled {
		t.Fatalf("Cancel(interrupted job) returned status %s, want cancelled", cancelled.Status)
	}
	got, err := s.store.Get(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusCancelled {
		t.Fatalf("job status after cancelling an interrupted job = %s, want cancelled", got.Status)
	}
}

// TestScheduler_CancelRefusesAnInterruptedNonCancellableJob: the
// interrupted-job cancel path honours the job's cancellable value.
func TestScheduler_CancelRefusesAnInterruptedNonCancellableJob(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	stopSeen := make(chan struct{})
	s.registry.Register(TypeMover, false, func(ctx context.Context, rc *RunContext) error {
		<-rc.StopRequested()
		close(stopSeen)
		return nil
	})
	j, err := s.Submit(ctx, TypeMover, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := s.EnterMaintenance(ctx); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}
	<-stopSeen
	waitFor(t, time.Second, func() bool {
		got, err := s.store.Get(ctx, j.ID)
		return err == nil && got.Status == StatusInterrupted
	})

	if _, err := s.Cancel(ctx, j.ID); !errors.Is(err, ErrJobNotCancellable) {
		t.Fatalf("Cancel(interrupted, non-cancellable job) = %v, want ErrJobNotCancellable", err)
	}
}

func TestScheduler_ResumeFailsForUnregisteredType(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	store := NewStore(db)
	s := NewScheduler(store, NewLogStore(t.TempDir()), NewHub(), NewRegistry())

	interrupted := &Job{ID: "j1", Type: TypeMover, Class: ClassArrayWrite, Status: StatusInterrupted, Resumable: true, CreatedAt: time.Now().UTC()}
	if err := store.Create(ctx, interrupted); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Resume(ctx, "j1"); !errors.Is(err, ErrJobTypeNotRegistered) {
		t.Fatalf("Resume(unregistered type) = %v, want ErrJobTypeNotRegistered", err)
	}
}

func TestScheduler_ResumeRefusesNonResumableType(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	started, release := registerBlocking(s, TypeSync, false)
	j, err := s.Submit(ctx, TypeSync, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	<-started

	// Resumable is a property of the job type (Q29), not of its current
	// status — a sync job refuses Resume for this reason alone, whether
	// it's running, interrupted, or anything else.
	if _, err := s.Resume(ctx, j.ID); !errors.Is(err, ErrJobNotResumable) {
		t.Fatalf("Resume(sync job) = %v, want ErrJobNotResumable (Q29)", err)
	}

	close(release)
	waitSucceeded(t, s, j.ID)
}

func TestScheduler_ResumeRefusesNonInterruptedJob(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	started, release := registerBlocking(s, TypeMover, false)
	j, err := s.Submit(ctx, TypeMover, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	<-started

	if _, err := s.Resume(ctx, j.ID); !errors.Is(err, ErrJobNotInterrupted) {
		t.Fatalf("Resume(running job) = %v, want ErrJobNotInterrupted", err)
	}

	close(release)
	waitSucceeded(t, s, j.ID)
}

func TestScheduler_ResumeContinuesFromCheckpoint(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	firstRunSawCheckpoint := make(chan []byte, 1)
	secondRunSawCheckpoint := make(chan []byte, 1)
	block := make(chan struct{})
	callCount := 0
	s.registry.Register(TypeMover, false, func(ctx context.Context, rc *RunContext) error {
		callCount++
		if callCount == 1 {
			firstRunSawCheckpoint <- rc.InitialCheckpoint()
			if err := rc.SaveCheckpoint([]byte("offset=42")); err != nil {
				return err
			}
			<-block
			return nil
		}
		secondRunSawCheckpoint <- rc.InitialCheckpoint()
		return nil
	})

	j, err := s.Submit(ctx, TypeMover, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if got := <-firstRunSawCheckpoint; got != nil {
		t.Fatalf("first run's InitialCheckpoint = %q, want nil", got)
	}

	waitFor(t, time.Second, func() bool {
		got, err := s.store.Get(ctx, j.ID)
		return err == nil && got.Checkpoint != nil
	})

	// A restart: RecoverFromRestart marks the still-running job
	// interrupted, and its own goroutine is still blocked (simulating a
	// process exit is out of scope for an in-process test; what matters
	// here is that the checkpoint already on disk is what Resume hands
	// back, unconditionally on the DB row, not on this goroutine's state).
	close(block)
	waitSucceeded(t, s, j.ID)
	if err := s.store.UpdateStatus(ctx, j.ID, StatusInterrupted, nil, "", "", nil, timePtr(time.Now().UTC())); err != nil {
		t.Fatalf("forcing job back to interrupted for the test: %v", err)
	}

	resumed, err := s.Resume(ctx, j.ID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.Status != StatusRunning {
		t.Fatalf("Resume returned status %s, want running", resumed.Status)
	}
	savedCheckpoint := <-secondRunSawCheckpoint
	if string(savedCheckpoint) != "offset=42" {
		t.Fatalf("resumed run's InitialCheckpoint = %q, want %q", savedCheckpoint, "offset=42")
	}

	waitSucceeded(t, s, j.ID)
}

func timePtr(t time.Time) *time.Time { return &t }

// schedulerOnSameDB returns a second, independent *Scheduler backed by the
// same *sql.DB s already uses — never a second connection or file, just a
// second in-memory Scheduler struct with none of s's own fields carried
// over — so a test can prove a piece of state actually round-trips
// through SQLite (#387, D16) rather than merely surviving in memory.
func schedulerOnSameDB(t *testing.T, s *Scheduler) *Scheduler {
	t.Helper()
	return NewScheduler(s.store, NewLogStore(t.TempDir()), NewHub(), NewRegistry())
}

// TestScheduler_RestorePersistedMaintenance_NoRowIsNormalOperation proves
// a fresh install, or one that has never run `array stop`, restores to
// ordinary operation rather than failing or wrongly entering maintenance.
func TestScheduler_RestorePersistedMaintenance_NoRowIsNormalOperation(t *testing.T) {
	s := newTestScheduler(t)
	if err := s.RestorePersistedMaintenance(context.Background()); err != nil {
		t.Fatalf("RestorePersistedMaintenance: %v", err)
	}
	if s.InMaintenance() {
		t.Fatal("InMaintenance() = true with no persisted row")
	}
}

// TestScheduler_RestorePersistedMaintenance_SurvivesAcrossASchedulerRestart
// is #387's own central regression at the Scheduler level: EnterMaintenance
// and MarkArrayStopped persist to SQLite, and a brand new Scheduler reading
// the same database — standing in for a hoservad restart — restores both,
// without ever calling EnterMaintenance itself. The data-disk-upgrade
// admission check (UR3, doc 02 §4) is what actually reads arrayStopped;
// admitDiskUpgradeDataLocked is called directly, under s2.mu exactly as
// its own doc comment requires, because building a full TypeDiskUpgradeData
// Submit here would need a registered job type and JSON params this test
// has no need for.
func TestScheduler_RestorePersistedMaintenance_SurvivesAcrossASchedulerRestart(t *testing.T) {
	ctx := context.Background()
	s1 := newTestScheduler(t)
	if err := s1.EnterMaintenance(ctx); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}
	s1.MarkArrayStopped()

	s2 := schedulerOnSameDB(t, s1)
	if err := s2.RestorePersistedMaintenance(ctx); err != nil {
		t.Fatalf("RestorePersistedMaintenance: %v", err)
	}
	if !s2.InMaintenance() {
		t.Fatal("InMaintenance() = false on a fresh Scheduler after EnterMaintenance+MarkArrayStopped were persisted — `array stop` did not survive the restart (#387)")
	}

	s2.mu.Lock()
	err := s2.admitDiskUpgradeDataLocked(ctx)
	s2.mu.Unlock()
	if err != nil {
		t.Fatalf("admitDiskUpgradeDataLocked on the restarted Scheduler = %v, want nil — arrayStopped must have been restored too", err)
	}
}

// TestScheduler_RestorePersistedMaintenance_ExitedMaintenanceStaysExited
// proves ExitMaintenance's own persisted write is what a restart actually
// sees — not the stale "stopped" row EnterMaintenance/MarkArrayStopped
// left, which would otherwise come back on a plain `array start` too.
func TestScheduler_RestorePersistedMaintenance_ExitedMaintenanceStaysExited(t *testing.T) {
	ctx := context.Background()
	s1 := newTestScheduler(t)
	if err := s1.EnterMaintenance(ctx); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}
	s1.MarkArrayStopped()
	s1.ExitMaintenance()

	s2 := schedulerOnSameDB(t, s1)
	if err := s2.RestorePersistedMaintenance(ctx); err != nil {
		t.Fatalf("RestorePersistedMaintenance: %v", err)
	}
	if s2.InMaintenance() {
		t.Fatal("InMaintenance() = true on a fresh Scheduler after ExitMaintenance was persisted")
	}
}

// TestScheduler_BeginArrayStart_ClearedArrayStoppedSurvivesARestart proves
// BeginArrayStart's own persisted write, not just its in-memory one: a
// crash between BeginArrayStart and a successful Start must never restore
// a "stop sequence completed" state a since-begun start has already
// invalidated, or a restarted daemon could admit a data-disk upgrade
// against an array whose start never actually finished.
func TestScheduler_BeginArrayStart_ClearedArrayStoppedSurvivesARestart(t *testing.T) {
	ctx := context.Background()
	s1 := newTestScheduler(t)
	if err := s1.EnterMaintenance(ctx); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}
	s1.MarkArrayStopped()
	if _, err := s1.BeginArrayStart(ctx); err != nil {
		t.Fatalf("BeginArrayStart: %v", err)
	}

	s2 := schedulerOnSameDB(t, s1)
	if err := s2.RestorePersistedMaintenance(ctx); err != nil {
		t.Fatalf("RestorePersistedMaintenance: %v", err)
	}
	if !s2.InMaintenance() {
		t.Fatal("InMaintenance() = false on the restarted Scheduler — BeginArrayStart must not touch maintenance mode itself")
	}
	s2.mu.Lock()
	err := s2.admitDiskUpgradeDataLocked(ctx)
	s2.mu.Unlock()
	if !errors.Is(err, ErrArrayNotStopped) {
		t.Fatalf("admitDiskUpgradeDataLocked on the restarted Scheduler = %v, want ErrArrayNotStopped — a since-begun start must not restore as a completed stop", err)
	}
}

// failingExitMaintenanceDBTX wraps a real storedb.DBTX and fails only the
// exact array_maintenance write ExitMaintenance makes (maintenance=0,
// array_stopped=0) while shouldFail is true — every other statement,
// including EnterMaintenance's and MarkArrayStopped's and BeginArrayStart's
// own array_maintenance writes (which always carry a 1 in one of the
// first two args once the array is actually stopped), passes straight
// through, so this can inject a failure at exactly one call in an
// otherwise-real stop/start sequence.
type failingExitMaintenanceDBTX struct {
	storedb.DBTX
	shouldFail func() bool
}

func (d failingExitMaintenanceDBTX) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if strings.Contains(query, "array_maintenance") && len(args) >= 2 {
		maintenance, _ := args[0].(int64)
		arrayStopped, _ := args[1].(int64)
		if maintenance == 0 && arrayStopped == 0 && d.shouldFail() {
			return nil, errors.New("simulated: array_maintenance write failure")
		}
	}
	return d.DBTX.ExecContext(ctx, query, args...)
}

// TestArraySequence_Start_FailedExitMaintenanceWriteFailsStartAndKeepsStateConsistent
// is #387 finding 3's own regression: a failed ExitMaintenance persist
// write must fail ArraySequence.Start itself — every mount and every
// service has already succeeded by the time Start reaches it, so
// reporting success while SQLite still holds the previous, stopped state
// would let a restart before the user retries put the daemon straight
// back into maintenance mode over an array that is actually live, with
// GetStatus reporting it stopped while it is not. In-memory state must
// stay exactly as consistent with the persisted row as it was before the
// failed call — never optimistically cleared — and a successful retry
// once the write stops failing must clear both.
func TestArraySequence_Start_FailedExitMaintenanceWriteFailsStartAndKeepsStateConsistent(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	failWrite := false
	wrapped := failingExitMaintenanceDBTX{DBTX: db, shouldFail: func() bool { return failWrite }}
	s := NewScheduler(NewStore(wrapped), NewLogStore(t.TempDir()), NewHub(), NewRegistry())

	if err := s.EnterMaintenance(ctx); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}
	s.MarkArrayStopped()

	seq := ArraySequence{Scheduler: s}

	failWrite = true
	if err := seq.Start(ctx); err == nil {
		t.Fatal("Start with a failing ExitMaintenance persist write = nil error, want one")
	}
	if !s.InMaintenance() {
		t.Fatal("InMaintenance() = false after a failed ExitMaintenance persist write — in-memory state must not disagree with the still-persisted stopped row")
	}

	s2 := schedulerOnSameDB(t, s)
	if err := s2.RestorePersistedMaintenance(ctx); err != nil {
		t.Fatalf("RestorePersistedMaintenance: %v", err)
	}
	if !s2.InMaintenance() {
		t.Fatal("InMaintenance() = false on a restarted Scheduler after array start's own exit-maintenance write failed (#387 finding 3) — persisted and in-memory state must never disagree")
	}

	failWrite = false
	if err := seq.Start(ctx); err != nil {
		t.Fatalf("Start (retry once the write no longer fails): %v", err)
	}
	if s.InMaintenance() {
		t.Fatal("InMaintenance() = true after a successful retry")
	}
}

func TestScheduler_MaintenanceModeRefusesNewJobs(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	s.registry.Register(TypeSync, false, blockingRun(make(chan struct{}), make(chan struct{}), nil))

	if err := s.EnterMaintenance(ctx); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}
	if !s.InMaintenance() {
		t.Fatal("InMaintenance() = false after EnterMaintenance")
	}

	_, err := s.Submit(ctx, TypeSync, nil, nil)
	if !errors.Is(err, ErrMaintenanceMode) {
		t.Fatalf("Submit during maintenance = %v, want ErrMaintenanceMode (Q70)", err)
	}
}

func TestScheduler_MaintenanceModeInterruptsQueuedJobs(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	aStarted, aRelease := registerBlocking(s, TypeSync, false)
	a, err := s.Submit(ctx, TypeSync, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	<-aStarted

	s.registry.Register(TypeScrub, false, blockingRun(make(chan struct{}), make(chan struct{}), nil))
	b, err := s.Submit(ctx, TypeScrub, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != StatusQueued {
		t.Fatalf("b.Status = %s, want queued", b.Status)
	}

	if err := s.EnterMaintenance(ctx); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}

	got, err := s.store.Get(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusInterrupted {
		t.Fatalf("queued job's status after EnterMaintenance = %s, want interrupted (Q70)", got.Status)
	}

	// a is non-resumable and was running when maintenance started, so
	// EnterMaintenance force-stops it too (Q70: "the rest are marked
	// interrupted") — it never reaches aRelease at all.
	close(aRelease)
	waitFor(t, time.Second, func() bool {
		got, err := s.store.Get(ctx, a.ID)
		return err == nil && got.Status == StatusInterrupted
	})
}

func TestScheduler_MaintenanceModeStopsResumableJobAtCheckpoint(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	stopSeen := make(chan struct{})
	returned := make(chan struct{})
	s.registry.Register(TypeMover, false, func(ctx context.Context, rc *RunContext) error {
		<-rc.StopRequested()
		close(stopSeen)
		if err := rc.SaveCheckpoint([]byte("checkpoint-at-stop")); err != nil {
			return err
		}
		close(returned)
		return nil
	})
	_, err := s.Submit(ctx, TypeMover, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if err := s.EnterMaintenance(ctx); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}
	<-stopSeen
	<-returned

	// runJob still has to persist the final status after Run returns —
	// wait for that write to land instead of racing it.
	waitFor(t, time.Second, func() bool {
		active, err := s.store.ListActive(ctx)
		return err == nil && len(active) == 0
	})
}

func TestScheduler_MaintenanceModeCancelsNonResumableRunningJob(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	started := make(chan struct{})
	s.registry.Register(TypeSync, false, func(ctx context.Context, rc *RunContext) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	j, err := s.Submit(ctx, TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	<-started

	if err := s.EnterMaintenance(ctx); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}

	waitFor(t, time.Second, func() bool {
		got, err := s.store.Get(ctx, j.ID)
		return err == nil && got.Status == StatusInterrupted
	})
}

// failWritesDB wraps a real *sql.DB and, once armed, fails every
// ExecContext call while leaving reads untouched — the shape a real
// write-path failure takes (a lock timeout, a full disk), unlike a closed
// connection, which would also break the reads this test needs to keep
// working.
type failWritesDB struct {
	*sql.DB
	fail atomic.Bool
}

func (f *failWritesDB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if f.fail.Load() {
		return nil, errors.New("simulated write failure")
	}
	return f.DB.ExecContext(ctx, query, args...)
}

// TestScheduler_Await_ReturnsPromptlyDespiteAFailedFinalStatusWrite is
// this issue's own hang: runJob sets the job's terminal Status in memory
// and publishes it through Hub even when the matching Store.UpdateStatus
// call itself fails (runJob only logs that failure) — so a persistence
// hiccup right at completion must not leave Await polling a store row
// that will never turn terminal, all the way until its own context ends.
func TestScheduler_Await_ReturnsPromptlyDespiteAFailedFinalStatusWrite(t *testing.T) {
	t.Run("subscribedBeforeRelease", func(t *testing.T) {
		ctx := context.Background()
		db := newTestDB(t)
		wrapped := &failWritesDB{DB: db}
		st := NewStore(wrapped)
		s := NewScheduler(st, NewLogStore(t.TempDir()), NewHub(), NewRegistry())

		started, release := registerBlocking(s, TypeSync, false)
		j, err := s.Submit(ctx, TypeSync, nil, nil)
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		<-started

		wrapped.fail.Store(true)

		awaitCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		type awaitResult struct {
			job     *Job
			err     error
			elapsed time.Duration
		}
		resultCh := make(chan awaitResult, 1)
		go func() {
			start := time.Now()
			finished, err := s.Await(awaitCtx, j.ID)
			resultCh <- awaitResult{finished, err, time.Since(start)}
		}()

		waitFor(t, time.Second, func() bool {
			s.hub.mu.Lock()
			defer s.hub.mu.Unlock()
			return len(s.hub.subs) > 0
		})

		close(release)

		result := <-resultCh
		if result.err != nil {
			t.Fatalf("Await: %v (took %v — did it fall back to polling until awaitCtx expired?)", result.err, result.elapsed)
		}
		if result.job.Status != StatusSucceeded {
			t.Fatalf("Await: Status = %s, want succeeded", result.job.Status)
		}
		if result.elapsed >= time.Second {
			t.Fatalf("Await took %v to return — it fell back to polling the (permanently non-terminal) store row instead of trusting the published terminal snapshot", result.elapsed)
		}
	})

	t.Run("subscribedAfterPublish", func(t *testing.T) {
		ctx := context.Background()
		db := newTestDB(t)
		wrapped := &failWritesDB{DB: db}
		st := NewStore(wrapped)
		s := NewScheduler(st, NewLogStore(t.TempDir()), NewHub(), NewRegistry())

		started, release := registerBlocking(s, TypeScrub, false)
		j, err := s.Submit(ctx, TypeScrub, nil, nil)
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		<-started

		wrapped.fail.Store(true)
		close(release)

		waitFor(t, time.Second, func() bool {
			_, ok := s.terminalSnapshot(j.ID)
			return ok
		})

		awaitCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		start := time.Now()
		finished, err := s.Await(awaitCtx, j.ID)
		elapsed := time.Since(start)

		if err != nil {
			t.Fatalf("Await: %v (took %v — did it fall back to polling until awaitCtx expired?)", err, elapsed)
		}
		if finished.Status != StatusSucceeded {
			t.Fatalf("Await: Status = %s, want succeeded", finished.Status)
		}
		if elapsed >= time.Second {
			t.Fatalf("Await took %v to return — it missed the Hub publish and did not consult the in-memory terminal snapshot", elapsed)
		}
	})
}

func TestScheduler_terminalSnapshotOmitsCheckpointAndParams(t *testing.T) {
	s := newTestScheduler(t)
	checkpoint := []byte("checkpoint payload")
	params := []byte("params payload")
	now := time.Now().UTC()
	s.rememberTerminalSnapshot(Job{
		ID:         "j1",
		Status:     StatusSucceeded,
		Checkpoint: checkpoint,
		Params:     params,
		FinishedAt: &now,
	})

	snap, ok := s.terminalSnapshot("j1")
	if !ok {
		t.Fatal("terminalSnapshot: not found")
	}
	if snap.Checkpoint != nil {
		t.Fatalf("terminal snapshot Checkpoint = %q, want nil", snap.Checkpoint)
	}
	if snap.Params != nil {
		t.Fatalf("terminal snapshot Params = %q, want nil", snap.Params)
	}
	if snap.ID != "j1" || snap.Status != StatusSucceeded {
		t.Fatalf("terminal snapshot = %+v, want id and terminal status only", snap)
	}
}

func TestScheduler_Await_ReturnsErrorWhenTerminalSnapshotEvicted(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	wrapped := &failWritesDB{DB: db}
	st := NewStore(wrapped)
	s := NewScheduler(st, NewLogStore(t.TempDir()), NewHub(), NewRegistry())

	for i := 0; i < maxTerminalSnapshots; i++ {
		now := time.Now().UTC()
		s.rememberTerminalSnapshot(Job{
			ID:         fmt.Sprintf("filler-%d", i),
			Status:     StatusSucceeded,
			FinishedAt: &now,
		})
	}

	evictedID := "filler-0"
	overflowID := "overflow"
	now := time.Now().UTC()
	s.rememberTerminalSnapshot(Job{
		ID:         overflowID,
		Status:     StatusSucceeded,
		FinishedAt: &now,
	})

	if _, ok := s.terminalSnapshot(evictedID); ok {
		t.Fatalf("evicted id %s still has a terminal snapshot", evictedID)
	}

	if err := st.Create(ctx, &Job{ID: evictedID, Type: TypeSync, Class: ClassParity, Status: StatusRunning, CreatedAt: now}); err != nil {
		t.Fatalf("Create(evicted job row): %v", err)
	}

	_, err := s.Await(ctx, evictedID)
	if !errors.Is(err, ErrTerminalSnapshotEvicted) {
		t.Fatalf("Await(evicted id) = %v, want ErrTerminalSnapshotEvicted", err)
	}
}

func TestScheduler_EvictedTerminalSnapshotMarkersAreBounded(t *testing.T) {
	s := NewScheduler(NewStore(newTestDB(t)), NewLogStore(t.TempDir()), NewHub(), NewRegistry())
	now := time.Now().UTC()
	for i := 0; i < maxTerminalSnapshots*2+1; i++ {
		s.rememberTerminalSnapshot(Job{
			ID:         fmt.Sprintf("bound-%d", i),
			Status:     StatusSucceeded,
			FinishedAt: &now,
		})
	}

	s.mu.Lock()
	n := len(s.evictedTerminalSnapshots)
	fifo := len(s.evictedTerminalSnapshotFIFO)
	s.mu.Unlock()
	if n > maxTerminalSnapshots {
		t.Fatalf("evicted markers = %d, want <= %d", n, maxTerminalSnapshots)
	}
	if fifo != n {
		t.Fatalf("evicted FIFO = %d, map = %d", fifo, n)
	}

	recentEvicted := fmt.Sprintf("bound-%d", maxTerminalSnapshots)
	if !s.terminalSnapshotEvicted(recentEvicted) {
		t.Fatalf("recently evicted %s has no marker — Await would hang", recentEvicted)
	}
	oldest := "bound-0"
	if s.terminalSnapshotEvicted(oldest) {
		t.Fatalf("oldest eviction marker for %s was not dropped", oldest)
	}
}

func TestBlockingStorageJob_NamesRunningParityJob(t *testing.T) {
	s := newTestScheduler(t)
	started, release := registerBlocking(s, TypeSync, false)

	j, err := s.Submit(context.Background(), TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	// Wait for the job's goroutine to finish recording its final status
	// before t.Cleanup closes the test DB (newTestDB's own t.Cleanup,
	// registered earlier and so run after this one).
	t.Cleanup(func() {
		close(release)
		await(t, s, j.ID)
	})
	<-started

	got := s.BlockingStorageJob()
	if got == nil {
		t.Fatal("BlockingStorageJob = nil while a sync is running")
	}
	if got.ID != j.ID || got.Class != ClassParity {
		t.Fatalf("BlockingStorageJob = %+v, want id %s class %s", got, j.ID, ClassParity)
	}
}

func TestBlockingStorageJob_IgnoresServiceJobs(t *testing.T) {
	s := newTestScheduler(t)
	started, release := registerBlocking(s, TypeAppdataBackup, false)

	j, err := s.Submit(context.Background(), TypeAppdataBackup, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	// Wait for the job's goroutine to finish recording its final status
	// before t.Cleanup closes the test DB (newTestDB's own t.Cleanup,
	// registered earlier and so run after this one).
	t.Cleanup(func() {
		close(release)
		await(t, s, j.ID)
	})
	<-started

	if got := s.BlockingStorageJob(); got != nil {
		t.Fatalf("BlockingStorageJob = %+v, want nil for a service job", got)
	}
}

func TestWaitForStorageJobs_WaitsThenReturns(t *testing.T) {
	s := newTestScheduler(t)
	started, release := registerBlocking(s, TypeSync, false)

	j, err := s.Submit(context.Background(), TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	waitErr := s.WaitForStorageJobs(ctx)
	cancel()
	if waitErr == nil {
		t.Fatal("WaitForStorageJobs returned while a sync was still running")
	}

	close(release)
	waitSucceeded(t, s, j.ID)
	if err := s.WaitForStorageJobs(context.Background()); err != nil {
		t.Fatalf("WaitForStorageJobs after sync finished: %v", err)
	}
}

// TestScheduler_PauseForBattery_RefusesMoverAndSync proves Q77's
// on-battery hold: once active, Submit refuses both a mover and a sync
// job (ErrOnBattery), but leaves every other type alone.
func TestScheduler_PauseForBattery_RefusesMoverAndSync(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	s.registry.Register(TypeMover, false, blockingRun(make(chan struct{}), make(chan struct{}), nil))
	s.registry.Register(TypeSync, false, blockingRun(make(chan struct{}), make(chan struct{}), nil))
	scrubStarted, scrubRelease := registerBlocking(s, TypeScrub, false)

	if paused := s.PauseForBattery(ctx); len(paused) != 0 {
		t.Fatalf("PauseForBattery with nothing running = %v, want none paused", paused)
	}
	if !s.OnBattery() {
		t.Fatal("OnBattery() = false after PauseForBattery")
	}

	if _, err := s.Submit(ctx, TypeMover, nil, nil); !errors.Is(err, ErrOnBattery) {
		t.Fatalf("Submit(TypeMover) while on battery = %v, want ErrOnBattery", err)
	}
	if _, err := s.Submit(ctx, TypeSync, nil, nil); !errors.Is(err, ErrOnBattery) {
		t.Fatalf("Submit(TypeSync) while on battery = %v, want ErrOnBattery", err)
	}
	scrubJob, err := s.Submit(ctx, TypeScrub, nil, nil)
	if err != nil {
		t.Fatalf("Submit(TypeScrub) while on battery = %v, want nil — only the mover and sync are held", err)
	}
	// Wait for the scrub job's goroutine to finish recording its final
	// status before t.Cleanup closes the test DB (newTestDB's own
	// t.Cleanup, registered earlier and so run after this one).
	t.Cleanup(func() {
		close(scrubRelease)
		await(t, s, scrubJob.ID)
	})
	<-scrubStarted
}

// TestScheduler_PauseForBattery_StopsRunningMoverAtCheckpoint proves the
// resumable-stop mechanism EnterMaintenance already uses for maintenance
// mode also drives Q77's on-battery pause: a running mover job is asked
// to stop, saves its checkpoint, and ends interrupted — never cancelled
// outright the way a non-resumable job would be. It also proves
// PauseForBattery itself waits for that to finish: by the time it
// returns, the store already reports StatusInterrupted, with no
// waitFor needed — the same guarantee that closes the ONBATT-then-
// immediate-ONLINE race TestUPSController_OnLine_
// ImmediatelyAfterOnBattery_StillResumes exercises through
// UPSController.
func TestScheduler_PauseForBattery_StopsRunningMoverAtCheckpoint(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	stopSeen := make(chan struct{})
	returned := make(chan struct{})
	s.registry.Register(TypeMover, false, func(ctx context.Context, rc *RunContext) error {
		<-rc.StopRequested()
		close(stopSeen)
		if err := rc.SaveCheckpoint([]byte("checkpoint-at-battery-pause")); err != nil {
			return err
		}
		close(returned)
		return nil
	})
	j, err := s.Submit(ctx, TypeMover, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	paused := s.PauseForBattery(ctx)
	if len(paused) != 1 || paused[0] != j.ID {
		t.Fatalf("PauseForBattery = %v, want [%s]", paused, j.ID)
	}
	<-stopSeen
	<-returned

	got, err := s.store.Get(ctx, j.ID)
	if err != nil || got.Status != StatusInterrupted {
		t.Fatalf("job status right after PauseForBattery returned = %v (err %v), want StatusInterrupted", got, err)
	}
}

// TestScheduler_Resume_RefusesBatteryHeldTypeWhileOnBattery proves
// Resume applies the same on-battery hold Submit and dispatch already
// do: an interrupted mover job — interrupted by maintenance mode here,
// deliberately not by PauseForBattery itself, to isolate this check from
// PauseForBattery's own resume path — must not be resumable while
// batteryHold is set, the same way a direct Resume call outside
// UPSController's own round trip could otherwise start it running on a
// UPS with limited runtime left.
func TestScheduler_Resume_RefusesBatteryHeldTypeWhileOnBattery(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	stopSeen := make(chan struct{})
	s.registry.Register(TypeMover, false, func(ctx context.Context, rc *RunContext) error {
		<-rc.StopRequested()
		close(stopSeen)
		return rc.SaveCheckpoint([]byte("checkpoint"))
	})
	j, err := s.Submit(ctx, TypeMover, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if err := s.EnterMaintenance(ctx); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}
	<-stopSeen
	waitFor(t, time.Second, func() bool {
		got, err := s.store.Get(ctx, j.ID)
		return err == nil && got.Status == StatusInterrupted
	})
	s.ExitMaintenance()

	s.PauseForBattery(ctx)

	if _, err := s.Resume(ctx, j.ID); !errors.Is(err, ErrOnBattery) {
		t.Fatalf("Resume(mover) while on battery = %v, want ErrOnBattery", err)
	}
}

// TestScheduler_ResumeFromBattery_DispatchesJobQueuedBeforeTheHold
// proves the dispatch() guard added for Q77's on-battery hold: a mover
// job already queued for an ordinary class conflict before the hold
// began stays queued — never started — once that conflict clears while
// still on battery, and only starts once ResumeFromBattery actually
// dispatches it. PauseForBattery itself never touches the queue; this is
// what makes a held job actually start again once power returns.
func TestScheduler_ResumeFromBattery_DispatchesJobQueuedBeforeTheHold(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	// Occupies ClassArrayWrite so the mover submission below queues
	// instead of running immediately.
	aStarted, aRelease := registerBlocking(s, TypeVMDiskRelocation, false)
	a, err := s.Submit(ctx, TypeVMDiskRelocation, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	<-aStarted

	bStarted, bRelease := registerBlocking(s, TypeMover, false)
	b, err := s.Submit(ctx, TypeMover, nil, nil)
	if err != nil {
		t.Fatalf("Submit(TypeMover): %v", err)
	}
	if b.Status != StatusQueued {
		t.Fatalf("b.Status = %s, want queued (class-conflicted with the running array-write job)", b.Status)
	}

	s.PauseForBattery(ctx)

	close(aRelease)
	waitSucceeded(t, s, a.ID)

	// a finishing would ordinarily free b to start (its class conflict
	// is gone) — the battery hold must keep it queued regardless.
	select {
	case <-bStarted:
		t.Fatal("the queued mover job started while still on battery")
	case <-time.After(50 * time.Millisecond):
	}
	got, err := s.store.Get(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusQueued {
		t.Fatalf("b.Status after a finished = %s, want still queued (Q77: held while on battery)", got.Status)
	}

	s.ResumeFromBattery()
	if s.OnBattery() {
		t.Fatal("OnBattery() = true after ResumeFromBattery")
	}
	<-bStarted
	close(bRelease)
	waitSucceeded(t, s, b.ID)
}

func TestScheduler_ShareMutationDrainWaitsAndMaintenanceRefuses(t *testing.T) {
	s := NewScheduler(nil, nil, nil, NewRegistry())
	if err := s.BeginShareMutation(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- s.DrainShareMutations(ctx)
	}()
	select {
	case err := <-done:
		t.Fatalf("DrainShareMutations returned before FinishShareMutation: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	s.FinishShareMutation()
	if err := <-done; err != nil {
		t.Fatalf("DrainShareMutations: %v", err)
	}

	if err := s.EnterMaintenance(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginShareMutation(); !errors.Is(err, ErrMaintenanceMode) {
		t.Fatalf("BeginShareMutation during maintenance = %v, want ErrMaintenanceMode", err)
	}
}
