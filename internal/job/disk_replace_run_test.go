package job

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/store"
)

func seedTwoDataDiskArray(t *testing.T, st *store.ArrayStore) {
	t.Helper()
	err := st.PutArray(context.Background(), store.ArraySettings{
		CreatePolicy: "mfs",
		MinFreeSpace: "20G",
		CreatedAt:    time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", Mountpoint: "/mnt/parity1"},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d1", Mountpoint: "/mnt/disk1"},
		{Role: store.ArrayRoleData, RoleIndex: 2, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "uuid-d2", Mountpoint: "/mnt/disk2"},
	})
	if err != nil {
		t.Fatalf("seedTwoDataDiskArray PutArray: %v", err)
	}
}

// scriptMountedUUID scripts findmnt for mountpoint the way RunDiskReplace's
// own pre-fix check reads it (disk.MountedUUID) — every test that reaches
// that check needs it scripted, or MountedUUID reports no filesystem found.
func scriptMountedUUID(r *disk.FakeRunner, mountpoint, uuid string) {
	r.Script("findmnt", []string{"-n", "-o", "UUID", mountpoint}, []byte(uuid+"\n"), nil)
}

func registerDiskReplace(t *testing.T, s *Scheduler, p disk.Provider, r disk.Runner, st *store.ArrayStore, genRoot string, mounter disk.UnitMounter, eng parity.Engine) {
	t.Helper()
	s.registry.Register(TypeDiskReplace, false, RunDiskReplace(DiskReplaceDeps{
		Provider:  p,
		Runner:    r,
		Store:     st,
		Generator: config.NewGenerator(genRoot),
		Mounter:   mounter,
		Parity:    eng,
		Now:       func() time.Time { return time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC) },
	}))
}

func TestRunDiskReplace_ReplacesADataDiskAndRunsFix(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	st := store.NewArrayStore(newTestDB(t))
	genRoot := t.TempDir()
	mounter := disk.NewFakeMounter()

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	p.AddDisk("/dev/sdz", disk.Disk{Size: 4 * disk.TB})
	r := disk.NewFakeRunner()
	scriptFilesystemUUID(r, "/dev/sdz", "uuid-new")
	scriptMountedUUID(r, "/mnt/disk1", "uuid-new")

	seedTwoDataDiskArray(t, st)

	eng := newRecordingEngine()
	eng.SetStatus(parity.ParityStatus{DataMounts: map[string]string{"d1": "/mnt/disk1", "d2": "/mnt/disk2"}})
	eng.ScriptFix([]parity.Progress{{}}, nil)

	registerDiskReplace(t, s, p, r, st, genRoot, mounter, eng)

	replacement := disk.AssignedDisk{Device: "/dev/sdz", Filesystem: disk.XFS}
	params := DiskReplaceParams{
		Confirmation: SingleDiskConfirmation(replacement),
		Mountpoint:   "/mnt/disk1",
		Disk:         replacement,
		Sizes:        map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdc": 4 * disk.TB, "/dev/sdz": 4 * disk.TB},
	}
	j, err := s.Submit(ctx, TypeDiskReplace, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}

	if _, ok := p.FormattedAs("/dev/sdz"); !ok {
		t.Fatal("replacement disk was not formatted")
	}

	got, err := st.GetDataDiskByMountpoint(ctx, "/mnt/disk1")
	if err != nil {
		t.Fatalf("GetDataDiskByMountpoint: %v", err)
	}
	if got.Device != "/dev/sdz" || got.FSUUID != "uuid-new" || got.RoleIndex != 1 {
		t.Fatalf("replaced disk = %+v", got)
	}

	other, err := st.GetDataDiskByMountpoint(ctx, "/mnt/disk2")
	if err != nil {
		t.Fatalf("GetDataDiskByMountpoint(/mnt/disk2): %v", err)
	}
	if other.Device != "/dev/sdc" {
		t.Fatalf("unrelated disk2 changed: %+v", other)
	}

	_, _, _, lastFix := eng.snapshot()
	if lastFix.Disk != "d1" {
		t.Fatalf("Fix ran against disk %q, want d1 (the replaced slot's own SnapRAID label)", lastFix.Disk)
	}
}

func TestRunDiskReplace_WrongConfirmationFormatsNothing(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	st := store.NewArrayStore(newTestDB(t))
	genRoot := t.TempDir()
	mounter := disk.NewFakeMounter()

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	p.AddDisk("/dev/sdz", disk.Disk{Size: 4 * disk.TB})
	seedTwoDataDiskArray(t, st)

	eng := newRecordingEngine()
	registerDiskReplace(t, s, p, disk.NewFakeRunner(), st, genRoot, mounter, eng)

	params := DiskReplaceParams{
		Confirmation: "erase /dev/sdz",
		Mountpoint:   "/mnt/disk1",
		Disk:         disk.AssignedDisk{Device: "/dev/sdz", Filesystem: disk.XFS},
		Sizes:        map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdc": 4 * disk.TB, "/dev/sdz": 4 * disk.TB},
	}
	j, err := s.Submit(ctx, TypeDiskReplace, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s (%s), want failed", finished.Status, finished.ErrorMessage)
	}
	if _, ok := p.FormattedAs("/dev/sdz"); ok {
		t.Fatal("wrong confirmation formatted /dev/sdz")
	}
	got, err := st.GetDataDiskByMountpoint(ctx, "/mnt/disk1")
	if err != nil {
		t.Fatalf("GetDataDiskByMountpoint: %v", err)
	}
	if got.Device != "/dev/sdb" {
		t.Fatalf("disk1 device = %q, want unchanged /dev/sdb", got.Device)
	}
}

// TestRunDiskReplace_RefusesASlotInRemoval covers #368: an evacuation
// queued ahead of a replace of the same slot marks it for removal before
// the replace job runs. store.ReplaceDataDisk never touches removal_state,
// so without this refusal the replacement disk would silently inherit
// removing/removed as its own state the moment it is adopted.
func TestRunDiskReplace_RefusesASlotInRemoval(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	st := store.NewArrayStore(newTestDB(t))
	genRoot := t.TempDir()
	mounter := disk.NewFakeMounter()

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	p.AddDisk("/dev/sdz", disk.Disk{Size: 4 * disk.TB})
	seedTwoDataDiskArray(t, st)
	if err := st.SetRemovalState(ctx, "/mnt/disk1", store.RemovalStateEvacuating, "evacuation-job"); err != nil {
		t.Fatalf("SetRemovalState: %v", err)
	}

	eng := newRecordingEngine()
	registerDiskReplace(t, s, p, disk.NewFakeRunner(), st, genRoot, mounter, eng)

	replacement := disk.AssignedDisk{Device: "/dev/sdz", Filesystem: disk.XFS}
	params := DiskReplaceParams{
		Confirmation: SingleDiskConfirmation(replacement),
		Mountpoint:   "/mnt/disk1",
		Disk:         replacement,
		Sizes:        map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdc": 4 * disk.TB, "/dev/sdz": 4 * disk.TB},
	}
	j, err := s.Submit(ctx, TypeDiskReplace, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s (%s), want failed", finished.Status, finished.ErrorMessage)
	}
	if !strings.Contains(finished.ErrorMessage, "/mnt/disk1") || !strings.Contains(finished.ErrorMessage, "leaving the array") {
		t.Fatalf("ErrorMessage = %q, want a removal-state refusal naming /mnt/disk1", finished.ErrorMessage)
	}
	if _, ok := p.FormattedAs("/dev/sdz"); ok {
		t.Fatal("a slot in removal formatted the replacement disk anyway")
	}
	got, err := st.GetDataDiskByMountpoint(ctx, "/mnt/disk1")
	if err != nil {
		t.Fatalf("GetDataDiskByMountpoint: %v", err)
	}
	if got.Device != "/dev/sdb" {
		t.Fatalf("disk1 device = %q, want unchanged /dev/sdb", got.Device)
	}
}

func TestRunDiskReplace_UnknownMountpointRefused(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	st := store.NewArrayStore(newTestDB(t))
	genRoot := t.TempDir()
	mounter := disk.NewFakeMounter()

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	p.AddDisk("/dev/sdz", disk.Disk{Size: 4 * disk.TB})
	seedTwoDataDiskArray(t, st)

	eng := newRecordingEngine()
	registerDiskReplace(t, s, p, disk.NewFakeRunner(), st, genRoot, mounter, eng)

	replacement := disk.AssignedDisk{Device: "/dev/sdz", Filesystem: disk.XFS}
	params := DiskReplaceParams{
		Confirmation: SingleDiskConfirmation(replacement),
		Mountpoint:   "/mnt/disk9",
		Disk:         replacement,
		Sizes:        map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdb": 4 * disk.TB, "/dev/sdc": 4 * disk.TB, "/dev/sdz": 4 * disk.TB},
	}
	j, err := s.Submit(ctx, TypeDiskReplace, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s (%s), want failed", finished.Status, finished.ErrorMessage)
	}
	if !strings.Contains(finished.ErrorMessage, "no data disk at that mountpoint") {
		t.Fatalf("ErrorMessage = %q, want ErrArrayDiskNotFound", finished.ErrorMessage)
	}
	if _, ok := p.FormattedAs("/dev/sdz"); ok {
		t.Fatal("unknown mountpoint formatted the replacement disk anyway")
	}
}

func TestRunDiskReplace_Q20RefusesAReplacementLargerThanParity(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	st := store.NewArrayStore(newTestDB(t))
	genRoot := t.TempDir()
	mounter := disk.NewFakeMounter()

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 4 * disk.TB})
	p.AddDisk("/dev/sdz", disk.Disk{Size: 8 * disk.TB})
	seedTwoDataDiskArray(t, st)

	eng := newRecordingEngine()
	registerDiskReplace(t, s, p, disk.NewFakeRunner(), st, genRoot, mounter, eng)

	replacement := disk.AssignedDisk{Device: "/dev/sdz", Filesystem: disk.XFS}
	params := DiskReplaceParams{
		Confirmation: SingleDiskConfirmation(replacement),
		Mountpoint:   "/mnt/disk1",
		Disk:         replacement,
		// The replacement (8 TB) is larger than the array's own parity
		// (4 TB) — Q20 must refuse this before anything is formatted.
		Sizes: map[string]int64{"/dev/sda": 4 * disk.TB, "/dev/sdc": 4 * disk.TB, "/dev/sdz": 8 * disk.TB},
	}
	j, err := s.Submit(ctx, TypeDiskReplace, nil, mustJSON(t, params))
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
	if _, ok := p.FormattedAs("/dev/sdz"); ok {
		t.Fatal("Q20 violation formatted the replacement disk anyway")
	}
}

// TestRunDiskReplace_FixFailureLeavesTopologyAlreadySwitched proves this
// issue's own safety-critical recovery property at the unit level (the
// lab test proves it against a real snapraid binary): once the
// replacement disk is formatted and its slot re-pointed, a failing (or,
// in the lab, interrupted) fix leaves that switch in place rather than
// half-applied — recoverable by an ordinary subsequent fix, never by
// reformatting again.
func TestRunDiskReplace_FixFailureLeavesTopologyAlreadySwitched(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	st := store.NewArrayStore(newTestDB(t))
	genRoot := t.TempDir()
	mounter := disk.NewFakeMounter()

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	p.AddDisk("/dev/sdz", disk.Disk{Size: 4 * disk.TB})
	r := disk.NewFakeRunner()
	scriptFilesystemUUID(r, "/dev/sdz", "uuid-new")
	scriptMountedUUID(r, "/mnt/disk1", "uuid-new")
	seedTwoDataDiskArray(t, st)

	eng := newRecordingEngine()
	eng.SetStatus(parity.ParityStatus{DataMounts: map[string]string{"d1": "/mnt/disk1", "d2": "/mnt/disk2"}})
	eng.ScriptFix(nil, context.DeadlineExceeded)

	registerDiskReplace(t, s, p, r, st, genRoot, mounter, eng)

	replacement := disk.AssignedDisk{Device: "/dev/sdz", Filesystem: disk.XFS}
	params := DiskReplaceParams{
		Confirmation: SingleDiskConfirmation(replacement),
		Mountpoint:   "/mnt/disk1",
		Disk:         replacement,
		Sizes:        map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdc": 4 * disk.TB, "/dev/sdz": 4 * disk.TB},
	}
	j, err := s.Submit(ctx, TypeDiskReplace, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s (%s), want failed", finished.Status, finished.ErrorMessage)
	}

	// The topology switch already committed before the fix step ran.
	got, err := st.GetDataDiskByMountpoint(ctx, "/mnt/disk1")
	if err != nil {
		t.Fatalf("GetDataDiskByMountpoint: %v", err)
	}
	if got.Device != "/dev/sdz" {
		t.Fatalf("disk1 device = %q after a failed fix, want /dev/sdz (already switched over)", got.Device)
	}
}

// TestRunDiskReplace_RefusesWhenSlotDiskStillPresentByIdentity is finding
// 1's own unit-level regression test: the slot's stored disk (/dev/sdb,
// WWN wwn-d1) is still reported by a fresh Provider.List, renumbered to
// /dev/sdx — a healthy disk that has not actually failed or been removed
// (doc 02 §4 steps 1-2). The lab test covers the "still mounted" half of
// the same check against a real mount; this covers the "still present by
// identity" half without needing one.
func TestRunDiskReplace_RefusesWhenSlotDiskStillPresentByIdentity(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	st := store.NewArrayStore(newTestDB(t))
	genRoot := t.TempDir()
	mounter := disk.NewFakeMounter()

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	// The slot's own disk (WWN wwn-d1) is still attached — renumbered to
	// /dev/sdx, but still the same physical disk by identity (Q21).
	p.AddDisk("/dev/sdx", disk.Disk{Size: 4 * disk.TB, WWN: "wwn-d1"})
	p.AddDisk("/dev/sdz", disk.Disk{Size: 4 * disk.TB})
	r := disk.NewFakeRunner()
	scriptFilesystemUUID(r, "/dev/sdz", "uuid-new")

	if err := st.PutArray(ctx, store.ArraySettings{
		CreatePolicy: "mfs", MinFreeSpace: "20G", CreatedAt: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", Mountpoint: "/mnt/parity1"},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d1", WWN: "wwn-d1", Mountpoint: "/mnt/disk1"},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}

	eng := newRecordingEngine()
	registerDiskReplace(t, s, p, r, st, genRoot, mounter, eng)

	replacement := disk.AssignedDisk{Device: "/dev/sdz", Filesystem: disk.XFS}
	params := DiskReplaceParams{
		Confirmation: SingleDiskConfirmation(replacement),
		Mountpoint:   "/mnt/disk1",
		Disk:         replacement,
		Sizes:        map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdz": 4 * disk.TB},
	}
	j, err := s.Submit(ctx, TypeDiskReplace, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s (%s), want failed — the slot's own disk is still present by identity", finished.Status, finished.ErrorMessage)
	}
	if !strings.Contains(finished.ErrorMessage, "still present") {
		t.Fatalf("ErrorMessage = %q, want ErrReplacementSlotDiskPresent", finished.ErrorMessage)
	}
	if _, ok := p.FormattedAs("/dev/sdz"); ok {
		t.Fatal("a still-present slot disk formatted the replacement anyway")
	}
	got, err := st.GetDataDiskByMountpoint(ctx, "/mnt/disk1")
	if err != nil {
		t.Fatalf("GetDataDiskByMountpoint: %v", err)
	}
	if got.Device != "/dev/sdb" {
		t.Fatalf("disk1 device = %q after a refused replace, want unchanged /dev/sdb", got.Device)
	}
}

// TestRunDiskReplace_WeakIdentityFSUUIDRefused is finding 1's own
// regression test for replace: a weak-identity replacement — no WWN or
// serial at all, as every disk in the loop-device lab is (doc 06 §3) —
// carries nothing refuseKnownIdentity can compare by WWN/serial alone, so
// a replacement that is actually the array's own disk2 under a
// renumbered path must be recognised by filesystem UUID instead, the same
// fallback matchArrayDisk and ConfirmReplacementTargetAbsent already use.
// ValidateDiskReplacement runs before ConfirmReplacementTargetAbsent in
// RunDiskReplace, so disk1's own old device need not be modelled absent
// here for this refusal to fire.
func TestRunDiskReplace_WeakIdentityFSUUIDRefused(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	st := store.NewArrayStore(newTestDB(t))
	genRoot := t.TempDir()
	mounter := disk.NewFakeMounter()

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	// /dev/vdd claims to be a fresh replacement for disk1, but its
	// filesystem UUID matches the array's own disk2 — the same physical
	// disk under a renumbered path, still a live member, not a fresh disk.
	p.AddDisk("/dev/vdd", disk.Disk{Size: 4 * disk.TB, WeakIdentity: true, FSUUID: "uuid-d2"})

	if err := st.PutArray(ctx, store.ArraySettings{
		CreatePolicy: "mfs", MinFreeSpace: "20G", CreatedAt: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", Mountpoint: "/mnt/parity1"},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/vdb", Filesystem: "xfs", FSUUID: "uuid-d1", WeakIdentity: true, Mountpoint: "/mnt/disk1"},
		{Role: store.ArrayRoleData, RoleIndex: 2, Device: "/dev/vdc", Filesystem: "xfs", FSUUID: "uuid-d2", WeakIdentity: true, Mountpoint: "/mnt/disk2"},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}

	eng := newRecordingEngine()
	registerDiskReplace(t, s, p, disk.NewFakeRunner(), st, genRoot, mounter, eng)

	replacement := disk.AssignedDisk{Device: "/dev/vdd", Filesystem: disk.XFS, WeakIdentity: true, FSUUID: "uuid-d2"}
	params := DiskReplaceParams{
		Confirmation: SingleDiskConfirmation(replacement),
		Mountpoint:   "/mnt/disk1",
		Disk:         replacement,
		// Every device Validate needs a size for (disk1's own old device
		// leaves the array, so its size is not needed) is present, so a
		// reverted fix would fall through to a real format instead of
		// masking the vulnerability behind an unrelated ErrMissingSize.
		Sizes: map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/vdc": 4 * disk.TB, "/dev/vdd": 4 * disk.TB},
	}
	j, err := s.Submit(ctx, TypeDiskReplace, nil, mustJSON(t, params))
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
	got, err := st.GetDataDiskByMountpoint(ctx, "/mnt/disk1")
	if err != nil {
		t.Fatalf("GetDataDiskByMountpoint: %v", err)
	}
	if got.Device != "/dev/vdb" {
		t.Fatalf("disk1 device = %q after a refused replace, want unchanged /dev/vdb", got.Device)
	}
}

// TestRunDiskReplace_RunTimeProviderReadCatchesStaleSubmitTimeIdentity is
// finding 1's own regression test for replace (#288 fix round 4): the
// params captured at submit time carry no FSUUID for the replacement —
// exactly what the replaceDisk handler submits for a disk with no
// filesystem yet — but by the time the job actually runs, a fresh
// Provider.List reports that device now carries disk2's own filesystem
// UUID. Unlike TestRunDiskReplace_WeakIdentityFSUUIDRefused, which puts
// the stored member's FSUUID straight into params, this only a fresh
// Provider.List read at run time can catch — via
// confirmTargetIdentityUnchanged's own unconditional FSUUID comparison
// (round 6): the submitted FSUUID was empty, the run-time one is not, so
// the job is refused before refuseKnownIdentity's membership check ever
// runs.
func TestRunDiskReplace_RunTimeProviderReadCatchesStaleSubmitTimeIdentity(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	st := store.NewArrayStore(newTestDB(t))
	genRoot := t.TempDir()
	mounter := disk.NewFakeMounter()

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	// /dev/vdd is presented as a fresh replacement for disk1, with no
	// FSUUID captured at submit time, but the provider already reports
	// its own filesystem UUID matching disk2's own stored member — the
	// same physical disk under a renumbered path, still a live member.
	p.AddDisk("/dev/vdd", disk.Disk{Size: 4 * disk.TB, WeakIdentity: true, FSUUID: "uuid-d2"})

	if err := st.PutArray(ctx, store.ArraySettings{
		CreatePolicy: "mfs", MinFreeSpace: "20G", CreatedAt: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", Mountpoint: "/mnt/parity1"},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/vdb", Filesystem: "xfs", FSUUID: "uuid-d1", WeakIdentity: true, Mountpoint: "/mnt/disk1"},
		{Role: store.ArrayRoleData, RoleIndex: 2, Device: "/dev/vdc", Filesystem: "xfs", FSUUID: "uuid-d2", WeakIdentity: true, Mountpoint: "/mnt/disk2"},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}

	eng := newRecordingEngine()
	registerDiskReplace(t, s, p, disk.NewFakeRunner(), st, genRoot, mounter, eng)

	// The params captured at submit time carry no FSUUID at all.
	submitted := disk.AssignedDisk{Device: "/dev/vdd", Filesystem: disk.XFS, WeakIdentity: true}
	params := DiskReplaceParams{
		Confirmation: SingleDiskConfirmation(submitted),
		Mountpoint:   "/mnt/disk1",
		Disk:         submitted,
		Sizes:        map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/vdc": 4 * disk.TB, "/dev/vdd": 4 * disk.TB},
	}
	j, err := s.Submit(ctx, TypeDiskReplace, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if _, ok := p.FormattedAs("/dev/vdd"); ok {
		t.Fatal("a submit-time FSUUID of \"\" let a run-time-matching disk be formatted anyway")
	}
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s (%s), want failed — /dev/vdd's current identity matches disk2's own filesystem UUID", finished.Status, finished.ErrorMessage)
	}
	if !strings.Contains(finished.ErrorMessage, "no longer matches what was confirmed") {
		t.Fatalf("ErrorMessage = %q, want ErrDiskIdentityDrifted", finished.ErrorMessage)
	}
}

// TestRunDiskReplace_QueuedBehindStorageJobRefusesNonMemberDiskWithFilesystem
// is finding 1's own regression test (#288 fix round 6): the submitted
// replacement is identity-less — WeakIdentity true, no WWN, Serial, ByIDName
// or FSUUID at all — and the plan and confirm dialog showed it as a fresh
// disk with no filesystem. While the job sits queued behind a running
// storage-class job (ClassTopology conflicts with every storage class, doc
// 01 §4), a hypervisor detach and reattach, or a bus reset, renumbers the
// disks: another identity-less disk now sits at the target's path, holding
// data of its own, and it is not an array member at all — so
// refuseKnownIdentity's own membership check, which only ever compares
// against disks.ArrayDisk rows, has nothing to catch it on. f5453e5
// formatted it the same way it formatted disk_add's non-member disk: an
// empty submitted FSUUID was silently overwritten with the run-time disk's,
// and a non-member disk was never checked against refuseKnownIdentity at
// all. RunDiskReplace calls the same confirmTargetIdentityUnchanged
// RunDiskAdd does, so it shares the fix.
func TestRunDiskReplace_QueuedBehindStorageJobRefusesNonMemberDiskWithFilesystem(t *testing.T) {
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
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/vdb", Filesystem: "xfs", FSUUID: "uuid-d1", WeakIdentity: true, Mountpoint: "/mnt/disk1"},
		{Role: store.ArrayRoleData, RoleIndex: 2, Device: "/dev/vdc", Filesystem: "xfs", FSUUID: "uuid-d2", WeakIdentity: true, Mountpoint: "/mnt/disk2"},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}

	eng := newRecordingEngine()
	registerDiskReplace(t, s, p, disk.NewFakeRunner(), st, genRoot, mounter, eng)

	syncStarted, syncRelease := registerBlocking(s, TypeSync, false)
	sync, err := s.Submit(ctx, TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit(sync): %v", err)
	}
	<-syncStarted

	submitted := disk.AssignedDisk{Device: "/dev/vdg", Filesystem: disk.XFS, WeakIdentity: true}
	params := DiskReplaceParams{
		Confirmation: SingleDiskConfirmation(submitted),
		Mountpoint:   "/mnt/disk1",
		Disk:         submitted,
		Sizes:        map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/vdc": 4 * disk.TB, "/dev/vdg": 4 * disk.TB},
	}
	j, err := s.Submit(ctx, TypeDiskReplace, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit(disk_replace): %v", err)
	}
	if j.Status != StatusQueued {
		t.Fatalf("disk_replace Status = %s, want queued behind the running sync (ClassTopology excludes every storage class)", j.Status)
	}

	// While disk_replace sits queued, a disk holding the user's own data —
	// not an array member, not even registered — comes to sit at this
	// exact same /dev/vdg path.
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
	got, err := st.GetDataDiskByMountpoint(ctx, "/mnt/disk1")
	if err != nil {
		t.Fatalf("GetDataDiskByMountpoint: %v", err)
	}
	if got.Device != "/dev/vdb" {
		t.Fatalf("disk1 device = %q after a refused replace, want unchanged /dev/vdb", got.Device)
	}
}

// TestRunDiskReplace_QueuedBehindStorageJobRefusesRenumberedStrongIdentityMember
// is disk_replace's analogue of
// TestRunDiskAdd_QueuedBehindStorageJobRefusesRenumberedStrongIdentityMember:
// a weak, empty-identity replacement submission whose path renumbers onto a
// serial-bearing array member while the job sits queued behind a running
// storage-class job. refuseKnownIdentity's own WWN/serial branch has
// nothing to compare a submitted empty WWN/serial against, and its FSUUID
// fallback requires WeakIdentity, so only confirmTargetIdentityUnchanged's
// fail-closed comparison catches it.
func TestRunDiskReplace_QueuedBehindStorageJobRefusesRenumberedStrongIdentityMember(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	st := store.NewArrayStore(newTestDB(t))
	genRoot := t.TempDir()
	mounter := disk.NewFakeMounter()

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	// At submit time this is a fresh, unformatted USB-bridge disk offered
	// as disk1's replacement: weak identity, no filesystem yet, so the
	// params captured here carry no WWN, serial or FSUUID at all.
	p.AddDisk("/dev/sdg", disk.Disk{Size: 4 * disk.TB, WeakIdentity: true})

	if err := st.PutArray(ctx, store.ArraySettings{
		CreatePolicy: "mfs", MinFreeSpace: "20G", CreatedAt: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", Mountpoint: "/mnt/parity1"},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/vdb", Filesystem: "xfs", FSUUID: "uuid-d1", WeakIdentity: true, Mountpoint: "/mnt/disk1"},
		// disk2 is a strong-identity member (a SATA/SAS disk, or a USB
		// enclosure that passes a serial through) — the common case for an
		// array member, unlike the weak-identity fixture above.
		{Role: store.ArrayRoleData, RoleIndex: 2, Device: "/dev/sdh", Filesystem: "xfs", FSUUID: "uuid-d2", Serial: "serial-d2", Mountpoint: "/mnt/disk2"},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}

	eng := newRecordingEngine()
	registerDiskReplace(t, s, p, disk.NewFakeRunner(), st, genRoot, mounter, eng)

	syncStarted, syncRelease := registerBlocking(s, TypeSync, false)
	sync, err := s.Submit(ctx, TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit(sync): %v", err)
	}
	<-syncStarted

	submitted := disk.AssignedDisk{Device: "/dev/sdg", Filesystem: disk.XFS, WeakIdentity: true}
	params := DiskReplaceParams{
		Confirmation: SingleDiskConfirmation(submitted),
		Mountpoint:   "/mnt/disk1",
		Disk:         submitted,
		Sizes:        map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdh": 4 * disk.TB, "/dev/sdg": 4 * disk.TB},
	}
	j, err := s.Submit(ctx, TypeDiskReplace, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit(disk_replace): %v", err)
	}
	if j.Status != StatusQueued {
		t.Fatalf("disk_replace Status = %s, want queued behind the running sync (ClassTopology excludes every storage class)", j.Status)
	}

	// While disk_replace sits queued, a bus reset drops the fresh USB disk
	// and disk2 — a strong-identity member — comes back at this exact same
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
	got, err := st.GetDataDiskByMountpoint(ctx, "/mnt/disk1")
	if err != nil {
		t.Fatalf("GetDataDiskByMountpoint: %v", err)
	}
	if got.Device != "/dev/vdb" {
		t.Fatalf("disk1 device = %q after a refused replace, want unchanged /dev/vdb", got.Device)
	}
}

// TestRunDiskReplace_RefusesWhenMountedFilesystemIsNotTheReplacement is
// finding 1's second regression test: even once the topology switch has
// already been persisted, RunDiskReplace must not run snapraid fix unless
// the slot's mountpoint is genuinely backed by the replacement's own
// filesystem — never trusting the switch alone.
func TestRunDiskReplace_RefusesWhenMountedFilesystemIsNotTheReplacement(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	st := store.NewArrayStore(newTestDB(t))
	genRoot := t.TempDir()
	mounter := disk.NewFakeMounter()

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	p.AddDisk("/dev/sdz", disk.Disk{Size: 4 * disk.TB})
	r := disk.NewFakeRunner()
	scriptFilesystemUUID(r, "/dev/sdz", "uuid-new")
	// The mountpoint reports a filesystem UUID that is neither the old
	// disk's nor the replacement's — the mount never actually switched
	// over, whatever the store row now says.
	scriptMountedUUID(r, "/mnt/disk1", "uuid-stale")
	seedTwoDataDiskArray(t, st)

	eng := newRecordingEngine()
	eng.SetStatus(parity.ParityStatus{DataMounts: map[string]string{"d1": "/mnt/disk1", "d2": "/mnt/disk2"}})
	eng.ScriptFix([]parity.Progress{{}}, nil)

	registerDiskReplace(t, s, p, r, st, genRoot, mounter, eng)

	replacement := disk.AssignedDisk{Device: "/dev/sdz", Filesystem: disk.XFS}
	params := DiskReplaceParams{
		Confirmation: SingleDiskConfirmation(replacement),
		Mountpoint:   "/mnt/disk1",
		Disk:         replacement,
		Sizes:        map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdc": 4 * disk.TB, "/dev/sdz": 4 * disk.TB},
	}
	j, err := s.Submit(ctx, TypeDiskReplace, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s (%s), want failed — the mountpoint is not backed by the replacement", finished.Status, finished.ErrorMessage)
	}
	if !strings.Contains(finished.ErrorMessage, "not the replacement's") {
		t.Fatalf("ErrorMessage = %q, want a mounted-filesystem mismatch refusal", finished.ErrorMessage)
	}
	if _, _, _, lastFix := eng.snapshot(); lastFix.Disk != "" {
		t.Fatalf("snapraid fix ran (Disk=%q) against a mountpoint not backed by the replacement", lastFix.Disk)
	}
}

// TestRunDiskReplace_AbandonsRemovalAndSucceeds_WhenEvacuatedOrUnpooled is
// #384's own regression: a slot marked evacuated or unpooled is no longer
// refused (store.ErrDiskLeavingArray) on the removal state alone —
// ReplaceEligibleDuringRemoval allows both — and the replacement's
// identity swap clears removal_state and removal_job_id in the same
// store write (store.ReplaceDataDiskAbandoningRemoval), so the slot
// rejoins the pool as an ordinary disk and SnapRAID's fix still runs
// against it.
func TestRunDiskReplace_AbandonsRemovalAndSucceeds_WhenEvacuatedOrUnpooled(t *testing.T) {
	for _, state := range []string{store.RemovalStateEvacuated, store.RemovalStateUnpooled} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			s := newTestScheduler(t)
			st := store.NewArrayStore(newTestDB(t))
			genRoot := t.TempDir()
			mounter := disk.NewFakeMounter()

			p := disk.NewFakeProvider()
			p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
			p.AddDisk("/dev/sdz", disk.Disk{Size: 4 * disk.TB})
			r := disk.NewFakeRunner()
			scriptFilesystemUUID(r, "/dev/sdz", "uuid-new")
			scriptMountedUUID(r, "/mnt/disk1", "uuid-new")

			seedTwoDataDiskArray(t, st)
			if err := st.SetRemovalState(ctx, "/mnt/disk1", store.RemovalStateEvacuated, "evacuation-job"); err != nil {
				t.Fatalf("SetRemovalState(evacuated): %v", err)
			}
			if state == store.RemovalStateUnpooled {
				if err := st.AdvanceRemovalState(ctx, "/mnt/disk1", store.RemovalStateEvacuated, store.RemovalStateUnpooled, "disk-remove-job"); err != nil {
					t.Fatalf("AdvanceRemovalState(unpooled): %v", err)
				}
			}

			eng := newRecordingEngine()
			eng.SetStatus(parity.ParityStatus{DataMounts: map[string]string{"d1": "/mnt/disk1", "d2": "/mnt/disk2"}})
			eng.ScriptFix([]parity.Progress{{}}, nil)

			registerDiskReplace(t, s, p, r, st, genRoot, mounter, eng)

			replacement := disk.AssignedDisk{Device: "/dev/sdz", Filesystem: disk.XFS}
			params := DiskReplaceParams{
				Confirmation: SingleDiskConfirmation(replacement),
				Mountpoint:   "/mnt/disk1",
				Disk:         replacement,
				Sizes:        map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdc": 4 * disk.TB, "/dev/sdz": 4 * disk.TB},
			}
			j, err := s.Submit(ctx, TypeDiskReplace, nil, mustJSON(t, params))
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}
			finished := await(t, s, j.ID)
			if finished.Status != StatusSucceeded {
				t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
			}
			if _, ok := p.FormattedAs("/dev/sdz"); !ok {
				t.Fatal("replacement disk was not formatted")
			}
			got, err := st.GetDataDiskByMountpoint(ctx, "/mnt/disk1")
			if err != nil {
				t.Fatalf("GetDataDiskByMountpoint: %v", err)
			}
			if got.Device != "/dev/sdz" || got.FSUUID != "uuid-new" {
				t.Fatalf("replaced disk = %+v", got)
			}
			if got.RemovalState != "" {
				t.Fatalf("removal_state = %q after replace, want cleared", got.RemovalState)
			}
			if got.RemovalJobID != "" {
				t.Fatalf("removal_job_id = %q after replace, want cleared", got.RemovalJobID)
			}
			_, _, _, lastFix := eng.snapshot()
			if lastFix.Disk != "d1" {
				t.Fatalf("Fix ran against disk %q, want d1", lastFix.Disk)
			}
		})
	}
}

// TestRunDiskReplace_StillRefusesWhenUnlisted is ReplaceEligibleDuringRemoval's
// own negative case: a disk already dropped from snapraid.conf has
// nothing left for SnapRAID's fix to rebuild against, so it stays
// refused exactly like TestRunDiskReplace_RefusesASlotInRemoval's
// evacuating case, even though both are "left the pool"
// (store.ArrayDisk.LeftPool).
func TestRunDiskReplace_StillRefusesWhenUnlisted(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	st := store.NewArrayStore(newTestDB(t))
	genRoot := t.TempDir()
	mounter := disk.NewFakeMounter()

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	p.AddDisk("/dev/sdz", disk.Disk{Size: 4 * disk.TB})
	seedTwoDataDiskArray(t, st)
	if err := st.SetRemovalState(ctx, "/mnt/disk1", store.RemovalStateEvacuated, "evacuation-job"); err != nil {
		t.Fatalf("SetRemovalState(evacuated): %v", err)
	}
	if err := st.AdvanceRemovalState(ctx, "/mnt/disk1", store.RemovalStateEvacuated, store.RemovalStateUnpooled, "disk-remove-job"); err != nil {
		t.Fatalf("AdvanceRemovalState(unpooled): %v", err)
	}
	if err := st.AdvanceRemovalState(ctx, "/mnt/disk1", store.RemovalStateUnpooled, store.RemovalStateUnlisted, "disk-remove-job"); err != nil {
		t.Fatalf("AdvanceRemovalState(unlisted): %v", err)
	}

	eng := newRecordingEngine()
	registerDiskReplace(t, s, p, disk.NewFakeRunner(), st, genRoot, mounter, eng)

	replacement := disk.AssignedDisk{Device: "/dev/sdz", Filesystem: disk.XFS}
	params := DiskReplaceParams{
		Confirmation: SingleDiskConfirmation(replacement),
		Mountpoint:   "/mnt/disk1",
		Disk:         replacement,
		Sizes:        map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdc": 4 * disk.TB, "/dev/sdz": 4 * disk.TB},
	}
	j, err := s.Submit(ctx, TypeDiskReplace, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s (%s), want failed", finished.Status, finished.ErrorMessage)
	}
	if !strings.Contains(finished.ErrorMessage, "/mnt/disk1") || !strings.Contains(finished.ErrorMessage, "leaving the array") {
		t.Fatalf("ErrorMessage = %q, want a removal-state refusal naming /mnt/disk1", finished.ErrorMessage)
	}
	if _, ok := p.FormattedAs("/dev/sdz"); ok {
		t.Fatal("an unlisted slot formatted the replacement disk anyway")
	}
}
