package job

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// resumeSealHarness is a scheduler with one interrupted, resumable mover
// whose Resume can be held inside its log repair.
type resumeSealHarness struct {
	s    *Scheduler
	ctx  context.Context
	id   string
	runs *int32
}

func newResumeSealHarness(t *testing.T) *resumeSealHarness {
	t.Helper()
	ctx := context.Background()
	s := newTestScheduler(t)
	var runs int32
	s.registry.Register(TypeMover, true, func(ctx context.Context, rc *RunContext) error {
		atomic.AddInt32(&runs, 1)
		return nil
	})
	s.registry.Register(TypeScrub, false, func(ctx context.Context, rc *RunContext) error { return nil })

	interrupted := &Job{ID: "j1", Type: TypeMover, Class: ClassArrayWrite, Status: StatusInterrupted, Resumable: true, Cancellable: true, CreatedAt: time.Now().UTC()}
	if err := s.store.Create(ctx, interrupted); err != nil {
		t.Fatal(err)
	}
	return &resumeSealHarness{s: s, ctx: ctx, id: interrupted.ID, runs: &runs}
}

func (h *resumeSealHarness) status(t *testing.T) Status {
	t.Helper()
	got, err := h.s.store.Get(h.ctx, h.id)
	if err != nil {
		t.Fatal(err)
	}
	return got.Status
}

func (h *resumeSealHarness) markersCleared() bool {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	return len(h.s.resuming) == 0
}

// holdSeal makes the next Resume stop inside its log repair until the
// returned release func is called, and returns a channel closed once it is
// there.
func (h *resumeSealHarness) holdSeal() (sealing <-chan struct{}, release func()) {
	in := make(chan struct{})
	out := make(chan struct{})
	h.s.beforeSealHook = func(string) {
		close(in)
		<-out
	}
	return in, func() { close(out) }
}

func (h *resumeSealHarness) resumeAsync() <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := h.s.Resume(h.ctx, h.id)
		done <- err
	}()
	return done
}

func waitDone[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not finish: it is blocked behind the lock the log repair holds", what)
		panic("unreachable")
	}
}

// TestScheduler_ResumeDoesNotHoldTheLockWhileSealing fails against a Resume
// that repairs the log under s.mu: the unrelated Submit blocks on s.mu for
// as long as the repair is held, so it cannot finish first.
func TestScheduler_ResumeDoesNotHoldTheLockWhileSealing(t *testing.T) {
	h := newResumeSealHarness(t)
	sealing, release := h.holdSeal()
	resumed := h.resumeAsync()
	<-sealing

	submitted := make(chan error, 1)
	go func() {
		_, err := h.s.Submit(h.ctx, TypeScrub, nil, nil)
		submitted <- err
	}()
	err := waitDone(t, "Submit of an unrelated job", submitted)
	if err != nil {
		release()
		t.Fatalf("Submit while a Resume is sealing: %v", err)
	}

	release()
	if err := waitDone(t, "Resume", resumed); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	waitSucceeded(t, h.s, h.id)
	if got := atomic.LoadInt32(h.runs); got != 1 {
		t.Fatalf("RunFunc invoked %d times, want 1", got)
	}
	if !h.markersCleared() {
		t.Fatal("s.resuming still holds the job after Resume returned")
	}
}

func TestScheduler_ResumeWhileSealingRefusesASecondResumeAndACancel(t *testing.T) {
	h := newResumeSealHarness(t)
	sealing, release := h.holdSeal()
	resumed := h.resumeAsync()
	<-sealing

	second := h.resumeAsync()
	err := waitDone(t, "second Resume", second)
	if !errors.Is(err, ErrJobResumeInProgress) || errors.Is(err, ErrJobAbortInProgress) {
		release()
		t.Fatalf("second Resume while sealing = %v, want ErrJobResumeInProgress and not ErrJobAbortInProgress", err)
	}

	cancelled := make(chan error, 1)
	go func() {
		_, err := h.s.Cancel(h.ctx, h.id)
		cancelled <- err
	}()
	err = waitDone(t, "Cancel", cancelled)
	if !errors.Is(err, ErrJobResumeInProgress) {
		release()
		t.Fatalf("Cancel while sealing = %v, want ErrJobResumeInProgress", err)
	}
	if got := h.status(t); got != StatusInterrupted {
		release()
		t.Fatalf("job status while sealing = %s, want interrupted", got)
	}

	release()
	if err := waitDone(t, "first Resume", resumed); err != nil {
		t.Fatalf("first Resume: %v", err)
	}
	waitSucceeded(t, h.s, h.id)
	if got := atomic.LoadInt32(h.runs); got != 1 {
		t.Fatalf("RunFunc invoked %d times, want exactly 1 — the refused Resume started a second run", got)
	}
}

func TestScheduler_BeginDatabaseRestoreRefusedWhileAResumeIsSealing(t *testing.T) {
	h := newResumeSealHarness(t)
	sealing, release := h.holdSeal()
	resumed := h.resumeAsync()
	<-sealing

	if _, err := h.s.BeginDatabaseRestore(h.ctx); !errors.Is(err, ErrJobsActiveForRestore) {
		release()
		t.Fatalf("BeginDatabaseRestore while a Resume is sealing = %v, want ErrJobsActiveForRestore", err)
	}

	release()
	if err := waitDone(t, "Resume", resumed); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	waitSucceeded(t, h.s, h.id)
}

// TestScheduler_ResumeRevalidatesAfterSealing changes each precondition
// while the repair runs, as a caller landing in the window the lock is
// released would; the Resume must refuse, leave the job interrupted and
// clear its marker.
func TestScheduler_ResumeRevalidatesAfterSealing(t *testing.T) {
	cases := []struct {
		name   string
		change func(t *testing.T, h *resumeSealHarness)
		want   error
	}{
		{"maintenance entered", func(t *testing.T, h *resumeSealHarness) {
			if err := h.s.EnterMaintenance(h.ctx); err != nil {
				t.Errorf("EnterMaintenance: %v", err)
			}
		}, ErrMaintenanceMode},
		{"battery hold set", func(t *testing.T, h *resumeSealHarness) {
			h.s.PauseForBattery(h.ctx)
		}, ErrOnBattery},
		{"database restore begun", func(t *testing.T, h *resumeSealHarness) {
			h.s.mu.Lock()
			h.s.databaseRestore = true
			h.s.mu.Unlock()
		}, ErrDatabaseRestoreInProgress},
		{"job no longer interrupted", func(t *testing.T, h *resumeSealHarness) {
			if err := h.s.store.UpdateStatus(h.ctx, h.id, StatusCancelled, nil, "", "", nil, timePtr(time.Now().UTC())); err != nil {
				t.Errorf("UpdateStatus: %v", err)
			}
		}, ErrJobNotInterrupted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newResumeSealHarness(t)
			h.s.beforeSealHook = func(string) { tc.change(t, h) }

			_, err := h.s.Resume(h.ctx, h.id)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Resume = %v, want %v", err, tc.want)
			}
			if tc.want != ErrJobNotInterrupted {
				if got := h.status(t); got != StatusInterrupted {
					t.Fatalf("job status after the refused Resume = %s, want interrupted", got)
				}
			}
			if !h.markersCleared() {
				t.Fatal("s.resuming still holds the job after the refused Resume")
			}
			if got := atomic.LoadInt32(h.runs); got != 0 {
				t.Fatalf("RunFunc invoked %d times by a refused Resume", got)
			}
		})
	}
}

func TestScheduler_ResumeRefusedAfterSealingCanBeRetried(t *testing.T) {
	h := newResumeSealHarness(t)
	h.s.beforeSealHook = func(string) {
		if err := h.s.EnterMaintenance(h.ctx); err != nil {
			t.Errorf("EnterMaintenance: %v", err)
		}
	}
	if _, err := h.s.Resume(h.ctx, h.id); !errors.Is(err, ErrMaintenanceMode) {
		t.Fatalf("Resume = %v, want ErrMaintenanceMode", err)
	}
	h.s.beforeSealHook = nil
	h.s.ExitMaintenance()

	if _, err := h.s.Resume(h.ctx, h.id); err != nil {
		t.Fatalf("Resume after the refusal cleared: %v", err)
	}
	waitSucceeded(t, h.s, h.id)
}

func TestScheduler_ResumeClearsItsMarkerWhenSealPanics(t *testing.T) {
	h := newResumeSealHarness(t)
	h.s.beforeSealHook = func(string) { panic("seal failed") }

	func() {
		defer func() {
			if recover() == nil {
				t.Error("Resume did not propagate the panic")
			}
		}()
		_, _ = h.s.Resume(h.ctx, h.id)
	}()

	if !h.markersCleared() {
		t.Fatal("s.resuming still holds the job after Seal panicked")
	}
	h.s.beforeSealHook = nil
	if _, err := h.s.Resume(h.ctx, h.id); err != nil {
		t.Fatalf("Resume after the panic: %v", err)
	}
	waitSucceeded(t, h.s, h.id)
}

func TestScheduler_ResumeRepairsAnUnclosedLogOutsideTheLock(t *testing.T) {
	h := newResumeSealHarness(t)
	writeLogRun(t, h.s.logs, h.id, "run one\n", false)

	if _, err := h.s.Resume(h.ctx, h.id); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	waitSucceeded(t, h.s, h.id)
	if got := readWholeLog(t, h.s.logs, h.id); !strings.HasPrefix(got, "run one\n") {
		t.Fatalf("log = %q, want run one's output kept and the log one complete gzip stream", got)
	}
}
