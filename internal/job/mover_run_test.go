package job

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// TestRunMover_LogsPartialReportOnError proves a mid-run error —
// SaveCheckpoint failing after at least one file has already been
// decided — still logs cache.Run's partial report (doc 09 §2's "honest
// reporting") instead of silently discarding it: RunMover used to return
// the bare error and never call report.Summary() at all.
func TestRunMover_LogsPartialReportOnError(t *testing.T) {
	share := newTestMoverShare(t)
	// A second file so the checkpoint failure below can interrupt the
	// run after the first file was already decided, not only right at
	// the very start.
	second := filepath.Join(share.CachePath, "second.mkv")
	if err := os.WriteFile(second, []byte("second bytes"), 0o640); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(second, old, old); err != nil {
		t.Fatal(err)
	}

	fn := RunMover(MoverDeps{
		Shares: func(ctx context.Context) ([]cache.Share, error) {
			return []cache.Share{share}, nil
		},
	})

	var out bytes.Buffer
	boom := errors.New("boom")
	rc := &RunContext{
		ctx:            context.Background(),
		out:            &out,
		stopRequested:  make(chan struct{}),
		saveCheckpoint: func(data []byte) error { return boom },
		setProgress:    func(int) {},
	}

	err := fn(context.Background(), rc)
	if !errors.Is(err, boom) {
		t.Fatalf("RunMover error = %v, want it to wrap %v", err, boom)
	}
	// "duration" only ever appears in Report.Summary()'s own line — the
	// per-file hooks.logf lines never contain it — so this specifically
	// proves Summary() was logged, not merely that some per-file line
	// was (which the pre-fix code also produced, since that logging
	// happens inside cache.Run itself, before the checkpoint error).
	if !strings.Contains(out.String(), "duration") {
		t.Fatalf("job output = %q, want the partial report's Summary() logged despite the error", out.String())
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

// TestRunMover_PersistsStructuredResult proves RunMover writes the
// cache.Report into ResultStore (#273), not only the job-log Summary line.
func TestRunMover_PersistsStructuredResult(t *testing.T) {
	db := newTestDB(t)
	results := cache.NewResultStore(db)
	share := newTestMoverShare(t)

	s := newTestScheduler(t)
	s.registry.Register(TypeMover, true, RunMover(MoverDeps{
		Shares: func(ctx context.Context) ([]cache.Share, error) {
			return []cache.Share{share}, nil
		},
		Results: results,
		UsagePlan: func(ctx context.Context) (string, []cache.UsageShare, error) {
			cacheMount := filepath.Dir(share.CachePath)
			return cacheMount, []cache.UsageShare{{
				Name: share.Name,
				Path: share.CachePath,
				Mode: "cache-then-move",
			}}, nil
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

	run, err := results.LastRun(context.Background())
	if err != nil {
		t.Fatalf("LastRun: %v", err)
	}
	if run.FilesMoved != 1 || run.BytesMoved != int64(len("movie bytes")) {
		t.Fatalf("persisted moved = %d/%d, want 1/%d", run.FilesMoved, run.BytesMoved, len("movie bytes"))
	}
	usage, err := results.CacheUsage(context.Background())
	if err != nil {
		t.Fatalf("CacheUsage: %v", err)
	}
	if usage.PendingMovesBytes != 0 {
		t.Fatalf("PendingMovesBytes = %d, want 0 after the file was moved", usage.PendingMovesBytes)
	}
}

func TestRunMover_PersistsResultWhenContextIsCancelled(t *testing.T) {
	db := newTestDB(t)
	results := cache.NewResultStore(db)
	share := newTestMoverShare(t)
	fn := RunMover(MoverDeps{
		Shares: func(context.Context) ([]cache.Share, error) {
			return []cache.Share{share}, nil
		},
		Results: results,
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	rc := &RunContext{
		ctx:            ctx,
		out:            &out,
		stopRequested:  make(chan struct{}),
		saveCheckpoint: func([]byte) error { return nil },
		setProgress:    func(int) {},
	}
	_ = fn(ctx, rc)
	run, err := results.LastRun(context.Background())
	if err != nil {
		t.Fatalf("LastRun after a cancelled run: %v", err)
	}
	if run.StartedAt.IsZero() {
		t.Fatal("cancelled run was not persisted")
	}
}
