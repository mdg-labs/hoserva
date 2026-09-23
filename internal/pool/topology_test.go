package pool

import (
	"errors"
	"strings"
	"testing"
)

var testDisks = []string{"/mnt/disk1", "/mnt/disk2", "/mnt/disk3"}

func TestCatchAllMount(t *testing.T) {
	m, err := CatchAllMount(testDisks, DefaultOptions())
	if err != nil {
		t.Fatalf("CatchAllMount: %v", err)
	}
	if m.Where != "/mnt/user" {
		t.Fatalf("Where = %q, want /mnt/user", m.Where)
	}
	if m.What != "/mnt/disk1=RW:/mnt/disk2=RW:/mnt/disk3=RW" {
		t.Fatalf("What = %q", m.What)
	}
	if m.CreatePolicy != DefaultCreatePolicy {
		t.Fatalf("CreatePolicy = %s, want the default (doc 02 §1: catch-all uses the default policy)", m.CreatePolicy)
	}
	if m.FSName != "hoserva-pool" {
		t.Fatalf("FSName = %q, want hoserva-pool", m.FSName)
	}
	for _, d := range testDisks {
		if !contains(m.RequiresMountsFor, d) {
			t.Fatalf("RequiresMountsFor %v missing data disk %s", m.RequiresMountsFor, d)
		}
	}
}

func TestCatchAllMount_ErrorsOnNoDataDisks(t *testing.T) {
	if _, err := CatchAllMount(nil, DefaultOptions()); !errors.Is(err, ErrNoDataDisks) {
		t.Fatalf("CatchAllMount(nil): got %v, want ErrNoDataDisks", err)
	}
}

// TestAppendDataDisk_GrowsTheBranchList is doc 02 §4 "Adding a disk" step
// 5: the new mount joins the existing list, in the order it was
// discovered, without disturbing any of the disks already there.
func TestAppendDataDisk_GrowsTheBranchList(t *testing.T) {
	got, err := AppendDataDisk(testDisks, "/mnt/disk4")
	if err != nil {
		t.Fatalf("AppendDataDisk: %v", err)
	}
	want := []string{"/mnt/disk1", "/mnt/disk2", "/mnt/disk3", "/mnt/disk4"}
	if len(got) != len(want) {
		t.Fatalf("AppendDataDisk: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AppendDataDisk: got %v, want %v", got, want)
		}
	}
	// The original slice must be untouched — a caller may still hold and
	// use it (e.g. to build the old Mount it's about to remount over).
	if len(testDisks) != 3 {
		t.Fatalf("AppendDataDisk mutated its input: %v", testDisks)
	}
}

func TestAppendDataDisk_RefusesADuplicateMount(t *testing.T) {
	if _, err := AppendDataDisk(testDisks, "/mnt/disk2"); !errors.Is(err, ErrDataDiskAlreadyPresent) {
		t.Fatalf("AppendDataDisk: got %v, want ErrDataDiskAlreadyPresent", err)
	}
}

func TestShareMount_CacheThenMove(t *testing.T) {
	share := Share{Name: "movies", CacheMode: CacheThenMove, CreatePolicy: KeepFoldersTogether}
	m, err := ShareMount(share, testDisks, "/mnt/cache", DefaultOptions())
	if err != nil {
		t.Fatalf("ShareMount: %v", err)
	}
	if m.Where != "/mnt/user/movies" {
		t.Fatalf("Where = %q", m.Where)
	}
	want := "/mnt/cache/movies=RW:/mnt/disk1/movies=NC:/mnt/disk2/movies=NC:/mnt/disk3/movies=NC"
	if m.What != want {
		t.Fatalf("What:\ngot:  %s\nwant: %s", m.What, want)
	}
	if !contains(m.RequiresMountsFor, CatchAllPath) {
		t.Fatalf("RequiresMountsFor %v missing the catch-all", m.RequiresMountsFor)
	}
	if !contains(m.RequiresMountsFor, "/mnt/cache") {
		t.Fatalf("RequiresMountsFor %v missing the cache disk", m.RequiresMountsFor)
	}
}

func TestShareMount_CacheOnly(t *testing.T) {
	share := Share{Name: "appdata", CacheMode: CacheOnly, CreatePolicy: BalanceAcrossDisks}
	m, err := ShareMount(share, testDisks, "/mnt/cache", DefaultOptions())
	if err != nil {
		t.Fatalf("ShareMount: %v", err)
	}
	if m.What != "/mnt/cache/appdata=RW" {
		t.Fatalf("What = %q", m.What)
	}
	for _, d := range testDisks {
		if contains(m.RequiresMountsFor, d) {
			t.Fatalf("RequiresMountsFor %v should not depend on a data disk for a cache-only share", m.RequiresMountsFor)
		}
	}
}

func TestShareMount_ArrayOnly(t *testing.T) {
	share := Share{Name: "backups", CacheMode: ArrayOnly, CreatePolicy: FillDisksInOrder}
	m, err := ShareMount(share, testDisks, "", DefaultOptions())
	if err != nil {
		t.Fatalf("ShareMount: %v", err)
	}
	want := "/mnt/disk1/backups=RW:/mnt/disk2/backups=RW:/mnt/disk3/backups=RW"
	if m.What != want {
		t.Fatalf("What:\ngot:  %s\nwant: %s", m.What, want)
	}
	if contains(m.RequiresMountsFor, "/mnt/cache") {
		t.Fatalf("RequiresMountsFor %v should not depend on the cache disk for an array-only share", m.RequiresMountsFor)
	}
}

func TestShareMount_ErrorsOnMissingCachePath(t *testing.T) {
	share := Share{Name: "movies", CacheMode: CacheThenMove, CreatePolicy: KeepFoldersTogether}
	if _, err := ShareMount(share, testDisks, "", DefaultOptions()); err == nil {
		t.Fatal("ShareMount(cache-then-move, no cache path): got nil error")
	}
}

func TestShareMount_ErrorsOnInvalidShareName(t *testing.T) {
	share := Share{Name: "../etc", CacheMode: ArrayOnly}
	if _, err := ShareMount(share, testDisks, "", DefaultOptions()); !errors.Is(err, ErrInvalidShareName) {
		t.Fatalf("ShareMount(invalid name): got %v, want ErrInvalidShareName", err)
	}
}

func TestMoverTargetMount(t *testing.T) {
	share := Share{Name: "movies", CacheMode: CacheThenMove, CreatePolicy: KeepFoldersTogether}
	m, err := MoverTargetMount(share, testDisks, DefaultOptions())
	if err != nil {
		t.Fatalf("MoverTargetMount: %v", err)
	}
	if m.Where != "/run/hoserva/array/movies" {
		t.Fatalf("Where = %q", m.Where)
	}
	want := "/mnt/disk1/movies=RW:/mnt/disk2/movies=RW:/mnt/disk3/movies=RW"
	if m.What != want {
		t.Fatalf("What:\ngot:  %s\nwant: %s", m.What, want)
	}
	if m.CreatePolicy != share.CreatePolicy {
		t.Fatalf("CreatePolicy = %s, want the share's own %s (doc 09 §2: mergerfs places the file exactly as it would a direct write)", m.CreatePolicy, share.CreatePolicy)
	}
	if contains(m.RequiresMountsFor, CatchAllPath) {
		t.Fatalf("RequiresMountsFor %v should not depend on the catch-all — /run/hoserva/array is a separate hierarchy", m.RequiresMountsFor)
	}
}

func TestMoverTargetMount_ErrorsOnNoDataDisks(t *testing.T) {
	share := Share{Name: "movies", CacheMode: CacheThenMove, CreatePolicy: KeepFoldersTogether}
	if _, err := MoverTargetMount(share, nil, DefaultOptions()); !errors.Is(err, ErrNoDataDisks) {
		t.Fatalf("MoverTargetMount(nil disks): got %v, want ErrNoDataDisks", err)
	}
}

func TestMoverTargetMount_ErrorsOnCacheOnly(t *testing.T) {
	share := Share{Name: "appdata", CacheMode: CacheOnly, CreatePolicy: BalanceAcrossDisks}
	if _, err := MoverTargetMount(share, testDisks, DefaultOptions()); !errors.Is(err, ErrCacheOnlyNotMoved) {
		t.Fatalf("MoverTargetMount(cache-only): got %v, want ErrCacheOnlyNotMoved", err)
	}
}

// TestRemoveDataDisk_ShrinksTheBranchList is doc 09 §4 step 7's own
// mechanism: the mirror of TestAppendDataDisk_GrowsTheBranchList.
func TestRemoveDataDisk_ShrinksTheBranchList(t *testing.T) {
	got, err := RemoveDataDisk(testDisks, "/mnt/disk2")
	if err != nil {
		t.Fatalf("RemoveDataDisk: %v", err)
	}
	want := []string{"/mnt/disk1", "/mnt/disk3"}
	if len(got) != len(want) {
		t.Fatalf("RemoveDataDisk: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("RemoveDataDisk: got %v, want %v", got, want)
		}
	}
	if len(testDisks) != 3 {
		t.Fatalf("RemoveDataDisk mutated its input: %v", testDisks)
	}
}

func TestRemoveDataDisk_RefusesAMissingMount(t *testing.T) {
	if _, err := RemoveDataDisk(testDisks, "/mnt/disk9"); !errors.Is(err, ErrDataDiskNotPresent) {
		t.Fatalf("RemoveDataDisk: got %v, want ErrDataDiskNotPresent", err)
	}
}

// TestCatchAllMountRemoving_MarksOnlyTheRemovingDiskNC is doc 09 §4 step
// 2: every other data disk stays RW, and removingDisk stays in the
// branch list (it is still mounted, still holding files the evacuation
// plan is still reading off it) — only its own create mode changes.
func TestCatchAllMountRemoving_MarksOnlyTheRemovingDiskNC(t *testing.T) {
	m, err := CatchAllMountRemoving(testDisks, "/mnt/disk2", DefaultOptions())
	if err != nil {
		t.Fatalf("CatchAllMountRemoving: %v", err)
	}
	want := "/mnt/disk1=RW:/mnt/disk2=NC:/mnt/disk3=RW"
	if m.What != want {
		t.Fatalf("What:\ngot:  %s\nwant: %s", m.What, want)
	}
	if !contains(m.RequiresMountsFor, "/mnt/disk2") {
		t.Fatalf("RequiresMountsFor %v should still require the removing disk's own mount", m.RequiresMountsFor)
	}
}

// TestCatchAllMountRemoving_EmptyRemovingDiskMatchesCatchAllMount proves
// the "Removing" variant with no removingDisk behaves exactly like the
// plain constructor.
func TestCatchAllMountRemoving_EmptyRemovingDiskMatchesCatchAllMount(t *testing.T) {
	plain, err := CatchAllMount(testDisks, DefaultOptions())
	if err != nil {
		t.Fatalf("CatchAllMount: %v", err)
	}
	removing, err := CatchAllMountRemoving(testDisks, "", DefaultOptions())
	if err != nil {
		t.Fatalf("CatchAllMountRemoving: %v", err)
	}
	if plain.What != removing.What {
		t.Fatalf("What: plain=%q, removing(empty)=%q, want equal", plain.What, removing.What)
	}
}

// TestShareMountRemoving_ArrayOnlyMarksTheRemovingDiskNC proves step 2's
// own sharp edge: an ArrayOnly share's branches are RW by default, so
// removingDisk actually changes this mount's own What= line.
func TestShareMountRemoving_ArrayOnlyMarksTheRemovingDiskNC(t *testing.T) {
	share := Share{Name: "backups", CacheMode: ArrayOnly, CreatePolicy: FillDisksInOrder}
	m, err := ShareMountRemoving(share, testDisks, "", "/mnt/disk2", DefaultOptions())
	if err != nil {
		t.Fatalf("ShareMountRemoving: %v", err)
	}
	want := "/mnt/disk1/backups=RW:/mnt/disk2/backups=NC:/mnt/disk3/backups=RW"
	if m.What != want {
		t.Fatalf("What:\ngot:  %s\nwant: %s", m.What, want)
	}
}

// TestShareMountRemoving_CacheThenMoveUnaffected proves step 2's own
// no-op case: a CacheThenMove share's data-disk branches are already NC
// regardless of removingDisk, since writes always land on cache first.
func TestShareMountRemoving_CacheThenMoveUnaffected(t *testing.T) {
	share := Share{Name: "movies", CacheMode: CacheThenMove, CreatePolicy: KeepFoldersTogether}
	plain, err := ShareMount(share, testDisks, "/mnt/cache", DefaultOptions())
	if err != nil {
		t.Fatalf("ShareMount: %v", err)
	}
	removing, err := ShareMountRemoving(share, testDisks, "/mnt/cache", "/mnt/disk2", DefaultOptions())
	if err != nil {
		t.Fatalf("ShareMountRemoving: %v", err)
	}
	if plain.What != removing.What {
		t.Fatalf("What: plain=%q, removing=%q, want equal — data-disk branches are already NC for cache-then-move", plain.What, removing.What)
	}
}

// TestMoverTargetMountRemoving_MarksOnlyTheRemovingDiskNC proves the
// mover's own write target stops placing newly relocated files on the
// disk being evacuated.
func TestMoverTargetMountRemoving_MarksOnlyTheRemovingDiskNC(t *testing.T) {
	share := Share{Name: "movies", CacheMode: CacheThenMove, CreatePolicy: KeepFoldersTogether}
	m, err := MoverTargetMountRemoving(share, testDisks, "/mnt/disk2", DefaultOptions())
	if err != nil {
		t.Fatalf("MoverTargetMountRemoving: %v", err)
	}
	want := "/mnt/disk1/movies=RW:/mnt/disk2/movies=NC:/mnt/disk3/movies=RW"
	if m.What != want {
		t.Fatalf("What:\ngot:  %s\nwant: %s", m.What, want)
	}
}

// TestMountRemoving_RefusesADiskNotInDataDisks: a removingDisk that does
// not exactly match a data disk would mark no branch NC and silently
// leave the evacuating disk open to new writes.
func TestMountRemoving_RefusesADiskNotInDataDisks(t *testing.T) {
	arrayOnly := Share{Name: "backups", CacheMode: ArrayOnly, CreatePolicy: FillDisksInOrder}
	cached := Share{Name: "movies", CacheMode: CacheThenMove, CreatePolicy: KeepFoldersTogether}
	for _, removing := range []string{"/mnt/disk2/", "/mnt/disk9"} {
		builds := map[string]func() error{
			"catch-all": func() error { _, err := CatchAllMountRemoving(testDisks, removing, DefaultOptions()); return err },
			"array-only share": func() error {
				_, err := ShareMountRemoving(arrayOnly, testDisks, "", removing, DefaultOptions())
				return err
			},
			"cache-then-move share": func() error {
				_, err := ShareMountRemoving(cached, testDisks, "/mnt/cache", removing, DefaultOptions())
				return err
			},
			"mover target": func() error {
				_, err := MoverTargetMountRemoving(cached, testDisks, removing, DefaultOptions())
				return err
			},
		}
		for name, build := range builds {
			if err := build(); !errors.Is(err, ErrDataDiskNotPresent) {
				t.Errorf("%s removing %q: got %v, want ErrDataDiskNotPresent", name, removing, err)
			}
		}
	}
}

func TestSharePathAndMoverTargetPath(t *testing.T) {
	if got := SharePath("movies"); got != "/mnt/user/movies" {
		t.Fatalf("SharePath(movies) = %q", got)
	}
	if got := MoverTargetPath("movies"); got != "/run/hoserva/array/movies" {
		t.Fatalf("MoverTargetPath(movies) = %q", got)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestRWBranchesOrderPreserved(t *testing.T) {
	got := rwBranches([]string{"/mnt/disk2", "/mnt/disk1"})
	if !strings.HasPrefix(got, "/mnt/disk2=RW:") {
		t.Fatalf("rwBranches did not preserve branch order: %q", got)
	}
}
