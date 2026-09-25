package api_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
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
	p.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB, WWN: "wwn-sdb"})
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
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-sdb", WWN: "wwn-sdb", Mountpoint: dataDir},
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
	if got.Disks[0].Role != apiv1.PoolDiskEntryRoleUnassigned || got.Disks[0].MountPoint != "" {
		t.Fatalf("Disks[0] = role %q mount %q, want unassigned/empty with no array topology", got.Disks[0].Role, got.Disks[0].MountPoint)
	}
}

// TestHandler_GetPool_ReportsAssignedDiskRoleAndMountPoint is #233: GetPool
// must join h.ArrayStore's persisted role and mountpoint per disk — parity
// and data disks report their real assignment, and a disk the array never
// picked up still honestly reports unassigned/empty rather than being
// silently dropped or given a role it doesn't have.
func TestHandler_GetPool_ReportsAssignedDiskRoleAndMountPoint(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newTestHandler(t)

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB, WWN: "wwn-sdb"})
	p.AddDisk("/dev/sdc", disk.Disk{Size: 4 * disk.TB, WWN: "wwn-sdc"})
	p.AddDisk("/dev/sdd", disk.Disk{Size: 4 * disk.TB, WWN: "wwn-sdd"})
	h.Disks = p

	dataDir := t.TempDir()

	arrayStore := store.NewArrayStore(newArrayStoreDB(t))
	if err := arrayStore.PutArray(ctx, store.ArraySettings{
		CreatePolicy: "mfs",
		MinFreeSpace: "1000000",
		CreatedAt:    time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-sdb", WWN: "wwn-sdb", Mountpoint: "/mnt/parity1"},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "uuid-sdc", WWN: "wwn-sdc", Mountpoint: dataDir},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	h.ArrayStore = arrayStore

	got, err := h.GetPool(ctx)
	if err != nil {
		t.Fatalf("GetPool: %v", err)
	}
	if len(got.Disks) != 3 {
		t.Fatalf("len(Disks) = %d, want 3", len(got.Disks))
	}
	byDevice := make(map[string]apiv1.PoolDiskEntry, len(got.Disks))
	for _, e := range got.Disks {
		byDevice[e.Device] = e
	}
	if e := byDevice["/dev/sdb"]; e.Role != apiv1.PoolDiskEntryRoleParity || e.MountPoint != "/mnt/parity1" {
		t.Fatalf("sdb = role %q mount %q, want parity/mnt/parity1", e.Role, e.MountPoint)
	}
	if e := byDevice["/dev/sdc"]; e.Role != apiv1.PoolDiskEntryRoleData || e.MountPoint != dataDir {
		t.Fatalf("sdc = role %q mount %q, want data/%s", e.Role, e.MountPoint, dataDir)
	}
	if e := byDevice["/dev/sdd"]; e.Role != apiv1.PoolDiskEntryRoleUnassigned || e.MountPoint != "" {
		t.Fatalf("sdd = role %q mount %q, want unassigned/empty (never assigned)", e.Role, e.MountPoint)
	}
}

// TestHandler_GetPool_ReportsRemovalState is #359's own acceptance
// criterion: getPool surfaces removalState for a disk currently in
// removal, and leaves it unset (null, never a zero-value placeholder)
// for every other disk — both for a matched (present-in-inventory) entry
// and for a stored member with no identity match (#326's own "missing"
// shape).
func TestHandler_GetPool_ReportsRemovalState(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newTestHandler(t)

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sdc", disk.Disk{Size: 4 * disk.TB, WWN: "wwn-sdc"})
	h.Disks = p

	dataDir := t.TempDir()
	arrayStore := store.NewArrayStore(newArrayStoreDB(t))
	if err := arrayStore.PutArray(ctx, store.ArraySettings{
		CreatePolicy: "mfs",
		MinFreeSpace: "1000000",
		CreatedAt:    time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "uuid-sdc", WWN: "wwn-sdc", Mountpoint: dataDir},
		{Role: store.ArrayRoleData, RoleIndex: 2, Device: "/dev/sdx", Filesystem: "xfs", FSUUID: "uuid-sdx", Mountpoint: "/mnt/disk2"},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	if err := arrayStore.SetRemovalState(ctx, dataDir, store.RemovalStateEvacuating, "job-1"); err != nil {
		t.Fatalf("SetRemovalState: %v", err)
	}
	h.ArrayStore = arrayStore

	got, err := h.GetPool(ctx)
	if err != nil {
		t.Fatalf("GetPool: %v", err)
	}
	byMount := make(map[string]apiv1.PoolDiskEntry, len(got.Disks))
	for _, e := range got.Disks {
		byMount[e.MountPoint] = e
	}
	removing, ok := byMount[dataDir]
	if !ok {
		t.Fatalf("Disks = %+v, missing the removing disk at %s", got.Disks, dataDir)
	}
	if !removing.RemovalState.Set || removing.RemovalState.Null || removing.RemovalState.Value != apiv1.DiskRemovalStateEvacuating {
		t.Fatalf("removing disk RemovalState = %+v, want set to evacuating", removing.RemovalState)
	}
	// sdx is a stored member with no identity match in inventory at all
	// (#326's own "missing" shape) — RemovalState must still round-trip
	// for that path.
	missing, ok := byMount["/mnt/disk2"]
	if !ok {
		t.Fatalf("Disks = %+v, missing the stored-only disk at /mnt/disk2", got.Disks)
	}
	if missing.RemovalState.Set {
		t.Fatalf("missing disk RemovalState = %+v, want unset (not in removal)", missing.RemovalState)
	}
}

// TestHandler_GetPool_MatchesRenumberedDiskByIdentity is #326: GetPool must
// match a stored array member to inventory by its WWN (Q21), not by the
// /dev/sdX path recorded at create-array time. A member that renumbered
// keeps its role and mountpoint at its new path, and a different disk that
// took over the vacated path is reported unassigned rather than inheriting
// the old member's assignment. Against the pre-#326 GetPool, which matches
// purely by device path, the renumbered member would come back unassigned
// and the new disk would wrongly inherit its data role.
func TestHandler_GetPool_MatchesRenumberedDiskByIdentity(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newTestHandler(t)

	dataDir := t.TempDir()
	p := disk.NewFakeProvider()
	// The stored data member renumbered from /dev/sdc to /dev/sdz.
	p.AddDisk("/dev/sdz", disk.Disk{Size: 4 * disk.TB, WWN: "wwn-data"})
	// A different physical disk now occupies the data member's old path.
	p.AddDisk("/dev/sdc", disk.Disk{Size: 4 * disk.TB, WWN: "wwn-new-disk"})
	h.Disks = p

	arrayStore := store.NewArrayStore(newArrayStoreDB(t))
	if err := arrayStore.PutArray(ctx, store.ArraySettings{
		CreatePolicy: "mfs",
		MinFreeSpace: "1000000",
		CreatedAt:    time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "uuid-data", WWN: "wwn-data", Mountpoint: dataDir},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	h.ArrayStore = arrayStore

	got, err := h.GetPool(ctx)
	if err != nil {
		t.Fatalf("GetPool: %v", err)
	}
	if len(got.Disks) != 2 {
		t.Fatalf("len(Disks) = %d, want 2", len(got.Disks))
	}
	byDevice := make(map[string]apiv1.PoolDiskEntry, len(got.Disks))
	for _, e := range got.Disks {
		byDevice[e.Device] = e
	}
	if e := byDevice["/dev/sdz"]; e.Role != apiv1.PoolDiskEntryRoleData || e.MountPoint != dataDir {
		t.Fatalf("sdz (renumbered data member) = role %q mount %q, want data/%s", e.Role, e.MountPoint, dataDir)
	}
	if e := byDevice["/dev/sdc"]; e.Role != apiv1.PoolDiskEntryRoleUnassigned || e.MountPoint != "" {
		t.Fatalf("sdc (new disk on old data path) = role %q mount %q, want unassigned/empty", e.Role, e.MountPoint)
	}
}

// TestHandler_GetPool_WeakIdentityDifferentSizeIsNotTheMember is #327
// through GET /pool: a stored weak-identity member with a recorded size
// must not be matched by a same-UUID inventory disk of a different
// capacity — that is the data-loss path when the real member is gone and
// only a clone remains.
func TestHandler_GetPool_WeakIdentityDifferentSizeIsNotTheMember(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newTestHandler(t)

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sdz", disk.Disk{Size: 8 * disk.TB, WeakIdentity: true, FSUUID: "uuid-weak"})
	h.Disks = p

	arrayStore := store.NewArrayStore(newArrayStoreDB(t))
	if err := arrayStore.PutArray(ctx, store.ArraySettings{
		CreatePolicy: "mfs",
		MinFreeSpace: "1000000",
		CreatedAt:    time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "uuid-weak", WeakIdentity: true, Mountpoint: "/mnt/disk1", Size: 4 * disk.TB, SizeSet: true},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	h.ArrayStore = arrayStore

	got, err := h.GetPool(ctx)
	if err != nil {
		t.Fatalf("GetPool: %v", err)
	}
	byDevice := make(map[string]apiv1.PoolDiskEntry, len(got.Disks))
	var missing *apiv1.PoolDiskEntry
	for i, e := range got.Disks {
		if e.Device != "" {
			byDevice[e.Device] = e
		}
		if e.State == apiv1.DiskStateMissing {
			missing = &got.Disks[i]
		}
	}
	if e := byDevice["/dev/sdz"]; e.Role != apiv1.PoolDiskEntryRoleUnassigned {
		t.Fatalf("different-size clone reported as %s member; want unassigned", e.Role)
	}
	if missing == nil || missing.Role != apiv1.PoolDiskEntryRoleData || missing.MountPoint != "/mnt/disk1" {
		t.Fatalf("missing member = %+v, want data at /mnt/disk1", missing)
	}
}

// TestHandler_GetPool_MatchesWeakIdentityDiskByFilesystemUUID is #326: a
// weak-identity disk (no wwn/serial by-id link at all, e.g. every disk in
// the loop-device lab, doc 06 §3) has nothing for disk.Identity.Matches to
// compare, so a renumbered weak-identity member must still be found by its
// stored filesystem UUID (Q21) rather than being reported missing.
func TestHandler_GetPool_MatchesWeakIdentityDiskByFilesystemUUID(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newTestHandler(t)

	dataDir := t.TempDir()
	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sdz", disk.Disk{Size: 4 * disk.TB, WeakIdentity: true, FSUUID: "uuid-weak"})
	h.Disks = p

	arrayStore := store.NewArrayStore(newArrayStoreDB(t))
	if err := arrayStore.PutArray(ctx, store.ArraySettings{
		CreatePolicy: "mfs",
		MinFreeSpace: "1000000",
		CreatedAt:    time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "uuid-weak", WeakIdentity: true, Mountpoint: dataDir},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	h.ArrayStore = arrayStore

	got, err := h.GetPool(ctx)
	if err != nil {
		t.Fatalf("GetPool: %v", err)
	}
	if len(got.Disks) != 1 {
		t.Fatalf("len(Disks) = %d, want 1", len(got.Disks))
	}
	if e := got.Disks[0]; e.Device != "/dev/sdz" || e.Role != apiv1.PoolDiskEntryRoleData || e.MountPoint != dataDir {
		t.Fatalf("sdz (renumbered weak-identity member) = device %q role %q mount %q, want /dev/sdz/data/%s", e.Device, e.Role, e.MountPoint, dataDir)
	}
}

// TestHandler_GetPool_ReportsMissingArrayDisk is #326: a stored array
// member with no identity match anywhere in inventory (a dead or pulled
// drive, doc 02 §4) must still get its own entry — stored device, role and
// mountpoint, state missing, no size — rather than disappearing from the
// response entirely, since that is the one slot the pool page's own
// replace-disk flow (#288) needs to show. Against the pre-#326 GetPool,
// which only ever emits an entry per h.Disks.List result, the missing data
// disk here would not appear in got.Disks at all.
func TestHandler_GetPool_ReportsMissingArrayDisk(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newTestHandler(t)

	p := disk.NewFakeProvider()
	// Only the parity disk is present; the data disk has failed or been
	// pulled and is absent from inventory entirely.
	p.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB, WWN: "wwn-parity"})
	h.Disks = p

	arrayStore := store.NewArrayStore(newArrayStoreDB(t))
	if err := arrayStore.PutArray(ctx, store.ArraySettings{
		CreatePolicy: "mfs",
		MinFreeSpace: "1000000",
		CreatedAt:    time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-parity", WWN: "wwn-parity", Mountpoint: "/mnt/parity1"},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "uuid-sdc", WWN: "wwn-sdc", Mountpoint: "/mnt/disk1"},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	h.ArrayStore = arrayStore

	got, err := h.GetPool(ctx)
	if err != nil {
		t.Fatalf("GetPool: %v", err)
	}
	if len(got.Disks) != 2 {
		t.Fatalf("len(Disks) = %d, want 2", len(got.Disks))
	}
	byDevice := make(map[string]apiv1.PoolDiskEntry, len(got.Disks))
	for _, e := range got.Disks {
		byDevice[e.Device] = e
	}
	missing, ok := byDevice["/dev/sdc"]
	if !ok {
		t.Fatalf("missing data disk /dev/sdc not reported at all, want a %q entry", apiv1.DiskStateMissing)
	}
	if missing.State != apiv1.DiskStateMissing || missing.Role != apiv1.PoolDiskEntryRoleData || missing.MountPoint != "/mnt/disk1" {
		t.Fatalf("sdc = state %q role %q mount %q, want missing/data//mnt/disk1", missing.State, missing.Role, missing.MountPoint)
	}
	if v, ok := missing.SizeBytes.Get(); ok {
		t.Fatalf("missing disk SizeBytes = %+v, want unset", v)
	}
	if e := byDevice["/dev/sdb"]; e.State != apiv1.DiskStateActive {
		t.Fatalf("sdb (present) state = %q, want active", e.State)
	}
}

// TestHandler_GetPool_AmbiguousCloneMatchesNoMember: a weak-identity member
// and a dd-made clone share one filesystem UUID, so both inventory disks
// match the same stored row. Neither may be reported as that member —
// picking one by inventory order would show two disks at one role and
// mountpoint, or the wrong one.
func TestHandler_GetPool_AmbiguousCloneMatchesNoMember(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newTestHandler(t)

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sdy", disk.Disk{Size: 4 * disk.TB, WeakIdentity: true, FSUUID: "uuid-weak"})
	p.AddDisk("/dev/sdz", disk.Disk{Size: 4 * disk.TB, WeakIdentity: true, FSUUID: "uuid-weak"})
	h.Disks = p

	arrayStore := store.NewArrayStore(newArrayStoreDB(t))
	if err := arrayStore.PutArray(ctx, store.ArraySettings{
		CreatePolicy: "mfs",
		MinFreeSpace: "1000000",
		CreatedAt:    time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "uuid-weak", WeakIdentity: true, Mountpoint: "/mnt/disk1"},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	h.ArrayStore = arrayStore

	got, err := h.GetPool(ctx)
	if err != nil {
		t.Fatalf("GetPool: %v", err)
	}
	for _, e := range got.Disks {
		if e.State == apiv1.DiskStateActive && e.Role != apiv1.PoolDiskEntryRoleUnassigned {
			t.Fatalf("%s reported as active %s member at %q; an ambiguous match must assign no inventory disk", e.Device, e.Role, e.MountPoint)
		}
	}
}

// TestHandler_GetPool_MissingMemberDoesNotReuseAPresentDevicePath: a
// missing member's stored /dev path now belongs to a different, present
// disk (the kernel reused it). The pool page keys and indexes entries by
// device, so the missing entry must not repeat that path.
func TestHandler_GetPool_MissingMemberDoesNotReuseAPresentDevicePath(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newTestHandler(t)

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sdc", disk.Disk{Size: 8 * disk.TB, WWN: "wwn-replacement"})
	h.Disks = p

	arrayStore := store.NewArrayStore(newArrayStoreDB(t))
	if err := arrayStore.PutArray(ctx, store.ArraySettings{
		CreatePolicy: "mfs",
		MinFreeSpace: "1000000",
		CreatedAt:    time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "uuid-sdc", WWN: "wwn-dead", Mountpoint: "/mnt/disk1"},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	h.ArrayStore = arrayStore

	got, err := h.GetPool(ctx)
	if err != nil {
		t.Fatalf("GetPool: %v", err)
	}
	seen := map[string]int{}
	var missing *apiv1.PoolDiskEntry
	for i, e := range got.Disks {
		if e.Device != "" {
			seen[e.Device]++
		}
		if e.State == apiv1.DiskStateMissing {
			missing = &got.Disks[i]
		}
	}
	if seen["/dev/sdc"] != 1 {
		t.Fatalf("/dev/sdc reported %d times, want once", seen["/dev/sdc"])
	}
	if missing == nil || missing.MountPoint != "/mnt/disk1" || missing.Role != apiv1.PoolDiskEntryRoleData {
		t.Fatalf("missing member = %+v, want a data entry at /mnt/disk1", missing)
	}
}
