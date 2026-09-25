package job

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/cache"
	"github.com/mdg-labs/hoserva/internal/parity"
	hoservastore "github.com/mdg-labs/hoserva/internal/store"
)

// evacuationOwnershipHarness is one scheduler, one real SQLite database
// holding both the array topology (store.ArrayStore) and the relocation
// manifest (parity.RelocationManifestStore), and a two-data-disk array
// whose disk1 holds one file to evacuate. Used by the tests below for
// #359's rule that an evacuation's removal state belongs to the job that
// set it.
type evacuationOwnershipHarness struct {
	s        *Scheduler
	arrays   *hoservastore.ArrayStore
	manifest *parity.RelocationManifestStore
	disk1    string
	src      string
	plan     cache.RebalancePlan
	share    cache.Share
}

func newEvacuationOwnershipHarness(t *testing.T) *evacuationOwnershipHarness {
	t.Helper()
	ctx := context.Background()
	db := newTestDB(t)
	base := t.TempDir()
	disk1 := filepath.Join(base, "disk1")
	disk2 := filepath.Join(base, "disk2")
	arrays := hoservastore.NewArrayStore(db)
	if err := arrays.PutArray(ctx, hoservastore.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "1M", CreatedAt: time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)}, []hoservastore.ArrayDisk{
		{Role: hoservastore.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d1", Mountpoint: disk1},
		{Role: hoservastore.ArrayRoleData, RoleIndex: 2, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "uuid-d2", Mountpoint: disk2},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	src := filepath.Join(disk1, "media")
	plan := newRebalanceTestPlan(t, src, filepath.Join(disk2, "media"), "media", "movie.mkv", "movie bytes")
	return &evacuationOwnershipHarness{
		s:        NewScheduler(NewStore(db), NewLogStore(t.TempDir()), NewHub(), NewRegistry()),
		arrays:   arrays,
		manifest: parity.NewRelocationManifestStore(db),
		disk1:    disk1,
		src:      src,
		plan:     plan,
		share:    cache.Share{Name: "media", Branches: []string{src}},
	}
}

// register binds TypeEvacuation and its abort the way cmd/hoservad's
// parityRegistrar.register does, over h's real stores.
func (h *evacuationOwnershipHarness) register(sync EvacuationSyncFunc, arrayReady func(context.Context) error) {
	h.s.registry.Register(TypeEvacuation, true, RunEvacuation(EvacuationDeps{
		Sync:             sync,
		TrackedFileCount: func(context.Context) (int, error) { return 1000, nil },
		Shares:           func(context.Context) ([]cache.Share, error) { return []cache.Share{h.share}, nil },
		Manifest:         h.manifest,
		Store:            h.arrays,
		ArrayReady:       arrayReady,
	}))
	h.s.registry.RegisterAbort(TypeEvacuation, EvacuationAbort(h.manifest, h.arrays, arrayReady))
}

func (h *evacuationOwnershipHarness) submit(t *testing.T) (*Job, error) {
	t.Helper()
	return h.s.Submit(context.Background(), TypeEvacuation, nil, mustJSON(t, EvacuationParams{Mountpoint: h.disk1, Plan: h.plan}))
}

// removal returns disk1's persisted removal state and the job holding it.
func (h *evacuationOwnershipHarness) removal(t *testing.T) (state, holder string) {
	t.Helper()
	_, disks, err := h.arrays.GetArray(context.Background())
	if err != nil {
		t.Fatalf("GetArray: %v", err)
	}
	for _, d := range disks {
		if d.Mountpoint == h.disk1 {
			return d.RemovalState, d.RemovalJobID
		}
	}
	t.Fatalf("disk1 %s not in the array", h.disk1)
	return "", ""
}

type countingHook struct {
	calls atomic.Int32
	// onCall, when set, runs inside the hook with the 1-based call number.
	onCall func(ctx context.Context, n int32) error
}

func (c *countingHook) hook(ctx context.Context) error {
	n := c.calls.Add(1)
	if c.onCall != nil {
		return c.onCall(ctx, n)
	}
	return nil
}

func okSync(context.Context, []parity.ManifestEntry, map[string]bool) error { return nil }

// TestEvacuationCancel_InterruptedJob_ReleasesItsRemovalState is finding
// 2(a): an evacuation interrupted before any relocation manifest was ever
// persisted (stopped during its pre-copy step, so batch 1's copy never
// completed), and one whose persisted manifest a share relocation's
// Replace(nil, nil) has since cleared, are both cancelled — and both
// must release the removal state their job holds and re-apply the pool,
// whatever the manifest slot says.
func TestEvacuationCancel_InterruptedJob_ReleasesItsRemovalState(t *testing.T) {
	for _, tc := range []struct {
		name string
		// stopDuringArrayReady stops the job gracefully inside its first
		// ArrayReady call, before any copy; otherwise it stops inside the
		// first batch's pre-delete sync, once that batch's manifest is
		// persisted.
		stopDuringArrayReady bool
		// clearManifest runs a share relocation's own clear while the
		// evacuation sits interrupted.
		clearManifest bool
	}{
		{name: "no manifest persisted yet", stopDuringArrayReady: true},
		{name: "manifest cleared by another job", clearManifest: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			h := newEvacuationOwnershipHarness(t)
			ready := &countingHook{}
			if tc.stopDuringArrayReady {
				ready.onCall = func(ctx context.Context, n int32) error {
					if n == 1 {
						return h.s.EnterMaintenance(ctx)
					}
					return nil
				}
			}
			sync := func(ctx context.Context, m []parity.ManifestEntry, rd map[string]bool) error {
				if !tc.stopDuringArrayReady {
					if err := h.s.EnterMaintenance(ctx); err != nil {
						return err
					}
				}
				return nil
			}
			h.register(sync, ready.hook)

			j, err := h.submit(t)
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}
			if got := await(t, h.s, j.ID); got.Status != StatusInterrupted {
				t.Fatalf("status = %s (%s), want interrupted", got.Status, got.ErrorMessage)
			}
			if state, holder := h.removal(t); state != hoservastore.RemovalStateEvacuating || holder != j.ID {
				t.Fatalf("removal state after the interrupt = (%q, %q), want (%q, %q)", state, holder, hoservastore.RemovalStateEvacuating, j.ID)
			}
			_, removing, err := h.manifest.Current(ctx)
			if err != nil {
				t.Fatalf("manifest.Current: %v", err)
			}
			if tc.stopDuringArrayReady && removing != nil {
				t.Fatalf("removing-disks set = %v, want none persisted — the job stopped before any copy", removing)
			}
			if tc.clearManifest {
				if !removing[h.disk1] {
					t.Fatalf("removing-disks set = %v, want the batch's own manifest persisted before the interrupt", removing)
				}
				if err := h.manifest.Replace(ctx, nil, nil); err != nil {
					t.Fatalf("Replace(nil, nil): %v", err)
				}
			}
			h.s.ExitMaintenance()

			callsBefore := ready.calls.Load()
			if _, err := h.s.Cancel(ctx, j.ID); err != nil {
				t.Fatalf("Cancel: %v", err)
			}
			if got := await(t, h.s, j.ID); got.Status != StatusCancelled {
				t.Fatalf("status after Cancel = %s (%s), want cancelled", got.Status, got.ErrorMessage)
			}
			if state, holder := h.removal(t); state != "" || holder != "" {
				t.Fatalf("removal state after cancelling the job that holds it = (%q, %q), want released", state, holder)
			}
			if ready.calls.Load() != callsBefore+1 {
				t.Fatalf("ArrayReady calls during Cancel = %d, want 1 — the pool must be re-applied once the disk takes writes again", ready.calls.Load()-callsBefore)
			}
			if _, err := os.Stat(filepath.Join(h.src, "movie.mkv")); err != nil {
				t.Fatalf("source must survive a cancelled evacuation: %v", err)
			}
		})
	}
}

// TestEvacuationCancel_LeavesAStateAnotherJobHolds is finding 2(b) at the
// abort: evacuation A is interrupted, then another evacuation run takes
// the same disk over (it holds the state now). Cancelling A must neither
// release that state nor re-apply the pool.
func TestEvacuationCancel_LeavesAStateAnotherJobHolds(t *testing.T) {
	ctx := context.Background()
	h := newEvacuationOwnershipHarness(t)
	ready := &countingHook{onCall: func(ctx context.Context, n int32) error {
		if n == 1 {
			return h.s.EnterMaintenance(ctx)
		}
		return nil
	}}
	h.register(okSync, ready.hook)

	a, err := h.submit(t)
	if err != nil {
		t.Fatalf("Submit(A): %v", err)
	}
	if got := await(t, h.s, a.ID); got.Status != StatusInterrupted {
		t.Fatalf("A status = %s (%s), want interrupted", got.Status, got.ErrorMessage)
	}
	h.s.ExitMaintenance()

	if err := h.arrays.SetRemovalState(ctx, h.disk1, hoservastore.RemovalStateEvacuating, "job-b"); err != nil {
		t.Fatalf("SetRemovalState(job-b): %v", err)
	}
	callsBefore := ready.calls.Load()
	if _, err := h.s.Cancel(ctx, a.ID); err != nil {
		t.Fatalf("Cancel(A): %v", err)
	}
	if state, holder := h.removal(t); state != hoservastore.RemovalStateEvacuating || holder != "job-b" {
		t.Fatalf("removal state after cancelling A = (%q, %q), want job-b's (%q, job-b) left alone", state, holder, hoservastore.RemovalStateEvacuating)
	}
	if ready.calls.Load() != callsBefore {
		t.Fatalf("ArrayReady called %d time(s) by A's cancel, want 0 — the disk must stay no-create for job-b", ready.calls.Load()-callsBefore)
	}
}

// TestSubmitEvacuation_RefusedWhileAnotherIsPending is finding 2(b) at
// admission: while an evacuation is queued, running or interrupted, no
// second one — same disk or any other — is admitted.
func TestSubmitEvacuation_RefusedWhileAnotherIsPending(t *testing.T) {
	ctx := context.Background()
	h := newEvacuationOwnershipHarness(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	ready := &countingHook{onCall: func(ctx context.Context, n int32) error {
		if n == 1 {
			close(entered)
			<-release
		}
		return nil
	}}
	h.register(okSync, ready.hook)
	rebalanceStarted, rebalanceRelease := registerBlocking(h.s, TypeRebalance, true)

	// Queued: an array-write rebalance is running, so the evacuation
	// queues behind it.
	rb, err := h.s.Submit(ctx, TypeRebalance, nil, mustJSON(t, RebalanceParams{}))
	if err != nil {
		t.Fatalf("Submit(rebalance): %v", err)
	}
	<-rebalanceStarted
	queued, err := h.submit(t)
	if err != nil {
		t.Fatalf("Submit(evacuation): %v", err)
	}
	if queued.Status != StatusQueued {
		t.Fatalf("first evacuation status = %s, want queued behind the rebalance", queued.Status)
	}
	if _, err := h.submit(t); !errors.Is(err, ErrEvacuationPending) {
		t.Fatalf("second Submit while one is queued = %v, want ErrEvacuationPending", err)
	}

	// Running: the evacuation starts once the rebalance ends, and sits
	// inside its pre-copy ArrayReady.
	close(rebalanceRelease)
	waitSucceeded(t, h.s, rb.ID)
	<-entered
	if _, err := h.submit(t); !errors.Is(err, ErrEvacuationPending) {
		t.Fatalf("second Submit while one is running = %v, want ErrEvacuationPending", err)
	}
	other := mustJSON(t, EvacuationParams{Mountpoint: filepath.Join(filepath.Dir(h.disk1), "disk2"), Plan: cache.RebalancePlan{}})
	if _, err := h.s.Submit(ctx, TypeEvacuation, nil, other); !errors.Is(err, ErrEvacuationPending) {
		t.Fatalf("Submit for another disk while one is running = %v, want ErrEvacuationPending", err)
	}

	// Interrupted: a graceful stop while it runs.
	if err := h.s.EnterMaintenance(ctx); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}
	close(release)
	if got := await(t, h.s, queued.ID); got.Status != StatusInterrupted {
		t.Fatalf("status = %s (%s), want interrupted", got.Status, got.ErrorMessage)
	}
	h.s.ExitMaintenance()
	if _, err := h.submit(t); !errors.Is(err, ErrEvacuationPending) {
		t.Fatalf("second Submit while one is interrupted = %v, want ErrEvacuationPending", err)
	}

	// Once it is cancelled, the disk can be evacuated again.
	if _, err := h.s.Cancel(ctx, queued.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	again, err := h.submit(t)
	if err != nil {
		t.Fatalf("Submit after the pending evacuation was cancelled: %v", err)
	}
	if got := await(t, h.s, again.ID); got.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", got.Status, got.ErrorMessage)
	}
}

// TestEvacuationCancel_DuringPreCopyArrayReady_ReleasesTheState is
// finding 2(c): a Cancel that lands while the job is still applying
// no-create to the live pool, before any copy, still releases the state
// this job set and re-applies the pool.
func TestEvacuationCancel_DuringPreCopyArrayReady_ReleasesTheState(t *testing.T) {
	ctx := context.Background()
	h := newEvacuationOwnershipHarness(t)
	entered := make(chan struct{})
	ready := &countingHook{onCall: func(ctx context.Context, n int32) error {
		if n == 1 {
			close(entered)
			<-ctx.Done()
			return ctx.Err()
		}
		if ctx.Err() != nil {
			return fmt.Errorf("re-apply ran with a cancelled context: %w", ctx.Err())
		}
		return nil
	}}
	var syncCalls atomic.Int32
	h.register(func(context.Context, []parity.ManifestEntry, map[string]bool) error {
		syncCalls.Add(1)
		return nil
	}, ready.hook)

	j, err := h.submit(t)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	<-entered
	if state, holder := h.removal(t); state != hoservastore.RemovalStateEvacuating || holder != j.ID {
		t.Fatalf("removal state inside the pre-copy ArrayReady = (%q, %q), want (%q, %q)", state, holder, hoservastore.RemovalStateEvacuating, j.ID)
	}
	if _, err := h.s.Cancel(ctx, j.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	got := await(t, h.s, j.ID)
	if got.Status != StatusCancelled {
		t.Fatalf("status = %s (%s), want cancelled", got.Status, got.ErrorMessage)
	}
	if state, holder := h.removal(t); state != "" || holder != "" {
		t.Fatalf("removal state after a cancel during the pre-copy ArrayReady = (%q, %q), want released", state, holder)
	}
	if ready.calls.Load() != 2 {
		t.Fatalf("ArrayReady calls = %d, want 2 (the interrupted pre-copy apply, then the re-apply after release)", ready.calls.Load())
	}
	if syncCalls.Load() != 0 {
		t.Fatalf("Sync called %d time(s), want 0 — nothing is copied after a cancel before the first copy", syncCalls.Load())
	}
}

// TestEvacuation_FailedThenRerun_NewJobTakesTheStateOver proves the way
// out of a plain failure, which leaves the disk "evacuating": once the
// failed job is terminal a new evacuation of the same disk is admitted,
// and it holds the state from then on.
func TestEvacuation_FailedThenRerun_NewJobTakesTheStateOver(t *testing.T) {
	h := newEvacuationOwnershipHarness(t)
	var mu sync.Mutex
	failNext := true
	h.register(func(context.Context, []parity.ManifestEntry, map[string]bool) error {
		mu.Lock()
		defer mu.Unlock()
		if failNext {
			failNext = false
			return fmt.Errorf("synthetic sync failure")
		}
		return nil
	}, (&countingHook{}).hook)

	failed, err := h.submit(t)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if got := await(t, h.s, failed.ID); got.Status != StatusFailed {
		t.Fatalf("status = %s (%s), want failed", got.Status, got.ErrorMessage)
	}
	if state, holder := h.removal(t); state != hoservastore.RemovalStateEvacuating || holder != failed.ID {
		t.Fatalf("removal state after a plain failure = (%q, %q), want (%q, %q)", state, holder, hoservastore.RemovalStateEvacuating, failed.ID)
	}

	rerun, err := h.submit(t)
	if err != nil {
		t.Fatalf("Submit after the failed job ended: %v", err)
	}
	if got := await(t, h.s, rerun.ID); got.Status != StatusSucceeded {
		t.Fatalf("rerun status = %s (%s), want succeeded", got.Status, got.ErrorMessage)
	}
	if state, holder := h.removal(t); state != hoservastore.RemovalStateEvacuated || holder != rerun.ID {
		t.Fatalf("removal state after the rerun = (%q, %q), want (%q, %q)", state, holder, hoservastore.RemovalStateEvacuated, rerun.ID)
	}
}
