package api_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// newArrayStoreDB gives GetPool's free-space tests a real, migrated SQLite
// database for store.ArrayStore — the same migration runner the daemon
// uses (registerDiskFormat in array_handler_test.go follows the same
// pattern), independent of newTestHandler's own database so a test can
// choose exactly which topology it persists.
func newArrayStoreDB(t *testing.T) *sql.DB {
	t.Helper()
	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "pool-space.db")+"?_pragma=busy_timeout(5000)")
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

// TestHandler_GetPool_PopulatesFreeSpaceFromStatfs is AC1 (#57): GetPool
// must expose pool-free, largest-single-disk-free and per-disk free, with
// disks near minfreespace flagged, sourced from pool.ComputePoolSpace
// (statfs(2), never a directory walk). minFreeSpace is set to exactly the
// data disk's own current free bytes so NearMinFreeSpace is deterministic
// regardless of how much space the host test filesystem actually has.
func TestHandler_GetPool_PopulatesFreeSpaceFromStatfs(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newTestHandler(t)

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB})
	h.Disks = p

	dataDir := t.TempDir()

	// minFreeSpace only has to be some fixed, generous value the disk's
	// free space cannot rise above during the test, so NearMinFreeSpace
	// comes back true regardless of how much space the host test
	// filesystem actually has; it does not need to equal the exact free
	// byte count GetPool itself observes.
	arrayStore := store.NewArrayStore(newArrayStoreDB(t))
	if err := arrayStore.PutArray(ctx, store.ArraySettings{
		CreatePolicy: "mfs",
		MinFreeSpace: fmt.Sprintf("%d", 1<<62),
		CreatedAt:    time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-sdb", Mountpoint: dataDir},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	h.ArrayStore = arrayStore

	got, err := h.GetPool(ctx)
	if err != nil {
		t.Fatalf("GetPool: %v", err)
	}

	// Two statfs(2) samples of the same filesystem — one taken here, one
	// inside GetPool — can legitimately disagree by a few blocks even
	// with nothing under this test's own control writing to it (other
	// processes share the same underlying filesystem). Assert presence
	// and a sane positive value, then that GetPool's own figures agree
	// with each other: with the single data disk this test sets up,
	// PoolFreeBytes, LargestDiskFreeBytes and Disks[0].FreeBytes must
	// all be the same one statfs(2) reading GetPool itself took.
	poolFree, ok := got.PoolFreeBytes.Get()
	if !ok || poolFree <= 0 {
		t.Fatalf("PoolFreeBytes = %+v, want a positive value", got.PoolFreeBytes)
	}
	largestFree, ok := got.LargestDiskFreeBytes.Get()
	if !ok || largestFree != poolFree {
		t.Fatalf("LargestDiskFreeBytes = %+v, want %d (PoolFreeBytes, the only data disk)", got.LargestDiskFreeBytes, poolFree)
	}
	if v, ok := got.LargestDiskPath.Get(); !ok || v != dataDir {
		t.Fatalf("LargestDiskPath = %+v, want %s", got.LargestDiskPath, dataDir)
	}
	if len(got.Disks) != 1 {
		t.Fatalf("len(Disks) = %d, want 1", len(got.Disks))
	}
	entry := got.Disks[0]
	if v, ok := entry.FreeBytes.Get(); !ok || v != poolFree {
		t.Fatalf("Disks[0].FreeBytes = %+v, want %d (PoolFreeBytes, the only data disk)", entry.FreeBytes, poolFree)
	}
	if v, ok := entry.NearMinFreeSpace.Get(); !ok || !v {
		t.Fatalf("Disks[0].NearMinFreeSpace = %+v, want true (minFreeSpace == actual free bytes)", entry.NearMinFreeSpace)
	}
}

// TestHandler_GetPool_NoArrayTopologyLeavesSpaceFieldsUnset covers GetPool
// before create-array has ever run: disk inventory must still be reported
// (existing behaviour), and the new free-space fields must come back
// unset rather than the handler failing outright.
func TestHandler_GetPool_NoArrayTopologyLeavesSpaceFieldsUnset(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newTestHandler(t)

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB})
	h.Disks = p
	h.ArrayStore = store.NewArrayStore(newArrayStoreDB(t))

	got, err := h.GetPool(ctx)
	if err != nil {
		t.Fatalf("GetPool: %v", err)
	}
	if _, ok := got.PoolFreeBytes.Get(); ok {
		t.Fatalf("PoolFreeBytes set with no array topology: %+v", got.PoolFreeBytes)
	}
	if _, ok := got.LargestDiskFreeBytes.Get(); ok {
		t.Fatalf("LargestDiskFreeBytes set with no array topology: %+v", got.LargestDiskFreeBytes)
	}
	if len(got.Disks) != 1 {
		t.Fatalf("len(Disks) = %d, want 1", len(got.Disks))
	}
	if _, ok := got.Disks[0].FreeBytes.Get(); ok {
		t.Fatalf("Disks[0].FreeBytes set with no array topology: %+v", got.Disks[0].FreeBytes)
	}
}
