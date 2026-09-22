package job

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// stepRecorder records step names in call order, safe for concurrent use
// across the goroutines a job's RunFunc runs in versus the goroutine
// driving MaintenanceChain.Run.
type stepRecorder struct {
	mu    sync.Mutex
	names []string
}

func (r *stepRecorder) record(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.names = append(r.names, name)
}

func (r *stepRecorder) get() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.names))
	copy(out, r.names)
	return out
}

// registerRecording registers t on s's registry with a RunFunc that records
// name on rec and returns immediately.
func registerRecording(s *Scheduler, t Type, rec *stepRecorder, name string) {
	s.registry.Register(t, false, func(ctx context.Context, rc *RunContext) error {
		rec.record(name)
		return nil
	})
}

type fakeGuard struct {
	blocked bool
	err     error
	calls   int
}

func (g *fakeGuard) Evaluate(ctx context.Context) (bool, error) {
	g.calls++
	return g.blocked, g.err
}

type fakeNotifier struct {
	mu       sync.Mutex
	notified int
}

func (n *fakeNotifier) NotifyGuardBlocked(ctx context.Context) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.notified++
}

func (n *fakeNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.notified
}

type fakeBackup struct {
	mu   sync.Mutex
	runs int
	err  error
}

func (b *fakeBackup) Run(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.runs++
	return b.err
}

func (b *fakeBackup) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.runs
}

// TestMaintenanceChain_RunsStepsInOrderAndSkipsDisabled exercises Q30's own
// order end to end: mover, diff + guard, sync, config backup — with scrub
// disabled by it not being the weekly day, and mover disabled explicitly —
// and checks the chain never reorders around a disabled step, only skips
// it.
func TestMaintenanceChain_RunsStepsInOrderAndSkipsDisabled(t *testing.T) {
	s := newTestScheduler(t)
	rec := &stepRecorder{}
	registerRecording(s, TypeMover, rec, "mover")
	registerRecording(s, TypeSync, rec, "sync")
	registerRecording(s, TypeScrub, rec, "scrub")

	guard := &fakeGuard{}
	backup := &fakeBackup{}
	chain := &MaintenanceChain{
		Scheduler: s,
		Guard:     guard,
		Backup:    backup,
		Enabled:   map[Step]bool{StepMover: false},
		Weekly:    false,
	}

	result, err := chain.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Blocked {
		t.Fatal("Run reported Blocked, want false")
	}

	got := rec.get()
	want := []string{"sync"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("recorded steps = %v, want %v (mover disabled, scrub not the weekly day)", got, want)
	}
	if guard.calls != 1 {
		t.Errorf("guard.calls = %d, want 1", guard.calls)
	}
	if backup.count() != 1 {
		t.Errorf("backup.count() = %d, want 1", backup.count())
	}

	steps := result.Steps
	if len(steps) != len(chainOrder) {
		t.Fatalf("len(result.Steps) = %d, want %d (one entry per chain step, disabled or not)", len(steps), len(chainOrder))
	}
	for i, step := range chainOrder {
		if steps[i].Step != step {
			t.Errorf("result.Steps[%d].Step = %s, want %s — chain order must never change", i, steps[i].Step, step)
		}
	}
	if !steps[0].Skipped {
		t.Error("mover step should be reported Skipped")
	}
	if !steps[3].Skipped {
		t.Error("scrub step should be reported Skipped on a non-weekly run")
	}
}

// TestMaintenanceChain_GuardBlockStopsChainAfterDiffAndNotifies is the
// issue's second acceptance criterion: a blocked guard stops the chain
// after diff, and notifies. Sync, scrub and config backup must never run.
func TestMaintenanceChain_GuardBlockStopsChainAfterDiffAndNotifies(t *testing.T) {
	s := newTestScheduler(t)
	rec := &stepRecorder{}
	registerRecording(s, TypeMover, rec, "mover")
	registerRecording(s, TypeSync, rec, "sync")
	registerRecording(s, TypeScrub, rec, "scrub")

	guard := &fakeGuard{blocked: true}
	notifier := &fakeNotifier{}
	backup := &fakeBackup{}
	chain := &MaintenanceChain{
		Scheduler: s,
		Guard:     guard,
		Notifier:  notifier,
		Backup:    backup,
		Weekly:    true,
	}

	result, err := chain.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Blocked {
		t.Fatal("result.Blocked = false, want true")
	}
	if notifier.count() != 1 {
		t.Errorf("notifier.count() = %d, want 1", notifier.count())
	}
	if backup.count() != 0 {
		t.Errorf("backup.count() = %d, want 0 — config backup must not run past a blocked guard", backup.count())
	}

	got := rec.get()
	if len(got) != 1 || got[0] != "mover" {
		t.Fatalf("recorded steps = %v, want [mover] — sync and scrub must not run past a blocked guard", got)
	}

	// The diff_guard step itself is the second entry (mover, diff_guard,
	// ...) and the chain must stop there, recording nothing past it.
	if len(result.Steps) != 2 {
		t.Fatalf("len(result.Steps) = %d, want 2 (mover, diff_guard)", len(result.Steps))
	}
	if result.Steps[1].Step != StepDiffGuard || !result.Steps[1].Blocked {
		t.Errorf("result.Steps[1] = %+v, want a blocked diff_guard entry", result.Steps[1])
	}
}

// TestMaintenanceChain_LongFirstStepDelaysSync is the issue's own required
// test: a long first step (mover) delays sync rather than overlapping it.
// Every ordering guarantee here comes from channels the test controls, not
// from a race against the clock (scheduler_test.go's own convention).
func TestMaintenanceChain_LongFirstStepDelaysSync(t *testing.T) {
	s := newTestScheduler(t)
	moverStarted, moverRelease := registerBlocking(s, TypeMover, false)

	syncStarted := make(chan struct{})
	s.registry.Register(TypeSync, false, func(ctx context.Context, rc *RunContext) error {
		close(syncStarted)
		return nil
	})

	chain := &MaintenanceChain{
		Scheduler: s,
		Guard:     &fakeGuard{},
		Weekly:    false,
	}

	runDone := make(chan error, 1)
	go func() {
		_, err := chain.Run(context.Background())
		runDone <- err
	}()

	// Deterministic: mover's RunFunc has signalled it started, which can
	// only happen after Scheduler.Submit(mover) returned to the chain and
	// startJobLocked spawned its goroutine — so at this exact point the
	// chain is blocked inside runJobStep's Await(mover), and Submit(sync)
	// has structurally not been called yet.
	select {
	case <-moverStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("mover never started")
	}

	select {
	case <-syncStarted:
		t.Fatal("sync started before the long-running mover step finished")
	default:
	}

	close(moverRelease)

	select {
	case <-syncStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("sync never started after mover finished")
	}

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("chain.Run never returned")
	}
}

// TestMaintenanceChain_JobStepFailureDoesNotStopTheChain checks that a
// step's own terminal failure (as opposed to the diff_guard block) does not
// halt the remaining steps — only the guard block does that (the issue's
// second acceptance criterion is explicit that this is the one thing that
// stops the chain).
func TestMaintenanceChain_JobStepFailureDoesNotStopTheChain(t *testing.T) {
	s := newTestScheduler(t)
	s.registry.Register(TypeMover, false, func(ctx context.Context, rc *RunContext) error {
		return errors.New("mover: simulated failure")
	})
	rec := &stepRecorder{}
	registerRecording(s, TypeSync, rec, "sync")
	backup := &fakeBackup{}

	chain := &MaintenanceChain{
		Scheduler: s,
		Guard:     &fakeGuard{},
		Backup:    backup,
		Weekly:    false,
	}

	result, err := chain.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := result.Steps[0]; got.Status != StatusFailed {
		t.Errorf("mover step status = %s, want failed", got.Status)
	}
	if got := rec.get(); len(got) != 1 || got[0] != "sync" {
		t.Fatalf("recorded steps = %v, want [sync] — a failed mover must not stop the chain", got)
	}
	if backup.count() != 1 {
		t.Errorf("backup.count() = %d, want 1", backup.count())
	}
}

// TestMaintenanceChain_MoverAndSyncStepsSkippedOnBattery proves Q77's
// "hold scheduled syncs" applies to the nightly chain too: with the
// scheduler already on battery, the mover and sync steps are both
// reported Skipped — never a chain failure — while diff+guard and config
// backup, which do not go through Submit, still run.
func TestMaintenanceChain_MoverAndSyncStepsSkippedOnBattery(t *testing.T) {
	s := newTestScheduler(t)
	rec := &stepRecorder{}
	registerRecording(s, TypeMover, rec, "mover")
	registerRecording(s, TypeSync, rec, "sync")
	s.PauseForBattery()

	guard := &fakeGuard{}
	backup := &fakeBackup{}
	chain := &MaintenanceChain{
		Scheduler: s,
		Guard:     guard,
		Backup:    backup,
		Weekly:    false,
	}

	result, err := chain.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Blocked {
		t.Fatal("Run reported Blocked, want false")
	}
	if got := rec.get(); len(got) != 0 {
		t.Fatalf("recorded steps = %v, want none — mover and sync are held while on battery", got)
	}
	if !result.Steps[0].Skipped {
		t.Errorf("mover step Skipped = %v, want true (Q77: held while on battery)", result.Steps[0].Skipped)
	}
	if !result.Steps[2].Skipped {
		t.Errorf("sync step Skipped = %v, want true (Q77: held while on battery)", result.Steps[2].Skipped)
	}
	if guard.calls != 1 {
		t.Errorf("guard.calls = %d, want 1 — diff+guard does not go through Submit", guard.calls)
	}
	if backup.count() != 1 {
		t.Errorf("backup.count() = %d, want 1 — config backup does not go through Submit", backup.count())
	}
}

func TestDetectConflict(t *testing.T) {
	base := time.Date(2026, 1, 1, 2, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		a, b ScheduledWindow
		want bool
	}{
		{
			name: "overlapping parity jobs conflict",
			a:    ScheduledWindow{Class: ClassParity, Start: base, Duration: time.Hour},
			b:    ScheduledWindow{Class: ClassParity, Start: base.Add(30 * time.Minute), Duration: time.Hour},
			want: true,
		},
		{
			name: "non-overlapping parity jobs do not conflict",
			a:    ScheduledWindow{Class: ClassParity, Start: base, Duration: time.Hour},
			b:    ScheduledWindow{Class: ClassParity, Start: base.Add(2 * time.Hour), Duration: time.Hour},
			want: false,
		},
		{
			name: "overlapping array-write jobs on different disks do not conflict",
			a:    ScheduledWindow{Class: ClassArrayWrite, ResourceIDs: []string{"disk1"}, Start: base, Duration: time.Hour},
			b:    ScheduledWindow{Class: ClassArrayWrite, ResourceIDs: []string{"disk2"}, Start: base, Duration: time.Hour},
			want: false,
		},
		{
			name: "overlapping array-write jobs on the same disk conflict",
			a:    ScheduledWindow{Class: ClassArrayWrite, ResourceIDs: []string{"disk1"}, Start: base, Duration: time.Hour},
			b:    ScheduledWindow{Class: ClassArrayWrite, ResourceIDs: []string{"disk1"}, Start: base, Duration: time.Hour},
			want: true,
		},
		{
			name: "overlapping service and parity jobs do not conflict, per doc 01 §4",
			a:    ScheduledWindow{Class: ClassService, Start: base, Duration: time.Hour},
			b:    ScheduledWindow{Class: ClassParity, Start: base, Duration: time.Hour},
			want: false,
		},
		{
			name: "two heavy topology jobs at once conflict",
			a:    ScheduledWindow{Class: ClassTopology, Start: base, Duration: 3 * time.Hour},
			b:    ScheduledWindow{Class: ClassTopology, Start: base.Add(time.Hour), Duration: time.Hour},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DetectConflict(tt.a, tt.b); got != tt.want {
				t.Errorf("DetectConflict(a, b) = %v, want %v", got, tt.want)
			}
			if got := DetectConflict(tt.b, tt.a); got != tt.want {
				t.Errorf("DetectConflict(b, a) = %v, want %v (must be symmetric)", got, tt.want)
			}
		})
	}
}
