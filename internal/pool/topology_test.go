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
