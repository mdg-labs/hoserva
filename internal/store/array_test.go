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

// TestArrayStore_CancelRemovalState_OnlyTheStateItRead proves
// cancelDiskRemoval's clear is a compare-and-swap on the state and job id
// its caller read: a re-evacuation of an evacuated disk that starts after
// that read leaves the new evacuation's state in place, and only a cancel
// that names the current state and job clears it.
func TestArrayStore_CancelRemovalState_OnlyTheStateItRead(t *testing.T) {
	ctx := context.Background()
	st := migratedArrayDB(t)
	twoDataDiskArray(t, st)

	if err := st.SetRemovalState(ctx, "/mnt/disk1", RemovalStateEvacuated, "job-a"); err != nil {
		t.Fatalf("SetRemovalState(evacuated, job-a): %v", err)
	}
	if err := st.SetRemovalState(ctx, "/mnt/disk1", RemovalStateEvacuating, "job-b"); err != nil {
		t.Fatalf("SetRemovalState(evacuating, job-b): %v", err)
	}

	cleared, err := st.CancelRemovalState(ctx, "/mnt/disk1", RemovalStateEvacuated, "job-a")
	if err != nil {
		t.Fatalf("CancelRemovalState(stale read): %v", err)
	}
	if cleared {
		t.Fatal("CancelRemovalState(evacuated, job-a) cleared the re-evacuation job-b holds")
	}
	if mountpoint, state, err := st.RemovingDisk(ctx); err != nil || mountpoint != "/mnt/disk1" || state != RemovalStateEvacuating {
		t.Fatalf("RemovingDisk after a stale cancel = (%q, %q, %v), want (/mnt/disk1, %q, nil)", mountpoint, state, err, RemovalStateEvacuating)
	}

	cleared, err = st.CancelRemovalState(ctx, "/mnt/disk1", RemovalStateEvacuating, "job-b")
	if err != nil {
		t.Fatalf("CancelRemovalState(current read): %v", err)
	}
	if !cleared {
		t.Fatal("CancelRemovalState(evacuating, job-b) did not clear the state it read")
	}
	if mountpoint, _, err := st.RemovingDisk(ctx); err != nil || mountpoint != "" {
		t.Fatalf("RemovingDisk after cancel = (%q, %v), want none", mountpoint, err)
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

func removalOf(t *testing.T, st *ArrayStore, mountpoint string) (state, holder string, present bool) {
	t.Helper()
	_, disks, err := st.GetArray(context.Background())
	if err != nil {
		t.Fatalf("GetArray: %v", err)
	}
	for _, d := range disks {
		if d.Mountpoint == mountpoint {
			return d.RemovalState, d.RemovalJobID, true
		}
	}
	return "", "", false
}

// TestArrayStore_AdvanceRemovalState_OnlyFromTheExpectedState proves the
// disk_remove job's own transitions (#358): each moves only from the
// state before it or repeats itself, and every other starting state is
// refused with nothing written.
func TestArrayStore_AdvanceRemovalState_OnlyFromTheExpectedState(t *testing.T) {
	ctx := context.Background()
	st := migratedArrayDB(t)
	twoDataDiskArray(t, st)

	if err := st.AdvanceRemovalState(ctx, "/mnt/disk1", RemovalStateEvacuated, RemovalStateUnpooled, "rm-1"); !errors.Is(err, ErrRemovalStateMismatch) {
		t.Fatalf("AdvanceRemovalState from NULL = %v, want ErrRemovalStateMismatch", err)
	}
	if err := st.SetRemovalState(ctx, "/mnt/disk1", RemovalStateEvacuating, "evac-1"); err != nil {
		t.Fatalf("SetRemovalState(evacuating): %v", err)
	}
	if err := st.AdvanceRemovalState(ctx, "/mnt/disk1", RemovalStateEvacuated, RemovalStateUnpooled, "rm-1"); !errors.Is(err, ErrRemovalStateMismatch) {
		t.Fatalf("AdvanceRemovalState from evacuating = %v, want ErrRemovalStateMismatch", err)
	}
	if state, holder, _ := removalOf(t, st, "/mnt/disk1"); state != RemovalStateEvacuating || holder != "evac-1" {
		t.Fatalf("after refused advances: (%q, %q), want (evacuating, evac-1)", state, holder)
	}

	if err := st.SetRemovalState(ctx, "/mnt/disk1", RemovalStateEvacuated, "evac-1"); err != nil {
		t.Fatalf("SetRemovalState(evacuated): %v", err)
	}
	if err := st.AdvanceRemovalState(ctx, "/mnt/disk1", RemovalStateUnpooled, RemovalStateUnlisted, "rm-1"); !errors.Is(err, ErrRemovalStateMismatch) {
		t.Fatalf("AdvanceRemovalState evacuated->unlisted = %v, want ErrRemovalStateMismatch (no skipping unpooled)", err)
	}
	for i := 0; i < 2; i++ {
		if err := st.AdvanceRemovalState(ctx, "/mnt/disk1", RemovalStateEvacuated, RemovalStateUnpooled, "rm-1"); err != nil {
			t.Fatalf("AdvanceRemovalState evacuated->unpooled (call %d): %v", i+1, err)
		}
	}
	if err := st.AdvanceRemovalState(ctx, "/mnt/disk1", RemovalStateUnpooled, RemovalStateUnlisted, "rm-2"); err != nil {
		t.Fatalf("AdvanceRemovalState unpooled->unlisted: %v", err)
	}
	if state, holder, _ := removalOf(t, st, "/mnt/disk1"); state != RemovalStateUnlisted || holder != "rm-2" {
		t.Fatalf("after advancing: (%q, %q), want (unlisted, rm-2)", state, holder)
	}
	if err := st.AdvanceRemovalState(ctx, "/mnt/parity1", RemovalStateEvacuated, RemovalStateUnpooled, "rm-1"); !errors.Is(err, ErrRemovalStateMismatch) {
		t.Fatalf("AdvanceRemovalState on the parity disk = %v, want ErrRemovalStateMismatch", err)
	}
	if err := st.AdvanceRemovalState(ctx, "/mnt/disk1", RemovalStateUnpooled, RemovalStateUnlisted, ""); err == nil {
		t.Fatal("AdvanceRemovalState with no job id succeeded")
	}
}

// TestArrayStore_SetRemovalState_RefusesADiskLeavingTheArray proves an
// evacuation cannot pull a disk that has already left the pool (#358)
// back to "evacuating" — that would put it back into every pool mount.
func TestArrayStore_SetRemovalState_RefusesADiskLeavingTheArray(t *testing.T) {
	ctx := context.Background()
	st := migratedArrayDB(t)
	twoDataDiskArray(t, st)
	if err := st.SetRemovalState(ctx, "/mnt/disk1", RemovalStateEvacuated, "evac-1"); err != nil {
		t.Fatalf("SetRemovalState(evacuated): %v", err)
	}
	for _, to := range []string{RemovalStateUnpooled, RemovalStateUnlisted} {
		from := RemovalStateEvacuated
		if to == RemovalStateUnlisted {
			from = RemovalStateUnpooled
		}
		if err := st.AdvanceRemovalState(ctx, "/mnt/disk1", from, to, "rm-1"); err != nil {
			t.Fatalf("AdvanceRemovalState -> %s: %v", to, err)
		}
		for _, again := range []string{RemovalStateEvacuating, RemovalStateEvacuated} {
			if err := st.SetRemovalState(ctx, "/mnt/disk1", again, "evac-2"); !errors.Is(err, ErrDiskLeavingArray) {
				t.Fatalf("SetRemovalState(%s) on a %s disk = %v, want ErrDiskLeavingArray", again, to, err)
			}
		}
		if state, holder, _ := removalOf(t, st, "/mnt/disk1"); state != to || holder != "rm-1" {
			t.Fatalf("after the refused SetRemovalState: (%q, %q), want (%q, rm-1)", state, holder, to)
		}
	}
}

// TestArrayStore_DeleteUnlistedDataDisk_OnlyOnceUnlisted proves the row
// goes only once the disk is out of snapraid.conf (#358, doc 09 §4 step
// 9), and a second delete reports the slot gone.
func TestArrayStore_DeleteUnlistedDataDisk_OnlyOnceUnlisted(t *testing.T) {
	ctx := context.Background()
	st := migratedArrayDB(t)
	twoDataDiskArray(t, st)

	if err := st.DeleteUnlistedDataDisk(ctx, "/mnt/disk1"); !errors.Is(err, ErrRemovalStateMismatch) {
		t.Fatalf("DeleteUnlistedDataDisk not in removal = %v, want ErrRemovalStateMismatch", err)
	}
	if err := st.SetRemovalState(ctx, "/mnt/disk1", RemovalStateEvacuated, "evac-1"); err != nil {
		t.Fatalf("SetRemovalState: %v", err)
	}
	if err := st.AdvanceRemovalState(ctx, "/mnt/disk1", RemovalStateEvacuated, RemovalStateUnpooled, "rm-1"); err != nil {
		t.Fatalf("AdvanceRemovalState -> unpooled: %v", err)
	}
	if err := st.DeleteUnlistedDataDisk(ctx, "/mnt/disk1"); !errors.Is(err, ErrRemovalStateMismatch) {
		t.Fatalf("DeleteUnlistedDataDisk while unpooled = %v, want ErrRemovalStateMismatch", err)
	}
	if _, _, present := removalOf(t, st, "/mnt/disk1"); !present {
		t.Fatal("an unpooled disk's row was deleted")
	}
	if err := st.AdvanceRemovalState(ctx, "/mnt/disk1", RemovalStateUnpooled, RemovalStateUnlisted, "rm-1"); err != nil {
		t.Fatalf("AdvanceRemovalState -> unlisted: %v", err)
	}
	if err := st.DeleteUnlistedDataDisk(ctx, "/mnt/disk1"); err != nil {
		t.Fatalf("DeleteUnlistedDataDisk: %v", err)
	}
	if _, _, present := removalOf(t, st, "/mnt/disk1"); present {
		t.Fatal("the unlisted disk's row is still there")
	}
	if _, _, present := removalOf(t, st, "/mnt/disk2"); !present {
		t.Fatal("the other data disk's row was deleted too")
	}
	if err := st.DeleteUnlistedDataDisk(ctx, "/mnt/disk1"); !errors.Is(err, ErrArrayDiskNotFound) {
		t.Fatalf("second DeleteUnlistedDataDisk = %v, want ErrArrayDiskNotFound", err)
	}
	if mp, _, err := st.RemovingDisk(ctx); err != nil || mp != "" {
		t.Fatalf("RemovingDisk after the delete = (%q, %v), want none", mp, err)
	}
}

// TestArrayDisk_LeavingArrayAndLeftPool pins which removal states count
// as leaving the array (every one) and as out of the pool (only the
// disk_remove job's own, #366).
func TestArrayDisk_LeavingArrayAndLeftPool(t *testing.T) {
	for _, c := range []struct {
		state             string
		leaving, leftPool bool
	}{
		{"", false, false},
		{RemovalStateEvacuating, true, false},
		{RemovalStateEvacuated, true, false},
		{RemovalStateUnpooled, true, true},
		{RemovalStateUnlisted, true, true},
	} {
		d := ArrayDisk{Role: ArrayRoleData, RemovalState: c.state}
		if got := d.LeavingArray(); got != c.leaving {
			t.Errorf("removal state %q: LeavingArray = %t, want %t", c.state, got, c.leaving)
		}
		if got := d.LeftPool(); got != c.leftPool {
			t.Errorf("removal state %q: LeftPool = %t, want %t", c.state, got, c.leftPool)
		}
	}
}

func pendingArrayRows() ([]ArrayDisk, []RecordedDisk) {
	disks := []ArrayDisk{
		{Role: ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d1", Serial: "DATA1", Mountpoint: "/mnt/disk1", Size: 4 << 40, SizeSet: true, MountSource: "/dev/disk/by-id/ata-X_DATA1-part1"},
		{Role: ArrayRoleData, RoleIndex: 2, Device: "/dev/sdc", Filesystem: "ext4", FSUUID: "uuid-d2", Serial: "DATA2", Mountpoint: "/mnt/disk2", Size: 2 << 40, SizeSet: true},
	}
	recorded := []RecordedDisk{
		{Role: ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Size: 8 << 40, WWN: "wwn-p", Serial: "PAR1", ByIDName: "wwn-p"},
		{Role: ArrayRoleCache, RoleIndex: 1, Device: "/dev/nvme0n1p3", Size: 400 << 30, Serial: "NVME", ByIDName: "nvme-x-part3", PartUUID: "aaaa-bbbb"},
	}
	return disks, recorded
}

func TestArrayStore_PutPendingArray_RoundTripsAndFlagsTheArrayPending(t *testing.T) {
	ctx := context.Background()
	st := migratedArrayDB(t)
	if pending, err := st.MigrationPending(ctx); err != nil || pending {
		t.Fatalf("MigrationPending with no array = %v, %v, want false", pending, err)
	}
	disks, recorded := pendingArrayRows()
	settings := ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "50G", CreatedAt: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}
	if err := st.PutPendingArray(ctx, settings, disks, recorded); err != nil {
		t.Fatalf("PutPendingArray: %v", err)
	}
	if pending, err := st.MigrationPending(ctx); err != nil || !pending {
		t.Fatalf("MigrationPending = %v, %v, want true", pending, err)
	}
	gotSettings, gotDisks, err := st.GetArray(ctx)
	if err != nil || !gotSettings.MigrationPending {
		t.Fatalf("GetArray = %+v, %v", gotSettings, err)
	}
	if len(gotDisks) != 2 || gotDisks[0].MountSource != "/dev/disk/by-id/ata-X_DATA1-part1" || gotDisks[1].MountSource != "" {
		t.Fatalf("disks = %+v, want the mount source of disk 1 and none for disk 2", gotDisks)
	}
	gotRecorded, err := st.RecordedDisks(ctx)
	if err != nil || len(gotRecorded) != 2 || gotRecorded[0] != recorded[0] || gotRecorded[1] != recorded[1] {
		t.Fatalf("RecordedDisks = %+v, %v, want %+v", gotRecorded, err, recorded)
	}
	// The recorded disks are never array disks: nothing mounts them or lists them
	// in a pool.
	for _, d := range gotDisks {
		if d.Role != ArrayRoleData {
			t.Errorf("array disk %+v: a pending array holds only the adopted data disks", d)
		}
	}
	if err := st.PutPendingArray(ctx, settings, disks, recorded); !errors.Is(err, ErrArrayExists) {
		t.Errorf("a second PutPendingArray = %v, want ErrArrayExists", err)
	}
}

func TestArrayStore_PutPendingArray_IsOneTransaction(t *testing.T) {
	ctx := context.Background()
	st := migratedArrayDB(t)
	disks, recorded := pendingArrayRows()
	disks[1].FSUUID = disks[0].FSUUID
	if err := st.PutPendingArray(ctx, ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "50G", CreatedAt: time.Now()}, disks, recorded); err == nil {
		t.Fatal("PutPendingArray accepted two data disks with one filesystem UUID")
	}
	if exists, err := st.Exists(ctx); err != nil || exists {
		t.Errorf("Exists = %v, %v after a failed PutPendingArray: its settings were committed without its disks", exists, err)
	}
	if rec, err := st.RecordedDisks(ctx); err != nil || len(rec) != 0 {
		t.Errorf("recorded disks = %+v, %v after a failed PutPendingArray", rec, err)
	}
}

// A one-data-disk array whose parity disk carries a copy of the data disk's
// filesystem, and so its UUID, can be recorded: the parity disk is a recorded
// identity, not an array_disks row with a UUID of its own to be unique.
func TestArrayStore_PutPendingArray_AParityDiskMayCarryTheDataDisksUUID(t *testing.T) {
	ctx := context.Background()
	st := migratedArrayDB(t)
	disks, recorded := pendingArrayRows()
	disks = disks[:1]
	if err := st.PutPendingArray(ctx, ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "50G", CreatedAt: time.Now()}, disks, recorded[:1]); err != nil {
		t.Fatal(err)
	}
	if rec, err := st.RecordedDisks(ctx); err != nil || len(rec) != 1 || rec[0].Role != ArrayRoleParity {
		t.Errorf("recorded = %+v, %v", rec, err)
	}
}

func TestArrayStore_PutArray_IsNotPending(t *testing.T) {
	ctx := context.Background()
	st := migratedArrayDB(t)
	if err := st.PutArray(ctx, ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "50G", CreatedAt: time.Now()}, []ArrayDisk{
		{Role: ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "u", Mountpoint: "/mnt/disk1"},
	}); err != nil {
		t.Fatal(err)
	}
	if pending, err := st.MigrationPending(ctx); err != nil || pending {
		t.Errorf("MigrationPending of an array made by hand = %v, %v, want false", pending, err)
	}
}

// DeletePendingArray undoes a failed adoption and can never delete an array
// that is not a pending migration's.
func TestArrayStore_DeletePendingArray(t *testing.T) {
	ctx := context.Background()
	st := migratedArrayDB(t)
	if deleted, err := st.DeletePendingArray(ctx); err != nil || deleted {
		t.Fatalf("DeletePendingArray with no array = %v, %v", deleted, err)
	}

	disks, recorded := pendingArrayRows()
	if err := st.PutPendingArray(ctx, ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "50G", CreatedAt: time.Now()}, disks, recorded); err != nil {
		t.Fatal(err)
	}
	if deleted, err := st.DeletePendingArray(ctx); err != nil || !deleted {
		t.Fatalf("DeletePendingArray = %v, %v, want it deleted", deleted, err)
	}
	if exists, _ := st.Exists(ctx); exists {
		t.Error("the array is still there")
	}
	if rec, err := st.RecordedDisks(ctx); err != nil || len(rec) != 0 {
		t.Errorf("recorded disks = %+v, %v after the delete", rec, err)
	}
	if _, got, err := st.GetArray(ctx); !errors.Is(err, ErrNoArray) || len(got) != 0 {
		t.Errorf("GetArray = %+v, %v", got, err)
	}

	// An array created by hand, or one past the point of no return, is not
	// touched.
	if err := st.PutArray(ctx, ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "50G", CreatedAt: time.Now()}, []ArrayDisk{
		{Role: ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "u", Mountpoint: "/mnt/disk1"},
	}); err != nil {
		t.Fatal(err)
	}
	if deleted, err := st.DeletePendingArray(ctx); err != nil || deleted {
		t.Fatalf("DeletePendingArray of an ordinary array = %v, %v, want nothing deleted", deleted, err)
	}
	if _, got, err := st.GetArray(ctx); err != nil || len(got) != 1 {
		t.Errorf("the ordinary array's disks = %+v, %v after DeletePendingArray: they were deleted", got, err)
	}
}

// A replacement disk is formatted and mounted by UUID: the slot's old by-id
// binding does not follow it.
func TestArrayStore_ReplaceDataDisk_ClearsTheMountSource(t *testing.T) {
	ctx := context.Background()
	st := migratedArrayDB(t)
	disks, recorded := pendingArrayRows()
	if err := st.PutPendingArray(ctx, ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "50G", CreatedAt: time.Now()}, disks, recorded); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceDataDisk(ctx, "/mnt/disk1", ArrayDisk{Device: "/dev/sdz", Filesystem: "xfs", FSUUID: "uuid-new", Serial: "NEW"}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetDataDiskByMountpoint(ctx, "/mnt/disk1")
	if err != nil || got.MountSource != "" || got.FSUUID != "uuid-new" {
		t.Errorf("replaced disk = %+v, %v, want no mount source", got, err)
	}
}

func parityInitRows() []ArrayDisk {
	return []ArrayDisk{
		{Role: ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-new-parity", Serial: "PAR1", Mountpoint: "/mnt/parity1", Size: 8 << 40, SizeSet: true},
		{Role: ArrayRoleCache, RoleIndex: 1, Device: "/dev/nvme0n1p3", Filesystem: "xfs", FSUUID: "uuid-new-cache", Serial: "CAC1", Mountpoint: "/mnt/cache", Size: 500 << 30, SizeSet: true},
	}
}

// The point of no return is one write: the formatted parity and cache disks
// become array disks and the migration stops being pending together, and the
// record of the former disks stays until the migration is finished, so a run
// that stopped in between is finished and never starts over.
func TestArrayStore_RecordParityInit_IsOneStepThatLeavesTheMigrationFinishing(t *testing.T) {
	ctx := context.Background()
	st := migratedArrayDB(t)
	disks, recorded := pendingArrayRows()
	if err := st.PutPendingArray(ctx, ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "50G", CreatedAt: time.Now()}, disks, recorded); err != nil {
		t.Fatal(err)
	}
	if finishing, err := st.MigrationFinishing(ctx); err != nil || finishing {
		t.Fatalf("MigrationFinishing while pending = %v, %v", finishing, err)
	}
	if unfinished, err := st.MigrationUnfinished(ctx); err != nil || !unfinished {
		t.Fatalf("MigrationUnfinished while pending = %v, %v", unfinished, err)
	}

	if err := st.RecordParityInit(ctx, parityInitRows()); err != nil {
		t.Fatalf("RecordParityInit: %v", err)
	}
	settings, got, err := st.GetArray(ctx)
	if err != nil || settings.MigrationPending || len(got) != 4 {
		t.Fatalf("array = %+v %+v, %v, want the two data disks and the parity and cache, not pending", settings, got, err)
	}
	if pending, _ := st.MigrationPending(ctx); pending {
		t.Error("the migration is still pending")
	}
	if finishing, err := st.MigrationFinishing(ctx); err != nil || !finishing {
		t.Errorf("MigrationFinishing = %v, %v, want true until FinishMigration", finishing, err)
	}
	if unfinished, err := st.MigrationUnfinished(ctx); err != nil || !unfinished {
		t.Errorf("MigrationUnfinished = %v, %v, want true until FinishMigration", unfinished, err)
	}
	if rec, err := st.RecordedDisks(ctx); err != nil || len(rec) != 2 {
		t.Errorf("recorded = %+v, %v, want the record kept until the migration is finished", rec, err)
	}
	if err := st.RecordParityInit(ctx, parityInitRows()); !errors.Is(err, ErrMigrationNotPending) {
		t.Errorf("a second RecordParityInit = %v, want ErrMigrationNotPending", err)
	}
	if deleted, err := st.DeletePendingArray(ctx); err != nil || deleted {
		t.Errorf("DeletePendingArray after the point of no return = %v, %v, want nothing deleted", deleted, err)
	}

	if err := st.FinishMigration(ctx); err != nil {
		t.Fatalf("FinishMigration: %v", err)
	}
	if finishing, err := st.MigrationFinishing(ctx); err != nil || finishing {
		t.Errorf("MigrationFinishing after FinishMigration = %v, %v", finishing, err)
	}
	if unfinished, err := st.MigrationUnfinished(ctx); err != nil || unfinished {
		t.Errorf("MigrationUnfinished after FinishMigration = %v, %v", unfinished, err)
	}
	if err := st.FinishMigration(ctx); !errors.Is(err, ErrMigrationNotFinishing) {
		t.Errorf("a second FinishMigration = %v, want ErrMigrationNotFinishing", err)
	}
	if _, got, err := st.GetArray(ctx); err != nil || len(got) != 4 {
		t.Errorf("disks after FinishMigration = %+v, %v", got, err)
	}
}

// A row that cannot be written leaves the migration pending and no parity or
// cache row behind.
func TestArrayStore_RecordParityInit_AFailedRowLeavesTheMigrationPending(t *testing.T) {
	ctx := context.Background()
	st := migratedArrayDB(t)
	disks, recorded := pendingArrayRows()
	if err := st.PutPendingArray(ctx, ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "50G", CreatedAt: time.Now()}, disks, recorded); err != nil {
		t.Fatal(err)
	}
	rows := parityInitRows()
	rows[1].FSUUID = rows[0].FSUUID
	if err := st.RecordParityInit(ctx, rows); err == nil {
		t.Fatal("RecordParityInit accepted two rows with one filesystem UUID")
	}
	settings, got, err := st.GetArray(ctx)
	if err != nil || !settings.MigrationPending || len(got) != 2 {
		t.Fatalf("array = %+v %+v, %v, want it still pending with its two data disks", settings, got, err)
	}
	if finishing, _ := st.MigrationFinishing(ctx); finishing {
		t.Error("a failed record left the migration finishing")
	}
}

func TestArrayStore_RecordParityInit_NeverTouchesAnArrayThatIsNotAPendingMigration(t *testing.T) {
	ctx := context.Background()
	st := migratedArrayDB(t)
	if err := st.RecordParityInit(ctx, parityInitRows()); !errors.Is(err, ErrMigrationNotPending) {
		t.Fatalf("RecordParityInit with no array = %v, want ErrMigrationNotPending", err)
	}
	if err := st.PutArray(ctx, ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "50G", CreatedAt: time.Now()}, []ArrayDisk{
		{Role: ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "u", Mountpoint: "/mnt/disk1"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordParityInit(ctx, parityInitRows()); !errors.Is(err, ErrMigrationNotPending) {
		t.Fatalf("RecordParityInit on an ordinary array = %v, want ErrMigrationNotPending", err)
	}
	if _, got, _ := st.GetArray(ctx); len(got) != 1 {
		t.Errorf("disks = %+v, want the ordinary array unchanged", got)
	}
	if err := st.FinishMigration(ctx); !errors.Is(err, ErrMigrationNotFinishing) {
		t.Errorf("FinishMigration on an ordinary array = %v, want ErrMigrationNotFinishing", err)
	}
	if unfinished, err := st.MigrationUnfinished(ctx); err != nil || unfinished {
		t.Errorf("MigrationUnfinished of an ordinary array = %v, %v", unfinished, err)
	}
}

// The initial sync a finished migration owes is recorded by the statement that
// finishes it, so no stop between the two can leave a finished migration with
// nothing owed; it is not owed before, on an ordinary array, or with no array,
// and it stays owed until it is cleared.
func TestArrayStore_FinishMigrationRecordsTheInitialSyncOwedInTheSameStatement(t *testing.T) {
	ctx := context.Background()
	st := migratedArrayDB(t)
	if owed, err := st.InitialSyncOwed(ctx); err != nil || owed {
		t.Fatalf("InitialSyncOwed with no array = %v, %v, want false", owed, err)
	}
	if err := st.ClearInitialSyncOwed(ctx); err != nil {
		t.Fatalf("ClearInitialSyncOwed with no array: %v", err)
	}
	disks, recorded := pendingArrayRows()
	if err := st.PutPendingArray(ctx, ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "50G", CreatedAt: time.Now()}, disks, recorded); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordParityInit(ctx, parityInitRows()); err != nil {
		t.Fatal(err)
	}
	if owed, err := st.InitialSyncOwed(ctx); err != nil || owed {
		t.Fatalf("InitialSyncOwed before FinishMigration = %v, %v, want false", owed, err)
	}

	if err := st.FinishMigration(ctx); err != nil {
		t.Fatal(err)
	}
	if owed, err := st.InitialSyncOwed(ctx); err != nil || !owed {
		t.Fatalf("InitialSyncOwed after FinishMigration = %v, %v, want true", owed, err)
	}
	if err := st.FinishMigration(ctx); !errors.Is(err, ErrMigrationNotFinishing) {
		t.Fatalf("a second FinishMigration = %v", err)
	}
	if owed, _ := st.InitialSyncOwed(ctx); !owed {
		t.Error("a refused second FinishMigration cleared the owed sync")
	}

	if err := st.ClearInitialSyncOwed(ctx); err != nil {
		t.Fatal(err)
	}
	if owed, err := st.InitialSyncOwed(ctx); err != nil || owed {
		t.Errorf("InitialSyncOwed after ClearInitialSyncOwed = %v, %v, want false", owed, err)
	}
}

func TestArrayStore_AnOrdinaryArrayOwesNoInitialSync(t *testing.T) {
	ctx := context.Background()
	st := migratedArrayDB(t)
	if err := st.PutArray(ctx, ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "50G", CreatedAt: time.Now()}, []ArrayDisk{
		{Role: ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "u", Mountpoint: "/mnt/disk1"},
	}); err != nil {
		t.Fatal(err)
	}
	if owed, err := st.InitialSyncOwed(ctx); err != nil || owed {
		t.Errorf("InitialSyncOwed of an ordinary array = %v, %v, want false", owed, err)
	}
}

// Only FinishMigration writes the stamp the post-migration checklist reads as
// "the migration finished": an array with no migration, a pending adoption, one
// part-way through its point of no return, and one whose adoption was undone and
// replaced by a new array have none.
func TestArrayStore_MigrationFinishedAtIsWrittenOnlyByFinishMigration(t *testing.T) {
	ctx := context.Background()
	st := migratedArrayDB(t)
	notFinished := func(when string) {
		t.Helper()
		if at, finished, err := st.MigrationFinishedAt(ctx); err != nil || finished || !at.IsZero() {
			t.Fatalf("MigrationFinishedAt %s = %v %v %v, want none", when, at, finished, err)
		}
	}
	notFinished("with no array")

	disks, recorded := pendingArrayRows()
	settings := ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "50G", CreatedAt: time.Now()}
	if err := st.PutPendingArray(ctx, settings, disks, recorded); err != nil {
		t.Fatal(err)
	}
	notFinished("while the adoption is pending")
	if deleted, err := st.DeletePendingArray(ctx); err != nil || !deleted {
		t.Fatalf("DeletePendingArray = %v, %v", deleted, err)
	}
	notFinished("after the adoption was undone")
	if err := st.PutArray(ctx, settings, []ArrayDisk{{Role: ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "u", Mountpoint: "/mnt/disk1"}}); err != nil {
		t.Fatal(err)
	}
	notFinished("for an array created by hand after the undo")
	if err := st.FinishMigration(ctx); !errors.Is(err, ErrMigrationNotFinishing) {
		t.Fatalf("FinishMigration on an ordinary array = %v", err)
	}
	notFinished("after a refused FinishMigration")

	st = migratedArrayDB(t)
	if err := st.PutPendingArray(ctx, settings, disks, recorded); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordParityInit(ctx, parityInitRows()); err != nil {
		t.Fatal(err)
	}
	notFinished("part-way through the point of no return")

	before := time.Now().UTC().Truncate(time.Second)
	if err := st.FinishMigration(ctx); err != nil {
		t.Fatal(err)
	}
	at, finished, err := st.MigrationFinishedAt(ctx)
	if err != nil || !finished || at.Before(before) || at.After(time.Now()) || at.Location() != time.UTC {
		t.Fatalf("MigrationFinishedAt after FinishMigration = %v %v %v, want a UTC time since %v", at, finished, err, before)
	}
	if err := st.FinishMigration(ctx); !errors.Is(err, ErrMigrationNotFinishing) {
		t.Fatalf("a second FinishMigration = %v", err)
	}
	if again, _, _ := st.MigrationFinishedAt(ctx); !again.Equal(at) {
		t.Errorf("a refused second FinishMigration moved the stamp from %v to %v", at, again)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE array_settings SET migration_finished_at = 'yesterday'`); err != nil {
		t.Fatal(err)
	}
	if _, finished, err := st.MigrationFinishedAt(ctx); err == nil || finished {
		t.Errorf("MigrationFinishedAt of an unreadable stamp = %v, %v, want an error and never finished", finished, err)
	}
}

// Adding migration_finished_at keeps an array_settings row the table already
// held, every value intact, and it reads as a migration that never finished
// (D16): an array that finished its migration before the column existed has no
// stamp, so its checklist does not apply.
func TestArraySettingsMigration_FinishedAtKeepsAnExistingRow(t *testing.T) {
	ctx := context.Background()
	const newest = "20261005085001_add_migration_session_checklist.sql"
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(db)
	applyMigrationFiles(t, db, "", newest)
	const recorded = `[{"role":"parity","roleIndex":1,"device":"/dev/sdb","size":1000}]`
	if _, err := db.ExecContext(ctx, `INSERT INTO array_settings (id, create_policy, min_free_space, created_at, migration_pending, migration_recorded, initial_sync_owed)
		VALUES (1, 'lfs', '75G', '2026-10-01T08:00:00Z', 0, ?, 1)`, recorded); err != nil {
		t.Fatal(err)
	}
	applyMigrationFiles(t, db, newest, "")

	var policy, minFree, createdAt, rec, finishedAt string
	var pending, owed int
	if err := db.QueryRowContext(ctx, `SELECT create_policy, min_free_space, created_at, migration_pending, migration_recorded, initial_sync_owed, migration_finished_at FROM array_settings WHERE id = 1`).
		Scan(&policy, &minFree, &createdAt, &pending, &rec, &owed, &finishedAt); err != nil {
		t.Fatalf("reading the row after the migration: %v", err)
	}
	if policy != "lfs" || minFree != "75G" || createdAt != "2026-10-01T08:00:00Z" || pending != 0 || rec != recorded || owed != 1 || finishedAt != "" {
		t.Errorf("the row after the migration = %q %q %q %d %q %d %q, want every value kept and an empty migration_finished_at", policy, minFree, createdAt, pending, rec, owed, finishedAt)
	}
	if _, finished, err := NewArrayStore(db).MigrationFinishedAt(ctx); err != nil || finished {
		t.Errorf("MigrationFinishedAt of the migrated row = %v, %v, want not finished", finished, err)
	}
}
