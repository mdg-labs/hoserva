package job

import (
	"context"
	"errors"
	"testing"

	"github.com/mdg-labs/hoserva/internal/disk"
)

// noopRun registers t on s's registry with a RunFunc that succeeds
// immediately — every test in this file cares only about what Submit does
// before a job's RunFunc is ever reached, never about the run itself.
func noopRun(s *Scheduler, t Type, cancellable bool) {
	s.registry.Register(t, cancellable, func(context.Context, *RunContext) error { return nil })
}

// orderRecordingBackup is a job.ConfigBackup fake that records, at the
// moment Run is called, how many job rows already exist in store — the
// only way to prove Submit's pre-topology backup (doc 10 §1, #406) runs
// strictly before the job it is about is ever persisted, not merely
// somewhere during Submit.
type orderRecordingBackup struct {
	store         *Store
	calls         int
	jobsAtLastRun int
	err           error
}

func (b *orderRecordingBackup) Run(ctx context.Context) error {
	b.calls++
	jobs, err := b.store.List(ctx, ListFilter{})
	if err != nil {
		return err
	}
	b.jobsAtLastRun = len(jobs)
	return b.err
}

// diskReplaceParams is a minimal, decode-valid job.DiskReplaceParams
// payload — this file never runs a real disk replacement, so its values
// only need to satisfy ValidateParams.
func diskReplaceParams() DiskReplaceParams {
	return DiskReplaceParams{
		Confirmation: "confirm",
		Mountpoint:   "/mnt/disk1",
		Disk:         disk.AssignedDisk{Device: "/dev/sdz"},
	}
}

// TestSubmit_TopologyJobRunsPreTopologyBackupBeforeJobIsCreated reproduces
// #406: before this change, nothing between Submit and a disk-topology
// job's own run ever called a config backup, so a disk replace (or add,
// remove, upgrade, format, pool remount) left no snapshot of the
// configuration from before the change. It fails to compile against the
// pre-#406 Scheduler, which has no SetTopologyBackup at all — confirmed by
// running it against `git stash` of this issue's scheduler.go change,
// where the build itself fails with "s.SetTopologyBackup undefined".
func TestSubmit_TopologyJobRunsPreTopologyBackupBeforeJobIsCreated(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	noopRun(s, TypeDiskReplace, false)

	backup := &orderRecordingBackup{store: s.store}
	s.SetTopologyBackup(backup)

	j, err := s.Submit(ctx, TypeDiskReplace, nil, mustJSON(t, diskReplaceParams()))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if backup.calls != 1 {
		t.Fatalf("pre-topology backup ran %d times, want exactly 1", backup.calls)
	}
	if backup.jobsAtLastRun != 0 {
		t.Fatalf("pre-topology backup saw %d job rows already persisted, want 0 — it must run before the job's first state change", backup.jobsAtLastRun)
	}

	jobs, err := s.store.List(ctx, ListFilter{})
	if err != nil {
		t.Fatalf("listing jobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != j.ID {
		t.Fatalf("expected exactly the submitted job %s to be persisted, got %v", j.ID, jobs)
	}
	await(t, s, j.ID)
}

// TestSubmit_EveryTopologyJobTypeRunsPreTopologyBackupNonTopologyNever is
// doc 01 §4's ClassTopology table, exercised end to end through Submit:
// every job type in that class (disk_format, disk_add, disk_remove,
// disk_replace, disk_upgrade_data, disk_upgrade_parity, pool_remount) runs
// the pre-topology backup exactly once, and a representative job of every
// other class (sync, mover, an appdata backup, a VM start) never does.
func TestSubmit_EveryTopologyJobTypeRunsPreTopologyBackupNonTopologyNever(t *testing.T) {
	upgradeData := DiskUpgradeDataParams{
		Confirmation: "confirm",
		Mountpoint:   "/mnt/disk1",
		Old:          disk.AssignedDisk{FSUUID: "uuid-old"},
		Disk:         disk.AssignedDisk{Device: "/dev/sdz"},
	}
	upgradeParity := DiskUpgradeParityParams{
		Confirmation:  "confirm",
		Mountpoint:    "/mnt/parity1",
		NewMountpoint: "/mnt/parity1-new",
		Disk:          disk.AssignedDisk{Device: "/dev/sdz"},
	}

	cases := []struct {
		name        string
		typ         Type
		params      []byte
		wantClass   Class
		cancellable bool
	}{
		{"disk_format", TypeDiskFormat, mustJSON(t, DiskFormatParams{Confirmation: "confirm", Data: []disk.AssignedDisk{{Device: "/dev/sdz"}}}), ClassTopology, false},
		{"disk_add", TypeDiskAdd, mustJSON(t, DiskAddParams{Confirmation: "confirm", Disk: disk.AssignedDisk{Device: "/dev/sdz"}}), ClassTopology, false},
		{"disk_remove", TypeDiskRemove, mustJSON(t, DiskRemoveParams{Mountpoint: "/mnt/disk1", Confirmation: "confirm"}), ClassTopology, false},
		{"disk_replace", TypeDiskReplace, mustJSON(t, diskReplaceParams()), ClassTopology, false},
		{"disk_upgrade_data", TypeDiskUpgradeData, mustJSON(t, upgradeData), ClassTopology, true},
		{"disk_upgrade_parity", TypeDiskUpgradeParity, mustJSON(t, upgradeParity), ClassTopology, true},
		{"pool_remount", TypePoolRemount, nil, ClassTopology, false},
		{"sync", TypeSync, nil, ClassParity, true},
		{"mover", TypeMover, nil, ClassArrayWrite, true},
		{"appdata_backup", TypeAppdataBackup, nil, ClassService, false},
		{"vm_start", TypeVMStart, nil, ClassVM, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if class, ok := ClassOf(tc.typ); !ok || class != tc.wantClass {
				t.Fatalf("ClassOf(%s) = %v, %v, want %s", tc.typ, class, ok, tc.wantClass)
			}

			ctx := context.Background()
			s := newTestScheduler(t)
			noopRun(s, tc.typ, tc.cancellable)

			backup := &fakeBackup{}
			s.SetTopologyBackup(backup)

			submitted, _ := s.Submit(ctx, tc.typ, nil, tc.params)
			if submitted != nil {
				await(t, s, submitted.ID)
			}

			wantCalls := 0
			if tc.wantClass == ClassTopology {
				wantCalls = 1
			}
			if got := backup.count(); got != wantCalls {
				t.Fatalf("pre-topology backup ran %d times for %s, want %d", got, tc.typ, wantCalls)
			}
		})
	}
}

// TestSubmit_TopologyJobFailsClosedWhenPreTopologyBackupFails is doc 10
// §1's fail-closed requirement (#406, matching the pre-update backup's own
// cmd/hoservad/update.go behaviour): a topology job whose pre-topology
// backup fails never starts, its error surfaces to the caller, and no job
// row — no state change of any kind — is left behind. A non-topology
// submission is unaffected by the very same failing backup, since it is
// never called for one.
func TestSubmit_TopologyJobFailsClosedWhenPreTopologyBackupFails(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	noopRun(s, TypeDiskAdd, false)
	noopRun(s, TypeSync, true)

	backupErr := errors.New("backup: destination unreachable")
	s.SetTopologyBackup(&fakeBackup{err: backupErr})

	j, err := s.Submit(ctx, TypeDiskAdd, nil, mustJSON(t, DiskAddParams{
		Confirmation: "confirm",
		Disk:         disk.AssignedDisk{Device: "/dev/sdz"},
	}))
	if err == nil {
		t.Fatal("Submit: expected an error, got nil")
	}
	if !errors.Is(err, backupErr) {
		t.Fatalf("Submit error = %v, want it to wrap %v", err, backupErr)
	}
	if j != nil {
		t.Fatalf("Submit returned a job (%v) despite the failed pre-topology backup", j)
	}

	jobs, err := s.store.List(ctx, ListFilter{})
	if err != nil {
		t.Fatalf("listing jobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("expected no job row to exist after a refused topology submission, got %v", jobs)
	}

	syncJob, err := s.Submit(ctx, TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit(sync) with the same failing backup wired: %v", err)
	}
	if syncJob == nil {
		t.Fatal("Submit(sync) returned no job")
	}
	await(t, s, syncJob.ID)
}

// TestSubmit_TopologyJobProceedsWhenNoPreTopologyBackupIsWired matches doc
// 10 §1's "no destination configured" default to update.Engine.backup's
// own nil-Backup behaviour (cmd/hoservad/update.go): a Scheduler with no
// TopologyBackup set skips the backup rather than refusing every topology
// submission — production always wires one (cmd/hoservad/main.go), so
// this only covers what a Scheduler with none does.
func TestSubmit_TopologyJobProceedsWhenNoPreTopologyBackupIsWired(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	noopRun(s, TypeDiskAdd, false)

	j, err := s.Submit(ctx, TypeDiskAdd, nil, mustJSON(t, DiskAddParams{
		Confirmation: "confirm",
		Disk:         disk.AssignedDisk{Device: "/dev/sdz"},
	}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if j == nil {
		t.Fatal("Submit returned no job")
	}
	await(t, s, j.ID)
}
