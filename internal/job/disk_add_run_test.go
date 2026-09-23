package job

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"
)

func seedArray(t *testing.T, st *store.ArrayStore, parityDevice, dataDevice string) {
	t.Helper()
	err := st.PutArray(context.Background(), store.ArraySettings{
		CreatePolicy: "mfs",
		MinFreeSpace: "20G",
		CreatedAt:    time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: parityDevice, Filesystem: "xfs", FSUUID: "uuid-p", Mountpoint: "/mnt/parity1"},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: dataDevice, Filesystem: "xfs", FSUUID: "uuid-d1", Mountpoint: "/mnt/disk1"},
	})
	if err != nil {
		t.Fatalf("seedArray PutArray: %v", err)
	}
}

func registerDiskAdd(t *testing.T, s *Scheduler, p disk.Provider, r disk.Runner, st *store.ArrayStore, genRoot string, mounter disk.UnitMounter) {
	t.Helper()
	s.registry.Register(TypeDiskAdd, false, RunDiskAdd(DiskAddDeps{
		Provider:  p,
		Runner:    r,
		Store:     st,
		Generator: config.NewGenerator(genRoot),
		Mounter:   mounter,
		Now:       func() time.Time { return time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC) },
	}))
}

func TestRunDiskAdd_AddsANewDataDiskAndRegeneratesConfig(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	st := store.NewArrayStore(newTestDB(t))
	genRoot := t.TempDir()
	mounter := disk.NewFakeMounter()

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	p.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB})
	p.AddDisk("/dev/sdc", disk.Disk{Size: 4 * disk.TB})
	r := disk.NewFakeRunner()
	scriptFilesystemUUID(r, "/dev/sdc", "uuid-d2")

	seedArray(t, st, "/dev/sda", "/dev/sdb")
	registerDiskAdd(t, s, p, r, st, genRoot, mounter)

	newDisk := disk.AssignedDisk{Device: "/dev/sdc", Filesystem: disk.XFS}
	params := DiskAddParams{
		Confirmation: SingleDiskConfirmation(newDisk),
		Disk:         newDisk,
		Sizes:        map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdb": 4 * disk.TB, "/dev/sdc": 4 * disk.TB},
	}

	j, err := s.Submit(ctx, TypeDiskAdd, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}

	if _, ok := p.FormattedAs("/dev/sdc"); !ok {
		t.Fatal("new disk was not formatted")
	}
	if _, ok := p.FormattedAs("/dev/sdb"); ok {
		t.Fatal("existing data disk was reformatted — no rebuild means disk1 is untouched")
	}

	_, disks, err := st.GetArray(ctx)
	if err != nil {
		t.Fatalf("GetArray: %v", err)
	}
	if len(disks) != 3 {
		t.Fatalf("len(disks) = %d, want 3", len(disks))
	}
	var added *store.ArrayDisk
	for i := range disks {
		if disks[i].Mountpoint == "/mnt/disk2" {
			added = &disks[i]
		}
	}
	if added == nil {
		t.Fatalf("no disk at /mnt/disk2: %+v", disks)
	}
	if added.Device != "/dev/sdc" || added.FSUUID != "uuid-d2" || added.RoleIndex != 2 {
		t.Fatalf("added disk = %+v", added)
	}

	mountedDisk2 := false
	for _, m := range mounter.Mounts {
		if m.Where == "/mnt/disk2" {
			mountedDisk2 = true
		}
	}
	if !mountedDisk2 {
		t.Fatalf("mounter.Mounts = %+v, want /mnt/disk2 among them", mounter.Mounts)
	}
}

func TestRunDiskAdd_WrongConfirmationFormatsNothing(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	st := store.NewArrayStore(newTestDB(t))
	genRoot := t.TempDir()
	mounter := disk.NewFakeMounter()

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	p.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB})
	p.AddDisk("/dev/sdc", disk.Disk{Size: 4 * disk.TB})
	seedArray(t, st, "/dev/sda", "/dev/sdb")
	registerDiskAdd(t, s, p, disk.NewFakeRunner(), st, genRoot, mounter)

	params := DiskAddParams{
		Confirmation: "erase /dev/sdc",
		Disk:         disk.AssignedDisk{Device: "/dev/sdc", Filesystem: disk.XFS},
		Sizes:        map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdb": 4 * disk.TB, "/dev/sdc": 4 * disk.TB},
	}
	j, err := s.Submit(ctx, TypeDiskAdd, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s (%s), want failed", finished.Status, finished.ErrorMessage)
	}
	if _, ok := p.FormattedAs("/dev/sdc"); ok {
		t.Fatal("wrong confirmation formatted /dev/sdc")
	}
	_, disks, err := st.GetArray(ctx)
	if err != nil {
		t.Fatalf("GetArray: %v", err)
	}
	if len(disks) != 2 {
		t.Fatalf("len(disks) = %d, want 2 (nothing added)", len(disks))
	}
}

func TestRunDiskAdd_Q20RefusesADiskLargerThanParity(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	st := store.NewArrayStore(newTestDB(t))
	genRoot := t.TempDir()
	mounter := disk.NewFakeMounter()

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 4 * disk.TB})
	p.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB})
	p.AddDisk("/dev/sdc", disk.Disk{Size: 8 * disk.TB})
	seedArray(t, st, "/dev/sda", "/dev/sdb")
	registerDiskAdd(t, s, p, disk.NewFakeRunner(), st, genRoot, mounter)

	newDisk := disk.AssignedDisk{Device: "/dev/sdc", Filesystem: disk.XFS}
	params := DiskAddParams{
		Confirmation: SingleDiskConfirmation(newDisk),
		Disk:         newDisk,
		// The new data disk (8 TB) is larger than the array's own parity
		// (4 TB) — Q20 must refuse this before anything is formatted.
		Sizes: map[string]int64{"/dev/sda": 4 * disk.TB, "/dev/sdb": 4 * disk.TB, "/dev/sdc": 8 * disk.TB},
	}
	j, err := s.Submit(ctx, TypeDiskAdd, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s (%s), want failed", finished.Status, finished.ErrorMessage)
	}
	if !strings.Contains(finished.ErrorMessage, "parity disk must be at least as large") {
		t.Fatalf("ErrorMessage = %q, want a Q20 refusal", finished.ErrorMessage)
	}
	if _, ok := p.FormattedAs("/dev/sdc"); ok {
		t.Fatal("Q20 violation formatted the disk anyway")
	}
	_, disks, err := st.GetArray(ctx)
	if err != nil {
		t.Fatalf("GetArray: %v", err)
	}
	if len(disks) != 2 {
		t.Fatalf("len(disks) = %d, want 2 (nothing added)", len(disks))
	}
}

func TestRunDiskAdd_DuplicateDeviceRefused(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	st := store.NewArrayStore(newTestDB(t))
	genRoot := t.TempDir()
	mounter := disk.NewFakeMounter()

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	p.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB})
	seedArray(t, st, "/dev/sda", "/dev/sdb")
	registerDiskAdd(t, s, p, disk.NewFakeRunner(), st, genRoot, mounter)

	// /dev/sdb is already the array's own data1 device.
	dup := disk.AssignedDisk{Device: "/dev/sdb", Filesystem: disk.XFS}
	params := DiskAddParams{
		Confirmation: SingleDiskConfirmation(dup),
		Disk:         dup,
		Sizes:        map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdb": 4 * disk.TB},
	}
	j, err := s.Submit(ctx, TypeDiskAdd, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s (%s), want failed", finished.Status, finished.ErrorMessage)
	}
	if !strings.Contains(finished.ErrorMessage, "assigned more than one role") {
		t.Fatalf("ErrorMessage = %q, want a duplicate-device refusal", finished.ErrorMessage)
	}
}

func TestRunDiskAdd_KnownIdentityRefused(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	st := store.NewArrayStore(newTestDB(t))
	genRoot := t.TempDir()
	mounter := disk.NewFakeMounter()

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	// /dev/sdd is a different /dev/sdX path, but the same physical disk
	// (WWN wwn-d1) as the array's own data1 — a renumbered member, not a
	// fresh disk (Q21, finding 3).
	p.AddDisk("/dev/sdd", disk.Disk{Size: 4 * disk.TB, WWN: "wwn-d1"})
	if err := st.PutArray(ctx, store.ArraySettings{
		CreatePolicy: "mfs", MinFreeSpace: "20G", CreatedAt: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", Mountpoint: "/mnt/parity1"},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d1", WWN: "wwn-d1", Mountpoint: "/mnt/disk1"},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	registerDiskAdd(t, s, p, disk.NewFakeRunner(), st, genRoot, mounter)

	same := disk.AssignedDisk{Device: "/dev/sdd", Filesystem: disk.XFS, WWN: "wwn-d1"}
	params := DiskAddParams{
		Confirmation: SingleDiskConfirmation(same),
		Disk:         same,
		Sizes:        map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdd": 4 * disk.TB},
	}
	j, err := s.Submit(ctx, TypeDiskAdd, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s (%s), want failed — /dev/sdd is already an array member by identity", finished.Status, finished.ErrorMessage)
	}
	if !strings.Contains(finished.ErrorMessage, "already a member") {
		t.Fatalf("ErrorMessage = %q, want ErrDiskAlreadyMember", finished.ErrorMessage)
	}
	if _, ok := p.FormattedAs("/dev/sdd"); ok {
		t.Fatal("a known-identity device was formatted anyway")
	}
}

// TestRunDiskAdd_WeakIdentityFSUUIDRefused is finding 1's own regression
// test: a weak-identity disk — no WWN or serial at all, as every disk in
// the loop-device lab is (doc 06 §3) — carries nothing refuseKnownIdentity
// can compare by WWN/serial alone, so a renumbered array member (its own
// filesystem still on it) must be recognised by filesystem UUID instead,
// the same fallback matchArrayDisk and ConfirmReplacementTargetAbsent
// already use.
func TestRunDiskAdd_WeakIdentityFSUUIDRefused(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	st := store.NewArrayStore(newTestDB(t))
	genRoot := t.TempDir()
	mounter := disk.NewFakeMounter()

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	// /dev/vdd is a different /dev/sdX path than the array's own weak-
	// identity data1 (/dev/vdc), but the same physical disk: no WWN or
	// serial, only its own filesystem UUID in common — a renumbered
	// member, not a fresh disk.
	p.AddDisk("/dev/vdd", disk.Disk{Size: 4 * disk.TB, WeakIdentity: true, FSUUID: "uuid-d1"})
	if err := st.PutArray(ctx, store.ArraySettings{
		CreatePolicy: "mfs", MinFreeSpace: "20G", CreatedAt: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", Mountpoint: "/mnt/parity1"},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/vdc", Filesystem: "xfs", FSUUID: "uuid-d1", WeakIdentity: true, Mountpoint: "/mnt/disk1"},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	registerDiskAdd(t, s, p, disk.NewFakeRunner(), st, genRoot, mounter)

	same := disk.AssignedDisk{Device: "/dev/vdd", Filesystem: disk.XFS, WeakIdentity: true, FSUUID: "uuid-d1"}
	params := DiskAddParams{
		Confirmation: SingleDiskConfirmation(same),
		Disk:         same,
		// Every device Validate needs a size for is present, so a
		// reverted fix would fall through to a real format instead of
		// masking the vulnerability behind an unrelated ErrMissingSize.
		Sizes: map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/vdc": 4 * disk.TB, "/dev/vdd": 4 * disk.TB},
	}
	j, err := s.Submit(ctx, TypeDiskAdd, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s (%s), want failed — /dev/vdd is already an array member by filesystem UUID", finished.Status, finished.ErrorMessage)
	}
	if !strings.Contains(finished.ErrorMessage, "already a member") {
		t.Fatalf("ErrorMessage = %q, want ErrDiskAlreadyMember", finished.ErrorMessage)
	}
	if _, ok := p.FormattedAs("/dev/vdd"); ok {
		t.Fatal("a weak-identity disk matching a stored member's filesystem UUID was formatted anyway")
	}
	_, disks, err := st.GetArray(ctx)
	if err != nil {
		t.Fatalf("GetArray: %v", err)
	}
	if len(disks) != 2 {
		t.Fatalf("len(disks) = %d, want 2 (nothing added)", len(disks))
	}
}

// TestRunDiskAdd_QueuedBehindStorageJobIsCheckedAgainstProviderAtRunTime is
// finding 1's own regression test (#288 fix round 4): the job's own params
// carry no FSUUID at all — exactly what the addDisk handler submits for a
// fresh USB-bridge disk with no filesystem yet (weak identity) — and the
// physical disk actually at that device path changes while the job sits
// queued behind a running storage-class job (ClassTopology conflicts with
// every storage class, doc 01 §4). Unlike
// TestRunDiskAdd_WeakIdentityFSUUIDRefused, which puts the stored member's
// FSUUID straight into params and so never exercises the submit-time data
// flow, this only a fresh Provider.List read at run time can catch — here,
// via confirmTargetIdentityUnchanged's own unconditional FSUUID comparison
// (round 6): the submitted FSUUID was empty, the run-time one is not, so
// the job is refused before refuseKnownIdentity's membership check ever
// runs.
func TestRunDiskAdd_QueuedBehindStorageJobIsCheckedAgainstProviderAtRunTime(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	st := store.NewArrayStore(newTestDB(t))
	genRoot := t.TempDir()
	mounter := disk.NewFakeMounter()

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	// At submit time this is a fresh, unformatted USB-bridge disk: weak
	// identity, no filesystem yet, so the params captured here carry no
	// FSUUID at all.
	p.AddDisk("/dev/sdg", disk.Disk{Size: 4 * disk.TB, WeakIdentity: true})
	if err := st.PutArray(ctx, store.ArraySettings{
		CreatePolicy: "mfs", MinFreeSpace: "20G", CreatedAt: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", Mountpoint: "/mnt/parity1"},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/vdc", Filesystem: "xfs", FSUUID: "uuid-d1", WeakIdentity: true, Mountpoint: "/mnt/disk1"},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	registerDiskAdd(t, s, p, disk.NewFakeRunner(), st, genRoot, mounter)

	syncStarted, syncRelease := registerBlocking(s, TypeSync, false)
	sync, err := s.Submit(ctx, TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit(sync): %v", err)
	}
	<-syncStarted

	submitted := disk.AssignedDisk{Device: "/dev/sdg", Filesystem: disk.XFS, WeakIdentity: true}
	params := DiskAddParams{
		Confirmation: SingleDiskConfirmation(submitted),
		Disk:         submitted,
		Sizes:        map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/vdc": 4 * disk.TB, "/dev/sdg": 4 * disk.TB},
	}
	j, err := s.Submit(ctx, TypeDiskAdd, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit(disk_add): %v", err)
	}
	if j.Status != StatusQueued {
		t.Fatalf("disk_add Status = %s, want queued behind the running sync (ClassTopology excludes every storage class)", j.Status)
	}

	// While disk_add sits queued, the fresh USB disk is dropped and
	// disk1 — also weak identity — comes back at this exact same
	// /dev/sdg path (a USB bus reset, doc 06 §3's own weak-identity
	// scenario).
	p.AddDisk("/dev/sdg", disk.Disk{Size: 4 * disk.TB, WeakIdentity: true, FSUUID: "uuid-d1"})

	close(syncRelease)
	waitSucceeded(t, s, sync.ID)

	finished := await(t, s, j.ID)
	if _, ok := p.FormattedAs("/dev/sdg"); ok {
		t.Fatal("a submit-time FSUUID of \"\" let a run-time-matching disk be formatted anyway")
	}
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s (%s), want failed — /dev/sdg now holds disk1's own filesystem", finished.Status, finished.ErrorMessage)
	}
	if !strings.Contains(finished.ErrorMessage, "no longer matches what was confirmed") {
		t.Fatalf("ErrorMessage = %q, want ErrDiskIdentityDrifted", finished.ErrorMessage)
	}
}

// TestRunDiskAdd_QueuedBehindStorageJobRefusesNonMemberDiskWithFilesystem is
// finding 1's own regression test (#288 fix round 6): the submitted target
// is identity-less — WeakIdentity true, no WWN, Serial, ByIDName or FSUUID
// at all, as every disk in the loop-device lab is (doc 06 §3) and as a
// virtio disk with no serial configured is in a VM guest (Q21) — and the
// plan and confirm dialog showed it as a fresh disk with no filesystem.
// While the job sits queued behind a running storage-class job (ClassTopology
// conflicts with every storage class, doc 01 §4), a hypervisor detach and
// reattach, or a bus reset, renumbers the disks: another identity-less disk
// now sits at the target's path, holding data of its own, and it is not an
// array member at all — so refuseKnownIdentity's own membership check,
// which only ever compares against disks.ArrayDisk rows, has nothing to
// catch it on. f5453e5 formatted it: its FSUUID comparison ran only when
// the submitted FSUUID was non-empty, so an empty submitted FSUUID was
// silently overwritten with whatever the run-time disk's FSUUID was, and a
// non-member disk was never checked against refuseKnownIdentity at all.
func TestRunDiskAdd_QueuedBehindStorageJobRefusesNonMemberDiskWithFilesystem(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	st := store.NewArrayStore(newTestDB(t))
	genRoot := t.TempDir()
	mounter := disk.NewFakeMounter()

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	// At submit time this is a fresh, unformatted disk with no by-id link
	// at all: weak identity, no filesystem yet, so the params captured
	// here carry no FSUUID.
	p.AddDisk("/dev/vdg", disk.Disk{Size: 4 * disk.TB, WeakIdentity: true})
	if err := st.PutArray(ctx, store.ArraySettings{
		CreatePolicy: "mfs", MinFreeSpace: "20G", CreatedAt: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", Mountpoint: "/mnt/parity1"},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/vdc", Filesystem: "xfs", FSUUID: "uuid-d1", WeakIdentity: true, Mountpoint: "/mnt/disk1"},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	registerDiskAdd(t, s, p, disk.NewFakeRunner(), st, genRoot, mounter)

	syncStarted, syncRelease := registerBlocking(s, TypeSync, false)
	sync, err := s.Submit(ctx, TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit(sync): %v", err)
	}
	<-syncStarted

	submitted := disk.AssignedDisk{Device: "/dev/vdg", Filesystem: disk.XFS, WeakIdentity: true}
	params := DiskAddParams{
		Confirmation: SingleDiskConfirmation(submitted),
		Disk:         submitted,
		Sizes:        map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/vdc": 4 * disk.TB, "/dev/vdg": 4 * disk.TB},
	}
	j, err := s.Submit(ctx, TypeDiskAdd, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit(disk_add): %v", err)
	}
	if j.Status != StatusQueued {
		t.Fatalf("disk_add Status = %s, want queued behind the running sync (ClassTopology excludes every storage class)", j.Status)
	}

	// While disk_add sits queued, a disk holding the user's own data — not
	// an array member, not even registered — comes to sit at this exact
	// same /dev/vdg path.
	p.AddDisk("/dev/vdg", disk.Disk{Size: 4 * disk.TB, WeakIdentity: true, FSUUID: "uuid-unrelated"})

	close(syncRelease)
	waitSucceeded(t, s, sync.ID)

	finished := await(t, s, j.ID)
	if _, ok := p.FormattedAs("/dev/vdg"); ok {
		t.Fatal("a disk that gained a filesystem while the job was queued was formatted anyway — it is not an array member, so only the identity-drift check, not refuseKnownIdentity, can catch it")
	}
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s (%s), want failed — /dev/vdg now holds a filesystem the confirmed plan never saw", finished.Status, finished.ErrorMessage)
	}
	if !strings.Contains(finished.ErrorMessage, "no longer matches what was confirmed") {
		t.Fatalf("ErrorMessage = %q, want ErrDiskIdentityDrifted", finished.ErrorMessage)
	}
	_, disks, err := st.GetArray(ctx)
	if err != nil {
		t.Fatalf("GetArray: %v", err)
	}
	if len(disks) != 2 {
		t.Fatalf("len(disks) = %d, want 2 (nothing added)", len(disks))
	}
}

// TestRunDiskAdd_QueuedBehindStorageJobRefusesRenumberedStrongIdentityMember
// is the regression test for a weak, empty-identity submission whose path
// renumbers onto a serial-bearing array member while the job sits queued
// behind a running storage-class job (ClassTopology conflicts with every
// storage class, doc 01 §4). Unlike
// TestRunDiskAdd_QueuedBehindStorageJobIsCheckedAgainstProviderAtRunTime,
// where the renumbered disk is also weak identity and so still caught by
// refuseKnownIdentity's own FSUUID fallback, this one's renumbered disk
// carries a real serial: refuseKnownIdentity's own WWN/serial branch has
// nothing to compare a submitted empty WWN/serial against, and its FSUUID
// fallback requires WeakIdentity, so only confirmTargetIdentityUnchanged's
// fail-closed comparison — which refuses on the WeakIdentity and Serial
// fields differing, not just FSUUID — catches it.
func TestRunDiskAdd_QueuedBehindStorageJobRefusesRenumberedStrongIdentityMember(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	st := store.NewArrayStore(newTestDB(t))
	genRoot := t.TempDir()
	mounter := disk.NewFakeMounter()

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	// At submit time this is a fresh, unformatted USB-bridge disk: weak
	// identity, no filesystem yet, so the params captured here carry no
	// WWN, serial or FSUUID at all.
	p.AddDisk("/dev/sdg", disk.Disk{Size: 4 * disk.TB, WeakIdentity: true})
	if err := st.PutArray(ctx, store.ArraySettings{
		CreatePolicy: "mfs", MinFreeSpace: "20G", CreatedAt: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", Mountpoint: "/mnt/parity1"},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/vdc", Filesystem: "xfs", FSUUID: "uuid-d1", WeakIdentity: true, Mountpoint: "/mnt/disk1"},
		// disk2 is a strong-identity member (a SATA/SAS disk, or a USB
		// enclosure that passes a serial through) — the common case for an
		// array member, unlike the weak-identity fixture above.
		{Role: store.ArrayRoleData, RoleIndex: 2, Device: "/dev/sdh", Filesystem: "xfs", FSUUID: "uuid-d2", Serial: "serial-d2", Mountpoint: "/mnt/disk2"},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	registerDiskAdd(t, s, p, disk.NewFakeRunner(), st, genRoot, mounter)

	syncStarted, syncRelease := registerBlocking(s, TypeSync, false)
	sync, err := s.Submit(ctx, TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit(sync): %v", err)
	}
	<-syncStarted

	submitted := disk.AssignedDisk{Device: "/dev/sdg", Filesystem: disk.XFS, WeakIdentity: true}
	params := DiskAddParams{
		Confirmation: SingleDiskConfirmation(submitted),
		Disk:         submitted,
		Sizes:        map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/vdc": 4 * disk.TB, "/dev/sdh": 4 * disk.TB, "/dev/sdg": 4 * disk.TB},
	}
	j, err := s.Submit(ctx, TypeDiskAdd, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit(disk_add): %v", err)
	}
	if j.Status != StatusQueued {
		t.Fatalf("disk_add Status = %s, want queued behind the running sync (ClassTopology excludes every storage class)", j.Status)
	}

	// While disk_add sits queued, a bus reset drops the fresh USB disk and
	// disk2 — a strong-identity member — comes back at this exact same
	// /dev/sdg path.
	p.AddDisk("/dev/sdg", disk.Disk{Size: 4 * disk.TB, Serial: "serial-d2", FSUUID: "uuid-d2"})

	close(syncRelease)
	waitSucceeded(t, s, sync.ID)

	finished := await(t, s, j.ID)
	if _, ok := p.FormattedAs("/dev/sdg"); ok {
		t.Fatal("a renumbered strong-identity array member was formatted anyway")
	}
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s (%s), want failed — /dev/sdg now holds disk2's own filesystem", finished.Status, finished.ErrorMessage)
	}
	if !strings.Contains(finished.ErrorMessage, "no longer matches what was confirmed") {
		t.Fatalf("ErrorMessage = %q, want ErrDiskIdentityDrifted", finished.ErrorMessage)
	}
	_, disks, err := st.GetArray(ctx)
	if err != nil {
		t.Fatalf("GetArray: %v", err)
	}
	if len(disks) != 3 {
		t.Fatalf("len(disks) = %d, want 3 (nothing added)", len(disks))
	}
}

func TestRunDiskAdd_RefusesWithoutAnArray(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	st := store.NewArrayStore(newTestDB(t))
	genRoot := t.TempDir()
	mounter := disk.NewFakeMounter()

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sdc", disk.Disk{Size: 4 * disk.TB})
	registerDiskAdd(t, s, p, disk.NewFakeRunner(), st, genRoot, mounter)

	newDisk := disk.AssignedDisk{Device: "/dev/sdc", Filesystem: disk.XFS}
	params := DiskAddParams{
		Confirmation: SingleDiskConfirmation(newDisk),
		Disk:         newDisk,
		Sizes:        map[string]int64{"/dev/sdc": 4 * disk.TB},
	}
	j, err := s.Submit(ctx, TypeDiskAdd, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s (%s), want failed", finished.Status, finished.ErrorMessage)
	}
	if !strings.Contains(finished.ErrorMessage, "no array topology") {
		t.Fatalf("ErrorMessage = %q, want ErrNoArray", finished.ErrorMessage)
	}
}
