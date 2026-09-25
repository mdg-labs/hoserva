package job

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/cache"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/store"
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
		Store:            newRebalanceTestArray(t, filepath.Join(base, "disk1"), filepath.Join(base, "disk2")),
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
		Store:            newRebalanceTestArray(t, filepath.Join(base, "disk1"), filepath.Join(base, "disk2")),
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

// newRebalanceTestArray is an array of the given data disks, none of
// them leaving it.
func newRebalanceTestArray(t *testing.T, mountpoints ...string) *store.ArrayStore {
	t.Helper()
	arrays := store.NewArrayStore(newTestDB(t))
	disks := make([]store.ArrayDisk, 0, len(mountpoints))
	for i, mp := range mountpoints {
		disks = append(disks, store.ArrayDisk{Role: store.ArrayRoleData, RoleIndex: i + 1, Device: fmt.Sprintf("/dev/sd%c", 'b'+i), Filesystem: "xfs", FSUUID: fmt.Sprintf("uuid-d%d", i+1), Mountpoint: mp})
	}
	if err := arrays.PutArray(context.Background(), store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "1M", CreatedAt: time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)}, disks); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	return arrays
}

// TestRunRebalance_RefusesAPlanTouchingALeavingDisk covers a plan that
// was computed before a disk entered removal — a rebalance interrupted,
// then resumed after an evacuation started. Its persisted plan still
// targets that disk; the run must refuse it before copying anything, or
// the file lands on a disk the evacuation already emptied (#366).
func TestRunRebalance_RefusesAPlanTouchingALeavingDisk(t *testing.T) {
	for _, state := range []string{store.RemovalStateEvacuating, store.RemovalStateEvacuated, store.RemovalStateUnpooled} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			s := newTestScheduler(t)
			base := t.TempDir()
			disk1, disk2 := filepath.Join(base, "disk1"), filepath.Join(base, "disk2")
			src, dst := filepath.Join(disk1, "media"), filepath.Join(disk2, "media")
			plan := newRebalanceTestPlan(t, src, dst, "media", "movie.mkv", "movie bytes")
			arrays := newRebalanceTestArray(t, disk1, disk2)
			if err := arrays.SetRemovalState(ctx, disk2, store.RemovalStateEvacuating, "evacuation-job"); err != nil {
				t.Fatalf("SetRemovalState: %v", err)
			}
			if state != store.RemovalStateEvacuating {
				if err := arrays.SetRemovalState(ctx, disk2, store.RemovalStateEvacuated, "evacuation-job"); err != nil {
					t.Fatalf("SetRemovalState(evacuated): %v", err)
				}
			}
			if state == store.RemovalStateUnpooled {
				if err := arrays.AdvanceRemovalState(ctx, disk2, store.RemovalStateEvacuated, store.RemovalStateUnpooled, "remove-job"); err != nil {
					t.Fatalf("AdvanceRemovalState(unpooled): %v", err)
				}
			}

			eng := newRecordingEngine()
			eng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)
			s.registry.Register(TypeRebalance, true, RunRebalance(RebalanceDeps{
				Sync:             syncFuncFromEngine(eng),
				TrackedFileCount: func(context.Context) (int, error) { return 1000, nil },
				Store:            arrays,
			}))

			j, err := s.Submit(ctx, TypeRebalance, nil, mustJSON(t, RebalanceParams{Plan: plan}))
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}
			finished := await(t, s, j.ID)
			if finished.Status != StatusFailed {
				t.Fatalf("status = %s, want failed", finished.Status)
			}
			if !strings.Contains(finished.ErrorMessage, disk2) {
				t.Fatalf("ErrorMessage = %q, want it to name %s", finished.ErrorMessage, disk2)
			}
			if _, err := os.Stat(filepath.Join(src, "movie.mkv")); err != nil {
				t.Fatalf("source must be untouched: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dst, "movie.mkv")); !os.IsNotExist(err) {
				t.Fatalf("nothing may be copied to a leaving disk: err=%v", err)
			}
		})
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
