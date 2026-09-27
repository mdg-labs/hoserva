package job

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestScheduler_ResumeRefusesJobWhoseRunnerIsStillAlive is #402's own
// reproduction of the double-run defect surfaced by #269's verifier:
// Resume trusted the store's own stored status and never checked
// s.running for the same id. It stands in for a whole-database restore
// (ImportConfig) that overwrote this still-running job's row — its
// stored status now reads interrupted, and its stored resource scope
// ("diskB") no longer overlaps what is actually running ("diskA"), so
// the ordinary class/resource-scope conflict check Resume otherwise
// relies on cannot catch it either. Without the fix, Resume starts a
// second, genuinely concurrent run of the same job id — two movers on
// one checkpoint (doc 09). With the fix, it is refused with
// ErrJobAlreadyRunning and the RunFunc is never invoked a second time.
func TestScheduler_ResumeRefusesJobWhoseRunnerIsStillAlive(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	st := NewStore(db)
	s := NewScheduler(st, NewLogStore(t.TempDir()), NewHub(), NewRegistry())

	var runs int32
	started := make(chan struct{})
	release := make(chan struct{})
	s.registry.Register(TypeMover, true, func(ctx context.Context, rc *RunContext) error {
		atomic.AddInt32(&runs, 1)
		select {
		case <-started:
		default:
			close(started)
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	})

	j, err := s.Submit(ctx, TypeMover, []string{"diskA"}, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	<-started

	if _, err := db.ExecContext(ctx,
		`UPDATE jobs SET status = 'interrupted', resource_ids = ? WHERE id = ?`,
		`["diskB"]`, j.ID); err != nil {
		t.Fatalf("simulating the restore's overwrite of the stored row: %v", err)
	}

	if _, err := s.Resume(ctx, j.ID); !errors.Is(err, ErrJobAlreadyRunning) {
		t.Fatalf("Resume while the runner is still alive = %v, want ErrJobAlreadyRunning", err)
	}

	if got := atomic.LoadInt32(&runs); got != 1 {
		t.Fatalf("RunFunc invoked %d times after the refused Resume, want exactly 1 — a second Resume started a concurrent run", got)
	}

	close(release)
	waitSucceeded(t, s, j.ID)
	if got := atomic.LoadInt32(&runs); got != 1 {
		t.Fatalf("RunFunc invoked %d times after the job finished, want exactly 1 — a second run started behind it", got)
	}
}

// TestScheduler_BeginDatabaseRestore_RefusesWhileJobActive covers the
// acquire-time refusal (#402): a whole-database restore must never begin
// while any job is queued or running, whether or not the scheduler
// itself started it — BeginDatabaseRestore checks the store, not only
// its own in-memory bookkeeping.
func TestScheduler_BeginDatabaseRestore_RefusesWhileJobActive(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	started, release := registerBlocking(s, TypeSync, true)

	j, err := s.Submit(ctx, TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	<-started

	if _, err := s.BeginDatabaseRestore(ctx); !errors.Is(err, ErrJobsActiveForRestore) {
		t.Fatalf("BeginDatabaseRestore while a job is running = %v, want ErrJobsActiveForRestore", err)
	}

	close(release)
	waitSucceeded(t, s, j.ID)
}

// TestScheduler_BeginDatabaseRestore_HoldRefusesSubmitAndResumeThenReleases
// covers acquire, refuse and release together (#402): once the hold is
// taken, a second acquire, Submit and Resume are all refused with the
// named error, and releasing it restores normal admission.
func TestScheduler_BeginDatabaseRestore_HoldRefusesSubmitAndResumeThenReleases(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	s.registry.Register(TypeSync, true, func(ctx context.Context, rc *RunContext) error { return nil })

	release, err := s.BeginDatabaseRestore(ctx)
	if err != nil {
		t.Fatalf("BeginDatabaseRestore: %v", err)
	}

	if _, err := s.BeginDatabaseRestore(ctx); !errors.Is(err, ErrDatabaseRestoreInProgress) {
		t.Fatalf("second BeginDatabaseRestore while held = %v, want ErrDatabaseRestoreInProgress", err)
	}
	if _, err := s.Submit(ctx, TypeSync, nil, nil); !errors.Is(err, ErrDatabaseRestoreInProgress) {
		t.Fatalf("Submit while a restore is held = %v, want ErrDatabaseRestoreInProgress", err)
	}
	if _, err := s.Resume(ctx, "does-not-exist"); !errors.Is(err, ErrDatabaseRestoreInProgress) {
		t.Fatalf("Resume while a restore is held = %v, want ErrDatabaseRestoreInProgress", err)
	}

	release()

	j, err := s.Submit(ctx, TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit after release: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("status after release = %s, want succeeded", finished.Status)
	}
}

// TestScheduler_Cancel_RefusesWhileDatabaseRestoreHeld is #402's own
// regression test for the scope update's Cancel direction: an operator
// cancelling an interrupted job — an evacuation or a data-disk upgrade,
// standing in here for TypeMover with a registered AbortFunc — while a
// database restore holds admission must be refused before abortAndCancel
// ever runs, never race RestoreDatabase for the manifest/removal-state
// writes an abort makes.
func TestScheduler_Cancel_RefusesWhileDatabaseRestoreHeld(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	s.registry.Register(TypeMover, true, func(ctx context.Context, rc *RunContext) error { return nil })
	var abortCalls int32
	s.registry.RegisterAbort(TypeMover, func(ctx context.Context, id string, params []byte) error {
		atomic.AddInt32(&abortCalls, 1)
		return nil
	})

	j := &Job{
		ID:          "interrupted-mover",
		Type:        TypeMover,
		Class:       ClassArrayWrite,
		Status:      StatusInterrupted,
		Resumable:   true,
		Cancellable: true,
		CreatedAt:   time.Now().UTC(),
	}
	if err := s.store.Create(ctx, j); err != nil {
		t.Fatalf("seeding the interrupted job: %v", err)
	}

	release, err := s.BeginDatabaseRestore(ctx)
	if err != nil {
		t.Fatalf("BeginDatabaseRestore: %v", err)
	}

	if _, err := s.Cancel(ctx, j.ID); !errors.Is(err, ErrDatabaseRestoreInProgress) {
		t.Fatalf("Cancel while a restore is held = %v, want ErrDatabaseRestoreInProgress", err)
	}
	if got := atomic.LoadInt32(&abortCalls); got != 0 {
		t.Fatalf("AbortFunc invoked %d times by a Cancel refused before it ran, want 0", got)
	}

	got, err := s.store.Get(ctx, j.ID)
	if err != nil {
		t.Fatalf("Get after the refused Cancel: %v", err)
	}
	if got.Status != StatusInterrupted {
		t.Fatalf("job status after the refused Cancel = %q, want it left untouched at interrupted", got.Status)
	}

	release()

	if _, err := s.Cancel(ctx, j.ID); err != nil {
		t.Fatalf("Cancel after release: %v", err)
	}
	if got := atomic.LoadInt32(&abortCalls); got != 1 {
		t.Fatalf("AbortFunc invoked %d times after release, want exactly 1", got)
	}
}

// TestScheduler_BeginDatabaseRestore_RefusesWhileAbortInProgress is #402's
// own regression test for the scope update's other direction: a database
// restore must never begin while a Cancel's abort of an interrupted job is
// still running, even though that job is neither queued nor running (the
// existing active-jobs check alone would miss it) — abortAndCancel's own
// writes must finish, or never start, before RestoreDatabase can touch the
// same tables.
func TestScheduler_BeginDatabaseRestore_RefusesWhileAbortInProgress(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	s.registry.Register(TypeMover, true, func(ctx context.Context, rc *RunContext) error { return nil })
	abortStarted := make(chan struct{})
	abortRelease := make(chan struct{})
	s.registry.RegisterAbort(TypeMover, func(ctx context.Context, id string, params []byte) error {
		close(abortStarted)
		<-abortRelease
		return nil
	})

	j := &Job{
		ID:          "interrupted-mover",
		Type:        TypeMover,
		Class:       ClassArrayWrite,
		Status:      StatusInterrupted,
		Resumable:   true,
		Cancellable: true,
		CreatedAt:   time.Now().UTC(),
	}
	if err := s.store.Create(ctx, j); err != nil {
		t.Fatalf("seeding the interrupted job: %v", err)
	}

	cancelErrCh := make(chan error, 1)
	go func() {
		_, err := s.Cancel(ctx, j.ID)
		cancelErrCh <- err
	}()
	<-abortStarted

	if _, err := s.BeginDatabaseRestore(ctx); !errors.Is(err, ErrJobsActiveForRestore) {
		t.Fatalf("BeginDatabaseRestore while an abort is running = %v, want ErrJobsActiveForRestore", err)
	}

	close(abortRelease)
	if err := <-cancelErrCh; err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	release, err := s.BeginDatabaseRestore(ctx)
	if err != nil {
		t.Fatalf("BeginDatabaseRestore after the abort finished: %v", err)
	}
	release()
}

// TestStore_InterruptByID_TouchesOnlyGivenIDs is #402's own regression
// test for the "mirror image" race #269's verifier also reported:
// InterruptActiveJobs' blanket UPDATE over every queued/running row would
// catch a job inserted into the live database around a restore even
// though it was never part of the archive being restored. InterruptByID
// must mark interrupted only the ids it is given, and leave every other
// row — however its own status reads — exactly as it was.
func TestStore_InterruptByID_TouchesOnlyGivenIDs(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewStore(db)

	archived := &Job{ID: "archived-job", Type: TypeSync, Class: ClassParity, Status: StatusRunning, CreatedAt: time.Now().UTC()}
	if err := s.Create(ctx, archived); err != nil {
		t.Fatalf("seeding the archived job: %v", err)
	}
	notArchived := &Job{ID: "not-archived-job", Type: TypeScrub, Class: ClassParity, Status: StatusRunning, CreatedAt: time.Now().UTC()}
	if err := s.Create(ctx, notArchived); err != nil {
		t.Fatalf("seeding the job inserted outside the restore window: %v", err)
	}

	if err := s.InterruptByID(ctx, []string{archived.ID}, time.Now().UTC()); err != nil {
		t.Fatalf("InterruptByID: %v", err)
	}

	got, err := s.Get(ctx, archived.ID)
	if err != nil {
		t.Fatalf("Get(archived): %v", err)
	}
	if got.Status != StatusInterrupted {
		t.Fatalf("archived job status = %q, want interrupted", got.Status)
	}

	stillRunning, err := s.Get(ctx, notArchived.ID)
	if err != nil {
		t.Fatalf("Get(notArchived): %v", err)
	}
	if stillRunning.Status != StatusRunning {
		t.Fatalf("job not named in InterruptByID's ids = %q, want it left untouched at running", stillRunning.Status)
	}
}
