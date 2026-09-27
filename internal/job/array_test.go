package job

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// fakeArrayService is ArraySequence's Service fake (CLAUDE.md): it
// records every Stop/Start call, in order, into a shared log, and can be
// scripted to fail either one — the "a container refuses to release an
// open file" scenario this file's central safety test reproduces. cancel,
// when set, is called the instant Start or Stop runs, before either
// checks ctx — standing in for a real ArrayService whose own systemctl
// call uses exec.CommandContext: a client that disconnects while that
// call is in flight cancels the request context out from under it, and
// the call returns ctx.Err() (here, whatever ctx.Err() reports after
// cancel runs) the same way a real one would.
type fakeArrayService struct {
	name     string
	stopErr  error
	startErr error
	cancel   context.CancelFunc
	log      *[]string
}

func (f *fakeArrayService) Name() string { return f.name }

func (f *fakeArrayService) Stop(ctx context.Context) error {
	*f.log = append(*f.log, "stop:"+f.name)
	if f.cancel != nil {
		f.cancel()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.stopErr
}

func (f *fakeArrayService) Start(ctx context.Context) error {
	*f.log = append(*f.log, "start:"+f.name)
	if f.cancel != nil {
		f.cancel()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.startErr
}

// fakeArrayMount is ArraySequence's Mount fake.
type fakeArrayMount struct {
	where      string
	mountErr   error
	unmountErr error
	log        *[]string
}

func (f *fakeArrayMount) Where() string { return f.where }

func (f *fakeArrayMount) Mount(ctx context.Context) error {
	*f.log = append(*f.log, "mount:"+f.where)
	return f.mountErr
}

func (f *fakeArrayMount) Unmount(ctx context.Context) error {
	*f.log = append(*f.log, "unmount:"+f.where)
	return f.unmountErr
}

// fakeStorageTarget is ArraySequence's StorageTarget fake (#372): it
// records when ConfirmReady ran (relative to whatever else writes
// into the same log) and can be scripted to fail, so a test can assert
// both the ordering Start owes it and that a failure there blocks every
// Service from starting. closeLog, when set, records every persist value
// Close was called with (#387) separately from log, so a test
// can assert Close's own argument without disturbing every other test's
// plain "close" log entry.
type fakeStorageTarget struct {
	err          error
	closeErr     error
	openErr      error
	recloseErr   error
	log          *[]string
	closeLog     *[]bool
	recloseCalls int
}

func (f *fakeStorageTarget) ConfirmReady(ctx context.Context, seq *ArraySequence) error {
	*f.log = append(*f.log, "confirmready")
	return f.err
}

// Close, like a real storageTargetSync.Close, makes exec.CommandContext
// calls of its own (stopping Docker/libvirt and the gate unit) — so this
// fake fails the same way a real one would if ctx is already cancelled
// when it runs, letting a test prove a compensating call runs on an
// uncancellable context rather than merely trusting that it does.
func (f *fakeStorageTarget) Close(ctx context.Context, persist bool) error {
	*f.log = append(*f.log, "close")
	if f.closeLog != nil {
		*f.closeLog = append(*f.closeLog, persist)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.closeErr
}

func (f *fakeStorageTarget) Open(ctx context.Context) error {
	*f.log = append(*f.log, "open")
	return f.openErr
}

// Reclose only ever writes a local flag file (storageTargetSync.Reclose
// never takes ctx as an argument to anything that can be cancelled), so
// unlike Close it does not fail on a cancelled ctx.
func (f *fakeStorageTarget) Reclose(ctx context.Context) error {
	*f.log = append(*f.log, "reclose")
	f.recloseCalls++
	return f.recloseErr
}

func sliceEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v (differs at index %d)", got, want, i)
		}
	}
}

// TestArraySequence_Stop_ServiceMustStopBeforeAnyUnmount is this issue's
// central data-loss-prevention test (doc 02 §4, safety-critical): a
// service that will not stop — a container still holding a file open on
// the pool is the literal doc 02 §4 example — must hold the whole stop
// sequence, not be skipped past on the way to unmounting storage under it.
// The scenario this reproduces: unmounting while a container's write is
// still in flight would let that write either be lost outright or land
// on whatever now-empty directory stands in for the mount, exactly the
// "container's write survives an unmount because the sequence didn't wait
// for it to stop" failure this dispatch calls out.
func TestArraySequence_Stop_ServiceMustStopBeforeAnyUnmount(t *testing.T) {
	var log []string
	s := newTestScheduler(t)

	container := &fakeArrayService{name: "container", stopErr: errors.New("container still holds /mnt/user/media/movie.mkv open"), log: &log}
	share := &fakeArrayMount{where: "/mnt/user/media", log: &log}
	catchAll := &fakeArrayMount{where: "/mnt/user", log: &log}
	disk1 := &fakeArrayMount{where: "/mnt/disk1", log: &log}

	seq := ArraySequence{
		Scheduler:   s,
		Services:    []ArrayService{container},
		ShareMounts: []ArrayMount{share},
		CatchAll:    catchAll,
		Disks:       []ArrayMount{disk1},
	}

	err := seq.Stop(context.Background())
	if err == nil {
		t.Fatal("Stop: got nil error, want the container's stop failure to propagate")
	}

	for _, m := range log {
		if m == "unmount:/mnt/user/media" || m == "unmount:/mnt/user" || m == "unmount:/mnt/disk1" {
			t.Fatalf("Stop unmounted %q after the container failed to stop — this is the exact data-loss scenario doc 02 §4 exists to prevent; full log: %v", m, log)
		}
	}
	sliceEqual(t, log, []string{"stop:container"})

	if !s.InMaintenance() {
		t.Fatal("Stop: scheduler must stay in maintenance mode after a failed stop, so nothing new can start against a half-stopped array")
	}
}

// TestArraySequence_Stop_ClosesStorageTargetGateAfterServicesBeforeUnmounts
// is #387's own ordering test for the storage-target gate closing on
// `array stop` (#372): the gate must close after
// Services (Samba/NFS have already stopped their own way) but before any
// unmount, exactly like a Service itself — a refusal to close it must hold
// the whole sequence up rather than being skipped past on the way to
// unmounting.
func TestArraySequence_Stop_ClosesStorageTargetGateAfterServicesBeforeUnmounts(t *testing.T) {
	var log []string
	s := newTestScheduler(t)

	svc := &fakeArrayService{name: "Samba", log: &log}
	target := &fakeStorageTarget{log: &log}
	catchAll := &fakeArrayMount{where: "/mnt/user", log: &log}

	seq := ArraySequence{
		Scheduler:     s,
		Services:      []ArrayService{svc},
		StorageTarget: target,
		CatchAll:      catchAll,
	}

	if err := seq.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	sliceEqual(t, log, []string{"stop:Samba", "close", "unmount:/mnt/user"})
}

// TestArraySequence_Stop_StorageTargetCloseFailureBlocksUnmounts proves a
// Close failure — Docker or libvirt refusing to stop, or the gate unit
// itself refusing to stop — holds Stop up exactly like a refused Samba or
// NFS stop: nothing unmounts, and maintenance mode stays on so an operator
// can resolve it and retry, rather than continuing to unmount storage a
// still-running service might hold open.
func TestArraySequence_Stop_StorageTargetCloseFailureBlocksUnmounts(t *testing.T) {
	var log []string
	s := newTestScheduler(t)

	target := &fakeStorageTarget{closeErr: errors.New("docker.service: still stopping"), log: &log}
	catchAll := &fakeArrayMount{where: "/mnt/user", log: &log}

	seq := ArraySequence{Scheduler: s, StorageTarget: target, CatchAll: catchAll}

	err := seq.Stop(context.Background())
	if err == nil {
		t.Fatal("Stop: got nil error, want the storage-target Close failure to propagate")
	}
	for _, m := range log {
		if m == "unmount:/mnt/user" {
			t.Fatalf("Stop unmounted %q after the storage-target gate failed to close; full log: %v", m, log)
		}
	}
	sliceEqual(t, log, []string{"close"})
	if !s.InMaintenance() {
		t.Fatal("Stop: scheduler must stay in maintenance mode after a failed gate close")
	}
}

// TestArraySequence_Stop_WaitsForARunningJobToFinishBeforeStoppingServices
// reproduces the gap EnterMaintenance alone leaves open: it only signals a
// running job to stop and returns immediately, so without Drain, Stop
// would proceed straight into stopping services and unmounting storage
// while the job's own goroutine might still be mid-write (doc 02 §4) —
// exactly the data-loss scenario this sequence exists to prevent, except
// the unprotected writer would be Hoserva's own in-process job rather
// than an external service.
func TestArraySequence_Stop_WaitsForARunningJobToFinishBeforeStoppingServices(t *testing.T) {
	var log []string
	s := newTestScheduler(t)
	started, release := registerBlocking(s, TypeMover, false)
	if _, err := s.Submit(context.Background(), TypeMover, nil, nil); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	<-started

	svc := &fakeArrayService{name: "container", log: &log}
	seq := ArraySequence{Scheduler: s, Services: []ArrayService{svc}}

	stopDone := make(chan error, 1)
	go func() { stopDone <- seq.Stop(context.Background()) }()

	select {
	case <-stopDone:
		t.Fatal("Stop returned while the running job was still mid-write — it must wait for the job to finish first")
	case <-time.After(50 * time.Millisecond):
	}
	if len(log) != 0 {
		t.Fatalf("Stop touched services (%v) before the running job finished", log)
	}

	close(release)
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stop did not return after the running job finished")
	}
	sliceEqual(t, log, []string{"stop:container"})
}

// TestArraySequence_Stop_DrainRespectsContextDeadline confirms Stop does
// not hang forever behind a job that never honors maintenance mode's
// signal — the caller's ctx still bounds the wait, and maintenance mode
// is left active exactly as it is for any other Stop failure.
func TestArraySequence_Stop_DrainRespectsContextDeadline(t *testing.T) {
	s := newTestScheduler(t)
	_, release := registerBlocking(s, TypeMover, false)
	j, err := s.Submit(context.Background(), TypeMover, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	// Release the blocking job and wait for its goroutine to finish
	// recording its final status before t.Cleanup closes the test DB
	// (newTestDB's own t.Cleanup, registered earlier and so run after
	// this one) — otherwise that write races the DB close.
	t.Cleanup(func() {
		close(release)
		await(t, s, j.ID)
	})

	seq := ArraySequence{Scheduler: s}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	if err := seq.Stop(ctx); err == nil {
		t.Fatal("Stop: got nil error, want the context deadline to propagate while the job is still running")
	}
	if !s.InMaintenance() {
		t.Fatal("Stop: maintenance mode must stay active when the drain wait times out")
	}
}

func TestArraySequence_Stop_EntersMaintenanceBeforeStoppingAnyService(t *testing.T) {
	var log []string
	s := newTestScheduler(t)

	var inMaintenanceAtStopCall bool
	svc := &recordingService{name: "vm", log: &log, onStop: func() {
		inMaintenanceAtStopCall = s.InMaintenance()
	}}

	seq := ArraySequence{Scheduler: s, Services: []ArrayService{svc}}
	if err := seq.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !inMaintenanceAtStopCall {
		t.Fatal("Stop: maintenance mode was not active yet when the first service's Stop ran")
	}
}

// recordingService lets a test observe scheduler state exactly at the
// moment Stop is called, which a plain fakeArrayService (recording only
// into a string log) cannot express.
type recordingService struct {
	name   string
	log    *[]string
	onStop func()
}

func (r *recordingService) Name() string { return r.name }
func (r *recordingService) Stop(ctx context.Context) error {
	*r.log = append(*r.log, "stop:"+r.name)
	if r.onStop != nil {
		r.onStop()
	}
	return nil
}
func (r *recordingService) Start(ctx context.Context) error {
	*r.log = append(*r.log, "start:"+r.name)
	return nil
}

func TestArraySequence_Stop_FullOrderingWhenEverythingSucceeds(t *testing.T) {
	var log []string
	s := newTestScheduler(t)

	vm := &fakeArrayService{name: "vm", log: &log}
	container := &fakeArrayService{name: "container", log: &log}
	share1 := &fakeArrayMount{where: "/mnt/user/media", log: &log}
	share2 := &fakeArrayMount{where: "/mnt/user/backups", log: &log}
	catchAll := &fakeArrayMount{where: "/mnt/user", log: &log}
	disk1 := &fakeArrayMount{where: "/mnt/disk1", log: &log}
	disk2 := &fakeArrayMount{where: "/mnt/disk2", log: &log}

	seq := ArraySequence{
		Scheduler:   s,
		Services:    []ArrayService{vm, container},
		ShareMounts: []ArrayMount{share1, share2},
		CatchAll:    catchAll,
		Disks:       []ArrayMount{disk1, disk2},
	}

	if err := seq.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	sliceEqual(t, log, []string{
		"stop:vm", "stop:container",
		"unmount:/mnt/user/media", "unmount:/mnt/user/backups",
		"unmount:/mnt/user",
		"unmount:/mnt/disk1", "unmount:/mnt/disk2",
	})
}

func TestArraySequence_Start_ReversesStopOrderAndExitsMaintenance(t *testing.T) {
	var log []string
	s := newTestScheduler(t)
	if err := s.EnterMaintenance(context.Background()); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}

	vm := &fakeArrayService{name: "vm", log: &log}
	container := &fakeArrayService{name: "container", log: &log}
	share1 := &fakeArrayMount{where: "/mnt/user/media", log: &log}
	catchAll := &fakeArrayMount{where: "/mnt/user", log: &log}
	disk1 := &fakeArrayMount{where: "/mnt/disk1", log: &log}
	disk2 := &fakeArrayMount{where: "/mnt/disk2", log: &log}

	seq := ArraySequence{
		Scheduler:   s,
		Services:    []ArrayService{vm, container}, // stop order
		ShareMounts: []ArrayMount{share1},
		CatchAll:    catchAll,
		Disks:       []ArrayMount{disk1, disk2},
	}

	if err := seq.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	sliceEqual(t, log, []string{
		"mount:/mnt/disk1", "mount:/mnt/disk2",
		"mount:/mnt/user",
		"mount:/mnt/user/media",
		"start:container", "start:vm", // reverse of stop order
	})

	if s.InMaintenance() {
		t.Fatal("Start: maintenance mode must be exited once every step succeeds")
	}
}

// TestArraySequence_Start_StorageTargetRunsBeforeServices is #372 finding
// 1's own regression test: without the hook, Start's own Services loop
// calls systemctl start against a unit whose drop-in binds it to
// hoserva-storage.target while the readiness flag behind that target is
// still whatever an earlier, not-ready boot left it as — reproduced here
// by the log ordering StorageTarget.ConfirmReady must appear in, after
// every mount and before any service starts.
func TestArraySequence_Start_StorageTargetRunsBeforeServices(t *testing.T) {
	var log []string
	svc := &fakeArrayService{name: "nfs", log: &log}
	share := &fakeArrayMount{where: "/mnt/user/media", log: &log}
	catchAll := &fakeArrayMount{where: "/mnt/user", log: &log}
	disk1 := &fakeArrayMount{where: "/mnt/disk1", log: &log}
	target := &fakeStorageTarget{log: &log}

	seq := ArraySequence{
		Services:      []ArrayService{svc},
		ShareMounts:   []ArrayMount{share},
		CatchAll:      catchAll,
		Disks:         []ArrayMount{disk1},
		StorageTarget: target,
	}

	if err := seq.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	sliceEqual(t, log, []string{
		"open",
		"mount:/mnt/disk1",
		"mount:/mnt/user",
		"mount:/mnt/user/media",
		"confirmready",
		"start:nfs",
	})
}

// TestArraySequence_Start_StorageTargetFailureBlocksServices proves a
// ConfirmReady failure — the pool never actually confirmed mounted, or
// the units failed to write — stops Start before any Service starts,
// exactly like every other Start failure (doc 02 §4 UR9's own DiskCheck
// failure does the same for a disk mismatch): a client must never be
// told the array is up, or have Samba/NFS started, over a storage gate
// this call could not bring current.
func TestArraySequence_Start_StorageTargetFailureBlocksServices(t *testing.T) {
	var log []string
	svc := &fakeArrayService{name: "nfs", log: &log}
	catchAll := &fakeArrayMount{where: "/mnt/user", log: &log}
	target := &fakeStorageTarget{err: errors.New("mnt-user.mount is not mounted"), log: &log}

	seq := ArraySequence{
		Services:      []ArrayService{svc},
		CatchAll:      catchAll,
		StorageTarget: target,
	}

	if err := seq.Start(context.Background()); err == nil {
		t.Fatal("Start: got nil error, want the StorageTarget failure to propagate")
	}

	for _, m := range log {
		if m == "start:nfs" {
			t.Fatalf("Start started %q after StorageTarget.ConfirmReady failed; full log: %v", m, log)
		}
	}
}

// TestArraySequence_Start_OpenFailureBlocksAnyMount is #387's own
// regression: every generated mount unit now carries ConditionPathExists=
// against the array-stopped flag Close sets, so Start must clear it
// (StorageTarget.Open) before its own first Mount call — a failure there
// must stop Start before any disk mounts, exactly like every other
// pre-mount Start failure, and must restore the "stop sequence completed"
// state Q70's data-disk-upgrade admission depends on.
func TestArraySequence_Start_OpenFailureBlocksAnyMount(t *testing.T) {
	ctx := context.Background()
	var log []string
	s := newTestScheduler(t)
	if err := s.EnterMaintenance(ctx); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}
	s.MarkArrayStopped()

	disk1 := &fakeArrayMount{where: "/mnt/disk1", log: &log}
	target := &fakeStorageTarget{openErr: errors.New("removing the array-stopped flag: permission denied"), log: &log}

	seq := ArraySequence{Scheduler: s, Disks: []ArrayMount{disk1}, StorageTarget: target}

	if err := seq.Start(ctx); err == nil {
		t.Fatal("Start: got nil error, want the StorageTarget.Open failure to propagate")
	}
	sliceEqual(t, log, []string{"open"})
	s.mu.Lock()
	err := s.admitDiskUpgradeDataLocked(ctx)
	s.mu.Unlock()
	if err != nil {
		t.Fatalf("admitDiskUpgradeDataLocked after a failed Open = %v, want nil — a start that never mounted anything must restore as still stopped", err)
	}
}

// TestArraySequence_Stop_ClosesStorageTargetPersisted proves Stop (a user
// `array stop`) closes the storage-target gate with persist=true (#387):
// only that call may leave the durable array-stopped
// condition flag in force.
func TestArraySequence_Stop_ClosesStorageTargetPersisted(t *testing.T) {
	var log []string
	var closeLog []bool
	s := newTestScheduler(t)
	target := &fakeStorageTarget{log: &log, closeLog: &closeLog}
	catchAll := &fakeArrayMount{where: "/mnt/user", log: &log}

	seq := ArraySequence{Scheduler: s, StorageTarget: target, CatchAll: catchAll}
	if err := seq.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(closeLog) != 1 || !closeLog[0] {
		t.Fatalf("Close persist argument = %v, want [true] for a user `array stop`", closeLog)
	}
}

// TestArraySequence_StopForShutdown_ClosesStorageTargetTransient proves
// StopForShutdown (a reboot or a UPS low-battery shutdown) closes the
// storage-target gate with persist=false (#387): a flag left in
// force here would fail every mount unit's own ConditionPathExists in the
// boot-time systemd transaction that runs before hoservad is even
// exec'd, stranding the array unmounted after an ordinary reboot.
func TestArraySequence_StopForShutdown_ClosesStorageTargetTransient(t *testing.T) {
	var log []string
	var closeLog []bool
	s := newTestScheduler(t)
	target := &fakeStorageTarget{log: &log, closeLog: &closeLog}
	catchAll := &fakeArrayMount{where: "/mnt/user", log: &log}

	seq := ArraySequence{Scheduler: s, StorageTarget: target, CatchAll: catchAll}
	if err := seq.StopForShutdown(context.Background()); err != nil {
		t.Fatalf("StopForShutdown: %v", err)
	}
	if len(closeLog) != 1 || closeLog[0] {
		t.Fatalf("Close persist argument = %v, want [false] for a reboot or UPS shutdown", closeLog)
	}
}

// fakeArrayDiskCheck is ArraySequence's DiskCheck fake (doc 02 §4 UR9).
type fakeArrayDiskCheck struct{ err error }

func (f fakeArrayDiskCheck) ConfirmArrayDisks(ctx context.Context) error { return f.err }

// TestArraySequence_Start_DiskMountFailureReclosesStorageTarget proves
// (#387) Open has already cleared the array-stopped
// condition flag before this loop runs, so a disk mount failure — before
// any Service ever starts — must restore it, or a still-persisted
// maintenance state would leave the flag missing.
func TestArraySequence_Start_DiskMountFailureReclosesStorageTarget(t *testing.T) {
	var log []string
	s := newTestScheduler(t)
	if err := s.EnterMaintenance(context.Background()); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}
	s.MarkArrayStopped()

	disk1 := &fakeArrayMount{where: "/mnt/disk1", mountErr: errors.New("device timeout"), log: &log}
	target := &fakeStorageTarget{log: &log}

	seq := ArraySequence{Scheduler: s, Disks: []ArrayMount{disk1}, StorageTarget: target}
	if err := seq.Start(context.Background()); err == nil {
		t.Fatal("Start: got nil error, want the disk mount failure to propagate")
	}
	if target.recloseCalls != 1 {
		t.Fatalf("Reclose called %d times, want 1 after a failed disk mount", target.recloseCalls)
	}
}

// TestArraySequence_Start_DiskCheckMismatchReclosesStorageTarget proves
// (#387) UR9's own disk-identity check: a
// mismatch unmounts the disks again and restores the scheduler's own
// "stop sequence completed" state, and must also restore the durable
// array-stopped condition flag, never leave it missing.
func TestArraySequence_Start_DiskCheckMismatchReclosesStorageTarget(t *testing.T) {
	var log []string
	s := newTestScheduler(t)
	if err := s.EnterMaintenance(context.Background()); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}
	s.MarkArrayStopped()

	disk1 := &fakeArrayMount{where: "/mnt/disk1", log: &log}
	target := &fakeStorageTarget{log: &log}
	check := fakeArrayDiskCheck{err: fmt.Errorf("%w: /mnt/disk1 holds a different filesystem", ErrArrayDiskMismatch)}

	seq := ArraySequence{Scheduler: s, Disks: []ArrayMount{disk1}, DiskCheck: check, StorageTarget: target}
	if err := seq.Start(context.Background()); err == nil {
		t.Fatal("Start: got nil error, want the disk-identity mismatch to propagate")
	}
	if target.recloseCalls != 1 {
		t.Fatalf("Reclose called %d times, want 1 after a disk-identity mismatch", target.recloseCalls)
	}
}

// TestArraySequence_Start_CatchAllMountFailureReclosesStorageTarget proves
// (#387) the catch-all's own mount step restores the flag on failure.
func TestArraySequence_Start_CatchAllMountFailureReclosesStorageTarget(t *testing.T) {
	var log []string
	s := newTestScheduler(t)
	if err := s.EnterMaintenance(context.Background()); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}
	s.MarkArrayStopped()

	catchAll := &fakeArrayMount{where: "/mnt/user", mountErr: errors.New("mergerfs: no such device"), log: &log}
	target := &fakeStorageTarget{log: &log}

	seq := ArraySequence{Scheduler: s, CatchAll: catchAll, StorageTarget: target}
	if err := seq.Start(context.Background()); err == nil {
		t.Fatal("Start: got nil error, want the catch-all mount failure to propagate")
	}
	if target.recloseCalls != 1 {
		t.Fatalf("Reclose called %d times, want 1 after a failed catch-all mount", target.recloseCalls)
	}
}

// TestArraySequence_Start_ShareMountFailureReclosesStorageTarget proves
// (#387) a per-share mount's own step restores the flag on failure.
func TestArraySequence_Start_ShareMountFailureReclosesStorageTarget(t *testing.T) {
	var log []string
	s := newTestScheduler(t)
	if err := s.EnterMaintenance(context.Background()); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}
	s.MarkArrayStopped()

	share := &fakeArrayMount{where: "/mnt/user/media", mountErr: errors.New("mergerfs: no such device"), log: &log}
	target := &fakeStorageTarget{log: &log}

	seq := ArraySequence{Scheduler: s, ShareMounts: []ArrayMount{share}, StorageTarget: target}
	if err := seq.Start(context.Background()); err == nil {
		t.Fatal("Start: got nil error, want the share mount failure to propagate")
	}
	if target.recloseCalls != 1 {
		t.Fatalf("Reclose called %d times, want 1 after a failed share mount", target.recloseCalls)
	}
}

// TestArraySequence_Start_ConfirmReadyFailureRollsBackToStopped proves a
// ConfirmReady failure — which lands before ConfirmReady ever opens the
// storage-target gate or starts Docker/libvirt — still rolls the whole
// sequence back through Close(persist=true), not a plain Reclose: unlike
// the pre-ConfirmReady failures, every disk, catch-all and share mount
// call Start already made succeeded and needs unmounting, not just the
// durable flag restored, so every one of those mounts is unmounted again
// in stop order, Close finds the gate already closed and leaves the
// durable flag in force, no Service is stopped (none had started yet —
// the Services loop runs after ConfirmReady), maintenance stays
// persisted, and the original failure is still in the returned error.
func TestArraySequence_Start_ConfirmReadyFailureRollsBackToStopped(t *testing.T) {
	var log []string
	var closeLog []bool
	s := newTestScheduler(t)
	if err := s.EnterMaintenance(context.Background()); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}
	s.MarkArrayStopped()

	confirmErr := errors.New("mnt-user.mount is not mounted")
	svc := &fakeArrayService{name: "nfs", log: &log}
	disk1 := &fakeArrayMount{where: "/mnt/disk1", log: &log}
	catchAll := &fakeArrayMount{where: "/mnt/user", log: &log}
	share := &fakeArrayMount{where: "/mnt/user/media", log: &log}
	target := &fakeStorageTarget{err: confirmErr, log: &log, closeLog: &closeLog}

	seq := ArraySequence{
		Scheduler:     s,
		Services:      []ArrayService{svc},
		ShareMounts:   []ArrayMount{share},
		CatchAll:      catchAll,
		Disks:         []ArrayMount{disk1},
		StorageTarget: target,
	}
	err := seq.Start(context.Background())
	if err == nil {
		t.Fatal("Start: got nil error, want the ConfirmReady failure to propagate")
	}
	if !errors.Is(err, confirmErr) {
		t.Fatalf("Start error %v does not wrap the original ConfirmReady failure %v", err, confirmErr)
	}
	sliceEqual(t, log, []string{
		"open",
		"mount:/mnt/disk1", "mount:/mnt/user", "mount:/mnt/user/media",
		"confirmready",
		"close",
		"unmount:/mnt/user/media", "unmount:/mnt/user", "unmount:/mnt/disk1",
	})
	if len(closeLog) != 1 || !closeLog[0] {
		t.Fatalf("Close persist argument = %v, want [true] so the durable array-stopped flag is left in force", closeLog)
	}
	if target.recloseCalls != 0 {
		t.Fatalf("Reclose called %d times, want 0 — a ConfirmReady failure rolls back through Close, never Reclose", target.recloseCalls)
	}
	if !s.InMaintenance() {
		t.Fatal("Start: maintenance mode must stay active when ConfirmReady fails")
	}
}

// TestArraySequence_Start_ServiceStartFailureRollsBackToStopped proves a
// Samba/NFS Service.Start failure that lands after another service already
// started rolls back through Close(persist=true): only the service that
// actually started is stopped (never the one whose own Start just failed,
// and never one the reverse-order loop had not reached yet), the gate
// closes with the durable flag restored, every mount unmounts in stop
// order, maintenance stays persisted, and the original failure is still in
// the returned error.
func TestArraySequence_Start_ServiceStartFailureRollsBackToStopped(t *testing.T) {
	var log []string
	var closeLog []bool
	s := newTestScheduler(t)
	if err := s.EnterMaintenance(context.Background()); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}
	s.MarkArrayStopped()

	startErr := errors.New("nfs-kernel-server.service failed to start")
	vm := &fakeArrayService{name: "vm", startErr: startErr, log: &log}
	nfs := &fakeArrayService{name: "nfs", log: &log}
	catchAll := &fakeArrayMount{where: "/mnt/user", log: &log}
	target := &fakeStorageTarget{log: &log, closeLog: &closeLog}

	seq := ArraySequence{
		Scheduler:     s,
		Services:      []ArrayService{vm, nfs}, // stop order: vm, then nfs; start order (reversed): nfs, then vm
		CatchAll:      catchAll,
		StorageTarget: target,
	}
	err := seq.Start(context.Background())
	if err == nil {
		t.Fatal("Start: got nil error, want the Service start failure to propagate")
	}
	if !errors.Is(err, startErr) {
		t.Fatalf("Start error %v does not wrap the original Service start failure %v", err, startErr)
	}
	sliceEqual(t, log, []string{
		"open", "mount:/mnt/user", "confirmready",
		"start:nfs", "start:vm",
		"stop:nfs",
		"close", "unmount:/mnt/user",
	})
	if len(closeLog) != 1 || !closeLog[0] {
		t.Fatalf("Close persist argument = %v, want [true] so the durable array-stopped flag is left in force", closeLog)
	}
	if target.recloseCalls != 0 {
		t.Fatalf("Reclose called %d times, want 0 — a Service start failure rolls back through Close, never Reclose", target.recloseCalls)
	}
	if !s.InMaintenance() {
		t.Fatal("Start: maintenance mode must stay active when a Service fails to start")
	}
	// The rollback completed the stop sequence, so "stop completed" is
	// back in memory and in the persisted row — a data-disk upgrade stays
	// admissible without another `array stop`, across a restart too.
	if !s.arrayStopped {
		t.Fatal("arrayStopped = false after a rollback that completed the stop sequence")
	}
	s2 := schedulerOnSameDB(t, s)
	if err := s2.RestorePersistedMaintenance(context.Background()); err != nil {
		t.Fatalf("RestorePersistedMaintenance: %v", err)
	}
	if !s2.arrayStopped {
		t.Fatal("persisted array_stopped = false after a rollback that completed the stop sequence")
	}
}

// TestArraySequence_Start_ServiceStartFailureRollbackRunsOnAnUncancellableContext
// proves rollbackToStopped survives a client that disconnects the instant
// the failing Service.Start call it is rolling back from returns (#387):
// Start is reached from an HTTP request (the API's StartArray passes its
// own request context straight through), so the fake's own smbd service
// cancels that context itself, standing in for the request's client going
// away while `systemctl start smbd` (smbd's own real Start) is in flight.
// Without running the rollback on context.WithoutCancel, the rollback's
// own first step — stopping nfs, the one service Start had already
// brought up — would see the same cancelled context and fail immediately,
// exactly like a real exec.CommandContext call would: the rollback would
// never stop nfs and would never reach StorageTarget.Close to close the
// gate, so Docker, libvirt and every other running service would stay up
// and the pool would stay mounted, while the client that triggered all of
// it is already gone and never sees the error.
func TestArraySequence_Start_ServiceStartFailureRollbackRunsOnAnUncancellableContext(t *testing.T) {
	var log []string
	var closeLog []bool
	s := newTestScheduler(t)
	if err := s.EnterMaintenance(context.Background()); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}
	s.MarkArrayStopped()

	ctx, cancel := context.WithCancel(context.Background())
	nfs := &fakeArrayService{name: "nfs", log: &log}
	// smbd's own Start is the one a disconnecting client cancels — it
	// cancels ctx itself, then observes (and returns) the same
	// context.Canceled a real exec.CommandContext call would.
	smbd := &fakeArrayService{name: "smbd", cancel: cancel, log: &log}
	catchAll := &fakeArrayMount{where: "/mnt/user", log: &log}
	target := &fakeStorageTarget{log: &log, closeLog: &closeLog}

	seq := ArraySequence{
		Scheduler:     s,
		Services:      []ArrayService{smbd, nfs}, // stop order: smbd, then nfs; start order (reversed): nfs, then smbd
		CatchAll:      catchAll,
		StorageTarget: target,
	}
	err := seq.Start(ctx)
	if err == nil {
		t.Fatal("Start: got nil error, want the cancelled Service start to propagate")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Start error %v does not wrap context.Canceled", err)
	}
	sliceEqual(t, log, []string{
		"open", "mount:/mnt/user", "confirmready",
		"start:nfs", "start:smbd",
		"stop:nfs",
		"close", "unmount:/mnt/user",
	})
	if len(closeLog) != 1 || !closeLog[0] {
		t.Fatalf("Close persist argument = %v, want [true] — the rollback must still reach Close, restoring the durable flag, even though the caller's own context was cancelled", closeLog)
	}
	if target.recloseCalls != 0 {
		t.Fatalf("Reclose called %d times, want 0 — the rollback ran to completion through Close, so it never needed the Reclose fallback", target.recloseCalls)
	}
	if !s.InMaintenance() {
		t.Fatal("Start: maintenance mode must stay active when a Service fails to start")
	}
}

// TestArraySequence_Start_ServiceStopFailureDuringRollbackStillRestoresTheFlag
// proves the rollback's own fallback (#387): a Service that genuinely
// refuses to stop — not a cancelled context, an actual "still stopping"
// failure — can still leave stopSequence's own Services loop short of
// ever reaching StorageTarget.Close, the one place that normally restores
// the durable array-stopped flag. The persisted row (MarkArrayStopped/
// BeginArrayStart) already treats the array as stopped by this point, so
// the flag it depends on must never disagree with it even though the
// stuck service still needs an operator to clear it and retry — the same
// way a plain `array stop` leaves maintenance mode on and asks for a
// retry rather than silently declaring success (doc 02 §4).
func TestArraySequence_Start_ServiceStopFailureDuringRollbackStillRestoresTheFlag(t *testing.T) {
	var log []string
	var closeLog []bool
	s := newTestScheduler(t)
	if err := s.EnterMaintenance(context.Background()); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}
	s.MarkArrayStopped()

	startErr := errors.New("nfs-kernel-server.service failed to start")
	stopErr := errors.New("docker.service: still stopping")
	vm := &fakeArrayService{name: "vm", startErr: startErr, log: &log}
	nfs := &fakeArrayService{name: "nfs", stopErr: stopErr, log: &log} // refuses to stop during the rollback
	catchAll := &fakeArrayMount{where: "/mnt/user", log: &log}
	target := &fakeStorageTarget{log: &log, closeLog: &closeLog}

	seq := ArraySequence{
		Scheduler:     s,
		Services:      []ArrayService{vm, nfs}, // stop order: vm, then nfs; start order (reversed): nfs, then vm
		CatchAll:      catchAll,
		StorageTarget: target,
	}
	err := seq.Start(context.Background())
	if err == nil {
		t.Fatal("Start: got nil error, want the Service start failure to propagate")
	}
	if !errors.Is(err, startErr) {
		t.Fatalf("Start error %v does not wrap the original Service start failure %v", err, startErr)
	}
	if !errors.Is(err, stopErr) {
		t.Fatalf("Start error %v does not wrap the rollback's own Service stop failure %v", err, stopErr)
	}
	sliceEqual(t, log, []string{
		"open", "mount:/mnt/user", "confirmready",
		"start:nfs", "start:vm",
		"stop:nfs",
		"reclose",
	})
	if len(closeLog) != 0 {
		t.Fatalf("Close called %v, want it never reached — the rollback's own Service stop never got past nfs", closeLog)
	}
	if target.recloseCalls != 1 {
		t.Fatalf("Reclose called %d times, want 1 — a Service that refuses to stop during rollback must still restore the durable array-stopped flag", target.recloseCalls)
	}
	if !s.InMaintenance() {
		t.Fatal("Start: maintenance mode must stay active when a Service fails to start")
	}
}

// TestArraySequence_Start_ExitMaintenanceFailureRollsBackToStopped proves
// an ExitMaintenanceChecked write failure — every mount, ConfirmReady and
// Service has already succeeded by then — rolls the whole sequence back
// through Close(persist=true): every started service is stopped, the gate
// closes with the durable flag restored, every mount unmounts in stop
// order, maintenance stays persisted (ExitMaintenanceChecked never flips
// it in memory when its own write fails), and the original failure is
// still in the returned error. failingExitMaintenanceDBTX
// (scheduler_test.go) targets exactly that one write, leaving
// EnterMaintenance's, MarkArrayStopped's and BeginArrayStart's own
// array_maintenance writes untouched.
func TestArraySequence_Start_ExitMaintenanceFailureRollsBackToStopped(t *testing.T) {
	var log []string
	var closeLog []bool
	ctx := context.Background()
	db := newTestDB(t)
	wrapped := failingExitMaintenanceDBTX{DBTX: db, shouldFail: func() bool { return true }}
	s := NewScheduler(NewStore(wrapped), NewLogStore(t.TempDir()), NewHub(), NewRegistry())

	if err := s.EnterMaintenance(ctx); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}
	s.MarkArrayStopped()

	svc := &fakeArrayService{name: "nfs", log: &log}
	catchAll := &fakeArrayMount{where: "/mnt/user", log: &log}
	target := &fakeStorageTarget{log: &log, closeLog: &closeLog}
	seq := ArraySequence{Scheduler: s, Services: []ArrayService{svc}, CatchAll: catchAll, StorageTarget: target}
	err := seq.Start(ctx)
	if err == nil {
		t.Fatal("Start: got nil error, want the ExitMaintenanceChecked write failure to propagate")
	}
	sliceEqual(t, log, []string{
		"open", "mount:/mnt/user", "confirmready", "start:nfs",
		"stop:nfs",
		"close", "unmount:/mnt/user",
	})
	if len(closeLog) != 1 || !closeLog[0] {
		t.Fatalf("Close persist argument = %v, want [true] so the durable array-stopped flag is left in force", closeLog)
	}
	if target.recloseCalls != 0 {
		t.Fatalf("Reclose called %d times, want 0 — an ExitMaintenanceChecked failure rolls back through Close, never Reclose", target.recloseCalls)
	}
	if !s.InMaintenance() {
		t.Fatal("Start: maintenance mode must stay active when the exited-maintenance write fails")
	}
}

// fakeReadinessGate is ArraySequence's Gate fake.
type fakeReadinessGate struct{ ready bool }

func (g fakeReadinessGate) Ready() bool { return g.ready }

// TestArraySequence_Start_RefusesWhenGateIsNotReady is Q69's own
// requirement restated at the mount sequence: the array must genuinely
// refuse to activate while degraded and unacknowledged, rather than
// mounting whatever disks happen to be present over an unacknowledged
// missing disk.
func TestArraySequence_Start_RefusesWhenGateIsNotReady(t *testing.T) {
	var log []string
	disk1 := &fakeArrayMount{where: "/mnt/disk1", log: &log}

	seq := ArraySequence{Gate: fakeReadinessGate{ready: false}, Disks: []ArrayMount{disk1}}
	err := seq.Start(context.Background())
	if !errors.Is(err, ErrStorageNotReady) {
		t.Fatalf("Start: got %v, want ErrStorageNotReady", err)
	}
	if len(log) != 0 {
		t.Fatalf("Start mounted %v while the gate reported not ready", log)
	}
}

// TestArraySequence_Start_ProceedsWhenGateIsReadyOrUnset confirms the gate
// check does not regress the ordinary path: a ready gate, or none set at
// all (every caller in this package's own tests predates the gate),
// mounts normally.
func TestArraySequence_Start_ProceedsWhenGateIsReadyOrUnset(t *testing.T) {
	for _, gate := range []ReadinessGate{nil, fakeReadinessGate{ready: true}} {
		var log []string
		disk1 := &fakeArrayMount{where: "/mnt/disk1", log: &log}
		seq := ArraySequence{Gate: gate, Disks: []ArrayMount{disk1}}
		if err := seq.Start(context.Background()); err != nil {
			t.Fatalf("Start with gate %v: %v", gate, err)
		}
		sliceEqual(t, log, []string{"mount:/mnt/disk1"})
	}
}

func TestArraySequence_Start_DoesNotExitMaintenanceOnFailure(t *testing.T) {
	var log []string
	s := newTestScheduler(t)
	if err := s.EnterMaintenance(context.Background()); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}

	disk1 := &fakeArrayMount{where: "/mnt/disk1", mountErr: errors.New("device timeout"), log: &log}

	seq := ArraySequence{Scheduler: s, Disks: []ArrayMount{disk1}}
	if err := seq.Start(context.Background()); err == nil {
		t.Fatal("Start: got nil error, want the disk mount failure to propagate")
	}
	if !s.InMaintenance() {
		t.Fatal("Start: maintenance mode must stay active when a mount step fails")
	}
}

// TestArraySequence_RefreshLive_UpdatesARunningPoolOnly: a disk added to
// a running array must reach the catch-all and every share mount at once
// (doc 02 §4 "Adding a disk" step 6), catch-all first; a stopped array is
// left alone for Start to mount, and no disk or service is touched.
func TestArraySequence_RefreshLive_UpdatesARunningPoolOnly(t *testing.T) {
	var log []string
	seq := ArraySequence{
		Services:    []ArrayService{&fakeArrayService{name: "samba", log: &log}},
		Disks:       []ArrayMount{&fakeArrayMount{where: "/mnt/disk1", log: &log}},
		CatchAll:    &fakeArrayMount{where: "/mnt/user", log: &log},
		ShareMounts: []ArrayMount{&fakeArrayMount{where: "/mnt/user/media", log: &log}},
	}

	if err := seq.RefreshLive(context.Background(), false); err != nil {
		t.Fatalf("RefreshLive(stopped): %v", err)
	}
	sliceEqual(t, log, nil)

	if err := seq.RefreshLive(context.Background(), true); err != nil {
		t.Fatalf("RefreshLive(running): %v", err)
	}
	sliceEqual(t, log, []string{"mount:/mnt/user", "mount:/mnt/user/media"})
}
