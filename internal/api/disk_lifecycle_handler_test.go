package api_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// newDiskLifecycleHandler wires a *api.Handler with a real ArrayStore and
// job.Scheduler sharing one SQLite database (as production does) plus a
// FakeProvider/FakeRunner/FakeEngine, and registers TypeDiskAdd and
// TypeDiskReplace against them — the same shape newArrayHandler already
// uses for TypeDiskFormat.
func newDiskLifecycleHandler(t *testing.T) (*api.Handler, *job.Scheduler, *disk.FakeProvider, *store.ArrayStore, *parity.FakeEngine, *disk.FakeRunner) {
	t.Helper()

	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "disk-lifecycle-test.db")
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
	eng.ScriptFix([]parity.Progress{{}}, nil)

	registry.Register(job.TypeDiskAdd, false, job.RunDiskAdd(job.DiskAddDeps{
		Provider:  p,
		Runner:    fakeRun,
		Store:     arrayStore,
		Generator: config.NewGenerator(genRoot),
		Mounter:   disk.NewFakeMounter(),
		Now:       func() time.Time { return time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC) },
	}))
	registry.Register(job.TypeDiskReplace, false, job.RunDiskReplace(job.DiskReplaceDeps{
		Provider:  p,
		Runner:    fakeRun,
		Store:     arrayStore,
		Generator: config.NewGenerator(genRoot),
		Mounter:   disk.NewFakeMounter(),
		Parity:    eng,
		Now:       func() time.Time { return time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC) },
	}))

	h := &api.Handler{
		Scheduler:  scheduler,
		Store:      jobStore,
		Logs:       logs,
		Disks:      p,
		ArrayStore: arrayStore,
	}
	return h, scheduler, p, arrayStore, eng, fakeRun
}

func scriptUUID(r *disk.FakeRunner, dev, uuid string) {
	r.Script("blkid", []string{"-s", "UUID", "-o", "value", dev}, []byte(uuid+"\n"), nil)
}

// scriptReplaceMountedUUID scripts findmnt for mountpoint the way
// RunDiskReplace's own pre-fix check reads it (disk.MountedUUID).
func scriptReplaceMountedUUID(r *disk.FakeRunner, mountpoint, uuid string) {
	r.Script("findmnt", []string{"-n", "-o", "UUID", mountpoint}, []byte(uuid+"\n"), nil)
}

// seedHandlerArray persists a parity+data+cache array — a cache disk is
// what makes Q18's parity-count+2 (=3) content-file-copy requirement
// satisfiable at all with only one parity and one data disk, so any test
// that goes as far as applyArrayFromStore (AddDisk/ReplaceDisk, not their
// read-only plan previews) needs it, exactly as the lab's own
// disk_lifecycle_lab_test.go does for the same reason.
func seedHandlerArray(t *testing.T, st *store.ArrayStore, p *disk.FakeProvider) {
	t.Helper()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	p.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB})
	p.AddDisk("/dev/sdcache", disk.Disk{Size: 4 * disk.TB})
	err := st.PutArray(context.Background(), store.ArraySettings{
		CreatePolicy: "mfs",
		MinFreeSpace: "20G",
		CreatedAt:    time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", Mountpoint: "/mnt/parity1"},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d1", Mountpoint: "/mnt/disk1"},
		{Role: store.ArrayRoleCache, RoleIndex: 1, Device: "/dev/sdcache", Filesystem: "xfs", FSUUID: "uuid-c", Mountpoint: "/mnt/cache"},
	})
	if err != nil {
		t.Fatalf("seedHandlerArray PutArray: %v", err)
	}
}

func TestHandler_PlanDiskAdd_ReturnsMountpointAndConfirmation(t *testing.T) {
	ctx := context.Background()
	h, _, p, st, _, _ := newDiskLifecycleHandler(t)
	seedHandlerArray(t, st, p)
	p.AddDisk("/dev/sdc", disk.Disk{Size: 4 * disk.TB, Model: "WDC WD40EFRX", WWN: "wwn-new"})

	plan, err := h.PlanDiskAdd(ctx, &apiv1.AddDiskPlanRequest{Device: "/dev/sdc"})
	if err != nil {
		t.Fatalf("PlanDiskAdd: %v", err)
	}
	if plan.Mountpoint != "/mnt/disk2" {
		t.Fatalf("Mountpoint = %q, want /mnt/disk2", plan.Mountpoint)
	}
	if plan.Confirmation != "ERASE /dev/sdc" {
		t.Fatalf("Confirmation = %q, want ERASE /dev/sdc", plan.Confirmation)
	}
	if plan.Filesystem != apiv1.ArrayDiskFilesystemXfs {
		t.Fatalf("Filesystem = %s, want xfs default", plan.Filesystem)
	}
	if got, ok := plan.Model.Get(); !ok || got != "WDC WD40EFRX" {
		t.Fatalf("Model = %+v, want WDC WD40EFRX (finding 3: the plan must show the target's identity)", plan.Model)
	}
	if got, ok := plan.Wwn.Get(); !ok || got != "wwn-new" {
		t.Fatalf("Wwn = %+v, want wwn-new", plan.Wwn)
	}
	if got, ok := plan.SizeBytes.Get(); !ok || got != 4*disk.TB {
		t.Fatalf("SizeBytes = %+v, want %d", plan.SizeBytes, 4*disk.TB)
	}
}

// TestHandler_PlanDiskAdd_WeakIdentityFSUUIDRefused is finding 1's own
// handler-level regression test (#288 fix round 4): resolveAssignedDisk
// copies the target's own filesystem UUID from a fresh disk inventory
// (array_handler.go's own "assigned.FSUUID = d.FSUUID") before
// job.ValidateDiskAddition ever runs — remove that line and this plan
// would succeed instead of refusing a weak-identity disk that is really
// disk1 under a renumbered path.
func TestHandler_PlanDiskAdd_WeakIdentityFSUUIDRefused(t *testing.T) {
	ctx := context.Background()
	h, _, p, st, _, _ := newDiskLifecycleHandler(t)
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	if err := st.PutArray(ctx, store.ArraySettings{
		CreatePolicy: "mfs", MinFreeSpace: "20G", CreatedAt: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", Mountpoint: "/mnt/parity1"},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/vdb", Filesystem: "xfs", FSUUID: "uuid-d1", WeakIdentity: true, Mountpoint: "/mnt/disk1"},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	// /dev/vdd is a different path than disk1's own /dev/vdb, but the
	// same physical disk: no WWN or serial, only its own filesystem UUID
	// in common — a renumbered member, not a fresh disk.
	p.AddDisk("/dev/vdd", disk.Disk{Size: 4 * disk.TB, WeakIdentity: true, FSUUID: "uuid-d1"})

	_, err := h.PlanDiskAdd(ctx, &apiv1.AddDiskPlanRequest{Device: "/dev/vdd"})
	status := apiError(t, h, err)
	if status.StatusCode != 400 || status.Response.Code != "invalid_plan" {
		t.Fatalf("PlanDiskAdd(weak-identity match) = %+v, want 400 invalid_plan", status)
	}
	if !strings.Contains(status.Response.Message, "already a member") {
		t.Fatalf("message = %q, want ErrDiskAlreadyMember", status.Response.Message)
	}
}

func TestHandler_PlanDiskAdd_NoArrayYetRefused(t *testing.T) {
	ctx := context.Background()
	h, _, p, _, _, _ := newDiskLifecycleHandler(t)
	p.AddDisk("/dev/sdc", disk.Disk{Size: 4 * disk.TB})

	_, err := h.PlanDiskAdd(ctx, &apiv1.AddDiskPlanRequest{Device: "/dev/sdc"})
	status := apiError(t, h, err)
	if status.StatusCode != 400 || status.Response.Code != "invalid_plan" {
		t.Fatalf("PlanDiskAdd(no array) = %+v, want 400 invalid_plan", status)
	}
}

func TestHandler_PlanDiskAdd_Q20RefusesOversizedDisk(t *testing.T) {
	ctx := context.Background()
	h, _, p, st, _, _ := newDiskLifecycleHandler(t)
	seedHandlerArray(t, st, p)
	p.AddDisk("/dev/sdc", disk.Disk{Size: 16 * disk.TB})

	_, err := h.PlanDiskAdd(ctx, &apiv1.AddDiskPlanRequest{Device: "/dev/sdc"})
	status := apiError(t, h, err)
	if status.StatusCode != 400 || status.Response.Code != "invalid_plan" {
		t.Fatalf("PlanDiskAdd(oversized) = %+v, want 400 invalid_plan", status)
	}
}

func TestHandler_AddDisk_WrongConfirmationRefusesAndFormatsNothing(t *testing.T) {
	ctx := context.Background()
	h, _, p, st, _, _ := newDiskLifecycleHandler(t)
	seedHandlerArray(t, st, p)
	p.AddDisk("/dev/sdc", disk.Disk{Size: 4 * disk.TB})

	_, err := h.AddDisk(ctx, &apiv1.AddDiskRequest{Device: "/dev/sdc", Confirmation: "erase /dev/sdc"})
	status := apiError(t, h, err)
	if status.StatusCode != 409 || status.Response.Code != "confirmation_required" {
		t.Fatalf("AddDisk(wrong confirm) = %+v, want 409 confirmation_required", status)
	}
	if _, ok := p.FormattedAs("/dev/sdc"); ok {
		t.Fatal("wrong confirmation formatted /dev/sdc")
	}
}

func TestHandler_AddDisk_Success_StartsJobAndPersistsTopology(t *testing.T) {
	ctx := context.Background()
	h, s, p, st, _, r := newDiskLifecycleHandler(t)
	seedHandlerArray(t, st, p)
	p.AddDisk("/dev/sdc", disk.Disk{Size: 4 * disk.TB})
	scriptUUID(r, "/dev/sdc", "uuid-added")

	got, err := h.AddDisk(ctx, &apiv1.AddDiskRequest{Device: "/dev/sdc", Confirmation: "ERASE /dev/sdc"})
	if err != nil {
		t.Fatalf("AddDisk: %v", err)
	}
	if got.Type != apiv1.JobTypeDiskAdd || got.Class != apiv1.JobClassTopology {
		t.Fatalf("job type/class = %s/%s, want disk_add/topology", got.Type, got.Class)
	}
	finished := awaitJob(t, s, got.ID.String())
	if finished.Status != job.StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	added, err := st.GetDataDiskByMountpoint(ctx, "/mnt/disk2")
	if err != nil {
		t.Fatalf("GetDataDiskByMountpoint(/mnt/disk2): %v", err)
	}
	if added.Device != "/dev/sdc" {
		t.Fatalf("added disk device = %q, want /dev/sdc", added.Device)
	}
}

func TestHandler_PlanDiskReplace_ReturnsRebuildAndConfirmation(t *testing.T) {
	ctx := context.Background()
	h, _, p, st, _, _ := newDiskLifecycleHandler(t)
	seedHandlerArray(t, st, p)
	p.AddDisk("/dev/sdz", disk.Disk{Size: 4 * disk.TB, Model: "WDC WD40EFRX"})

	plan, err := h.PlanDiskReplace(ctx, &apiv1.ReplaceDiskPlanRequest{Mountpoint: "/mnt/disk1", Device: "/dev/sdz"})
	if err != nil {
		t.Fatalf("PlanDiskReplace: %v", err)
	}
	if plan.PreviousDevice != "/dev/sdb" {
		t.Fatalf("PreviousDevice = %q, want /dev/sdb", plan.PreviousDevice)
	}
	if plan.ReplacementDevice != "/dev/sdz" {
		t.Fatalf("ReplacementDevice = %q, want /dev/sdz", plan.ReplacementDevice)
	}
	if plan.Rebuild != "snapraid fix -d d1" {
		t.Fatalf("Rebuild = %q, want snapraid fix -d d1", plan.Rebuild)
	}
	if plan.Confirmation != "ERASE /dev/sdz" {
		t.Fatalf("Confirmation = %q, want ERASE /dev/sdz", plan.Confirmation)
	}
	if got, ok := plan.Model.Get(); !ok || got != "WDC WD40EFRX" {
		t.Fatalf("Model = %+v, want WDC WD40EFRX (finding 3: the plan must show the replacement's identity)", plan.Model)
	}
}

// TestHandler_PlanDiskReplace_WeakIdentityFSUUIDRefused is finding 1's own
// handler-level regression test for replace (#288 fix round 4): the same
// resolveAssignedDisk line PlanDiskAdd's own regression test proves,
// exercised here through planDiskReplace instead.
func TestHandler_PlanDiskReplace_WeakIdentityFSUUIDRefused(t *testing.T) {
	ctx := context.Background()
	h, _, p, st, _, _ := newDiskLifecycleHandler(t)
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	if err := st.PutArray(ctx, store.ArraySettings{
		CreatePolicy: "mfs", MinFreeSpace: "20G", CreatedAt: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", Mountpoint: "/mnt/parity1"},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/vdb", Filesystem: "xfs", FSUUID: "uuid-d1", WeakIdentity: true, Mountpoint: "/mnt/disk1"},
		{Role: store.ArrayRoleData, RoleIndex: 2, Device: "/dev/vdc", Filesystem: "xfs", FSUUID: "uuid-d2", WeakIdentity: true, Mountpoint: "/mnt/disk2"},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	// /dev/vdd is offered as disk1's replacement; disk1's own old device
	// (/dev/vdb) is absent from inventory, so
	// ConfirmReplacementTargetAbsent passes, but /dev/vdd's own
	// filesystem UUID matches disk2's own stored member — the same
	// physical disk under a renumbered path, still a live member, not a
	// fresh disk.
	p.AddDisk("/dev/vdd", disk.Disk{Size: 4 * disk.TB, WeakIdentity: true, FSUUID: "uuid-d2"})

	_, err := h.PlanDiskReplace(ctx, &apiv1.ReplaceDiskPlanRequest{Mountpoint: "/mnt/disk1", Device: "/dev/vdd"})
	status := apiError(t, h, err)
	if status.StatusCode != 400 || status.Response.Code != "invalid_plan" {
		t.Fatalf("PlanDiskReplace(weak-identity match) = %+v, want 400 invalid_plan", status)
	}
	if !strings.Contains(status.Response.Message, "already a member") {
		t.Fatalf("message = %q, want ErrDiskAlreadyMember", status.Response.Message)
	}
}

func TestHandler_PlanDiskReplace_UnknownMountpointRefused(t *testing.T) {
	ctx := context.Background()
	h, _, p, st, _, _ := newDiskLifecycleHandler(t)
	seedHandlerArray(t, st, p)
	p.AddDisk("/dev/sdz", disk.Disk{Size: 4 * disk.TB})

	_, err := h.PlanDiskReplace(ctx, &apiv1.ReplaceDiskPlanRequest{Mountpoint: "/mnt/disk9", Device: "/dev/sdz"})
	status := apiError(t, h, err)
	if status.StatusCode != 404 || status.Response.Code != "disk_slot_not_found" {
		t.Fatalf("PlanDiskReplace(unknown mountpoint) = %+v, want 404 disk_slot_not_found", status)
	}
}

// TestHandler_PlanDiskReplace_SlotDiskStillPresentRefused is finding 1's
// own API-level regression test: the slot's stored disk (/dev/sdb, WWN
// wwn-d1) is still reported by the fake provider's inventory, renumbered
// to /dev/sdy — a healthy disk that has not actually failed or been
// removed (doc 02 §4 steps 1-2). PlanDiskReplace must refuse before ever
// building a plan, let alone letting the operator confirm one.
func TestHandler_PlanDiskReplace_SlotDiskStillPresentRefused(t *testing.T) {
	ctx := context.Background()
	h, _, p, st, _, _ := newDiskLifecycleHandler(t)
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	p.AddDisk("/dev/sdcache", disk.Disk{Size: 4 * disk.TB})
	// The slot's own disk (WWN wwn-d1) is still attached, renumbered.
	p.AddDisk("/dev/sdy", disk.Disk{Size: 4 * disk.TB, WWN: "wwn-d1"})
	p.AddDisk("/dev/sdz", disk.Disk{Size: 4 * disk.TB})
	if err := st.PutArray(ctx, store.ArraySettings{
		CreatePolicy: "mfs", MinFreeSpace: "20G", CreatedAt: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", Mountpoint: "/mnt/parity1"},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d1", WWN: "wwn-d1", Mountpoint: "/mnt/disk1"},
		{Role: store.ArrayRoleCache, RoleIndex: 1, Device: "/dev/sdcache", Filesystem: "xfs", FSUUID: "uuid-c", Mountpoint: "/mnt/cache"},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}

	_, err := h.PlanDiskReplace(ctx, &apiv1.ReplaceDiskPlanRequest{Mountpoint: "/mnt/disk1", Device: "/dev/sdz"})
	status := apiError(t, h, err)
	if status.StatusCode != 409 || status.Response.Code != "slot_disk_present" {
		t.Fatalf("PlanDiskReplace(slot disk still present) = %+v, want 409 slot_disk_present", status)
	}

	_, err = h.ReplaceDisk(ctx, &apiv1.ReplaceDiskRequest{Mountpoint: "/mnt/disk1", Device: "/dev/sdz", Confirmation: "ERASE /dev/sdz"})
	status = apiError(t, h, err)
	if status.StatusCode != 409 || status.Response.Code != "slot_disk_present" {
		t.Fatalf("ReplaceDisk(slot disk still present) = %+v, want 409 slot_disk_present", status)
	}
	if _, ok := p.FormattedAs("/dev/sdz"); ok {
		t.Fatal("a still-present slot disk formatted the replacement anyway")
	}
}

func TestHandler_ReplaceDisk_WrongConfirmationRefusesAndFormatsNothing(t *testing.T) {
	ctx := context.Background()
	h, _, p, st, _, _ := newDiskLifecycleHandler(t)
	seedHandlerArray(t, st, p)
	p.AddDisk("/dev/sdz", disk.Disk{Size: 4 * disk.TB})

	_, err := h.ReplaceDisk(ctx, &apiv1.ReplaceDiskRequest{Mountpoint: "/mnt/disk1", Device: "/dev/sdz", Confirmation: "erase /dev/sdz"})
	status := apiError(t, h, err)
	if status.StatusCode != 409 || status.Response.Code != "confirmation_required" {
		t.Fatalf("ReplaceDisk(wrong confirm) = %+v, want 409 confirmation_required", status)
	}
	if _, ok := p.FormattedAs("/dev/sdz"); ok {
		t.Fatal("wrong confirmation formatted /dev/sdz")
	}
}

func TestHandler_ReplaceDisk_Success_StartsJobAndSwitchesTopology(t *testing.T) {
	ctx := context.Background()
	h, s, p, st, eng, r := newDiskLifecycleHandler(t)
	seedHandlerArray(t, st, p)
	p.AddDisk("/dev/sdz", disk.Disk{Size: 4 * disk.TB})
	scriptUUID(r, "/dev/sdz", "uuid-replaced")
	scriptReplaceMountedUUID(r, "/mnt/disk1", "uuid-replaced")
	eng.SetStatus(parity.ParityStatus{DataMounts: map[string]string{"d1": "/mnt/disk1"}})

	got, err := h.ReplaceDisk(ctx, &apiv1.ReplaceDiskRequest{Mountpoint: "/mnt/disk1", Device: "/dev/sdz", Confirmation: "ERASE /dev/sdz"})
	if err != nil {
		t.Fatalf("ReplaceDisk: %v", err)
	}
	if got.Type != apiv1.JobTypeDiskReplace || got.Class != apiv1.JobClassTopology {
		t.Fatalf("job type/class = %s/%s, want disk_replace/topology", got.Type, got.Class)
	}
	finished := awaitJob(t, s, got.ID.String())
	if finished.Status != job.StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	switched, err := st.GetDataDiskByMountpoint(ctx, "/mnt/disk1")
	if err != nil {
		t.Fatalf("GetDataDiskByMountpoint(/mnt/disk1): %v", err)
	}
	if switched.Device != "/dev/sdz" {
		t.Fatalf("disk1 device = %q, want /dev/sdz", switched.Device)
	}
}

// TestHandler_ReplaceDisk_AllowsAnEvacuatedOrUnpooledDiskOnceGenuinelyMissing
// is #384's own regression: a slot marked evacuated or unpooled is no
// longer refused (disk_leaving_array) on the removal state alone once its
// old disk is genuinely missing (no strong identity for
// ConfirmReplacementTargetAbsent to still find present, the same shape a
// dead disk's loop device leaves once detached, doc 06 §3). PlanDiskReplace
// and ReplaceDisk both succeed, the job clears removal_state in the same
// write that adopts the replacement, and the slot's device switches over —
// the lab test (cmd/hoservad/disk_replace_removal_lab_test.go) proves the
// full data-loss scenario this unlocks against a real SnapRAID fix.
func TestHandler_ReplaceDisk_AllowsAnEvacuatedOrUnpooledDiskOnceGenuinelyMissing(t *testing.T) {
	for _, state := range []string{store.RemovalStateEvacuated, store.RemovalStateUnpooled} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			h, s, p, st, eng, r := newDiskLifecycleHandler(t)
			seedHandlerArray(t, st, p)
			markRemoval(t, st, "/mnt/disk1", state)
			p.AddDisk("/dev/sdz", disk.Disk{Size: 4 * disk.TB})
			scriptUUID(r, "/dev/sdz", "uuid-replaced")
			scriptReplaceMountedUUID(r, "/mnt/disk1", "uuid-replaced")
			eng.SetStatus(parity.ParityStatus{DataMounts: map[string]string{"d1": "/mnt/disk1"}})

			plan, err := h.PlanDiskReplace(ctx, &apiv1.ReplaceDiskPlanRequest{Mountpoint: "/mnt/disk1", Device: "/dev/sdz"})
			if err != nil {
				t.Fatalf("PlanDiskReplace(%s disk): %v", state, err)
			}
			if plan.Confirmation != "ERASE /dev/sdz" {
				t.Fatalf("Confirmation = %q, want ERASE /dev/sdz", plan.Confirmation)
			}

			got, err := h.ReplaceDisk(ctx, &apiv1.ReplaceDiskRequest{Mountpoint: "/mnt/disk1", Device: "/dev/sdz", Confirmation: "ERASE /dev/sdz"})
			if err != nil {
				t.Fatalf("ReplaceDisk(%s disk): %v", state, err)
			}
			finished := awaitJob(t, s, got.ID.String())
			if finished.Status != job.StatusSucceeded {
				t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
			}
			switched, err := st.GetDataDiskByMountpoint(ctx, "/mnt/disk1")
			if err != nil {
				t.Fatalf("GetDataDiskByMountpoint(/mnt/disk1): %v", err)
			}
			if switched.Device != "/dev/sdz" {
				t.Fatalf("disk1 device = %q, want /dev/sdz", switched.Device)
			}
			if switched.RemovalState != "" {
				t.Fatalf("disk1 removal_state = %q after replace, want cleared", switched.RemovalState)
			}
		})
	}
}
