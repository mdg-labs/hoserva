package job

import (
	"context"
	"errors"
	"testing"
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
