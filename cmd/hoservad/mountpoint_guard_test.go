package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"
)

// noMountpointGuard is what every storageTargetSync test that names a
// production /mnt path installs: the real guard would create that
// directory (Q69), which no test may do outside its own temp directory.
func noMountpointGuard(context.Context, string) error { return nil }

// immutableFilesystem is a disk.Runner that records `chattr +i` / `-i` per
// path, and a stand-in for the kernel's own refusal: write fails with EPERM
// on a path whose immutable bit is set, exactly what a write into an
// unmounted, immutable mountpoint returns (doc 02 §1, Q69).
type immutableFilesystem struct {
	mu        sync.Mutex
	immutable map[string]bool
	calls     []disk.RunCall
	chattrErr error

	// catchAll is where newGuardTestSync redirects pool.CatchAllPath.
	catchAll string
}

func newImmutableFilesystem() *immutableFilesystem {
	return &immutableFilesystem{immutable: map[string]bool{}}
}

func (f *immutableFilesystem) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, disk.RunCall{Name: name, Args: append([]string(nil), args...)})
	if name == "chattr" && len(args) == 2 {
		if f.chattrErr != nil {
			return nil, f.chattrErr
		}
		f.immutable[args[1]] = args[0] == "+i"
	}
	return nil, nil
}

func (f *immutableFilesystem) write(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.immutable[path] {
		return &os.PathError{Op: "open", Path: filepath.Join(path, "stray"), Err: syscall.EPERM}
	}
	return os.WriteFile(filepath.Join(path, "stray"), []byte("boot device data"), 0o644)
}

func (f *immutableFilesystem) chattrsOf(flag string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if c.Name == "chattr" && len(c.Args) == 2 && c.Args[0] == flag {
			out = append(out, c.Args[1])
		}
	}
	return out
}

// newGuardTestSync installs the real disk.GuardMountpoint over fs, except
// that the production catch-all path is redirected to fs.catchAll under the
// test's own temp directory: the guard creates its directory, which no test
// may do under /mnt.
func newGuardTestSync(t *testing.T, fs *immutableFilesystem) *storageTargetSync {
	t.Helper()
	s := newTestStorageTargetSync(t)
	s.Runner = fs
	fs.catchAll = filepath.Join(t.TempDir(), "user")
	s.MountpointGuard = func(ctx context.Context, path string) error {
		if path == pool.CatchAllPath {
			path = fs.catchAll
		}
		return disk.GuardMountpoint(ctx, fs, path)
	}
	return s
}

func slotDirs(t *testing.T, names ...string) []string {
	t.Helper()
	root := t.TempDir()
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, filepath.Join(root, n))
	}
	return out
}

func containsAll(got []string, want ...string) bool {
	set := map[string]bool{}
	for _, g := range got {
		set[g] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

func TestNewArrayDiskMounter_GuardsTheSlotBeforeSystemdMountsIt(t *testing.T) {
	fs := newImmutableFilesystem()
	where := slotDirs(t, "disk1")[0]

	err := newArrayDiskMounter(fs, disk.SystemdMounter{Runner: fs}).Mount(context.Background(), disk.MountUnit{Where: where, UUID: "uuid-d1", Filesystem: "xfs"})
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}

	var order []string
	for _, c := range fs.calls {
		switch {
		case c.Name == "chattr":
			order = append(order, "chattr "+c.Args[0])
		case c.Name == "systemctl" && len(c.Args) > 0 && c.Args[0] == "start":
			order = append(order, "start")
		}
	}
	if len(order) != 2 || order[0] != "chattr +i" || order[1] != "start" {
		t.Fatalf("got call order %v, want chattr +i before the mount unit starts", order)
	}
	if !fs.immutable[where] {
		t.Fatalf("%s was not marked immutable", where)
	}
}

func TestStorageTargetSync_Startup_GuardsEveryUnmountedEmptySlotAndNoOtherOne(t *testing.T) {
	fs := newImmutableFilesystem()
	s := newGuardTestSync(t, fs)
	dirs := slotDirs(t, "disk1", "parity1", "cache", "disk2")
	if err := os.MkdirAll(dirs[3], 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirs[3], "stray"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	seq := &job.ArraySequence{Gate: storageTargetTestGate{ready: false}}
	for _, d := range append(dirs, "/proc") {
		seq.Disks = append(seq.Disks, storageTargetTestMount{where: d})
	}

	if err := s.Startup(context.Background(), seq); err != nil {
		t.Fatalf("Startup: %v — a finding on one slot must not fail startup", err)
	}

	got := fs.chattrsOf("+i")
	if !containsAll(got, dirs[0], dirs[1], dirs[2]) {
		t.Fatalf("chattr +i issued for %v, want every empty unmounted slot %v", got, dirs[:3])
	}
	if len(got) != 3 {
		t.Fatalf("chattr +i issued for %v, want only the three empty unmounted slots (never /proc, which is mounted, nor the non-empty %s)", got, dirs[3])
	}
}

func TestStorageTargetSync_Startup_AFailedChattrDoesNotFailStartupOrHideTheOtherSlots(t *testing.T) {
	fs := newImmutableFilesystem()
	fs.chattrErr = errors.New("operation not supported")
	s := newGuardTestSync(t, fs)
	dirs := slotDirs(t, "disk1", "disk2")
	seq := &job.ArraySequence{Gate: storageTargetTestGate{ready: false}}
	for _, d := range dirs {
		seq.Disks = append(seq.Disks, storageTargetTestMount{where: d})
	}

	if err := s.Startup(context.Background(), seq); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	if got := fs.chattrsOf("+i"); len(got) != 2 {
		t.Fatalf("chattr +i attempted for %v, want both slots tried even though the first failed", got)
	}
}

// The data-loss scenario doc 02 §1 / Q69 exist for: a data disk that fails
// to mount at boot (nofail) leaves its slot an ordinary directory on the
// boot device.
func TestStorageTargetSync_Startup_ASlotWhoseDiskFailedToMountRejectsWrites(t *testing.T) {
	fs := newImmutableFilesystem()
	s := newGuardTestSync(t, fs)
	where := slotDirs(t, "disk1")[0]
	seq := &job.ArraySequence{
		Gate:  storageTargetTestGate{ready: true},
		Disks: []job.ArrayMount{storageTargetTestMount{where: where, mountErr: errors.New("device dependency never resolved")}},
	}

	if err := s.Startup(context.Background(), seq); err == nil {
		t.Fatal("Startup with a disk that failed to mount = nil error, want one")
	}

	err := fs.write(where)
	if !errors.Is(err, syscall.EPERM) {
		t.Fatalf("write into the unmounted slot = %v, want EPERM instead of landing on the boot device", err)
	}
	if entries, _ := os.ReadDir(where); len(entries) != 0 {
		t.Fatalf("the unmounted slot holds %d entries after the refused write, want none", len(entries))
	}
}

func TestStorageTargetSync_Startup_WithoutTheGuardTheSameWriteLandsOnTheBootDevice(t *testing.T) {
	fs := newImmutableFilesystem()
	where := slotDirs(t, "disk1")[0]
	if err := os.MkdirAll(where, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := fs.write(where); err != nil {
		t.Fatalf("write into an unguarded slot = %v, want it accepted (the baseline the guard changes)", err)
	}
}

func TestStorageTargetSync_Startup_InMaintenanceStillGuardsTheSlots(t *testing.T) {
	fs := newImmutableFilesystem()
	s := newGuardTestSync(t, fs)
	where := slotDirs(t, "disk1")[0]
	scheduler := newTestScheduler(t)
	if err := scheduler.EnterMaintenance(context.Background()); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}
	seq := &job.ArraySequence{Gate: storageTargetTestGate{ready: true}, Disks: []job.ArrayMount{storageTargetTestMount{where: where}}, Scheduler: scheduler}

	if err := s.Startup(context.Background(), seq); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	if got := fs.chattrsOf("+i"); !containsAll(got, where) {
		t.Fatalf("chattr +i issued for %v, want %s: an array stopped for a disk swap has every slot unmounted", got, where)
	}
}

func TestStorageTargetSync_Update_GuardsSlotsOnEveryRebuildEvenWhenNothingElseChanged(t *testing.T) {
	fs := newImmutableFilesystem()
	s := newGuardTestSync(t, fs)
	dirs := slotDirs(t, "disk1", "disk2")
	seq := &job.ArraySequence{Gate: storageTargetTestGate{ready: false}}
	for _, d := range dirs {
		seq.Disks = append(seq.Disks, storageTargetTestMount{where: d})
	}

	s.Update(context.Background(), seq)
	if got := fs.chattrsOf("+i"); !containsAll(got, dirs...) {
		t.Fatalf("first Update: chattr +i issued for %v, want %v", got, dirs)
	}

	fresh := slotDirs(t, "disk3")[0]
	seq2 := &job.ArraySequence{Gate: storageTargetTestGate{ready: false}, Disks: append(append([]job.ArrayMount(nil), seq.Disks...), storageTargetTestMount{where: fresh})}
	s.Update(context.Background(), seq2)
	if got := fs.chattrsOf("+i"); !containsAll(got, fresh) {
		t.Fatalf("second Update: chattr +i issued for %v, want the added slot %s guarded too", got, fresh)
	}
}

// A bare-metal restore (doc 10 §1) writes disk mount units, snapraid.conf
// and pool mounts from the restored database and mounts nothing itself
// (regenerateArrayFiles); its ArrayReady hook then rebuilds the array
// sequence from that same store and hands it to storageTargetSync.Update.
// The restored slots' mountpoints do not exist yet on the fresh install:
// the guard must create them immutable, before any disk is mounted.
func TestStorageTargetSync_Update_GuardsTheSlotsARestoredArrayDescribes(t *testing.T) {
	ctx := context.Background()
	fs := newImmutableFilesystem()
	s := newGuardTestSync(t, fs)
	dirs := slotDirs(t, "parity1", "disk1", "disk2", "cache")
	arrays, shares := newTestArrayAndShareStore(t)
	err := arrays.PutArray(ctx, store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "20G", CreatedAt: time.Now().UTC()}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", WWN: "wwn-p", Serial: "ser-p", Mountpoint: dirs[0]},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d1", WWN: "wwn-d1", Serial: "ser-d1", Mountpoint: dirs[1]},
		{Role: store.ArrayRoleData, RoleIndex: 2, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "uuid-d2", WWN: "wwn-d2", Serial: "ser-d2", Mountpoint: dirs[2]},
		{Role: store.ArrayRoleCache, RoleIndex: 1, Device: "/dev/sdd", Filesystem: "xfs", FSUUID: "uuid-c", WWN: "wwn-c", Serial: "ser-c", Mountpoint: dirs[3]},
	})
	if err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	// The fresh install has none of the restored disks attached yet, so the
	// gate is not ready and Update mounts nothing.
	seq, err := newArraySequence(ctx, newTestScheduler(t), arrays, shares, disk.NewFakeProvider(), fs, nil)
	if err != nil || seq == nil {
		t.Fatalf("newArraySequence: seq=%v err=%v", seq, err)
	}

	s.Update(ctx, seq)

	wantGuarded := append(append([]string(nil), dirs...), fs.catchAll)
	if got := fs.chattrsOf("+i"); !containsAll(got, wantGuarded...) || len(got) != len(wantGuarded) {
		t.Fatalf("chattr +i issued for %v, want exactly the restored slots and the catch-all %v", got, wantGuarded)
	}
	for _, d := range wantGuarded {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Fatalf("restored slot mountpoint %s was not created: %v", d, err)
		}
		if err := fs.write(d); !errors.Is(err, syscall.EPERM) {
			t.Fatalf("write into restored, unmounted slot %s = %v, want EPERM", d, err)
		}
	}
}

func TestStorageTargetSync_Startup_GuardsTheUnmountedEmptyCatchAllBeforeMountingIt(t *testing.T) {
	fs := newImmutableFilesystem()
	s := newGuardTestSync(t, fs)
	slot := slotDirs(t, "disk1")[0]
	seq := &job.ArraySequence{
		Gate:     storageTargetTestGate{ready: false},
		Disks:    []job.ArrayMount{storageTargetTestMount{where: slot}},
		CatchAll: storageTargetTestMount{where: pool.CatchAllPath},
	}

	if err := s.Startup(context.Background(), seq); err != nil {
		t.Fatalf("Startup: %v", err)
	}

	if got := fs.chattrsOf("+i"); !containsAll(got, slot, fs.catchAll) || len(got) != 2 {
		t.Fatalf("chattr +i issued for %v, want the slot and the catch-all %s", got, fs.catchAll)
	}
}

func TestStorageTargetSync_Startup_NeverGuardsAMountedCatchAll(t *testing.T) {
	fs := newImmutableFilesystem()
	s := newGuardTestSync(t, fs)
	seq := &job.ArraySequence{
		Gate:     storageTargetTestGate{ready: false},
		CatchAll: storageTargetTestMount{where: "/proc"},
	}

	if err := s.Startup(context.Background(), seq); err != nil {
		t.Fatalf("Startup: %v", err)
	}

	if got := fs.chattrsOf("+i"); len(got) != 0 {
		t.Fatalf("chattr +i issued for %v, want none: the catch-all is mounted", got)
	}
}

func TestStorageTargetSync_Startup_AFailedChattrOnTheCatchAllDoesNotFailStartupOrHideTheSlots(t *testing.T) {
	fs := newImmutableFilesystem()
	fs.chattrErr = errors.New("operation not supported")
	s := newGuardTestSync(t, fs)
	slot := slotDirs(t, "disk1")[0]
	seq := &job.ArraySequence{
		Gate:     storageTargetTestGate{ready: false},
		Disks:    []job.ArrayMount{storageTargetTestMount{where: slot}},
		CatchAll: storageTargetTestMount{where: pool.CatchAllPath},
	}

	if err := s.Startup(context.Background(), seq); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	if got := fs.chattrsOf("+i"); !containsAll(got, slot, fs.catchAll) {
		t.Fatalf("chattr +i attempted for %v, want the slot and the catch-all both tried", got)
	}
}

// The stopped-array data-loss scenario (doc 02 §1, Q69): the pool is
// unmounted for a disk swap and a container or cron job outside Hoserva's
// gated services writes under /mnt/user.
func TestStorageTargetSync_Startup_AnUnmountedCatchAllRejectsWrites(t *testing.T) {
	fs := newImmutableFilesystem()
	s := newGuardTestSync(t, fs)
	scheduler := newTestScheduler(t)
	if err := scheduler.EnterMaintenance(context.Background()); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}
	seq := &job.ArraySequence{
		Gate:      storageTargetTestGate{ready: true},
		CatchAll:  storageTargetTestMount{where: pool.CatchAllPath},
		Scheduler: scheduler,
	}

	if err := s.Startup(context.Background(), seq); err != nil {
		t.Fatalf("Startup: %v", err)
	}

	err := fs.write(fs.catchAll)
	if !errors.Is(err, syscall.EPERM) {
		t.Fatalf("write into the unmounted catch-all = %v, want EPERM instead of landing on the boot device", err)
	}
	if entries, _ := os.ReadDir(fs.catchAll); len(entries) != 0 {
		t.Fatalf("the unmounted catch-all holds %d entries after the refused write, want none", len(entries))
	}
}

func TestStorageTargetSync_Update_GuardsTheCatchAllOnEveryRebuild(t *testing.T) {
	fs := newImmutableFilesystem()
	s := newGuardTestSync(t, fs)
	seq := &job.ArraySequence{
		Gate:     storageTargetTestGate{ready: false},
		CatchAll: storageTargetTestMount{where: pool.CatchAllPath},
	}

	s.Update(context.Background(), seq)
	s.Update(context.Background(), seq)

	if got := fs.chattrsOf("+i"); len(got) != 2 || got[0] != fs.catchAll || got[1] != fs.catchAll {
		t.Fatalf("chattr +i issued for %v, want the catch-all on each of the two Update passes", got)
	}
}

// useCatchAllGuardOver installs the real disk.GuardMountpoint over fs as the
// array stop/start path's catch-all guard, with pool.CatchAllPath redirected
// to a directory under the test's own temp directory: nothing under /mnt is
// created.
func useCatchAllGuardOver(t *testing.T, fs *immutableFilesystem) {
	t.Helper()
	fs.catchAll = filepath.Join(t.TempDir(), "user")
	prev := catchAllGuard
	catchAllGuard = func(ctx context.Context, _ disk.Runner, path string) error {
		if path == pool.CatchAllPath {
			path = fs.catchAll
		}
		return disk.GuardMountpoint(ctx, fs, path)
	}
	t.Cleanup(func() { catchAllGuard = prev })
}

// The stopped-array data-loss scenario on the array stop path (doc 02 §1,
// Q69): the daemon's own array sequence, built the way main.go builds it, is
// stopped for a disk swap, and a container or cron job outside Hoserva's
// gated services then writes under /mnt/user. Stop itself makes the catch-all
// immutable once the pool is down, so the write fails instead of landing on
// the boot device — including for a pool that was mounted through every
// Startup and Update pass.
func TestArraySequence_StopMakesTheUnmountedCatchAllRejectWrites(t *testing.T) {
	ctx, h, _, _, _, disks, runner := newLiveArrayShutdownEnv(t)
	createLiveArray(t, ctx, h, disks, runner)
	fs := newImmutableFilesystem()
	useCatchAllGuardOver(t, fs)

	if err := h.CurrentArray().Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if got := fs.chattrsOf("+i"); len(got) != 1 || got[0] != fs.catchAll {
		t.Fatalf("chattr +i issued for %v, want exactly the catch-all %s", got, fs.catchAll)
	}
	if err := fs.write(fs.catchAll); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("write into the stopped array's catch-all = %v, want EPERM instead of landing on the boot device", err)
	}
	if entries, _ := os.ReadDir(fs.catchAll); len(entries) != 0 {
		t.Fatalf("the unmounted catch-all holds %d entries after the refused write, want none", len(entries))
	}
}

type recordingLiveMounter struct {
	events *[]string
	failOn string
}

func (m recordingLiveMounter) Mount(_ context.Context, mnt pool.Mount) error {
	*m.events = append(*m.events, "mount "+mnt.Where)
	if m.failOn == "mount" {
		return errors.New("mount failed")
	}
	return nil
}

func (m recordingLiveMounter) Unmount(_ context.Context, where string) error {
	*m.events = append(*m.events, "unmount "+where)
	if m.failOn == "unmount" {
		return errors.New("unmount failed")
	}
	return nil
}

// catchAllOfNewArraySequence builds the array sequence the way main.go does
// and swaps only the systemd mounter under the guard for a recorder, so
// Start's own catch-all Mount can run without creating /mnt/user.
func catchAllOfNewArraySequence(t *testing.T, events *[]string, failOn string) job.ArrayMount {
	t.Helper()
	ctx, h, _, _, _, disks, runner := newLiveArrayShutdownEnv(t)
	createLiveArray(t, ctx, h, disks, runner)
	mc, ok := h.CurrentArray().CatchAll.(pool.MountController)
	if !ok {
		t.Fatalf("CatchAll is %T, want pool.MountController", h.CurrentArray().CatchAll)
	}
	guarded, ok := mc.Mounter.(guardedCatchAllMounter)
	if !ok {
		t.Fatalf("CatchAll's mounter is %T, want guardedCatchAllMounter", mc.Mounter)
	}
	guarded.inner = recordingLiveMounter{events: events, failOn: failOn}
	mc.Mounter = guarded
	return mc
}

func TestArraySequence_GuardsTheCatchAllBeforeItMountsAgain(t *testing.T) {
	var events []string
	catchAll := catchAllOfNewArraySequence(t, &events, "")
	prev := catchAllGuard
	catchAllGuard = func(_ context.Context, _ disk.Runner, path string) error {
		events = append(events, "guard "+path)
		return nil
	}
	t.Cleanup(func() { catchAllGuard = prev })

	if err := catchAll.Unmount(context.Background()); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	if err := catchAll.Mount(context.Background()); err != nil {
		t.Fatalf("Mount: %v", err)
	}

	want := []string{
		"unmount " + pool.CatchAllPath, "guard " + pool.CatchAllPath,
		"guard " + pool.CatchAllPath, "mount " + pool.CatchAllPath,
	}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events = %v, want %v", events, want)
		}
	}
}

func TestArraySequence_ACatchAllGuardFindingNeverFailsTheMountOrUnmount(t *testing.T) {
	var events []string
	catchAll := catchAllOfNewArraySequence(t, &events, "")
	prev := catchAllGuard
	catchAllGuard = func(context.Context, disk.Runner, string) error { return errors.New("operation not supported") }
	t.Cleanup(func() { catchAllGuard = prev })

	if err := catchAll.Unmount(context.Background()); err != nil {
		t.Fatalf("Unmount with a failing guard: %v", err)
	}
	if err := catchAll.Mount(context.Background()); err != nil {
		t.Fatalf("Mount with a failing guard: %v", err)
	}
}

func TestArraySequence_ADeadUnmountLeavesTheCatchAllUnguarded(t *testing.T) {
	var events []string
	catchAll := catchAllOfNewArraySequence(t, &events, "unmount")
	guards := 0
	prev := catchAllGuard
	catchAllGuard = func(context.Context, disk.Runner, string) error { guards++; return nil }
	t.Cleanup(func() { catchAllGuard = prev })

	if err := catchAll.Unmount(context.Background()); err == nil {
		t.Fatal("Unmount succeeded, want the underlying failure")
	}
	if guards != 0 {
		t.Fatalf("guard ran %d times after a failed unmount, want 0: the pool is still mounted", guards)
	}
}

// The daemon builds the four jobs that assign a slot's mountpoint inside
// run(), which no test can execute; a Mounter field there that is not
// newArrayDiskMounter's own result would leave every slot they create
// unguarded.
func TestMain_MountsEveryMountpointAssigningJobThroughTheGuardedMounter(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"DiskFormatDeps":        {"Mounter"},
		"DiskAddDeps":           {"Mounter"},
		"DiskReplaceDeps":       {"Mounter"},
		"DiskUpgradeParityDeps": {"Mounter", "UpgradeMounter"},
	}
	seen := map[string]int{}
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		fields, ok := want[sel.Sel.Name]
		if !ok {
			return true
		}
		for _, field := range fields {
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok || kv.Key.(*ast.Ident).Name != field {
					continue
				}
				built := false
				if call, ok := kv.Value.(*ast.CallExpr); ok {
					id, isIdent := call.Fun.(*ast.Ident)
					built = isIdent && id.Name == "newArrayDiskMounter"
				}
				if !built {
					t.Errorf("main.go: %s.%s is not built by newArrayDiskMounter", sel.Sel.Name, field)
				}
				seen[sel.Sel.Name+"."+field]++
			}
		}
		return true
	})
	for typ, fields := range want {
		for _, field := range fields {
			if seen[typ+"."+field] != 1 {
				t.Errorf("main.go sets %s.%s %d times, want exactly once", typ, field, seen[typ+"."+field])
			}
		}
	}
}
