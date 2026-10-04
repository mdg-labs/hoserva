package job

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/disk"
)

type queuedBehindImport struct {
	s       *Scheduler
	pending atomic.Bool
	checkFn atomic.Pointer[func(context.Context) (bool, error)]
	release chan struct{}
	ran     atomic.Int32
	aborted atomic.Int32
}

func newQueuedBehindImport(t *testing.T, queued Type, abort AbortFunc) (*queuedBehindImport, *Job, *Job) {
	t.Helper()
	h := &queuedBehindImport{s: newTestScheduler(t)}
	started, release := registerBlocking(h.s, TypeMigrationImport, false)
	h.release = release
	h.s.registry.Register(queued, true, func(context.Context, *RunContext) error {
		h.ran.Add(1)
		return nil
	})
	if abort != nil {
		h.s.registry.RegisterAbort(queued, abort)
	}
	h.s.SetMigrationPending(func(ctx context.Context) (bool, error) {
		if fn := h.checkFn.Load(); fn != nil {
			return (*fn)(ctx)
		}
		return h.pending.Load(), nil
	})

	ctx := context.Background()
	imp, err := h.s.Submit(ctx, TypeMigrationImport, nil, newImportHarness(t).params())
	if err != nil {
		t.Fatalf("submitting the import: %v", err)
	}
	<-started
	var params []byte
	if queued == TypeDiskAdd {
		newDisk := disk.AssignedDisk{Device: "/dev/sdc", Filesystem: disk.XFS}
		params = mustJSON(t, DiskAddParams{Confirmation: SingleDiskConfirmation(newDisk), Disk: newDisk, Sizes: map[string]int64{"/dev/sdc": 4 * disk.TB}})
	}
	q, err := h.s.Submit(ctx, queued, nil, params)
	if err != nil {
		t.Fatalf("submitting %s while the import runs and nothing is pending: %v", queued, err)
	}
	if q.Status != StatusQueued {
		t.Fatalf("%s status = %s, want queued behind the import", queued, q.Status)
	}
	return h, imp, q
}

func awaitJob(t *testing.T, s *Scheduler, id string) *Job {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	j, err := s.Await(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestScheduler_RefusesAQueuedStorageJobWhenAMigrationBecamePendingMeanwhile(t *testing.T) {
	for _, typ := range []Type{TypeSync, TypeMover, TypeDiskAdd} {
		t.Run(string(typ), func(t *testing.T) {
			h, imp, q := newQueuedBehindImport(t, typ, nil)
			h.pending.Store(true)
			close(h.release)
			awaitJob(t, h.s, imp.ID)

			got := awaitJob(t, h.s, q.ID)
			if got.Status != StatusFailed || got.ErrorCode != "migration_in_progress" || got.ErrorMessage != ErrMigrationInProgress.Error() {
				t.Errorf("queued %s = %s %q %q, want failed migration_in_progress %q", typ, got.Status, got.ErrorCode, got.ErrorMessage, ErrMigrationInProgress)
			}
			if h.ran.Load() != 0 {
				t.Errorf("%s ran %d times against the pending adoption", typ, h.ran.Load())
			}
			if blocking := h.s.BlockingStorageJob(); blocking != nil {
				t.Errorf("a refused job still blocks storage: %+v", blocking)
			}
		})
	}
}

func TestScheduler_RunsTheAbortOfARefusedQueuedJobFirst(t *testing.T) {
	var h *queuedBehindImport
	var terminalWhenAborted atomic.Bool
	abort := func(ctx context.Context, id string, _ []byte) error {
		j, err := h.s.store.Get(ctx, id)
		if err != nil || j.Status.Terminal() {
			terminalWhenAborted.Store(true)
		}
		h.aborted.Add(1)
		return nil
	}
	var imp, q *Job
	h, imp, q = newQueuedBehindImport(t, TypeDiskAdd, abort)
	h.pending.Store(true)
	close(h.release)
	awaitJob(t, h.s, imp.ID)

	got := awaitJob(t, h.s, q.ID)
	if got.Status != StatusFailed || got.ErrorCode != "migration_in_progress" {
		t.Errorf("queued job = %s %q, want failed migration_in_progress", got.Status, got.ErrorCode)
	}
	if h.aborted.Load() != 1 || terminalWhenAborted.Load() {
		t.Errorf("abort calls = %d, job already terminal when it ran = %t, want 1 and false", h.aborted.Load(), terminalWhenAborted.Load())
	}
	if h.ran.Load() != 0 {
		t.Errorf("the refused job ran %d times", h.ran.Load())
	}
}

func TestScheduler_LeavesARefusedQueuedJobInterruptedWhenItsAbortFails(t *testing.T) {
	abort := func(context.Context, string, []byte) error { return errors.New("injected: unwind failed") }
	h, imp, q := newQueuedBehindImport(t, TypeDiskAdd, abort)
	h.pending.Store(true)
	close(h.release)
	awaitJob(t, h.s, imp.ID)

	got := awaitJob(t, h.s, q.ID)
	if got.Status != StatusInterrupted || got.ErrorCode != "job_abort_failed" || !strings.Contains(got.ErrorMessage, "injected: unwind failed") {
		t.Errorf("queued job = %s %q %q, want interrupted job_abort_failed with the abort's error", got.Status, got.ErrorCode, got.ErrorMessage)
	}
	if h.ran.Load() != 0 {
		t.Errorf("the refused job ran %d times", h.ran.Load())
	}
}

func TestScheduler_RefusesAQueuedStorageJobWhenTheMigrationCheckFails(t *testing.T) {
	for _, typ := range []Type{TypeScrub, TypeDiskAdd} {
		t.Run(string(typ), func(t *testing.T) {
			h, imp, q := newQueuedBehindImport(t, typ, nil)
			fn := func(context.Context) (bool, error) { return false, errors.New("injected: the database is gone") }
			h.checkFn.Store(&fn)
			close(h.release)
			awaitJob(t, h.s, imp.ID)

			got := awaitJob(t, h.s, q.ID)
			if got.Status != StatusFailed || got.ErrorCode != "migration_check_failed" || !strings.Contains(got.ErrorMessage, "injected: the database is gone") {
				t.Errorf("queued %s = %s %q %q, want failed migration_check_failed with the check's error", typ, got.Status, got.ErrorCode, got.ErrorMessage)
			}
			if h.ran.Load() != 0 {
				t.Errorf("%s ran %d times although the check could not confirm nothing is pending", typ, h.ran.Load())
			}
		})
	}
}

func TestScheduler_StartsAQueuedStorageJobWhenNoMigrationIsPending(t *testing.T) {
	for _, typ := range []Type{TypeSync, TypeMover, TypeDiskAdd} {
		t.Run(string(typ), func(t *testing.T) {
			h, imp, q := newQueuedBehindImport(t, typ, nil)
			close(h.release)
			awaitJob(t, h.s, imp.ID)

			got := awaitJob(t, h.s, q.ID)
			if got.Status != StatusSucceeded || h.ran.Load() != 1 {
				t.Errorf("queued %s = %s after %d runs, want succeeded after 1", typ, got.Status, h.ran.Load())
			}
		})
	}
}

func TestScheduler_StartsAQueuedServiceJobWhileAMigrationIsPending(t *testing.T) {
	s := newTestScheduler(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var runs atomic.Int32
	s.registry.Register(TypeConfigBackup, false, func(context.Context, *RunContext) error {
		if runs.Add(1) == 1 {
			close(started)
			<-release
		}
		return nil
	})
	s.SetMigrationPending(func(context.Context) (bool, error) { return true, nil })

	ctx := context.Background()
	first, err := s.Submit(ctx, TypeConfigBackup, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	q, err := s.Submit(ctx, TypeConfigBackup, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if q.Status != StatusQueued {
		t.Fatalf("service job status = %s, want queued behind the first", q.Status)
	}
	close(release)
	awaitJob(t, s, first.ID)

	if got := awaitJob(t, s, q.ID); got.Status != StatusSucceeded || runs.Load() != 2 {
		t.Errorf("queued service job = %s after %d total runs, want succeeded after 2", got.Status, runs.Load())
	}
}

func TestScheduler_EndsARefusedQueuedJobCancelledWhenCancelledDuringItsAbort(t *testing.T) {
	inAbort := make(chan struct{})
	releaseAbort := make(chan struct{})
	abort := func(context.Context, string, []byte) error {
		close(inAbort)
		<-releaseAbort
		return nil
	}
	t.Cleanup(func() {
		select {
		case <-releaseAbort:
		default:
			close(releaseAbort)
		}
	})
	h, imp, q := newQueuedBehindImport(t, TypeDiskAdd, abort)
	h.pending.Store(true)
	close(h.release)
	awaitJob(t, h.s, imp.ID)
	select {
	case <-inAbort:
	case <-time.After(10 * time.Second):
		t.Fatal("the refused job's abort never ran")
	}

	if _, err := h.s.Cancel(context.Background(), q.ID); err != nil {
		t.Fatalf("cancelling a job whose refusal is being finished: %v", err)
	}
	close(releaseAbort)

	if got := awaitJob(t, h.s, q.ID); got.Status != StatusCancelled || h.ran.Load() != 0 {
		t.Errorf("queued job = %s after %d runs, want cancelled and never run", got.Status, h.ran.Load())
	}
}
