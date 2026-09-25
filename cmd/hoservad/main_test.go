package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// parityRegistrationEnv wires a daemon the way run() does, for exactly
// the pieces #265 touches: a registry and scheduler, a Handler, and a
// parityRegistrar whose ensure method is reachable from TypeDiskFormat's
// own ArrayReady hook — the same hook newLiveArrayCreateEnv
// (array_test.go, #262) wires rebuildArraySequence through, extended
// here to also call parityReg.ensure the way topologyChanged does in
// main.go.
type parityRegistrationEnv struct {
	handler    *api.Handler
	registry   *job.Registry
	scheduler  *job.Scheduler
	parityReg  *parityRegistrar
	chainGuard *diffGuardHolder
	configRoot string
	provider   *disk.FakeProvider
	runner     *disk.FakeRunner
}

func newParityRegistrationEnv(t *testing.T) (context.Context, *parityRegistrationEnv) {
	t.Helper()

	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "hoservad-parity-registration-test.db")
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
	configRoot := t.TempDir()

	handler := &api.Handler{Scheduler: scheduler, Store: job.NewStore(db), Disks: provider, ArrayStore: arrays}
	chainGuard := &diffGuardHolder{}
	parityReg := &parityRegistrar{
		configRoot: configRoot,
		stateDir:   t.TempDir(),
		db:         db,
		registry:   registry,
		handler:    handler,
		shareStore: shares,
		arrayStore: arrays,
		chainGuard: chainGuard,
		generator:  cfggen.NewGenerator(configRoot),
		diskUnits:  disk.NewFakeMounter(),
		mounts:     job.NewFakeMountTable(),
	}

	// arrayReadyHook mirrors main.go's own topologyChanged (fail-on-
	// failure half, wireTopologyHooks): rebuild the live ArraySequence
	// from the store, then let parityReg notice a live array creation the
	// same way #265's own topologyChanged does. handler.ArrayReady is set
	// to parityReg.callArrayReady, exactly main.go's own line (#361) —
	// not this closure directly — so a test proves CancelDiskRemoval
	// reaches it through the same indirection production wiring uses.
	arrayReadyHook := func(ctx context.Context) error {
		seq, err := newArraySequence(ctx, scheduler, arrays, shares, provider, fakeRunner)
		if err != nil {
			return err
		}
		handler.SetArray(seq)
		// Mirrors topologyChanged's own call into parityReg.ensure
		// (main.go, #265): the live CreateArray job's own Generator
		// has already written snapraid.conf under configRoot by the
		// time this ArrayReady hook runs.
		return parityReg.ensure(ctx)
	}
	parityReg.arrayReady = arrayReadyHook
	handler.ArrayReady = parityReg.callArrayReady

	registry.Register(job.TypeDiskFormat, false, job.RunDiskFormat(job.DiskFormatDeps{
		Provider:   provider,
		Runner:     fakeRunner,
		Store:      arrays,
		Generator:  cfggen.NewGenerator(configRoot),
		Mounter:    disk.NewFakeMounter(),
		ArrayReady: arrayReadyHook,
		Now:        func() time.Time { return time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC) },
	}))

	return context.Background(), &parityRegistrationEnv{
		handler:    handler,
		registry:   registry,
		scheduler:  scheduler,
		parityReg:  parityReg,
		chainGuard: chainGuard,
		configRoot: configRoot,
		provider:   provider,
		runner:     fakeRunner,
	}
}

// assertAlreadyRegistered proves typ is registered on the real registry
// production wiring uses, the same way job.Registry.Register's own doc
// comment says to notice it: a second Register call for an already-bound
// type panics before it ever touches r.entries, so recovering the panic
// here neither registers typ a second time nor leaves any state behind —
// this is deliberately never proven by calling Submit and letting a job
// actually run, since that would drive the real Sync/Scrub/Fix/
// ShareRelocation job types into a real SnapRAID invocation, which
// CLAUDE.md never allows outside the lab.
func assertAlreadyRegistered(t *testing.T, registry *job.Registry, typ job.Type) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("registry.Register(%s, ...) did not panic — the live array creation never registered it (job_type_not_registered until a restart, #265)", typ)
		}
	}()
	registry.Register(typ, false, func(ctx context.Context, rc *job.RunContext) error { return nil })
}

// TestParityRegistrar_WiresJobTypesAndHandlerFieldsAfterLiveArrayCreation
// is #265's own regression: a freshly onboarded daemon (no snapraid.conf
// at startup, so parityEngine is nil and every job.Registry.Register call
// gated behind "if parityEngine != nil" never ran) left
// TypeSync/TypeScrub/TypeFix/TypeShareRelocation/TypeRebalance/
// TypeEvacuation unregistered and Handler.Parity/ParityGuard/
// RelocationManifest/RebalanceShares nil until hoservad restarted — even
// though a live POST /disks/array had already written snapraid.conf and
// succeeded. This proves the same daemon, with no restart, has all six
// job types registered and Handler's own parity-derived fields wired
// immediately after that live creation succeeds, and that
// StartRebalance/PlanRebalance — which don't need a real SnapRAID
// invocation when there is nothing to move — actually run to completion
// through the registered TypeRebalance job type.
func TestParityRegistrar_WiresJobTypesAndHandlerFieldsAfterLiveArrayCreation(t *testing.T) {
	ctx, env := newParityRegistrationEnv(t)
	h := env.handler
	env.provider.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB, WWN: "wwn-parity", Serial: "PARITY1", ByIDName: "wwn-wwn-parity"})
	env.provider.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB, Serial: "DATA1"})
	env.provider.AddDisk("/dev/sdc", disk.Disk{Size: 4 * disk.TB, Serial: "DATA2"})
	env.runner.Script("blkid", []string{"-s", "UUID", "-o", "value", "/dev/disk/by-id/wwn-wwn-parity"}, []byte("uuid-parity1\n"), nil)
	env.runner.Script("blkid", []string{"-s", "UUID", "-o", "value", "/dev/sdb"}, []byte("uuid-disk1\n"), nil)
	env.runner.Script("blkid", []string{"-s", "UUID", "-o", "value", "/dev/sdc"}, []byte("uuid-disk2\n"), nil)

	// Before the array exists: every parity job type is unregistered
	// (Submit fails closed, synchronously, before anything could run),
	// Handler's own parity fields are unset, and the rebalance/evacuation
	// handlers refuse with 501 not_configured.
	if _, err := h.Scheduler.Submit(ctx, job.TypeSync, nil, nil); !errors.Is(err, job.ErrJobTypeNotRegistered) {
		t.Fatalf("Submit(TypeSync) before array creation = %v, want ErrJobTypeNotRegistered", err)
	}
	if _, err := h.Scheduler.Submit(ctx, job.TypeScrub, nil, nil); !errors.Is(err, job.ErrJobTypeNotRegistered) {
		t.Fatalf("Submit(TypeScrub) before array creation = %v, want ErrJobTypeNotRegistered", err)
	}
	if engine, _, manifest, rebalanceShares := h.CurrentParity(); engine != nil || manifest != nil || rebalanceShares != nil {
		t.Fatalf("CurrentParity before array creation = (engine set=%t, manifest set=%t, rebalanceShares set=%t), want all unset", engine != nil, manifest != nil, rebalanceShares != nil)
	}
	if _, err := h.PlanRebalance(ctx); err == nil {
		t.Fatal("PlanRebalance before array creation succeeded, want 501 not_configured")
	} else if status := handlerAPIError(t, h, err); status.StatusCode != 501 || status.Response.Code != "not_configured" {
		t.Fatalf("PlanRebalance before array creation = %+v, want 501 not_configured", status)
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

	// After the live creation, no restart: every parity job type is
	// registered on the same *job.Registry Submit already checked above —
	// a second Register call for any of them now panics.
	for _, typ := range []job.Type{job.TypeSync, job.TypeScrub, job.TypeFix, job.TypeShareRelocation, job.TypeRebalance, job.TypeEvacuation, job.TypeDiskRemove} {
		assertAlreadyRegistered(t, env.registry, typ)
	}

	engine, _, manifest, rebalanceShares := h.CurrentParity()
	if engine == nil {
		t.Fatal("CurrentParity: Parity is nil after a live array creation — GetParity/RunParityDiff/RunDoctor would still 501 not_configured until a restart (#265)")
	}
	if manifest == nil {
		t.Fatal("CurrentParity: RelocationManifest is nil after a live array creation")
	}
	if rebalanceShares == nil {
		t.Fatal("CurrentParity: RebalanceShares is nil after a live array creation")
	}
	if guard := env.chainGuard.get(); guard == nil {
		t.Fatal("scheduleRunner's own diffGuardHolder is still unset after a live array creation — the nightly chain would keep silently no-op'ing (#265)")
	}

	// PlanRebalance/StartRebalance actually run, through the real
	// TypeRebalance registration, with no shares configured — an empty
	// plan, so RunRebalance's own batch loop never calls Sync or
	// TrackedFileCount and this never drives a real SnapRAID invocation.
	plan, err := h.PlanRebalance(ctx)
	if err != nil {
		t.Fatalf("PlanRebalance after a live array creation: %v", err)
	}
	if len(plan.Moves) != 0 {
		t.Fatalf("PlanRebalance Moves = %d, want 0 (no shares configured)", len(plan.Moves))
	}
	rebalanceJob, err := h.StartRebalance(ctx, &apiv1.StartRebalanceRequest{Confirmation: job.RebalanceConfirmation()})
	if err != nil {
		t.Fatalf("StartRebalance after a live array creation: %v", err)
	}
	finishedRebalance, err := h.Scheduler.Await(ctx, rebalanceJob.ID.String())
	if err != nil {
		t.Fatalf("Await(rebalance): %v", err)
	}
	if finishedRebalance.Status != job.StatusSucceeded {
		t.Fatalf("rebalance job status = %s (%s), want succeeded — TypeRebalance is unreachable until a restart without #265's fix", finishedRebalance.Status, finishedRebalance.ErrorMessage)
	}
}

// TestFinishDiskRemoval_ReachesTheJobParityRegistrarRegisters is #358's
// reachability criterion: finishDiskRemoval, on the Handler run() wires,
// is 501 until the array exists, and once a live array creation has
// registered the parity job types it queues the disk_remove job
// parityRegistrar.register bound — this test registers no RunFunc of its
// own. The disk is evacuated in the store but nothing is mounted, so the
// real job refuses before changing anything, which is what proves it ran.
func TestFinishDiskRemoval_ReachesTheJobParityRegistrarRegisters(t *testing.T) {
	ctx, env := newParityRegistrationEnv(t)
	h := env.handler
	env.provider.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB, WWN: "wwn-parity", Serial: "PARITY1", ByIDName: "wwn-wwn-parity"})
	env.provider.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB, Serial: "DATA1"})
	env.provider.AddDisk("/dev/sdc", disk.Disk{Size: 4 * disk.TB, Serial: "DATA2"})
	env.provider.AddDisk("/dev/sdd", disk.Disk{Size: 4 * disk.TB, Serial: "DATA3"})
	env.runner.Script("blkid", []string{"-s", "UUID", "-o", "value", "/dev/disk/by-id/wwn-wwn-parity"}, []byte("uuid-parity1\n"), nil)
	env.runner.Script("blkid", []string{"-s", "UUID", "-o", "value", "/dev/sdb"}, []byte("uuid-disk1\n"), nil)
	env.runner.Script("blkid", []string{"-s", "UUID", "-o", "value", "/dev/sdc"}, []byte("uuid-disk2\n"), nil)
	env.runner.Script("blkid", []string{"-s", "UUID", "-o", "value", "/dev/sdd"}, []byte("uuid-disk3\n"), nil)
	// Three data disks: with one parity disk Q18 needs three content-file
	// copies, and without disk2 the boot device, disk1 and disk3 hold them.
	plan := disk.TopologyPlan{
		Parity: []disk.AssignedDisk{{Device: "/dev/sda", Filesystem: disk.XFS}},
		Data:   []disk.AssignedDisk{{Device: "/dev/sdb", Filesystem: disk.XFS}, {Device: "/dev/sdc", Filesystem: disk.XFS}, {Device: "/dev/sdd", Filesystem: disk.XFS}},
	}
	createReq := &apiv1.CreateArrayRequest{Confirmation: plan.Confirmation(), Disks: []apiv1.ArrayDiskAssignment{
		xfsAssignment("/dev/sda", apiv1.ArrayDiskRoleParity),
		xfsAssignment("/dev/sdb", apiv1.ArrayDiskRoleData),
		xfsAssignment("/dev/sdc", apiv1.ArrayDiskRoleData),
		xfsAssignment("/dev/sdd", apiv1.ArrayDiskRoleData),
	}}

	req := &apiv1.FinishDiskRemovalRequest{Mountpoint: "/mnt/disk2", Confirmation: job.EvacuationConfirmation("/mnt/disk2")}
	if _, err := h.FinishDiskRemoval(ctx, req); err == nil {
		t.Fatal("FinishDiskRemoval before the array exists succeeded, want 501 not_configured")
	} else if status := handlerAPIError(t, h, err); status.StatusCode != 501 || status.Response.Code != "not_configured" {
		t.Fatalf("FinishDiskRemoval before the array exists = %+v, want 501 not_configured", status)
	}

	created, err := h.CreateArray(ctx, createReq)
	if err != nil {
		t.Fatalf("CreateArray: %v", err)
	}
	if finished, err := h.Scheduler.Await(ctx, created.ID.String()); err != nil || finished.Status != job.StatusSucceeded {
		t.Fatalf("create-array = (%v, %v), want succeeded", finished, err)
	}
	if err := h.ArrayStore.SetRemovalState(ctx, "/mnt/disk2", store.RemovalStateEvacuated, "evac-1"); err != nil {
		t.Fatalf("SetRemovalState: %v", err)
	}

	j, err := h.FinishDiskRemoval(ctx, req)
	if err != nil {
		t.Fatalf("FinishDiskRemoval: %v", err)
	}
	if j.Type != apiv1.JobTypeDiskRemove {
		t.Fatalf("queued job type = %s, want disk_remove", j.Type)
	}
	finished, err := h.Scheduler.Await(ctx, j.ID.String())
	if err != nil {
		t.Fatalf("Await(disk_remove): %v", err)
	}
	if finished.Status != job.StatusFailed || !strings.Contains(finished.ErrorMessage, "/mnt/disk2 is not mounted, so it cannot be confirmed empty") {
		t.Fatalf("disk_remove = %s (%s), want the registered job's own refusal of an unmounted disk", finished.Status, finished.ErrorMessage)
	}
	if d, err := h.ArrayStore.GetDataDiskByMountpoint(ctx, "/mnt/disk2"); err != nil || d.RemovalState != store.RemovalStateEvacuated {
		t.Fatalf("disk2 after the refusal = (%+v, %v), want still evacuated", d, err)
	}
}

// TestCancelDiskRemoval_ReachesArrayReadyParityRegistrarWires is #361's
// own reachability criterion: cancelDiskRemoval, on the Handler run()
// wires, is 501 until Handler.ArrayReady is wired (main.go's own
// `handler.ArrayReady = parityReg.callArrayReady` line), and once a live
// array creation exists, cancelling an evacuated disk clears its removal
// state and calls through parityReg.callArrayReady into the same
// ArraySequence rebuild every disk-topology job's own ArrayReady already
// runs — proven here by the call succeeding at all: callArrayReady
// returns its own "not wired yet" error otherwise (parityRegistrar's own
// doc comment), so a nil ArrayReady or an unwired parityReg.arrayReady
// would fail this the same way a genuinely broken wiring would. It also
// proves what that rebuild actually changes — the acceptance criterion's
// own "the disk's branches are RW again after cancel" — the way
// TestNewArraySequence_RemovingDiskMarksOnlyThatDiskNC
// (cmd/hoservad/array_test.go) already inspects a live pool.MountController's
// own Mnt.What: disk2's own catch-all branch goes from NC (while
// evacuated) to RW again once CancelDiskRemoval's own rebuild runs,
// never just the disk's row in the database.
func TestCancelDiskRemoval_ReachesArrayReadyParityRegistrarWires(t *testing.T) {
	ctx, env := newParityRegistrationEnv(t)
	h := env.handler
	env.provider.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB, WWN: "wwn-parity", Serial: "PARITY1", ByIDName: "wwn-wwn-parity"})
	env.provider.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB, Serial: "DATA1"})
	env.provider.AddDisk("/dev/sdc", disk.Disk{Size: 4 * disk.TB, Serial: "DATA2"})
	env.provider.AddDisk("/dev/sdd", disk.Disk{Size: 4 * disk.TB, Serial: "DATA3"})
	env.runner.Script("blkid", []string{"-s", "UUID", "-o", "value", "/dev/disk/by-id/wwn-wwn-parity"}, []byte("uuid-parity1\n"), nil)
	env.runner.Script("blkid", []string{"-s", "UUID", "-o", "value", "/dev/sdb"}, []byte("uuid-disk1\n"), nil)
	env.runner.Script("blkid", []string{"-s", "UUID", "-o", "value", "/dev/sdc"}, []byte("uuid-disk2\n"), nil)
	env.runner.Script("blkid", []string{"-s", "UUID", "-o", "value", "/dev/sdd"}, []byte("uuid-disk3\n"), nil)
	plan := disk.TopologyPlan{
		Parity: []disk.AssignedDisk{{Device: "/dev/sda", Filesystem: disk.XFS}},
		Data:   []disk.AssignedDisk{{Device: "/dev/sdb", Filesystem: disk.XFS}, {Device: "/dev/sdc", Filesystem: disk.XFS}, {Device: "/dev/sdd", Filesystem: disk.XFS}},
	}
	createReq := &apiv1.CreateArrayRequest{Confirmation: plan.Confirmation(), Disks: []apiv1.ArrayDiskAssignment{
		xfsAssignment("/dev/sda", apiv1.ArrayDiskRoleParity),
		xfsAssignment("/dev/sdb", apiv1.ArrayDiskRoleData),
		xfsAssignment("/dev/sdc", apiv1.ArrayDiskRoleData),
		xfsAssignment("/dev/sdd", apiv1.ArrayDiskRoleData),
	}}

	req := &apiv1.CancelDiskRemovalRequest{Mountpoint: "/mnt/disk2"}
	h.ArrayStore = nil
	if err := h.CancelDiskRemoval(ctx, req); err == nil {
		t.Fatal("CancelDiskRemoval before the array exists succeeded, want 501 not_configured")
	} else if status := handlerAPIError(t, h, err); status.StatusCode != 501 || status.Response.Code != "not_configured" {
		t.Fatalf("CancelDiskRemoval before the array exists = %+v, want 501 not_configured", status)
	}
	h.ArrayStore = env.parityReg.arrayStore

	created, err := h.CreateArray(ctx, createReq)
	if err != nil {
		t.Fatalf("CreateArray: %v", err)
	}
	if finished, err := h.Scheduler.Await(ctx, created.ID.String()); err != nil || finished.Status != job.StatusSucceeded {
		t.Fatalf("create-array = (%v, %v), want succeeded", finished, err)
	}
	if err := h.ArrayStore.SetRemovalState(ctx, "/mnt/disk2", store.RemovalStateEvacuated, "evac-1"); err != nil {
		t.Fatalf("SetRemovalState: %v", err)
	}
	// Rebuilds h.Array from the store directly — the same rebuild an
	// evacuation job's own ArrayReady call would have already run — so
	// the in-memory ArraySequence actually reflects disk2's evacuated
	// state (NC) before CancelDiskRemoval is asked to reverse it; without
	// this, h.Array would still carry the RW branch list CreateArray's own
	// rebuild left behind, and the RW assertion after cancel would prove
	// nothing.
	if err := env.parityReg.arrayReady(ctx); err != nil {
		t.Fatalf("rebuilding the array with disk2 evacuated: %v", err)
	}
	beforeCatchAll, ok := h.Array.CatchAll.(pool.MountController)
	if !ok {
		t.Fatalf("CatchAll is %T, want pool.MountController", h.Array.CatchAll)
	}
	if !strings.Contains(beforeCatchAll.Mnt.What, "/mnt/disk2=NC") {
		t.Fatalf("catch-all What before cancel = %q, want /mnt/disk2=NC", beforeCatchAll.Mnt.What)
	}

	// Wraps parityReg.arrayReady with a counter after create-array's own
	// job already ran it once, so this only counts the call
	// CancelDiskRemoval itself makes through Handler.ArrayReady
	// (parityReg.callArrayReady) — proof the handler method really
	// reaches this wiring, not just that the disk's own row changed.
	inner := env.parityReg.arrayReady
	calls := 0
	env.parityReg.arrayReady = func(ctx context.Context) error {
		calls++
		return inner(ctx)
	}

	if err := h.CancelDiskRemoval(ctx, req); err != nil {
		t.Fatalf("CancelDiskRemoval: %v", err)
	}
	if calls != 1 {
		t.Fatalf("ArrayReady ran %d time(s) for CancelDiskRemoval, want exactly 1 — Handler.ArrayReady did not reach parityReg.callArrayReady", calls)
	}
	d, err := h.ArrayStore.GetDataDiskByMountpoint(ctx, "/mnt/disk2")
	if err != nil {
		t.Fatalf("GetDataDiskByMountpoint: %v", err)
	}
	if d.RemovalState != "" || d.RemovalJobID != "" {
		t.Fatalf("disk2 after cancel = %+v, want its removal state cleared", d)
	}
	afterCatchAll, ok := h.Array.CatchAll.(pool.MountController)
	if !ok {
		t.Fatalf("CatchAll after cancel is %T, want pool.MountController", h.Array.CatchAll)
	}
	if !strings.Contains(afterCatchAll.Mnt.What, "/mnt/disk2=RW") || strings.Contains(afterCatchAll.Mnt.What, "/mnt/disk2=NC") {
		t.Fatalf("catch-all What after cancel = %q, want /mnt/disk2=RW and no longer NC", afterCatchAll.Mnt.What)
	}
}

// TestParityRegistrar_Ensure_NoOpOnceAlreadyRegisteredAtStartup is the
// restart path #265 must leave unchanged: an array that already existed
// when the daemon started (register called once, directly, the way
// run() calls it before any ArrayReady hook exists yet) must never be
// re-registered by a later ArrayReady call — ensure is a no-op once
// done is true, whichever caller set it.
func TestParityRegistrar_Ensure_NoOpOnceAlreadyRegisteredAtStartup(t *testing.T) {
	_, env := newParityRegistrationEnv(t)
	if err := os.WriteFile(filepath.Join(env.configRoot, snapraidConfRelPath), []byte("placeholder"), 0o644); err != nil {
		t.Fatalf("writing placeholder snapraid.conf: %v", err)
	}
	engine, err := newSnapraidEngine(env.configRoot, env.parityReg.stateDir, nil)
	if err != nil {
		t.Fatalf("newSnapraidEngine: %v", err)
	}
	if engine == nil {
		t.Fatal("newSnapraidEngine returned nil for an existing snapraid.conf")
	}
	env.parityReg.register(engine)

	if err := env.parityReg.ensure(context.Background()); err != nil {
		t.Fatalf("ensure after startup registration: %v", err)
	}
	// No panic below proves ensure did not try to register a second time.
	assertAlreadyRegistered(t, env.registry, job.TypeSync)
}

// TestParityRegistrar_Ensure_ConcurrentCallsRegisterExactlyOnce is the
// acceptance criterion's own "no double-registration panic ... between
// the hook and a concurrent HTTP request or chain tick (go test -race)":
// several goroutines calling ensure concurrently — the shape a real
// daemon could hit if more than one ArrayReady-driven job somehow raced
// (disk add/replace/upgrade all share the same hook) — must never panic,
// and must leave every parity job type registered exactly once.
func TestParityRegistrar_Ensure_ConcurrentCallsRegisterExactlyOnce(t *testing.T) {
	_, env := newParityRegistrationEnv(t)
	if err := os.WriteFile(filepath.Join(env.configRoot, snapraidConfRelPath), []byte("placeholder"), 0o644); err != nil {
		t.Fatalf("writing placeholder snapraid.conf: %v", err)
	}

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = env.parityReg.ensure(context.Background())
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("ensure[%d]: %v", i, err)
		}
	}

	if engine, _, _, _ := env.handler.CurrentParity(); engine == nil {
		t.Fatal("CurrentParity: Parity is nil after concurrent ensure calls")
	}
	assertAlreadyRegistered(t, env.registry, job.TypeSync)
}

// TestParityRegistrar_Ensure_SafeForConcurrentCurrentParityReads proves
// CurrentParity/diffGuardHolder.get, read from goroutines standing in for
// concurrent HTTP requests and the schedule loop's own tick, race-detect
// clean against a concurrent ensure call standing in for the ArrayReady
// hook running on the create-array job's own goroutine (#265, mirroring
// #263's own Handler.SetArray/CurrentArray concurrency proof).
func TestParityRegistrar_Ensure_SafeForConcurrentCurrentParityReads(t *testing.T) {
	_, env := newParityRegistrationEnv(t)
	if err := os.WriteFile(filepath.Join(env.configRoot, snapraidConfRelPath), []byte("placeholder"), 0o644); err != nil {
		t.Fatalf("writing placeholder snapraid.conf: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					env.handler.CurrentParity()
					env.chainGuard.get()
				}
			}
		}()
	}

	if err := env.parityReg.ensure(context.Background()); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	close(stop)
	wg.Wait()

	if engine, _, _, _ := env.handler.CurrentParity(); engine == nil {
		t.Fatal("CurrentParity: Parity is nil after ensure")
	}
}
