package job

import (
	"context"
	"io"
)

// RunContext is what Scheduler hands a running job's RunFunc: its own
// cancellable context, the checkpoint it resumed from (if any), a signal
// for maintenance mode's graceful stop (Q70), and the log writer and
// checkpoint/progress hooks that make its execution observable (Q29, Q74,
// doc 01 §5).
type RunContext struct {
	id            string
	ctx           context.Context
	checkpoint    []byte
	params        []byte
	stopRequested <-chan struct{}
	out           io.Writer

	saveCheckpoint func(data []byte) error
	setProgress    func(pct int)
}

// JobID is the id of the job this run belongs to — the same id on every
// resume of it.
func (rc *RunContext) JobID() string { return rc.id }

// Context is the job's own context: cancelled when the job is cancelled,
// or forcibly for a non-resumable job caught running when maintenance mode
// starts (Q70). A resumable RunFunc should prefer StopRequested for a
// graceful stop and treat Context's cancellation as the harder, "stop now"
// signal.
func (rc *RunContext) Context() context.Context { return rc.ctx }

// InitialCheckpoint is the checkpoint data Resume was called with — nil
// for a job's first run, or for a non-resumable job type.
func (rc *RunContext) InitialCheckpoint() []byte { return rc.checkpoint }

// Params is the JSON request payload Submit persisted for this job —
// nil when the type has none. RunFunc must read options from here, not
// from the original HTTP request, so they survive a restart between
// queue and run.
func (rc *RunContext) Params() []byte { return rc.params }

// StopRequested is closed when maintenance mode asks a resumable job to
// stop at its next checkpoint (Q70) rather than keep working. A resumable
// RunFunc must poll this (or select on it) between checkpoints; the
// scheduler marks the job interrupted once RunFunc returns, whatever it
// returns.
func (rc *RunContext) StopRequested() <-chan struct{} { return rc.stopRequested }

// Output is the job's combined stdout/stderr capture (Q74) — a compressed
// file, one per job, downloadable through getJobLog.
func (rc *RunContext) Output() io.Writer { return rc.out }

// SaveCheckpoint persists a resumable job's progress marker (Q29) so
// Resume can continue from it instead of from zero. Calling it for a
// non-resumable job type is harmless — nothing ever reads the checkpoint
// back for one — but every resumable RunFunc should call it regularly.
func (rc *RunContext) SaveCheckpoint(data []byte) error { return rc.saveCheckpoint(data) }

// SetProgress reports the job's current percentage (0-100), broadcast over
// the events hub (doc 01 §5's job_progress) and persisted so GetJob
// reflects it immediately.
func (rc *RunContext) SetProgress(pct int) { rc.setProgress(pct) }
