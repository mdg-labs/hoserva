package main

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	cfggen "github.com/mdg-labs/hoserva/internal/config"
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

// arrayTestCatchAll is the catch-all half of ArraySequence's Mount fake for
// hoservad's L1 wiring test: it records fusermount/mergerfs through the
// injected Runner without os.MkdirAll on pool.CatchAllPath (#207).
type arrayTestCatchAll struct {
	where  string
	argv   []string
	runner disk.Runner
}

func (c arrayTestCatchAll) Where() string { return c.where }

func (c arrayTestCatchAll) Mount(ctx context.Context) error {
	if len(c.argv) == 0 {
		return fmt.Errorf("arrayTestCatchAll: no argv")
	}
	_, err := c.runner.Run(ctx, c.argv[0], c.argv[1:]...)
	return err
}

func (c arrayTestCatchAll) Unmount(ctx context.Context) error {
	_, err := c.runner.Run(ctx, "fusermount", "-u", c.where)
	return err
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

	realCatchAll, ok := h.Array.CatchAll.(pool.MountController)
	if !ok {
		t.Fatalf("CatchAll is %T, want pool.MountController for argv extraction", h.Array.CatchAll)
	}
	h.Array.CatchAll = arrayTestCatchAll{
		where:  pool.CatchAllPath,
		argv:   realCatchAll.Mnt.Argv(),
		runner: runner,
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

// newLiveArrayCreateEnv wires job.TypeDiskFormat exactly the way main.go's
// own registration does — including the ArrayReady hook that rebuilds
// Handler.Array from freshly persisted topology (#262) — against a real
// scheduler and a real, migrated SQLite database, so a test here proves
// the same construction a restart performs at startup also runs after a
// live CreateArray, without needing to actually restart anything.
func newLiveArrayCreateEnv(t *testing.T) (context.Context, *api.Handler, *disk.FakeProvider, *disk.FakeRunner) {
	t.Helper()

	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "hoservad-live-array-create-test.db")
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
			seq, err := newArraySequence(ctx, scheduler, arrays, provider, fakeRunner)
			if err != nil {
				return err
			}
			h.Array = seq
			return nil
		},
		Now: func() time.Time { return time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC) },
	}))

	return context.Background(), h, provider, fakeRunner
}

// liveArrayCreateReq assigns one parity and two data disks — Q18 refuses
// a plan that cannot place at least three content-file copies on
// distinct physical devices, so a single data disk plus parity is not
// enough for CreateArray to actually succeed.
func liveArrayCreateReq(parity, data1, data2 string) *apiv1.CreateArrayRequest {
	plan := disk.TopologyPlan{
		Parity: []disk.AssignedDisk{{Device: parity, Filesystem: disk.XFS}},
		Data: []disk.AssignedDisk{
			{Device: data1, Filesystem: disk.XFS},
			{Device: data2, Filesystem: disk.XFS},
		},
	}
	return &apiv1.CreateArrayRequest{
		Confirmation: plan.Confirmation(),
		Disks: []apiv1.ArrayDiskAssignment{
			xfsAssignment(parity, apiv1.ArrayDiskRoleParity),
			xfsAssignment(data1, apiv1.ArrayDiskRoleData),
			xfsAssignment(data2, apiv1.ArrayDiskRoleData),
		},
	}
}

func xfsAssignment(dev string, role apiv1.ArrayDiskRole) apiv1.ArrayDiskAssignment {
	a := apiv1.ArrayDiskAssignment{Device: dev, Role: role}
	a.SetFilesystem(apiv1.NewOptArrayDiskFilesystem(apiv1.ArrayDiskFilesystemXfs))
	return a
}

// TestHandler_CreateArray_RefreshesArraySequenceWithoutRestart is #262's
// own regression: a POST /disks/array that succeeds against an already
// running daemon left Handler.Array nil (StartArray 501 not_configured)
// and the pool unmounted until hoservad restarted, because
// newArraySequence was only ever evaluated once, at daemon startup. This
// proves a live CreateArray job now rebuilds ArraySequence from the
// topology it just persisted — same Gate, same StorageGate readiness
// check newArraySequence always runs — so array/start (and the mount it
// performs) works without a restart, with no other job or process
// standing in for one.
func TestHandler_CreateArray_RefreshesArraySequenceWithoutRestart(t *testing.T) {
	ctx, h, disks, runner := newLiveArrayCreateEnv(t)
	disks.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB, WWN: "wwn-parity", Serial: "PARITY1", ByIDName: "wwn-wwn-parity"})
	disks.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB, Serial: "DATA1"})
	disks.AddDisk("/dev/sdc", disk.Disk{Size: 4 * disk.TB, Serial: "DATA2"})
	runner.Script("blkid", []string{"-s", "UUID", "-o", "value", "/dev/disk/by-id/wwn-wwn-parity"}, []byte("uuid-parity1\n"), nil)
	runner.Script("blkid", []string{"-s", "UUID", "-o", "value", "/dev/sdb"}, []byte("uuid-disk1\n"), nil)
	runner.Script("blkid", []string{"-s", "UUID", "-o", "value", "/dev/sdc"}, []byte("uuid-disk2\n"), nil)

	if h.Array != nil {
		t.Fatal("Handler.Array is already set before any array has ever been created")
	}
	if _, err := h.StartArray(ctx); err == nil {
		t.Fatal("StartArray succeeded with no array yet — want 501 not_configured")
	} else if status := handlerAPIError(t, h, err); status.StatusCode != 501 || status.Response.Code != "not_configured" {
		t.Fatalf("StartArray (no array) = %+v, want 501 not_configured", status)
	}

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

	if h.Array == nil {
		t.Fatal("Handler.Array is nil after a live CreateArray succeeded — array/start would still 501 not_configured until hoservad restarts (#262)")
	}
	gate, ok := h.Array.Gate.(*disk.StorageGate)
	if !ok {
		t.Fatalf("Gate is %T, want *disk.StorageGate", h.Array.Gate)
	}
	if !gate.Ready() {
		t.Fatal("gate.Ready() = false right after creating the array from the disks that were just formatted — Evaluate was skipped rebuilding the sequence")
	}
	realCatchAll, ok := h.Array.CatchAll.(pool.MountController)
	if !ok {
		t.Fatalf("CatchAll is %T, want pool.MountController — the live rebuild must not invent a second unmount order", h.Array.CatchAll)
	}

	// Swap in the same argv-recording fake TestNewArraySequence_Wires...
	// uses before calling Start, so this proves array/start actually runs
	// mount(8) through the injected Runner rather than a second real
	// mkdir/mergerfs invocation against pool.CatchAllPath.
	h.Array.CatchAll = arrayTestCatchAll{
		where:  pool.CatchAllPath,
		argv:   realCatchAll.Mnt.Argv(),
		runner: runner,
	}

	got, err := h.StartArray(ctx)
	if err != nil {
		t.Fatalf("StartArray after a live CreateArray: %v", err)
	}
	if got.MaintenanceMode.Or(false) {
		t.Fatal("StartArray: maintenanceMode must be false after a successful start")
	}
	startCalls := runner.Calls()
	if len(startCalls) == 0 {
		t.Fatal("StartArray ran no mount calls — array/start did nothing")
	}
	last := startCalls[len(startCalls)-1]
	if last.Name != "mergerfs" {
		t.Fatalf("StartArray's last call = %+v, want mergerfs (the catch-all)", last)
	}
}
