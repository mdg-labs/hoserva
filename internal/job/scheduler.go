package job

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/mdg-labs/hoserva/internal/disk"
)

// diskUpgradeDataCheckpointAtReleasing reports whether a data-disk
// upgrade checkpoint is at releasing: past its release decision, so the
// job is never cancellable again (doc 02 §4 E3, UR7, invariant 4).
func diskUpgradeDataCheckpointAtReleasing(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	var cp disk.DataDiskUpgradeCheckpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return false
	}
	return cp.Phase == disk.DataDiskUpgradePhaseReleasing
}

// RunFunc is a job type's actual work — the mover walking the pool mount,
// SnapRAID's sync, a container pull, and so on (doc 01 §4). This package
// runs it in a goroutine under RunContext's cancellable context; it
// computes nothing about placement or storage itself (D1) — RunFunc is
// where a future issue plugs in the real subsystem, via Registry.Register.
type RunFunc func(ctx context.Context, rc *RunContext) error

var (
	ErrMaintenanceMode   = errors.New("job: maintenance mode is active — new jobs are refused")
	ErrOnBattery         = errors.New("job: on battery — the mover is paused and scheduled syncs are held until power returns")
	ErrJobNotCancellable = errors.New("job: this job's tool does not support cancellation")
	ErrJobNotResumable   = errors.New("job: this job type does not persist a checkpoint to resume from")
	ErrJobNotInterrupted = errors.New("job: only an interrupted job can be resumed")
	ErrJobNotRunning     = errors.New("job: this job is not queued or running")
	// ErrTerminalSnapshotEvicted is returned by Await when a job's final
	// Store.UpdateStatus never succeeded, the in-memory terminal snapshot
	// was evicted to bound scheduler memory, and the store row is still
	// non-terminal — there is no safe way to keep waiting.
	ErrTerminalSnapshotEvicted = errors.New("job: terminal snapshot evicted before final status was persisted")
	// ErrArrayNotStopped refuses a data-disk upgrade unless the array's
	// stop sequence has completed (doc 02 §4 E8, UR3): maintenance mode
	// alone is not enough, since a stop that failed partway can leave
	// services or mounts up. It is the one job type maintenance mode
	// admits; every other type is refused while maintenance is active.
	ErrArrayNotStopped = errors.New("job: stop the array first (`hoserva array stop`) — a data-disk upgrade runs only once the stop sequence has completed")
	// ErrDiskUpgradePastRelease is Cancel's refusal once a data-disk
	// upgrade's checkpoint is at releasing, whether the job is queued,
	// running or interrupted (doc 02 §4 E3, invariant 4).
	ErrDiskUpgradePastRelease = fmt.Errorf("%w: the data-disk upgrade is past its release decision and has committed to the new disk — resume it to finish", ErrJobNotCancellable)
	// ErrJobAbortInProgress refuses a Cancel or Resume of a job whose
	// abort is already running (doc 02 §4 UR7).
	ErrJobAbortInProgress = errors.New("job: an abort of this job is already running")
	// ErrCancelRequested is what a data-disk upgrade's save of its
	// releasing checkpoint returns when a Cancel won the race for it
	// (doc 02 §4 E3, UR7): the checkpoint is not saved and Release never
	// runs.
	ErrCancelRequested = errors.New("job: cancel requested before the release decision was saved")
	// ErrJobNeedsRetry is a RunFunc's own signal that it stopped cleanly
	// at a resumable checkpoint on its own decision — never through a
	// Cancel or EnterMaintenance stop — and wants the scheduler to record
	// it exactly as it would a maintenance-mode interruption: resumable
	// through an explicit Resume, never a plain terminal failure with no
	// way back. A RunFunc wraps this with fmt.Errorf's %w.
	ErrJobNeedsRetry = errors.New("job: stopped at a resumable checkpoint; resume once the condition that stopped it clears")
)

// stopReason is set on a runningJob before it is asked to stop, so its
// completion handler (Scheduler.runJob) knows which terminal status to
// record — the same context cancellation is used for both a user Cancel
// and maintenance mode's forced stop of a non-resumable job, and only this
// field tells them apart afterwards.
type stopReason int

const (
	reasonNone stopReason = iota
	reasonCancel
	reasonMaintenance
	// reasonBatteryHold marks a mover job PauseForBattery asked to stop
	// at its next checkpoint (Q77) — mechanically the same "resumable job
	// stops gracefully" path EnterMaintenance already uses for
	// reasonMaintenance, so runJob still records it StatusInterrupted
	// (job.Status has no separate "paused" state). What makes an
	// on-battery pause different from a maintenance-mode interruption is
	// behavioural, not a status value: UPSController resumes it itself
	// once power returns, rather than leaving it for an explicit user
	// Resume the way Q29 requires after a restart or `array stop`.
	reasonBatteryHold
)

type runningJob struct {
	job    *Job
	run    RunFunc
	cancel context.CancelFunc
	stopCh chan struct{}
	done   chan struct{}

	// mu guards cancellable, reason and stopSignalled. Lock order is
	// Scheduler.mu, then mu.
	mu sync.Mutex
	// cancellable is decided with a cancel under mu: runJob's
	// saveCheckpoint clears it, and never sets it again, when a
	// TypeDiskUpgradeData run saves its releasing checkpoint (doc 02 §4
	// UR7).
	cancellable   bool
	reason        stopReason
	stopSignalled bool
}

type queuedJob struct {
	job         *Job
	run         RunFunc
	cancellable bool
}

// Scheduler is the job system's own scheduler (doc 01 §4): it enforces the
// mutually exclusive job classes itself — never trusting a caller (the API
// handlers, the CLI) to have checked first — persists every job through
// Store, captures its output through Logs, and broadcasts every state
// change through Hub for /api/v1/events (doc 01 §5).
type Scheduler struct {
	store    *Store
	logs     *LogStore
	hub      *Hub
	registry *Registry

	mu                          sync.Mutex
	running                     map[string]*runningJob
	queue                       []*queuedJob
	terminalSnapshots           map[string]*Job
	terminalSnapshotFIFO        []string
	evictedTerminalSnapshots    map[string]struct{}
	evictedTerminalSnapshotFIFO []string
	maintenance                 bool
	// arrayStopped is true only once ArraySequence.Stop has completed every
	// step since maintenance mode was last entered (doc 02 §4 UR3).
	arrayStopped bool
	// aborting holds the id of every job whose abort (Cancel of a queued
	// or interrupted job) is running, so no Resume or second Cancel of it
	// runs at the same time (doc 02 §4 UR7).
	aborting map[string]bool
	// batteryHold is Q77's own on-battery hold: lighter than maintenance
	// mode — it refuses only TypeMover and TypeSync (Submit and dispatch
	// both check it) and never touches a job of any other type, unlike
	// EnterMaintenance's own refusal of everything.
	batteryHold bool
}

// NewScheduler wires a Scheduler to its persistence, log capture, event
// broadcast and job-type registry. logs may be nil — a job then runs with
// its output discarded, which every test that isn't exercising Q74 itself
// uses to avoid needing a log directory.
func NewScheduler(store *Store, logs *LogStore, hub *Hub, registry *Registry) *Scheduler {
	return &Scheduler{
		store:    store,
		logs:     logs,
		hub:      hub,
		registry: registry,
		running:  make(map[string]*runningJob),
		aborting: make(map[string]bool),
	}
}

// RecoverFromRestart marks every job left queued or running interrupted
// (doc 01 §4: "marked interrupted after a restart, never resumed
// automatically"). It is the daemon's own responsibility to call this
// exactly once, before Scheduler accepts any Submit — Scheduler starts
// with empty running/queued sets regardless, so a restart never
// reconstructs in-flight work from the database on its own.
func (s *Scheduler) RecoverFromRestart(ctx context.Context) error {
	return s.store.InterruptActive(ctx, time.Now().UTC())
}

// Submit persists a new job of type t and starts it immediately unless its
// class conflicts with a job already running (doc 01 §4), in which case it
// is queued until dispatch() finds it a slot. t must have a RunFunc bound
// through Registry.Register — nothing here knows how to run any job type
// itself. params is the JSON request payload for types that have one
// (validated here against t); nil or empty for types that have none.
func (s *Scheduler) Submit(ctx context.Context, t Type, resourceIDs []string, params []byte) (*Job, error) {
	if err := ValidateType(t); err != nil {
		return nil, err
	}
	if err := ValidateParams(t, params); err != nil {
		return nil, err
	}
	entry, ok := s.registry.lookup(t)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrJobTypeNotRegistered, t)
	}
	class, _ := ClassOf(t)

	s.mu.Lock()
	if t == TypeDiskUpgradeData {
		if err := s.admitDiskUpgradeDataLocked(ctx); err != nil {
			s.mu.Unlock()
			return nil, err
		}
	} else if s.maintenance {
		s.mu.Unlock()
		return nil, ErrMaintenanceMode
	}
	if s.batteryHold && isBatteryHeldType(t) {
		s.mu.Unlock()
		return nil, ErrOnBattery
	}

	now := time.Now().UTC()
	j := &Job{
		ID:          uuid.NewString(),
		Type:        t,
		Class:       class,
		Resumable:   Resumable(t),
		Cancellable: entry.cancellable,
		ResourceIDs: resourceIDs,
		Params:      bytes.Clone(params),
		CreatedAt:   now,
	}
	if s.hasConflictWithRunningLocked(class, resourceIDs) {
		j.Status = StatusQueued
	} else {
		j.Status = StatusRunning
		j.StartedAt = &now
	}

	if err := s.store.Create(ctx, j); err != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("job: persisting new job: %w", err)
	}

	if j.Status == StatusQueued {
		s.queue = append(s.queue, &queuedJob{job: j, run: entry.run, cancellable: entry.cancellable})
	} else {
		s.startJobLocked(j, entry.run, entry.cancellable)
	}
	s.mu.Unlock()

	s.hub.Publish(j)
	return j, nil
}

// admitDiskUpgradeDataLocked is doc 02 §4 E8 for a new data-disk
// upgrade: refused while another is pending, and admitted only once the
// array's stop sequence has completed (UR3). Callers must hold s.mu, so
// the check and the job's creation are one step against ArraySequence
// .Start's own BeginArrayStart.
func (s *Scheduler) admitDiskUpgradeDataLocked(ctx context.Context) error {
	pending, err := s.store.ListPending(ctx, TypeDiskUpgradeData)
	if err != nil {
		return fmt.Errorf("job: checking for a pending data-disk upgrade: %w", err)
	}
	if len(pending) > 0 {
		return fmt.Errorf("%w: job %s", ErrDiskUpgradeDataPending, pending[0].ID)
	}
	if !s.maintenance || !s.arrayStopped {
		return ErrArrayNotStopped
	}
	return nil
}

// Cancel asks a running job to stop, removes a queued job, or ends an
// interrupted, cancellable job cancelled. A queued or interrupted job
// whose type registered an AbortFunc runs it first, and ends cancelled
// only once it succeeded; otherwise the job is left interrupted with the
// abort's error recorded, and the error is returned. For a data-disk
// upgrade that abort is its Unwind (doc 02 §4 E3). A running job's
// outcome is recorded by runJob once its run returns.
func (s *Scheduler) Cancel(ctx context.Context, id string) (*Job, error) {
	s.mu.Lock()
	if rj, ok := s.running[id]; ok {
		rj.mu.Lock()
		if !rj.cancellable {
			rj.mu.Unlock()
			s.mu.Unlock()
			if rj.job.Type == TypeDiskUpgradeData {
				return nil, ErrDiskUpgradePastRelease
			}
			return nil, ErrJobNotCancellable
		}
		rj.reason = reasonCancel
		rj.cancel()
		rj.mu.Unlock()
		snapshot := *rj.job
		s.mu.Unlock()
		return &snapshot, nil
	}

	for i, q := range s.queue {
		if q.job.ID != id {
			continue
		}
		if !q.cancellable && q.job.Type == TypeDiskUpgradeData {
			s.mu.Unlock()
			return nil, ErrDiskUpgradePastRelease
		}
		s.queue = append(s.queue[:i:i], s.queue[i+1:]...)
		abort, hasAbort := s.registry.lookupAbort(q.job.Type)
		if !hasAbort {
			now := time.Now().UTC()
			q.job.Status = StatusCancelled
			q.job.FinishedAt = &now
			snapshot := *q.job
			s.mu.Unlock()

			if err := s.store.UpdateStatus(ctx, id, StatusCancelled, snapshot.Progress, "", "", snapshot.StartedAt, &now); err != nil {
				return nil, fmt.Errorf("job: cancelling queued job %s: %w", id, err)
			}
			s.hub.Publish(&snapshot)
			return &snapshot, nil
		}
		s.aborting[id] = true
		job := *q.job
		s.mu.Unlock()
		defer s.releaseAbort(id)
		return s.abortAndCancel(ctx, &job, abort)
	}

	if s.aborting[id] {
		s.mu.Unlock()
		return nil, ErrJobAbortInProgress
	}
	s.aborting[id] = true
	s.mu.Unlock()
	defer s.releaseAbort(id)

	existing, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if existing.Status != StatusInterrupted {
		return nil, fmt.Errorf("%w: job %s has status %s", ErrJobNotRunning, id, existing.Status)
	}
	if existing.Type == TypeDiskUpgradeData && diskUpgradeDataCheckpointAtReleasing(existing.Checkpoint) {
		return nil, ErrDiskUpgradePastRelease
	}
	if !existing.Cancellable {
		return nil, ErrJobNotCancellable
	}
	abort, _ := s.registry.lookupAbort(existing.Type)
	return s.abortAndCancel(ctx, existing, abort)
}

// abortAndCancel runs abort, if any, then records j cancelled. If abort
// fails, j is recorded interrupted with the abort's error code instead
// (an *OutcomeError's Code, or job_abort_failed), and the error is
// returned. The caller holds j's entry in s.aborting. A caller that goes
// away does not stop an abort halfway.
func (s *Scheduler) abortAndCancel(ctx context.Context, j *Job, abort AbortFunc) (*Job, error) {
	ctx = context.WithoutCancel(ctx)
	if abort != nil {
		if err := abort(ctx, j.Params); err != nil {
			code := "job_abort_failed"
			var oe *OutcomeError
			if errors.As(err, &oe) && oe.Code != "" {
				code = oe.Code
			}
			now := time.Now().UTC()
			if serr := s.store.UpdateStatus(ctx, j.ID, StatusInterrupted, j.Progress, code, err.Error(), j.StartedAt, &now); serr != nil {
				return nil, fmt.Errorf("job: aborting job %s: %w (recording the failure: %v)", j.ID, err, serr)
			}
			j.Status = StatusInterrupted
			j.ErrorCode = code
			j.ErrorMessage = err.Error()
			j.FinishedAt = &now
			s.hub.Publish(j)
			return nil, fmt.Errorf("job: aborting job %s: %w", j.ID, err)
		}
	}
	now := time.Now().UTC()
	if err := s.store.UpdateStatus(ctx, j.ID, StatusCancelled, j.Progress, "", "", j.StartedAt, &now); err != nil {
		return nil, fmt.Errorf("job: cancelling job %s: %w", j.ID, err)
	}
	j.Status = StatusCancelled
	j.ErrorCode = ""
	j.ErrorMessage = ""
	j.FinishedAt = &now
	s.hub.Publish(j)
	return j, nil
}

func (s *Scheduler) releaseAbort(id string) {
	s.mu.Lock()
	delete(s.aborting, id)
	s.mu.Unlock()
}

// Resume restarts an interrupted, resumable job from its last checkpoint
// (Q29) — always an explicit call, never automatic. The job's type must
// still have a RunFunc bound through Registry.Register; a process restart
// loses nothing here since the registry is rebuilt at startup, not
// persisted. The job is read and started under s.mu, so a concurrent
// Cancel's abort of the same job and this never both run (doc 02 §4
// UR7). A data-disk upgrade resumed at releasing is not cancellable,
// decided and persisted before the job is visible to Cancel.
func (s *Scheduler) Resume(ctx context.Context, id string) (*Job, error) {
	s.mu.Lock()
	if s.aborting[id] {
		s.mu.Unlock()
		return nil, ErrJobAbortInProgress
	}
	existing, err := s.store.Get(ctx, id)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if !existing.Resumable {
		s.mu.Unlock()
		return nil, ErrJobNotResumable
	}
	if existing.Status != StatusInterrupted {
		s.mu.Unlock()
		return nil, ErrJobNotInterrupted
	}
	entry, ok := s.registry.lookup(existing.Type)
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrJobTypeNotRegistered, existing.Type)
	}
	if existing.Type == TypeDiskUpgradeData {
		if !s.maintenance {
			s.mu.Unlock()
			return nil, ErrArrayNotStopped
		}
	} else if s.maintenance {
		s.mu.Unlock()
		return nil, ErrMaintenanceMode
	}
	if s.batteryHold && isBatteryHeldType(existing.Type) {
		s.mu.Unlock()
		return nil, ErrOnBattery
	}

	cancellable := entry.cancellable
	if existing.Type == TypeDiskUpgradeData && diskUpgradeDataCheckpointAtReleasing(existing.Checkpoint) {
		cancellable = false
	}
	if cancellable != existing.Cancellable {
		if err := s.store.SetCancellable(ctx, id, cancellable); err != nil {
			s.mu.Unlock()
			return nil, fmt.Errorf("job: resuming job %s: %w", id, err)
		}
		existing.Cancellable = cancellable
	}

	now := time.Now().UTC()
	if s.hasConflictWithRunningLocked(existing.Class, existing.ResourceIDs) {
		existing.Status = StatusQueued
		existing.FinishedAt = nil
	} else {
		existing.Status = StatusRunning
		existing.StartedAt = &now
		existing.FinishedAt = nil
	}
	existing.ErrorCode = ""
	existing.ErrorMessage = ""

	if err := s.store.UpdateStatus(ctx, id, existing.Status, existing.Progress, "", "", existing.StartedAt, existing.FinishedAt); err != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("job: resuming job %s: %w", id, err)
	}

	if existing.Status == StatusQueued {
		s.queue = append(s.queue, &queuedJob{job: existing, run: entry.run, cancellable: cancellable})
	} else {
		s.startJobLocked(existing, entry.run, cancellable)
	}
	snapshot := *existing
	s.mu.Unlock()

	s.hub.Publish(&snapshot)
	return &snapshot, nil
}

// EnterMaintenance puts the scheduler in maintenance mode (Q70,
// `hoserva array stop`): Submit and Resume refuse from this point on,
// every resumable job currently running is asked to stop at its next
// checkpoint, and every non-resumable running job — and every job that was
// only queued, never started — is marked interrupted immediately, since
// neither has a checkpoint worth preserving.
//
// Calling it again while already in maintenance mode signals any running
// job it has not signalled yet — a data-disk upgrade, the one job type
// maintenance mode admits — so a shutdown's stop sequence stops it at its
// next checkpoint too (doc 02 §4 E4). Every call clears the "stop
// sequence completed" state (UR3) until ArraySequence.Stop sets it again.
func (s *Scheduler) EnterMaintenance(ctx context.Context) error {
	s.mu.Lock()
	s.maintenance = true
	s.arrayStopped = false

	queue := s.queue
	s.queue = nil
	running := make([]*runningJob, 0, len(s.running))
	for _, rj := range s.running {
		running = append(running, rj)
	}
	s.mu.Unlock()

	now := time.Now().UTC()
	for _, q := range queue {
		q.job.Status = StatusInterrupted
		q.job.FinishedAt = &now
		if err := s.store.UpdateStatus(ctx, q.job.ID, StatusInterrupted, q.job.Progress, "", "", q.job.StartedAt, &now); err != nil {
			log.Printf("job: marking queued job %s interrupted for maintenance mode: %v", q.job.ID, err)
			continue
		}
		s.hub.Publish(q.job)
	}

	for _, rj := range running {
		rj.mu.Lock()
		if rj.stopSignalled {
			rj.mu.Unlock()
			continue
		}
		rj.stopSignalled = true
		if rj.reason == reasonNone {
			rj.reason = reasonMaintenance
		}
		rj.mu.Unlock()
		if rj.job.Resumable {
			close(rj.stopCh)
		} else {
			rj.cancel()
		}
	}
	return nil
}

// Drain blocks until every job running at the moment of the call has
// actually finished — succeeded, failed, cancelled or interrupted — or
// ctx is done, whichever comes first. EnterMaintenance only signals
// running jobs to stop and returns without waiting for them; a caller
// that must not proceed until the array is genuinely idle (ArraySequence
// .Stop, doc 02 §4) calls Drain immediately after EnterMaintenance.
func (s *Scheduler) Drain(ctx context.Context) error {
	s.mu.Lock()
	dones := make([]<-chan struct{}, 0, len(s.running))
	for _, rj := range s.running {
		dones = append(dones, rj.done)
	}
	s.mu.Unlock()

	for _, done := range dones {
		select {
		case <-done:
		case <-ctx.Done():
			return fmt.Errorf("job: waiting for running jobs to stop: %w", ctx.Err())
		}
	}
	return nil
}

// ExitMaintenance reverses EnterMaintenance (`hoserva array start`, Q70).
// It does not resume anything on its own — every interrupted job stays
// interrupted until an explicit Resume call.
func (s *Scheduler) ExitMaintenance() {
	s.mu.Lock()
	s.maintenance = false
	s.arrayStopped = false
	s.mu.Unlock()
}

// MarkArrayStopped records that ArraySequence.Stop completed every step
// (doc 02 §4 UR3): the only state that admits a data-disk upgrade.
func (s *Scheduler) MarkArrayStopped() {
	s.mu.Lock()
	s.arrayStopped = s.maintenance
	s.mu.Unlock()
}

// BeginArrayStart is ArraySequence.Start's first step: it refuses while a
// data-disk upgrade is pending (doc 02 §4 E6), and otherwise clears the
// "stop sequence completed" state under the same lock Submit admits a
// data-disk upgrade under, so no upgrade is admitted once a start begins.
func (s *Scheduler) BeginArrayStart(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending, err := s.store.ListPending(ctx, TypeDiskUpgradeData)
	if err != nil {
		return fmt.Errorf("job: checking for a pending data-disk upgrade: %w", err)
	}
	if len(pending) > 0 {
		return fmt.Errorf("%w: job %s — resume it, or cancel it to abort back to the old disk", ErrDiskUpgradeDataPending, pending[0].ID)
	}
	s.arrayStopped = false
	return nil
}

// InMaintenance reports whether maintenance mode is currently active.
func (s *Scheduler) InMaintenance() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maintenance
}

// isBatteryHeldType reports whether t is one of the two types Q77's
// on-battery hold refuses — exactly the mover and sync, never scrub,
// fix, check or any other class (Q77's own default: "pause the mover
// and hold scheduled syncs", not the whole Parity/Array-write classes).
func isBatteryHeldType(t Type) bool {
	return t == TypeMover || t == TypeSync
}

// PauseForBattery is Q77's on-battery reaction: from this point on,
// Submit and dispatch refuse a new or queued TypeMover/TypeSync job
// (ErrOnBattery) until ResumeFromBattery is called, and any TypeMover
// job currently running is asked to stop at its next checkpoint — the
// same graceful-stop mechanism EnterMaintenance already uses for a
// resumable job, but without touching anything else: a sync already
// running keeps running (Q77 only "holds" a sync that hasn't started
// yet), and no other job class is affected at all. It returns the ID of
// the mover job it asked to stop, if any, so a caller (job.UPSController)
// can resume it once power returns — only once that job has actually
// finished stopping, waited for the same way Drain waits for a running
// job's own done channel, so a short outage (ONLINE arriving before the
// stop is fully processed) can never race Resume against a checkpoint
// still being written: by the time PauseForBattery returns, the job's
// own Store.UpdateStatus(StatusInterrupted) has already happened, and
// Resume will find it interrupted rather than failing with
// ErrJobNotInterrupted and leaving it stuck. Idempotent: calling it
// again while already on battery is a no-op.
func (s *Scheduler) PauseForBattery(ctx context.Context) []string {
	s.mu.Lock()
	if s.batteryHold {
		s.mu.Unlock()
		return nil
	}
	s.batteryHold = true
	var toStop []*runningJob
	for _, rj := range s.running {
		if rj.job.Type == TypeMover {
			toStop = append(toStop, rj)
		}
	}
	s.mu.Unlock()

	paused := make([]string, 0, len(toStop))
	for _, rj := range toStop {
		rj.mu.Lock()
		if rj.stopSignalled {
			rj.mu.Unlock()
			continue
		}
		rj.stopSignalled = true
		rj.reason = reasonBatteryHold
		rj.mu.Unlock()
		close(rj.stopCh)
		paused = append(paused, rj.job.ID)
	}
	for _, rj := range toStop {
		select {
		case <-rj.done:
		case <-ctx.Done():
		}
	}
	return paused
}

// ResumeFromBattery reverses PauseForBattery: Submit and dispatch stop
// refusing a mover or sync job. It does not itself resume the mover job
// PauseForBattery paused — job.UPSController calls Resume for that,
// using the ID PauseForBattery returned, once it has called this.
func (s *Scheduler) ResumeFromBattery() {
	s.mu.Lock()
	s.batteryHold = false
	s.mu.Unlock()
	// Unlike ExitMaintenance, PauseForBattery never emptied the queue —
	// a held mover or sync job is still sitting there, queued, waiting.
	// dispatch() is what actually starts it now that the hold is gone.
	s.dispatch()
}

// OnBattery reports whether the on-battery hold is currently active.
func (s *Scheduler) OnBattery() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.batteryHold
}

// BlockingStorageJob returns a currently running Parity, Array-write or
// Topology job, if any. Self-update and rollback refuse while one is
// running and name it (Q67); reboot waits for it instead (Q68).
func (s *Scheduler) BlockingStorageJob() *Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rj := range s.running {
		if IsStorageClass(rj.job.Class) {
			cp := *rj.job
			return &cp
		}
	}
	return nil
}

// WaitForStorageJobs blocks until no Parity, Array-write or Topology job
// is running, or ctx is done. Reboot uses this before the Q70 sequence
// (Q68) — it waits rather than refusing.
func (s *Scheduler) WaitForStorageJobs(ctx context.Context) error {
	for {
		s.mu.Lock()
		var done <-chan struct{}
		for _, rj := range s.running {
			if IsStorageClass(rj.job.Class) {
				done = rj.done
				break
			}
		}
		s.mu.Unlock()
		if done == nil {
			return nil
		}
		select {
		case <-done:
		case <-ctx.Done():
			return fmt.Errorf("job: waiting for storage jobs: %w", ctx.Err())
		}
	}
}

// PendingDiskUpgradeData returns the oldest data-disk upgrade that is
// pending — queued, running or interrupted at any checkpoint — or nil
// (doc 02 §4: pending from submit until succeeded, failed or cancelled).
// It reads the store, which records a job's end only after runJob has
// finished it, so a job is still pending until its outcome is written.
func (s *Scheduler) PendingDiskUpgradeData(ctx context.Context) (*Job, error) {
	pending, err := s.store.ListPending(ctx, TypeDiskUpgradeData)
	if err != nil {
		return nil, fmt.Errorf("job: listing pending data-disk upgrades: %w", err)
	}
	if len(pending) == 0 {
		return nil, nil
	}
	return pending[0], nil
}

// awaitPollInterval is Await's fallback tick: Hub.Publish drops an update
// for a subscriber whose buffer is full rather than blocking (Hub's own doc
// comment), so a slow receiver can miss the exact event that would have
// woken it. Await treats every wakeup as only a hint to re-check the store,
// never as truth on its own, so a dropped event costs at most one tick of
// latency, not a hang.
const awaitPollInterval = 25 * time.Millisecond

// maxTerminalSnapshots caps how many failed final-status writes the
// scheduler remembers in memory. Await and chain sequencing only need id,
// terminal status and error fields from these entries — not Checkpoint or
// Params — so each snapshot is stored without those slices.
const maxTerminalSnapshots = 64

const (
	finalStatusWriteRetries    = 3
	finalStatusWriteRetryDelay = 10 * time.Millisecond
)

// Await blocks until id reaches a terminal status (succeeded, failed,
// cancelled or interrupted) or ctx is done, whichever comes first. It is
// the primitive MaintenanceChain (chain.go) uses to know a job-backed step
// has actually finished — not just that Submit returned — before starting
// the next one (Q30: "each step starts when the previous one finishes").
//
// runJob sets the job's final Status in memory and publishes it through
// Hub regardless of whether the matching Store.UpdateStatus itself
// succeeded (it only logs that failure) — so a store row can be left
// non-terminal even after the job has genuinely finished. Awaiting only
// s.store.Get would then poll forever until ctx is done. When the store
// row is still non-terminal, Await also consults the scheduler's own
// terminalSnapshots entry — the same snapshot runJob built when the final
// write failed — and trusts a delivered Hub message for id that is already
// terminal, instead of only using Hub as a hint to recheck the store.
func (s *Scheduler) Await(ctx context.Context, id string) (*Job, error) {
	ch, unsubscribe := s.hub.Subscribe()
	defer unsubscribe()

	for {
		j, err := s.store.Get(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("job: awaiting job %s: %w", id, err)
		}
		if j.Status.Terminal() {
			s.forgetTerminalSnapshot(id)
			return j, nil
		}
		if snap, ok := s.terminalSnapshot(id); ok {
			return snap, nil
		}
		if s.terminalSnapshotEvicted(id) {
			return nil, fmt.Errorf("job: awaiting job %s: %w", id, ErrTerminalSnapshotEvicted)
		}
		select {
		case published := <-ch:
			if published != nil && published.ID == id && published.Status.Terminal() {
				return published, nil
			}
			// Some other job's update, or not yet terminal — only a hint
			// to loop and re-check the store above; Hub fans out every
			// job's updates, not just id's, and may have dropped the one
			// that actually matters here.
		case <-time.After(awaitPollInterval):
		case <-ctx.Done():
			return nil, fmt.Errorf("job: awaiting job %s: %w", id, ctx.Err())
		}
	}
}

func (s *Scheduler) terminalSnapshot(id string) (*Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, ok := s.terminalSnapshots[id]
	if !ok || !snap.Status.Terminal() {
		return nil, false
	}
	copy := *snap
	return &copy, true
}

func terminalSnapshotFrom(j Job) Job {
	return Job{
		ID:           j.ID,
		Status:       j.Status,
		ErrorCode:    j.ErrorCode,
		ErrorMessage: j.ErrorMessage,
		StartedAt:    j.StartedAt,
		FinishedAt:   j.FinishedAt,
	}
}

func (s *Scheduler) rememberTerminalSnapshot(j Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.terminalSnapshots == nil {
		s.terminalSnapshots = make(map[string]*Job)
		s.evictedTerminalSnapshots = make(map[string]struct{})
	}
	s.clearEvictedTerminalSnapshotLocked(j.ID)
	if _, exists := s.terminalSnapshots[j.ID]; exists {
		snap := terminalSnapshotFrom(j)
		s.terminalSnapshots[j.ID] = &snap
		return
	}
	for len(s.terminalSnapshotFIFO) >= maxTerminalSnapshots {
		evictID := s.terminalSnapshotFIFO[0]
		s.terminalSnapshotFIFO = s.terminalSnapshotFIFO[1:]
		delete(s.terminalSnapshots, evictID)
		s.markTerminalSnapshotEvictedLocked(evictID)
	}
	snap := terminalSnapshotFrom(j)
	s.terminalSnapshots[j.ID] = &snap
	s.terminalSnapshotFIFO = append(s.terminalSnapshotFIFO, j.ID)
}

func (s *Scheduler) markTerminalSnapshotEvictedLocked(id string) {
	if _, ok := s.evictedTerminalSnapshots[id]; ok {
		return
	}
	s.evictedTerminalSnapshots[id] = struct{}{}
	s.evictedTerminalSnapshotFIFO = append(s.evictedTerminalSnapshotFIFO, id)
	for len(s.evictedTerminalSnapshotFIFO) > maxTerminalSnapshots {
		old := s.evictedTerminalSnapshotFIFO[0]
		s.evictedTerminalSnapshotFIFO = s.evictedTerminalSnapshotFIFO[1:]
		delete(s.evictedTerminalSnapshots, old)
	}
}

func (s *Scheduler) clearEvictedTerminalSnapshotLocked(id string) {
	delete(s.evictedTerminalSnapshots, id)
	removeString(&s.evictedTerminalSnapshotFIFO, id)
}

func removeString(ids *[]string, id string) {
	for i, fid := range *ids {
		if fid == id {
			*ids = append((*ids)[:i], (*ids)[i+1:]...)
			return
		}
	}
}

func (s *Scheduler) terminalSnapshotEvicted(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.evictedTerminalSnapshots[id]
	return ok
}

func (s *Scheduler) forgetTerminalSnapshot(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.terminalSnapshots, id)
	s.clearEvictedTerminalSnapshotLocked(id)
	removeString(&s.terminalSnapshotFIFO, id)
}

// hasConflictWithRunningLocked reports whether a job of class/resourceIDs
// conflicts with any currently running job (doc 01 §4). Callers must hold
// s.mu.
func (s *Scheduler) hasConflictWithRunningLocked(class Class, resourceIDs []string) bool {
	for _, rj := range s.running {
		if conflicts(class, rj.job.Class, resourceIDs, rj.job.ResourceIDs) {
			return true
		}
	}
	return false
}

// startJobLocked launches j's goroutine. Callers must hold s.mu, and must
// have already persisted j with a running status.
func (s *Scheduler) startJobLocked(j *Job, run RunFunc, cancellable bool) {
	ctx, cancel := context.WithCancel(context.Background())
	rj := &runningJob{
		job:         j,
		run:         run,
		cancellable: cancellable,
		cancel:      cancel,
		stopCh:      make(chan struct{}),
		done:        make(chan struct{}),
	}
	s.running[j.ID] = rj
	go s.runJob(ctx, rj)
}

// dispatch starts every still-queued job that no longer conflicts with
// what's currently running, in submission order, skipping past (not
// blocking on) a queued job that still conflicts so an unrelated class
// isn't held up behind it. In maintenance mode only a data-disk upgrade,
// the one type it admits, is started (doc 02 §4 E1 Queued).
func (s *Scheduler) dispatch() {
	s.mu.Lock()
	defer s.mu.Unlock()

	var remaining []*queuedJob
	started := make([]*Job, 0, len(s.queue))
	for _, q := range s.queue {
		if s.maintenance && q.job.Type != TypeDiskUpgradeData {
			remaining = append(remaining, q)
			continue
		}
		if s.batteryHold && isBatteryHeldType(q.job.Type) {
			remaining = append(remaining, q)
			continue
		}
		conflict := s.hasConflictWithRunningLocked(q.job.Class, q.job.ResourceIDs)
		if !conflict {
			for _, sj := range started {
				if conflicts(q.job.Class, sj.Class, q.job.ResourceIDs, sj.ResourceIDs) {
					conflict = true
					break
				}
			}
		}
		if conflict {
			remaining = append(remaining, q)
			continue
		}

		now := time.Now().UTC()
		q.job.Status = StatusRunning
		q.job.StartedAt = &now
		if err := s.store.UpdateStatus(context.Background(), q.job.ID, StatusRunning, q.job.Progress, "", "", q.job.StartedAt, nil); err != nil {
			log.Printf("job: starting queued job %s: %v", q.job.ID, err)
			remaining = append(remaining, q)
			continue
		}
		s.startJobLocked(q.job, q.run, q.cancellable)
		started = append(started, q.job)
		s.hub.Publish(q.job)
	}
	s.queue = remaining
}

// runJob executes rj.run to completion, records the outcome, and hands off
// to dispatch() so anything waiting behind rj's class can start.
func (s *Scheduler) runJob(ctx context.Context, rj *runningJob) {
	defer close(rj.done)

	out := io.Discard
	var closer io.Closer
	if s.logs != nil {
		w, err := s.logs.Create(rj.job.ID)
		if err != nil {
			log.Printf("job: opening log for job %s: %v", rj.job.ID, err)
		} else {
			out = w
			closer = w
		}
	}

	rc := &RunContext{
		ctx:           ctx,
		checkpoint:    rj.job.Checkpoint,
		params:        rj.job.Params,
		stopRequested: rj.stopCh,
		out:           out,
		saveCheckpoint: func(data []byte) error {
			if rj.job.Type == TypeDiskUpgradeData && diskUpgradeDataCheckpointAtReleasing(data) {
				return s.saveDiskUpgradeReleasing(rj, data)
			}
			return s.store.SaveCheckpoint(context.Background(), rj.job.ID, data)
		},
		setProgress: func(pct int) {
			p := pct
			if err := s.store.UpdateProgress(context.Background(), rj.job.ID, &p); err != nil {
				log.Printf("job: recording progress for job %s: %v", rj.job.ID, err)
			}
			s.mu.Lock()
			rj.job.Progress = &p
			snapshot := *rj.job
			s.mu.Unlock()
			s.hub.Publish(&snapshot)
		},
	}

	runErr := rj.run(ctx, rc)

	if closer != nil {
		if err := closer.Close(); err != nil {
			log.Printf("job: closing log for job %s: %v", rj.job.ID, err)
		}
	}

	rj.mu.Lock()
	reason := rj.reason
	rj.mu.Unlock()

	now := time.Now().UTC()
	var status Status
	var errCode, errMessage string
	var outcome *OutcomeError
	hasOutcome := errors.As(runErr, &outcome)
	switch {
	case hasOutcome && outcome.Status == StatusInterrupted:
		status = StatusInterrupted
		errCode = outcome.Code
		errMessage = runErr.Error()
	case reason == reasonCancel:
		status = StatusCancelled
	case hasOutcome:
		status = outcome.Status
		errCode = outcome.Code
		errMessage = runErr.Error()
	case reason == reasonMaintenance, reason == reasonBatteryHold:
		status = StatusInterrupted
	case errors.Is(runErr, ErrJobNeedsRetry):
		status = StatusInterrupted
		errCode = "job_needs_retry"
		errMessage = runErr.Error()
	case runErr != nil:
		status = StatusFailed
		errCode = "job_failed"
		errMessage = runErr.Error()
	default:
		status = StatusSucceeded
	}

	// Drop the job from s.running as soon as its run has actually
	// finished, ahead of the store write below — not after it. The store
	// is the source of truth (D4): once it shows a terminal status, an
	// external caller (Cancel, in particular) must never still find the
	// job in s.running and misreport it as merely non-cancellable instead
	// of not-running. This only touches the bookkeeping map; rj.job's own
	// fields are still mutated at their original point, below.
	s.mu.Lock()
	delete(s.running, rj.job.ID)
	s.mu.Unlock()

	storeErr := s.store.UpdateStatus(context.Background(), rj.job.ID, status, rj.job.Progress, errCode, errMessage, rj.job.StartedAt, &now)
	for attempt := 1; storeErr != nil && attempt < finalStatusWriteRetries; attempt++ {
		time.Sleep(finalStatusWriteRetryDelay)
		storeErr = s.store.UpdateStatus(context.Background(), rj.job.ID, status, rj.job.Progress, errCode, errMessage, rj.job.StartedAt, &now)
	}
	if storeErr != nil {
		log.Printf("job: recording final status for job %s: %v", rj.job.ID, storeErr)
	}

	s.mu.Lock()
	rj.job.Status = status
	rj.job.ErrorCode = errCode
	rj.job.ErrorMessage = errMessage
	rj.job.FinishedAt = &now
	finished := *rj.job
	s.mu.Unlock()

	if storeErr != nil {
		s.rememberTerminalSnapshot(finished)
	}

	s.hub.Publish(&finished)
	s.dispatch()
}

// saveDiskUpgradeReleasing saves a data-disk upgrade's releasing
// checkpoint and makes the job uncancellable, decided under the same
// locks Cancel takes (doc 02 §4 E3 race row, UR7): if a Cancel came
// first, nothing is saved and ErrCancelRequested is returned, so Release
// never runs; otherwise every later Cancel is refused.
func (s *Scheduler) saveDiskUpgradeReleasing(rj *runningJob, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rj.mu.Lock()
	defer rj.mu.Unlock()
	if rj.reason == reasonCancel {
		return ErrCancelRequested
	}
	if err := s.store.SaveCheckpoint(context.Background(), rj.job.ID, data); err != nil {
		return err
	}
	rj.cancellable = false
	rj.job.Cancellable = false
	if err := s.store.SetCancellable(context.Background(), rj.job.ID, false); err != nil {
		log.Printf("job: recording job %s as no longer cancellable: %v", rj.job.ID, err)
	}
	return nil
}

// OutcomeError lets a RunFunc record a specific status and error code.
// Status is StatusFailed or StatusInterrupted. An interrupted outcome is
// recorded even over a Cancel: the run could not establish what a
// cancelled or failed status would claim (doc 02 §4 invariant 3). A
// failed outcome yields to a Cancel. An AbortFunc may return one to name
// the error code Cancel records when the abort fails.
type OutcomeError struct {
	Status Status
	Code   string
	Err    error
}

func (e *OutcomeError) Error() string { return e.Err.Error() }

func (e *OutcomeError) Unwrap() error { return e.Err }
