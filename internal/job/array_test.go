package job

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeArrayService is ArraySequence's Service fake (CLAUDE.md): it
// records every Stop/Start call, in order, into a shared log, and can be
// scripted to fail either one — the "a container refuses to release an
// open file" scenario this file's central safety test reproduces.
type fakeArrayService struct {
	name     string
	stopErr  error
	startErr error
	log      *[]string
}

func (f *fakeArrayService) Name() string { return f.name }

func (f *fakeArrayService) Stop(ctx context.Context) error {
	*f.log = append(*f.log, "stop:"+f.name)
	return f.stopErr
}

func (f *fakeArrayService) Start(ctx context.Context) error {
	*f.log = append(*f.log, "start:"+f.name)
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

// fakeStorageTarget is ArraySequence's StorageTarget fake (#372 finding
// 1): it records when ConfirmReady ran (relative to whatever else writes
// into the same log) and can be scripted to fail, so a test can assert
// both the ordering Start owes it and that a failure there blocks every
// Service from starting.
type fakeStorageTarget struct {
	err error
	log *[]string
}

func (f *fakeStorageTarget) ConfirmReady(ctx context.Context, seq *ArraySequence) error {
	*f.log = append(*f.log, "confirmready")
	return f.err
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
