package job

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/mdg-labs/hoserva/internal/parity"
)

// usageComputingEngine adds a scriptable ComputeShareUsage to
// recordingEngine, so RunSync's optional shareUsageComputer hook (#223)
// can be exercised without a real SnapraidEngine or database.
type usageComputingEngine struct {
	*recordingEngine

	mu       sync.Mutex
	computed int
	failWith error
}

func newUsageComputingEngine() *usageComputingEngine {
	return &usageComputingEngine{recordingEngine: newRecordingEngine()}
}

func (u *usageComputingEngine) ComputeShareUsage(ctx context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.computed++
	return u.failWith
}

func (u *usageComputingEngine) computedCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.computed
}

// TestRunSync_ComputesShareUsageAfterSuccess confirms a real, non-dry-run
// sync that succeeds triggers exactly one ComputeShareUsage call
// (doc 02 §1 line 78: only as a step of the sync job).
func TestRunSync_ComputesShareUsageAfterSuccess(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	eng := newUsageComputingEngine()
	eng.ScriptSync([]parity.Progress{{Phase: "syncing", Percent: 100}}, nil)
	s.registry.Register(TypeSync, false, RunSync(eng))

	j, err := s.Submit(ctx, TypeSync, nil, mustJSON(t, SyncParams{}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	if got := eng.computedCount(); got != 1 {
		t.Fatalf("ComputeShareUsage called %d times, want 1", got)
	}
}

// TestRunSync_DryRunNeverComputesShareUsage: a dry run never writes a new
// content file, so there is nothing fresh for `snapraid list` to read.
func TestRunSync_DryRunNeverComputesShareUsage(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	eng := newUsageComputingEngine()
	eng.ScriptSync([]parity.Progress{{Phase: "diff", Percent: 100}}, nil)
	s.registry.Register(TypeSync, false, RunSync(eng))

	j, err := s.Submit(ctx, TypeSync, nil, mustJSON(t, SyncParams{DryRun: true}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	if got := eng.computedCount(); got != 0 {
		t.Fatalf("ComputeShareUsage called %d times on a dry run, want 0", got)
	}
}

// TestRunSync_GuardBlockNeverComputesShareUsage: a sync that never ran
// (blocked by the threshold guard, no Confirm) has no new content file
// either.
func TestRunSync_GuardBlockNeverComputesShareUsage(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	eng := newUsageComputingEngine()
	eng.ScriptGuardBlock(trippedGuard())
	eng.ScriptSync([]parity.Progress{{Phase: "syncing", Percent: 100}}, nil)
	s.registry.Register(TypeSync, false, RunSync(eng))

	j, err := s.Submit(ctx, TypeSync, nil, mustJSON(t, SyncParams{Confirm: false}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s, want failed", finished.Status)
	}
	if got := eng.computedCount(); got != 0 {
		t.Fatalf("ComputeShareUsage called %d times after a guard block, want 0", got)
	}
}

// TestRunSync_ShareUsageComputeFailureFailsTheJob: parity itself synced
// successfully, but persisting the figures failed — RunSync surfaces
// that rather than silently losing it.
func TestRunSync_ShareUsageComputeFailureFailsTheJob(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	eng := newUsageComputingEngine()
	eng.ScriptSync([]parity.Progress{{Phase: "syncing", Percent: 100}}, nil)
	eng.failWith = errors.New("database is locked")
	s.registry.Register(TypeSync, false, RunSync(eng))

	j, err := s.Submit(ctx, TypeSync, nil, mustJSON(t, SyncParams{}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s, want failed", finished.Status)
	}
	if !strings.Contains(finished.ErrorMessage, "database is locked") {
		t.Fatalf("ErrorMessage = %q, want it to mention the share usage failure", finished.ErrorMessage)
	}
	_, wrote, _, _ := eng.snapshot()
	if !wrote {
		t.Fatal("sync itself did not run even though only the usage computation should have failed")
	}
}
