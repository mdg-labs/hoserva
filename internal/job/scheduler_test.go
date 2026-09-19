package job

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
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

	bStarted, bRelease := registerBlocking(s, TypeRebalance, false)
	b, err := s.Submit(ctx, TypeRebalance, []string{"disk-2"}, nil)
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

	bStarted, bRelease := registerBlocking(s, TypeEvacuation, false)
	b, err := s.Submit(ctx, TypeEvacuation, []string{"disk-1"}, nil)
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
	_, err := s.Submit(ctx, TypeDiskAdd, nil, nil)
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
	s.registry.Register(TypeRebalance, false, func(ctx context.Context, rc *RunContext) error {
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

	j, err := s.Submit(ctx, TypeRebalance, nil, nil)
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
