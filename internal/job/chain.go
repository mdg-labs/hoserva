package job

import (
	"context"
	"errors"
	"fmt"
)

// Step identifies one of MaintenanceChain's fixed steps (Q30, doc 03 §8.4).
type Step string

const (
	StepMover        Step = "mover"
	StepDiffGuard    Step = "diff_guard"
	StepSync         Step = "sync"
	StepScrub        Step = "scrub"
	StepConfigBackup Step = "config_backup"
)

// chainOrder is Q30's own order: mover, diff + guard, sync, scrub (on the
// weekly day only) and config backup. Touch has no step of its own — Q17
// makes it an automatic action Sync takes before syncing, and
// parity.Engine's own interface doc says as much: "Touch is not part of
// this interface at all". MaintenanceChain.Run always walks exactly this
// slice; nothing in this package can reorder it. Enabling or disabling a
// step (doc 03 §8.4: "individual steps can be disabled, not reordered") is
// the only lever MaintenanceChain.Enabled gives a caller.
var chainOrder = []Step{StepMover, StepDiffGuard, StepSync, StepScrub, StepConfigBackup}

// ChainOrder returns Q30's fixed maintenance chain step order.
func ChainOrder() []Step {
	return chainOrder
}

// DiffGuard is the chain's "diff + guard" step (doc 02 §2): run `snapraid
// diff` and evaluate the deletion threshold guard against it. It is
// deliberately not a Scheduler job — like parity.Engine's own Diff method,
// it runs synchronously and holds no job class (Engine's doc comment: "Diff
// and Status are not job types at all") — so internal/parity implements
// this directly against a real parity.Engine and threshold guard without
// this package importing internal/parity; every chain test uses a fake.
// Evaluate reports blocked=true exactly when the guard held that night's
// sync back (doc 02 §2: "the sync is held... does not proceed until a
// human decides").
type DiffGuard interface {
	Evaluate(ctx context.Context) (blocked bool, err error)
}

// ChainNotifier delivers the high-priority notification a blocked guard
// fires "through every configured channel" (doc 02 §2). internal/notify
// implements this once it exists; every chain test uses a fake.
type ChainNotifier interface {
	NotifyGuardBlocked(ctx context.Context)
}

// ConfigBackup is the chain's last step (Q30, doc 10 §1): a consistent
// snapshot of the daemon's own config and database, taken after that
// night's sync. Doc 01 §4's mutually-exclusive-class table has no entry
// for it yet, so — like DiffGuard — this package calls it directly rather
// than through Scheduler.Submit, the same way ArraySequence calls
// ArrayService directly instead of submitting a job for each one.
// internal/backup implements this once it exists; a nil ConfigBackup makes
// the step a skip, not a chain failure, since nothing has claimed it yet.
type ConfigBackup interface {
	Run(ctx context.Context) error
}

// StepResult is one step's outcome, in the order MaintenanceChain.Run
// executed it.
type StepResult struct {
	Step Step
	// Skipped is true when the step was disabled, or — for scrub — this
	// run isn't the weekly day, or — for config backup — no ConfigBackup
	// is configured yet, or — for the mover — TypeMover has no registered
	// RunFunc on this Scheduler. An unregistered sync or scrub is a chain
	// failure, not a skip: omitting parity would consume the nightly
	// window and look like success.
	Skipped bool
	// JobID and Status are set for a step that ran as a Scheduler job
	// (mover, sync, scrub); both are zero for diff_guard and config_backup.
	JobID  string
	Status Status
	// Blocked is set on the diff_guard step when the threshold guard held
	// the sync back.
	Blocked bool
	// Err is the error that stopped the chain at this step, if any.
	Err error
}

// ChainResult is MaintenanceChain.Run's full account of one run, in step
// order, for the caller to log, notify on, or show in the UI.
type ChainResult struct {
	Steps []StepResult
	// Blocked is true when the diff_guard step stopped the chain — sync,
	// scrub and config backup never ran (doc 02 §2).
	Blocked bool
}

// MaintenanceChain is Q30's one chained nightly maintenance run: mover,
// diff + guard, sync, scrub (on the weekly day) and config backup, each
// step starting only once the previous one has actually finished rather
// than at a fixed clock time, so a slow step delays the next one instead of
// racing it (Q30: "a mover run longer than an hour silently breaks the
// ordering [with fixed clock times]. Chaining makes the order structural.").
type MaintenanceChain struct {
	Scheduler *Scheduler
	Guard     DiffGuard
	Backup    ConfigBackup
	Notifier  ChainNotifier

	// Enabled disables an individual step (doc 03 §8.4) without touching
	// its position in chainOrder. A step missing from the map, or a nil
	// map, defaults to enabled.
	Enabled map[Step]bool

	// Weekly reports whether this run is the weekly scrub day (Q30: "On
	// the weekly day, scrub runs after the sync"). MaintenanceChain has no
	// calendar of its own; the caller that triggers a run decides.
	Weekly bool
}

// Run walks chainOrder once, starting each enabled step only after the
// previous one has finished. A step that can't even start — Submit
// refusing, or Guard/Backup themselves returning an error — stops the
// chain immediately; a job step's own terminal failure does not, since the
// remaining steps (config backup, in particular) are independent of
// whether an earlier one found something to do.
func (c *MaintenanceChain) Run(ctx context.Context) (ChainResult, error) {
	var result ChainResult
	for _, step := range chainOrder {
		if step == StepScrub && !c.Weekly {
			result.Steps = append(result.Steps, StepResult{Step: step, Skipped: true})
			continue
		}
		if !c.enabled(step) {
			result.Steps = append(result.Steps, StepResult{Step: step, Skipped: true})
			continue
		}

		sr, blocked, err := c.runStep(ctx, step)
		result.Steps = append(result.Steps, sr)
		if blocked {
			result.Blocked = true
			return result, nil
		}
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

func (c *MaintenanceChain) enabled(step Step) bool {
	if c.Enabled == nil {
		return true
	}
	enabled, ok := c.Enabled[step]
	return !ok || enabled
}

func (c *MaintenanceChain) runStep(ctx context.Context, step Step) (StepResult, bool, error) {
	switch step {
	case StepMover:
		return c.runJobStep(ctx, step, TypeMover)
	case StepSync:
		return c.runJobStep(ctx, step, TypeSync)
	case StepScrub:
		return c.runJobStep(ctx, step, TypeScrub)
	case StepDiffGuard:
		return c.runDiffGuard(ctx)
	case StepConfigBackup:
		return c.runConfigBackup(ctx)
	default:
		err := fmt.Errorf("job: maintenance chain: unknown step %q", step)
		return StepResult{Step: step, Err: err}, false, err
	}
}

// runJobStep submits t and blocks until it reaches a terminal status before
// returning — the mechanism that holds each job-backed step's class in
// turn (doc 01 §4) and makes a slow step delay the next one rather than
// overlap it.
func (c *MaintenanceChain) runJobStep(ctx context.Context, step Step, t Type) (StepResult, bool, error) {
	j, err := c.Scheduler.Submit(ctx, t, nil, nil)
	if err != nil {
		if errors.Is(err, ErrJobTypeNotRegistered) && step == StepMover {
			return StepResult{Step: step, Skipped: true}, false, nil
		}
		wrapped := fmt.Errorf("job: maintenance chain: starting %s: %w", t, err)
		return StepResult{Step: step, Err: wrapped}, false, wrapped
	}
	finished, err := c.Scheduler.Await(ctx, j.ID)
	if err != nil {
		wrapped := fmt.Errorf("job: maintenance chain: waiting for %s: %w", t, err)
		return StepResult{Step: step, JobID: j.ID, Err: wrapped}, false, wrapped
	}
	return StepResult{Step: step, JobID: finished.ID, Status: finished.Status}, false, nil
}

func (c *MaintenanceChain) runDiffGuard(ctx context.Context) (StepResult, bool, error) {
	if c.Guard == nil {
		err := fmt.Errorf("job: maintenance chain: no diff guard configured")
		return StepResult{Step: StepDiffGuard, Err: err}, false, err
	}
	blocked, err := c.Guard.Evaluate(ctx)
	if err != nil {
		wrapped := fmt.Errorf("job: maintenance chain: diff + guard: %w", err)
		return StepResult{Step: StepDiffGuard, Err: wrapped}, false, wrapped
	}
	if blocked && c.Notifier != nil {
		c.Notifier.NotifyGuardBlocked(ctx)
	}
	return StepResult{Step: StepDiffGuard, Blocked: blocked}, blocked, nil
}

func (c *MaintenanceChain) runConfigBackup(ctx context.Context) (StepResult, bool, error) {
	if c.Backup == nil {
		return StepResult{Step: StepConfigBackup, Skipped: true}, false, nil
	}
	if err := c.Backup.Run(ctx); err != nil {
		wrapped := fmt.Errorf("job: maintenance chain: config backup: %w", err)
		return StepResult{Step: StepConfigBackup, Err: wrapped}, false, wrapped
	}
	return StepResult{Step: StepConfigBackup}, false, nil
}
