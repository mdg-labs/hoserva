//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never
// on the host: it is built with `go test -tags lab -c` on the host
// (compiling touches no device) and the binary is run inside the lab
// container, the same pattern disk_lifecycle_lab_test.go uses. The data-
// disk tests drive doc 02 §4's upgrade state machine end to end against
// real loop devices, real mount/umount and a real snapraid binary; test
// names carry the cells they cover. disk.DirectMountController stands in
// for systemd mount units, which this init-system-less container lacks.

package job

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/store"
)

// labAwait is await with a timeout sized for real I/O on loop devices.
func labAwait(t *testing.T, s *Scheduler, id string) *Job {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	j, err := s.Await(ctx, id)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	return j
}

const (
	labUpgradeFileCount = 300
	labUpgradeFileSize  = 256 << 10
	labUpgradeSlot      = "/mnt/disk1"
)

// labUpgradeEnv is a real three-disk array (parity1, disk1, disk2) built
// through TypeDiskFormat, filled with data and synced, plus a larger
// fourth loop device B for disk1.
type labUpgradeEnv struct {
	t        *testing.T
	ctx      context.Context
	lab      string
	runner   disk.CommandRunner
	provider *disk.LinuxProvider
	s        *Scheduler
	st       *store.ArrayStore
	genRoot  string
	engine   *parity.SnapraidEngine
	oldDev   string
	oldUUID  string
	newDev   string
	sizes    map[string]int64
	hashes   map[string]string
	disk2    string
	disk2Sum string

	seqMu sync.Mutex
	seq   *ArraySequence
}

func newLabUpgradeEnv(t *testing.T, prefix string) *labUpgradeEnv {
	t.Helper()
	e := &labUpgradeEnv{
		t:      t,
		ctx:    context.Background(),
		lab:    labDir(t),
		runner: disk.CommandRunner{},
	}
	e.provider = &disk.LinuxProvider{Lister: disk.NewLister(), Exec: e.runner}
	resetBootContentDir(t)
	_ = os.RemoveAll("/mnt/.hoserva-disk-upgrade-staging")

	parityDev := createLoopImage(e.ctx, t, e.runner, e.lab, prefix+"-parity", "480M")
	e.oldDev = createLoopImage(e.ctx, t, e.runner, e.lab, prefix+"-data1", "320M")
	data2Dev := createLoopImage(e.ctx, t, e.runner, e.lab, prefix+"-data2", "320M")
	e.newDev = createLoopImage(e.ctx, t, e.runner, e.lab, prefix+"-new", "400M")
	e.sizes = map[string]int64{parityDev: 480 << 20, e.oldDev: 320 << 20, data2Dev: 320 << 20, e.newDev: 400 << 20}
	t.Cleanup(func() {
		for _, p := range []string{"/mnt/.hoserva-disk-upgrade-staging/mnt/disk1", "/mnt/disk1", "/mnt/disk2", "/mnt/parity1"} {
			for i := 0; i < 3; i++ {
				unmountIfMounted(e.runner, p)
			}
		}
	})

	db := newLabProductionDB(t)
	e.s = NewScheduler(NewStore(db), NewLogStore(t.TempDir()), NewHub(), NewRegistry())
	e.st = store.NewArrayStore(db)
	e.genRoot = t.TempDir()
	labRegisterDiskFormatDeps(t, e.s, e.provider, e.runner, e.st, e.genRoot, disk.DirectMounter{Runner: e.runner})
	formatParams := DiskFormatParams{
		Parity: []disk.AssignedDisk{{Device: parityDev, Filesystem: disk.XFS}},
		Data:   []disk.AssignedDisk{{Device: e.oldDev, Filesystem: disk.XFS}, {Device: data2Dev, Filesystem: disk.XFS}},
		Sizes:  map[string]int64{parityDev: 480 << 20, e.oldDev: 320 << 20, data2Dev: 320 << 20},
	}
	formatParams.Confirmation = formatParams.Plan().Confirmation()
	j, err := e.s.Submit(e.ctx, TypeDiskFormat, nil, mustJSON(t, formatParams))
	if err != nil {
		t.Fatalf("Submit(disk_format): %v", err)
	}
	if done := labAwait(t, e.s, j.ID); done.Status != StatusSucceeded {
		t.Fatalf("disk_format = %s (%s)", done.Status, done.ErrorMessage)
	}
	e.oldUUID = blkidUUID(e.ctx, e.runner, e.oldDev)

	e.engine = &parity.SnapraidEngine{
		ConfPath: filepath.Join(e.genRoot, "snapraid.conf"),
		LogDir:   filepath.Join(e.genRoot, "snapraid-logs"),
		Runner:   parity.CommandRunner{},
		Guard:    parity.Guard{Config: parity.GuardConfig{RemovedFilesMax: 100000, RemovedUpdatedPercent: 100}},
	}
	e.hashes = map[string]string{}
	for i := 0; i < labUpgradeFileCount; i++ {
		rel := filepath.Join("movies", fmt.Sprintf("f%03d.bin", i))
		data := writeLabFile(t, filepath.Join(labUpgradeSlot, rel), labUpgradeFileSize)
		// Each file differs, so a copy that mixes them up is caught.
		data[0] = byte(i)
		data[1] = byte(i >> 8)
		if err := os.WriteFile(filepath.Join(labUpgradeSlot, rel), data, 0o644); err != nil {
			t.Fatal(err)
		}
		e.hashes[rel] = sha256HexOfFile(t, filepath.Join(labUpgradeSlot, rel))
	}
	e.disk2 = filepath.Join("/mnt/disk2", "kept.bin")
	writeLabFile(t, e.disk2, 500_000)
	e.disk2Sum = sha256HexOfFile(t, e.disk2)
	e.sync()

	if err := e.rebuildSeq(e.ctx); err != nil {
		t.Fatalf("building the array sequence: %v", err)
	}
	deps := e.deps()
	e.s.registry.Register(TypeDiskUpgradeData, true, RunDiskUpgradeData(deps))
	e.s.registry.RegisterAbort(TypeDiskUpgradeData, AbortDiskUpgradeData(deps))
	return e
}

// newLabProductionDB is newTestDB opened with hoservad's own DSN (WAL
// mode and a busy timeout, store.DSN), since these tests read job state
// while a real upgrade writes its progress.
func newLabProductionDB(t *testing.T) *sql.DB {
	t.Helper()
	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	db, err := sql.Open("sqlite", store.DSN(filepath.Join(t.TempDir(), "hoserva.db")))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	return db
}

func (e *labUpgradeEnv) sync() {
	e.t.Helper()
	ch, err := e.engine.Sync(e.ctx, parity.SyncOpts{})
	if err != nil {
		e.t.Fatalf("Sync: %v", err)
	}
	if final := drainRealProgress(e.t, ch); final.Err != nil {
		e.t.Fatalf("Sync failed: %v", final.Err)
	}
}

// rebuildSeq is the lab's ArrayReady: the array sequence rebuilt from
// SQLite, with UR9's check over the real kernel mount table.
func (e *labUpgradeEnv) rebuildSeq(ctx context.Context) error {
	_, arrayDisks, err := e.st.GetArray(ctx)
	if err != nil {
		return err
	}
	units, err := mountUnitsFromStore(arrayDisks)
	if err != nil {
		return err
	}
	var mounts []ArrayMount
	for _, u := range units {
		mounts = append(mounts, disk.DirectMountController{Unit: u, Runner: e.runner})
	}
	seq := &ArraySequence{
		Scheduler: e.s,
		Disks:     mounts,
		DiskCheck: ArrayDiskUUIDCheck{Mounts: disk.KernelMounts{Runner: e.runner}, Disks: units},
	}
	e.seqMu.Lock()
	e.seq = seq
	e.seqMu.Unlock()
	return nil
}

func (e *labUpgradeEnv) currentSeq() *ArraySequence {
	e.seqMu.Lock()
	defer e.seqMu.Unlock()
	return e.seq
}

func (e *labUpgradeEnv) deps() DiskUpgradeDataDeps {
	return DiskUpgradeDataDeps{
		Provider:   e.provider,
		Runner:     e.runner,
		Store:      e.st,
		Generator:  config.NewGenerator(e.genRoot),
		Parity:     e.engine,
		Mounts:     disk.KernelMounts{Runner: e.runner},
		Array:      e.currentSeq,
		ArrayReady: e.rebuildSeq,
	}
}

func (e *labUpgradeEnv) params() []byte {
	replacement := disk.AssignedDisk{Device: e.newDev, Filesystem: disk.XFS}
	return mustJSON(e.t, DiskUpgradeDataParams{
		Confirmation: SingleDiskConfirmation(replacement),
		Mountpoint:   labUpgradeSlot,
		Old:          disk.AssignedDisk{Device: e.oldDev, Filesystem: disk.XFS, FSUUID: e.oldUUID},
		Disk:         replacement,
		Sizes:        e.sizes,
	})
}

// assertNothingMounted is the Stopped set, read from the kernel mount
// table.
func (e *labUpgradeEnv) assertNothingMounted(when string) {
	e.t.Helper()
	k := disk.KernelMounts{Runner: e.runner}
	for _, p := range []string{"/mnt/parity1", labUpgradeSlot, "/mnt/disk2", "/mnt/.hoserva-disk-upgrade-staging/mnt/disk1"} {
		mounted, err := k.IsMounted(e.ctx, p)
		if err != nil {
			e.t.Fatalf("reading the mount table %s: %v", when, err)
		}
		if mounted {
			e.t.Fatalf("%s is mounted %s, want the Stopped set", p, when)
		}
	}
}

// assertOldDiskUnchanged mounts A read-only at a scratch path and
// compares every file with its hash from before the upgrade (invariant 2).
func (e *labUpgradeEnv) assertOldDiskUnchanged(when string) {
	e.t.Helper()
	scratch := filepath.Join(e.lab, "old-disk-check")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.runner.Run(e.ctx, "mount", "-o", "ro", "-U", e.oldUUID, scratch); err != nil {
		e.t.Fatalf("mounting A read-only %s: %v", when, err)
	}
	defer unmountIfMounted(e.runner, scratch)
	e.assertTree(scratch, "A "+when)
}

func (e *labUpgradeEnv) assertTree(root, what string) {
	e.t.Helper()
	for rel, want := range e.hashes {
		if got := sha256HexOfFile(e.t, filepath.Join(root, rel)); got != want {
			e.t.Fatalf("%s: %s = %s, want %s", what, rel, got, want)
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, "movies"))
	if err != nil {
		e.t.Fatal(err)
	}
	if len(entries) != len(e.hashes) {
		e.t.Fatalf("%s: %d files under movies, want %d", what, len(entries), len(e.hashes))
	}
}

func (e *labUpgradeEnv) checkpoint(id string) disk.DataDiskUpgradeCheckpoint {
	e.t.Helper()
	j, err := e.s.store.Get(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	var cp disk.DataDiskUpgradeCheckpoint
	if len(j.Checkpoint) > 0 {
		if err := json.Unmarshal(j.Checkpoint, &cp); err != nil {
			e.t.Fatal(err)
		}
	}
	return cp
}

// waitForCopying waits until the job has saved its copying checkpoint:
// Formatting is done and the copy has begun.
func (e *labUpgradeEnv) waitForCopying(id string) {
	e.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if e.checkpoint(id).Phase == disk.DataDiskUpgradePhaseCopying {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatal("the upgrade never reached its copying checkpoint")
}

// TestLabDiskUpgradeData_E1_E4_E5_InterruptedMidCopyResumedAfterRestartLosesNothing
// is the issue's acceptance lab test. After a real `array stop` (UR3) the
// upgrade is submitted and interrupted mid-copy by the shutdown path's
// stop sequence (E4); every mount is unwound and A is unchanged. A new
// scheduler over the same database stands in for the restarted daemon:
// RecoverFromRestart and startup recovery hold the array stopped (UR1,
// E6), and Resume through the job system (E5 Interrupted at copying)
// finishes the upgrade. B then serves the slot after `array start` with
// every file intact, UR9 confirming it, and A is still unchanged.
func TestLabDiskUpgradeData_E1_E4_E5_InterruptedMidCopyResumedAfterRestartLosesNothing(t *testing.T) {
	e := newLabUpgradeEnv(t, "u289a")
	if err := e.currentSeq().Stop(e.ctx); err != nil {
		t.Fatalf("array stop: %v", err)
	}
	e.assertNothingMounted("after array stop")

	j, err := e.s.Submit(e.ctx, TypeDiskUpgradeData, nil, e.params())
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	e.waitForCopying(j.ID)
	if err := e.currentSeq().Stop(e.ctx); err != nil {
		t.Fatalf("the shutdown stop sequence: %v", err)
	}
	interrupted := labAwait(t, e.s, j.ID)
	cp := e.checkpoint(j.ID)
	t.Logf("after the shutdown stop: status=%s checkpoint=%+v", interrupted.Status, cp)
	if interrupted.Status != StatusInterrupted || cp.Phase != disk.DataDiskUpgradePhaseCopying {
		t.Fatalf("status %s at %s, want interrupted mid-copy", interrupted.Status, cp.Phase)
	}
	if cp.NewUUID == "" || cp.NewUUID != blkidUUID(e.ctx, e.runner, e.newDev) {
		t.Fatalf("checkpoint UUID %q, want B's %q (UR4)", cp.NewUUID, blkidUUID(e.ctx, e.runner, e.newDev))
	}
	e.assertNothingMounted("after the interruption")
	e.assertOldDiskUnchanged("after the interruption")

	// The restarted daemon: a new scheduler over the same database.
	restarted := NewScheduler(e.s.store, NewLogStore(t.TempDir()), NewHub(), NewRegistry())
	e.s = restarted
	if err := e.rebuildSeq(e.ctx); err != nil {
		t.Fatal(err)
	}
	deps := e.deps()
	restarted.registry.Register(TypeDiskUpgradeData, true, RunDiskUpgradeData(deps))
	restarted.registry.RegisterAbort(TypeDiskUpgradeData, AbortDiskUpgradeData(deps))
	if err := restarted.RecoverFromRestart(e.ctx); err != nil {
		t.Fatal(err)
	}
	if err := RecoverDiskUpgradeData(e.ctx, restarted, deps); err != nil {
		t.Fatalf("startup recovery: %v", err)
	}
	if !restarted.InMaintenance() {
		t.Fatal("the restarted daemon is not in maintenance mode with an upgrade pending (UR1)")
	}
	if err := e.currentSeq().Start(e.ctx); !errors.Is(err, ErrDiskUpgradeDataPending) {
		t.Fatalf("array start while interrupted = %v, want disk_upgrade_pending (E6)", err)
	}
	e.assertNothingMounted("after the refused array start")

	if _, err := restarted.Resume(e.ctx, j.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	done := labAwait(t, restarted, j.ID)
	if done.Status != StatusSucceeded {
		t.Fatalf("resumed upgrade = %s (%s: %s), want succeeded", done.Status, done.ErrorCode, done.ErrorMessage)
	}
	e.assertNothingMounted("after Done")
	switched, err := e.st.GetDataDiskByMountpoint(e.ctx, labUpgradeSlot)
	if err != nil {
		t.Fatal(err)
	}
	newUUID := blkidUUID(e.ctx, e.runner, e.newDev)
	if switched.Device != e.newDev || switched.FSUUID != newUUID {
		t.Fatalf("SQLite names %s/%s, want B %s/%s", switched.Device, switched.FSUUID, e.newDev, newUUID)
	}
	unit, err := os.ReadFile(filepath.Join(e.genRoot, "systemd", "system", disk.UnitFileName(labUpgradeSlot)))
	if err != nil || !strings.Contains(string(unit), "/dev/disk/by-uuid/"+newUUID) {
		t.Fatalf("S's generated unit does not name B (%v):\n%s", err, unit)
	}
	e.assertOldDiskUnchanged("after Done")

	if err := e.currentSeq().Start(e.ctx); err != nil {
		t.Fatalf("array start after Done: %v", err)
	}
	if got := findmntUUID(e.ctx, e.runner, labUpgradeSlot); got != newUUID {
		t.Fatalf("%s holds %q after array start, want B %q", labUpgradeSlot, got, newUUID)
	}
	e.assertTree(labUpgradeSlot, "B at S")
	if got := sha256HexOfFile(t, e.disk2); got != e.disk2Sum {
		t.Fatal("disk2's file changed across the upgrade")
	}
	writeLabFile(t, filepath.Join(labUpgradeSlot, "after-upgrade.bin"), 100_000)
	e.sync()
	diff, err := e.engine.Diff(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if diff.Added+diff.Removed+diff.Updated != 0 {
		t.Fatalf("diff after the post-upgrade sync = %+v, want clean", diff)
	}
}

// TestLabDiskUpgradeData_E3_CancelMidCopyKeepsTheOldDiskServingEverything:
// a running cancel mid-copy unwinds and ends cancelled (E3 None to
// Diffing, running); SQLite names A, and `array start` serves A with every
// file (After cancelled).
func TestLabDiskUpgradeData_E3_CancelMidCopyKeepsTheOldDiskServingEverything(t *testing.T) {
	e := newLabUpgradeEnv(t, "u289b")
	if err := e.currentSeq().Stop(e.ctx); err != nil {
		t.Fatalf("array stop: %v", err)
	}
	j, err := e.s.Submit(e.ctx, TypeDiskUpgradeData, nil, e.params())
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	e.waitForCopying(j.ID)
	if _, err := e.s.Cancel(e.ctx, j.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	done := labAwait(t, e.s, j.ID)
	if done.Status != StatusCancelled {
		t.Fatalf("status = %s (%s), want cancelled", done.Status, done.ErrorMessage)
	}
	e.assertNothingMounted("after the cancel")
	slot, err := e.st.GetDataDiskByMountpoint(e.ctx, labUpgradeSlot)
	if err != nil || slot.FSUUID != e.oldUUID {
		t.Fatalf("SQLite names %+v (%v), want A", slot, err)
	}
	if err := e.currentSeq().Start(e.ctx); err != nil {
		t.Fatalf("array start after the cancel: %v", err)
	}
	if got := findmntUUID(e.ctx, e.runner, labUpgradeSlot); got != e.oldUUID {
		t.Fatalf("%s holds %q, want A %q", labUpgradeSlot, got, e.oldUUID)
	}
	e.assertTree(labUpgradeSlot, "A at S after the cancel")
}

// TestLabDiskUpgradeData_UR5_MissingStagingRecoveryAndAbortWithRealUmount
// uses the real tools: G was never
// created (the run stopped before Formatting), and a real `umount` of it
// exits 32. Startup recovery succeeds and records nothing, and the abort
// ends the job cancelled (E4 None or Formatting, E3 Interrupted at none).
func TestLabDiskUpgradeData_UR5_MissingStagingRecoveryAndAbortWithRealUmount(t *testing.T) {
	e := newLabUpgradeEnv(t, "u289c")
	if err := e.currentSeq().Stop(e.ctx); err != nil {
		t.Fatalf("array stop: %v", err)
	}
	staging := "/mnt/.hoserva-disk-upgrade-staging/mnt/disk1"
	if _, err := os.Stat(staging); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging path %s exists before the test (%v)", staging, err)
	}
	if out, err := e.runner.Run(e.ctx, "umount", staging); err == nil || !strings.Contains(err.Error(), "exit status 32") {
		t.Fatalf("real umount of the missing staging path = %q, %v; want exit status 32", out, err)
	}

	now := time.Now().UTC()
	j := &Job{ID: uuid.NewString(), Type: TypeDiskUpgradeData, Class: ClassTopology, Status: StatusRunning, Resumable: true, Cancellable: true, Params: e.params(), CreatedAt: now, StartedAt: &now}
	if err := e.s.store.Create(e.ctx, j); err != nil {
		t.Fatal(err)
	}
	if err := e.s.RecoverFromRestart(e.ctx); err != nil {
		t.Fatal(err)
	}
	if err := RecoverDiskUpgradeData(e.ctx, e.s, e.deps()); err != nil {
		t.Fatalf("startup recovery: %v", err)
	}
	got, err := e.s.store.Get(e.ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusInterrupted || got.ErrorCode != "" {
		t.Fatalf("after recovery = %s/%s %q, want interrupted with no cleanup failure", got.Status, got.ErrorCode, got.ErrorMessage)
	}
	cancelled, err := e.s.Cancel(e.ctx, j.ID)
	if err != nil || cancelled.Status != StatusCancelled {
		t.Fatalf("Cancel = %v, %v; want cancelled", cancelled, err)
	}
	e.assertNothingMounted("after the abort")
}

// TestLabDiskUpgradeParity_HappyPathThroughJobSystem proves
// TypeDiskUpgradeParity, submitted through the real scheduler exactly as
// upgradeDisk would submit it, formats the new parity disk, mounts it at
// the fresh slot disk.NextParityMountpoint computes, copies and verifies
// the parity file byte for byte, switches snapraid.conf to it, passes a
// real `snapraid check`, and leaves the array live and protected — the
// old parity disk's own file untouched throughout (doc 02 §4 "Larger
// parity disk", Q71).
func TestLabDiskUpgradeParity_HappyPathThroughJobSystem(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	execRunner := disk.CommandRunner{}
	provider := &disk.LinuxProvider{Lister: disk.NewLister(), Exec: execRunner}
	resetBootContentDir(t)

	parityDev := createLoopImage(ctx, t, execRunner, lab, "p289-parity-old", "320M")
	data1Dev := createLoopImage(ctx, t, execRunner, lab, "p289-parity-data1", "320M")
	data2Dev := createLoopImage(ctx, t, execRunner, lab, "p289-parity-data2", "320M")
	loopSize := int64(320 << 20)

	t.Cleanup(func() {
		unmountIfMounted(execRunner, "/mnt/disk1")
		unmountIfMounted(execRunner, "/mnt/disk2")
		unmountIfMounted(execRunner, "/mnt/parity1")
		unmountIfMounted(execRunner, "/mnt/parity2")
	})

	s := newTestScheduler(t)
	st, genRoot := labRegisterDiskFormat(t, s, provider, execRunner, disk.DirectMounter{Runner: execRunner})

	formatParams := DiskFormatParams{
		Parity: []disk.AssignedDisk{{Device: parityDev, Filesystem: disk.XFS}},
		Data: []disk.AssignedDisk{
			{Device: data1Dev, Filesystem: disk.XFS},
			{Device: data2Dev, Filesystem: disk.XFS},
		},
		Sizes: map[string]int64{parityDev: loopSize, data1Dev: loopSize, data2Dev: loopSize},
	}
	formatParams.Confirmation = formatParams.Plan().Confirmation()
	j, err := s.Submit(ctx, TypeDiskFormat, nil, mustJSON(t, formatParams))
	if err != nil {
		t.Fatalf("Submit(disk_format): %v", err)
	}
	if finished := await(t, s, j.ID); finished.Status != StatusSucceeded {
		t.Fatalf("disk_format status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}

	confPath := filepath.Join(genRoot, "snapraid.conf")
	engine := &parity.SnapraidEngine{
		ConfPath: confPath,
		LogDir:   filepath.Join(genRoot, "snapraid-logs"),
		Runner:   parity.CommandRunner{},
		Guard:    parity.Guard{Config: parity.GuardConfig{RemovedFilesMax: 100000, RemovedUpdatedPercent: 100}},
	}

	writeLabFile(t, filepath.Join("/mnt/disk1", "before-upgrade.bin"), 300_000)
	syncCh, err := engine.Sync(ctx, parity.SyncOpts{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if final := drainRealProgress(t, syncCh); final.Err != nil {
		t.Fatalf("Sync failed: %v", final.Err)
	}

	oldParityFile := filepath.Join("/mnt/parity1", "snapraid.parity")
	oldBytesBefore, err := os.ReadFile(oldParityFile)
	if err != nil {
		t.Fatalf("reading old parity file before upgrade: %v", err)
	}
	if len(oldBytesBefore) == 0 {
		t.Fatal("old parity file is empty after a real sync — nothing to copy")
	}

	newDev := createLoopImage(ctx, t, execRunner, lab, "p289-parity-new", "340M")
	newSize := int64(340 << 20)
	newMountpoint := disk.NextParityMountpoint([]string{"/mnt/parity1", "/mnt/disk1", "/mnt/disk2"})
	if newMountpoint != "/mnt/parity2" {
		t.Fatalf("NextParityMountpoint = %q, want /mnt/parity2", newMountpoint)
	}

	mounter := disk.DirectMounter{Runner: execRunner}
	s.registry.Register(TypeDiskUpgradeParity, true, RunDiskUpgradeParity(DiskUpgradeParityDeps{
		Provider:       provider,
		Runner:         execRunner,
		Store:          st,
		Generator:      config.NewGenerator(genRoot),
		Mounter:        mounter,
		UpgradeMounter: mounter,
		Parity:         engine,
	}))

	replacement := disk.AssignedDisk{Device: newDev, Filesystem: disk.XFS}
	upgradeParams := DiskUpgradeParityParams{
		Confirmation:  SingleDiskConfirmation(replacement),
		Mountpoint:    "/mnt/parity1",
		NewMountpoint: newMountpoint,
		Disk:          replacement,
		Sizes:         map[string]int64{data1Dev: loopSize, data2Dev: loopSize, newDev: newSize},
	}
	uj, err := s.Submit(ctx, TypeDiskUpgradeParity, nil, mustJSON(t, upgradeParams))
	if err != nil {
		t.Fatalf("Submit(disk_upgrade_parity): %v", err)
	}
	finished := await(t, s, uj.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("disk_upgrade_parity status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}

	if got := blkidType(ctx, execRunner, newDev); got != "xfs" {
		t.Fatalf("new parity disk %s: blkid TYPE = %q, want xfs", newDev, got)
	}

	// Release unmounts the old parity disk, so its file is no longer
	// reachable at /mnt/parity1 — remount
	// the same physical disk at a scratch path to confirm its content is
	// still exactly what it was before the upgrade.
	oldParityRemount := filepath.Join(lab, "p289-parity-old-remount")
	if err := os.MkdirAll(oldParityRemount, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", oldParityRemount, err)
	}
	t.Cleanup(func() { unmountIfMounted(execRunner, oldParityRemount) })
	if err := mounter.Mount(ctx, disk.MountUnit{
		Where:      oldParityRemount,
		UUID:       blkidUUID(ctx, execRunner, parityDev),
		Filesystem: disk.XFS,
	}); err != nil {
		t.Fatalf("remounting the old parity disk to verify its content: %v", err)
	}

	oldBytesAfter, err := os.ReadFile(filepath.Join(oldParityRemount, "snapraid.parity"))
	if err != nil {
		t.Fatalf("reading old parity file after upgrade: %v", err)
	}
	if string(oldBytesAfter) != string(oldBytesBefore) {
		t.Fatal("old parity file's content changed during a successful upgrade")
	}
	newParityFile := filepath.Join(newMountpoint, "snapraid.parity")
	newBytes, err := os.ReadFile(newParityFile)
	if err != nil {
		t.Fatalf("reading new parity file after upgrade: %v", err)
	}
	if string(newBytes) != string(oldBytesBefore) {
		t.Fatal("new parity file does not match the old one byte for byte")
	}

	_, disks, err := st.GetArray(ctx)
	if err != nil {
		t.Fatalf("GetArray: %v", err)
	}
	var found bool
	for _, d := range disks {
		if d.Device == newDev && d.Mountpoint == newMountpoint {
			found = true
		}
	}
	if !found {
		t.Fatalf("no array disk with device=%s mountpoint=%s after upgrade: %+v", newDev, newMountpoint, disks)
	}

	// A further write, synced against the new parity disk, proves it is
	// genuinely live and writable, not merely a copy nobody uses.
	writeLabFile(t, filepath.Join("/mnt/disk2", "after-upgrade.bin"), 90_000)
	postCh, err := engine.Sync(ctx, parity.SyncOpts{})
	if err != nil {
		t.Fatalf("post-upgrade Sync: %v", err)
	}
	if final := drainRealProgress(t, postCh); final.Err != nil {
		t.Fatalf("post-upgrade Sync failed: %v", final.Err)
	}
	diff, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("post-upgrade Diff: %v", err)
	}
	if diff.Added != 0 || diff.Removed != 0 || diff.Updated != 0 {
		t.Fatalf("post-upgrade Diff reported pending changes: %+v, want none", diff)
	}
}
