package job

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/cache"
	"github.com/mdg-labs/hoserva/internal/parity"
)

// newRebalanceTestPlan writes content at srcDir/rel and returns the
// single-move cache.RebalancePlan a RunRebalance job would be submitted
// with for it — the shape startRebalance builds from a fresh
// cache.PlanRebalance call, reused here directly since these tests are
// about the job wiring, not plan computation (#55 already tests that).
func newRebalanceTestPlan(t *testing.T, srcDir, dstDir, share, rel, content string) cache.RebalancePlan {
	t.Helper()
	mustWriteFile(t, filepath.Join(srcDir, rel), content)
	return cache.RebalancePlan{Moves: []cache.RebalanceMove{{
		Share:        share,
		RelPath:      rel,
		SourceBranch: srcDir,
		TargetBranch: dstDir,
		Size:         int64(len(content)),
	}}}
}

// TestRunRebalance_MovesThroughScheduler proves job.TypeRebalance's own
// registration reaches cache.RunRebalance through a real
// Scheduler.Submit/Await round trip (registry wiring, doc 09 §3, #274) —
// not calling RunRebalance directly, the way #55's own package-level
// tests do.
func TestRunRebalance_MovesThroughScheduler(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	base := t.TempDir()
	src := filepath.Join(base, "disk1", "media")
	dst := filepath.Join(base, "disk2", "media")
	plan := newRebalanceTestPlan(t, src, dst, "media", "movie.mkv", "movie bytes")

	eng := newRecordingEngine()
	eng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)

	s.registry.Register(TypeRebalance, true, RunRebalance(RebalanceDeps{
		Sync:             syncFuncFromEngine(eng),
		TrackedFileCount: func(context.Context) (int, error) { return 1000, nil },
	}))

	j, err := s.Submit(ctx, TypeRebalance, nil, mustJSON(t, RebalanceParams{Plan: plan}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	if _, err := os.Stat(filepath.Join(src, "movie.mkv")); !os.IsNotExist(err) {
		t.Fatalf("source should be gone after a completed rebalance: err=%v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "movie.mkv"))
	if err != nil || string(got) != "movie bytes" {
		t.Fatalf("target content = %q, %v, want %q", got, err, "movie bytes")
	}
}

// TestRunRebalance_GuardBlocked_LeavesSourceUntouched is this issue's own
// central safety test (CLAUDE.md: "anything that can lose data gets its
// test before its implementation"), the rebalance-job equivalent of
// TestRunShareRelocation_ToCache_GuardBlocked_LeavesArrayUntouched: the
// job wiring this issue adds must route cache.RunRebalance's own
// protecting sync through the real threshold guard, driven through a real
// Scheduler.Submit call — a blocked sync fails the job before anything is
// deleted, with the verified target copy already made surviving so a
// later, confirmed rebalance can still finish.
func TestRunRebalance_GuardBlocked_LeavesSourceUntouched(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	base := t.TempDir()
	src := filepath.Join(base, "disk1", "media")
	dst := filepath.Join(base, "disk2", "media")
	plan := newRebalanceTestPlan(t, src, dst, "media", "movie.mkv", "movie bytes")

	eng := newRecordingEngine()
	eng.ScriptGuardBlock(trippedGuard())

	s.registry.Register(TypeRebalance, true, RunRebalance(RebalanceDeps{
		Sync:             syncFuncFromEngine(eng),
		TrackedFileCount: func(context.Context) (int, error) { return 1000, nil },
	}))

	j, err := s.Submit(ctx, TypeRebalance, nil, mustJSON(t, RebalanceParams{Plan: plan}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s, want failed", finished.Status)
	}
	if !strings.Contains(finished.ErrorMessage, "threshold guard blocked the sync") {
		t.Fatalf("ErrorMessage = %q, want a threshold-guard block", finished.ErrorMessage)
	}
	if _, err := os.Stat(filepath.Join(src, "movie.mkv")); err != nil {
		t.Fatalf("source must survive a blocked sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "movie.mkv")); err != nil {
		t.Fatalf("verified target copy must survive a blocked sync: %v", err)
	}
}

func TestValidateParams_RebalanceRequiresAPlan(t *testing.T) {
	for _, params := range [][]byte{nil, []byte("null"), []byte("")} {
		if err := ValidateParams(TypeRebalance, params); err == nil {
			t.Fatalf("ValidateParams(rebalance, %q) = nil, want rejection", params)
		}
	}
	// An empty plan (no moves) is legitimate — an already-balanced pool —
	// and must not be rejected.
	if err := ValidateParams(TypeRebalance, mustJSON(t, RebalanceParams{})); err != nil {
		t.Fatalf("ValidateParams(rebalance, empty plan) = %v, want nil", err)
	}
}
