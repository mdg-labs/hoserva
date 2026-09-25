package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func migratedArrayDB(t *testing.T) *ArrayStore {
	t.Helper()
	migrations, err := Load()
	if err != nil {
		t.Fatalf("loading migrations: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "array-test.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	return NewArrayStore(db)
}

func TestArrayStore_PutGetAndRefuseOverwrite(t *testing.T) {
	ctx := context.Background()
	st := migratedArrayDB(t)

	exists, err := st.Exists(ctx)
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if exists {
		t.Fatal("fresh database already has array topology")
	}
	if _, _, err := st.GetArray(ctx); !errors.Is(err, ErrNoArray) {
		t.Fatalf("GetArray = %v, want ErrNoArray", err)
	}

	settings := ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "20G", CreatedAt: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)}
	disks := []ArrayDisk{
		{Role: ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", WWN: "wwn-p", Mountpoint: "/mnt/parity1", Size: 8 << 40, SizeSet: true},
		{Role: ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d", Serial: "DATA1", Mountpoint: "/mnt/disk1", Size: 4 << 40, SizeSet: true},
		{Role: ArrayRoleCache, RoleIndex: 1, Device: "/dev/sdc", Filesystem: "ext4", FSUUID: "uuid-c", Mountpoint: "/mnt/cache", Size: 1 << 40, SizeSet: true},
	}
	if err := st.PutArray(ctx, settings, disks); err != nil {
		t.Fatalf("PutArray: %v", err)
	}

	gotSettings, gotDisks, err := st.GetArray(ctx)
	if err != nil {
		t.Fatalf("GetArray: %v", err)
	}
	if gotSettings.CreatePolicy != "mfs" || gotSettings.MinFreeSpace != "20G" {
		t.Fatalf("settings = %+v", gotSettings)
	}
	if len(gotDisks) != 3 {
		t.Fatalf("len(disks) = %d, want 3", len(gotDisks))
	}
	if gotDisks[0].Role != ArrayRoleParity || gotDisks[0].WWN != "wwn-p" || gotDisks[0].FSUUID != "uuid-p" {
		t.Fatalf("parity = %+v", gotDisks[0])
	}
	if !gotDisks[0].SizeSet || gotDisks[0].Size != 8<<40 {
		t.Fatalf("parity size = %d set=%v, want 8TiB set", gotDisks[0].Size, gotDisks[0].SizeSet)
	}
	if gotDisks[1].Role != ArrayRoleData || gotDisks[1].Serial != "DATA1" {
		t.Fatalf("data = %+v", gotDisks[1])
	}
	if !gotDisks[1].SizeSet || gotDisks[1].Size != 4<<40 {
		t.Fatalf("data size = %d set=%v, want 4TiB set", gotDisks[1].Size, gotDisks[1].SizeSet)
	}
	if gotDisks[2].Role != ArrayRoleCache || gotDisks[2].Filesystem != "ext4" {
		t.Fatalf("cache = %+v", gotDisks[2])
	}

	if err := st.PutArray(ctx, settings, disks); !errors.Is(err, ErrArrayExists) {
		t.Fatalf("second PutArray = %v, want ErrArrayExists", err)
	}
}

// twoDataDiskArray puts a minimal array (one parity disk, two data disks)
// into st, for the removal-state tests below.
func twoDataDiskArray(t *testing.T, st *ArrayStore) {
	t.Helper()
	ctx := context.Background()
	settings := ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "20G", CreatedAt: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)}
	disks := []ArrayDisk{
		{Role: ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", Mountpoint: "/mnt/parity1"},
		{Role: ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d1", Mountpoint: "/mnt/disk1"},
		{Role: ArrayRoleData, RoleIndex: 2, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "uuid-d2", Mountpoint: "/mnt/disk2"},
	}
	if err := st.PutArray(ctx, settings, disks); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
}

// TestArrayStore_SetRemovalState_PersistsAndRoundTrips proves the basic
// contract #359 needs: SetRemovalState persists the state, GetArray/
// ListArrayDisks reads it back on the matching row (and NULL, i.e. "", on
// every other), and RemovingDisk reports the same disk and state.
func TestArrayStore_SetRemovalState_PersistsAndRoundTrips(t *testing.T) {
	ctx := context.Background()
	st := migratedArrayDB(t)
	twoDataDiskArray(t, st)

	if err := st.SetRemovalState(ctx, "/mnt/disk1", RemovalStateEvacuating, "job-a"); err != nil {
		t.Fatalf("SetRemovalState: %v", err)
	}

	_, disks, err := st.GetArray(ctx)
	if err != nil {
		t.Fatalf("GetArray: %v", err)
	}
	for _, d := range disks {
		switch d.Mountpoint {
		case "/mnt/disk1":
			if d.RemovalState != RemovalStateEvacuating {
				t.Fatalf("disk1 RemovalState = %q, want %q", d.RemovalState, RemovalStateEvacuating)
			}
		default:
			if d.RemovalState != "" {
				t.Fatalf("%s RemovalState = %q, want empty", d.Mountpoint, d.RemovalState)
			}
		}
	}

	mountpoint, state, err := st.RemovingDisk(ctx)
	if err != nil {
		t.Fatalf("RemovingDisk: %v", err)
	}
	if mountpoint != "/mnt/disk1" || state != RemovalStateEvacuating {
		t.Fatalf("RemovingDisk = (%q, %q), want (/mnt/disk1, %q)", mountpoint, state, RemovalStateEvacuating)
	}
}

// TestArrayStore_SetRemovalState_RefusesASecondDisk is this issue's own
// safety-critical invariant: only one disk is ever in removal at a time
// (the *Removing mount builders each take a single removingDisk), so a
// second disk's own SetRemovalState call must refuse while the first is
// still non-NULL, naming the disk already in removal.
func TestArrayStore_SetRemovalState_RefusesASecondDisk(t *testing.T) {
	ctx := context.Background()
	st := migratedArrayDB(t)
	twoDataDiskArray(t, st)

	if err := st.SetRemovalState(ctx, "/mnt/disk1", RemovalStateEvacuating, "job-a"); err != nil {
		t.Fatalf("SetRemovalState(disk1): %v", err)
	}
	err := st.SetRemovalState(ctx, "/mnt/disk2", RemovalStateEvacuating, "job-a")
	if !errors.Is(err, ErrAnotherDiskRemoving) {
		t.Fatalf("SetRemovalState(disk2) while disk1 is removing = %v, want ErrAnotherDiskRemoving", err)
	}

	// The refused call must not have partially applied: disk1 is still
	// the one and only disk in removal.
	mountpoint, state, dErr := st.RemovingDisk(ctx)
	if dErr != nil {
		t.Fatalf("RemovingDisk: %v", dErr)
	}
	if mountpoint != "/mnt/disk1" || state != RemovalStateEvacuating {
		t.Fatalf("RemovingDisk after the refused call = (%q, %q), want unchanged (/mnt/disk1, %q)", mountpoint, state, RemovalStateEvacuating)
	}
}

// TestArrayStore_SetRemovalState_SameDiskAgainIsIdempotent proves the
// case RunEvacuation's own resume path depends on: calling
// SetRemovalState again for the disk that is already removing — with the
// same state, or the "evacuating" -> "evacuated" transition — succeeds
// rather than refusing itself as "another disk".
func TestArrayStore_SetRemovalState_SameDiskAgainIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st := migratedArrayDB(t)
	twoDataDiskArray(t, st)

	if err := st.SetRemovalState(ctx, "/mnt/disk1", RemovalStateEvacuating, "job-a"); err != nil {
		t.Fatalf("SetRemovalState(evacuating): %v", err)
	}
	if err := st.SetRemovalState(ctx, "/mnt/disk1", RemovalStateEvacuating, "job-a"); err != nil {
		t.Fatalf("SetRemovalState(evacuating) again: %v", err)
	}
	if err := st.SetRemovalState(ctx, "/mnt/disk1", RemovalStateEvacuated, "job-a"); err != nil {
		t.Fatalf("SetRemovalState(evacuated): %v", err)
	}
	_, state, err := st.RemovingDisk(ctx)
	if err != nil {
		t.Fatalf("RemovingDisk: %v", err)
	}
	if state != RemovalStateEvacuated {
		t.Fatalf("state = %q, want %q", state, RemovalStateEvacuated)
	}
}

// TestArrayStore_SetRemovalState_RefusesNonDataDisk proves SetRemovalState
// only ever accepts a data disk's own mountpoint — evacuation only ever
// makes sense for one (doc 02 §4's same restriction for evacuationDataDisk,
// internal/api/rebalance_handler.go).
func TestArrayStore_SetRemovalState_RefusesNonDataDisk(t *testing.T) {
	ctx := context.Background()
	st := migratedArrayDB(t)
	twoDataDiskArray(t, st)

	if err := st.SetRemovalState(ctx, "/mnt/parity1", RemovalStateEvacuating, "job-a"); !errors.Is(err, ErrArrayDiskNotFound) {
		t.Fatalf("SetRemovalState(parity1) = %v, want ErrArrayDiskNotFound", err)
	}
}

// TestArrayStore_SetRemovalState_RecordsTheHoldingJob proves the row
// carries the job that set its state, and that a later job setting the
// same disk takes it over.
func TestArrayStore_SetRemovalState_RecordsTheHoldingJob(t *testing.T) {
	ctx := context.Background()
	st := migratedArrayDB(t)
	twoDataDiskArray(t, st)

	holder := func() string {
		t.Helper()
		_, disks, err := st.GetArray(ctx)
		if err != nil {
			t.Fatalf("GetArray: %v", err)
		}
		for _, d := range disks {
			if d.Mountpoint == "/mnt/disk1" {
				return d.RemovalJobID
			}
		}
		t.Fatal("disk1 not found")
		return ""
	}

	if err := st.SetRemovalState(ctx, "/mnt/disk1", RemovalStateEvacuating, "job-a"); err != nil {
		t.Fatalf("SetRemovalState(job-a): %v", err)
	}
	if got := holder(); got != "job-a" {
		t.Fatalf("RemovalJobID = %q, want job-a", got)
	}
	if err := st.SetRemovalState(ctx, "/mnt/disk1", RemovalStateEvacuating, "job-b"); err != nil {
		t.Fatalf("SetRemovalState(job-b): %v", err)
	}
	if got := holder(); got != "job-b" {
		t.Fatalf("RemovalJobID after job-b took the disk over = %q, want job-b", got)
	}
	if err := st.SetRemovalState(ctx, "/mnt/disk1", RemovalStateEvacuating, ""); err == nil {
		t.Fatal("SetRemovalState with an empty job id succeeded, want a refusal")
	}
}

// TestArrayStore_ReleaseRemovalState_OnlyByTheHoldingJob proves the
// release a cancelled evacuation makes: a job that does not hold the
// state leaves it alone, the holding job clears it (and its job id), and
// a second disk can then enter removal.
func TestArrayStore_ReleaseRemovalState_OnlyByTheHoldingJob(t *testing.T) {
	ctx := context.Background()
	st := migratedArrayDB(t)
	twoDataDiskArray(t, st)

	if err := st.SetRemovalState(ctx, "/mnt/disk1", RemovalStateEvacuating, "job-b"); err != nil {
		t.Fatalf("SetRemovalState: %v", err)
	}
	released, err := st.ReleaseRemovalState(ctx, "/mnt/disk1", "job-a")
	if err != nil {
		t.Fatalf("ReleaseRemovalState(job-a): %v", err)
	}
	if released {
		t.Fatal("ReleaseRemovalState(job-a) released a state job-b holds")
	}
	if mountpoint, state, err := st.RemovingDisk(ctx); err != nil || mountpoint != "/mnt/disk1" || state != RemovalStateEvacuating {
		t.Fatalf("RemovingDisk after a non-holder release = (%q, %q, %v), want (/mnt/disk1, %q, nil)", mountpoint, state, err, RemovalStateEvacuating)
	}

	released, err = st.ReleaseRemovalState(ctx, "/mnt/disk1", "job-b")
	if err != nil {
		t.Fatalf("ReleaseRemovalState(job-b): %v", err)
	}
	if !released {
		t.Fatal("ReleaseRemovalState(job-b) did not release the state job-b holds")
	}
	mountpoint, _, err := st.RemovingDisk(ctx)
	if err != nil {
		t.Fatalf("RemovingDisk: %v", err)
	}
	if mountpoint != "" {
		t.Fatalf("RemovingDisk after release = %q, want none", mountpoint)
	}
	_, disks, err := st.GetArray(ctx)
	if err != nil {
		t.Fatalf("GetArray: %v", err)
	}
	for _, d := range disks {
		if d.RemovalJobID != "" {
			t.Fatalf("%s RemovalJobID = %q after release, want empty", d.Mountpoint, d.RemovalJobID)
		}
	}

	if err := st.SetRemovalState(ctx, "/mnt/disk2", RemovalStateEvacuating, "job-c"); err != nil {
		t.Fatalf("SetRemovalState(disk2) after releasing disk1: %v", err)
	}
}

// TestArrayStore_ReleaseRemovalState_LeavesEvacuatedAlone proves a
// release never undoes "evacuated", even by the job that reached it.
func TestArrayStore_ReleaseRemovalState_LeavesEvacuatedAlone(t *testing.T) {
	ctx := context.Background()
	st := migratedArrayDB(t)
	twoDataDiskArray(t, st)

	if err := st.SetRemovalState(ctx, "/mnt/disk1", RemovalStateEvacuated, "job-a"); err != nil {
		t.Fatalf("SetRemovalState: %v", err)
	}
	released, err := st.ReleaseRemovalState(ctx, "/mnt/disk1", "job-a")
	if err != nil {
		t.Fatalf("ReleaseRemovalState: %v", err)
	}
	if released {
		t.Fatal("ReleaseRemovalState released an evacuated disk")
	}
	if _, state, err := st.RemovingDisk(ctx); err != nil || state != RemovalStateEvacuated {
		t.Fatalf("state = (%q, %v), want %q", state, err, RemovalStateEvacuated)
	}
}

// TestArrayStore_RemovingDisk_NoneByDefault proves RemovingDisk reports
// no disk when no removal is in progress, the common case every mount
// build must byte-match the pre-#359 output for.
func TestArrayStore_RemovingDisk_NoneByDefault(t *testing.T) {
	ctx := context.Background()
	st := migratedArrayDB(t)
	twoDataDiskArray(t, st)

	mountpoint, state, err := st.RemovingDisk(ctx)
	if err != nil {
		t.Fatalf("RemovingDisk: %v", err)
	}
	if mountpoint != "" || state != "" {
		t.Fatalf("RemovingDisk = (%q, %q), want both empty", mountpoint, state)
	}
}
