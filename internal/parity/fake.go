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

	list    ListReport
	listErr error

	syncSteps  []Progress
	syncErr    error
	guardBlock *GuardResult
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

// SetList scripts the report the next List call returns.
func (f *FakeEngine) SetList(l ListReport) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.list = l
	f.listErr = nil
}

// FailList scripts List to fail.
func (f *FakeEngine) FailList(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listErr = err
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

// ScriptGuardBlock scripts the threshold guard (doc 02 §2) to block the
// next Sync call that does not set SyncOpts.Confirm — exactly
// SnapraidEngine's own Sync when its guard evaluation is Blocked: Sync
// returns a *GuardBlockedError carrying result instead of streaming
// anything, and no scripted sync step ever runs. A call with
// opts.Confirm set bypasses this and streams the scripted steps
// normally, the same "review the diff and sync anyway" path a real
// caller takes — matching CLAUDE.md's fake-must-scriptably-reproduce
// rule for the one behavior this package exists to guarantee. The block
// stays scripted across calls, the way a real guard re-blocks an
// unconfirmed retry against the same diff; call ScriptSync again with no
// preceding ScriptGuardBlock (or construct a fresh FakeEngine) to test
// an unblocked sync.
func (f *FakeEngine) ScriptGuardBlock(result GuardResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := result
	f.guardBlock = &r
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

func (f *FakeEngine) List(ctx context.Context) (ListReport, error) {
	if err := ctx.Err(); err != nil {
		return ListReport{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return ListReport{}, f.listErr
	}
	return f.list, nil
}

func (f *FakeEngine) Sync(ctx context.Context, opts SyncOpts) (<-chan Progress, error) {
	f.mu.Lock()
	steps, immediateErr := f.syncSteps, f.syncErr
	blocked := f.guardBlock
	f.mu.Unlock()

	if blocked != nil && !opts.Confirm {
		return nil, &GuardBlockedError{Result: *blocked}
	}
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
