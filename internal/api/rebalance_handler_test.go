package api_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/cache"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// rebalanceTestShare writes one file under a temp "disk1" share branch
// and returns the cache.Share PlanRebalance/PlanEvacuation see (doc 09
// §3-4), plus its own two data-disk mountpoints, matching the shape
// cmd/hoservad's own rebalanceSharesFromStore builds.
func rebalanceTestShare(t *testing.T) (share cache.Share, disk1, disk2 string) {
	t.Helper()
	base := t.TempDir()
	disk1 = filepath.Join(base, "disk1")
	disk2 = filepath.Join(base, "disk2")
	src := filepath.Join(disk1, "media")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "movie.mkv"), []byte("movie bytes"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(disk2, "media"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	share = cache.Share{Name: "media", Branches: []string{src, filepath.Join(disk2, "media")}}
	return share, disk1, disk2
}

// newRebalanceTestHandler wires a *api.Handler to a real SQLite database
// and job.Scheduler (newTestHandler's own shape, handler_test.go), plus
// ArrayStore (seeded with two data-disk slots) and RebalanceShares
// (doc 09 §3-4, #274), and registers real job.TypeRebalance/
// job.TypeEvacuation RunFuncs against a no-op Sync — these tests are
// about the handler's own plan/confirm/submit wiring, not the threshold
// guard (already covered by internal/job's own
// TestRunRebalance_GuardBlocked_LeavesSourceUntouched and the lab).
func newRebalanceTestHandler(t *testing.T) (h *api.Handler, scheduler *job.Scheduler, share cache.Share, disk1, disk2 string) {
	t.Helper()
	share, disk1, disk2 = rebalanceTestShare(t)

	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "rebalance-api-test.db")
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}

	arrayStore := store.NewArrayStore(db)
	if err := arrayStore.PutArray(context.Background(), store.ArraySettings{
		CreatePolicy: "mspmfs", MinFreeSpace: "10G", CreatedAt: time.Now().UTC(),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/loop0", Filesystem: "xfs", FSUUID: "uuid-d1", Mountpoint: disk1},
		{Role: store.ArrayRoleData, RoleIndex: 2, Device: "/dev/loop1", Filesystem: "xfs", FSUUID: "uuid-d2", Mountpoint: disk2},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}

	jobStore := job.NewStore(db)
	logs := job.NewLogStore(t.TempDir())
	registry := job.NewRegistry()
	scheduler = job.NewScheduler(jobStore, logs, job.NewHub(), registry)

	sharesFn := func(context.Context) ([]cache.Share, error) { return []cache.Share{share}, nil }
	noopSync := func(context.Context, []parity.ManifestEntry) error { return nil }
	noopEvacuationSync := func(context.Context, []parity.ManifestEntry, map[string]bool) error { return nil }
	trackedCount := func(context.Context) (int, error) { return 1000, nil }
	registry.Register(job.TypeRebalance, true, job.RunRebalance(job.RebalanceDeps{
		Sync:             noopSync,
		TrackedFileCount: trackedCount,
	}))
	registry.Register(job.TypeEvacuation, true, job.RunEvacuation(job.EvacuationDeps{
		Sync:             noopEvacuationSync,
		TrackedFileCount: trackedCount,
		Shares:           sharesFn,
		Store:            arrayStore,
		// No live pool is mounted in this handler-level test (its own
		// TestHandler_PlanDiskEvacuation_ThenEvacuateDisk_RunsThroughRealJob
		// doc comment: only the API-layer wiring, not a real mount) — a
		// no-op stands in the way newRebalanceTestHandler's own noopSync
		// stands in for a real snapraid sync.
		ArrayReady: func(context.Context) error { return nil },
	}))

	h = &api.Handler{
		Scheduler:       scheduler,
		Store:           jobStore,
		Logs:            logs,
		ArrayStore:      arrayStore,
		RebalanceShares: sharesFn,
	}
	return h, scheduler, share, disk1, disk2
}

// TestHandler_PlanRebalance_ThenStartRebalance_RunsThroughRealJob proves
// startRebalance's own confirmation gate and the job it queues both work
// through a real Scheduler.Submit/Await round trip — the API-layer half
// of #274's registry-wiring acceptance criterion (internal/job's own
// TestRunRebalance_MovesThroughScheduler, driven with a hand-built plan,
// proves the job-registry half moves a real file). PlanRebalance's own
// skew computation reads real per-branch free space (doc 09 §3), which
// two temp directories on the same filesystem never show — so this test
// legitimately sees an empty plan and, unlike the evacuation tests below,
// only proves the wiring, not a move.
func TestHandler_PlanRebalance_ThenStartRebalance_RunsThroughRealJob(t *testing.T) {
	ctx := context.Background()
	h, _, _, _, _ := newRebalanceTestHandler(t)

	plan, err := h.PlanRebalance(ctx)
	if err != nil {
		t.Fatalf("PlanRebalance: %v", err)
	}
	if plan.Confirmation != "REBALANCE" {
		t.Fatalf("PlanRebalance.Confirmation = %q, want REBALANCE", plan.Confirmation)
	}

	if _, err := h.StartRebalance(ctx, &apiv1.StartRebalanceRequest{Confirmation: "wrong"}); err == nil {
		t.Fatal("StartRebalance(wrong confirmation) = nil, want confirmation_required")
	} else if status := apiError(t, h, err); status.StatusCode != 409 || status.Response.Code != "confirmation_required" {
		t.Fatalf("StartRebalance(wrong confirmation) = %+v, want 409 confirmation_required", status)
	}

	j, err := h.StartRebalance(ctx, &apiv1.StartRebalanceRequest{Confirmation: plan.Confirmation})
	if err != nil {
		t.Fatalf("StartRebalance: %v", err)
	}
	if j.Type != apiv1.JobTypeRebalance {
		t.Fatalf("Job.Type = %s, want rebalance", j.Type)
	}
	waitForStatus(t, h.Store, j.ID.String(), job.StatusSucceeded)
}

// TestHandler_PlanDiskEvacuation_UnknownMountpointRefused proves an
// evacuation plan for a slot the array does not have is refused before
// anything is computed, matching PlanDiskReplace's own disk_slot_not_found
// shape.
func TestHandler_PlanDiskEvacuation_UnknownMountpointRefused(t *testing.T) {
	ctx := context.Background()
	h, _, _, _, _ := newRebalanceTestHandler(t)

	_, err := h.PlanDiskEvacuation(ctx, &apiv1.EvacuateDiskPlanRequest{Mountpoint: "/mnt/disk9"})
	if err == nil {
		t.Fatal("PlanDiskEvacuation(unknown mountpoint) = nil, want disk_slot_not_found")
	}
	if status := apiError(t, h, err); status.StatusCode != 404 || status.Response.Code != "disk_slot_not_found" {
		t.Fatalf("PlanDiskEvacuation(unknown mountpoint) = %+v, want 404 disk_slot_not_found", status)
	}
}

// TestHandler_EvacuateDisk_WrongConfirmationRefused proves a stale or
// forged confirmation phrase is refused before any job is submitted —
// the evacuation-specific half of startRebalance's own
// never-trust-a-client-supplied-plan reasoning.
func TestHandler_EvacuateDisk_WrongConfirmationRefused(t *testing.T) {
	ctx := context.Background()
	h, _, _, disk1, _ := newRebalanceTestHandler(t)

	_, err := h.EvacuateDisk(ctx, &apiv1.EvacuateDiskRequest{Mountpoint: disk1, Confirmation: "wrong"})
	if err == nil {
		t.Fatal("EvacuateDisk(wrong confirmation) = nil, want confirmation_required")
	}
	if status := apiError(t, h, err); status.StatusCode != 409 || status.Response.Code != "confirmation_required" {
		t.Fatalf("EvacuateDisk(wrong confirmation) = %+v, want 409 confirmation_required", status)
	}
	jobs, err := h.Store.List(ctx, job.ListFilter{})
	if err != nil {
		t.Fatalf("listing jobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("a wrong confirmation must never submit a job, got %d", len(jobs))
	}
}

// TestHandler_PlanDiskEvacuation_RefusesWhileAnotherDiskIsRemoving proves
// #359's own 409 refusal: while a different disk is already in removal,
// planDiskEvacuation must refuse before computing anything, naming the
// disk that is already removing. Re-planning the disk that is already in
// removal — the "resume" case — must not be refused.
func TestHandler_PlanDiskEvacuation_RefusesWhileAnotherDiskIsRemoving(t *testing.T) {
	ctx := context.Background()
	h, _, _, disk1, disk2 := newRebalanceTestHandler(t)

	if err := h.ArrayStore.SetRemovalState(ctx, disk1, store.RemovalStateEvacuating, "job-1"); err != nil {
		t.Fatalf("SetRemovalState: %v", err)
	}

	_, err := h.PlanDiskEvacuation(ctx, &apiv1.EvacuateDiskPlanRequest{Mountpoint: disk2})
	if err == nil {
		t.Fatal("PlanDiskEvacuation(a second disk) = nil, want disk_removal_in_progress")
	}
	if status := apiError(t, h, err); status.StatusCode != 409 || status.Response.Code != "disk_removal_in_progress" {
		t.Fatalf("PlanDiskEvacuation(a second disk) = %+v, want 409 disk_removal_in_progress", status)
	}

	// Re-planning disk1 itself — already the one removing — must not be
	// refused by this same check.
	if _, err := h.PlanDiskEvacuation(ctx, &apiv1.EvacuateDiskPlanRequest{Mountpoint: disk1}); err != nil {
		t.Fatalf("PlanDiskEvacuation(the disk already removing) = %v, want no refusal", err)
	}
}

// TestHandler_EvacuateDisk_RefusesWhileAnotherDiskIsRemoving is the
// evacuateDisk half of the same refusal.
func TestHandler_EvacuateDisk_RefusesWhileAnotherDiskIsRemoving(t *testing.T) {
	ctx := context.Background()
	h, _, _, disk1, disk2 := newRebalanceTestHandler(t)

	if err := h.ArrayStore.SetRemovalState(ctx, disk1, store.RemovalStateEvacuating, "job-1"); err != nil {
		t.Fatalf("SetRemovalState: %v", err)
	}

	_, err := h.EvacuateDisk(ctx, &apiv1.EvacuateDiskRequest{Mountpoint: disk2, Confirmation: job.EvacuationConfirmation(disk2)})
	if err == nil {
		t.Fatal("EvacuateDisk(a second disk) = nil, want disk_removal_in_progress")
	}
	if status := apiError(t, h, err); status.StatusCode != 409 || status.Response.Code != "disk_removal_in_progress" {
		t.Fatalf("EvacuateDisk(a second disk) = %+v, want 409 disk_removal_in_progress", status)
	}
	jobs, err := h.Store.List(ctx, job.ListFilter{})
	if err != nil {
		t.Fatalf("listing jobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("a refused second disk must never submit a job, got %d", len(jobs))
	}
}

// TestHandler_EvacuateDisk_RefusedWhileAnEvacuationIsPending proves
// evacuateDisk surfaces job.Scheduler's evacuation admission (#359) as a
// 409 evacuation_pending, submitting nothing, while an earlier evacuation
// of the same disk sits interrupted — and accepts the disk again once that
// job is cancelled.
func TestHandler_EvacuateDisk_RefusedWhileAnEvacuationIsPending(t *testing.T) {
	ctx := context.Background()
	h, scheduler, _, disk1, _ := newRebalanceTestHandler(t)

	finished := time.Now().UTC()
	interrupted := &job.Job{
		ID:          "11111111-1111-1111-1111-111111111111",
		Type:        job.TypeEvacuation,
		Class:       job.ClassArrayWrite,
		Status:      job.StatusInterrupted,
		Resumable:   true,
		Cancellable: true,
		Params:      []byte(`{"mountpoint":"` + disk1 + `","plan":{"moves":[]}}`),
		CreatedAt:   finished,
		FinishedAt:  &finished,
	}
	if err := h.Store.Create(ctx, interrupted); err != nil {
		t.Fatalf("creating the interrupted evacuation: %v", err)
	}

	req := &apiv1.EvacuateDiskRequest{Mountpoint: disk1, Confirmation: job.EvacuationConfirmation(disk1)}
	_, err := h.EvacuateDisk(ctx, req)
	if status := apiError(t, h, err); status.StatusCode != 409 || status.Response.Code != "evacuation_pending" {
		t.Fatalf("EvacuateDisk while an evacuation is interrupted = %+v, want 409 evacuation_pending", status)
	}
	jobs, err := h.Store.List(ctx, job.ListFilter{})
	if err != nil {
		t.Fatalf("listing jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("a refused evacuation must never submit a job, got %d jobs", len(jobs))
	}

	if _, err := scheduler.Cancel(ctx, interrupted.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	j, err := h.EvacuateDisk(ctx, req)
	if err != nil {
		t.Fatalf("EvacuateDisk after the interrupted one was cancelled: %v", err)
	}
	waitForStatus(t, h.Store, j.ID.String(), job.StatusSucceeded)
}

// TestHandler_PlanDiskEvacuation_IOErrorIsInternal is this round's own
// regression test for the finding that blocked #274's first attempt: an
// I/O error while cache.PlanEvacuation walks the disk being evacuated (a
// real EACCES here, standing in for the lab's own EIO reading a failing
// disk) must be reported as an opaque internal error, never invalid_plan
// — invalid_plan tells the caller to fix its own plan, and there is
// nothing wrong with this one.
func TestHandler_PlanDiskEvacuation_IOErrorIsInternal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses the permission check this test relies on")
	}
	ctx := context.Background()
	h, _, share, disk1, _ := newRebalanceTestHandler(t)

	unreadable := share.Branches[0]
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o755) })

	_, err := h.PlanDiskEvacuation(ctx, &apiv1.EvacuateDiskPlanRequest{Mountpoint: disk1})
	if err == nil {
		t.Fatal("PlanDiskEvacuation(unreadable branch) = nil, want an error")
	}
	status := apiError(t, h, err)
	if status.StatusCode != 500 || status.Response.Code != "internal" {
		t.Fatalf("PlanDiskEvacuation(unreadable branch) = %+v, want an opaque 500 internal error, not invalid_plan", status)
	}
}

// TestHandler_PlanDiskEvacuation_ThenEvacuateDisk_RunsThroughRealJob
// proves evacuateDisk's own confirmation gate and the job it queues
// actually evacuate the disk planDiskEvacuation previewed — the
// API-layer half of #274's registry-wiring acceptance criterion
// (internal/job's own
// TestRunEvacuation_MovesThroughScheduler_PostCheckPasses proves the
// job-registry half).
func TestHandler_PlanDiskEvacuation_ThenEvacuateDisk_RunsThroughRealJob(t *testing.T) {
	ctx := context.Background()
	h, _, share, disk1, _ := newRebalanceTestHandler(t)

	plan, err := h.PlanDiskEvacuation(ctx, &apiv1.EvacuateDiskPlanRequest{Mountpoint: disk1})
	if err != nil {
		t.Fatalf("PlanDiskEvacuation: %v", err)
	}
	wantConfirmation := "REMOVE " + disk1
	if len(plan.Moves) != 1 || plan.Confirmation != wantConfirmation {
		t.Fatalf("PlanDiskEvacuation = %+v, want one move and confirmation %q", plan, wantConfirmation)
	}

	j, err := h.EvacuateDisk(ctx, &apiv1.EvacuateDiskRequest{Mountpoint: disk1, Confirmation: plan.Confirmation})
	if err != nil {
		t.Fatalf("EvacuateDisk: %v", err)
	}
	if j.Type != apiv1.JobTypeEvacuation {
		t.Fatalf("Job.Type = %s, want evacuation", j.Type)
	}
	waitForStatus(t, h.Store, j.ID.String(), job.StatusSucceeded)

	entries, err := os.ReadDir(share.Branches[0])
	if err != nil {
		t.Fatalf("reading evacuated share branch: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("evacuated share branch still has entries = %+v, want empty (EvacuationPostCheck should have run and passed)", entries)
	}
}

// markRemoval puts the data disk at mountpoint into removal state state
// through the same store calls the evacuation job (SetRemovalState) and
// the disk_remove job (AdvanceRemovalState) make.
func markRemoval(t *testing.T, st *store.ArrayStore, mountpoint, state string) {
	t.Helper()
	ctx := context.Background()
	switch state {
	case store.RemovalStateEvacuating, store.RemovalStateEvacuated:
		if err := st.SetRemovalState(ctx, mountpoint, state, "evacuation-job"); err != nil {
			t.Fatalf("marking %s %s: %v", mountpoint, state, err)
		}
		return
	}
	if err := st.SetRemovalState(ctx, mountpoint, store.RemovalStateEvacuated, "evacuation-job"); err != nil {
		t.Fatalf("marking %s evacuated: %v", mountpoint, err)
	}
	if err := st.AdvanceRemovalState(ctx, mountpoint, store.RemovalStateEvacuated, store.RemovalStateUnpooled, "disk-remove-job"); err != nil {
		t.Fatalf("marking %s unpooled: %v", mountpoint, err)
	}
	if state == store.RemovalStateUnlisted {
		if err := st.AdvanceRemovalState(ctx, mountpoint, store.RemovalStateUnpooled, store.RemovalStateUnlisted, "disk-remove-job"); err != nil {
			t.Fatalf("marking %s unlisted: %v", mountpoint, err)
		}
	}
}

// TestHandler_Evacuation_RefusesADiskThatLeftThePool proves #366's
// synchronous refusal: planDiskEvacuation and evacuateDisk answer 409
// disk_leaving_array for a disk already unpooled or unlisted, and no job
// is queued.
func TestHandler_Evacuation_RefusesADiskThatLeftThePool(t *testing.T) {
	for _, state := range []string{store.RemovalStateUnpooled, store.RemovalStateUnlisted} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			h, _, _, disk1, _ := newRebalanceTestHandler(t)
			markRemoval(t, h.ArrayStore, disk1, state)

			_, err := h.PlanDiskEvacuation(ctx, &apiv1.EvacuateDiskPlanRequest{Mountpoint: disk1})
			if status := apiError(t, h, err); status.StatusCode != 409 || status.Response.Code != "disk_leaving_array" {
				t.Fatalf("PlanDiskEvacuation(%s disk) = %+v, want 409 disk_leaving_array", state, status)
			}
			_, err = h.EvacuateDisk(ctx, &apiv1.EvacuateDiskRequest{Mountpoint: disk1, Confirmation: job.EvacuationConfirmation(disk1)})
			if status := apiError(t, h, err); status.StatusCode != 409 || status.Response.Code != "disk_leaving_array" {
				t.Fatalf("EvacuateDisk(%s disk) = %+v, want 409 disk_leaving_array", state, status)
			}
			jobs, err := h.Store.List(ctx, job.ListFilter{})
			if err != nil {
				t.Fatalf("listing jobs: %v", err)
			}
			if len(jobs) != 0 {
				t.Fatalf("a refused evacuation submitted %d job(s), want none", len(jobs))
			}
		})
	}
}

// TestHandler_PlanDiskEvacuation_KeepsItsOwnDiskInRemoval proves that
// leaving disks in removal out of the plan never leaves out the disk
// being evacuated: re-planning an "evacuating" or "evacuated" disk still
// plans its file off it.
func TestHandler_PlanDiskEvacuation_KeepsItsOwnDiskInRemoval(t *testing.T) {
	for _, state := range []string{store.RemovalStateEvacuating, store.RemovalStateEvacuated} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			h, _, share, disk1, _ := newRebalanceTestHandler(t)
			markRemoval(t, h.ArrayStore, disk1, state)

			plan, err := h.PlanDiskEvacuation(ctx, &apiv1.EvacuateDiskPlanRequest{Mountpoint: disk1})
			if err != nil {
				t.Fatalf("PlanDiskEvacuation(%s disk): %v", state, err)
			}
			if len(plan.Moves) != 1 || plan.Moves[0].SourceBranch != share.Branches[0] || plan.Moves[0].TargetBranch != share.Branches[1] {
				t.Fatalf("PlanDiskEvacuation(%s disk) moves = %+v, want movie.mkv from %s to %s", state, plan.Moves, share.Branches[0], share.Branches[1])
			}
		})
	}
}
