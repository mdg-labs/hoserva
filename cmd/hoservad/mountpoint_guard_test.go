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

func newGuardTestSync(t *testing.T, fs *immutableFilesystem) *storageTargetSync {
	t.Helper()
	s := newTestStorageTargetSync(t)
	s.Runner = fs
	s.MountpointGuard = nil
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

	if got := fs.chattrsOf("+i"); !containsAll(got, dirs...) || len(got) != len(dirs) {
		t.Fatalf("chattr +i issued for %v, want exactly the restored slots %v", got, dirs)
	}
	for _, d := range dirs {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Fatalf("restored slot mountpoint %s was not created: %v", d, err)
		}
		if err := fs.write(d); !errors.Is(err, syscall.EPERM) {
			t.Fatalf("write into restored, unmounted slot %s = %v, want EPERM", d, err)
		}
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
