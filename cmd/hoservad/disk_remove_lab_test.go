//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never
// on the host: it is built with `go test -tags lab -c` from the host
// (compiling touches no device) and the resulting binary is run with
// `docker compose exec -T lab <binary>` inside the lab container.
//
// It covers #358, doc 09 §4 steps 7-9 — finishing a disk's removal —
// against a real array: four loop disks formatted and mounted at /mnt/
// parity1 and /mnt/disk1-3 by a real create-array job, a real mergerfs
// pool, and the real snapraid binary reading the snapraid.conf the
// daemon generates. The daemon is built by p358StartDaemon through the
// functions run() calls: openDatabase/applyMigrations, the scheduler and
// RecoverFromRestart, newArraySequence, newSnapraidEngine and
// parityRegistrar.register (which registers the disk_remove job under
// test), newShareService and wireTopologyHooks. Evacuation and the
// finish go through the Handler's own evacuateDisk and
// finishDiskRemoval.
//
// What differs from a production daemon, all because of the lab:
//   - There is no init system, so disks are mounted and a data disk's
//     own unit is stopped with disk.DirectMounter (mount/umount by UUID)
//     instead of disk.SystemdMounter, and the pool is first brought up
//     with pool.Mounter from the mounts newArraySequence built. From
//     then on the topology hooks drive the running pool themselves.
//   - The generated snapraid.conf places its first content copy at
//     parity.BootContentPath (Q18). The daemon must regenerate that file
//     itself for this issue, so it is used as generated; the directory
//     is this lab container's own, never the host's, reset per test the
//     way internal/job's own lab tests reset it (resetBootContentDir).
//   - The snapraid engine's runner is wrapped (p358SyncCounter) to count
//     every sync the daemon runs and, for the interruption test, to stop
//     one sync before it starts.

package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	cfggen "github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/share"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

const (
	p358Share  = "p358share"
	p358Parity = "/mnt/parity1"
	p358Disk1  = "/mnt/disk1"
	p358Disk2  = "/mnt/disk2"
	p358Disk3  = "/mnt/disk3"
)

// p358SyncCounter wraps the real snapraid runner. It counts every sync
// started and every one that exited cleanly, and refuses the next
// failNext syncs before they start — a daemon killed between two steps.
type p358SyncCounter struct {
	mu        sync.Mutex
	failNext  int
	started   int
	succeeded int
}

func (c *p358SyncCounter) Start(ctx context.Context, name string, args ...string) (parity.Process, error) {
	isSync := len(args) > 0 && args[len(args)-1] == "sync"
	if isSync {
		c.mu.Lock()
		if c.failNext > 0 {
			c.failNext--
			c.mu.Unlock()
			return nil, errors.New("p358: the daemon stopped before this sync started")
		}
		c.started++
		c.mu.Unlock()
	}
	p, err := parity.CommandRunner{}.Start(ctx, name, args...)
	if err != nil || !isSync {
		return p, err
	}
	return &p358CountedProcess{Process: p, c: c}, nil
}

func (c *p358SyncCounter) counts() (started, succeeded int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.started, c.succeeded
}

type p358CountedProcess struct {
	parity.Process
	c *p358SyncCounter
}

func (p *p358CountedProcess) Wait() error {
	err := p.Process.Wait()
	if err == nil {
		p.c.mu.Lock()
		p.c.succeeded++
		p.c.mu.Unlock()
	}
	return err
}

// p358Daemon is the part of a running hoservad #358 touches, built the
// way run() builds it (this file's header).
type p358Daemon struct {
	db        *sql.DB
	arrays    *store.ArrayStore
	shares    *store.ShareStore
	scheduler *job.Scheduler
	handler   *api.Handler
	rebuild   func(ctx context.Context) error
}

func p358StartDaemon(t *testing.T, stateDir, configRoot string, counter *p358SyncCounter) *p358Daemon {
	t.Helper()
	ctx := context.Background()
	exec := disk.CommandRunner{}

	db, err := openDatabase(stateDir)
	if err != nil {
		t.Fatalf("openDatabase: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := applyMigrations(ctx, db, stateDir); err != nil {
		t.Fatalf("applyMigrations: %v", err)
	}
	registry := job.NewRegistry()
	jobStore := job.NewStore(db)
	scheduler := job.NewScheduler(jobStore, job.NewLogStore(filepath.Join(stateDir, "logs")), job.NewHub(), registry)
	if err := scheduler.RecoverFromRestart(ctx); err != nil {
		t.Fatalf("RecoverFromRestart: %v", err)
	}
	arrays := store.NewArrayStore(db)
	shares := store.NewShareStore(db)
	provider := &disk.LinuxProvider{Lister: disk.NewLister(), Exec: exec}
	handler := &api.Handler{Scheduler: scheduler, Store: jobStore, ArrayStore: arrays, Disks: provider}
	rebuild := func(ctx context.Context) error {
		seq, err := newArraySequence(ctx, scheduler, arrays, shares, provider, exec)
		if err != nil {
			return err
		}
		handler.SetArray(seq)
		return nil
	}
	if err := rebuild(ctx); err != nil {
		t.Fatalf("newArraySequence at startup: %v", err)
	}

	generator := cfggen.NewGenerator(configRoot)
	parityReg := &parityRegistrar{
		configRoot: configRoot,
		stateDir:   stateDir,
		db:         db,
		registry:   registry,
		handler:    handler,
		shareStore: shares,
		arrayStore: arrays,
		chainGuard: &diffGuardHolder{},
		generator:  generator,
		diskUnits:  disk.DirectMounter{Runner: exec},
		mounts:     disk.KernelMounts{Runner: exec},
	}
	engine, err := newSnapraidEngine(configRoot, stateDir, counter)
	if err != nil {
		t.Fatalf("newSnapraidEngine: %v", err)
	}
	var usages share.UsageReader
	if engine != nil {
		parityReg.register(engine)
		usages = engine.Usage
	}
	shareService := newShareService(shares, arrays, generator, pool.SystemdMounter{Runner: exec}, usages)
	shareService.PostCommit = rebuild
	topologyChanged := wireTopologyHooks(shareService, rebuild, parityReg, handler)
	registry.Register(job.TypeDiskFormat, false, job.RunDiskFormat(job.DiskFormatDeps{
		Provider:   provider,
		Runner:     exec,
		Store:      arrays,
		Generator:  generator,
		Mounter:    disk.DirectMounter{Runner: exec},
		ArrayReady: topologyChanged,
	}))
	return &p358Daemon{db: db, arrays: arrays, shares: shares, scheduler: scheduler, handler: handler, rebuild: rebuild}
}

// engine is the daemon's own parity engine, the one every parity job it
// registered runs through.
func (d *p358Daemon) engine(t *testing.T) *parity.SnapraidEngine {
	t.Helper()
	eng, _, _, _ := d.handler.CurrentParity()
	se, ok := eng.(*parity.SnapraidEngine)
	if !ok || se == nil {
		t.Fatalf("CurrentParity engine is %T, want a *parity.SnapraidEngine", eng)
	}
	return se
}

func (d *p358Daemon) await(t *testing.T, id string) *job.Job {
	t.Helper()
	finished, err := d.scheduler.Await(context.Background(), id)
	if err != nil {
		t.Fatalf("Await(%s): %v", id, err)
	}
	return finished
}

func (d *p358Daemon) removalState(t *testing.T, mountpoint string) string {
	t.Helper()
	row, err := d.arrays.GetDataDiskByMountpoint(context.Background(), mountpoint)
	if err != nil {
		t.Fatalf("GetDataDiskByMountpoint(%s): %v", mountpoint, err)
	}
	return row.RemovalState
}

func (d *p358Daemon) finish(t *testing.T) *job.Job {
	t.Helper()
	j, err := d.handler.FinishDiskRemoval(context.Background(), &apiv1.FinishDiskRemovalRequest{Mountpoint: p358Disk2, Confirmation: job.EvacuationConfirmation(p358Disk2)})
	if err != nil {
		t.Fatalf("FinishDiskRemoval: %v", err)
	}
	return d.await(t, j.ID.String())
}

// p358Env is one test's array: the daemon's state and config
// directories, the counter every sync goes through, and the sha256 of
// every file written to disk1 and disk3.
type p358Env struct {
	stateDir, configRoot string
	counter              *p358SyncCounter
	d                    *p358Daemon
	files                map[string]string
}

func p358ResetBootContentDir(t *testing.T) {
	t.Helper()
	dir := filepath.Dir(parity.BootContentPath)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("removing stale %s: %v", dir, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
}

func p358Sha(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// p358Setup creates the array through a real create-array job, one
// array-only share, a mounted pool, files on every data disk, and a
// first sync. disk2 is then evacuated through evacuateDisk.
func p358Setup(t *testing.T, name string) *p358Env {
	t.Helper()
	lab := shareRelocLabDir(t)
	ctx := context.Background()
	exec := disk.CommandRunner{}
	p358ResetBootContentDir(t)
	p359ClaimCatchAllPath(t)

	devs := map[string]string{}
	for _, role := range []string{"parity", "data1", "data2", "data3"} {
		devs[role] = p265CreateLoopImage(ctx, t, exec, lab, name+"-"+role, "400M")
	}
	t.Cleanup(func() {
		for _, m := range []string{p358Disk1, p358Disk2, p358Disk3, p358Parity} {
			p265UnmountIfMounted(exec, m)
		}
	})

	env := &p358Env{stateDir: t.TempDir(), configRoot: t.TempDir(), counter: &p358SyncCounter{}, files: map[string]string{}}
	env.d = p358StartDaemon(t, env.stateDir, env.configRoot, env.counter)

	size := int64(400 << 20)
	format := job.DiskFormatParams{
		Parity: []disk.AssignedDisk{{Device: devs["parity"], Filesystem: disk.XFS}},
		Data: []disk.AssignedDisk{
			{Device: devs["data1"], Filesystem: disk.XFS},
			{Device: devs["data2"], Filesystem: disk.XFS},
			{Device: devs["data3"], Filesystem: disk.XFS},
		},
		Sizes:        map[string]int64{devs["parity"]: size, devs["data1"]: size, devs["data2"]: size, devs["data3"]: size},
		CreatePolicy: "mfs",
		MinFreeSpace: "1M",
	}
	format.Confirmation = format.Plan().Confirmation()
	params, err := json.Marshal(format)
	if err != nil {
		t.Fatalf("marshaling DiskFormatParams: %v", err)
	}
	created, err := env.d.scheduler.Submit(ctx, job.TypeDiskFormat, nil, params)
	if err != nil {
		t.Fatalf("Submit(disk_format): %v", err)
	}
	if finished := env.d.await(t, created.ID); finished.Status != job.StatusSucceeded {
		t.Fatalf("create-array = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	env.d.engine(t).Runner = env.counter

	if env.d.handler.CurrentArray() == nil {
		t.Fatal("no array sequence after create-array")
	}
	created2 := time.Now().UTC()
	if err := env.d.shares.Insert(ctx, store.Share{Name: p358Share, CacheMode: "array-only", CreatePolicy: "mfs", CreatedAt: created2, UpdatedAt: created2}); err != nil {
		t.Fatalf("inserting share: %v", err)
	}
	for _, m := range []string{p358Disk1, p358Disk2, p358Disk3} {
		if err := os.MkdirAll(filepath.Join(m, p358Share), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	if err := env.d.rebuild(ctx); err != nil {
		t.Fatalf("rebuilding the array sequence: %v", err)
	}
	mounter := pool.Mounter{Runner: exec}
	seq := env.d.handler.CurrentArray()
	for _, m := range append([]job.ArrayMount{seq.CatchAll}, seq.ShareMounts...) {
		mc, ok := m.(pool.MountController)
		if !ok {
			t.Fatalf("pool mount %s is %T, want pool.MountController", m.Where(), m)
		}
		p359MountAndTrack(t, mounter, mc.Mnt)
	}

	for _, spec := range []struct {
		mount, prefix string
		n             int
	}{{p358Disk1, "a", 8}, {p358Disk2, "b", 5}, {p358Disk3, "c", 8}} {
		for i := 0; i < spec.n; i++ {
			path := filepath.Join(spec.mount, p358Share, fmt.Sprintf("%s-%02d.bin", spec.prefix, i))
			shareRelocLabWriteFile(t, path, 100_000)
			if spec.mount != p358Disk2 {
				env.files[path] = p358Sha(t, path)
			}
		}
	}
	env.sync(t, false)

	plan, err := env.d.handler.PlanDiskEvacuation(ctx, &apiv1.EvacuateDiskPlanRequest{Mountpoint: p358Disk2})
	if err != nil {
		t.Fatalf("PlanDiskEvacuation: %v", err)
	}
	if len(plan.Moves) != 5 {
		t.Fatalf("evacuation plan moves %d files, want disk2's 5", len(plan.Moves))
	}
	evac, err := env.d.handler.EvacuateDisk(ctx, &apiv1.EvacuateDiskRequest{Mountpoint: p358Disk2, Confirmation: plan.Confirmation})
	if err != nil {
		t.Fatalf("EvacuateDisk: %v", err)
	}
	if finished := env.d.await(t, evac.ID.String()); finished.Status != job.StatusSucceeded {
		t.Fatalf("evacuation = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	if state := env.d.removalState(t, p358Disk2); state != store.RemovalStateEvacuated {
		t.Fatalf("disk2 after the evacuation = %q, want evacuated", state)
	}
	// Every file that was on disk2 now lives on disk1 or disk3.
	for _, m := range plan.Moves {
		path := filepath.Join(m.TargetBranch, m.RelPath)
		env.files[path] = p358Sha(t, path)
	}
	return env
}

// sync runs the daemon's own sync job; confirm is the guard's "review
// the diff and sync anyway".
func (e *p358Env) sync(t *testing.T, confirm bool) {
	t.Helper()
	body, err := json.Marshal(job.SyncParams{Confirm: confirm})
	if err != nil {
		t.Fatal(err)
	}
	j, err := e.d.scheduler.Submit(context.Background(), job.TypeSync, nil, body)
	if err != nil {
		t.Fatalf("Submit(sync): %v", err)
	}
	if finished := e.d.await(t, j.ID); finished.Status != job.StatusSucceeded {
		t.Fatalf("sync = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
}

// restart closes this daemon's database and starts a new one over the
// same state and config directories. The pool, like a real one under
// systemd, stays mounted across the restart.
func (e *p358Env) restart(t *testing.T) {
	t.Helper()
	if err := e.d.db.Close(); err != nil {
		t.Fatalf("closing the first daemon's database: %v", err)
	}
	e.d = p358StartDaemon(t, e.stateDir, e.configRoot, e.counter)
}

func (e *p358Env) snapraidConf(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(e.configRoot, snapraidConfRelPath))
	if err != nil {
		t.Fatalf("reading snapraid.conf: %v", err)
	}
	return string(body)
}

// p358LiveBranches reads the branches the running mergerfs mount at
// where has right now, from its runtime control file.
func p358LiveBranches(t *testing.T, where string) []string {
	t.Helper()
	buf := make([]byte, 8192)
	n, err := syscall.Getxattr(filepath.Join(where, ".mergerfs"), "user.mergerfs.branches", buf)
	if err != nil {
		t.Fatalf("reading the live branches of %s: %v", where, err)
	}
	var out []string
	for _, entry := range strings.Split(string(buf[:n]), ":") {
		path, _, _ := strings.Cut(entry, "=")
		out = append(out, path)
	}
	return out
}

// requireRemoved is the end state every scenario must reach: disk2 is
// unmounted, gone from the array, from every live and generated branch
// list and from snapraid.conf, where disk1 and disk3 keep their own d1
// and d3; SnapRAID has nothing to sync and its check passes; and a file
// deleted from disk1 comes back byte for byte from parity.
func (e *p358Env) requireRemoved(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := e.d.arrays.GetDataDiskByMountpoint(ctx, p358Disk2); !errors.Is(err, store.ErrArrayDiskNotFound) {
		t.Fatalf("disk2's row = %v, want gone", err)
	}
	if mounted, err := disk.IsMountpoint(p358Disk2); err != nil || mounted {
		t.Fatalf("disk2 mounted = (%v, %v), want unmounted", mounted, err)
	}
	for _, where := range []string{pool.CatchAllPath, pool.SharePath(p358Share), pool.MoverTargetPath(p358Share)} {
		branches := p358LiveBranches(t, where)
		for _, b := range branches {
			if b == p358Disk2 || strings.HasPrefix(b, p358Disk2+"/") {
				t.Fatalf("live mount %s still branches onto %s: %v", where, b, branches)
			}
		}
		if len(branches) != 2 {
			t.Fatalf("live mount %s branches = %v, want disk1's and disk3's", where, branches)
		}
	}
	units, err := filepath.Glob(filepath.Join(e.configRoot, "systemd/system", "*.mount"))
	if err != nil {
		t.Fatal(err)
	}
	for _, unit := range units {
		body, err := os.ReadFile(unit)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), p358Disk2) {
			t.Fatalf("generated unit %s still names disk2:\n%s", unit, body)
		}
	}
	conf := e.snapraidConf(t)
	if strings.Contains(conf, p358Disk2) {
		t.Fatalf("snapraid.conf still names disk2:\n%s", conf)
	}
	for _, want := range []string{"data d1 " + p358Disk1 + "/\n", "data d3 " + p358Disk3 + "/\n"} {
		if !strings.Contains(conf, want) {
			t.Fatalf("snapraid.conf lacks %q — a surviving disk was renamed:\n%s", want, conf)
		}
	}

	engine := e.d.engine(t)
	if out, err := (disk.CommandRunner{}).Run(ctx, "snapraid", "-c", engine.ConfPath, "status"); err != nil {
		t.Fatalf("snapraid status after the removal: %v\n%s", err, out)
	}
	report, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("snapraid diff after the removal: %v", err)
	}
	if report.Added+report.Removed+report.Updated+report.Moved+report.Copied != 0 {
		t.Fatalf("snapraid diff after the removal = %+v, want nothing to sync", report)
	}
	for _, m := range []string{p358Disk1, p358Disk3} {
		entries, err := os.ReadDir(filepath.Join(m, p358Share))
		if err != nil {
			t.Fatal(err)
		}
		if got := report.PerDisk[m].FilesBefore; got != len(entries) {
			t.Fatalf("SnapRAID tracks %d files on %s, want its %d — a surviving disk lost tracked files", got, m, len(entries))
		}
	}
	if out, err := (disk.CommandRunner{}).Run(ctx, "snapraid", "-c", engine.ConfPath, "check"); err != nil {
		t.Fatalf("snapraid check after the removal: %v\n%s", err, out)
	}

	victim := filepath.Join(p358Disk1, p358Share, "a-03.bin")
	want := e.files[victim]
	if want == "" {
		t.Fatalf("no recorded hash for %s", victim)
	}
	if err := os.Remove(victim); err != nil {
		t.Fatalf("deleting %s: %v", victim, err)
	}
	one := 1
	body, err := json.Marshal(job.FixParams{Confirm: true, Disk: &one})
	if err != nil {
		t.Fatal(err)
	}
	fj, err := e.d.scheduler.Submit(ctx, job.TypeFix, nil, body)
	if err != nil {
		t.Fatalf("Submit(fix -d 1): %v", err)
	}
	if finished := e.d.await(t, fj.ID); finished.Status != job.StatusSucceeded {
		t.Fatalf("fix = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	if got := p358Sha(t, victim); got != want {
		t.Fatalf("restored %s sha256 = %s, want %s", victim, got, want)
	}
}

// TestLabDiskRemove_EvacuatedDiskLeavesTheArray_ParityStillRecovers is
// the acceptance criterion's data-loss scenario: disk2 of three is
// evacuated through evacuateDisk, then finishDiskRemoval runs the
// registered job, which succeeds with one step-8 sync and names the disk
// safe to remove — and requireRemoved's end state holds.
func TestLabDiskRemove_EvacuatedDiskLeavesTheArray_ParityStillRecovers(t *testing.T) {
	e := p358Setup(t, "p358ok")
	_, before := e.counter.counts()
	j := e.d.finish(t)
	if j.Status != job.StatusSucceeded {
		t.Fatalf("disk_remove = %s (%s), want succeeded", j.Status, j.ErrorMessage)
	}
	if _, after := e.counter.counts(); after-before != 1 {
		t.Fatalf("%d successful syncs during the removal, want exactly 1", after-before)
	}
	e.requireRemoved(t)
}

// TestLabDiskRemove_GuardTrip_StopsUnpooledWithNothingSynced is the guard
// scenario: an unrelated mass deletion on disk1 is pending when the
// finish runs. The step-8 sync is blocked by the guard, the job fails
// with disk2 unpooled, still mounted and still in snapraid.conf, and the
// deletion is still unsynced. Once the change is reviewed (a confirmed
// sync), a re-run completes.
func TestLabDiskRemove_GuardTrip_StopsUnpooledWithNothingSynced(t *testing.T) {
	ctx := context.Background()
	e := p358Setup(t, "p358guard")
	for i := 4; i < 8; i++ {
		path := filepath.Join(p358Disk1, p358Share, fmt.Sprintf("a-%02d.bin", i))
		if err := os.Remove(path); err != nil {
			t.Fatalf("deleting %s: %v", path, err)
		}
		delete(e.files, path)
	}
	_, before := e.counter.counts()

	j := e.d.finish(t)
	if j.Status != job.StatusFailed || !strings.Contains(j.ErrorMessage, "threshold guard blocked the sync") {
		t.Fatalf("disk_remove = %s (%s), want failed by the threshold guard", j.Status, j.ErrorMessage)
	}
	if _, after := e.counter.counts(); after != before {
		t.Fatalf("%d syncs succeeded past the tripped guard", after-before)
	}
	if state := e.d.removalState(t, p358Disk2); state != store.RemovalStateUnpooled {
		t.Fatalf("disk2 after the guard trip = %q, want unpooled", state)
	}
	if mounted, err := disk.IsMountpoint(p358Disk2); err != nil || !mounted {
		t.Fatalf("disk2 mounted = (%v, %v), want still mounted", mounted, err)
	}
	if !strings.Contains(e.snapraidConf(t), "data d2 "+p358Disk2+"/\n") {
		t.Fatal("disk2 left snapraid.conf past the tripped guard")
	}
	report, err := e.d.engine(t).Diff(ctx)
	if err != nil {
		t.Fatalf("snapraid diff: %v", err)
	}
	if report.Removed != 4 {
		t.Fatalf("pending removals = %d, want disk1's 4 still unsynced", report.Removed)
	}

	e.sync(t, true)
	again := e.d.finish(t)
	if again.Status != job.StatusSucceeded {
		t.Fatalf("re-run = %s (%s), want succeeded", again.Status, again.ErrorMessage)
	}
	e.requireRemoved(t)
}

// TestLabDiskRemove_StoppedAfterStep7_RestartedAndFinished stops the
// daemon between step 7 and the step-8 sync, restarts it over the same
// database, and finishes the removal from there: exactly one successful
// step-8 sync across both runs.
func TestLabDiskRemove_StoppedAfterStep7_RestartedAndFinished(t *testing.T) {
	e := p358Setup(t, "p358stop7")
	_, before := e.counter.counts()
	e.counter.mu.Lock()
	e.counter.failNext = 1
	e.counter.mu.Unlock()

	j := e.d.finish(t)
	if j.Status != job.StatusFailed || !strings.Contains(j.ErrorMessage, "stopped before this sync started") {
		t.Fatalf("disk_remove = %s (%s), want stopped before the sync", j.Status, j.ErrorMessage)
	}
	if state := e.d.removalState(t, p358Disk2); state != store.RemovalStateUnpooled {
		t.Fatalf("disk2 after the stop = %q, want unpooled", state)
	}
	if !strings.Contains(e.snapraidConf(t), "data d2 "+p358Disk2+"/\n") {
		t.Fatal("disk2 left snapraid.conf before its sync")
	}
	for _, b := range p358LiveBranches(t, pool.CatchAllPath) {
		if b == p358Disk2 {
			t.Fatal("step 7 did not take disk2 out of the running catch-all")
		}
	}

	e.restart(t)
	if state := e.d.removalState(t, p358Disk2); state != store.RemovalStateUnpooled {
		t.Fatalf("disk2 after the restart = %q, want unpooled", state)
	}
	again := e.d.finish(t)
	if again.Status != job.StatusSucceeded {
		t.Fatalf("re-run = %s (%s), want succeeded", again.Status, again.ErrorMessage)
	}
	if _, after := e.counter.counts(); after-before != 1 {
		t.Fatalf("%d successful step-8 syncs across both runs, want exactly 1", after-before)
	}
	e.requireRemoved(t)
}

// TestLabDiskRemove_StoppedAfterTheSync_RestartedAndFinished stops the
// job once the step-8 sync has run and snapraid.conf has been
// regenerated without disk2 — its unmount fails, because something holds
// the disk open — restarts the daemon, and finishes the removal: the
// re-run never syncs again.
func TestLabDiskRemove_StoppedAfterTheSync_RestartedAndFinished(t *testing.T) {
	e := p358Setup(t, "p358stop8")
	started, before := e.counter.counts()
	holder, err := os.Open(filepath.Join(p358Disk2, p358Share))
	if err != nil {
		t.Fatalf("holding disk2 open: %v", err)
	}
	t.Cleanup(func() { _ = holder.Close() })

	j := e.d.finish(t)
	if j.Status != job.StatusFailed || !strings.Contains(j.ErrorMessage, "unmounting "+p358Disk2) {
		t.Fatalf("disk_remove = %s (%s), want the busy unmount to stop it", j.Status, j.ErrorMessage)
	}
	if state := e.d.removalState(t, p358Disk2); state != store.RemovalStateUnlisted {
		t.Fatalf("disk2 after the stop = %q, want unlisted", state)
	}
	if strings.Contains(e.snapraidConf(t), p358Disk2) {
		t.Fatal("snapraid.conf was not regenerated without disk2 before step 9")
	}
	syncsStarted, _ := e.counter.counts()
	if syncsStarted-started != 1 {
		t.Fatalf("%d syncs started in the first run, want 1", syncsStarted-started)
	}
	if err := holder.Close(); err != nil {
		t.Fatalf("releasing disk2: %v", err)
	}

	e.restart(t)
	again := e.d.finish(t)
	if again.Status != job.StatusSucceeded {
		t.Fatalf("re-run = %s (%s), want succeeded", again.Status, again.ErrorMessage)
	}
	if now, after := e.counter.counts(); now != syncsStarted || after-before != 1 {
		t.Fatalf("syncs started %d -> %d, succeeded %d during the removal — the re-run synced again", syncsStarted, now, after-before)
	}
	e.requireRemoved(t)
}
