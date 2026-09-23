package main

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	cfggen "github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/share"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

func newArrayTestEnv(t *testing.T) (context.Context, *api.Handler, *store.ArrayStore, *store.ShareStore, *disk.FakeProvider, *disk.FakeRunner) {
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
	shares := store.NewShareStore(db)
	jobStore := job.NewStore(db)
	scheduler := job.NewScheduler(jobStore, job.NewLogStore(t.TempDir()), job.NewHub(), job.NewRegistry())
	h := &api.Handler{Scheduler: scheduler, Store: jobStore}
	return context.Background(), h, arrays, shares, disk.NewFakeProvider(), disk.NewFakeRunner()
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

func attachDaemonArray(t *testing.T, ctx context.Context, h *api.Handler, arrays *store.ArrayStore, shares *store.ShareStore, disks disk.Provider, runner disk.Runner) {
	t.Helper()
	seq, err := newArraySequence(ctx, h.Scheduler, arrays, shares, disks, runner)
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

// arrayTestCatchAll is ArraySequence's Mount fake for hoservad's L1
// wiring tests: it records fusermount/mergerfs through the injected
// Runner without os.MkdirAll or a real mount attempt against a real
// path under /mnt or /run/hoserva (#207). Despite the name (kept to
// avoid touching every existing caller), it stands in for any single
// ArrayMount — the catch-all, or, for #268's own ShareMounts wiring
// test, a share's own mount and its mover write target.
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
	ctx, h, arrays, shares, disks, runner := newArrayTestEnv(t)
	assigned := persistSampleArray(t, arrays)
	presentMatchingDisks(disks, assigned)

	attachDaemonArray(t, ctx, h, arrays, shares, disks, runner)
	if h.Array == nil {
		t.Fatal("Handler.Array is nil with a persisted topology — stop/start would 501 not_configured and never run ArraySequence")
	}
	if len(h.Array.Services) != 2 {
		t.Fatalf("Services = %d, want Samba and NFS (doc 02 §4, #309)", len(h.Array.Services))
	}
	if got := h.Array.Services[0].Name(); got != "Samba" {
		t.Fatalf("Services[0].Name() = %q, want Samba to stop before NFS", got)
	}
	if got := h.Array.Services[1].Name(); got != "NFS" {
		t.Fatalf("Services[1].Name() = %q, want NFS", got)
	}
	for i, svc := range h.Array.Services {
		if _, ok := svc.(disk.ServiceUnitController); !ok {
			t.Fatalf("Services[%d] is %T, want disk.ServiceUnitController", i, svc)
		}
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
	gate, ok := storageGateOf(h.Array.Gate)
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
	if len(stopCalls) != 7 {
		t.Fatalf("StopArray runner calls = %+v, want Samba and NFS's LoadState checked and stopped, then catch-all, then both disks", stopCalls)
	}
	requireArgv(t, stopCalls[0], "systemctl", "show", "--property=LoadState", "--value", "smbd.service")
	requireArgv(t, stopCalls[1], "systemctl", "stop", "smbd.service")
	requireArgv(t, stopCalls[2], "systemctl", "show", "--property=LoadState", "--value", "nfs-kernel-server.service")
	requireArgv(t, stopCalls[3], "systemctl", "stop", "nfs-kernel-server.service")
	requireArgv(t, stopCalls[4], "fusermount", "-u", pool.CatchAllPath)
	requireArgv(t, stopCalls[5], "systemctl", "stop", "mnt-parity1.mount")
	requireArgv(t, stopCalls[6], "systemctl", "stop", "mnt-disk1.mount")

	isolateDiskCheck(t, h.Array)
	got, err = h.StartArray(ctx)
	if err != nil {
		t.Fatalf("StartArray: %v", err)
	}
	if got.MaintenanceMode.Or(false) {
		t.Fatal("StartArray: maintenanceMode must be false after a successful start")
	}
	startCalls := runner.Calls()[len(stopCalls):]
	if len(startCalls) != 9 {
		t.Fatalf("StartArray runner calls = %+v, want both disks, then catch-all, then NFS and Samba's LoadState checked and started (reverse of stop order)", startCalls)
	}
	requireArgv(t, startCalls[0], "systemctl", "start", "mnt-parity1.mount")
	requireArgv(t, startCalls[1], "systemctl", "start", "mnt-disk1.mount")
	if startCalls[2].Name != "mergerfs" {
		t.Fatalf("StartArray catch-all call = %+v, want mergerfs", startCalls[2])
	}
	if len(startCalls[2].Args) == 0 || startCalls[2].Args[len(startCalls[2].Args)-1] != pool.CatchAllPath {
		t.Fatalf("StartArray catch-all argv = %v, want mountpoint %q", startCalls[2].Args, pool.CatchAllPath)
	}
	requireArgv(t, startCalls[3], "systemctl", "show", "--property=LoadState", "--value", "nfs-kernel-server.service")
	requireArgv(t, startCalls[4], "systemctl", "show", "--property=UnitFileState", "--value", "nfs-kernel-server.service")
	requireArgv(t, startCalls[5], "systemctl", "start", "nfs-kernel-server.service")
	requireArgv(t, startCalls[6], "systemctl", "show", "--property=LoadState", "--value", "smbd.service")
	requireArgv(t, startCalls[7], "systemctl", "show", "--property=UnitFileState", "--value", "smbd.service")
	requireArgv(t, startCalls[8], "systemctl", "start", "smbd.service")
}

// TestNewArraySequence_StopAbortsBeforeAnyUnmountWhenSambaFailsToStop is
// #309's own central data-loss scenario, reachable through the real
// wiring: Samba refusing to release a client's connection must abort
// StopArray before the catch-all or any disk unmounts, leaving the array
// mounted — exactly the "a client still connected can write into the bare
// mountpoint" failure mode #309 describes. It fails against the
// newArraySequence on dev, which never populates Services at all.
func TestNewArraySequence_StopAbortsBeforeAnyUnmountWhenSambaFailsToStop(t *testing.T) {
	ctx, h, arrays, shares, disks, runner := newArrayTestEnv(t)
	assigned := persistSampleArray(t, arrays)
	presentMatchingDisks(disks, assigned)
	attachDaemonArray(t, ctx, h, arrays, shares, disks, runner)

	realCatchAll, ok := h.Array.CatchAll.(pool.MountController)
	if !ok {
		t.Fatalf("CatchAll is %T, want pool.MountController for argv extraction", h.Array.CatchAll)
	}
	h.Array.CatchAll = arrayTestCatchAll{
		where:  pool.CatchAllPath,
		argv:   realCatchAll.Mnt.Argv(),
		runner: runner,
	}

	runner.Script("systemctl", []string{"stop", "smbd.service"}, nil, fmt.Errorf("smbd.service: Job failed, unit is holding open files"))

	_, err := h.StopArray(ctx, confirmStop())
	if err == nil {
		t.Fatal("StopArray: got nil error, want Samba's stop failure to propagate")
	}

	calls := runner.Calls()
	if len(calls) != 2 {
		t.Fatalf("StopArray runner calls = %+v, want only Samba's LoadState check and its failed stop — the array must stay mounted", calls)
	}
	requireArgv(t, calls[0], "systemctl", "show", "--property=LoadState", "--value", "smbd.service")
	requireArgv(t, calls[1], "systemctl", "stop", "smbd.service")

	status, err := h.GetStatus(ctx)
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if !status.MaintenanceMode.Or(false) {
		t.Fatal("GetStatus: maintenanceMode must still be true — a failed stop must not silently fall back to normal operation")
	}
}

// TestNewArraySequence_SharesRejoinStopAndStart is #268's own
// reproduction: a persisted share must have its own mount and (since it
// is not cache-only) its mover write target woven into ArraySequence's
// ShareMounts, unmounted before the catch-all on Stop and mounted after
// it on Start (doc 02 §4). Against today's newArraySequence, which never
// reads store.ShareStore, ShareMounts stays empty — both the wiring
// assertions and the Stop/Start call-ordering assertions below fail
// against it.
func TestNewArraySequence_SharesRejoinStopAndStart(t *testing.T) {
	ctx, h, arrays, shares, disks, runner := newArrayTestEnv(t)
	assigned := persistSampleArray(t, arrays)
	presentMatchingDisks(disks, assigned)

	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	if err := shares.Insert(ctx, store.Share{
		Name:         "media",
		CacheMode:    "array-only",
		CreatePolicy: "mfs",
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("Insert share: %v", err)
	}

	attachDaemonArray(t, ctx, h, arrays, shares, disks, runner)
	if h.Array == nil {
		t.Fatal("Handler.Array is nil with a persisted topology")
	}
	if len(h.Array.ShareMounts) != 2 {
		t.Fatalf("ShareMounts = %d, want 2 (the share's own mount and its mover write target)", len(h.Array.ShareMounts))
	}
	shareMount, ok := h.Array.ShareMounts[0].(pool.MountController)
	if !ok {
		t.Fatalf("ShareMounts[0] is %T, want pool.MountController", h.Array.ShareMounts[0])
	}
	if shareMount.Where() != pool.SharePath("media") {
		t.Fatalf("ShareMounts[0].Where() = %q, want %q", shareMount.Where(), pool.SharePath("media"))
	}
	moverMount, ok := h.Array.ShareMounts[1].(pool.MountController)
	if !ok {
		t.Fatalf("ShareMounts[1] is %T, want pool.MountController", h.Array.ShareMounts[1])
	}
	if moverMount.Where() != pool.MoverTargetPath("media") {
		t.Fatalf("ShareMounts[1].Where() = %q, want %q", moverMount.Where(), pool.MoverTargetPath("media"))
	}

	// Swap the catch-all and both share mounts for the argv-recording
	// fake before calling Stop/Start, exactly as
	// TestNewArraySequence_WiresStopAndStartWhenTopologyExists does for
	// the catch-all alone (#207): this proves the wiring and ordering
	// without ever touching a real path under /mnt or /run/hoserva.
	realCatchAll, ok := h.Array.CatchAll.(pool.MountController)
	if !ok {
		t.Fatalf("CatchAll is %T, want pool.MountController", h.Array.CatchAll)
	}
	h.Array.CatchAll = arrayTestCatchAll{where: pool.CatchAllPath, argv: realCatchAll.Mnt.Argv(), runner: runner}
	h.Array.ShareMounts[0] = arrayTestCatchAll{where: shareMount.Where(), argv: shareMount.Mnt.Argv(), runner: runner}
	h.Array.ShareMounts[1] = arrayTestCatchAll{where: moverMount.Where(), argv: moverMount.Mnt.Argv(), runner: runner}

	got, err := h.StopArray(ctx, confirmStop())
	if err != nil {
		t.Fatalf("StopArray: %v", err)
	}
	if !got.MaintenanceMode.Or(false) {
		t.Fatal("StopArray: maintenanceMode must be true after a successful stop")
	}
	stopCalls := runner.Calls()
	if len(stopCalls) != 9 {
		t.Fatalf("StopArray runner calls = %+v, want Samba and NFS's LoadState checked and stopped, then the share mount, its mover target, the catch-all, then both disks", stopCalls)
	}
	requireArgv(t, stopCalls[0], "systemctl", "show", "--property=LoadState", "--value", "smbd.service")
	requireArgv(t, stopCalls[1], "systemctl", "stop", "smbd.service")
	requireArgv(t, stopCalls[2], "systemctl", "show", "--property=LoadState", "--value", "nfs-kernel-server.service")
	requireArgv(t, stopCalls[3], "systemctl", "stop", "nfs-kernel-server.service")
	requireArgv(t, stopCalls[4], "fusermount", "-u", pool.SharePath("media"))
	requireArgv(t, stopCalls[5], "fusermount", "-u", pool.MoverTargetPath("media"))
	requireArgv(t, stopCalls[6], "fusermount", "-u", pool.CatchAllPath)
	requireArgv(t, stopCalls[7], "systemctl", "stop", "mnt-parity1.mount")
	requireArgv(t, stopCalls[8], "systemctl", "stop", "mnt-disk1.mount")

	isolateDiskCheck(t, h.Array)
	got, err = h.StartArray(ctx)
	if err != nil {
		t.Fatalf("StartArray: %v", err)
	}
	if got.MaintenanceMode.Or(false) {
		t.Fatal("StartArray: maintenanceMode must be false after a successful start")
	}
	startCalls := runner.Calls()[len(stopCalls):]
	if len(startCalls) != 11 {
		t.Fatalf("StartArray runner calls = %+v, want both disks, the catch-all, the share mount, its mover target, then NFS and Samba's LoadState checked and started", startCalls)
	}
	requireArgv(t, startCalls[0], "systemctl", "start", "mnt-parity1.mount")
	requireArgv(t, startCalls[1], "systemctl", "start", "mnt-disk1.mount")
	if startCalls[2].Name != "mergerfs" || startCalls[2].Args[len(startCalls[2].Args)-1] != pool.CatchAllPath {
		t.Fatalf("StartArray call[2] = %+v, want the catch-all mergerfs mount", startCalls[2])
	}
	if startCalls[3].Name != "mergerfs" || startCalls[3].Args[len(startCalls[3].Args)-1] != pool.SharePath("media") {
		t.Fatalf("StartArray call[3] = %+v, want the share's own mergerfs mount, after the catch-all", startCalls[3])
	}
	if startCalls[4].Name != "mergerfs" || startCalls[4].Args[len(startCalls[4].Args)-1] != pool.MoverTargetPath("media") {
		t.Fatalf("StartArray call[4] = %+v, want the mover write target mergerfs mount", startCalls[4])
	}
	requireArgv(t, startCalls[5], "systemctl", "show", "--property=LoadState", "--value", "nfs-kernel-server.service")
	requireArgv(t, startCalls[6], "systemctl", "show", "--property=UnitFileState", "--value", "nfs-kernel-server.service")
	requireArgv(t, startCalls[7], "systemctl", "start", "nfs-kernel-server.service")
	requireArgv(t, startCalls[8], "systemctl", "show", "--property=LoadState", "--value", "smbd.service")
	requireArgv(t, startCalls[9], "systemctl", "show", "--property=UnitFileState", "--value", "smbd.service")
	requireArgv(t, startCalls[10], "systemctl", "start", "smbd.service")
}

// TestNewArraySequence_NilWhenNoArray is the empty-path half of the
// data-loss scenario: with no persisted topology, Array stays nil so
// stop/start 501 rather than unmounting an empty path.
func TestNewArraySequence_NilWhenNoArray(t *testing.T) {
	ctx, h, arrays, shares, disks, runner := newArrayTestEnv(t)

	attachDaemonArray(t, ctx, h, arrays, shares, disks, runner)
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
	ctx, h, arrays, shares, disks, runner := newArrayTestEnv(t)
	assigned := persistSampleArray(t, arrays)
	presentMatchingDisks(disks, assigned[:1])

	attachDaemonArray(t, ctx, h, arrays, shares, disks, runner)
	if h.Array == nil {
		t.Fatal("Handler.Array is nil with a persisted topology — Start would 501 instead of refusing through the gate")
	}
	gate, ok := storageGateOf(h.Array.Gate)
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
	ctx, h, arrays, shares, disks, runner := newArrayTestEnv(t)
	persistSampleArray(t, arrays)
	failing := failListProvider{Provider: disks, err: context.DeadlineExceeded}

	seq, err := newArraySequence(ctx, h.Scheduler, arrays, shares, failing, runner)
	if err != nil {
		t.Fatalf("newArraySequence: %v — a List failure must not abort daemon startup", err)
	}
	if seq == nil {
		t.Fatal("Handler.Array would be nil — stop/start would 501 instead of refusing through the gate")
	}
	h.Array = seq
	gate, ok := storageGateOf(seq.Gate)
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
	gate, ok := storageGateOf(h.Array.Gate)
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

	isolateDiskCheck(t, h.Array)
	got, err := h.StartArray(ctx)
	if err != nil {
		t.Fatalf("StartArray after a live CreateArray: %v", err)
	}
	if got.MaintenanceMode.Or(false) {
		t.Fatal("StartArray: maintenanceMode must be false after a successful start")
	}
	startCalls := runner.Calls()
	if len(startCalls) < 3 {
		t.Fatal("StartArray ran too few calls — want at least the catch-all mount and both services started")
	}
	catchAllIdx := -1
	for i, c := range startCalls {
		if c.Name == "mergerfs" {
			catchAllIdx = i
		}
	}
	if catchAllIdx == -1 {
		t.Fatalf("StartArray calls = %+v, want a mergerfs call for the catch-all", startCalls)
	}
	after := startCalls[catchAllIdx+1:]
	if len(after) != 6 {
		t.Fatalf("calls after the catch-all mount = %+v, want NFS then Samba's LoadState checked and started (reverse of stop order)", after)
	}
	requireArgv(t, after[0], "systemctl", "show", "--property=LoadState", "--value", "nfs-kernel-server.service")
	requireArgv(t, after[1], "systemctl", "show", "--property=UnitFileState", "--value", "nfs-kernel-server.service")
	requireArgv(t, after[2], "systemctl", "start", "nfs-kernel-server.service")
	requireArgv(t, after[3], "systemctl", "show", "--property=LoadState", "--value", "smbd.service")
	requireArgv(t, after[4], "systemctl", "show", "--property=UnitFileState", "--value", "smbd.service")
	requireArgv(t, after[5], "systemctl", "start", "smbd.service")
}

// failOnMountMounter is share.Mounter's Mount fake for
// TestShareService_PostCommit_KeepsArraySequenceShareMountsInSyncWithStore's
// own failed-Create half: it refuses any mount whose path contains
// refuseSubstr, so a test can force share.Service.Create's own
// syncLiveMounts call to fail and drive rollbackCreate, without ever
// touching a real path under /mnt or /run/hoserva (#207).
type failOnMountMounter struct{ refuseSubstr string }

func (m failOnMountMounter) Mount(ctx context.Context, mnt pool.Mount) error {
	if strings.Contains(mnt.Where, m.refuseSubstr) {
		return fmt.Errorf("failOnMountMounter: refusing to mount %s", mnt.Where)
	}
	return nil
}

func (m failOnMountMounter) Unmount(ctx context.Context, where string) error { return nil }

// TestShareService_PostCommit_KeepsArraySequenceShareMountsInSyncWithStore
// is #268's second finding: newArraySequence only ever runs at daemon
// startup or a disk-topology change, so without share.Service telling it
// to look again, Handler.Array's ShareMounts stayed exactly as stale as
// it was at the last of those two events — a live createShare left it
// missing the new share entirely, and a live deleteShare left a stale
// entry behind (array start would then recreate a deleted share's
// directory on a data disk, and array stop would fail on it). This
// drives a real share.Service — the same construction main.go's own
// run() wires, with shareService.PostCommit set to the same rebuild
// closure — through Create, Delete, and a failed Create (whose own
// rollbackCreate deletes the row it just inserted), asserting
// Handler.Array.ShareMounts always matches what store.ShareStore
// actually holds immediately afterward. Against 7898360, which never
// sets PostCommit, ShareMounts never changes after daemon construction
// and every assertion below except the first fails.
func TestShareService_PostCommit_KeepsArraySequenceShareMountsInSyncWithStore(t *testing.T) {
	ctx, h, arrays, shares, disks, runner := newArrayTestEnv(t)

	root := t.TempDir()
	assigned := []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", WWN: "wwn-p", Serial: "PARITY1", ByIDName: "wwn-wwn-p", Mountpoint: filepath.Join(root, "parity1")},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d", WWN: "wwn-d", Serial: "DATA1", ByIDName: "wwn-wwn-d", Mountpoint: filepath.Join(root, "disk1")},
	}
	if err := arrays.PutArray(ctx, store.ArraySettings{
		CreatePolicy: "mfs",
		MinFreeSpace: "20G",
		CreatedAt:    time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
	}, assigned); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	presentMatchingDisks(disks, assigned)

	attachDaemonArray(t, ctx, h, arrays, shares, disks, runner)
	if h.Array == nil {
		t.Fatal("Handler.Array is nil with a persisted topology")
	}
	if len(h.Array.ShareMounts) != 0 {
		t.Fatalf("ShareMounts = %d before any share exists, want 0", len(h.Array.ShareMounts))
	}

	generator := cfggen.NewGenerator(filepath.Join(root, "etc"))
	shareService := newShareService(shares, arrays, generator, failOnMountMounter{refuseSubstr: "badshare"}, nil)
	shareService.PostCommit = func(ctx context.Context) error {
		seq, err := newArraySequence(ctx, h.Scheduler, arrays, shares, disks, runner)
		if err != nil {
			return err
		}
		h.Array = seq
		return nil
	}

	if _, err := shareService.Create(ctx, share.CreateInput{Name: "media", CacheMode: pool.ArrayOnly}); err != nil {
		t.Fatalf("Create(media): %v", err)
	}
	if len(h.Array.ShareMounts) != 2 {
		t.Fatalf("ShareMounts = %d right after Create(media), want 2 (the share's own mount and its mover write target)", len(h.Array.ShareMounts))
	}

	if _, err := shareService.Create(ctx, share.CreateInput{Name: "badshare", CacheMode: pool.ArrayOnly}); err == nil {
		t.Fatal("Create(badshare): got nil error, want the injected mount failure to propagate")
	}
	if _, err := shares.Get(ctx, "badshare"); err == nil {
		t.Fatal("badshare is still in the store after its own rollbackCreate — Create's rollback did not delete it")
	}
	if len(h.Array.ShareMounts) != 2 {
		t.Fatalf("ShareMounts = %d after a rolled-back Create(badshare), want 2 (unchanged — badshare was never really created)", len(h.Array.ShareMounts))
	}

	if err := shareService.Delete(ctx, "media", true); err != nil {
		t.Fatalf("Delete(media): %v", err)
	}
	if len(h.Array.ShareMounts) != 0 {
		t.Fatalf("ShareMounts = %d after Delete(media), want 0 (the deleted share must not remount on the next array start)", len(h.Array.ShareMounts))
	}
}

// storageGateOf unwraps newArraySequence's gate: the storage readiness
// gate inside job.PendingUpgradeGate (doc 02 §4 UR2).
func storageGateOf(g job.ReadinessGate) (*disk.StorageGate, bool) {
	wrapped, ok := g.(job.PendingUpgradeGate)
	if !ok {
		return nil, false
	}
	inner, ok := wrapped.Gate.(*disk.StorageGate)
	return inner, ok
}

// isolateDiskCheck asserts newArraySequence wired UR9's check (doc 02 §4)
// with the production mount-table reader and SQLite's UUIDs, then swaps
// that reader for an empty fake table so Start never reads the host's
// mount table from a test.
func isolateDiskCheck(t *testing.T, seq *job.ArraySequence) *job.FakeMountTable {
	t.Helper()
	check, ok := seq.DiskCheck.(job.ArrayDiskUUIDCheck)
	if !ok {
		t.Fatalf("DiskCheck is %T, want job.ArrayDiskUUIDCheck (UR9)", seq.DiskCheck)
	}
	if _, ok := check.Mounts.(disk.KernelMounts); !ok {
		t.Fatalf("DiskCheck.Mounts is %T, want disk.KernelMounts", check.Mounts)
	}
	if len(check.Disks) != len(seq.Disks) {
		t.Fatalf("DiskCheck covers %d disks, want every array disk (%d)", len(check.Disks), len(seq.Disks))
	}
	table := job.NewFakeMountTable()
	check.Mounts = table
	seq.DiskCheck = check
	return table
}

// TestNewArraySequence_UR9_StartRefusesADiskThatIsNotTheOneSQLiteNames:
// through the daemon's own wiring, a mounted array disk whose filesystem
// is not the one SQLite names stops array start before the pool or any
// service starts, unmounts the disks again and keeps maintenance mode
// (doc 02 §4 UR9).
func TestNewArraySequence_UR9_StartRefusesADiskThatIsNotTheOneSQLiteNames(t *testing.T) {
	ctx, h, arrays, shares, disks, runner := newArrayTestEnv(t)
	assigned := persistSampleArray(t, arrays)
	presentMatchingDisks(disks, assigned)
	attachDaemonArray(t, ctx, h, arrays, shares, disks, runner)
	check := h.Array.DiskCheck.(job.ArrayDiskUUIDCheck)
	for i, u := range check.Disks {
		if u.Where != assigned[i].Mountpoint || u.UUID != assigned[i].FSUUID {
			t.Fatalf("DiskCheck.Disks[%d] = %+v, want %s at %s from SQLite", i, u, assigned[i].FSUUID, assigned[i].Mountpoint)
		}
	}
	table := isolateDiskCheck(t, h.Array)
	table.Preload("/mnt/disk1", "uuid-of-another-disk")

	if _, err := h.StopArray(ctx, confirmStop()); err != nil {
		t.Fatalf("StopArray: %v", err)
	}
	before := len(runner.Calls())
	_, err := h.StartArray(ctx)
	status := handlerAPIError(t, h, err)
	if status.StatusCode != 409 || status.Response.Code != "array_disk_mismatch" {
		t.Fatalf("StartArray = %+v, want 409 array_disk_mismatch", status)
	}
	var sawUnmount bool
	for _, c := range runner.Calls()[before:] {
		if c.Name == "mergerfs" || (c.Name == "systemctl" && len(c.Args) == 2 && c.Args[0] == "start" && strings.HasSuffix(c.Args[1], ".service")) {
			t.Fatalf("StartArray ran %+v after a mismatched disk", c)
		}
		if c.Name == "systemctl" && len(c.Args) == 2 && c.Args[0] == "stop" && c.Args[1] == "mnt-disk1.mount" {
			sawUnmount = true
		}
	}
	if !sawUnmount {
		t.Fatal("StartArray did not unmount the disks again after the mismatch")
	}
	if !h.Scheduler.InMaintenance() {
		t.Fatal("maintenance mode ended after a refused start")
	}
}

// TestNewArraySequence_PendingDiskUpgradeHoldsTheArrayStopped proves the
// daemon's wiring of doc 02 §4 while a data-disk upgrade is pending: the
// readiness gate reports not ready (UR2), `array start` is refused with
// disk_upgrade_pending (E6), and so is `array stop` (E7), which leaves
// every mount untouched.
func TestNewArraySequence_PendingDiskUpgradeHoldsTheArrayStopped(t *testing.T) {
	ctx, h, arrays, shares, disks, runner := newArrayTestEnv(t)
	assigned := persistSampleArray(t, arrays)
	presentMatchingDisks(disks, assigned)
	attachDaemonArray(t, ctx, h, arrays, shares, disks, runner)
	if !h.Array.Gate.Ready() {
		t.Fatal("gate not ready before any upgrade")
	}

	now := time.Now().UTC()
	if err := h.Store.Create(ctx, &job.Job{
		ID: "11111111-1111-1111-1111-111111111111", Type: job.TypeDiskUpgradeData, Class: job.ClassTopology,
		Status: job.StatusInterrupted, Resumable: true, Cancellable: true, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if h.Array.Gate.Ready() {
		t.Fatal("the readiness gate reports ready while a data-disk upgrade is pending (UR2)")
	}
	_, err := h.StartArray(ctx)
	if status := handlerAPIError(t, h, err); status.StatusCode != 409 || status.Response.Code != "disk_upgrade_pending" {
		t.Fatalf("StartArray = %+v, want 409 disk_upgrade_pending", status)
	}
	_, err = h.StopArray(ctx, confirmStop())
	if status := handlerAPIError(t, h, err); status.StatusCode != 409 || status.Response.Code != "disk_upgrade_pending" {
		t.Fatalf("StopArray = %+v, want 409 disk_upgrade_pending", status)
	}
	if calls := runner.Calls(); len(calls) != 0 {
		t.Fatalf("array start/stop acted while an upgrade was pending: %+v", calls)
	}
}
