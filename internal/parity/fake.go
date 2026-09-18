package parity

import (
	"context"
	"sync"
	"time"
)

// FakeEngine is a scriptable simulator of Engine (doc 06 §2): a test tells
// it what the next diff, sync, scrub or status call should produce —
// including a failure — and it plays that back instead of running
// snapraid.
type FakeEngine struct {
	mu sync.Mutex

	diff    DiffReport
	diffErr error

	status    ParityStatus
	statusErr error

	syncSteps  []Progress
	syncErr    error
	scrubSteps []Progress
	scrubErr   error
	fixSteps   []Progress
	fixErr     error
	checkSteps []Progress
	checkErr   error

	// Sleep paces streamed progress; tests override it to run instantly.
	Sleep func(time.Duration)
}

// NewFakeEngine returns a FakeEngine with no scripted behaviour: Diff and
// Status return zero values, Sync and Scrub stream no progress.
func NewFakeEngine() *FakeEngine {
	return &FakeEngine{Sleep: time.Sleep}
}

// SetDiff scripts the report the next Diff call returns.
func (f *FakeEngine) SetDiff(d DiffReport) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.diff = d
	f.diffErr = nil
}

// FailDiff scripts Diff to fail — for example, snapraid itself erroring on
// a missing content file.
func (f *FakeEngine) FailDiff(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.diffErr = err
}

// SetStatus scripts the report the next Status call returns.
func (f *FakeEngine) SetStatus(s ParityStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = s
	f.statusErr = nil
}

// FailStatus scripts Status to fail.
func (f *FakeEngine) FailStatus(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusErr = err
}

// ScriptSync scripts the next Sync call. If immediateErr is non-nil, Sync
// returns it directly and no channel is produced — simulating a rejection
// before the run starts. Otherwise Sync streams steps in order over the
// returned channel and closes it; a failure mid-run (a disk disappearing,
// doc 02 §6) is expressed by giving the last step a non-nil Err.
func (f *FakeEngine) ScriptSync(steps []Progress, immediateErr error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncSteps = steps
	f.syncErr = immediateErr
}

// ScriptScrub is ScriptSync for Scrub.
func (f *FakeEngine) ScriptScrub(steps []Progress, immediateErr error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scrubSteps = steps
	f.scrubErr = immediateErr
}

// ScriptFix is ScriptSync for Fix.
func (f *FakeEngine) ScriptFix(steps []Progress, immediateErr error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fixSteps = steps
	f.fixErr = immediateErr
}

// ScriptCheck is ScriptSync for Check.
func (f *FakeEngine) ScriptCheck(steps []Progress, immediateErr error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkSteps = steps
	f.checkErr = immediateErr
}

func (f *FakeEngine) Diff(ctx context.Context) (DiffReport, error) {
	if err := ctx.Err(); err != nil {
		return DiffReport{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.diffErr != nil {
		return DiffReport{}, f.diffErr
	}
	return f.diff, nil
}

func (f *FakeEngine) Status(ctx context.Context) (ParityStatus, error) {
	if err := ctx.Err(); err != nil {
		return ParityStatus{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statusErr != nil {
		return ParityStatus{}, f.statusErr
	}
	return f.status, nil
}

func (f *FakeEngine) Sync(ctx context.Context, opts SyncOpts) (<-chan Progress, error) {
	f.mu.Lock()
	steps, immediateErr := f.syncSteps, f.syncErr
	f.mu.Unlock()
	return f.stream(ctx, steps, immediateErr)
}

func (f *FakeEngine) Scrub(ctx context.Context, pct, olderThanDays int) (<-chan Progress, error) {
	f.mu.Lock()
	steps, immediateErr := f.scrubSteps, f.scrubErr
	f.mu.Unlock()
	return f.stream(ctx, steps, immediateErr)
}

func (f *FakeEngine) Fix(ctx context.Context, opts FixOpts) (<-chan Progress, error) {
	f.mu.Lock()
	steps, immediateErr := f.fixSteps, f.fixErr
	f.mu.Unlock()
	return f.stream(ctx, steps, immediateErr)
}

func (f *FakeEngine) Check(ctx context.Context, opts CheckOpts) (<-chan Progress, error) {
	f.mu.Lock()
	steps, immediateErr := f.checkSteps, f.checkErr
	f.mu.Unlock()
	return f.stream(ctx, steps, immediateErr)
}

func (f *FakeEngine) stream(ctx context.Context, steps []Progress, immediateErr error) (<-chan Progress, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if immediateErr != nil {
		return nil, immediateErr
	}

	ch := make(chan Progress)
	go func() {
		defer close(ch)
		for i, step := range steps {
			if i > 0 {
				f.Sleep(0)
			}
			select {
			case <-ctx.Done():
				return
			case ch <- step:
			}
			if step.Err != nil {
				return
			}
		}
	}()
	return ch, nil
}

var _ Engine = (*FakeEngine)(nil)
