package api_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// newDiskUpgradeHandler builds a *api.Handler with a real ArrayStore and
// job.Scheduler sharing one SQLite database, the way main.go builds it:
// both upgrade job types registered, the data-disk upgrade's RunFunc and
// AbortFunc sharing one job.DiskUpgradeDataDeps whose array sequence is
// the handler's own. job.FakeMountTable stands in for disk.KernelMounts;
// staging overrides the data-disk upgrade's staging path, since the copy
// really walks and writes directories.
func newDiskUpgradeHandler(t *testing.T, staging string) (*api.Handler, *job.Scheduler, *disk.FakeProvider, *store.ArrayStore, *parity.FakeEngine, *disk.FakeRunner) {
	t.Helper()

	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "disk-upgrade-test.db")
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}

	jobStore := job.NewStore(db)
	logs := job.NewLogStore(t.TempDir())
	registry := job.NewRegistry()
	scheduler := job.NewScheduler(jobStore, logs, job.NewHub(), registry)
	arrayStore := store.NewArrayStore(db)

	p := disk.NewFakeProvider()
	fakeRun := disk.NewFakeRunner()
	genRoot := t.TempDir()
	eng := parity.NewFakeEngine()
	eng.SetDiff(parity.DiffReport{})
	eng.ScriptCheck([]parity.Progress{{Percent: 100}}, nil)

	h := &api.Handler{
		Scheduler:  scheduler,
		Store:      jobStore,
		Logs:       logs,
		Disks:      p,
		ArrayStore: arrayStore,
	}
	h.SetArray(&job.ArraySequence{Scheduler: scheduler})

	upgradeDataDeps := job.DiskUpgradeDataDeps{
		Provider:    p,
		Runner:      fakeRun,
		Store:       arrayStore,
		Generator:   config.NewGenerator(genRoot),
		Parity:      eng,
		Mounts:      job.NewFakeMountTable(),
		Array:       h.CurrentArray,
		Now:         func() time.Time { return time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC) },
		StagingPath: func(string) string { return staging },
		Sleep:       func(time.Duration) {},
	}
	registry.Register(job.TypeDiskUpgradeData, true, job.RunDiskUpgradeData(upgradeDataDeps))
	registry.RegisterAbort(job.TypeDiskUpgradeData, job.AbortDiskUpgradeData(upgradeDataDeps))
	mounter := disk.NewFakeMounter()
	registry.Register(job.TypeDiskUpgradeParity, true, job.RunDiskUpgradeParity(job.DiskUpgradeParityDeps{
		Provider:       p,
		Runner:         fakeRun,
		Store:          arrayStore,
		Generator:      config.NewGenerator(genRoot),
		Mounter:        mounter,
		UpgradeMounter: mounter,
		Parity:         eng,
		Now:            func() time.Time { return time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC) },
	}))
	return h, scheduler, p, arrayStore, eng, fakeRun
}

// seedUpgradeArray persists parity1, the slot being upgraded at oldWhere
// and disk2, and lists every disk plus the replacement /dev/sdz.
func seedUpgradeArray(t *testing.T, st *store.ArrayStore, p *disk.FakeProvider, oldWhere string, replacementSize int64) {
	t.Helper()
	p.AddDisk("/dev/sda", disk.Disk{Size: 16 * disk.TB})
	p.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB})
	p.AddDisk("/dev/sdc", disk.Disk{Size: 4 * disk.TB})
	p.AddDisk("/dev/sdz", disk.Disk{Size: replacementSize})
	if err := st.PutArray(context.Background(), store.ArraySettings{
		CreatePolicy: "mfs", MinFreeSpace: "20G", CreatedAt: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", Mountpoint: "/mnt/parity1"},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d1", Mountpoint: oldWhere},
		{Role: store.ArrayRoleData, RoleIndex: 2, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "uuid-d2", Mountpoint: "/mnt/disk2"},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
}

// TestHandler_UpgradeDisk_Data_Success_StartsJobAndSwitchesTopology: the
// plan comes first and names the steps; upgradeDisk, after `array stop`,
// starts the registered job, which runs to Done and names B for the slot
// (doc 02 §4 E1, E8).
func TestHandler_UpgradeDisk_Data_Success_StartsJobAndSwitchesTopology(t *testing.T) {
	ctx := context.Background()
	oldWhere := t.TempDir()
	staging := t.TempDir()
	h, s, p, st, _, r := newDiskUpgradeHandler(t, staging)
	if err := os.WriteFile(filepath.Join(oldWhere, "keep.bin"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("seed old disk file: %v", err)
	}
	seedUpgradeArray(t, st, p, oldWhere, 10*disk.TB)
	scriptUUID(r, "/dev/sdz", "uuid-new")

	plan, err := h.PlanDiskUpgrade(ctx, &apiv1.DiskUpgradePlanRequest{Mountpoint: oldWhere, Device: "/dev/sdz"})
	if err != nil {
		t.Fatalf("PlanDiskUpgrade: %v", err)
	}
	if plan.Role != apiv1.ArrayDiskRoleData || plan.PreviousDevice != "/dev/sdb" || plan.ReplacementDevice != "/dev/sdz" || len(plan.Steps) == 0 {
		t.Fatalf("plan = %+v, want the data slot's old and new disks and its steps", plan)
	}

	if _, err := h.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
		t.Fatalf("StopArray: %v", err)
	}
	got, err := h.UpgradeDisk(ctx, &apiv1.UpgradeDiskRequest{Mountpoint: oldWhere, Device: "/dev/sdz", Confirmation: plan.Confirmation})
	if err != nil {
		t.Fatalf("UpgradeDisk: %v", err)
	}
	if got.Type != apiv1.JobTypeDiskUpgradeData || got.Class != apiv1.JobClassTopology {
		t.Fatalf("job type/class = %s/%s, want disk_upgrade_data/topology", got.Type, got.Class)
	}
	finished := awaitJob(t, s, got.ID.String())
	if finished.Status != job.StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	switched, err := st.GetDataDiskByMountpoint(ctx, oldWhere)
	if err != nil {
		t.Fatalf("GetDataDiskByMountpoint(%s): %v", oldWhere, err)
	}
	if switched.Device != "/dev/sdz" || switched.FSUUID != "uuid-new" {
		t.Fatalf("upgraded slot = %s/%s, want /dev/sdz/uuid-new", switched.Device, switched.FSUUID)
	}
}

// TestHandler_UpgradeDisk_Data_LiveArrayRefused: with the array started
// the upgrade is refused with array_not_stopped and never queued (E8).
func TestHandler_UpgradeDisk_Data_LiveArrayRefused(t *testing.T) {
	ctx := context.Background()
	oldWhere := t.TempDir()
	h, _, p, st, _, _ := newDiskUpgradeHandler(t, t.TempDir())
	seedUpgradeArray(t, st, p, oldWhere, 10*disk.TB)

	replacement := disk.AssignedDisk{Device: "/dev/sdz", Filesystem: disk.XFS}
	_, err := h.UpgradeDisk(ctx, &apiv1.UpgradeDiskRequest{Mountpoint: oldWhere, Device: "/dev/sdz", Confirmation: job.SingleDiskConfirmation(replacement)})
	status := apiError(t, h, err)
	if status.StatusCode != 409 || status.Response.Code != "array_not_stopped" {
		t.Fatalf("UpgradeDisk(data, live array) = %+v, want 409 array_not_stopped", status)
	}
	jobs, err := h.Store.List(ctx, job.ListFilter{})
	if err != nil || len(jobs) != 0 {
		t.Fatalf("jobs after the refusal = %v, %v; want none", jobs, err)
	}
}

// TestHandler_PlanDiskUpgrade_Data_ExceedingParityRefusedWithReason: a
// replacement larger than parity is refused at the plan, which says why
// (Q20).
func TestHandler_PlanDiskUpgrade_Data_ExceedingParityRefusedWithReason(t *testing.T) {
	ctx := context.Background()
	oldWhere := t.TempDir()
	h, _, p, st, _, _ := newDiskUpgradeHandler(t, t.TempDir())
	seedUpgradeArray(t, st, p, oldWhere, 20*disk.TB)

	_, err := h.PlanDiskUpgrade(ctx, &apiv1.DiskUpgradePlanRequest{Mountpoint: oldWhere, Device: "/dev/sdz"})
	status := apiError(t, h, err)
	if status.StatusCode != 400 || status.Response.Code != "invalid_plan" || !strings.Contains(status.Response.Message, "upgrade parity first") {
		t.Fatalf("PlanDiskUpgrade(larger than parity) = %+v, want 400 invalid_plan explaining the parity rule", status)
	}
}

// TestHandler_UpgradeDisk_Data_SecondUpgradeWhilePendingRefused: E8's
// pending row through the API: 409 disk_upgrade_pending naming the job.
func TestHandler_UpgradeDisk_Data_SecondUpgradeWhilePendingRefused(t *testing.T) {
	ctx := context.Background()
	oldWhere := t.TempDir()
	h, _, p, st, _, _ := newDiskUpgradeHandler(t, t.TempDir())
	seedUpgradeArray(t, st, p, oldWhere, 10*disk.TB)
	if _, err := h.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
		t.Fatalf("StopArray: %v", err)
	}
	pending := seedPendingDiskUpgrade(t, h, job.StatusInterrupted, nil)

	replacement := disk.AssignedDisk{Device: "/dev/sdz", Filesystem: disk.XFS}
	_, err := h.UpgradeDisk(ctx, &apiv1.UpgradeDiskRequest{Mountpoint: oldWhere, Device: "/dev/sdz", Confirmation: job.SingleDiskConfirmation(replacement)})
	status := apiError(t, h, err)
	if status.StatusCode != 409 || status.Response.Code != "disk_upgrade_pending" || !strings.Contains(status.Response.Message, pending) {
		t.Fatalf("UpgradeDisk while one is pending = %+v, want 409 disk_upgrade_pending naming %s", status, pending)
	}
}

// seedPendingDiskUpgrade writes a data-disk upgrade job row with status
// and checkpoint straight into the store and returns its id.
func seedPendingDiskUpgrade(t *testing.T, h *api.Handler, status job.Status, checkpoint []byte) string {
	t.Helper()
	now := time.Now().UTC()
	id := "22222222-2222-2222-2222-222222222222"
	params := job.DiskUpgradeDataParams{
		Confirmation: "ERASE /dev/sdz",
		Mountpoint:   "/mnt/disk1",
		Old:          disk.AssignedDisk{Device: "/dev/sdb", Filesystem: disk.XFS, FSUUID: "uuid-d1"},
		Disk:         disk.AssignedDisk{Device: "/dev/sdz", Filesystem: disk.XFS},
	}
	body, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Store.Create(context.Background(), &job.Job{
		ID: id, Type: job.TypeDiskUpgradeData, Class: job.ClassTopology, Status: status,
		Resumable: true, Cancellable: true, Params: body, Checkpoint: checkpoint, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestHandler_CancelJob_DataDiskUpgradeAtReleasingRefused: E3 Interrupted
// at releasing through the API: 409 job_not_cancellable, and the job stays
// interrupted.
func TestHandler_CancelJob_DataDiskUpgradeAtReleasingRefused(t *testing.T) {
	ctx := context.Background()
	h, _, _, _, _, _ := newDiskUpgradeHandler(t, t.TempDir())
	id := seedPendingDiskUpgrade(t, h, job.StatusInterrupted, []byte(`{"phase":"releasing","new_uuid":"uuid-new"}`))

	_, err := h.CancelJob(ctx, apiv1.CancelJobParams{JobId: uuid.MustParse(id)})
	status := apiError(t, h, err)
	if status.StatusCode != 409 || status.Response.Code != "job_not_cancellable" || !strings.Contains(status.Response.Message, "release decision") {
		t.Fatalf("CancelJob(at releasing) = %+v, want 409 job_not_cancellable explaining the release decision", status)
	}
	got, _ := h.Store.Get(ctx, id)
	if got.Status != job.StatusInterrupted {
		t.Fatalf("status = %s, want still interrupted", got.Status)
	}
}

// TestHandler_UpgradeDisk_WrongConfirmationRefusesAndFormatsNothing proves
// upgradeDisk refuses a stale or forged confirmation before ever
// submitting a job.
func TestHandler_UpgradeDisk_WrongConfirmationRefusesAndFormatsNothing(t *testing.T) {
	ctx := context.Background()
	oldWhere := t.TempDir()
	staging := t.TempDir()
	h, _, p, st, _, _ := newDiskUpgradeHandler(t, staging)

	p.AddDisk("/dev/sda", disk.Disk{Size: 16 * disk.TB})
	p.AddDisk("/dev/sdz", disk.Disk{Size: 10 * disk.TB})
	if err := st.PutArray(ctx, store.ArraySettings{
		CreatePolicy: "mfs", MinFreeSpace: "20G", CreatedAt: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", Mountpoint: "/mnt/parity1"},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d1", Mountpoint: oldWhere},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}

	_, err := h.UpgradeDisk(ctx, &apiv1.UpgradeDiskRequest{Mountpoint: oldWhere, Device: "/dev/sdz", Confirmation: "wrong"})
	status := apiError(t, h, err)
	if status.StatusCode != 409 || status.Response.Code != "confirmation_required" {
		t.Fatalf("UpgradeDisk(wrong confirm) = %+v, want 409 confirmation_required", status)
	}
	if _, ok := p.FormattedAs("/dev/sdz"); ok {
		t.Fatal("wrong confirmation formatted /dev/sdz")
	}
}

// TestHandler_UpgradeDisk_Parity_WrongNewMountpointRefused proves
// upgradeDisk refuses a parity upgrade whose newMountpoint no longer
// matches the array's current topology — planDiskUpgrade's plan is the
// only source of a valid newMountpoint.
func TestHandler_UpgradeDisk_Parity_WrongNewMountpointRefused(t *testing.T) {
	ctx := context.Background()
	oldParity := t.TempDir()
	staging := t.TempDir()
	h, _, p, st, _, _ := newDiskUpgradeHandler(t, staging)

	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	p.AddDisk("/dev/sdz", disk.Disk{Size: 16 * disk.TB})
	if err := st.PutArray(ctx, store.ArraySettings{
		CreatePolicy: "mfs", MinFreeSpace: "20G", CreatedAt: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", Mountpoint: oldParity},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d1", Mountpoint: "/mnt/disk1"},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}

	replacement := disk.AssignedDisk{Device: "/dev/sdz", Filesystem: disk.XFS}
	_, err := h.UpgradeDisk(ctx, &apiv1.UpgradeDiskRequest{
		Mountpoint:    oldParity,
		Device:        "/dev/sdz",
		Confirmation:  job.SingleDiskConfirmation(replacement),
		NewMountpoint: apiv1.NewOptString("/mnt/stale-slot"),
	})
	status := apiError(t, h, err)
	if status.StatusCode != 400 || status.Response.Code != "invalid_plan" {
		t.Fatalf("UpgradeDisk(stale newMountpoint) = %+v, want 400 invalid_plan", status)
	}
	if _, ok := p.FormattedAs("/dev/sdz"); ok {
		t.Fatal("a stale newMountpoint formatted the replacement disk anyway")
	}
}
