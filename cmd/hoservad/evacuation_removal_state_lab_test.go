//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never
// on the host: it is built with `go test -tags lab -c` from the host
// (compiling touches no device) and the resulting binary is run with
// `docker compose exec -T lab <binary>` inside the lab container, the
// same pattern evacuation_lab_test.go and parity_registrar_lab_test.go
// use in this package.
//
// It covers #359, doc 09 §4 step 2 — the disk being evacuated is
// no-create in every pool mount from before the first copy — against a
// real mergerfs pool that is mounted and taking writes before the job
// starts. The daemon side is built by p359StartDaemon through the
// functions main.go's run() calls: openDatabase/applyMigrations, the job
// scheduler and RecoverFromRestart, newArraySequence, newShareService,
// parityRegistrar.register and wireTopologyHooks. So the evacuation job
// is the one parityRegistrar.register registers, its ArrayReady is the
// hook wireTopologyHooks binds, and every mount's branches, options and
// create policy come from newArraySequence.
//
// Two things differ from a production daemon, both because of the lab:
//   - The lab has no init system, so the pool is first brought up by
//     pool.Mounter (direct mergerfs exec) with the pool.Mount values
//     newArraySequence built, instead of by starting their systemd units.
//     From then on the hook drives the running pool itself, through the
//     SystemdMounter newArraySequence builds: for a mount that is already
//     up, that is findmnt plus mergerfs's runtime xattrs, no systemctl.
//   - The snapraid engine is a lab engine over the same data disks
//     (p359LabEngine), not one opened from the generated snapraid.conf,
//     whose first content file is on the boot disk (Q18) — the same
//     reason parity_registrar_lab_test.go rewrites that file.
//
// Every test uses its own loop disks, never disk1-3 of `make lab-up`'s
// standing array. disk1 is three times the size of disk2/disk3, so under
// the array's `mfs` create policy a new file lands on disk1 unless disk1
// is no-create.

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/cache"
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
	p359Share = "p359share"
	// p359TopDir is where the catch-all writer creates files: a top-level
	// directory that is not a share.
	p359TopDir = "p359-top"
	// p359ShareWriteDir is where the share writer creates files, inside
	// the share.
	p359ShareWriteDir = "new"
)

// p359CreateLoopDisk truncates a fresh sparse image of sizeMB, attaches
// it, formats it XFS and mounts it at mountpoint — createLoopDiskForPoolTest's
// own recipe (internal/pool/add_disk_lab_test.go), reproduced here because
// it is unexported there. Detaches only this device, backed by the image
// this call created (CLAUDE.md: never losetup -D).
func p359CreateLoopDisk(t *testing.T, r disk.Runner, lab, name string, sizeMB int, mountpoint string) {
	t.Helper()
	ctx := context.Background()

	imgDir := filepath.Join(lab, "img")
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", imgDir, err)
	}
	img := filepath.Join(imgDir, name+".img")
	if err := os.Remove(img); err != nil && !os.IsNotExist(err) {
		t.Fatalf("removing stale %s: %v", img, err)
	}
	if _, err := r.Run(ctx, "truncate", "-s", fmt.Sprintf("%dM", sizeMB), img); err != nil {
		t.Fatalf("truncate %s: %v", img, err)
	}
	out, err := r.Run(ctx, "losetup", "--find", "--show", img)
	if err != nil {
		t.Fatalf("losetup --find --show %s: %v", img, err)
	}
	dev := strings.TrimSpace(string(out))
	t.Cleanup(func() {
		_, _ = r.Run(context.Background(), "losetup", "-d", dev)
	})
	if !strings.HasPrefix(dev, "/dev/loop") {
		t.Fatalf("losetup %s: got %q, want a /dev/loopN device", img, dev)
	}

	if _, err := r.Run(ctx, "mkfs.xfs", "-q", dev); err != nil {
		t.Fatalf("mkfs.xfs %s: %v", dev, err)
	}
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", mountpoint, err)
	}
	if _, err := r.Run(ctx, "mount", dev, mountpoint); err != nil {
		t.Fatalf("mount %s %s: %v", dev, mountpoint, err)
	}
	t.Cleanup(func() {
		_, _ = r.Run(context.Background(), "umount", mountpoint)
	})
}

// p359LabEngine is shareRelocLabEngine plus two exclude lines: the
// writers in TestLabEvacuation_NoCreateKeepsNewWritesOffTheDisk create
// files while the job's syncs run, and snapraid refuses a file whose
// size changes under it mid-sync. Nothing the evacuation moves is under
// either excluded path.
func p359LabEngine(t *testing.T, lab, name string, mounts []string) *parity.SnapraidEngine {
	t.Helper()
	parityDir := filepath.Join(lab, "mnt/parity1")
	cacheDir := filepath.Join(lab, "mnt/cache")

	var conf strings.Builder
	fmt.Fprintf(&conf, "parity %s\n", filepath.Join(parityDir, name+".parity"))
	fmt.Fprintf(&conf, "content %s\n", filepath.Join(parityDir, name+".content"))
	fmt.Fprintf(&conf, "content %s\n", filepath.Join(cacheDir, name+".content"))
	for i, m := range mounts {
		fmt.Fprintf(&conf, "data d%d %s\n", i+1, m+"/")
	}
	fmt.Fprintf(&conf, "exclude /%s/\n", p359TopDir)
	fmt.Fprintf(&conf, "exclude /%s/%s/\n", p359Share, p359ShareWriteDir)

	workDir := filepath.Join(lab, name+"-work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", workDir, err)
	}
	confPath := filepath.Join(workDir, "snapraid.conf")
	if err := os.WriteFile(confPath, []byte(conf.String()), 0o644); err != nil {
		t.Fatalf("writing %s: %v", confPath, err)
	}
	return &parity.SnapraidEngine{
		ConfPath: confPath,
		LogDir:   filepath.Join(workDir, "logs"),
		Runner:   parity.CommandRunner{},
		Guard:    parity.Guard{Config: parity.GuardConfig{RemovedFilesMax: 100000, RemovedUpdatedPercent: 100}},
	}
}

// p359ClaimCatchAllPath makes pool.CatchAllPath free for this test's own
// catch-all. smb-check.sh, which `make test-integration` runs before the
// lab tests, leaves it as a symlink to the standing array's own pool; the
// symlink is moved aside for the test and put back at cleanup, which runs
// after the test's pool is unmounted. Anything else already mounted there
// fails the test.
func p359ClaimCatchAllPath(t *testing.T) {
	t.Helper()
	if target, err := os.Readlink(pool.CatchAllPath); err == nil {
		if err := os.Remove(pool.CatchAllPath); err != nil {
			t.Fatalf("moving the %s symlink aside: %v", pool.CatchAllPath, err)
		}
		t.Cleanup(func() {
			if err := os.Remove(pool.CatchAllPath); err != nil && !os.IsNotExist(err) {
				t.Errorf("removing %s before restoring its symlink: %v", pool.CatchAllPath, err)
				return
			}
			if err := os.Symlink(target, pool.CatchAllPath); err != nil {
				t.Errorf("restoring the %s symlink: %v", pool.CatchAllPath, err)
			}
		})
	}
	if pool.IsMounted(pool.CatchAllPath) {
		t.Fatalf("%s is already a mount point in this lab — a previous test left a pool mounted", pool.CatchAllPath)
	}
}

// p359Array is one test's three loop data disks, disk1 the largest.
type p359Array struct {
	lab   string
	disks []string
}

func p359NewArray(t *testing.T, name string) p359Array {
	t.Helper()
	lab := shareRelocLabDir(t)
	p359ClaimCatchAllPath(t)
	exec := disk.CommandRunner{}
	a := p359Array{lab: lab}
	for i, size := range []int{900, 300, 300} {
		label := fmt.Sprintf("%s-disk%d", name, i+1)
		mountpoint := filepath.Join(lab, label)
		p359CreateLoopDisk(t, exec, lab, label, size, mountpoint)
		a.disks = append(a.disks, mountpoint)
	}
	return a
}

// seed stores the array (create policy mfs) and one array-only share.
func (a p359Array) seed(t *testing.T, d *p359Daemon) {
	t.Helper()
	ctx := context.Background()
	var disks []store.ArrayDisk
	for i, m := range a.disks {
		label := filepath.Base(m)
		disks = append(disks, store.ArrayDisk{
			Role: store.ArrayRoleData, RoleIndex: i + 1, Mountpoint: m,
			Device: "/dev/" + label, Filesystem: "xfs", FSUUID: label, WWN: label, Serial: strings.ToUpper(label), ByIDName: label,
		})
	}
	now := time.Now().UTC()
	if err := d.arrays.PutArray(ctx, store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "1M", CreatedAt: now}, disks); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	if err := d.shares.Insert(ctx, store.Share{Name: p359Share, CacheMode: "array-only", CreatePolicy: "mfs", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("inserting share: %v", err)
	}
	if err := d.rebuildArraySequence(ctx); err != nil {
		t.Fatalf("building the array sequence: %v", err)
	}
}

// fill writes n files into disk1's own share branch (what the evacuation
// moves) and filler on disk3, so the plan runs in more than one batch.
func (a p359Array) fill(t *testing.T, n int) []string {
	t.Helper()
	var moved []string
	for i := 0; i < n; i++ {
		rel := fmt.Sprintf("existing-%02d.bin", i)
		shareRelocLabWriteFile(t, filepath.Join(a.disks[0], p359Share, rel), 200_000)
		moved = append(moved, rel)
	}
	for i := 0; i < 20; i++ {
		shareRelocLabWriteFile(t, filepath.Join(a.disks[2], "filler", fmt.Sprintf("f%02d.bin", i)), 1_000)
	}
	return moved
}

// p359Daemon is the part of a running hoservad #359 touches, built the
// way run() builds it (this file's header).
type p359Daemon struct {
	db                   *sql.DB
	arrays               *store.ArrayStore
	shares               *store.ShareStore
	jobs                 *job.Store
	scheduler            *job.Scheduler
	handler              *api.Handler
	shareService         *share.Service
	engine               *parity.SnapraidEngine
	rebuildArraySequence func(ctx context.Context) error
}

// p359StartDaemon opens stateDir's database the way run() does and wires
// the evacuation job, its abort and the topology hooks over it, in run()'s
// order. configRoot is where the generated unit files go.
func p359StartDaemon(t *testing.T, stateDir, configRoot string, engine *parity.SnapraidEngine) *p359Daemon {
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
	rebuildArraySequence := func(ctx context.Context) error {
		seq, err := newArraySequence(ctx, scheduler, arrays, shares, provider, exec)
		if err != nil {
			return err
		}
		handler.SetArray(seq)
		return nil
	}
	if err := rebuildArraySequence(ctx); err != nil {
		t.Fatalf("newArraySequence at startup: %v", err)
	}

	parityReg := &parityRegistrar{
		configRoot: configRoot,
		stateDir:   stateDir,
		db:         db,
		registry:   registry,
		handler:    handler,
		shareStore: shares,
		arrayStore: arrays,
		chainGuard: &diffGuardHolder{},
	}
	parityReg.register(engine)
	shareService := newShareService(shares, arrays, cfggen.NewGenerator(configRoot), pool.SystemdMounter{Runner: exec}, engine.Usage)
	shareService.PostCommit = rebuildArraySequence
	wireTopologyHooks(shareService, rebuildArraySequence, parityReg, handler)

	return &p359Daemon{
		db:                   db,
		arrays:               arrays,
		shares:               shares,
		jobs:                 jobStore,
		scheduler:            scheduler,
		handler:              handler,
		shareService:         shareService,
		engine:               engine,
		rebuildArraySequence: rebuildArraySequence,
	}
}

// poolMounts are the current ArraySequence's catch-all, share and
// mover-target mounts, exactly as newArraySequence built them.
func (d *p359Daemon) poolMounts(t *testing.T) []pool.Mount {
	t.Helper()
	seq := d.handler.CurrentArray()
	if seq == nil || seq.CatchAll == nil {
		t.Fatal("no array sequence with a catch-all")
	}
	var mounts []pool.Mount
	for _, m := range append([]job.ArrayMount{seq.CatchAll}, seq.ShareMounts...) {
		mc, ok := m.(pool.MountController)
		if !ok {
			t.Fatalf("pool mount %s is a %T, want pool.MountController", m.Where(), m)
		}
		mounts = append(mounts, mc.Mnt)
	}
	return mounts
}

// mountPool brings the pool up from poolMounts through pool.Mounter (this
// file's header) and unmounts it again at cleanup, shares first.
func (d *p359Daemon) mountPool(t *testing.T) {
	t.Helper()
	mounter := pool.Mounter{Runner: disk.CommandRunner{}}
	for _, m := range d.poolMounts(t) {
		if err := mounter.Mount(context.Background(), m); err != nil {
			t.Fatalf("mounting %s: %v", m.Where, err)
		}
		where := m.Where
		t.Cleanup(func() { _ = mounter.Unmount(context.Background(), where) })
	}
}

func (d *p359Daemon) plan(t *testing.T, mountpoint string) []byte {
	t.Helper()
	shares, err := rebalanceSharesFromStore(d.shares, d.arrays)(context.Background())
	if err != nil {
		t.Fatalf("rebalanceSharesFromStore: %v", err)
	}
	plan, err := cache.PlanEvacuation(context.Background(), mountpoint, shares, cache.Deps{})
	if err != nil {
		t.Fatalf("PlanEvacuation: %v", err)
	}
	params, err := json.Marshal(job.EvacuationParams{Mountpoint: mountpoint, Plan: plan})
	if err != nil {
		t.Fatalf("marshaling EvacuationParams: %v", err)
	}
	return params
}

func (d *p359Daemon) removal(t *testing.T, mountpoint string) (state, holder string) {
	t.Helper()
	_, disks, err := d.arrays.GetArray(context.Background())
	if err != nil {
		t.Fatalf("GetArray: %v", err)
	}
	for _, a := range disks {
		if a.Mountpoint == mountpoint {
			return a.RemovalState, a.RemovalJobID
		}
	}
	t.Fatalf("%s is not an array disk", mountpoint)
	return "", ""
}

// p359BranchMode returns the create mode (RW, NC, RO) the running
// mergerfs mount at where currently gives branch, read from its runtime
// control file.
func p359BranchMode(where, branch string) (string, error) {
	buf := make([]byte, 8192)
	n, err := syscall.Getxattr(filepath.Join(where, ".mergerfs"), "user.mergerfs.branches", buf)
	if err != nil {
		return "", fmt.Errorf("reading the live branches of %s: %w", where, err)
	}
	for _, entry := range strings.Split(string(buf[:n]), ":") {
		path, mode, _ := strings.Cut(entry, "=")
		if path == branch {
			mode, _, _ = strings.Cut(mode, ",")
			return mode, nil
		}
	}
	return "", fmt.Errorf("%s has no branch %s in %q", where, branch, buf[:n])
}

// p359RequireMode fails unless every pool mount's own branch on disk1 has
// mode.
func p359RequireMode(t *testing.T, disk1, mode string) {
	t.Helper()
	for _, c := range []struct{ where, branch string }{
		{pool.CatchAllPath, disk1},
		{pool.SharePath(p359Share), filepath.Join(disk1, p359Share)},
		{pool.MoverTargetPath(p359Share), filepath.Join(disk1, p359Share)},
	} {
		got, err := p359BranchMode(c.where, c.branch)
		if err != nil {
			t.Fatal(err)
		}
		if got != mode {
			t.Fatalf("live mount %s gives disk1's branch %s mode %s, want %s", c.where, c.branch, got, mode)
		}
	}
}

// p359Target is one place new files are written through the pool.
type p359Target struct {
	name    string
	poolDir string // written through the pool
	ctl     string // the mount whose live branches place poolDir's files
	rel     string // poolDir's path relative to each data disk
}

func p359Targets() []p359Target {
	return []p359Target{
		{name: "catch-all", poolDir: filepath.Join(pool.CatchAllPath, p359TopDir), ctl: pool.CatchAllPath, rel: p359TopDir},
		{name: "share", poolDir: filepath.Join(pool.SharePath(p359Share), p359ShareWriteDir), ctl: pool.SharePath(p359Share), rel: filepath.Join(p359Share, p359ShareWriteDir)},
	}
}

// landedOn reports which data disk holds tgt's file name.
func (a p359Array) landedOn(t *testing.T, tgt p359Target, name string) string {
	t.Helper()
	for _, d := range a.disks {
		if _, err := os.Stat(filepath.Join(d, tgt.rel, name)); err == nil {
			return d
		}
	}
	t.Fatalf("%s file %s is on no data disk", tgt.name, name)
	return ""
}

// writeThrough writes n files through each target and returns, per
// target, the disks they landed on.
func (a p359Array) writeThrough(t *testing.T, prefix string, n int) map[string][]string {
	t.Helper()
	landed := map[string][]string{}
	for _, tgt := range p359Targets() {
		if err := os.MkdirAll(tgt.poolDir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", tgt.poolDir, err)
		}
		for i := 0; i < n; i++ {
			name := fmt.Sprintf("%s-%02d.bin", prefix, i)
			if err := os.WriteFile(filepath.Join(tgt.poolDir, name), []byte("written through the pool"), 0o644); err != nil {
				t.Fatalf("writing through the %s: %v", tgt.name, err)
			}
			landed[tgt.name] = append(landed[tgt.name], a.landedOn(t, tgt, name))
		}
	}
	return landed
}

// removeThrough deletes what writeThrough wrote, through the pool.
func (a p359Array) removeThrough(t *testing.T, prefix string, n int) {
	t.Helper()
	for _, tgt := range p359Targets() {
		for i := 0; i < n; i++ {
			if err := os.Remove(filepath.Join(tgt.poolDir, fmt.Sprintf("%s-%02d.bin", prefix, i))); err != nil {
				t.Fatalf("removing through the %s: %v", tgt.name, err)
			}
		}
	}
}

// p359Writer keeps writing new files through one target while an
// evacuation runs. It writes only once the target's own live mount gives
// disk1's branch NC, and never before: a write that starts after NC is
// read back from the running mount cannot be placed on disk1 by that
// mount, so every file it records must land elsewhere.
type p359Writer struct {
	tgt     p359Target
	branch  string // disk1's branch in tgt.ctl
	mu      sync.Mutex
	written []string
	err     error
}

func (w *p359Writer) run(stop <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	for i := 0; ; i++ {
		select {
		case <-stop:
			return
		default:
		}
		mode, err := p359BranchMode(w.tgt.ctl, w.branch)
		if err != nil {
			w.fail(err)
			return
		}
		if mode != "NC" {
			time.Sleep(2 * time.Millisecond)
			continue
		}
		name := fmt.Sprintf("during-%04d.bin", i)
		if err := os.WriteFile(filepath.Join(w.tgt.poolDir, name), []byte("new write during the evacuation"), 0o644); err != nil {
			w.fail(fmt.Errorf("writing through the %s: %w", w.tgt.name, err))
			return
		}
		w.mu.Lock()
		w.written = append(w.written, name)
		w.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
}

func (w *p359Writer) fail(err error) {
	w.mu.Lock()
	w.err = err
	w.mu.Unlock()
}

// TestLabEvacuation_NoCreateKeepsNewWritesOffTheDisk is #359's data-loss
// / refill scenario. The pool is mounted and writable, and new files
// written through the catch-all and the share land on disk1, the disk
// with the most free space under `mfs`. An evacuation of disk1 then runs
// while two writers keep creating files through both: from the moment the
// job has switched the running mounts to no-create, every new file lands
// on disk2 or disk3, the evacuation still passes its post-check and
// succeeds, and disk1 ends "evacuated" and still no-create.
func TestLabEvacuation_NoCreateKeepsNewWritesOffTheDisk(t *testing.T) {
	ctx := context.Background()
	a := p359NewArray(t, "p359nc")
	d := p359StartDaemon(t, t.TempDir(), t.TempDir(), p359LabEngine(t, a.lab, "p359nc", a.disks))
	a.seed(t, d)
	moved := a.fill(t, 5)
	d.mountPool(t)
	disk1 := a.disks[0]

	// Before the evacuation, the running pool places new files on disk1.
	for tgt, disks := range a.writeThrough(t, "before", 3) {
		for _, got := range disks {
			if got != disk1 {
				t.Fatalf("before the evacuation a file written through the %s landed on %s, want %s — the scenario needs disk1 to be where mfs places new files", tgt, got, disk1)
			}
		}
	}
	a.removeThrough(t, "before", 3)
	p359RequireMode(t, disk1, "RW")
	shareRelocLabSyncOnce(t, ctx, d.engine)

	params := d.plan(t, disk1)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	var writers []*p359Writer
	for _, tgt := range p359Targets() {
		branch := disk1
		if tgt.ctl != pool.CatchAllPath {
			branch = filepath.Join(disk1, p359Share)
		}
		w := &p359Writer{tgt: tgt, branch: branch}
		writers = append(writers, w)
		wg.Add(1)
		go w.run(stop, &wg)
	}

	j, err := d.scheduler.Submit(ctx, job.TypeEvacuation, nil, params)
	if err != nil {
		close(stop)
		wg.Wait()
		t.Fatalf("Submit(TypeEvacuation): %v", err)
	}
	finished, err := d.scheduler.Await(ctx, j.ID)
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if finished.Status != job.StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}

	for _, w := range writers {
		if w.err != nil {
			t.Fatalf("%s writer: %v", w.tgt.name, w.err)
		}
		if len(w.written) == 0 {
			t.Fatalf("the %s writer wrote nothing during the evacuation — the running mount never gave disk1's branch NC while the job ran", w.tgt.name)
		}
		for _, name := range w.written {
			if got := a.landedOn(t, w.tgt, name); got == disk1 {
				t.Fatalf("%s written through the %s during the evacuation landed on disk1", name, w.tgt.name)
			}
		}
		t.Logf("%s writer: %d files written during the evacuation, none on disk1", w.tgt.name, len(w.written))
	}

	for _, rel := range moved {
		if _, err := os.Stat(filepath.Join(disk1, p359Share, rel)); !os.IsNotExist(err) {
			t.Fatalf("evacuated file %s is still on disk1: %v", rel, err)
		}
	}
	if state, holder := d.removal(t, disk1); state != store.RemovalStateEvacuated || holder != j.ID {
		t.Fatalf("removal state = (%q, %q), want (%q, %q)", state, holder, store.RemovalStateEvacuated, j.ID)
	}

	// Still no-create once the job has ended.
	p359RequireMode(t, disk1, "NC")
	for tgt, disks := range a.writeThrough(t, "after", 10) {
		for _, got := range disks {
			if got == disk1 {
				t.Fatalf("after the evacuation a file written through the %s landed on disk1", tgt)
			}
		}
	}
}

// TestLabEvacuation_FailedLiveNoCreate_FailsBeforeAnyCopy is #359's
// "if the live NC update fails, the evacuation job fails before copying
// anything", through the hook wireTopologyHooks binds for evacuations.
// The catch-all is the pool's own, but something else is mounted at the
// share's mount point, so applying the share mount to the running pool
// fails. The job must fail with that error, having copied nothing.
func TestLabEvacuation_FailedLiveNoCreate_FailsBeforeAnyCopy(t *testing.T) {
	ctx := context.Background()
	exec := disk.CommandRunner{}
	a := p359NewArray(t, "p359fail")
	d := p359StartDaemon(t, t.TempDir(), t.TempDir(), p359LabEngine(t, a.lab, "p359fail", a.disks))
	a.seed(t, d)
	moved := a.fill(t, 3)
	disk1 := a.disks[0]

	mounter := pool.Mounter{Runner: exec}
	catchAll := d.poolMounts(t)[0]
	if err := mounter.Mount(ctx, catchAll); err != nil {
		t.Fatalf("mounting the catch-all: %v", err)
	}
	t.Cleanup(func() { _ = mounter.Unmount(context.Background(), catchAll.Where) })
	foreign := pool.SharePath(p359Share)
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", foreign, err)
	}
	if _, err := exec.Run(ctx, "mount", "-t", "tmpfs", "-o", "size=1m", "p359-foreign", foreign); err != nil {
		t.Fatalf("mounting a foreign tmpfs at %s: %v", foreign, err)
	}
	t.Cleanup(func() { _, _ = exec.Run(context.Background(), "umount", foreign) })
	shareRelocLabSyncOnce(t, ctx, d.engine)

	j, err := d.scheduler.Submit(ctx, job.TypeEvacuation, nil, d.plan(t, disk1))
	if err != nil {
		t.Fatalf("Submit(TypeEvacuation): %v", err)
	}
	finished, err := d.scheduler.Await(ctx, j.ID)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if finished.Status != job.StatusFailed {
		t.Fatalf("status = %s (%s), want failed — the running pool never took no-create for disk1", finished.Status, finished.ErrorMessage)
	}
	if !strings.Contains(finished.ErrorMessage, "applying no-create to the live pool") || !strings.Contains(finished.ErrorMessage, "already mounted by something other than") {
		t.Fatalf("ErrorMessage = %q, want the failed live update of %s", finished.ErrorMessage, foreign)
	}
	for _, rel := range moved {
		if _, err := os.Stat(filepath.Join(disk1, p359Share, rel)); err != nil {
			t.Fatalf("source %s must be untouched: %v", rel, err)
		}
		for _, other := range a.disks[1:] {
			if _, err := os.Stat(filepath.Join(other, p359Share, rel)); !os.IsNotExist(err) {
				t.Fatalf("%s was copied to %s (err=%v) — nothing may be copied before no-create is live", rel, other, err)
			}
		}
	}
	if manifest, removing, err := d.engine.Relocation.Current(ctx); err != nil || manifest != nil || removing != nil {
		t.Fatalf("relocation manifest = (%v, %v, %v), want nothing persisted", manifest, removing, err)
	}
	if state, holder := d.removal(t, disk1); state != store.RemovalStateEvacuating || holder != j.ID {
		t.Fatalf("removal state after a plain failure = (%q, %q), want (%q, %q)", state, holder, store.RemovalStateEvacuating, j.ID)
	}
}

// TestLabEvacuation_RestartMidEvacuation_UnitsKeepNoCreate_ResumeReappliesIt
// is #359's restart criterion. A daemon starts an evacuation of disk1 and
// is stopped mid-run (maintenance mode interrupts it); its database is
// closed. A second daemon opens the same database file: its freshly built
// ArraySequence and the unit files it generates both give disk1 NC in
// every pool mount. The running pool is then made to give disk1 RW again
// — standing in for a pool that lost the live setting — and resuming the
// job through the second daemon's own registration puts NC back before
// the job finishes: disk1 ends "evacuated", still NC, and new writes land
// elsewhere.
func TestLabEvacuation_RestartMidEvacuation_UnitsKeepNoCreate_ResumeReappliesIt(t *testing.T) {
	ctx := context.Background()
	a := p359NewArray(t, "p359rs")
	stateDir := t.TempDir()
	first := p359StartDaemon(t, stateDir, t.TempDir(), p359LabEngine(t, a.lab, "p359rs", a.disks))
	a.seed(t, first)
	moved := a.fill(t, 6)
	first.mountPool(t)
	disk1 := a.disks[0]
	shareRelocLabSyncOnce(t, ctx, first.engine)

	j, err := first.scheduler.Submit(ctx, job.TypeEvacuation, nil, first.plan(t, disk1))
	if err != nil {
		t.Fatalf("Submit(TypeEvacuation): %v", err)
	}
	if err := first.scheduler.EnterMaintenance(ctx); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}
	stopped, err := first.scheduler.Await(ctx, j.ID)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if stopped.Status != job.StatusInterrupted {
		t.Fatalf("status = %s (%s), want interrupted mid-evacuation", stopped.Status, stopped.ErrorMessage)
	}
	if _, err := os.Stat(filepath.Join(disk1, p359Share, moved[len(moved)-1])); err != nil {
		t.Fatalf("the interrupted job already moved every file (%v) — it was not stopped mid-evacuation", err)
	}
	p359RequireMode(t, disk1, "NC")
	if err := first.db.Close(); err != nil {
		t.Fatalf("closing the first daemon's database: %v", err)
	}

	configRoot := t.TempDir()
	second := p359StartDaemon(t, stateDir, configRoot, p359LabEngine(t, a.lab, "p359rs", a.disks))
	if state, holder := second.removal(t, disk1); state != store.RemovalStateEvacuating || holder != j.ID {
		t.Fatalf("removal state after the restart = (%q, %q), want (%q, %q)", state, holder, store.RemovalStateEvacuating, j.ID)
	}

	// The restarted daemon's own mounts and generated units give disk1 NC.
	for _, m := range second.poolMounts(t) {
		if !strings.Contains(":"+m.What+":", ":"+disk1+"=NC:") && !strings.Contains(":"+m.What+":", ":"+filepath.Join(disk1, p359Share)+"=NC:") {
			t.Fatalf("restarted daemon's %s mount has branches %q, want disk1 NC", m.Where, m.What)
		}
	}
	if err := second.shareService.ApplyTopology(ctx, false); err != nil {
		t.Fatalf("ApplyTopology: %v", err)
	}
	for _, where := range []string{pool.CatchAllPath, pool.SharePath(p359Share), pool.MoverTargetPath(p359Share)} {
		unit := filepath.Join(configRoot, "systemd/system", pool.UnitFileName(where))
		body, err := os.ReadFile(unit)
		if err != nil {
			t.Fatalf("reading the regenerated unit for %s: %v", where, err)
		}
		if !strings.Contains(string(body), disk1+"=NC") && !strings.Contains(string(body), filepath.Join(disk1, p359Share)+"=NC") {
			t.Fatalf("regenerated unit %s does not give disk1 NC:\n%s", unit, body)
		}
	}

	// The running pool loses NC; the resume must put it back.
	for _, m := range second.poolMounts(t) {
		ctl := filepath.Join(m.Where, ".mergerfs")
		if err := syscall.Setxattr(ctl, "user.mergerfs.branches", []byte(strings.ReplaceAll(m.What, "=NC", "=RW")), 0); err != nil {
			t.Fatalf("setting %s back to RW: %v", m.Where, err)
		}
	}
	p359RequireMode(t, disk1, "RW")

	if _, err := second.scheduler.Resume(ctx, j.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	finished, err := second.scheduler.Await(ctx, j.ID)
	if err != nil {
		t.Fatalf("Await(resumed): %v", err)
	}
	if finished.Status != job.StatusSucceeded {
		t.Fatalf("resumed status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	p359RequireMode(t, disk1, "NC")
	for _, rel := range moved {
		if _, err := os.Stat(filepath.Join(disk1, p359Share, rel)); !os.IsNotExist(err) {
			t.Fatalf("evacuated file %s is still on disk1: %v", rel, err)
		}
	}
	if state, holder := second.removal(t, disk1); state != store.RemovalStateEvacuated || holder != j.ID {
		t.Fatalf("removal state after the resumed job = (%q, %q), want (%q, %q)", state, holder, store.RemovalStateEvacuated, j.ID)
	}
	for tgt, disks := range a.writeThrough(t, "after", 10) {
		for _, got := range disks {
			if got == disk1 {
				t.Fatalf("after the resumed evacuation a file written through the %s landed on disk1", tgt)
			}
		}
	}
}
