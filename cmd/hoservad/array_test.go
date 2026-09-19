package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

func newArrayTestEnv(t *testing.T) (context.Context, *api.Handler, *store.ArrayStore, *disk.FakeProvider, *disk.FakeRunner) {
	t.Helper()

	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "hoservad-array-test.db")
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
	jobStore := job.NewStore(db)
	scheduler := job.NewScheduler(jobStore, job.NewLogStore(t.TempDir()), job.NewHub(), job.NewRegistry())
	h := &api.Handler{Scheduler: scheduler, Store: jobStore}
	return context.Background(), h, arrays, disk.NewFakeProvider(), disk.NewFakeRunner()
}

func sampleArrayDisks() []store.ArrayDisk {
	return []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", WWN: "wwn-p", Serial: "PARITY1", ByIDName: "wwn-wwn-p", Mountpoint: "/mnt/parity1"},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d", WWN: "wwn-d", Serial: "DATA1", ByIDName: "wwn-wwn-d", Mountpoint: "/mnt/disk1"},
	}
}

func persistSampleArray(t *testing.T, arrays *store.ArrayStore) []store.ArrayDisk {
	t.Helper()
	disks := sampleArrayDisks()
	if err := arrays.PutArray(context.Background(), store.ArraySettings{
		CreatePolicy: "mfs",
		MinFreeSpace: "20G",
		CreatedAt:    time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
	}, disks); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	return disks
}

func presentMatchingDisks(p *disk.FakeProvider, disks []store.ArrayDisk) {
	for _, d := range disks {
		p.AddDisk(d.Device, disk.Disk{
			WWN:          d.WWN,
			Serial:       d.Serial,
			WeakIdentity: d.WeakIdentity,
			ByIDName:     d.ByIDName,
		})
	}
}

func attachDaemonArray(t *testing.T, ctx context.Context, h *api.Handler, arrays *store.ArrayStore, disks disk.Provider, runner disk.Runner) {
	t.Helper()
	seq, err := newArraySequence(ctx, h.Scheduler, arrays, disks, runner)
	if err != nil {
		t.Fatalf("newArraySequence: %v", err)
	}
	h.Array = seq
}

func handlerAPIError(t *testing.T, h *api.Handler, err error) *apiv1.ErrorStatusCode {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	return h.NewError(context.Background(), err)
}

func requireArgv(t *testing.T, got disk.RunCall, name string, args ...string) {
	t.Helper()
	if got.Name != name {
		t.Fatalf("call name = %q, want %q (full call %+v)", got.Name, name, got)
	}
	if len(got.Args) != len(args) {
		t.Fatalf("call args = %v, want %v", got.Args, args)
	}
	for i := range args {
		if got.Args[i] != args[i] {
			t.Fatalf("call args = %v, want %v (differs at %d)", got.Args, args, i)
		}
	}
}

func confirmStop() *apiv1.StopArrayRequest {
	return &apiv1.StopArrayRequest{Confirm: true}
}

// TestNewArraySequence_WiresStopAndStartWhenTopologyExists is the live-array
// half of #190's data-loss scenario: a persisted topology must leave
// Handler.Array set to one job.ArraySequence built from the daemon's
// scheduler, a disk.StorageGate already evaluated against Provider.List,
// MountUnitControllers, and the catch-all MountController. Leaving Array
// nil here is what made POST /array/stop|/start return 501 and never
// enter or leave maintenance. Inventing a second unmount order, or
// skipping Evaluate so Start always refuses, is the other half.
func TestNewArraySequence_WiresStopAndStartWhenTopologyExists(t *testing.T) {
	ctx, h, arrays, disks, runner := newArrayTestEnv(t)
	assigned := persistSampleArray(t, arrays)
	presentMatchingDisks(disks, assigned)

	attachDaemonArray(t, ctx, h, arrays, disks, runner)
	if h.Array == nil {
		t.Fatal("Handler.Array is nil with a persisted topology — stop/start would 501 not_configured and never run ArraySequence")
	}
	if len(h.Array.Services) != 0 {
		t.Fatalf("Services = %d, want empty until real ArrayService implementations exist", len(h.Array.Services))
	}
	if len(h.Array.ShareMounts) != 0 {
		t.Fatalf("ShareMounts = %d, want empty (no persisted share list)", len(h.Array.ShareMounts))
	}
	if _, ok := h.Array.CatchAll.(pool.MountController); !ok {
		t.Fatalf("CatchAll is %T, want pool.MountController — hoservad must not invent a second unmount order", h.Array.CatchAll)
	}
	if h.Array.CatchAll.Where() != pool.CatchAllPath {
		t.Fatalf("CatchAll.Where() = %q, want %q", h.Array.CatchAll.Where(), pool.CatchAllPath)
	}
	if len(h.Array.Disks) != len(assigned) {
		t.Fatalf("Disks = %d, want %d assigned disks", len(h.Array.Disks), len(assigned))
	}
	for i, m := range h.Array.Disks {
		if _, ok := m.(disk.MountUnitController); !ok {
			t.Fatalf("Disks[%d] is %T, want disk.MountUnitController", i, m)
		}
		if m.Where() != assigned[i].Mountpoint {
			t.Fatalf("Disks[%d].Where() = %q, want %q", i, m.Where(), assigned[i].Mountpoint)
		}
	}
	gate, ok := h.Array.Gate.(*disk.StorageGate)
	if !ok {
		t.Fatalf("Gate is %T, want *disk.StorageGate", h.Array.Gate)
	}
	if !gate.Ready() {
		t.Fatal("gate.Ready() = false with every expected disk present — Evaluate was skipped at construction, so every Start would refuse")
	}

	got, err := h.StopArray(ctx, confirmStop())
	if err != nil {
		t.Fatalf("StopArray: %v", err)
	}
	if !got.MaintenanceMode.Or(false) {
		t.Fatal("StopArray: maintenanceMode must be true after a successful stop")
	}
	stopCalls := runner.Calls()
	if len(stopCalls) != 3 {
		t.Fatalf("StopArray runner calls = %+v, want catch-all then both disks", stopCalls)
	}
	requireArgv(t, stopCalls[0], "fusermount", "-u", pool.CatchAllPath)
	requireArgv(t, stopCalls[1], "systemctl", "stop", "mnt-parity1.mount")
	requireArgv(t, stopCalls[2], "systemctl", "stop", "mnt-disk1.mount")

	got, err = h.StartArray(ctx)
	if err != nil {
		t.Fatalf("StartArray: %v", err)
	}
	if got.MaintenanceMode.Or(false) {
		t.Fatal("StartArray: maintenanceMode must be false after a successful start")
	}
	startCalls := runner.Calls()[len(stopCalls):]
	if len(startCalls) != 3 {
		t.Fatalf("StartArray runner calls = %+v, want both disks then catch-all", startCalls)
	}
	requireArgv(t, startCalls[0], "systemctl", "start", "mnt-parity1.mount")
	requireArgv(t, startCalls[1], "systemctl", "start", "mnt-disk1.mount")
	if startCalls[2].Name != "mergerfs" {
		t.Fatalf("StartArray catch-all call = %+v, want mergerfs", startCalls[2])
	}
	if len(startCalls[2].Args) == 0 || startCalls[2].Args[len(startCalls[2].Args)-1] != pool.CatchAllPath {
		t.Fatalf("StartArray catch-all argv = %v, want mountpoint %q", startCalls[2].Args, pool.CatchAllPath)
	}
}

// TestNewArraySequence_NilWhenNoArray is the empty-path half of the
// data-loss scenario: with no persisted topology, Array stays nil so
// stop/start 501 rather than unmounting an empty path.
func TestNewArraySequence_NilWhenNoArray(t *testing.T) {
	ctx, h, arrays, disks, runner := newArrayTestEnv(t)

	attachDaemonArray(t, ctx, h, arrays, disks, runner)
	if h.Array != nil {
		t.Fatal("Handler.Array is set with no persisted topology — stop/start must 501, not unmount an empty path")
	}

	_, err := h.StopArray(ctx, confirmStop())
	status := handlerAPIError(t, h, err)
	if status.StatusCode != 501 || status.Response.Code != "not_configured" {
		t.Fatalf("StopArray = %+v, want 501 not_configured", status)
	}
	_, err = h.StartArray(ctx)
	status = handlerAPIError(t, h, err)
	if status.StatusCode != 501 || status.Response.Code != "not_configured" {
		t.Fatalf("StartArray = %+v, want 501 not_configured", status)
	}
	if calls := runner.Calls(); len(calls) != 0 {
		t.Fatalf("no-array stop/start touched mounts: %+v", calls)
	}
}

// TestNewArraySequence_StartRefusesWhenGateNotReady is Q69's own
// data-loss scenario at daemon construction: a persisted topology whose
// expected disks are not all present must leave the gate unready, and
// Start must return storage_not_ready without mounting anything. A
// construction that skips the gate, or Starts while it is unready, would
// mount a degraded array.
func TestNewArraySequence_StartRefusesWhenGateNotReady(t *testing.T) {
	ctx, h, arrays, disks, runner := newArrayTestEnv(t)
	assigned := persistSampleArray(t, arrays)
	presentMatchingDisks(disks, assigned[:1])

	attachDaemonArray(t, ctx, h, arrays, disks, runner)
	if h.Array == nil {
		t.Fatal("Handler.Array is nil with a persisted topology — Start would 501 instead of refusing through the gate")
	}
	gate, ok := h.Array.Gate.(*disk.StorageGate)
	if !ok {
		t.Fatalf("Gate is %T, want *disk.StorageGate", h.Array.Gate)
	}
	if gate.Ready() {
		t.Fatal("gate.Ready() = true with a missing expected disk — Start would mount a degraded array")
	}

	_, err := h.StartArray(ctx)
	status := handlerAPIError(t, h, err)
	if status.StatusCode != 409 || status.Response.Code != "storage_not_ready" {
		t.Fatalf("StartArray = %+v, want 409 storage_not_ready", status)
	}
	if status.Response.Message != job.ErrStorageNotReady.Error() {
		t.Fatalf("StartArray message = %q, want %q", status.Response.Message, job.ErrStorageNotReady)
	}
	if calls := runner.Calls(); len(calls) != 0 {
		t.Fatalf("StartArray mounted while the gate was not ready: %+v", calls)
	}
}

type failListProvider struct {
	disk.Provider
	err error
}

func (p failListProvider) List(context.Context) ([]disk.Disk, error) {
	return nil, p.err
}

// TestNewArraySequence_ListErrorLeavesGateUnready is the availability half
// of daemon construction: a transient Provider.List failure must still
// return a sequence (so the API and Stop stay up) with the gate unready
// (so Start refuses). Returning that error used to abort hoservad entirely.
func TestNewArraySequence_ListErrorLeavesGateUnready(t *testing.T) {
	ctx, h, arrays, disks, runner := newArrayTestEnv(t)
	persistSampleArray(t, arrays)
	failing := failListProvider{Provider: disks, err: context.DeadlineExceeded}

	seq, err := newArraySequence(ctx, h.Scheduler, arrays, failing, runner)
	if err != nil {
		t.Fatalf("newArraySequence: %v — a List failure must not abort daemon startup", err)
	}
	if seq == nil {
		t.Fatal("Handler.Array would be nil — stop/start would 501 instead of refusing through the gate")
	}
	h.Array = seq
	gate, ok := seq.Gate.(*disk.StorageGate)
	if !ok {
		t.Fatalf("Gate is %T, want *disk.StorageGate", seq.Gate)
	}
	if gate.Ready() {
		t.Fatal("gate.Ready() = true when inventory could not be evaluated — Start would mount a degraded array")
	}

	_, err = h.StartArray(ctx)
	status := handlerAPIError(t, h, err)
	if status.StatusCode != 409 || status.Response.Code != "storage_not_ready" {
		t.Fatalf("StartArray = %+v, want 409 storage_not_ready", status)
	}
	if calls := runner.Calls(); len(calls) != 0 {
		t.Fatalf("StartArray mounted while inventory was unavailable: %+v", calls)
	}
}
