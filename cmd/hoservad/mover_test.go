package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

func newMoverTestStores(t *testing.T) (*store.ShareStore, *store.ArrayStore) {
	t.Helper()
	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "hoservad-mover-test.db")
	db, err := sql.Open("sqlite", store.DSN(dbPath))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	return store.NewShareStore(db), store.NewArrayStore(db)
}

func insertMoverTestShare(t *testing.T, shares *store.ShareStore, name, cacheMode string) {
	t.Helper()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	if err := shares.Insert(context.Background(), store.Share{
		Name:         name,
		CacheMode:    cacheMode,
		CreatePolicy: "mfs",
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("insert share %s: %v", name, err)
	}
}

func putMoverTestArray(t *testing.T, arrays *store.ArrayStore, minFreeSpace string) {
	t.Helper()
	if err := arrays.PutArray(context.Background(), store.ArraySettings{
		CreatePolicy: "mfs",
		MinFreeSpace: minFreeSpace,
		CreatedAt:    time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d1", WWN: "wwn-d1", Serial: "DATA1", ByIDName: "wwn-wwn-d1", Mountpoint: "/mnt/disk1"},
		{Role: store.ArrayRoleData, RoleIndex: 2, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "uuid-d2", WWN: "wwn-d2", Serial: "DATA2", ByIDName: "wwn-wwn-d2", Mountpoint: "/mnt/disk2"},
		{Role: store.ArrayRoleCache, RoleIndex: 1, Device: "/dev/sdd", Filesystem: "xfs", FSUUID: "uuid-c1", WWN: "wwn-c1", Serial: "CACHE1", ByIDName: "wwn-wwn-c1", Mountpoint: "/mnt/cache"},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
}

// TestMoverSharesFromStore_ResolvesOnlyCacheThenMoveShares proves the
// resolver builds a cache.Share only for a cache-then-move share (#53,
// scope: "resolving cache-then-move shares from internal/store"), with its
// cache and array-only mount paths and per-data-disk branches built the
// same way pool's own MoverTargetMount and ShareMount do, and skips a
// cache-only or array-only share entirely — the mover never touches data
// that already lives permanently on cache or the array.
func TestMoverSharesFromStore_ResolvesOnlyCacheThenMoveShares(t *testing.T) {
	shares, arrays := newMoverTestStores(t)
	putMoverTestArray(t, arrays, "20G")
	insertMoverTestShare(t, shares, "movies", string(pool.CacheThenMove))
	insertMoverTestShare(t, shares, "cacheonly", string(pool.CacheOnly))
	insertMoverTestShare(t, shares, "arrayonly", string(pool.ArrayOnly))

	got, err := moverSharesFromStore(shares, arrays)(context.Background())
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("resolved shares = %+v, want exactly one cache-then-move share", got)
	}
	s := got[0]
	if s.Name != "movies" {
		t.Fatalf("Name = %q, want %q", s.Name, "movies")
	}
	if s.CachePath != "/mnt/cache/movies" {
		t.Fatalf("CachePath = %q, want %q", s.CachePath, "/mnt/cache/movies")
	}
	if s.ArrayPath != pool.MoverTargetPath("movies") {
		t.Fatalf("ArrayPath = %q, want %q", s.ArrayPath, pool.MoverTargetPath("movies"))
	}
	wantBranches := []string{"/mnt/disk1/movies", "/mnt/disk2/movies"}
	if len(s.Branches) != len(wantBranches) || s.Branches[0] != wantBranches[0] || s.Branches[1] != wantBranches[1] {
		t.Fatalf("Branches = %v, want %v", s.Branches, wantBranches)
	}
	if s.MinFreeSpace != 20*(1<<30) {
		t.Fatalf("MinFreeSpace = %d, want %d (20G)", s.MinFreeSpace, 20*(1<<30))
	}
}

// TestMoverSharesFromStore_NoArrayYetIsNoShares proves the resolver treats
// store.ErrNoArray the same "nothing to do yet" way newArraySequence does
// (array.go) — the mover has nothing to relocate before create-array has
// ever run, so this must not fail a mover job before the array exists.
func TestMoverSharesFromStore_NoArrayYetIsNoShares(t *testing.T) {
	shares, arrays := newMoverTestStores(t)
	insertMoverTestShare(t, shares, "movies", string(pool.CacheThenMove))

	got, err := moverSharesFromStore(shares, arrays)(context.Background())
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("resolved shares = %+v, want none before create-array has run", got)
	}
}

// TestMoverSharesFromStore_NoCacheDiskIsAnError proves an inconsistent
// topology — a cache-then-move share persisted against an array with no
// cache disk — is reported rather than silently resolved to a nonsense
// cache path.
func TestMoverSharesFromStore_NoCacheDiskIsAnError(t *testing.T) {
	shares, arrays := newMoverTestStores(t)
	if err := arrays.PutArray(context.Background(), store.ArraySettings{
		CreatePolicy: "mfs",
		MinFreeSpace: "20G",
		CreatedAt:    time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d1", WWN: "wwn-d1", Serial: "DATA1", ByIDName: "wwn-wwn-d1", Mountpoint: "/mnt/disk1"},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	insertMoverTestShare(t, shares, "movies", string(pool.CacheThenMove))

	_, err := moverSharesFromStore(shares, arrays)(context.Background())
	if err == nil {
		t.Fatal("expected an error for a cache-then-move share with no cache disk")
	}
}

func TestParseMinFreeSpaceBytes(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{in: "50G", want: 50 * (1 << 30)},
		{in: "50GB", want: 50 * (1 << 30)},
		{in: "500M", want: 500 * (1 << 20)},
		{in: "1T", want: 1 << 40},
		{in: "1024", want: 1024},
		{in: "", want: 50 * (1 << 30)}, // pool.DefaultOptions() fallback
		{in: "not-a-size", wantErr: true},
	}
	for _, tc := range cases {
		got, err := parseMinFreeSpaceBytes(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseMinFreeSpaceBytes(%q) = %d, nil, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseMinFreeSpaceBytes(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseMinFreeSpaceBytes(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
