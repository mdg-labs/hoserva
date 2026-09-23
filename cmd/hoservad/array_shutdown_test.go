package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/api"
	cfggen "github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"
	"github.com/mdg-labs/hoserva/internal/update"

	_ "modernc.org/sqlite"
)

// newLiveArrayShutdownEnv wires a scheduler, Handler, job.UPSController and
// update.Engine exactly the way main.go's own run() does — including the
// ArrayReady hook that calls handler.SetArray from a live CreateArray
// (#262) — so a test here proves both shutdown consumers #263 fixes
// (the UPS controller's low-battery shutdown, the update engine's own
// reboot shutdown) resolve the daemon's real, current array at the moment
// a shutdown actually runs, rather than whatever existed when
// newUPSController/newUpdateEngine were called, before any array existed.
func newLiveArrayShutdownEnv(t *testing.T) (context.Context, *api.Handler, *job.UPSController, *update.Engine, *update.FakeHost, *disk.FakeProvider, *disk.FakeRunner) {
	t.Helper()

	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "hoservad-array-shutdown-test.db")
	db, err := sql.Open("sqlite", store.DSN(dbPath))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}

	arrays := store.NewArrayStore(db)
	shares := store.NewShareStore(db)
	registry := job.NewRegistry()
	scheduler := job.NewScheduler(job.NewStore(db), job.NewLogStore(t.TempDir()), job.NewHub(), registry)
	provider := disk.NewFakeProvider()
	fakeRunner := disk.NewFakeRunner()

	h := &api.Handler{Scheduler: scheduler, Store: job.NewStore(db), Disks: provider, ArrayStore: arrays}

	registry.Register(job.TypeDiskFormat, false, job.RunDiskFormat(job.DiskFormatDeps{
		Provider:  provider,
		Runner:    fakeRunner,
		Store:     arrays,
		Generator: cfggen.NewGenerator(t.TempDir()),
		Mounter:   disk.NewFakeMounter(),
		ArrayReady: func(ctx context.Context) error {
			seq, err := newArraySequence(ctx, scheduler, arrays, shares, provider, fakeRunner)
			if err != nil {
				return err
			}
			h.SetArray(seq)
			return nil
		},
		Now: func() time.Time { return time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC) },
	}))

	upsController := newUPSController(scheduler, h.CurrentArray, nil, fakeRunner)

	fakeHost := &update.FakeHost{}
	updateEngine := &update.Engine{
		Host:     fakeHost,
		Shutdown: updateShutdownLookup{currentArray: h.CurrentArray},
	}

	return context.Background(), h, upsController, updateEngine, fakeHost, provider, fakeRunner
}

// createLiveArray drives the same CreateArray request
// TestHandler_CreateArray_RefreshesArraySequenceWithoutRestart (array_test.go)
// uses and waits for the job to finish, failing the test if it didn't
// succeed — the "create an array live" half both regression tests below
// share. It returns how many calls runner had already recorded (the
// disk_format job's own mkfs/blkid calls), so callers can isolate exactly
// what a later shutdown adds.
func createLiveArray(t *testing.T, ctx context.Context, h *api.Handler, disks *disk.FakeProvider, runner *disk.FakeRunner) int {
	t.Helper()
	disks.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB, WWN: "wwn-parity", Serial: "PARITY1", ByIDName: "wwn-wwn-parity"})
	disks.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB, Serial: "DATA1"})
	disks.AddDisk("/dev/sdc", disk.Disk{Size: 4 * disk.TB, Serial: "DATA2"})
	runner.Script("blkid", []string{"-s", "UUID", "-o", "value", "/dev/disk/by-id/wwn-wwn-parity"}, []byte("uuid-parity1\n"), nil)
	runner.Script("blkid", []string{"-s", "UUID", "-o", "value", "/dev/sdb"}, []byte("uuid-disk1\n"), nil)
	runner.Script("blkid", []string{"-s", "UUID", "-o", "value", "/dev/sdc"}, []byte("uuid-disk2\n"), nil)

	j, err := h.CreateArray(ctx, liveArrayCreateReq("/dev/sda", "/dev/sdb", "/dev/sdc"))
	if err != nil {
		t.Fatalf("CreateArray: %v", err)
	}
	finished, err := h.Scheduler.Await(ctx, j.ID.String())
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if finished.Status != job.StatusSucceeded {
		t.Fatalf("create-array job status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	return len(runner.Calls())
}

// TestUPSController_LowBatteryAfterLiveArrayCreation_StopsRealArrayFirst is
// #263's central safety-critical regression: exactly the data-loss
// scenario the issue describes. Before this fix, newUPSController captured
// arraySeq by value once at daemon startup — nil/empty when no array
// existed yet — so a low-battery event after a live array creation (#262)
// ran UPSShutdown.Shutdown against that stale empty sequence: Stop trivially
// "succeeds" with nothing to unmount, and PowerOff runs immediately while
// the real, live-created array is still mounted. This proves the fix:
// HandleNotify(LOWBATT) must unmount the array's own catch-all and disk
// mounts before systemctl poweroff ever runs.
func TestUPSController_LowBatteryAfterLiveArrayCreation_StopsRealArrayFirst(t *testing.T) {
	ctx, h, upsController, _, _, disks, runner := newLiveArrayShutdownEnv(t)

	if h.CurrentArray() != nil {
		t.Fatal("CurrentArray() is already set before any array has ever been created")
	}
	before := createLiveArray(t, ctx, h, disks, runner)
	if h.CurrentArray() == nil {
		t.Fatal("CurrentArray() is nil after a live CreateArray succeeded — the shutdown path would still see no array to stop")
	}

	if err := upsController.HandleNotify(ctx, job.UPSNotifyLowBattery); err != nil {
		t.Fatalf("HandleNotify(LOWBATT): %v", err)
	}

	calls := runner.Calls()[before:]
	if len(calls) == 0 {
		t.Fatal("low-battery shutdown made no calls at all — want the array's own unmounts then poweroff")
	}
	last := calls[len(calls)-1]
	if last.Name != "systemctl" || len(last.Args) != 1 || last.Args[0] != "poweroff" {
		t.Fatalf("last call = %+v, want the final `systemctl poweroff`", last)
	}
	if len(calls) < 6 {
		t.Fatalf("calls = %+v, want Samba and NFS's LoadState checked and stopped and at least one real unmount before poweroff — a stale/empty ArraySequence would jump straight to poweroff, which is exactly #263's data-loss scenario", calls)
	}
	for _, c := range calls[:len(calls)-1] {
		if c.Name == "systemctl" && len(c.Args) == 1 && c.Args[0] == "poweroff" {
			t.Fatalf("poweroff ran before every unmount finished: %+v", calls)
		}
	}
	requireArgv(t, calls[0], "systemctl", "show", "--property=LoadState", "--value", "smbd.service")
	requireArgv(t, calls[1], "systemctl", "stop", "smbd.service")
	requireArgv(t, calls[2], "systemctl", "show", "--property=LoadState", "--value", "nfs-kernel-server.service")
	requireArgv(t, calls[3], "systemctl", "stop", "nfs-kernel-server.service")
	requireArgv(t, calls[4], "systemctl", "stop", pool.UnitFileName(pool.CatchAllPath))
}

// TestUpdateEngine_RebootAfterLiveArrayCreation_StopsRealArrayFirst is the
// update-engine half of #263: update.Engine.Reboot must run the same real,
// current array's Stop — not a stale/nil one captured at construction —
// before calling Host.Reboot.
func TestUpdateEngine_RebootAfterLiveArrayCreation_StopsRealArrayFirst(t *testing.T) {
	ctx, h, _, updateEngine, fakeHost, disks, runner := newLiveArrayShutdownEnv(t)

	before := createLiveArray(t, ctx, h, disks, runner)
	if h.CurrentArray() == nil {
		t.Fatal("CurrentArray() is nil after a live CreateArray succeeded")
	}

	if err := updateEngine.Reboot(ctx); err != nil {
		t.Fatalf("Reboot: %v", err)
	}

	if fakeHost.RebootCalls != 1 {
		t.Fatalf("Host.Reboot calls = %d, want 1", fakeHost.RebootCalls)
	}
	calls := runner.Calls()[before:]
	if len(calls) == 0 {
		t.Fatal("Reboot's own shutdown stopped nothing — want the real array's own unmounts, exactly #263's data-loss scenario if it stayed stale/nil")
	}
	requireArgv(t, calls[0], "systemctl", "show", "--property=LoadState", "--value", "smbd.service")
	requireArgv(t, calls[1], "systemctl", "stop", "smbd.service")
	requireArgv(t, calls[2], "systemctl", "show", "--property=LoadState", "--value", "nfs-kernel-server.service")
	requireArgv(t, calls[3], "systemctl", "stop", "nfs-kernel-server.service")
	requireArgv(t, calls[4], "systemctl", "stop", pool.UnitFileName(pool.CatchAllPath))
}
