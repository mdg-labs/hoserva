package job

import (
	"context"
	"testing"

	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/store"
)

// guardedTestMounter is the daemon's own mounter shape (disk.GuardedMounter
// around the UnitMounter that brings a slot's disk up) with the directory
// creation left out: every mountpoint these jobs assign is a production
// /mnt path, which a test must never create. The chattr itself still goes
// through r.
func guardedTestMounter(r disk.Runner, inner disk.UnitMounter) disk.GuardedMounter {
	return disk.GuardedMounter{
		Mounter: inner,
		Runner:  r,
		Ensure: func(ctx context.Context, r disk.Runner, path string) error {
			return disk.SetImmutable(ctx, r, path, true)
		},
	}
}

func immutableCalls(r *disk.FakeRunner) []string {
	var out []string
	for _, c := range r.Calls() {
		if c.Name == "chattr" && len(c.Args) == 2 && c.Args[0] == "+i" {
			out = append(out, c.Args[1])
		}
	}
	return out
}

func assertEveryMountedSlotWasGuarded(t *testing.T, r *disk.FakeRunner, mounter *disk.FakeMounter, want ...string) {
	t.Helper()
	guarded := map[string]bool{}
	for _, p := range immutableCalls(r) {
		guarded[p] = true
	}
	mounted := map[string]bool{}
	for _, m := range mounter.Mounts {
		mounted[m.Where] = true
		if !guarded[m.Where] {
			t.Errorf("%s was mounted without chattr +i first (chattr +i issued for %v)", m.Where, immutableCalls(r))
		}
	}
	for _, w := range want {
		if !mounted[w] {
			t.Errorf("%s was never mounted (mounts: %+v), so the guard was not exercised for it", w, mounter.Mounts)
		}
		if !guarded[w] {
			t.Errorf("chattr +i was not issued for %s (issued for %v)", w, immutableCalls(r))
		}
	}
}

func TestRunDiskFormat_MakesEveryNewSlotMountpointImmutableBeforeMountingIt(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	p.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB})
	p.AddDisk("/dev/sdc", disk.Disk{Size: 4 * disk.TB})
	runner := disk.NewFakeRunner()
	scriptFilesystemUUID(runner, "/dev/sda", "uuid-parity1")
	scriptFilesystemUUID(runner, "/dev/sdb", "uuid-disk1")
	scriptFilesystemUUID(runner, "/dev/sdc", "uuid-cache")
	inner := disk.NewFakeMounter()
	registerDiskFormatDeps(t, s, p, runner, store.NewArrayStore(newTestDB(t)), t.TempDir(), guardedTestMounter(runner, inner))

	params := validFormatParamsWithCache("/dev/sda", "/dev/sdb", "/dev/sdc", map[string]int64{
		"/dev/sda": 8 * disk.TB, "/dev/sdb": 4 * disk.TB, "/dev/sdc": 4 * disk.TB,
	})
	j, err := s.Submit(ctx, TypeDiskFormat, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if finished := await(t, s, j.ID); finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}

	assertEveryMountedSlotWasGuarded(t, runner, inner, "/mnt/parity1", "/mnt/disk1", "/mnt/cache")
}

func TestRunDiskAdd_MakesTheNewSlotMountpointImmutableBeforeMountingIt(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	st := store.NewArrayStore(newTestDB(t))
	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	p.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB})
	p.AddDisk("/dev/sdc", disk.Disk{Size: 4 * disk.TB})
	r := disk.NewFakeRunner()
	scriptFilesystemUUID(r, "/dev/sdc", "uuid-d2")
	seedArray(t, st, "/dev/sda", "/dev/sdb")
	inner := disk.NewFakeMounter()
	registerDiskAdd(t, s, p, r, st, t.TempDir(), guardedTestMounter(r, inner))

	newDisk := disk.AssignedDisk{Device: "/dev/sdc", Filesystem: disk.XFS}
	j, err := s.Submit(ctx, TypeDiskAdd, nil, mustJSON(t, DiskAddParams{
		Confirmation: SingleDiskConfirmation(newDisk),
		Disk:         newDisk,
		Sizes:        map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdb": 4 * disk.TB, "/dev/sdc": 4 * disk.TB},
	}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if finished := await(t, s, j.ID); finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}

	assertEveryMountedSlotWasGuarded(t, r, inner, "/mnt/disk2")
}

func TestRunDiskReplace_MakesTheReplacementSlotMountpointImmutableBeforeMountingIt(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	st := store.NewArrayStore(newTestDB(t))
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
	inner := disk.NewFakeMounter()
	registerDiskReplace(t, s, p, r, st, t.TempDir(), guardedTestMounter(r, inner), eng)

	replacement := disk.AssignedDisk{Device: "/dev/sdz", Filesystem: disk.XFS}
	j, err := s.Submit(ctx, TypeDiskReplace, nil, mustJSON(t, DiskReplaceParams{
		Confirmation: SingleDiskConfirmation(replacement),
		Mountpoint:   "/mnt/disk1",
		Disk:         replacement,
		Sizes:        map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdc": 4 * disk.TB, "/dev/sdz": 4 * disk.TB},
	}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if finished := await(t, s, j.ID); finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}

	assertEveryMountedSlotWasGuarded(t, r, inner, "/mnt/disk1")
}
