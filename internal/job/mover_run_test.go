package job

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/cache"
)

func newTestMoverShare(t *testing.T) cache.Share {
	t.Helper()
	base := t.TempDir()
	share := cache.Share{
		Name:      "movies",
		CachePath: filepath.Join(base, "cache", "movies"),
		ArrayPath: filepath.Join(base, "array", "movies"),
	}
	src := filepath.Join(share.CachePath, "movie.mkv")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("movie bytes"), 0o640); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(src, old, old); err != nil {
		t.Fatal(err)
	}
	return share
}

// TestRunMover_MovesThroughScheduler proves TypeMover, once registered
// with RunMover, actually runs cache.Run through a real Scheduler.Submit/
// Await round trip (#53's own "nothing ever runs the mover" finding) —
// not merely that cache.Run works in isolation.
func TestRunMover_MovesThroughScheduler(t *testing.T) {
	s := newTestScheduler(t)
	share := newTestMoverShare(t)

	s.registry.Register(TypeMover, true, RunMover(MoverDeps{
		Shares: func(ctx context.Context) ([]cache.Share, error) {
			return []cache.Share{share}, nil
		},
	}))

	j, err := s.Submit(context.Background(), TypeMover, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished, err := s.Await(context.Background(), j.ID)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if finished.Status != StatusSucceeded {
		t.Fatalf("job status = %s, want %s", finished.Status, StatusSucceeded)
	}

	dst := filepath.Join(share.ArrayPath, "movie.mkv")
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "movie bytes" {
		t.Fatalf("target content = %q, %v, want %q", got, err, "movie bytes")
	}
	if _, err := os.Stat(filepath.Join(share.CachePath, "movie.mkv")); !os.IsNotExist(err) {
		t.Fatalf("source should be gone after the mover job ran: err=%v", err)
	}
}

// TestRunMover_SharesErrorFailsTheJob proves a failure resolving shares —
// the daemon's own configured state, not a job-params payload — fails the
// job rather than silently doing nothing.
func TestRunMover_SharesErrorFailsTheJob(t *testing.T) {
	s := newTestScheduler(t)
	wantErr := context.DeadlineExceeded

	s.registry.Register(TypeMover, true, RunMover(MoverDeps{
		Shares: func(ctx context.Context) ([]cache.Share, error) {
			return nil, wantErr
		},
	}))

	j, err := s.Submit(context.Background(), TypeMover, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished, err := s.Await(context.Background(), j.ID)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if finished.Status != StatusFailed {
		t.Fatalf("job status = %s, want %s", finished.Status, StatusFailed)
	}
}

// TestMaintenanceChain_MoverStepRunsWhenRegistered proves the nightly
// chain's own first step (Q30) actually reaches a registered mover,
// rather than only ever taking the Skipped fallback for an unregistered
// TypeMover.
func TestMaintenanceChain_MoverStepRunsWhenRegistered(t *testing.T) {
	s := newTestScheduler(t)
	share := newTestMoverShare(t)

	s.registry.Register(TypeMover, true, RunMover(MoverDeps{
		Shares: func(ctx context.Context) ([]cache.Share, error) {
			return []cache.Share{share}, nil
		},
	}))
	registerRecording(s, TypeSync, &stepRecorder{}, "sync")

	chain := &MaintenanceChain{
		Scheduler: s,
		Guard:     &fakeGuard{},
	}

	result, err := chain.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Steps[0].Step != StepMover || result.Steps[0].Skipped {
		t.Fatalf("mover step = %+v, want it to have actually run", result.Steps[0])
	}
	if result.Steps[0].Status != StatusSucceeded {
		t.Fatalf("mover step status = %s, want %s", result.Steps[0].Status, StatusSucceeded)
	}

	dst := filepath.Join(share.ArrayPath, "movie.mkv")
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("mover step should have actually moved the file: %v", err)
	}
}
