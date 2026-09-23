package job

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/store"
)

// Tests in this file name the doc 02 §4 state-machine cell they cover
// (E1–E8, UR1–UR9, invariants) in the test name.

const (
	upgradeOldUUID = "uuid-a"
	upgradeNewUUID = "uuid-b"
)

// recorder is a goroutine-safe event log shared by the fakes below.
type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(e string) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

func (r *recorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

type recService struct {
	name    string
	rec     *recorder
	mu      sync.Mutex
	stopErr error
	block   chan struct{}
}

func (s *recService) Name() string { return s.name }

func (s *recService) Stop(ctx context.Context) error {
	s.mu.Lock()
	block := s.block
	err := s.stopErr
	s.mu.Unlock()
	if block != nil {
		<-block
	}
	s.rec.add("stop:" + s.name)
	return err
}

func (s *recService) Start(ctx context.Context) error {
	s.rec.add("start:" + s.name)
	return nil
}

func (s *recService) setStopErr(err error) {
	s.mu.Lock()
	s.stopErr = err
	s.mu.Unlock()
}

type recMount struct {
	where string
	rec   *recorder
	// table, when set, is mounted and unmounted along with the recording,
	// the way a real mount is visible in the kernel mount table.
	table *FakeMountTable
	uuid  string
}

func (m *recMount) Where() string { return m.where }

func (m *recMount) Mount(ctx context.Context) error {
	m.rec.add("mount:" + m.where)
	if m.table != nil {
		return m.table.Mount(ctx, disk.MountUnit{Where: m.where, UUID: m.uuid})
	}
	return nil
}

func (m *recMount) Unmount(ctx context.Context) error {
	m.rec.add("unmount:" + m.where)
	if m.table != nil {
		if mounted, _ := m.table.IsMounted(ctx, m.where); mounted {
			return m.table.UnmountOnce(ctx, m.where)
		}
	}
	return nil
}

// scriptedDiffEngine is parity.FakeEngine with Diff answered per call:
// call n (1-based) returns diffs(n). block, when set for call n, holds
// that call until the channel is closed.
type scriptedDiffEngine struct {
	*parity.FakeEngine
	mu      sync.Mutex
	calls   int
	diffs   func(n int) (parity.DiffReport, error)
	block   map[int]chan struct{}
	entered map[int]chan struct{}
}

func newScriptedDiffEngine(diffs func(n int) (parity.DiffReport, error)) *scriptedDiffEngine {
	return &scriptedDiffEngine{FakeEngine: parity.NewFakeEngine(), diffs: diffs, block: map[int]chan struct{}{}, entered: map[int]chan struct{}{}}
}

// holdCall makes call n wait until the returned release channel is
// closed; entered is closed once call n has started.
func (e *scriptedDiffEngine) holdCall(n int) (entered, release chan struct{}) {
	e.mu.Lock()
	defer e.mu.Unlock()
	entered = make(chan struct{})
	release = make(chan struct{})
	e.block[n] = release
	e.entered[n] = entered
	return entered, release
}

func (e *scriptedDiffEngine) Diff(ctx context.Context) (parity.DiffReport, error) {
	e.mu.Lock()
	e.calls++
	n := e.calls
	block := e.block[n]
	entered := e.entered[n]
	e.mu.Unlock()
	if entered != nil {
		close(entered)
	}
	if block != nil {
		<-block
	}
	return e.diffs(n)
}

func (e *scriptedDiffEngine) diffCalls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

func cleanDiffs(int) (parity.DiffReport, error) { return parity.DiffReport{}, nil }

type upgradeHarness struct {
	t        *testing.T
	ctx      context.Context
	s        *Scheduler
	st       *store.ArrayStore
	provider *disk.FakeProvider
	runner   *disk.FakeRunner
	mounts   *FakeMountTable
	engine   *scriptedDiffEngine
	rec      *recorder
	samba    *recService
	nfs      *recService
	seq      *ArraySequence
	genRoot  string
	logDir   string
	oldWhere string
	staging  string
	deps     DiskUpgradeDataDeps

	arrayReadyMu    sync.Mutex
	arrayReadyErr   error
	arrayReadyBlock chan struct{}
	arrayReadyIn    chan struct{}
	arrayReadyCalls int
}

func newUpgradeHarness(t *testing.T) *upgradeHarness {
	t.Helper()
	ctx := context.Background()
	db := newTestDB(t)
	logDir := t.TempDir()
	s := NewScheduler(NewStore(db), NewLogStore(logDir), NewHub(), NewRegistry())
	st := store.NewArrayStore(db)
	root := t.TempDir()
	h := &upgradeHarness{
		t:        t,
		ctx:      ctx,
		s:        s,
		st:       st,
		provider: disk.NewFakeProvider(),
		runner:   disk.NewFakeRunner(),
		mounts:   NewFakeMountTable(),
		engine:   newScriptedDiffEngine(cleanDiffs),
		rec:      &recorder{},
		genRoot:  t.TempDir(),
		logDir:   logDir,
		oldWhere: filepath.Join(root, "disk1"),
		staging:  filepath.Join(root, "staging", "disk1"),
	}
	if err := os.MkdirAll(h.oldWhere, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(h.staging, 0o755); err != nil {
		t.Fatal(err)
	}
	writeUpgradeTree(t, h.oldWhere)

	if err := st.PutArray(ctx, store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "20G", CreatedAt: time.Now()}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", Mountpoint: "/mnt/parity1"},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: upgradeOldUUID, Mountpoint: h.oldWhere},
		{Role: store.ArrayRoleData, RoleIndex: 2, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "uuid-d2", Mountpoint: "/mnt/disk2"},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	h.provider.AddDisk("/dev/sda", disk.Disk{Size: 16 * disk.TB})
	h.provider.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB})
	h.provider.AddDisk("/dev/sdc", disk.Disk{Size: 4 * disk.TB})
	h.provider.AddDisk("/dev/sdz", disk.Disk{Size: 10 * disk.TB})
	h.runner.Script("blkid", []string{"-s", "UUID", "-o", "value", "/dev/sdz"}, []byte(upgradeNewUUID+"\n"), nil)

	h.samba = &recService{name: "samba", rec: h.rec}
	h.nfs = &recService{name: "nfs", rec: h.rec}
	h.seq = &ArraySequence{
		Scheduler:   s,
		Services:    []ArrayService{h.samba, h.nfs},
		ShareMounts: []ArrayMount{&recMount{where: "/mnt/user/media", rec: h.rec, table: h.mounts, uuid: "fuse"}},
		CatchAll:    &recMount{where: "/mnt/user", rec: h.rec, table: h.mounts, uuid: "fuse"},
		Disks: []ArrayMount{
			&recMount{where: "/mnt/parity1", rec: h.rec, table: h.mounts, uuid: "uuid-p"},
			&recMount{where: h.oldWhere, rec: h.rec, table: h.mounts, uuid: upgradeOldUUID},
			&recMount{where: "/mnt/disk2", rec: h.rec, table: h.mounts, uuid: "uuid-d2"},
		},
	}
	h.deps = DiskUpgradeDataDeps{
		Provider:    h.provider,
		Runner:      h.runner,
		Store:       st,
		Generator:   config.NewGenerator(h.genRoot),
		Parity:      h.engine,
		Mounts:      h.mounts,
		Array:       func() *ArraySequence { return h.seq },
		ArrayReady:  h.arrayReady,
		StagingPath: func(string) string { return h.staging },
		Sleep:       func(time.Duration) {},
	}
	return h
}

func (h *upgradeHarness) arrayReady(ctx context.Context) error {
	h.arrayReadyMu.Lock()
	h.arrayReadyCalls++
	err := h.arrayReadyErr
	block := h.arrayReadyBlock
	in := h.arrayReadyIn
	h.arrayReadyMu.Unlock()
	if in != nil {
		close(in)
	}
	if block != nil {
		<-block
	}
	return err
}

func (h *upgradeHarness) readyCalls() int {
	h.arrayReadyMu.Lock()
	defer h.arrayReadyMu.Unlock()
	return h.arrayReadyCalls
}

func (h *upgradeHarness) setArrayReady(err error, in, block chan struct{}) {
	h.arrayReadyMu.Lock()
	h.arrayReadyErr = err
	h.arrayReadyIn = in
	h.arrayReadyBlock = block
	h.arrayReadyMu.Unlock()
}

// runBlockingTopologyJob starts a topology-class job that runs until the
// returned func is called, so an upgrade resumed meanwhile is queued
// behind it. The func releases the job and waits for it to end.
func (h *upgradeHarness) runBlockingTopologyJob() func() {
	h.t.Helper()
	blocker := make(chan struct{})
	h.s.registry.Register(TypePoolRemount, false, func(ctx context.Context, rc *RunContext) error {
		<-blocker
		return nil
	})
	j, err := h.s.Submit(h.ctx, TypePoolRemount, nil, nil)
	if err != nil {
		h.t.Fatalf("Submit(blocking topology job): %v", err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			close(blocker)
			h.await(j.ID)
		})
	}
}

func (h *upgradeHarness) register() {
	h.s.registry.Register(TypeDiskUpgradeData, true, RunDiskUpgradeData(h.deps))
	h.s.registry.RegisterAbort(TypeDiskUpgradeData, AbortDiskUpgradeData(h.deps))
}

// stopArray runs the real stop sequence, which UR3 requires before a
// data-disk upgrade is admitted.
func (h *upgradeHarness) stopArray() {
	h.t.Helper()
	if err := h.seq.Stop(h.ctx); err != nil {
		h.t.Fatalf("ArraySequence.Stop: %v", err)
	}
}

func (h *upgradeHarness) params() []byte {
	return mustJSON(h.t, h.paramsValue())
}

func (h *upgradeHarness) paramsValue() DiskUpgradeDataParams {
	newDisk := disk.AssignedDisk{Device: "/dev/sdz", Filesystem: disk.XFS}
	return DiskUpgradeDataParams{
		Confirmation: SingleDiskConfirmation(newDisk),
		Mountpoint:   h.oldWhere,
		Old:          disk.AssignedDisk{Device: "/dev/sdb", Filesystem: disk.XFS, FSUUID: upgradeOldUUID},
		Disk:         newDisk,
		Sizes:        map[string]int64{"/dev/sda": 16 * disk.TB, "/dev/sdb": 4 * disk.TB, "/dev/sdc": 4 * disk.TB, "/dev/sdz": 10 * disk.TB},
	}
}

func (h *upgradeHarness) submit() *Job {
	h.t.Helper()
	j, err := h.s.Submit(h.ctx, TypeDiskUpgradeData, nil, h.params())
	if err != nil {
		h.t.Fatalf("Submit(TypeDiskUpgradeData): %v", err)
	}
	return j
}

// seedInterrupted writes an interrupted data-disk upgrade at cp straight
// into the store — the state a run, a restart or a failed step leaves.
func (h *upgradeHarness) seedInterrupted(cp *disk.DataDiskUpgradeCheckpoint) *Job {
	h.t.Helper()
	var data []byte
	if cp != nil {
		data = mustJSON(h.t, *cp)
	}
	now := time.Now().UTC()
	j := &Job{
		ID:          uuid.NewString(),
		Type:        TypeDiskUpgradeData,
		Class:       ClassTopology,
		Status:      StatusInterrupted,
		Resumable:   true,
		Cancellable: true,
		Params:      h.params(),
		Checkpoint:  data,
		CreatedAt:   now,
		StartedAt:   &now,
		FinishedAt:  &now,
	}
	if err := h.s.store.Create(h.ctx, j); err != nil {
		h.t.Fatalf("seeding interrupted job: %v", err)
	}
	return j
}

func (h *upgradeHarness) await(id string) *Job {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
	defer cancel()
	j, err := h.s.Await(ctx, id)
	if err != nil {
		h.t.Fatalf("Await: %v", err)
	}
	return j
}

func (h *upgradeHarness) slotUUID() string {
	h.t.Helper()
	_, disks, err := h.st.GetArray(h.ctx)
	if err != nil {
		h.t.Fatalf("GetArray: %v", err)
	}
	for _, d := range disks {
		if d.Mountpoint == h.oldWhere {
			return d.FSUUID
		}
	}
	h.t.Fatalf("no slot at %s", h.oldWhere)
	return ""
}

func (h *upgradeHarness) checkpoint(id string) disk.DataDiskUpgradeCheckpoint {
	h.t.Helper()
	j, err := h.s.store.Get(h.ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	var cp disk.DataDiskUpgradeCheckpoint
	if len(j.Checkpoint) > 0 {
		if err := json.Unmarshal(j.Checkpoint, &cp); err != nil {
			h.t.Fatal(err)
		}
	}
	return cp
}

func (h *upgradeHarness) jobLog(id string) string {
	h.t.Helper()
	r, err := h.s.logs.Open(id)
	if err != nil {
		h.t.Fatalf("opening job log: %v", err)
	}
	defer func() { _ = r.Close() }()
	gz, err := gzip.NewReader(r)
	if err != nil {
		h.t.Fatalf("opening job log: %v", err)
	}
	b, err := io.ReadAll(gz)
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}

// assertStopped is the Stopped mount set: nothing is mounted.
func (h *upgradeHarness) assertStopped() {
	h.t.Helper()
	if m := h.mounts.MountedPaths(); len(m) != 0 {
		sort.Strings(m)
		h.t.Fatalf("still mounted after Unwind: %v, want the Stopped set (nothing)", m)
	}
}

func (h *upgradeHarness) mountCount(path, uuid string) int {
	n := 0
	for _, op := range h.mounts.Ops() {
		if op == "mount "+filepath.Clean(path)+" "+uuid {
			n++
		}
	}
	return n
}

func (h *upgradeHarness) generatedUnitFor(where string) string {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.genRoot, "systemd", "system", disk.UnitFileName(where)))
	if err != nil {
		return ""
	}
	return string(b)
}

// writeUpgradeTree writes the same deterministic tree every time, so a
// staging copy built with it verifies against the old disk.
func writeUpgradeTree(t *testing.T, root string) {
	t.Helper()
	mtime := time.Date(2021, 5, 6, 7, 8, 9, 0, time.UTC)
	if err := os.MkdirAll(filepath.Join(root, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"a/one.bin": bytes.Repeat([]byte("hoserva"), 400),
		"b.txt":     []byte("the old disk's second file\n"),
	}
	for rel, data := range files {
		p := filepath.Join(root, rel)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{filepath.Join(root, "a"), root} {
		if err := os.Chtimes(d, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
}

// treeDigest hashes every path, mode, mtime and content under root.
func treeDigest(t *testing.T, root string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		_, _ = fmt.Fprintf(h, "%s|%v|%d|", rel, info.Mode(), info.ModTime().UnixNano())
		if info.Mode().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			h.Write(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// E1: a fresh upgrade runs from None to Done — Unwind, Establish(Old),
// the pre-format diff, format, copy, verify, remount, the clean diff,
// Release and the final Unwind.
func TestDiskUpgradeData_E1_FreshRunReleasesAndEndsStopped(t *testing.T) {
	h := newUpgradeHarness(t)
	h.register()
	h.stopArray()
	before := treeDigest(t, h.oldWhere)

	j := h.submit()
	done := h.await(j.ID)
	if done.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s: %s), want succeeded", done.Status, done.ErrorCode, done.ErrorMessage)
	}
	if got := h.slotUUID(); got != upgradeNewUUID {
		t.Fatalf("SQLite names %s for the slot, want %s (Done)", got, upgradeNewUUID)
	}
	_, disks, err := h.st.GetArray(h.ctx)
	if err != nil {
		t.Fatalf("GetArray: %v", err)
	}
	for _, d := range disks {
		if d.Mountpoint != h.oldWhere {
			continue
		}
		if !d.SizeSet || d.Size != 10*disk.TB {
			t.Fatalf("upgraded slot size = %d set=%v, want 10TiB set (#341)", d.Size, d.SizeSet)
		}
	}
	h.assertStopped()
	if unit := h.generatedUnitFor(h.oldWhere); !strings.Contains(unit, "/dev/disk/by-uuid/"+upgradeNewUUID) {
		t.Fatalf("the slot's generated mount unit does not name B:\n%s", unit)
	}
	if got, _ := h.provider.FormattedAs("/dev/sdz"); got != disk.XFS {
		t.Fatalf("B formatted as %q, want xfs", got)
	}
	if h.engine.diffCalls() != 2 {
		t.Fatalf("snapraid diff ran %d times, want 2 (before formatting, then against B)", h.engine.diffCalls())
	}
	// Invariant 2: A is read, never written, and mounted only for Old and
	// Copy — never again once B is at S.
	if after := treeDigest(t, h.oldWhere); after != before {
		t.Fatal("the old disk's tree changed during the upgrade")
	}
	if n := h.mountCount(h.oldWhere, upgradeOldUUID); n != 1 {
		t.Fatalf("A was mounted at S %d times, want exactly once (Establish(Old))", n)
	}
	ops := h.mounts.Ops()
	firstB := indexOf(ops, "mount "+h.oldWhere+" "+upgradeNewUUID)
	if firstB < 0 {
		t.Fatalf("B was never mounted at S: %v", ops)
	}
	for _, op := range ops[firstB:] {
		if op == "mount "+h.oldWhere+" "+upgradeOldUUID {
			t.Fatalf("A was mounted again after B took S: %v", ops)
		}
	}
	// Establish(Old) mounts the other disks and A before anything reads.
	wantPrefix := []string{"mount /mnt/parity1 uuid-p", "mount /mnt/disk2 uuid-d2", "mount " + h.oldWhere + " " + upgradeOldUUID}
	if len(ops) < 3 || strings.Join(ops[:3], ",") != strings.Join(wantPrefix, ",") {
		t.Fatalf("first mounts = %v, want %v", ops, wantPrefix)
	}
	stored, err := h.s.store.Get(h.ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Cancellable {
		t.Fatal("the job is still reported cancellable after its releasing checkpoint (UR7)")
	}
	if !h.s.InMaintenance() {
		t.Fatal("maintenance mode ended with the upgrade; the array stays stopped until the user starts it (E1 Releasing)")
	}
	if h.readyCalls() != 1 {
		t.Fatalf("array sequence rebuilt %d times, want 1 (Release)", h.readyCalls())
	}
}

// TestDiskUpgradeData_WeakIdentityPersistsSizeBytes is #341 at the job
// write: a weak-identity replacement's size must land in array_disks so
// Q21's UUID+size match does not fall back to UUID only.
func TestDiskUpgradeData_WeakIdentityPersistsSizeBytes(t *testing.T) {
	h := newUpgradeHarness(t)
	h.provider.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB, WeakIdentity: true, FSUUID: upgradeOldUUID})
	h.provider.AddDisk("/dev/sdz", disk.Disk{Size: 10 * disk.TB, WeakIdentity: true})
	params := h.paramsValue()
	params.Old.WeakIdentity = true
	params.Disk.WeakIdentity = true
	h.register()
	h.stopArray()

	j, err := h.s.Submit(h.ctx, TypeDiskUpgradeData, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	done := h.await(j.ID)
	if done.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s: %s), want succeeded", done.Status, done.ErrorCode, done.ErrorMessage)
	}
	_, disks, err := h.st.GetArray(h.ctx)
	if err != nil {
		t.Fatalf("GetArray: %v", err)
	}
	for _, d := range disks {
		if d.Mountpoint != h.oldWhere {
			continue
		}
		if !d.WeakIdentity || d.FSUUID != upgradeNewUUID {
			t.Fatalf("upgraded slot = %+v, want weak-identity %s", d, upgradeNewUUID)
		}
		if !d.SizeSet || d.Size != 10*disk.TB {
			t.Fatalf("upgraded slot size = %d set=%v, want 10TiB set", d.Size, d.SizeSet)
		}
		return
	}
	t.Fatalf("no data disk at %s", h.oldWhere)
}

func indexOf(ops []string, want string) int {
	for i, op := range ops {
		if op == want {
			return i
		}
	}
	return -1
}

// E2 None, dirty diff: failed with disk_upgrade_array_not_synced, and
// nothing was formatted. Edge case "Dirty diff before Formatting".
func TestDiskUpgradeData_E2None_DirtyDiffFailsWithoutFormatting(t *testing.T) {
	h := newUpgradeHarness(t)
	h.engine.diffs = func(int) (parity.DiffReport, error) { return parity.DiffReport{Added: 3}, nil }
	h.register()
	h.stopArray()

	done := h.await(h.submit().ID)
	if done.Status != StatusFailed || done.ErrorCode != codeDiskUpgradeArrayNotSynced {
		t.Fatalf("status = %s/%s, want failed/%s", done.Status, done.ErrorCode, codeDiskUpgradeArrayNotSynced)
	}
	if !strings.Contains(done.ErrorMessage, "start the array, sync it, stop it again") {
		t.Fatalf("message %q does not tell the user how to proceed", done.ErrorMessage)
	}
	if _, formatted := h.provider.FormattedAs("/dev/sdz"); formatted {
		t.Fatal("B was formatted despite a dirty pre-format diff")
	}
	if h.slotUUID() != upgradeOldUUID {
		t.Fatal("SQLite no longer names A")
	}
	h.assertStopped()
}

// E2 None, any other error: A's UUID at S does not match what the job
// fixed at submit (UR4), so Establish(Old) fails. Failed, nothing
// formatted. Edge case "The copy source is an unmounted directory".
func TestDiskUpgradeData_E2None_UnconfirmedOldDiskFailsWithoutFormatting(t *testing.T) {
	h := newUpgradeHarness(t)
	h.mounts.ReportUUID[h.oldWhere] = "uuid-someone-else"
	h.register()
	h.stopArray()

	done := h.await(h.submit().ID)
	if done.Status != StatusFailed {
		t.Fatalf("status = %s, want failed", done.Status)
	}
	if _, formatted := h.provider.FormattedAs("/dev/sdz"); formatted {
		t.Fatal("B was formatted although A was not confirmed at S")
	}
	if h.engine.diffCalls() != 0 {
		t.Fatal("snapraid diff ran against an unconfirmed slot")
	}
	if entries := dirEntries(t, h.staging); len(entries) != 0 {
		t.Fatalf("staging holds %v, want nothing copied", entries)
	}
	h.assertStopped()
}

// E2 Formatting: G does not hold B's new UUID after it is mounted. Failed.
func TestDiskUpgradeData_E2Formatting_StagingUUIDMismatchFails(t *testing.T) {
	h := newUpgradeHarness(t)
	h.mounts.ReportUUID[h.staging] = "uuid-wrong"
	h.register()
	h.stopArray()

	done := h.await(h.submit().ID)
	if done.Status != StatusFailed {
		t.Fatalf("status = %s, want failed", done.Status)
	}
	if entries := dirEntries(t, h.staging); len(entries) != 0 {
		t.Fatalf("staging holds %v, want nothing copied through an unconfirmed G", entries)
	}
	if h.slotUUID() != upgradeOldUUID {
		t.Fatal("SQLite no longer names A")
	}
	h.assertStopped()
}

// E2 Copying: a copy error ends the job interrupted at copying with B's
// UUID, resumable.
func TestDiskUpgradeData_E2Copying_ErrorInterruptsAtCopying(t *testing.T) {
	h := newUpgradeHarness(t)
	// A directory where the copy writes a regular file fails that entry.
	if err := os.MkdirAll(filepath.Join(h.staging, "b.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.register()
	h.stopArray()

	j := h.submit()
	done := h.await(j.ID)
	if done.Status != StatusInterrupted || done.ErrorCode != "job_needs_retry" {
		t.Fatalf("status = %s/%s, want interrupted/job_needs_retry", done.Status, done.ErrorCode)
	}
	cp := h.checkpoint(j.ID)
	if cp.Phase != disk.DataDiskUpgradePhaseCopying || cp.NewUUID != upgradeNewUUID || cp.LastPath != "a/one.bin" {
		t.Fatalf("checkpoint = %+v, want copying after a/one.bin with B's UUID", cp)
	}
	if h.slotUUID() != upgradeOldUUID {
		t.Fatal("SQLite no longer names A")
	}
	h.assertStopped()
}

// E2 Verifying, mismatch: failed with disk_upgrade_copy_mismatch naming
// the path; A is still the array's disk.
func TestDiskUpgradeData_E2Verifying_MismatchFails(t *testing.T) {
	h := newUpgradeHarness(t)
	writeUpgradeTree(t, h.staging)
	if err := os.WriteFile(filepath.Join(h.staging, "b.txt"), []byte("different"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.register()
	h.stopArray()
	j := h.seedInterrupted(&disk.DataDiskUpgradeCheckpoint{Phase: disk.DataDiskUpgradePhaseVerifying, NewUUID: upgradeNewUUID})

	if _, err := h.s.Resume(h.ctx, j.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	done := h.await(j.ID)
	if done.Status != StatusFailed || done.ErrorCode != codeDiskUpgradeCopyMismatch {
		t.Fatalf("status = %s/%s, want failed/%s", done.Status, done.ErrorCode, codeDiskUpgradeCopyMismatch)
	}
	if !strings.Contains(done.ErrorMessage, "b.txt") {
		t.Fatalf("message %q does not name the mismatched path", done.ErrorMessage)
	}
	if h.slotUUID() != upgradeOldUUID {
		t.Fatal("SQLite no longer names A")
	}
	h.assertStopped()
}

// E2 Verifying, any other error: interrupted at verifying.
func TestDiskUpgradeData_E2Verifying_OtherErrorInterrupts(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory without permission")
	}
	h := newUpgradeHarness(t)
	writeUpgradeTree(t, h.staging)
	locked := filepath.Join(h.staging, "a")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	h.register()
	h.stopArray()
	j := h.seedInterrupted(&disk.DataDiskUpgradeCheckpoint{Phase: disk.DataDiskUpgradePhaseVerifying, NewUUID: upgradeNewUUID})

	if _, err := h.s.Resume(h.ctx, j.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	done := h.await(j.ID)
	if done.Status != StatusInterrupted || done.ErrorCode != "job_needs_retry" {
		t.Fatalf("status = %s/%s, want interrupted/job_needs_retry", done.Status, done.ErrorCode)
	}
	if cp := h.checkpoint(j.ID); cp.Phase != disk.DataDiskUpgradePhaseVerifying {
		t.Fatalf("checkpoint = %+v, want verifying", cp)
	}
	h.assertStopped()
}

// E2 Remounting: S does not hold B's UUID after the swap. Interrupted at
// remounting, SQLite names A, and no diff or release ran.
func TestDiskUpgradeData_E2Remounting_UnconfirmedSwapInterrupts(t *testing.T) {
	h := newUpgradeHarness(t)
	h.register()
	h.stopArray()
	// S reports A's UUID whatever is mounted there: Establish(Old) passes,
	// the post-swap confirmation of B does not.
	h.mounts.ReportUUID[h.oldWhere] = upgradeOldUUID

	j := h.submit()
	done := h.await(j.ID)
	if done.Status != StatusInterrupted || done.ErrorCode != "job_needs_retry" {
		t.Fatalf("status = %s/%s, want interrupted/job_needs_retry", done.Status, done.ErrorCode)
	}
	if cp := h.checkpoint(j.ID); cp.Phase != disk.DataDiskUpgradePhaseRemounting {
		t.Fatalf("checkpoint = %+v, want remounting", cp)
	}
	if h.engine.diffCalls() != 1 {
		t.Fatalf("diff calls = %d, want 1 (only the pre-format gate)", h.engine.diffCalls())
	}
	if h.slotUUID() != upgradeOldUUID {
		t.Fatal("SQLite no longer names A")
	}
	h.assertStopped()
}

// E2 Diffing, dirty: Unwind, then failed with disk_upgrade_diff_not_clean
// and the counts; SQLite names A and the mount unit was not regenerated.
// Edge cases "Dirty diff after Remounting" and "Dirty diff reported as
// success" (invariant 3).
func TestDiskUpgradeData_E2Diffing_DirtyDiffFailsWithSQLiteNamingA(t *testing.T) {
	h := newUpgradeHarness(t)
	h.engine.diffs = func(n int) (parity.DiffReport, error) {
		if n == 1 {
			return parity.DiffReport{}, nil
		}
		return parity.DiffReport{Removed: 2, Updated: 1}, nil
	}
	h.register()
	h.stopArray()

	done := h.await(h.submit().ID)
	if done.Status != StatusFailed || done.ErrorCode != codeDiskUpgradeDiffNotClean {
		t.Fatalf("status = %s/%s, want failed/%s", done.Status, done.ErrorCode, codeDiskUpgradeDiffNotClean)
	}
	if !strings.Contains(done.ErrorMessage, "2 removed, 1 updated") {
		t.Fatalf("message %q does not carry the counts", done.ErrorMessage)
	}
	if h.slotUUID() != upgradeOldUUID {
		t.Fatal("SQLite names B after a dirty diff")
	}
	if h.readyCalls() != 0 {
		t.Fatal("Release ran after a dirty diff")
	}
	h.assertStopped()
	// Ended: array start is allowed and mounts A (E3 After cancelled / E6).
	if err := h.seq.Start(h.ctx); err != nil {
		t.Fatalf("array start after the failed upgrade: %v", err)
	}
}

// E2 Diffing, error: interrupted at diffing, resumable.
func TestDiskUpgradeData_E2Diffing_DiffErrorInterrupts(t *testing.T) {
	h := newUpgradeHarness(t)
	h.engine.diffs = func(n int) (parity.DiffReport, error) {
		if n == 1 {
			return parity.DiffReport{}, nil
		}
		return parity.DiffReport{}, errors.New("snapraid: content file unreadable")
	}
	h.register()
	h.stopArray()

	j := h.submit()
	done := h.await(j.ID)
	if done.Status != StatusInterrupted || done.ErrorCode != "job_needs_retry" {
		t.Fatalf("status = %s/%s, want interrupted/job_needs_retry", done.Status, done.ErrorCode)
	}
	if cp := h.checkpoint(j.ID); cp.Phase != disk.DataDiskUpgradePhaseDiffing {
		t.Fatalf("checkpoint = %+v, want diffing", cp)
	}
	h.assertStopped()
}

// E2 Releasing, the transaction fails: interrupted at releasing, SQLite
// still names A. B's UUID colliding with disk2's makes the UPDATE fail.
func TestDiskUpgradeData_E2Releasing_TransactionFailsInterruptsWithSQLiteNamingA(t *testing.T) {
	h := newUpgradeHarness(t)
	h.register()
	h.stopArray()
	j := h.seedInterrupted(&disk.DataDiskUpgradeCheckpoint{Phase: disk.DataDiskUpgradePhaseReleasing, NewUUID: "uuid-d2"})

	if _, err := h.s.Resume(h.ctx, j.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	done := h.await(j.ID)
	if done.Status != StatusInterrupted || done.ErrorCode != "job_needs_retry" {
		t.Fatalf("status = %s/%s, want interrupted/job_needs_retry", done.Status, done.ErrorCode)
	}
	if h.slotUUID() != upgradeOldUUID {
		t.Fatal("SQLite changed although the transaction failed")
	}
	h.assertStopped()
}

// E2 Releasing, regeneration or rebuild fails after the commit:
// interrupted at releasing with SQLite naming B; Cancel and array start
// are refused (E3, E6); the resume regenerates S's unit and succeeds (E5
// Interrupted at releasing). Edge case "Regeneration fails after
// Release's commit, then the job is resumed".
func TestDiskUpgradeData_E2Releasing_RebuildFailsAfterCommitThenResumeRegenerates(t *testing.T) {
	h := newUpgradeHarness(t)
	h.setArrayReady(errors.New("rebuilding: disk inventory unavailable"), nil, nil)
	h.register()
	h.stopArray()

	j := h.submit()
	done := h.await(j.ID)
	if done.Status != StatusInterrupted || done.ErrorCode != "job_needs_retry" {
		t.Fatalf("status = %s/%s, want interrupted/job_needs_retry", done.Status, done.ErrorCode)
	}
	if h.slotUUID() != upgradeNewUUID {
		t.Fatal("SQLite does not name B after the committed transaction")
	}
	if cp := h.checkpoint(j.ID); cp.Phase != disk.DataDiskUpgradePhaseReleasing {
		t.Fatalf("checkpoint = %+v, want releasing", cp)
	}
	h.assertStopped()

	if _, err := h.s.Cancel(h.ctx, j.ID); !errors.Is(err, ErrJobNotCancellable) {
		t.Fatalf("Cancel(interrupted at releasing) = %v, want job_not_cancellable (E3)", err)
	}
	if err := h.seq.Start(h.ctx); !errors.Is(err, ErrDiskUpgradeDataPending) {
		t.Fatalf("array start while interrupted at releasing = %v, want disk_upgrade_pending (E6)", err)
	}

	// Make the regenerated unit visibly stale, then resume.
	unitPath := filepath.Join(h.genRoot, "systemd", "system", disk.UnitFileName(h.oldWhere))
	if err := os.WriteFile(unitPath, []byte("stale unit naming "+upgradeOldUUID), 0o644); err != nil {
		t.Fatal(err)
	}
	h.setArrayReady(nil, nil, nil)
	before := len(h.mounts.Ops())
	if _, err := h.s.Resume(h.ctx, j.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	done = h.await(j.ID)
	if done.Status != StatusSucceeded {
		t.Fatalf("status after resume = %s (%s), want succeeded", done.Status, done.ErrorMessage)
	}
	if unit := h.generatedUnitFor(h.oldWhere); !strings.Contains(unit, "/dev/disk/by-uuid/"+upgradeNewUUID) {
		t.Fatalf("resume at releasing did not regenerate S's unit:\n%s", unit)
	}
	for _, op := range h.mounts.Ops()[before:] {
		if strings.HasPrefix(op, "mount ") {
			t.Fatalf("resume at releasing mounted %q, want no disk mounted", op)
		}
	}
	h.assertStopped()
}

// E2 Any, final Unwind fails: the job is interrupted at its last saved
// checkpoint with disk_upgrade_cleanup_failed naming what is still
// mounted, even though the run itself succeeded.
func TestDiskUpgradeData_E2Any_FinalUnwindFailureIsInterruptedCleanupFailed(t *testing.T) {
	h := newUpgradeHarness(t)
	h.register()
	h.stopArray()
	h.setArrayReady(nil, nil, nil)
	h.mounts.UnmountErr["/mnt/disk2"] = errors.New("exit status 32: umount: /mnt/disk2: target is busy")
	// The first Unwind finds nothing mounted and so never unmounts disk2;
	// only the final Unwind meets the busy disk.

	j := h.submit()
	done := h.await(j.ID)
	if done.Status != StatusInterrupted || done.ErrorCode != codeDiskUpgradeCleanupFailed {
		t.Fatalf("status = %s/%s, want interrupted/%s", done.Status, done.ErrorCode, codeDiskUpgradeCleanupFailed)
	}
	if !strings.Contains(done.ErrorMessage, "/mnt/disk2") {
		t.Fatalf("message %q does not name the path still mounted", done.ErrorMessage)
	}
	if cp := h.checkpoint(j.ID); cp.Phase != disk.DataDiskUpgradePhaseReleasing {
		t.Fatalf("checkpoint = %+v, want releasing (the last one saved)", cp)
	}
	if err := h.seq.Start(h.ctx); !errors.Is(err, ErrDiskUpgradeDataPending) {
		t.Fatalf("array start after a failed Unwind = %v, want disk_upgrade_pending", err)
	}
}

// Unwind order: services, then pool mounts, then disks, then G; a service
// that will not stop keeps every mount up (never unmounts a disk under a
// live service).
func TestDiskUpgradeData_Unwind_ServiceThatWillNotStopKeepsDisksMounted(t *testing.T) {
	h := newUpgradeHarness(t)
	h.mounts.Preload("/mnt/user", "fuse")
	h.mounts.Preload(h.oldWhere, upgradeOldUUID)
	h.samba.setStopErr(errors.New("smbd holds files open"))

	err := h.deps.unwind(h.ctx, h.oldWhere)
	var oe *OutcomeError
	if !errors.As(err, &oe) || oe.Code != codeDiskUpgradeCleanupFailed || oe.Status != StatusInterrupted {
		t.Fatalf("unwind with a stuck service = %v, want an interrupted disk_upgrade_cleanup_failed outcome", err)
	}
	if len(h.mounts.Ops()) != 0 {
		t.Fatalf("unwind unmounted %v under a service that would not stop", h.mounts.Ops())
	}
	if !strings.Contains(err.Error(), "/mnt/user") || !strings.Contains(err.Error(), h.oldWhere) {
		t.Fatalf("error %q does not name the paths still mounted", err)
	}

	h.samba.setStopErr(nil)
	if err := h.deps.unwind(h.ctx, h.oldWhere); err != nil {
		t.Fatalf("unwind: %v", err)
	}
	ops := h.mounts.Ops()
	if strings.Join(ops, ",") != "umount /mnt/user,umount "+h.oldWhere {
		t.Fatalf("unmount order = %v, want the pool before the disk", ops)
	}
	h.assertStopped()
}

// UR5: a stacked mount is unmounted layer by layer until the table no
// longer lists the path; an unreadable mount table is a failure, never
// "unmounted".
func TestDiskUpgradeData_UR5_UnmountsStackedMountsAndFailsClosedOnUnreadableTable(t *testing.T) {
	h := newUpgradeHarness(t)
	h.mounts.Preload(h.oldWhere, upgradeOldUUID)
	h.mounts.Preload(h.oldWhere, upgradeNewUUID)
	if err := h.deps.unwind(h.ctx, h.oldWhere); err != nil {
		t.Fatalf("unwind: %v", err)
	}
	h.assertStopped()

	h.mounts.Preload(h.oldWhere, upgradeOldUUID)
	h.mounts.TableErr = errors.New("reading /proc/self/mountinfo: permission denied")
	if err := h.deps.unwind(h.ctx, h.oldWhere); err == nil {
		t.Fatal("unwind with an unreadable mount table = nil, want cleanup failed")
	}
}

// UR5 with the production mount-table reader: G does not exist (the run
// stopped before Formatting created it) and umount would exit 32 for it.
// Unwind never calls umount for it and succeeds. Edge case "G does not
// exist at recovery or abort".
func TestDiskUpgradeData_UR5_MissingStagingWithRealExit32IsUnmounted(t *testing.T) {
	h := newUpgradeHarness(t)
	missing := filepath.Join(t.TempDir(), "never-created", "mnt", "disk1")
	h.deps.StagingPath = func(string) string { return missing }
	mountinfo := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(mountinfo, []byte("22 1 8:2 / / rw - ext4 /dev/sda2 rw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := disk.NewFakeRunner()
	r.Script("umount", []string{missing}, nil, errors.New("exit status 32: umount: "+missing+": no mount point specified."))
	h.deps.Mounts = disk.KernelMounts{Runner: r, MountInfo: mountinfo}

	if err := h.deps.unwind(h.ctx, h.oldWhere); err != nil {
		t.Fatalf("unwind with a missing staging path: %v", err)
	}
	for _, c := range r.Calls() {
		if c.Name == "umount" {
			t.Fatalf("unwind ran umount %v for a path the mount table does not list", c.Args)
		}
	}
}

// E3 Queued at none: accepted, removed from the queue, Unwind, cancelled.
// E3 Queued at releasing: refused, stays queued, reported not cancellable.
func TestDiskUpgradeData_E3_QueuedCancel(t *testing.T) {
	h := newUpgradeHarness(t)
	h.register()
	finishBlocker := h.runBlockingTopologyJob()
	if err := h.s.EnterMaintenance(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.s.MarkArrayStopped()

	atNone := h.seedInterrupted(nil)
	if resumed, err := h.s.Resume(h.ctx, atNone.ID); err != nil || resumed.Status != StatusQueued {
		t.Fatalf("Resume behind a running topology job = %v, %v; want queued", resumed, err)
	}
	h.mounts.Preload(h.oldWhere, upgradeOldUUID)
	cancelled, err := h.s.Cancel(h.ctx, atNone.ID)
	if err != nil || cancelled.Status != StatusCancelled {
		t.Fatalf("Cancel(queued at none) = %v, %v; want cancelled", cancelled, err)
	}
	h.assertStopped()

	atReleasing := h.seedInterrupted(&disk.DataDiskUpgradeCheckpoint{Phase: disk.DataDiskUpgradePhaseReleasing, NewUUID: upgradeNewUUID})
	resumed, err := h.s.Resume(h.ctx, atReleasing.ID)
	if err != nil || resumed.Status != StatusQueued {
		t.Fatalf("Resume(at releasing) behind a running topology job = %v, %v; want queued", resumed, err)
	}
	if resumed.Cancellable {
		t.Fatal("a job queued at releasing is reported cancellable (UR7)")
	}
	stored, _ := h.s.store.Get(h.ctx, atReleasing.ID)
	if stored.Cancellable {
		t.Fatal("the store reports a job queued at releasing as cancellable (UR7)")
	}
	if _, err := h.s.Cancel(h.ctx, atReleasing.ID); !errors.Is(err, ErrDiskUpgradePastRelease) {
		t.Fatalf("Cancel(queued at releasing) = %v, want job_not_cancellable", err)
	}
	if got, _ := h.s.store.Get(h.ctx, atReleasing.ID); got.Status != StatusQueued {
		t.Fatalf("status after the refused cancel = %s, want still queued", got.Status)
	}
	finishBlocker()
	if done := h.await(atReleasing.ID); done.Status != StatusSucceeded {
		t.Fatalf("the job queued at releasing = %s (%s), want succeeded once it ran", done.Status, done.ErrorMessage)
	}
}

// E3 Queued, Unwind fails: the job is left interrupted with
// disk_upgrade_cleanup_failed and the cancel reports it.
func TestDiskUpgradeData_E3_QueuedCancelWithFailedUnwindStaysInterrupted(t *testing.T) {
	h := newUpgradeHarness(t)
	h.register()
	finishBlocker := h.runBlockingTopologyJob()
	defer finishBlocker()
	if err := h.s.EnterMaintenance(h.ctx); err != nil {
		t.Fatal(err)
	}
	j := h.seedInterrupted(nil)
	if _, err := h.s.Resume(h.ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	h.mounts.Preload(h.oldWhere, upgradeOldUUID)
	h.mounts.UnmountErr[h.oldWhere] = errors.New("target is busy")

	if _, err := h.s.Cancel(h.ctx, j.ID); !errors.Is(err, ErrDiskUpgradeCleanupFailed) {
		t.Fatalf("Cancel = %v, want disk_upgrade_cleanup_failed", err)
	}
	got, _ := h.s.store.Get(h.ctx, j.ID)
	if got.Status != StatusInterrupted || got.ErrorCode != codeDiskUpgradeCleanupFailed {
		t.Fatalf("job = %s/%s, want interrupted/%s", got.Status, got.ErrorCode, codeDiskUpgradeCleanupFailed)
	}
}

// E3 None to Diffing, running: the cancel is accepted with the running
// job, the run returns, Unwind runs, then cancelled with SQLite naming A.
func TestDiskUpgradeData_E3_RunningCancelUnwindsThenCancelled(t *testing.T) {
	h := newUpgradeHarness(t)
	entered, release := h.engine.holdCall(2)
	h.register()
	h.stopArray()

	j := h.submit()
	<-entered
	resp, err := h.s.Cancel(h.ctx, j.ID)
	if err != nil || resp.Status != StatusRunning {
		t.Fatalf("Cancel(running at diffing) = %v, %v; want the running job", resp, err)
	}
	close(release)
	done := h.await(j.ID)
	if done.Status != StatusCancelled {
		t.Fatalf("status = %s (%s), want cancelled", done.Status, done.ErrorMessage)
	}
	if h.slotUUID() != upgradeOldUUID || h.readyCalls() != 0 {
		t.Fatal("Release ran for a cancelled upgrade")
	}
	h.assertStopped()
	// After cancelled: array start is allowed and mounts A.
	if err := h.seq.Start(h.ctx); err != nil {
		t.Fatalf("array start after cancel: %v", err)
	}
	if top, _ := h.mounts.Top(h.oldWhere); top != upgradeOldUUID {
		t.Fatalf("array start mounted %q at S, want A", top)
	}
}

// E3 running cancel with a failing unmount: the job is not recorded
// cancelled; it is interrupted with disk_upgrade_cleanup_failed, still
// pending, so array start stays refused. Edge case "An unmount fails
// during a running cancel".
func TestDiskUpgradeData_E3_RunningCancelWithFailedUnwindIsInterrupted(t *testing.T) {
	h := newUpgradeHarness(t)
	entered, release := h.engine.holdCall(2)
	h.register()
	h.stopArray()

	j := h.submit()
	<-entered
	h.mounts.UnmountErr[h.oldWhere] = errors.New("exit status 32: umount: target is busy")
	if _, err := h.s.Cancel(h.ctx, j.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	close(release)
	done := h.await(j.ID)
	if done.Status != StatusInterrupted || done.ErrorCode != codeDiskUpgradeCleanupFailed {
		t.Fatalf("status = %s/%s, want interrupted/%s", done.Status, done.ErrorCode, codeDiskUpgradeCleanupFailed)
	}
	if err := h.seq.Start(h.ctx); !errors.Is(err, ErrDiskUpgradeDataPending) {
		t.Fatalf("array start = %v, want disk_upgrade_pending", err)
	}
	// The cancel can be retried once the unmount succeeds.
	delete(h.mounts.UnmountErr, h.oldWhere)
	if got, err := h.s.Cancel(h.ctx, j.ID); err != nil || got.Status != StatusCancelled {
		t.Fatalf("retried Cancel = %v, %v; want cancelled", got, err)
	}
	h.assertStopped()
}

// E3 Diffing race, cancel first: the save of releasing is refused, Release
// never runs, and the job ends cancelled with SQLite naming A.
func TestDiskUpgradeData_E3_DiffingRaceCancelBeforeSaveNeverReleases(t *testing.T) {
	h := newUpgradeHarness(t)
	entered, release := h.engine.holdCall(2)
	h.register()
	h.stopArray()

	j := h.submit()
	<-entered
	// The diff is clean and returns after the cancel: the next step is the
	// save of releasing, which must lose to the cancel.
	if _, err := h.s.Cancel(h.ctx, j.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	close(release)
	done := h.await(j.ID)
	if done.Status != StatusCancelled {
		t.Fatalf("status = %s (%s), want cancelled", done.Status, done.ErrorMessage)
	}
	if cp := h.checkpoint(j.ID); cp.Phase != disk.DataDiskUpgradePhaseDiffing {
		t.Fatalf("checkpoint = %+v, want diffing (releasing never saved)", cp)
	}
	if h.slotUUID() != upgradeOldUUID || h.readyCalls() != 0 {
		t.Fatal("Release ran although the cancel came first")
	}
}

// E3 Diffing race, save first, and E3 Releasing running: once releasing is
// saved every cancel is refused and the job finishes succeeded.
func TestDiskUpgradeData_E3_ReleasingRunningRefusesCancel(t *testing.T) {
	h := newUpgradeHarness(t)
	in := make(chan struct{})
	block := make(chan struct{})
	h.setArrayReady(nil, in, block)
	h.register()
	h.stopArray()

	j := h.submit()
	<-in
	if _, err := h.s.Cancel(h.ctx, j.ID); !errors.Is(err, ErrDiskUpgradePastRelease) {
		t.Fatalf("Cancel(running at releasing) = %v, want job_not_cancellable", err)
	}
	close(block)
	done := h.await(j.ID)
	if done.Status != StatusSucceeded || h.slotUUID() != upgradeNewUUID {
		t.Fatalf("status = %s, SQLite = %s; want succeeded naming B", done.Status, h.slotUUID())
	}
}

// E3 Releasing, resumed: a job resumed at releasing is not cancellable
// from the moment it is visible (E5, UR7). Edge case "Cancel at releasing
// after a resume".
func TestDiskUpgradeData_E3_ResumedAtReleasingIsNeverCancellable(t *testing.T) {
	h := newUpgradeHarness(t)
	in := make(chan struct{})
	block := make(chan struct{})
	h.setArrayReady(nil, in, block)
	h.register()
	if err := h.s.EnterMaintenance(h.ctx); err != nil {
		t.Fatal(err)
	}
	j := h.seedInterrupted(&disk.DataDiskUpgradeCheckpoint{Phase: disk.DataDiskUpgradePhaseReleasing, NewUUID: upgradeNewUUID})

	resumed, err := h.s.Resume(h.ctx, j.ID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.Cancellable {
		t.Fatal("Resume at releasing returned a cancellable job")
	}
	if _, err := h.s.Cancel(h.ctx, j.ID); !errors.Is(err, ErrDiskUpgradePastRelease) {
		t.Fatalf("Cancel right after Resume at releasing = %v, want job_not_cancellable", err)
	}
	<-in
	if _, err := h.s.Cancel(h.ctx, j.ID); !errors.Is(err, ErrDiskUpgradePastRelease) {
		t.Fatalf("Cancel inside the resumed Release = %v, want job_not_cancellable", err)
	}
	close(block)
	if done := h.await(j.ID); done.Status != StatusSucceeded {
		t.Fatalf("status = %s, want succeeded", done.Status)
	}
}

// E3 Interrupted at none to diffing: Unwind, then cancelled; SQLite names
// A and array start is allowed. Edge case "Restart ... before the first
// checkpoint" (E3 part).
func TestDiskUpgradeData_E3_InterruptedCancelUnwindsThenCancelled(t *testing.T) {
	for _, cp := range []*disk.DataDiskUpgradeCheckpoint{
		nil,
		{Phase: disk.DataDiskUpgradePhaseCopying, LastPath: "a", NewUUID: upgradeNewUUID},
		{Phase: disk.DataDiskUpgradePhaseVerifying, NewUUID: upgradeNewUUID},
		{Phase: disk.DataDiskUpgradePhaseRemounting, NewUUID: upgradeNewUUID},
		{Phase: disk.DataDiskUpgradePhaseDiffing, NewUUID: upgradeNewUUID},
	} {
		name := "none"
		if cp != nil {
			name = string(cp.Phase)
		}
		t.Run(name, func(t *testing.T) {
			h := newUpgradeHarness(t)
			h.register()
			if err := RecoverDiskUpgradeData(h.ctx, h.s, h.deps); err != nil {
				t.Fatal(err)
			}
			j := h.seedInterrupted(cp)
			if err := RecoverDiskUpgradeData(h.ctx, h.s, h.deps); err != nil {
				t.Fatal(err)
			}
			h.mounts.Preload(h.oldWhere, upgradeNewUUID)
			h.mounts.Preload("/mnt/disk2", "uuid-d2")

			got, err := h.s.Cancel(h.ctx, j.ID)
			if err != nil || got.Status != StatusCancelled {
				t.Fatalf("Cancel = %v, %v; want cancelled", got, err)
			}
			h.assertStopped()
			if h.slotUUID() != upgradeOldUUID {
				t.Fatal("SQLite no longer names A")
			}
			if err := h.seq.Start(h.ctx); err != nil {
				t.Fatalf("array start after the abort: %v", err)
			}
		})
	}
}

// E3 Interrupted, Unwind fails: 409 disk_upgrade_cleanup_failed and the job
// stays interrupted. Edge case "An unmount fails during a cancel of an
// interrupted job".
func TestDiskUpgradeData_E3_InterruptedCancelWithFailedUnwindStaysInterrupted(t *testing.T) {
	h := newUpgradeHarness(t)
	h.register()
	if err := h.s.EnterMaintenance(h.ctx); err != nil {
		t.Fatal(err)
	}
	j := h.seedInterrupted(&disk.DataDiskUpgradeCheckpoint{Phase: disk.DataDiskUpgradePhaseDiffing, NewUUID: upgradeNewUUID})
	h.mounts.Preload(h.oldWhere, upgradeNewUUID)
	h.mounts.UnmountErr[h.oldWhere] = errors.New("target is busy")

	if _, err := h.s.Cancel(h.ctx, j.ID); !errors.Is(err, ErrDiskUpgradeCleanupFailed) {
		t.Fatalf("Cancel = %v, want disk_upgrade_cleanup_failed", err)
	}
	got, _ := h.s.store.Get(h.ctx, j.ID)
	if got.Status != StatusInterrupted || got.ErrorCode != codeDiskUpgradeCleanupFailed {
		t.Fatalf("job = %s/%s, want interrupted/%s", got.Status, got.ErrorCode, codeDiskUpgradeCleanupFailed)
	}
	if err := h.seq.Start(h.ctx); !errors.Is(err, ErrDiskUpgradeDataPending) {
		t.Fatalf("array start = %v, want disk_upgrade_pending", err)
	}
}

// UR7: an abort and a resume of the same job never run together.
func TestDiskUpgradeData_UR7_ResumeRefusedWhileAbortRuns(t *testing.T) {
	h := newUpgradeHarness(t)
	h.register()
	if err := h.s.EnterMaintenance(h.ctx); err != nil {
		t.Fatal(err)
	}
	j := h.seedInterrupted(nil)
	// The abort's Unwind blocks in the first service stop.
	block := make(chan struct{})
	h.samba.mu.Lock()
	h.samba.block = block
	h.samba.mu.Unlock()

	cancelDone := make(chan error, 1)
	go func() {
		_, err := h.s.Cancel(h.ctx, j.ID)
		cancelDone <- err
	}()
	waitFor(t, 2*time.Second, func() bool {
		h.s.mu.Lock()
		defer h.s.mu.Unlock()
		return h.s.aborting[j.ID]
	})
	if _, err := h.s.Resume(h.ctx, j.ID); !errors.Is(err, ErrJobAbortInProgress) {
		t.Fatalf("Resume during the abort = %v, want ErrJobAbortInProgress", err)
	}
	if _, err := h.s.Cancel(h.ctx, j.ID); !errors.Is(err, ErrJobAbortInProgress) {
		t.Fatalf("second Cancel during the abort = %v, want ErrJobAbortInProgress", err)
	}
	h.samba.mu.Lock()
	h.samba.block = nil
	h.samba.mu.Unlock()
	close(block)
	if err := <-cancelDone; err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if _, err := h.s.Resume(h.ctx, j.ID); !errors.Is(err, ErrJobNotInterrupted) {
		t.Fatalf("Resume after the abort = %v, want job_not_interrupted", err)
	}
}

// E3 Done or Ended: refused with job_not_running.
func TestDiskUpgradeData_E3_EndedJobCancelRefused(t *testing.T) {
	h := newUpgradeHarness(t)
	h.register()
	h.stopArray()
	done := h.await(h.submit().ID)
	if done.Status != StatusSucceeded {
		t.Fatalf("status = %s", done.Status)
	}
	if _, err := h.s.Cancel(h.ctx, done.ID); !errors.Is(err, ErrJobNotRunning) {
		t.Fatalf("Cancel(succeeded) = %v, want job_not_running", err)
	}
}

// E4 and UR1, UR2, UR8: after a restart, RecoverFromRestart marks the
// running upgrade interrupted and startup recovery enters maintenance mode
// and unwinds whatever the killed run or boot left mounted; the readiness
// gate reports not ready and array start is refused.
func TestDiskUpgradeData_E4_StartupRecoveryUnwindsAndHoldsTheArrayStopped(t *testing.T) {
	for _, cp := range []*disk.DataDiskUpgradeCheckpoint{
		nil,
		{Phase: disk.DataDiskUpgradePhaseCopying, LastPath: "a", NewUUID: upgradeNewUUID},
		{Phase: disk.DataDiskUpgradePhaseVerifying, NewUUID: upgradeNewUUID},
		{Phase: disk.DataDiskUpgradePhaseRemounting, NewUUID: upgradeNewUUID},
		{Phase: disk.DataDiskUpgradePhaseDiffing, NewUUID: upgradeNewUUID},
		{Phase: disk.DataDiskUpgradePhaseReleasing, NewUUID: upgradeNewUUID},
	} {
		name := "none"
		if cp != nil {
			name = string(cp.Phase)
		}
		t.Run(name, func(t *testing.T) {
			h := newUpgradeHarness(t)
			h.register()
			j := h.seedInterrupted(cp)
			// The killed run left the job running in the store, and the
			// mounts of its state behind; a reboot could add A at S.
			if err := h.s.store.UpdateStatus(h.ctx, j.ID, StatusRunning, nil, "", "", j.StartedAt, nil); err != nil {
				t.Fatal(err)
			}
			h.mounts.Preload("/mnt/parity1", "uuid-p")
			h.mounts.Preload(h.oldWhere, upgradeOldUUID)
			h.mounts.Preload(h.staging, upgradeNewUUID)
			h.mounts.Preload("/mnt/user", "fuse")

			h.s.registry.Register(TypeMover, true, func(context.Context, *RunContext) error { return nil })
			if err := h.s.RecoverFromRestart(h.ctx); err != nil {
				t.Fatal(err)
			}
			if err := RecoverDiskUpgradeData(h.ctx, h.s, h.deps); err != nil {
				t.Fatalf("startup recovery: %v", err)
			}
			if !h.s.InMaintenance() {
				t.Fatal("startup recovery left maintenance mode off with an upgrade pending (UR1)")
			}
			h.assertStopped()
			got, _ := h.s.store.Get(h.ctx, j.ID)
			if got.Status != StatusInterrupted {
				t.Fatalf("status = %s, want interrupted", got.Status)
			}
			gate := PendingUpgradeGate{Gate: readyGate{}, Scheduler: h.s}
			if gate.Ready() {
				t.Fatal("the readiness gate reports ready with an upgrade pending (UR2)")
			}
			if err := h.seq.Start(h.ctx); !errors.Is(err, ErrDiskUpgradeDataPending) {
				t.Fatalf("array start = %v, want disk_upgrade_pending (E6)", err)
			}
			if _, err := h.s.Submit(h.ctx, TypeMover, nil, nil); !errors.Is(err, ErrMaintenanceMode) {
				t.Fatalf("Submit(mover) after recovery = %v, want maintenance_mode", err)
			}
		})
	}
}

type readyGate struct{}

func (readyGate) Ready() bool { return true }

// E4 with G never created: startup recovery succeeds against the
// production mount-table reader, so the daemon starts. Edge case
// "Restart ... before the first checkpoint".
func TestDiskUpgradeData_E4_RecoveryWithMissingStagingSucceeds(t *testing.T) {
	h := newUpgradeHarness(t)
	missing := filepath.Join(t.TempDir(), "never-created")
	h.deps.StagingPath = func(string) string { return missing }
	mountinfo := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(mountinfo, []byte("22 1 8:2 / / rw - ext4 /dev/sda2 rw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := disk.NewFakeRunner()
	r.Script("umount", []string{missing}, nil, errors.New("exit status 32: umount: "+missing+": no mount point specified."))
	h.deps.Mounts = disk.KernelMounts{Runner: r, MountInfo: mountinfo}
	h.register()
	j := h.seedInterrupted(nil)

	if err := RecoverDiskUpgradeData(h.ctx, h.s, h.deps); err != nil {
		t.Fatalf("startup recovery: %v", err)
	}
	got, _ := h.s.store.Get(h.ctx, j.ID)
	if got.ErrorCode == codeDiskUpgradeCleanupFailed {
		t.Fatalf("startup recovery recorded a cleanup failure for a path that never existed: %s", got.ErrorMessage)
	}
	// And the abort of the same job succeeds too (E3 Interrupted at none).
	if c, err := h.s.Cancel(h.ctx, j.ID); err != nil || c.Status != StatusCancelled {
		t.Fatalf("Cancel = %v, %v; want cancelled", c, err)
	}
}

// E4 and UR8: a startup recovery that cannot unmount is recorded on the job
// and does not stop the daemon; resume and cancel stay reachable.
func TestDiskUpgradeData_UR8_FailedStartupRecoveryIsRecordedNotFatal(t *testing.T) {
	h := newUpgradeHarness(t)
	h.register()
	j := h.seedInterrupted(&disk.DataDiskUpgradeCheckpoint{Phase: disk.DataDiskUpgradePhaseDiffing, NewUUID: upgradeNewUUID})
	h.mounts.Preload(h.oldWhere, upgradeNewUUID)
	h.mounts.UnmountErr[h.oldWhere] = errors.New("target is busy")

	if err := RecoverDiskUpgradeData(h.ctx, h.s, h.deps); err != nil {
		t.Fatalf("startup recovery returned %v, want nil — the failure belongs on the job", err)
	}
	got, _ := h.s.store.Get(h.ctx, j.ID)
	if got.Status != StatusInterrupted || got.ErrorCode != codeDiskUpgradeCleanupFailed || !strings.Contains(got.ErrorMessage, h.oldWhere) {
		t.Fatalf("job = %s/%s %q, want interrupted/%s naming %s", got.Status, got.ErrorCode, got.ErrorMessage, codeDiskUpgradeCleanupFailed, h.oldWhere)
	}
	if !h.s.InMaintenance() {
		t.Fatal("maintenance mode is off after a failed recovery")
	}
	// Resume reaches the job; its first Unwind fails the same way and it
	// ends interrupted at the same checkpoint (E5).
	if _, err := h.s.Resume(h.ctx, j.ID); err != nil {
		t.Fatalf("Resume after a failed recovery: %v", err)
	}
	done := h.await(j.ID)
	if done.Status != StatusInterrupted || done.ErrorCode != codeDiskUpgradeCleanupFailed {
		t.Fatalf("resume = %s/%s, want interrupted/%s", done.Status, done.ErrorCode, codeDiskUpgradeCleanupFailed)
	}
	if cp := h.checkpoint(j.ID); cp.Phase != disk.DataDiskUpgradePhaseDiffing {
		t.Fatalf("checkpoint = %+v, want diffing", cp)
	}
}

// E4 Done or Ended: startup recovery does nothing.
func TestDiskUpgradeData_E4_NothingPendingNothingDone(t *testing.T) {
	h := newUpgradeHarness(t)
	h.register()
	h.mounts.Preload(h.oldWhere, upgradeOldUUID)
	if err := RecoverDiskUpgradeData(h.ctx, h.s, h.deps); err != nil {
		t.Fatal(err)
	}
	if h.s.InMaintenance() || len(h.mounts.Ops()) != 0 {
		t.Fatal("startup recovery acted with no upgrade pending")
	}
}

// E4 clean shutdown: the shared stop sequence, run while an upgrade is
// running in maintenance mode, asks the upgrade to stop at its next
// checkpoint and proceeds once it has. The job ends interrupted and
// unwound.
func TestDiskUpgradeData_E4_ShutdownStopSequenceStopsARunningUpgrade(t *testing.T) {
	h := newUpgradeHarness(t)
	entered, release := h.engine.holdCall(1)
	h.register()
	h.stopArray()

	j := h.submit()
	<-entered
	stopped := make(chan error, 1)
	go func() { stopped <- h.seq.Stop(h.ctx) }()
	waitFor(t, 2*time.Second, func() bool {
		h.s.mu.Lock()
		defer h.s.mu.Unlock()
		rj, ok := h.s.running[j.ID]
		if !ok {
			return false
		}
		rj.mu.Lock()
		defer rj.mu.Unlock()
		return rj.stopSignalled
	})
	close(release)
	if err := <-stopped; err != nil {
		t.Fatalf("stop sequence: %v", err)
	}
	done := h.await(j.ID)
	if done.Status != StatusInterrupted {
		t.Fatalf("status = %s (%s), want interrupted", done.Status, done.ErrorMessage)
	}
	if _, formatted := h.provider.FormattedAs("/dev/sdz"); formatted {
		t.Fatal("B was formatted after the stop was requested")
	}
	h.assertStopped()
}

// E5: Resume of a queued or running upgrade is refused.
func TestDiskUpgradeData_E5_ResumeOfRunningRefused(t *testing.T) {
	h := newUpgradeHarness(t)
	entered, release := h.engine.holdCall(1)
	h.register()
	h.stopArray()
	j := h.submit()
	<-entered
	if _, err := h.s.Resume(h.ctx, j.ID); !errors.Is(err, ErrJobNotInterrupted) {
		t.Fatalf("Resume(running) = %v, want job_not_interrupted", err)
	}
	close(release)
	if done := h.await(j.ID); done.Status != StatusSucceeded {
		t.Fatalf("status = %s", done.Status)
	}
	if _, err := h.s.Resume(h.ctx, j.ID); !errors.Is(err, ErrJobNotInterrupted) {
		t.Fatalf("Resume(succeeded) = %v, want job_not_interrupted", err)
	}
}

// E5 Interrupted at none: Unwind, then E1's None row again — the diff gate
// runs again and B is formatted again.
func TestDiskUpgradeData_E5_ResumeAtNoneRerunsTheDiffGate(t *testing.T) {
	h := newUpgradeHarness(t)
	h.register()
	if err := h.s.EnterMaintenance(h.ctx); err != nil {
		t.Fatal(err)
	}
	j := h.seedInterrupted(nil)
	h.mounts.Preload(h.staging, "uuid-half-formatted")
	if _, err := h.s.Resume(h.ctx, j.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if done := h.await(j.ID); done.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s)", done.Status, done.ErrorMessage)
	}
	if h.engine.diffCalls() != 2 {
		t.Fatalf("diff calls = %d, want 2", h.engine.diffCalls())
	}
	if _, formatted := h.provider.FormattedAs("/dev/sdz"); !formatted {
		t.Fatal("B was not formatted again")
	}
	h.assertStopped()
}

// E5 Interrupted at copying: Unwind, then Establish(Copy) with B's UUID
// from the checkpoint (UR4: B is never re-read), and the copy continues
// after the last saved path.
func TestDiskUpgradeData_E5_ResumeAtCopyingContinuesAfterLastPath(t *testing.T) {
	h := newUpgradeHarness(t)
	// The earlier run copied "a" and "a/one.bin".
	writeUpgradeTree(t, h.staging)
	if err := os.Remove(filepath.Join(h.staging, "b.txt")); err != nil {
		t.Fatal(err)
	}
	before := ctime(t, filepath.Join(h.staging, "a", "one.bin"))
	h.runner = disk.NewFakeRunner()
	h.deps.Runner = h.runner
	h.register()
	if err := h.s.EnterMaintenance(h.ctx); err != nil {
		t.Fatal(err)
	}
	j := h.seedInterrupted(&disk.DataDiskUpgradeCheckpoint{Phase: disk.DataDiskUpgradePhaseCopying, LastPath: "a/one.bin", NewUUID: upgradeNewUUID})
	// Boot left A at S and nothing at G.
	h.mounts.Preload(h.oldWhere, upgradeOldUUID)

	if _, err := h.s.Resume(h.ctx, j.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if done := h.await(j.ID); done.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s)", done.Status, done.ErrorMessage)
	}
	if h.mountCount(h.staging, upgradeNewUUID) != 1 {
		t.Fatalf("G was not mounted with the checkpoint's UUID: %v", h.mounts.Ops())
	}
	for _, c := range h.runner.Calls() {
		if c.Name == "blkid" {
			t.Fatalf("resume re-read a device: blkid %v (UR4)", c.Args)
		}
	}
	if after := ctime(t, filepath.Join(h.staging, "a", "one.bin")); !after.Equal(before) {
		t.Fatal("a/one.bin, at or before the last saved path, was copied again")
	}
	if _, err := os.Stat(filepath.Join(h.staging, "b.txt")); err != nil {
		t.Fatalf("b.txt was not copied on resume: %v", err)
	}
}

func ctime(t *testing.T, path string) time.Time {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	return time.Unix(int64(st.Ctim.Sec), int64(st.Ctim.Nsec))
}

// E5 Interrupted at verifying: Establish(Copy), verify from the start.
func TestDiskUpgradeData_E5_ResumeAtVerifyingVerifiesAndCompletes(t *testing.T) {
	h := newUpgradeHarness(t)
	writeUpgradeTree(t, h.staging)
	h.register()
	if err := h.s.EnterMaintenance(h.ctx); err != nil {
		t.Fatal(err)
	}
	j := h.seedInterrupted(&disk.DataDiskUpgradeCheckpoint{Phase: disk.DataDiskUpgradePhaseVerifying, NewUUID: upgradeNewUUID})
	if _, err := h.s.Resume(h.ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if done := h.await(j.ID); done.Status != StatusSucceeded || h.slotUUID() != upgradeNewUUID {
		t.Fatalf("status = %s (%s), SQLite %s", done.Status, done.ErrorMessage, h.slotUUID())
	}
	if h.mountCount(h.oldWhere, upgradeOldUUID) != 1 || h.mountCount(h.staging, upgradeNewUUID) != 1 {
		t.Fatalf("Establish(Copy) did not mount A at S and B at G: %v", h.mounts.Ops())
	}
}

// E5 Interrupted at remounting and at diffing: Unwind, then Establish(New),
// which never mounts A; remounting saves diffing. Nothing is copied and A
// is never mounted, even when boot left it at S. Edge cases "A at S on
// resume after a reboot" and "Going back to copying after B may have
// served S".
func TestDiskUpgradeData_E5_ResumeAtRemountingOrDiffingNeverMountsA(t *testing.T) {
	for _, phase := range []disk.DataDiskUpgradePhase{disk.DataDiskUpgradePhaseRemounting, disk.DataDiskUpgradePhaseDiffing} {
		t.Run(string(phase), func(t *testing.T) {
			h := newUpgradeHarness(t)
			h.register()
			if err := h.s.EnterMaintenance(h.ctx); err != nil {
				t.Fatal(err)
			}
			j := h.seedInterrupted(&disk.DataDiskUpgradeCheckpoint{Phase: phase, NewUUID: upgradeNewUUID})
			h.mounts.Preload(h.oldWhere, upgradeOldUUID)
			if _, err := h.s.Resume(h.ctx, j.ID); err != nil {
				t.Fatal(err)
			}
			done := h.await(j.ID)
			if done.Status != StatusSucceeded || h.slotUUID() != upgradeNewUUID {
				t.Fatalf("status = %s (%s), SQLite %s", done.Status, done.ErrorMessage, h.slotUUID())
			}
			if h.mountCount(h.oldWhere, upgradeOldUUID) != 0 {
				t.Fatalf("A was mounted on a resume at %s: %v", phase, h.mounts.Ops())
			}
			if h.mountCount(h.staging, upgradeNewUUID) != 0 {
				t.Fatalf("G was mounted on a resume at %s: %v", phase, h.mounts.Ops())
			}
			if entries := dirEntries(t, h.staging); len(entries) != 0 {
				t.Fatalf("resume at %s copied %v", phase, entries)
			}
			h.assertStopped()
		})
	}
}

// E5, Establish fails on resume: interrupted at the same checkpoint with
// disk_upgrade_mount_unconfirmed; nothing is read or written.
func TestDiskUpgradeData_E5_EstablishFailureIsMountUnconfirmed(t *testing.T) {
	for _, phase := range []disk.DataDiskUpgradePhase{disk.DataDiskUpgradePhaseCopying, disk.DataDiskUpgradePhaseVerifying, disk.DataDiskUpgradePhaseRemounting, disk.DataDiskUpgradePhaseDiffing} {
		t.Run(string(phase), func(t *testing.T) {
			h := newUpgradeHarness(t)
			h.register()
			if err := h.s.EnterMaintenance(h.ctx); err != nil {
				t.Fatal(err)
			}
			h.mounts.ReportUUID[h.oldWhere] = "uuid-someone-else"
			h.mounts.ReportUUID[h.staging] = "uuid-someone-else"
			j := h.seedInterrupted(&disk.DataDiskUpgradeCheckpoint{Phase: phase, NewUUID: upgradeNewUUID})
			if _, err := h.s.Resume(h.ctx, j.ID); err != nil {
				t.Fatal(err)
			}
			done := h.await(j.ID)
			if done.Status != StatusInterrupted || done.ErrorCode != codeDiskUpgradeMountUnconfirmed {
				t.Fatalf("status = %s/%s, want interrupted/%s", done.Status, done.ErrorCode, codeDiskUpgradeMountUnconfirmed)
			}
			if cp := h.checkpoint(j.ID); cp.Phase != phase {
				t.Fatalf("checkpoint = %+v, want %s", cp, phase)
			}
			if entries := dirEntries(t, h.staging); len(entries) != 0 {
				t.Fatalf("staging holds %v", entries)
			}
			if h.engine.diffCalls() != 0 {
				t.Fatal("snapraid diff ran against unconfirmed mounts")
			}
			h.assertStopped()
		})
	}
}

// E5 Interrupted at releasing with B missing: Release still commits, the
// job succeeds, and its log carries the notice that A holds a complete
// copy.
func TestDiskUpgradeData_E5_ResumeAtReleasingWithNewDiskMissing(t *testing.T) {
	h := newUpgradeHarness(t)
	h.provider = disk.NewFakeProvider()
	h.deps.Provider = h.provider
	h.register()
	if err := h.s.EnterMaintenance(h.ctx); err != nil {
		t.Fatal(err)
	}
	j := h.seedInterrupted(&disk.DataDiskUpgradeCheckpoint{Phase: disk.DataDiskUpgradePhaseReleasing, NewUUID: upgradeNewUUID})
	if _, err := h.s.Resume(h.ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if done := h.await(j.ID); done.Status != StatusSucceeded || h.slotUUID() != upgradeNewUUID {
		t.Fatalf("status = %s (%s), SQLite %s", done.Status, done.ErrorMessage, h.slotUUID())
	}
	log := h.jobLog(j.ID)
	if !strings.Contains(log, "was not found at release") || !strings.Contains(log, "still holds a complete copy") {
		t.Fatalf("job log lacks the missing-disk notice:\n%s", log)
	}
}

// E6: array start is refused, with nothing mounted and maintenance mode
// kept, for every pending state; allowed once the upgrade ended.
func TestDiskUpgradeData_E6_ArrayStartRefusedWhilePending(t *testing.T) {
	h := newUpgradeHarness(t)
	entered, release := h.engine.holdCall(1)
	h.register()
	h.stopArray()
	j := h.submit()
	<-entered
	before := len(h.rec.all())
	if err := h.seq.Start(h.ctx); !errors.Is(err, ErrDiskUpgradeDataPending) || !strings.Contains(err.Error(), j.ID) {
		t.Fatalf("array start while running at none = %v, want disk_upgrade_pending naming %s", err, j.ID)
	}
	for _, e := range h.rec.all()[before:] {
		if strings.HasPrefix(e, "mount:") || strings.HasPrefix(e, "start:") {
			t.Fatalf("array start did %s while an upgrade was pending", e)
		}
	}
	if !h.s.InMaintenance() {
		t.Fatal("maintenance mode ended")
	}
	close(release)
	if done := h.await(j.ID); done.Status != StatusSucceeded {
		t.Fatalf("status = %s", done.Status)
	}
	if err := h.seq.Start(h.ctx); err != nil {
		t.Fatalf("array start after Done: %v", err)
	}
}

// UR9: array start confirms by UUID every mounted array disk before the
// pool or any service starts; a mismatch unmounts the disks again, starts
// nothing above them and keeps maintenance mode. Edge case "B left at S
// across a restart, then array start".
func TestDiskUpgradeData_UR9_ArrayStartRefusesAMismatchedDisk(t *testing.T) {
	h := newUpgradeHarness(t)
	h.stopArray()
	// B was left at S and survives: whatever mounts there, B is visible.
	h.mounts.Preload(h.oldWhere, upgradeNewUUID)
	h.mounts.ReportUUID[h.oldWhere] = upgradeNewUUID
	h.seq.DiskCheck = ArrayDiskUUIDCheck{Mounts: h.mounts, Disks: []disk.MountUnit{
		{Where: "/mnt/parity1", UUID: "uuid-p"},
		{Where: h.oldWhere, UUID: upgradeOldUUID},
		{Where: "/mnt/disk2", UUID: "uuid-d2"},
	}}
	before := len(h.rec.all())
	err := h.seq.Start(h.ctx)
	if !errors.Is(err, ErrArrayDiskMismatch) {
		t.Fatalf("array start = %v, want ErrArrayDiskMismatch", err)
	}
	for _, e := range h.rec.all()[before:] {
		if e == "mount:/mnt/user" || e == "mount:/mnt/user/media" || strings.HasPrefix(e, "start:") {
			t.Fatalf("array start did %s after a mismatched disk", e)
		}
	}
	if !h.s.InMaintenance() {
		t.Fatal("maintenance mode ended after a refused start")
	}
	for _, p := range []string{"/mnt/parity1", "/mnt/disk2"} {
		if _, mounted := h.mounts.Top(p); mounted {
			t.Fatalf("%s is still mounted after the refused start", p)
		}
	}
}

// E7: the stop command is refused while an upgrade is pending, without
// signalling the job; tested at the API (internal/api). Here: the shared
// stop sequence itself still runs (E4, shutdown).

// E8: a second data-disk upgrade is refused with disk_upgrade_pending; a
// parity-disk upgrade or any other job with maintenance_mode.
func TestDiskUpgradeData_E8_SecondSubmitRefusedWhilePending(t *testing.T) {
	h := newUpgradeHarness(t)
	h.register()
	h.s.registry.Register(TypeDiskUpgradeParity, true, func(context.Context, *RunContext) error { return nil })
	h.stopArray()
	pending := h.seedInterrupted(nil)

	_, err := h.s.Submit(h.ctx, TypeDiskUpgradeData, nil, h.params())
	if !errors.Is(err, ErrDiskUpgradeDataPending) || !strings.Contains(err.Error(), pending.ID) {
		t.Fatalf("second data-disk upgrade = %v, want disk_upgrade_pending naming %s", err, pending.ID)
	}
	parityParams := mustJSON(t, DiskUpgradeParityParams{Confirmation: "x", Mountpoint: "/mnt/parity1", NewMountpoint: "/mnt/parity2", Disk: disk.AssignedDisk{Device: "/dev/sdz"}})
	if _, err := h.s.Submit(h.ctx, TypeDiskUpgradeParity, nil, parityParams); !errors.Is(err, ErrMaintenanceMode) {
		t.Fatalf("parity-disk upgrade while pending = %v, want maintenance_mode", err)
	}
}

// E8 and UR3: a data-disk upgrade is admitted only once a stop sequence
// has completed — not with the array started, and not after a stop that
// failed partway, even though maintenance mode is on. Edge case "S already
// mounted before the first run, after a stop that failed partway".
func TestDiskUpgradeData_E8_UR3_AdmittedOnlyAfterACompletedStop(t *testing.T) {
	h := newUpgradeHarness(t)
	h.register()

	if _, err := h.s.Submit(h.ctx, TypeDiskUpgradeData, nil, h.params()); !errors.Is(err, ErrArrayNotStopped) {
		t.Fatalf("submit with the array started = %v, want array_not_stopped", err)
	}
	h.samba.setStopErr(errors.New("smbd will not stop"))
	if err := h.seq.Stop(h.ctx); err == nil {
		t.Fatal("stop with a stuck service succeeded")
	}
	if !h.s.InMaintenance() {
		t.Fatal("a failed stop left maintenance mode off")
	}
	if _, err := h.s.Submit(h.ctx, TypeDiskUpgradeData, nil, h.params()); !errors.Is(err, ErrArrayNotStopped) {
		t.Fatalf("submit after a failed stop = %v, want array_not_stopped (UR3)", err)
	}
	h.samba.setStopErr(nil)
	h.stopArray()
	j := h.submit()
	if done := h.await(j.ID); done.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s)", done.Status, done.ErrorMessage)
	}
	// Done, array still stopped: the next upgrade is admitted (E8).
	next, err := h.s.Submit(h.ctx, TypeDiskUpgradeData, nil, h.params())
	if err != nil {
		t.Fatalf("submit after Done with the array stopped = %v, want admitted", err)
	}
	h.await(next.ID)
	// After array start, refused again.
	h2 := newUpgradeHarness(t)
	h2.register()
	h2.stopArray()
	if err := h2.seq.Start(h2.ctx); err != nil {
		t.Fatal(err)
	}
	if err := h2.s.EnterMaintenance(h2.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h2.s.Submit(h2.ctx, TypeDiskUpgradeData, nil, h2.params()); !errors.Is(err, ErrArrayNotStopped) {
		t.Fatalf("submit after start then bare maintenance = %v, want array_not_stopped", err)
	}
}

// UR3: a start that fails before changing anything — the readiness gate
// refusing a degraded array, or the disk check failing and every disk
// unmounting again — leaves the stop completed, so a data-disk upgrade
// is still admitted without another `array stop`. A start that fails with
// a disk it could not unmount again is not a completed stop.
func TestDiskUpgradeData_UR3_FailedStartKeepsTheCompletedStop(t *testing.T) {
	h := newUpgradeHarness(t)
	h.register()
	h.stopArray()

	h.seq.Gate = fakeReadinessGate{ready: false}
	if err := h.seq.Start(h.ctx); !errors.Is(err, ErrStorageNotReady) {
		t.Fatalf("start with an unready gate = %v, want storage_not_ready", err)
	}
	h.seq.Gate = nil
	checkErr := errors.New("disk1 carries the wrong filesystem")
	h.seq.DiskCheck = failingDiskCheck{err: checkErr}
	if err := h.seq.Start(h.ctx); !errors.Is(err, checkErr) {
		t.Fatalf("start with a failing disk check = %v, want %v", err, checkErr)
	}
	j := h.submit()
	if done := h.await(j.ID); done.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s)", done.Status, done.ErrorMessage)
	}

	// A stop that failed partway is not made complete by a refused start.
	h2 := newUpgradeHarness(t)
	h2.register()
	h2.samba.setStopErr(errors.New("smbd will not stop"))
	if err := h2.seq.Stop(h2.ctx); err == nil {
		t.Fatal("stop with a stuck service succeeded")
	}
	h2.seq.Gate = fakeReadinessGate{ready: false}
	if err := h2.seq.Start(h2.ctx); !errors.Is(err, ErrStorageNotReady) {
		t.Fatalf("start with an unready gate = %v, want storage_not_ready", err)
	}
	if _, err := h2.s.Submit(h2.ctx, TypeDiskUpgradeData, nil, h2.params()); !errors.Is(err, ErrArrayNotStopped) {
		t.Fatalf("submit after a failed stop and a refused start = %v, want array_not_stopped", err)
	}
}

type failingDiskCheck struct{ err error }

func (c failingDiskCheck) ConfirmArrayDisks(context.Context) error { return c.err }

// UR4: A's UUID is the one fixed at submit — if SQLite names something
// else for the slot, the run refuses before formatting.
func TestDiskUpgradeData_UR4_SlotNoLongerNamingAFailsBeforeFormatting(t *testing.T) {
	h := newUpgradeHarness(t)
	h.register()
	h.stopArray()
	p := h.paramsValue()
	p.Old.FSUUID = "uuid-from-another-time"
	j, err := h.s.Submit(h.ctx, TypeDiskUpgradeData, nil, mustJSON(t, p))
	if err != nil {
		t.Fatal(err)
	}
	if done := h.await(j.ID); done.Status != StatusFailed {
		t.Fatalf("status = %s, want failed", done.Status)
	}
	if _, formatted := h.provider.FormattedAs("/dev/sdz"); formatted {
		t.Fatal("B was formatted")
	}
}

// E1 None's identity step: B must still be the disk the plan confirmed by
// its by-id identity.
func TestDiskUpgradeData_E1None_DriftedNewDiskFailsBeforeFormatting(t *testing.T) {
	h := newUpgradeHarness(t)
	h.register()
	h.stopArray()
	p := h.paramsValue()
	p.Disk.Serial = "SERIAL-CONFIRMED"
	p.Confirmation = SingleDiskConfirmation(p.Disk)
	j, err := h.s.Submit(h.ctx, TypeDiskUpgradeData, nil, mustJSON(t, p))
	if err != nil {
		t.Fatal(err)
	}
	done := h.await(j.ID)
	if done.Status != StatusFailed || !strings.Contains(done.ErrorMessage, "no longer matches") {
		t.Fatalf("status = %s (%s), want failed on identity drift", done.Status, done.ErrorMessage)
	}
	if _, formatted := h.provider.FormattedAs("/dev/sdz"); formatted {
		t.Fatal("B was formatted")
	}
}

// PendingDiskUpgradeData treats queued, running and interrupted at any
// checkpoint as pending, and nothing else.
func TestDiskUpgradeData_PendingCoversEveryNonEndedStatus(t *testing.T) {
	h := newUpgradeHarness(t)
	if p, err := h.s.PendingDiskUpgradeData(h.ctx); err != nil || p != nil {
		t.Fatalf("pending with none = %v, %v", p, err)
	}
	j := h.seedInterrupted(nil)
	for _, st := range []Status{StatusInterrupted, StatusQueued, StatusRunning} {
		if err := h.s.store.UpdateStatus(h.ctx, j.ID, st, nil, "", "", nil, nil); err != nil {
			t.Fatal(err)
		}
		if p, err := h.s.PendingDiskUpgradeData(h.ctx); err != nil || p == nil || p.ID != j.ID {
			t.Fatalf("pending with a %s upgrade = %v, %v", st, p, err)
		}
	}
	for _, st := range []Status{StatusSucceeded, StatusFailed, StatusCancelled} {
		if err := h.s.store.UpdateStatus(h.ctx, j.ID, st, nil, "", "", nil, nil); err != nil {
			t.Fatal(err)
		}
		if p, err := h.s.PendingDiskUpgradeData(h.ctx); err != nil || p != nil {
			t.Fatalf("pending with a %s upgrade = %v, %v; want none", st, p, err)
		}
	}
}
