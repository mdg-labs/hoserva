package job

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

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

// statusSnapshotBackup is a job.ConfigBackup fake that records, at the
// moment Run is called, every job currently in store keyed by its Type —
// the only way to prove a queued job's own pre-topology backup ran
// strictly after an earlier, conflicting topology job had already
// finished, and strictly before the queued job's own status moved off
// StatusQueued, without racing the goroutines that would otherwise decide
// that timing (#408).
type statusSnapshotBackup struct {
	mu        sync.Mutex
	store     *Store
	calls     int
	snapshots []map[Type]Status
}

func (b *statusSnapshotBackup) Run(ctx context.Context) error {
	jobs, err := b.store.List(ctx, ListFilter{})
	if err != nil {
		return err
	}
	snap := make(map[Type]Status, len(jobs))
	for _, j := range jobs {
		snap[j.Type] = j.Status
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	b.snapshots = append(b.snapshots, snap)
	return nil
}

func (b *statusSnapshotBackup) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func (b *statusSnapshotBackup) snapshotAt(i int) map[Type]Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.snapshots[i]
}

// TestDispatch_QueuedTopologyJobRunsPreTopologyBackupOnlyAfterItsOwnTurn is
// #408's own data-loss reproduction: a queued ClassTopology job's
// pre-topology backup (doc 10 §1) must describe the configuration from
// just before its own change, not from whenever it happened to be
// submitted. Before this fix, Submit ran the backup unconditionally for
// every ClassTopology job, including one immediately queued behind an
// already-running topology job — so disk_replace's own snapshot was taken
// while disk_add was still in flight, and nothing captured the
// configuration between the two changes. This fails against dev at
// 9d2f35e: confirmed by running it there — `go test -run
// TestDispatch_QueuedTopologyJobRunsPreTopologyBackupOnlyAfterItsOwnTurn
// ./internal/job/` — which fails with "pre-topology backup ran 2 times
// right after disk_replace was queued ..., want exactly 1" at the
// assertion right after Submit(disk_replace), since the pre-#408
// scheduler.go's Submit already called the backup a second time there,
// before disk_add had even finished.
func TestDispatch_QueuedTopologyJobRunsPreTopologyBackupOnlyAfterItsOwnTurn(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	startedAdd, releaseAdd := registerBlocking(s, TypeDiskAdd, false)
	startedReplace, releaseReplace := registerBlocking(s, TypeDiskReplace, false)

	backup := &statusSnapshotBackup{store: s.store}
	s.SetTopologyBackup(backup)

	add, err := s.Submit(ctx, TypeDiskAdd, nil, mustJSON(t, DiskAddParams{
		Confirmation: "confirm",
		Disk:         disk.AssignedDisk{Device: "/dev/sdz"},
	}))
	if err != nil {
		t.Fatalf("Submit(disk_add): %v", err)
	}
	<-startedAdd
	if got := backup.count(); got != 1 {
		t.Fatalf("pre-topology backup ran %d times for the immediately-started disk_add, want exactly 1", got)
	}

	replace, err := s.Submit(ctx, TypeDiskReplace, nil, mustJSON(t, diskReplaceParams()))
	if err != nil {
		t.Fatalf("Submit(disk_replace): %v", err)
	}
	queued, err := s.store.Get(ctx, replace.ID)
	if err != nil {
		t.Fatalf("Get(disk_replace): %v", err)
	}
	if queued.Status != StatusQueued {
		t.Fatalf("disk_replace status = %s, want %s", queued.Status, StatusQueued)
	}
	if got := backup.count(); got != 1 {
		t.Fatalf("pre-topology backup ran %d times right after disk_replace was queued behind a still-running disk_add, want exactly 1 (only disk_add's own start-time backup) — a second call here means disk_replace's own backup ran before its turn, describing the configuration from before disk_add's change finished rather than from just before disk_replace's own", got)
	}

	close(releaseAdd)
	waitSucceeded(t, s, add.ID)

	<-startedReplace
	if got := backup.count(); got != 2 {
		t.Fatalf("pre-topology backup ran %d times once disk_replace's RunFunc started, want exactly 2 (disk_add's own, then disk_replace's own)", got)
	}
	close(releaseReplace)
	waitSucceeded(t, s, replace.ID)

	if got := backup.count(); got != 2 {
		t.Fatalf("pre-topology backup ran %d times in total, want exactly 2 — one per topology job, never more", got)
	}

	snap := backup.snapshotAt(1)
	if got := snap[TypeDiskAdd]; got != StatusSucceeded {
		t.Fatalf("disk_replace's own pre-topology backup ran while disk_add's status was %s, want %s — it must run only after the earlier, conflicting topology job has actually finished", got, StatusSucceeded)
	}
	if got := snap[TypeDiskReplace]; got != StatusQueued {
		t.Fatalf("disk_replace's own pre-topology backup ran while its own status was already %s, want %s — the backup must run before the job it is about starts", got, StatusQueued)
	}
}

// sequencedBackup is a job.ConfigBackup fake whose successive Run calls
// return the next error in errs, in order — nil once errs is exhausted —
// so a test can make one queued topology job's own start-time backup fail
// while a later one's succeeds.
type sequencedBackup struct {
	mu    sync.Mutex
	errs  []error
	calls int
}

func (b *sequencedBackup) Run(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	var err error
	if b.calls < len(b.errs) {
		err = b.errs[b.calls]
	}
	b.calls++
	return err
}

// TestDispatch_QueuedTopologyJobFailsClosedWhenItsBackupFails is #408's own
// fail-closed requirement for a queued job's start-time backup: it ends
// StatusFailed with the backup's error recorded, its RunFunc is never
// called, and the scheduler keeps dispatching the rest of the queue
// instead of getting stuck behind it.
func TestDispatch_QueuedTopologyJobFailsClosedWhenItsBackupFails(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	// TypeMover (ClassArrayWrite, a storage class) holds up the queued
	// topology jobs below exactly like another topology job would, but
	// never calls the pre-topology backup itself — keeping the backup
	// fake's two calls entirely about disk_replace and disk_add.
	startedMover, releaseMover := registerBlocking(s, TypeMover, true)
	startedReplace, releaseReplace := registerBlocking(s, TypeDiskReplace, false)
	defer close(releaseReplace)
	noopRun(s, TypeDiskAdd, false)

	backupErr := errors.New("backup: destination unreachable")
	backup := &sequencedBackup{errs: []error{backupErr}}
	s.SetTopologyBackup(backup)

	mover, err := s.Submit(ctx, TypeMover, nil, nil)
	if err != nil {
		t.Fatalf("Submit(mover): %v", err)
	}
	<-startedMover

	replace, err := s.Submit(ctx, TypeDiskReplace, nil, mustJSON(t, diskReplaceParams()))
	if err != nil {
		t.Fatalf("Submit(disk_replace): %v", err)
	}
	add, err := s.Submit(ctx, TypeDiskAdd, nil, mustJSON(t, DiskAddParams{
		Confirmation: "confirm",
		Disk:         disk.AssignedDisk{Device: "/dev/sdz"},
	}))
	if err != nil {
		t.Fatalf("Submit(disk_add): %v", err)
	}

	close(releaseMover)
	waitSucceeded(t, s, mover.ID)

	failed := await(t, s, replace.ID)
	if failed.Status != StatusFailed {
		t.Fatalf("disk_replace status = %s, want %s", failed.Status, StatusFailed)
	}
	if !strings.Contains(failed.ErrorMessage, backupErr.Error()) {
		t.Fatalf("disk_replace error message = %q, want it to mention %q", failed.ErrorMessage, backupErr.Error())
	}

	select {
	case <-startedReplace:
		t.Fatal("disk_replace's RunFunc ran despite its own pre-topology backup failing")
	default:
	}

	succeeded := await(t, s, add.ID)
	if succeeded.Status != StatusSucceeded {
		t.Fatalf("disk_add status = %s, want %s — the queue must continue past disk_replace's failed backup rather than getting stuck behind it", succeeded.Status, StatusSucceeded)
	}
}

// gatedBackup is a job.ConfigBackup fake whose i-th call (0-based) blocks
// on gates[i], if non-nil, until the test closes it — used to hold a
// dispatch-time backup open so a test can deterministically land a call
// inside the exact window between dispatch() removing a queued job from
// s.queue and that backup returning.
type gatedBackup struct {
	mu    sync.Mutex
	calls int
	gates []chan struct{}
}

func (b *gatedBackup) Run(ctx context.Context) error {
	b.mu.Lock()
	idx := b.calls
	b.calls++
	var gate chan struct{}
	if idx < len(b.gates) {
		gate = b.gates[idx]
	}
	b.mu.Unlock()
	if gate != nil {
		<-gate
	}
	return nil
}

func (b *gatedBackup) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// TestCancel_EndsQueuedTopologyJobCancelledWhileItsStartTimeBackupIsDispatching
// covers the window dispatch() creates between removing a queued
// ClassTopology job from s.queue and deciding its start-time backup's
// outcome (#408): for that window the job is in neither s.queue nor
// s.running, so neither of Cancel's own other branches would otherwise
// recognize it. Cancel cannot interrupt a backup already running outside
// s.mu, so it only records the request and returns the job's current
// snapshot without error — and once the backup returns, the job ends
// cancelled instead of starting, with its RunFunc never called.
func TestCancel_EndsQueuedTopologyJobCancelledWhileItsStartTimeBackupIsDispatching(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	startedAdd, releaseAdd := registerBlocking(s, TypeDiskAdd, false)
	startedReplace, _ := registerBlocking(s, TypeDiskReplace, false)

	gate := make(chan struct{})
	backup := &gatedBackup{gates: []chan struct{}{nil, gate}}
	s.SetTopologyBackup(backup)

	add, err := s.Submit(ctx, TypeDiskAdd, nil, mustJSON(t, DiskAddParams{
		Confirmation: "confirm",
		Disk:         disk.AssignedDisk{Device: "/dev/sdz"},
	}))
	if err != nil {
		t.Fatalf("Submit(disk_add): %v", err)
	}
	<-startedAdd

	replace, err := s.Submit(ctx, TypeDiskReplace, nil, mustJSON(t, diskReplaceParams()))
	if err != nil {
		t.Fatalf("Submit(disk_replace): %v", err)
	}

	close(releaseAdd)
	waitSucceeded(t, s, add.ID)

	waitFor(t, time.Second, func() bool { return backup.count() == 2 })

	cancelled, err := s.Cancel(ctx, replace.ID)
	if err != nil {
		t.Fatalf("Cancel while disk_replace's start-time backup is dispatching: %v", err)
	}
	if cancelled == nil {
		t.Fatal("Cancel returned no job")
	}

	close(gate)

	final := await(t, s, replace.ID)
	if final.Status != StatusCancelled {
		t.Fatalf("disk_replace status = %s, want %s", final.Status, StatusCancelled)
	}

	select {
	case <-startedReplace:
		t.Fatal("disk_replace's RunFunc ran despite being cancelled while its start-time backup was dispatching")
	default:
	}
}

// TestDispatch_QueuedTopologyJobIsInterruptedIfMaintenanceEntersWhileItsBackupRuns
// covers the other half of the same #408 window: EnterMaintenance already
// interrupts every job still in s.queue at the moment it runs, but a
// queued ClassTopology job whose turn dispatch() had already taken is
// removed from s.queue before that — its own start-time backup may still
// be running. It must end up interrupted once that backup returns, never
// started (maintenance mode admits nothing but a data-disk upgrade), and
// never left stuck queued with nothing left to dispatch it.
func TestDispatch_QueuedTopologyJobIsInterruptedIfMaintenanceEntersWhileItsBackupRuns(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	startedMover, releaseMover := registerBlocking(s, TypeMover, true)
	startedAdd, _ := registerBlocking(s, TypeDiskAdd, false)

	gate := make(chan struct{})
	backup := &gatedBackup{gates: []chan struct{}{gate}}
	s.SetTopologyBackup(backup)

	mover, err := s.Submit(ctx, TypeMover, nil, nil)
	if err != nil {
		t.Fatalf("Submit(mover): %v", err)
	}
	<-startedMover

	add, err := s.Submit(ctx, TypeDiskAdd, nil, mustJSON(t, DiskAddParams{
		Confirmation: "confirm",
		Disk:         disk.AssignedDisk{Device: "/dev/sdz"},
	}))
	if err != nil {
		t.Fatalf("Submit(disk_add): %v", err)
	}

	close(releaseMover)
	waitSucceeded(t, s, mover.ID)

	waitFor(t, time.Second, func() bool { return backup.count() == 1 })

	if err := s.EnterMaintenance(ctx); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}

	close(gate)

	interrupted := await(t, s, add.ID)
	if interrupted.Status != StatusInterrupted {
		t.Fatalf("disk_add status = %s, want %s", interrupted.Status, StatusInterrupted)
	}

	select {
	case <-startedAdd:
		t.Fatal("disk_add's RunFunc ran despite maintenance mode being entered while its pre-topology backup was running")
	default:
	}
}

// TestSubmit_QueuesBehindADispatchingTopologyJobsStartTimeBackup is #408's
// mutual-exclusion reproduction for the window dispatch() creates between
// removing a queued ClassTopology job from s.queue and deciding its
// start-time backup's outcome: that job is in neither s.queue nor
// s.running, but doc 01 §4's exclusion must still hold for it exactly as
// if it were already running. Before this fix, hasConflictWithRunningLocked
// only ever consulted s.running, so a sync submitted while disk_replace's
// own start-time backup was still dispatching came back running instead
// of queued, and ran at the same time as the topology change it was
// supposed to be excluded from.
func TestSubmit_QueuesBehindADispatchingTopologyJobsStartTimeBackup(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	startedAdd, releaseAdd := registerBlocking(s, TypeDiskAdd, false)
	noopRun(s, TypeDiskReplace, false)
	noopRun(s, TypeSync, true)

	gate := make(chan struct{})
	backup := &gatedBackup{gates: []chan struct{}{nil, gate}}
	s.SetTopologyBackup(backup)

	add, err := s.Submit(ctx, TypeDiskAdd, nil, mustJSON(t, DiskAddParams{
		Confirmation: "confirm",
		Disk:         disk.AssignedDisk{Device: "/dev/sdz"},
	}))
	if err != nil {
		t.Fatalf("Submit(disk_add): %v", err)
	}
	<-startedAdd

	replace, err := s.Submit(ctx, TypeDiskReplace, nil, mustJSON(t, diskReplaceParams()))
	if err != nil {
		t.Fatalf("Submit(disk_replace): %v", err)
	}

	close(releaseAdd)
	waitSucceeded(t, s, add.ID)

	// disk_replace's turn has now come: dispatch() has removed it from
	// s.queue and is running its own start-time backup, held open by the
	// gate — it is in neither s.queue nor s.running.
	waitFor(t, time.Second, func() bool { return backup.count() == 2 })

	if blocking := s.BlockingStorageJob(); blocking == nil || blocking.ID != replace.ID {
		t.Fatalf("BlockingStorageJob() = %v, want disk_replace (%s) while its start-time backup is dispatching", blocking, replace.ID)
	}

	sync, err := s.Submit(ctx, TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit(sync): %v", err)
	}
	syncRow, err := s.store.Get(ctx, sync.ID)
	if err != nil {
		t.Fatalf("Get(sync): %v", err)
	}
	if syncRow.Status != StatusQueued {
		t.Fatalf("sync status = %s, want %s — a sync must never start while a topology job's start-time backup is still dispatching", syncRow.Status, StatusQueued)
	}

	close(gate)

	waitSucceeded(t, s, replace.ID)
	waitSucceeded(t, s, sync.ID)
}

// diskUpgradeDataParams is a minimal, decode-valid job.DiskUpgradeDataParams
// payload — none of the tests below drive a real data-disk upgrade run, so
// its values only need to satisfy decodeDiskUpgradeDataParams.
func diskUpgradeDataParams() DiskUpgradeDataParams {
	return DiskUpgradeDataParams{
		Confirmation: "confirm",
		Mountpoint:   "/mnt/disk1",
		Old:          disk.AssignedDisk{FSUUID: "uuid-old"},
		Disk:         disk.AssignedDisk{Device: "/dev/sdz"},
	}
}

// seedInterruptedDiskUpgradeData writes an interrupted, cancellable
// TypeDiskUpgradeData job straight into the store, at checkpoint cp — the
// state a previous run or a restart left it in — so a test can Resume it
// without driving a real upgrade through disk.DataDiskUpgradeHooks.
func seedInterruptedDiskUpgradeData(t *testing.T, s *Scheduler, cp disk.DataDiskUpgradeCheckpoint) *Job {
	t.Helper()
	now := time.Now().UTC()
	j := &Job{
		ID:          uuid.NewString(),
		Type:        TypeDiskUpgradeData,
		Class:       ClassTopology,
		Status:      StatusInterrupted,
		Resumable:   true,
		Cancellable: true,
		Params:      mustJSON(t, diskUpgradeDataParams()),
		Checkpoint:  mustJSON(t, cp),
		CreatedAt:   now,
		StartedAt:   &now,
		FinishedAt:  &now,
	}
	if err := s.store.Create(context.Background(), j); err != nil {
		t.Fatalf("seeding interrupted disk_upgrade_data job: %v", err)
	}
	return j
}

// countingAbort is a job.AbortFunc fake that records how many times it ran
// and always returns retErr.
type countingAbort struct {
	mu     sync.Mutex
	calls  int
	retErr error
}

func (a *countingAbort) run(context.Context, string, []byte) error {
	a.mu.Lock()
	a.calls++
	a.mu.Unlock()
	return a.retErr
}

func (a *countingAbort) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

// resumeDiskUpgradeDataBehindMover reproduces #408's own finding scenario:
// an interrupted data-disk upgrade is resumed while `array stop`'s stop
// sequence is still draining a job it asked to stop — a resumable
// TypeMover here, still in s.running because EnterMaintenance only closes
// its stop channel and this file's blockingRun never reads it, so the
// mover keeps running exactly as one still finishing its own last
// checkpoint would. Resume needs maintenance mode (doc 02 §4 UR1), entered
// here, and queues upgrade behind the still-running mover (doc 01 §4:
// Topology excludes every storage class globally) — dispatch() only picks
// it up once the caller closes the returned release channel.
func resumeDiskUpgradeDataBehindMover(t *testing.T, s *Scheduler, upgrade *Job) (releaseMover chan struct{}) {
	t.Helper()
	ctx := context.Background()
	startedMover, releaseMover := registerBlocking(s, TypeMover, true)

	if _, err := s.Submit(ctx, TypeMover, nil, nil); err != nil {
		t.Fatalf("Submit(mover): %v", err)
	}
	<-startedMover

	if err := s.EnterMaintenance(ctx); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}

	resumed, err := s.Resume(ctx, upgrade.ID)
	if err != nil {
		t.Fatalf("Resume(disk_upgrade_data): %v", err)
	}
	if resumed.Status != StatusQueued {
		t.Fatalf("disk_upgrade_data status after Resume = %s, want %s — the mover must still be running for it to queue behind", resumed.Status, StatusQueued)
	}
	return releaseMover
}

// TestDispatch_DispatchingDiskUpgradeDataCancelRunsAbortBeforeCancelled is
// the #408 finding's own data-loss scenario: a resumed data-disk upgrade
// queues behind a still-draining `array stop`, reaches its turn once that
// job finishes, and is cancelled while its start-time backup is still
// dispatching. TypeDiskUpgradeData registers an AbortFunc (Unwind, doc 02
// §4 E3), so the job must not end cancelled until that abort has actually
// run and succeeded — ending it cancelled with no Unwind, the way 325b7ae
// did, would let a later `array start` mount over whatever Unwind was
// supposed to have cleaned up (doc 02 §4 invariant 3). Fails against
// 325b7ae: abort ran 0 times and the job still ended cancelled.
func TestDispatch_DispatchingDiskUpgradeDataCancelRunsAbortBeforeCancelled(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	startedUpgrade, _ := registerBlocking(s, TypeDiskUpgradeData, true)
	abort := &countingAbort{}
	s.registry.RegisterAbort(TypeDiskUpgradeData, abort.run)

	upgrade := seedInterruptedDiskUpgradeData(t, s, disk.DataDiskUpgradeCheckpoint{Phase: disk.DataDiskUpgradePhaseCopying})

	gate := make(chan struct{})
	backup := &gatedBackup{gates: []chan struct{}{gate}}
	s.SetTopologyBackup(backup)

	releaseMover := resumeDiskUpgradeDataBehindMover(t, s, upgrade)
	close(releaseMover)

	waitFor(t, time.Second, func() bool { return backup.count() == 1 })

	cancelled, err := s.Cancel(ctx, upgrade.ID)
	if err != nil {
		t.Fatalf("Cancel while disk_upgrade_data's start-time backup is dispatching: %v", err)
	}
	if cancelled == nil {
		t.Fatal("Cancel returned no job")
	}

	close(gate)

	final := await(t, s, upgrade.ID)
	if final.Status != StatusCancelled {
		t.Fatalf("disk_upgrade_data status = %s, want %s", final.Status, StatusCancelled)
	}
	if got := abort.count(); got != 1 {
		t.Fatalf("abort ran %d times, want exactly 1 before the job was recorded cancelled", got)
	}

	select {
	case <-startedUpgrade:
		t.Fatal("disk_upgrade_data's RunFunc ran despite being cancelled while its start-time backup was dispatching")
	default:
	}
}

// TestDispatch_DispatchingDiskUpgradeDataCancelWithFailingAbortEndsInterrupted
// is the same window as above, except Unwind itself fails: the job must
// end interrupted, with the abort's own error recorded, never cancelled —
// doc 02 §4 invariant 3 (cancelled means Unwind succeeded) and E3's own
// "Interrupted at none to diffing" row (a failed Unwind during a cancel
// leaves the job interrupted, disk_upgrade_cleanup_failed) both require
// it. Fails against 325b7ae: abort ran 0 times and the job still ended
// cancelled rather than interrupted.
func TestDispatch_DispatchingDiskUpgradeDataCancelWithFailingAbortEndsInterrupted(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	startedUpgrade, _ := registerBlocking(s, TypeDiskUpgradeData, true)
	abortErr := errors.New("unwind: unmount /mnt/disk1 busy")
	abort := &countingAbort{retErr: abortErr}
	s.registry.RegisterAbort(TypeDiskUpgradeData, abort.run)

	upgrade := seedInterruptedDiskUpgradeData(t, s, disk.DataDiskUpgradeCheckpoint{Phase: disk.DataDiskUpgradePhaseCopying})

	gate := make(chan struct{})
	backup := &gatedBackup{gates: []chan struct{}{gate}}
	s.SetTopologyBackup(backup)

	releaseMover := resumeDiskUpgradeDataBehindMover(t, s, upgrade)
	close(releaseMover)

	waitFor(t, time.Second, func() bool { return backup.count() == 1 })

	if _, err := s.Cancel(ctx, upgrade.ID); err != nil {
		t.Fatalf("Cancel while disk_upgrade_data's start-time backup is dispatching: %v", err)
	}

	close(gate)

	final := await(t, s, upgrade.ID)
	if final.Status != StatusInterrupted {
		t.Fatalf("disk_upgrade_data status = %s, want %s — a failed Unwind must never be reported as cancelled", final.Status, StatusInterrupted)
	}
	if !strings.Contains(final.ErrorMessage, abortErr.Error()) {
		t.Fatalf("disk_upgrade_data error message = %q, want it to mention %q", final.ErrorMessage, abortErr.Error())
	}
	if got := abort.count(); got != 1 {
		t.Fatalf("abort ran %d times, want exactly 1", got)
	}

	select {
	case <-startedUpgrade:
		t.Fatal("disk_upgrade_data's RunFunc ran despite being cancelled while its start-time backup was dispatching")
	default:
	}
}

// TestDispatch_DispatchingDiskUpgradeDataBackupFailureRunsAbortBeforeFailed
// is the #408 finding's "same root cause" case: a resumed data-disk
// upgrade's start-time backup itself fails once its turn comes. It must
// not be recorded failed until Unwind has run and succeeded — doc 02 §4
// invariant 3 (`failed` means Unwind succeeded) applies here exactly as it
// does to a cancel. Fails against 325b7ae: abort ran 0 times before the
// job was recorded failed.
func TestDispatch_DispatchingDiskUpgradeDataBackupFailureRunsAbortBeforeFailed(t *testing.T) {
	s := newTestScheduler(t)

	startedUpgrade, _ := registerBlocking(s, TypeDiskUpgradeData, true)
	abort := &countingAbort{}
	s.registry.RegisterAbort(TypeDiskUpgradeData, abort.run)

	upgrade := seedInterruptedDiskUpgradeData(t, s, disk.DataDiskUpgradeCheckpoint{Phase: disk.DataDiskUpgradePhaseCopying})

	backupErr := errors.New("backup: destination unreachable")
	s.SetTopologyBackup(&fakeBackup{err: backupErr})

	releaseMover := resumeDiskUpgradeDataBehindMover(t, s, upgrade)
	close(releaseMover)

	final := await(t, s, upgrade.ID)
	if final.Status != StatusFailed {
		t.Fatalf("disk_upgrade_data status = %s, want %s", final.Status, StatusFailed)
	}
	if !strings.Contains(final.ErrorMessage, backupErr.Error()) {
		t.Fatalf("disk_upgrade_data error message = %q, want it to mention %q", final.ErrorMessage, backupErr.Error())
	}
	if got := abort.count(); got != 1 {
		t.Fatalf("abort ran %d times, want exactly 1 before the job was recorded failed", got)
	}

	select {
	case <-startedUpgrade:
		t.Fatal("disk_upgrade_data's RunFunc ran despite its own pre-topology backup failing")
	default:
	}
}

// TestDispatch_DispatchingDiskUpgradeDataBackupFailureWithFailingAbortEndsInterrupted
// covers a backup failure and a failed Unwind together: the job must end
// interrupted with the abort's own error, not the backup's, and never
// failed — same reasoning as the cancel case above.
func TestDispatch_DispatchingDiskUpgradeDataBackupFailureWithFailingAbortEndsInterrupted(t *testing.T) {
	s := newTestScheduler(t)

	startedUpgrade, _ := registerBlocking(s, TypeDiskUpgradeData, true)
	abortErr := errors.New("unwind: unmount /mnt/disk1 busy")
	abort := &countingAbort{retErr: abortErr}
	s.registry.RegisterAbort(TypeDiskUpgradeData, abort.run)

	upgrade := seedInterruptedDiskUpgradeData(t, s, disk.DataDiskUpgradeCheckpoint{Phase: disk.DataDiskUpgradePhaseCopying})

	backupErr := errors.New("backup: destination unreachable")
	s.SetTopologyBackup(&fakeBackup{err: backupErr})

	releaseMover := resumeDiskUpgradeDataBehindMover(t, s, upgrade)
	close(releaseMover)

	final := await(t, s, upgrade.ID)
	if final.Status != StatusInterrupted {
		t.Fatalf("disk_upgrade_data status = %s, want %s — a failed Unwind must never be reported as failed", final.Status, StatusInterrupted)
	}
	if !strings.Contains(final.ErrorMessage, abortErr.Error()) {
		t.Fatalf("disk_upgrade_data error message = %q, want it to mention the abort's own error %q, not the backup's", final.ErrorMessage, abortErr.Error())
	}
	if got := abort.count(); got != 1 {
		t.Fatalf("abort ran %d times, want exactly 1", got)
	}

	select {
	case <-startedUpgrade:
		t.Fatal("disk_upgrade_data's RunFunc ran despite its own pre-topology backup failing")
	default:
	}
}

// TestDispatch_DispatchingDiskUpgradeDataAtReleasingBackupFailureEndsInterruptedResumable
// is #408's own reopened finding: a data-disk upgrade resumed at its
// releasing checkpoint is past its release decision, so doc 02 §4
// invariant 4 leaves `succeeded` as the only outcome — Cancel already
// refuses it with ErrDiskUpgradePastRelease rather than running Unwind on
// it. A failed start-time backup must respect that same invariant: the
// job must end interrupted at its unchanged checkpoint, never failed, and
// Unwind must never run for it at all, since there is nothing for it to
// undo. It must also stay genuinely resumable: given a working backup, the
// same job runs on to success from the same checkpoint. Fails against
// 8784f25: recorded failed (`pre_topology_backup_failed`) with Unwind
// having run once.
func TestDispatch_DispatchingDiskUpgradeDataAtReleasingBackupFailureEndsInterruptedResumable(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	startedUpgrade, releaseUpgrade := registerBlocking(s, TypeDiskUpgradeData, true)
	abort := &countingAbort{}
	s.registry.RegisterAbort(TypeDiskUpgradeData, abort.run)

	upgrade := seedInterruptedDiskUpgradeData(t, s, disk.DataDiskUpgradeCheckpoint{Phase: disk.DataDiskUpgradePhaseReleasing})

	backupErr := errors.New("backup: destination unreachable")
	s.SetTopologyBackup(&fakeBackup{err: backupErr})

	releaseMover := resumeDiskUpgradeDataBehindMover(t, s, upgrade)
	close(releaseMover)

	final := await(t, s, upgrade.ID)
	if final.Status != StatusInterrupted {
		t.Fatalf("disk_upgrade_data status = %s, want %s — past release, invariant 4 leaves succeeded as the only outcome; a failed backup must never end it failed", final.Status, StatusInterrupted)
	}
	if !strings.Contains(final.ErrorMessage, backupErr.Error()) {
		t.Fatalf("disk_upgrade_data error message = %q, want it to mention %q", final.ErrorMessage, backupErr.Error())
	}
	if got := abort.count(); got != 0 {
		t.Fatalf("abort (Unwind) ran %d times, want 0 — Cancel already refuses this job past release, so its start-time backup failing must not run Unwind either", got)
	}
	if !diskUpgradeDataCheckpointAtReleasing(final.Checkpoint) {
		t.Fatalf("disk_upgrade_data checkpoint = %s, want it to remain at releasing so the job is still resumable", final.Checkpoint)
	}
	if !final.Resumable {
		t.Fatal("disk_upgrade_data is no longer resumable after a failed start-time backup at releasing")
	}

	select {
	case <-startedUpgrade:
		t.Fatal("disk_upgrade_data's RunFunc ran despite its own pre-topology backup failing")
	default:
	}

	s.SetTopologyBackup(&fakeBackup{})
	if _, err := s.Resume(ctx, upgrade.ID); err != nil {
		t.Fatalf("Resume after a backup-failed interrupt at releasing: %v", err)
	}
	<-startedUpgrade
	close(releaseUpgrade)

	succeeded := await(t, s, upgrade.ID)
	if succeeded.Status != StatusSucceeded {
		t.Fatalf("disk_upgrade_data status after resuming past the failed backup = %s, want %s", succeeded.Status, StatusSucceeded)
	}
}
