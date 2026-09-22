package job

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeUPSNotifier struct {
	mu         sync.Mutex
	onBattery  int
	batteryLow int
}

func (n *fakeUPSNotifier) NotifyUPSOnBattery(ctx context.Context) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.onBattery++
}

func (n *fakeUPSNotifier) NotifyUPSBatteryLow(ctx context.Context) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.batteryLow++
}

func (n *fakeUPSNotifier) counts() (onBattery, batteryLow int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.onBattery, n.batteryLow
}

// fakeShutdown is ShutdownSequence's own fake — it records that it ran
// and can be scripted to fail, standing in for UPSShutdown in a test
// that only cares whether HandleNotify(LOWBATT) reached it, not what
// UPSShutdown itself does (that composition is proven directly, below).
type fakeShutdown struct {
	mu   sync.Mutex
	runs int
	err  error
}

func (f *fakeShutdown) Shutdown(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs++
	return f.err
}

func (f *fakeShutdown) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs
}

// fakePowerOff is PowerOff's own fake.
type fakePowerOff struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (f *fakePowerOff) PowerOff(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.err
}

func (f *fakePowerOff) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// registerMoverThatStopsAtCheckpoint registers TypeMover with a RunFunc
// that blocks until StopRequested is closed, saves a checkpoint, then
// returns — the same shape TestScheduler_PauseForBattery_
// StopsRunningMoverAtCheckpoint already exercises directly against
// Scheduler, reused here so UPSController's own tests prove the whole
// on-battery/power-restored round trip through it. A resumed run (its
// own InitialCheckpoint non-empty) signals resumed before blocking on
// StopRequested again, so a test can end it cleanly with a second
// PauseForBattery instead of leaking the goroutine.
func registerMoverThatStopsAtCheckpoint(s *Scheduler) (resumed chan struct{}) {
	resumed = make(chan struct{}, 4)
	s.registry.Register(TypeMover, false, func(ctx context.Context, rc *RunContext) error {
		if len(rc.InitialCheckpoint()) > 0 {
			resumed <- struct{}{}
		}
		select {
		case <-rc.StopRequested():
			_ = rc.SaveCheckpoint([]byte("checkpoint"))
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	return resumed
}

// TestUPSController_OnBattery_NotifiesAndPausesMover proves Q77's
// on-battery reaction end to end through UPSController: the notifier
// fires, a running mover job is asked to stop and ends interrupted, and
// a subsequent sync submission is held.
func TestUPSController_OnBattery_NotifiesAndPausesMover(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	registerMoverThatStopsAtCheckpoint(s)
	s.registry.Register(TypeSync, false, blockingRun(make(chan struct{}), make(chan struct{}), nil))

	j, err := s.Submit(ctx, TypeMover, nil, nil)
	if err != nil {
		t.Fatalf("Submit(TypeMover): %v", err)
	}

	notifier := &fakeUPSNotifier{}
	c := &UPSController{Scheduler: s, Notifier: notifier}
	if err := c.HandleNotify(ctx, UPSNotifyOnBattery); err != nil {
		t.Fatalf("HandleNotify(ONBATT): %v", err)
	}

	if onBattery, batteryLow := notifier.counts(); onBattery != 1 || batteryLow != 0 {
		t.Fatalf("notifier counts = (onBattery=%d, batteryLow=%d), want (1, 0)", onBattery, batteryLow)
	}

	waitFor(t, time.Second, func() bool {
		got, err := s.store.Get(ctx, j.ID)
		return err == nil && got.Status == StatusInterrupted
	})

	if _, err := s.Submit(ctx, TypeSync, nil, nil); !errors.Is(err, ErrOnBattery) {
		t.Fatalf("Submit(TypeSync) after ONBATT = %v, want ErrOnBattery", err)
	}
}

// TestUPSController_OnBattery_IsIdempotent proves a repeated ONBATT (NUT
// itself can send more than one before ONLINE) never double-pauses or
// panics on a double channel close.
func TestUPSController_OnBattery_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	registerMoverThatStopsAtCheckpoint(s)

	j, err := s.Submit(ctx, TypeMover, nil, nil)
	if err != nil {
		t.Fatalf("Submit(TypeMover): %v", err)
	}

	notifier := &fakeUPSNotifier{}
	c := &UPSController{Scheduler: s, Notifier: notifier}
	if err := c.HandleNotify(ctx, UPSNotifyOnBattery); err != nil {
		t.Fatalf("first HandleNotify(ONBATT): %v", err)
	}
	if err := c.HandleNotify(ctx, UPSNotifyOnBattery); err != nil {
		t.Fatalf("second HandleNotify(ONBATT): %v", err)
	}
	if onBattery, _ := notifier.counts(); onBattery != 1 {
		t.Fatalf("notifier.onBattery = %d, want 1 (idempotent)", onBattery)
	}

	// Wait for the paused mover's own asynchronous completion to land
	// before the test returns and its store closes — otherwise its
	// final status write races the test's own cleanup.
	waitFor(t, time.Second, func() bool {
		got, err := s.store.Get(ctx, j.ID)
		return err == nil && got.Status == StatusInterrupted
	})
}

// TestUPSController_OnLine_ResumesMoverAndAllowsSync proves Q77's
// power-restored reaction: the mover job paused for the battery hold is
// automatically resumed (not left for an explicit user action, unlike an
// ordinary maintenance-mode interruption), and a sync submission is
// allowed again.
func TestUPSController_OnLine_ResumesMoverAndAllowsSync(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	resumed := registerMoverThatStopsAtCheckpoint(s)
	syncStarted, syncRelease := registerBlocking(s, TypeSync, false)

	j, err := s.Submit(ctx, TypeMover, nil, nil)
	if err != nil {
		t.Fatalf("Submit(TypeMover): %v", err)
	}

	notifier := &fakeUPSNotifier{}
	c := &UPSController{Scheduler: s, Notifier: notifier}
	if err := c.HandleNotify(ctx, UPSNotifyOnBattery); err != nil {
		t.Fatalf("HandleNotify(ONBATT): %v", err)
	}
	waitFor(t, time.Second, func() bool {
		got, err := s.store.Get(ctx, j.ID)
		return err == nil && got.Status == StatusInterrupted
	})

	if err := c.HandleNotify(ctx, UPSNotifyOnLine); err != nil {
		t.Fatalf("HandleNotify(ONLINE): %v", err)
	}

	select {
	case <-resumed:
	case <-time.After(time.Second):
		t.Fatal("the paused mover job was never resumed after ONLINE")
	}
	waitFor(t, time.Second, func() bool {
		got, err := s.store.Get(ctx, j.ID)
		return err == nil && got.Status == StatusRunning
	})

	if s.OnBattery() {
		t.Fatal("OnBattery() = true after ONLINE")
	}
	// The resumed mover job (Array-write) still holds a storage-class
	// slot, so this sync (Parity) queues behind it rather than running —
	// Submit succeeding (no ErrOnBattery) is what this asserts; doc 01
	// §4's own class exclusion, not the battery hold, is why it waits.
	sync, err := s.Submit(ctx, TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit(TypeSync) after ONLINE = %v, want nil", err)
	}

	// The resumed mover run is still blocked on its own StopRequested —
	// end it cleanly (rather than leaking the goroutine) the same way a
	// second battery event would. PauseForBattery re-engages the hold,
	// so it is released again right after — otherwise the queued sync
	// below would never be dispatched, and this test would hang.
	s.PauseForBattery(ctx)
	waitFor(t, time.Second, func() bool {
		got, err := s.store.Get(ctx, j.ID)
		return err == nil && got.Status == StatusInterrupted
	})
	s.ResumeFromBattery()
	<-syncStarted
	close(syncRelease)
	waitSucceeded(t, s, sync.ID)
}

// TestUPSController_OnLine_ImmediatelyAfterOnBattery_StillResumes
// proves the fix for the race PauseForBattery's own doc comment
// describes: without PauseForBattery waiting for the mover it stopped
// to actually reach StatusInterrupted, an ONLINE arriving right after
// ONBATT (a short outage, with no artificial delay between them the way
// TestUPSController_OnLine_ResumesMoverAndAllowsSync deliberately adds)
// could call Resume before the checkpoint write lands, get
// ErrJobNotInterrupted, and leave the mover stuck — never resumed,
// since Q77's on-battery/power-restored pair is the one case that
// resumes automatically rather than waiting for an explicit user
// action.
func TestUPSController_OnLine_ImmediatelyAfterOnBattery_StillResumes(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	resumed := registerMoverThatStopsAtCheckpoint(s)

	j, err := s.Submit(ctx, TypeMover, nil, nil)
	if err != nil {
		t.Fatalf("Submit(TypeMover): %v", err)
	}

	c := &UPSController{Scheduler: s}
	if err := c.HandleNotify(ctx, UPSNotifyOnBattery); err != nil {
		t.Fatalf("HandleNotify(ONBATT): %v", err)
	}
	if err := c.HandleNotify(ctx, UPSNotifyOnLine); err != nil {
		t.Fatalf("HandleNotify(ONLINE) immediately after ONBATT: %v", err)
	}

	select {
	case <-resumed:
	case <-time.After(time.Second):
		t.Fatal("the paused mover job was never resumed after an immediate ONLINE")
	}
	waitFor(t, time.Second, func() bool {
		got, err := s.store.Get(ctx, j.ID)
		return err == nil && got.Status == StatusRunning
	})
}

// TestUPSController_OnLine_WithoutAPriorOnBatteryIsNoop proves ONLINE
// arriving without a matching ONBATT (e.g. upsmon restarting already
// online) does nothing — there is nothing to resume or release.
func TestUPSController_OnLine_WithoutAPriorOnBatteryIsNoop(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	c := &UPSController{Scheduler: s}
	if err := c.HandleNotify(ctx, UPSNotifyOnLine); err != nil {
		t.Fatalf("HandleNotify(ONLINE): %v", err)
	}
	if s.OnBattery() {
		t.Fatal("OnBattery() = true after a bare ONLINE")
	}
}

// TestUPSController_LowBattery_NotifiesAndShutsDown proves
// HandleNotify(LOWBATT) notifies and hands off to the configured
// ShutdownSequence, regardless of whether ONBATT was ever seen first.
func TestUPSController_LowBattery_NotifiesAndShutsDown(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	notifier := &fakeUPSNotifier{}
	shutdown := &fakeShutdown{}
	c := &UPSController{Scheduler: s, Notifier: notifier, Shutdown: shutdown}

	if err := c.HandleNotify(ctx, UPSNotifyLowBattery); err != nil {
		t.Fatalf("HandleNotify(LOWBATT): %v", err)
	}
	if _, batteryLow := notifier.counts(); batteryLow != 1 {
		t.Fatalf("notifier.batteryLow = %d, want 1", batteryLow)
	}
	if shutdown.count() != 1 {
		t.Fatalf("shutdown.count() = %d, want 1", shutdown.count())
	}
}

// TestUPSController_LowBattery_WithoutShutdownConfiguredIsAnError proves
// HandleNotify(LOWBATT) never silently does nothing when no
// ShutdownSequence is wired — a low-battery event with no shutdown
// configured must surface as a real error, not a quiet no-op.
func TestUPSController_LowBattery_WithoutShutdownConfiguredIsAnError(t *testing.T) {
	c := &UPSController{Scheduler: newTestScheduler(t)}
	if err := c.HandleNotify(context.Background(), UPSNotifyLowBattery); err == nil {
		t.Fatal("HandleNotify(LOWBATT) with no ShutdownSequence configured = nil, want an error")
	}
}

// TestUPSController_HandleNotify_UnknownTypeIsNoop proves a NUT
// notification type outside Q77's own defined reaction set (COMMBAD,
// REPLBATT, ...) does nothing.
func TestUPSController_HandleNotify_UnknownTypeIsNoop(t *testing.T) {
	s := newTestScheduler(t)
	notifier := &fakeUPSNotifier{}
	c := &UPSController{Scheduler: s, Notifier: notifier}
	if err := c.HandleNotify(context.Background(), "COMMBAD"); err != nil {
		t.Fatalf("HandleNotify(COMMBAD) = %v, want nil", err)
	}
	if onBattery, batteryLow := notifier.counts(); onBattery != 0 || batteryLow != 0 {
		t.Fatalf("notifier counts = (%d, %d), want (0, 0) — an unknown type must not notify", onBattery, batteryLow)
	}
}

// TestUPSShutdown_ChecksArrayWriteJobThenPowersOff proves Q70 and Q77's
// own low-battery composition end to end for a running Array-write job:
// a real ArraySequence (not a fake) checkpoints a running mover job,
// stops every configured service, and only then is PowerOff called.
func TestUPSShutdown_ChecksArrayWriteJobThenPowersOff(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	moverStopped := make(chan struct{})
	s.registry.Register(TypeMover, false, func(ctx context.Context, rc *RunContext) error {
		<-rc.StopRequested()
		_ = rc.SaveCheckpoint([]byte("checkpoint-at-low-battery"))
		close(moverStopped)
		return nil
	})

	mover, err := s.Submit(ctx, TypeMover, nil, nil)
	if err != nil {
		t.Fatalf("Submit(TypeMover): %v", err)
	}

	var log []string
	svc := &fakeArrayService{name: "svc", log: &log}
	shutdown := UPSShutdown{
		Array: ArraySequence{Scheduler: s, Services: []ArrayService{svc}},
		Power: &fakePowerOff{},
	}
	notifier := &fakeUPSNotifier{}
	c := &UPSController{Scheduler: s, Notifier: notifier, Shutdown: shutdown}

	if err := c.HandleNotify(ctx, UPSNotifyLowBattery); err != nil {
		t.Fatalf("HandleNotify(LOWBATT): %v", err)
	}

	<-moverStopped
	waitFor(t, time.Second, func() bool {
		got, err := s.store.Get(ctx, mover.ID)
		return err == nil && got.Status == StatusInterrupted
	})
	if len(log) != 1 || log[0] != "stop:svc" {
		t.Fatalf("service log = %v, want [stop:svc]", log)
	}
	power := shutdown.Power.(*fakePowerOff)
	if power.count() != 1 {
		t.Fatalf("PowerOff.count() = %d, want 1", power.count())
	}
	if _, batteryLow := notifier.counts(); batteryLow != 1 {
		t.Fatalf("notifier.batteryLow = %d, want 1", batteryLow)
	}
}

// TestUPSShutdown_MarksRunningSyncInterruptedThenPowersOff covers the
// acceptance criterion's other named case: a running sync (Parity class,
// non-resumable) is marked interrupted — never left running — before the
// host powers off. A sync and a mover can never be running at the same
// time (doc 01 §4: Parity excludes Array-write globally), so this is its
// own scenario rather than sharing TestUPSShutdown_
// ChecksArrayWriteJobThenPowersOff's mover.
func TestUPSShutdown_MarksRunningSyncInterruptedThenPowersOff(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	syncStarted, syncRelease := registerBlocking(s, TypeSync, false)
	defer close(syncRelease)

	sync, err := s.Submit(ctx, TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit(TypeSync): %v", err)
	}
	<-syncStarted

	var log []string
	svc := &fakeArrayService{name: "svc", log: &log}
	shutdown := UPSShutdown{
		Array: ArraySequence{Scheduler: s, Services: []ArrayService{svc}},
		Power: &fakePowerOff{},
	}
	notifier := &fakeUPSNotifier{}
	c := &UPSController{Scheduler: s, Notifier: notifier, Shutdown: shutdown}

	if err := c.HandleNotify(ctx, UPSNotifyLowBattery); err != nil {
		t.Fatalf("HandleNotify(LOWBATT): %v", err)
	}

	waitFor(t, time.Second, func() bool {
		got, err := s.store.Get(ctx, sync.ID)
		return err == nil && got.Status == StatusInterrupted
	})
	if len(log) != 1 || log[0] != "stop:svc" {
		t.Fatalf("service log = %v, want [stop:svc]", log)
	}
	power := shutdown.Power.(*fakePowerOff)
	if power.count() != 1 {
		t.Fatalf("PowerOff.count() = %d, want 1", power.count())
	}
}

// TestUPSShutdown_ServiceRefusesToStop_NeverPowersOff proves the same
// safety ArraySequence.Stop already guarantees on its own: if a service
// refuses to stop, UPSShutdown must not power the host off — that would
// be exactly the "disk pulled mid-write" hazard Q70's own ordering
// exists to prevent.
func TestUPSShutdown_ServiceRefusesToStop_NeverPowersOff(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	var log []string
	svc := &fakeArrayService{name: "svc", stopErr: errors.New("simulated: file still open"), log: &log}
	power := &fakePowerOff{}
	shutdown := UPSShutdown{
		Array: ArraySequence{Scheduler: s, Services: []ArrayService{svc}},
		Power: power,
	}
	c := &UPSController{Scheduler: s, Shutdown: shutdown}

	if err := c.HandleNotify(ctx, UPSNotifyLowBattery); err == nil {
		t.Fatal("HandleNotify(LOWBATT) with a service that refuses to stop = nil, want an error")
	}
	if power.count() != 0 {
		t.Fatalf("PowerOff.count() = %d, want 0 — a refused service stop must never reach PowerOff", power.count())
	}
}
