//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never
// on the host: it is built with `go test -tags lab -c` from the host
// (compiling touches no device) and the resulting binary is run with
// `docker compose exec -T lab <binary>` inside the lab container. It is
// this fix round's own close for #265's blocking finding:
// TestParityRegistrar_WiresJobTypesAndHandlerFieldsAfterLiveArrayCreation
// (main_test.go) only proves TypeSync/TypeScrub/.../Handler's own
// parity-derived fields become registered — it never proves any of them
// actually reaches a real `snapraid sync`/`snapraid scrub` once
// reachable, and it drives StartRebalance, never TypeSync/TypeScrub
// themselves or the nightly maintenance chain. This file drives a real
// job.TypeDiskFormat submission — real loop devices, real mkfs.xfs, real
// mounts at the documented /mnt/parity1, /mnt/disk1, /mnt/disk2 (doc 01
// §6, the same mountpoint convention internal/job/disk_run_lab_test.go
// already mounts real loop devices at) — whose ArrayReady hook is built
// by newTopologyChangedHook (main.go), the exact function main.go's own
// run() builds topologyChanged from, wrapped only to rewrite
// snapraid.conf onto lab-safe content paths (this file's own
// p265WriteLabSnapraidConf, below) before the wrapped hook's own
// parityReg.ensure call opens it. So this test runs the production hook
// body unmodified, not a reimplementation of it, and then proves the
// daemon reaches a real `snapraid sync` and a real `snapraid scrub` with
// no restart: TypeSync succeeds through the scheduler, GetParity and
// RunParityDiff no longer 501, and MaintenanceChain.Run — built the same
// way scheduleRunner.tickChain builds it — actually runs its sync and
// scrub steps rather than skipping or failing them.
//
// The generated snapraid.conf itself is replaced before parityReg.ensure
// opens it: production's own Layout.Render (internal/parity/layout.go)
// always places its first content copy at parity.BootContentPath,
// `/var/lib/hoserva/snapraid.content` (Q18) — a path this lab, like every
// dev/test environment, must never write to (CLAUDE.md), the same reason
// internal/parity/snapraid_lab_test.go's own labEngine helper never uses
// Layout.Render either.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/api"
	cfggen "github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// p265Mountpoints are the real, production-hardcoded mountpoints
// (internal/job/disk_run.go's arrayDisksFromPlan) a create-array job
// assigns a single-parity, two-data plan.
var p265Mountpoints = []string{"/mnt/parity1", "/mnt/disk1", "/mnt/disk2"}

// p265CreateLoopImage is internal/job's own createLoopImage
// (disk_run_lab_test.go), reproduced here for the same reason every
// other lab helper in this package that mirrors one from a different
// package does: it is unexported there and this file cannot import it.
// truncate a fresh image under this lab's own $LAB/img, attach it to a
// loop device this lab owns, and register its detach — mkfs happens
// inside the real job.RunDiskFormat submission below, not here.
func p265CreateLoopImage(ctx context.Context, t *testing.T, r disk.Runner, lab, name, size string) string {
	t.Helper()

	imgDir := filepath.Join(lab, "img")
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", imgDir, err)
	}
	img := filepath.Join(imgDir, name+".img")
	if err := os.Remove(img); err != nil && !os.IsNotExist(err) {
		t.Fatalf("removing stale %s: %v", img, err)
	}

	if _, err := r.Run(ctx, "truncate", "-s", size, img); err != nil {
		t.Fatalf("truncate %s: %v", img, err)
	}

	out, err := r.Run(ctx, "losetup", "--find", "--show", img)
	if err != nil {
		t.Fatalf("losetup --find --show %s: %v", img, err)
	}
	dev := strings.TrimSpace(string(out))
	// Registered as soon as losetup --find --show returns successfully —
	// before either check below, which can itself t.Fatalf — so a loop
	// device this call actually attached is never leaked because a later
	// validation failed.
	t.Cleanup(func() {
		_, _ = r.Run(context.Background(), "losetup", "-d", dev)
	})
	if !strings.HasPrefix(dev, "/dev/loop") {
		t.Fatalf("losetup %s: got %q, want a /dev/loopN device", img, dev)
	}

	backing, err := r.Run(ctx, "losetup", "-j", img, "--output", "NAME", "--noheadings")
	if err != nil || strings.TrimSpace(string(backing)) != dev {
		t.Fatalf("losetup -j %s: got %q (err %v), want %q — refusing to trust a device this call did not just attach", img, backing, err, dev)
	}

	return dev
}

// p265UnmountIfMounted is internal/job's own unmountIfMounted
// (disk_run_lab_test.go): best-effort, never fails the test — a mount
// this test's own job never reached is already the clean state a
// t.Cleanup wants to leave behind.
func p265UnmountIfMounted(r disk.Runner, where string) {
	_, _ = r.Run(context.Background(), "umount", where)
}

// p265WriteLabSnapraidConf overwrites the snapraid.conf the live
// create-array job's own real Generator just wrote at configRoot with one
// that places its content copies on /mnt/parity1 and /mnt/disk2 instead
// of parity.BootContentPath — see this file's own header comment for
// why. Otherwise identical in shape to internal/parity's own labEngine/
// this package's own shareRelocLabEngine: parity, content, then data
// lines, over the real, now-mounted p265Mountpoints directories.
func p265WriteLabSnapraidConf(configRoot string) error {
	var conf strings.Builder
	fmt.Fprintf(&conf, "parity %s\n", filepath.Join(p265Mountpoints[0], "snapraid.parity"))
	fmt.Fprintf(&conf, "content %s\n", filepath.Join(p265Mountpoints[0], "snapraid.content"))
	fmt.Fprintf(&conf, "content %s\n", filepath.Join(p265Mountpoints[2], "snapraid.content"))
	fmt.Fprintf(&conf, "data d1 %s/\n", p265Mountpoints[1])
	fmt.Fprintf(&conf, "data d2 %s/\n", p265Mountpoints[2])
	return os.WriteFile(filepath.Join(configRoot, snapraidConfRelPath), []byte(conf.String()), 0o644)
}

// TestLabParityRegistrar_LiveArrayCreationReachesRealSyncAndMaintenanceChain
// is #265's own lab acceptance test: a daemon wiring built the way
// main.go builds it — parityRegistrar, chainGuard, Handler — with no
// array and no snapraid.conf, so parityEngine is nil exactly as it is at
// a freshly onboarded daemon's own startup. A live job.TypeDiskFormat
// submission (real loop devices, real mkfs.xfs, real mounts) runs the
// ArrayReady hook this issue's own parityReg.ensure wires — the same
// hook topologyChanged calls in main.go — with no restart in between.
// TypeSync must then succeed through the scheduler against a real
// `snapraid sync`, GetParity and RunParityDiff must no longer 501, and
// MaintenanceChain.Run — built the same shape scheduleRunner.tickChain
// builds it — must actually run its sync and scrub steps against a real
// `snapraid scrub` rather than skipping or failing them.
func TestLabParityRegistrar_LiveArrayCreationReachesRealSyncAndMaintenanceChain(t *testing.T) {
	lab := shareRelocLabDir(t)
	ctx := context.Background()
	exec := disk.CommandRunner{}
	provider := &disk.LinuxProvider{Lister: disk.NewLister(), Exec: exec}

	parityDev := p265CreateLoopImage(ctx, t, exec, lab, "p265-parity", "320M")
	data1Dev := p265CreateLoopImage(ctx, t, exec, lab, "p265-data1", "320M")
	data2Dev := p265CreateLoopImage(ctx, t, exec, lab, "p265-data2", "320M")
	loopSize := int64(320 << 20)

	// Registered after the image cleanups above, so t.Cleanup's LIFO order
	// unmounts these real mounts before the loop devices backing them are
	// detached.
	t.Cleanup(func() {
		p265UnmountIfMounted(exec, p265Mountpoints[1])
		p265UnmountIfMounted(exec, p265Mountpoints[2])
		p265UnmountIfMounted(exec, p265Mountpoints[0])
	})

	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "hoservad-p265-lab.db")
	db, err := sql.Open("sqlite", store.DSN(dbPath))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(ctx); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}

	arrays := store.NewArrayStore(db)
	shares := store.NewShareStore(db)
	registry := job.NewRegistry()
	scheduler := job.NewScheduler(job.NewStore(db), job.NewLogStore(t.TempDir()), job.NewHub(), registry)

	handler := &api.Handler{Scheduler: scheduler, Store: job.NewStore(db), Disks: provider, ArrayStore: arrays}
	chainGuard := &diffGuardHolder{}
	// configRoot lives under this test's own TempDir, never /etc — only
	// the array's own mountpoints (hardcoded to /mnt/parityN/diskN, doc
	// 01 §6) are real, container-local paths; the generated snapraid.conf
	// itself is not.
	configRoot := t.TempDir()
	generator := cfggen.NewGenerator(configRoot)
	parityReg := &parityRegistrar{
		configRoot: configRoot,
		stateDir:   t.TempDir(),
		db:         db,
		registry:   registry,
		handler:    handler,
		shareStore: shares,
		arrayStore: arrays,
		chainGuard: chainGuard,
	}

	// shareService and rebuildArraySequence are newTopologyChangedHook's
	// (main.go) other two dependencies, built the same way run() builds
	// them: a real share.Service — Mounter and Usages left nil, since
	// ApplyTopology never dereferences either while the pool.CatchAllPath
	// catch-all isn't mounted, which it never is in this test — and a
	// rebuildArraySequence closure over the same newArraySequence call
	// run() makes. Building both real, rather than stubbing the hook's
	// dependencies out, means hook below is the exact function main.go
	// builds topologyChanged from, not a narrower stand-in for it.
	shareService := newShareService(shares, arrays, generator, nil, nil)
	rebuildArraySequence := func(ctx context.Context) error {
		seq, err := newArraySequence(ctx, scheduler, arrays, shares, provider, exec)
		if err != nil {
			return err
		}
		handler.SetArray(seq)
		return nil
	}
	hook := newTopologyChangedHook(shareService, rebuildArraySequence, parityReg, handler)

	// Before any array exists: parityEngine is nil at "startup" exactly as
	// it is on a freshly onboarded daemon (main.go's own `if parityEngine
	// != nil` gate never ran), so TypeSync is unreachable.
	if _, err := scheduler.Submit(ctx, job.TypeSync, nil, nil); err == nil {
		t.Fatal("Submit(TypeSync) succeeded before any array exists — want ErrJobTypeNotRegistered")
	}
	if engine, _, _, _ := handler.CurrentParity(); engine != nil {
		t.Fatal("CurrentParity: Parity is already set before any array exists")
	}

	registry.Register(job.TypeDiskFormat, false, job.RunDiskFormat(job.DiskFormatDeps{
		Provider:  provider,
		Runner:    exec,
		Store:     arrays,
		Generator: generator,
		Mounter:   disk.DirectMounter{Runner: exec},
		// hook is newTopologyChangedHook's own return value — the exact
		// ArrayReady hook main.go's run() builds topologyChanged from —
		// wrapped only to rewrite snapraid.conf onto lab-safe content
		// paths before hook's own parityReg.ensure call opens it; see
		// the file header comment for why the rewrite is needed.
		ArrayReady: func(ctx context.Context) error {
			if err := p265WriteLabSnapraidConf(configRoot); err != nil {
				return fmt.Errorf("rewriting lab snapraid.conf: %w", err)
			}
			return hook(ctx)
		},
	}))

	plan := job.DiskFormatParams{
		Parity: []disk.AssignedDisk{{Device: parityDev, Filesystem: disk.XFS}},
		Data: []disk.AssignedDisk{
			{Device: data1Dev, Filesystem: disk.XFS},
			{Device: data2Dev, Filesystem: disk.XFS},
		},
		Sizes:        map[string]int64{parityDev: loopSize, data1Dev: loopSize, data2Dev: loopSize},
		CreatePolicy: "mfs",
		MinFreeSpace: "1M",
	}
	plan.Confirmation = plan.Plan().Confirmation()
	params, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshaling DiskFormatParams: %v", err)
	}

	j, err := scheduler.Submit(ctx, job.TypeDiskFormat, nil, params)
	if err != nil {
		t.Fatalf("Submit(TypeDiskFormat): %v", err)
	}
	finished, err := scheduler.Await(ctx, j.ID)
	if err != nil {
		t.Fatalf("Await(create-array): %v", err)
	}
	if finished.Status != job.StatusSucceeded {
		t.Fatalf("create-array job status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}

	// No restart between the live create-array job above and everything
	// below — this is #265's own regression.
	engine, _, _, _ := handler.CurrentParity()
	if engine == nil {
		t.Fatal("CurrentParity: Parity is nil after a live array creation — TypeSync/GetParity/RunParityDiff would still 501/job_type_not_registered until a restart (#265)")
	}

	// A real file on a real, now-mounted data disk: an empty array's own
	// first-ever scrub refuses with "the array appears to be empty"
	// (real snapraid 12.4-1, confirmed against this exact lab image) —
	// distinct from the case #265 fixes, so this proves the maintenance
	// chain's scrub step against ordinary array contents, not that edge.
	if err := os.WriteFile(filepath.Join(p265Mountpoints[1], "p265.txt"), []byte("hoserva #265 lab test\n"), 0o644); err != nil {
		t.Fatalf("writing a file to %s: %v", p265Mountpoints[1], err)
	}

	syncJob, err := scheduler.Submit(ctx, job.TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit(TypeSync) after a live array creation: %v — TypeSync must already be registered with no restart", err)
	}
	syncFinished, err := scheduler.Await(ctx, syncJob.ID)
	if err != nil {
		t.Fatalf("Await(sync): %v", err)
	}
	if syncFinished.Status != job.StatusSucceeded {
		t.Fatalf("sync job status = %s (%s), want succeeded — a real `snapraid sync` against the freshly created array", syncFinished.Status, syncFinished.ErrorMessage)
	}

	if _, err := handler.GetParity(ctx); err != nil {
		t.Fatalf("GetParity after a live array creation and sync: %v — want a real parity snapshot, not 501 not_configured", err)
	}
	if _, err := handler.RunParityDiff(ctx); err != nil {
		t.Fatalf("RunParityDiff after a live array creation and sync: %v — want a real diff, not 501 not_configured", err)
	}

	// Drive the nightly maintenance chain exactly the shape
	// scheduleRunner.tickChain builds it (schedule.go) — Weekly forced
	// true so the scrub step actually runs rather than being skipped for
	// not being the weekly day, proving both job-backed steps this fix
	// wires reach a real snapraid invocation, not just registration.
	chain := &job.MaintenanceChain{
		Scheduler: scheduler,
		Guard:     chainGuard.get(),
		Weekly:    true,
	}
	if chain.Guard == nil {
		t.Fatal("chainGuard.get() is nil after a live array creation — the nightly chain would keep silently no-op'ing (#265)")
	}
	result, err := chain.Run(ctx)
	if err != nil {
		t.Fatalf("MaintenanceChain.Run: %v", err)
	}
	if result.Blocked {
		t.Fatal("MaintenanceChain.Run reported Blocked — want the threshold guard to pass against a freshly created, freshly synced array")
	}
	var sawSync, sawScrub bool
	for _, step := range result.Steps {
		switch step.Step {
		case job.StepSync:
			sawSync = true
			if step.Skipped {
				t.Fatal("MaintenanceChain.Run skipped the sync step — TypeSync must be registered by parityReg.ensure with no restart")
			}
			if step.Status != job.StatusSucceeded {
				t.Fatalf("MaintenanceChain.Run sync step status = %s (%s), want succeeded", step.Status, step.Err)
			}
		case job.StepScrub:
			sawScrub = true
			if step.Skipped {
				t.Fatal("MaintenanceChain.Run skipped the scrub step — want it to run on the forced weekly day")
			}
			if step.Status != job.StatusSucceeded {
				t.Fatalf("MaintenanceChain.Run scrub step status = %s (%s), want succeeded", step.Status, step.Err)
			}
		}
	}
	if !sawSync {
		t.Fatal("MaintenanceChain.Run reported no sync step at all")
	}
	if !sawScrub {
		t.Fatal("MaintenanceChain.Run reported no scrub step at all")
	}
}
