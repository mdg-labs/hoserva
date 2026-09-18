package job

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
)

// RunFunc is a job type's actual work — the mover walking the pool mount,
// SnapRAID's sync, a container pull, and so on (doc 01 §4). This package
// runs it in a goroutine under RunContext's cancellable context; it
// computes nothing about placement or storage itself (D1) — RunFunc is
// where a future issue plugs in the real subsystem, via Registry.Register.
type RunFunc func(ctx context.Context, rc *RunContext) error

var (
	ErrMaintenanceMode   = errors.New("job: maintenance mode is active — new jobs are refused")
	ErrJobNotCancellable = errors.New("job: this job's tool does not support cancellation")
	ErrJobNotResumable   = errors.New("job: this job type does not persist a checkpoint to resume from")
	ErrJobNotInterrupted = errors.New("job: only an interrupted job can be resumed")
	ErrJobNotRunning     = errors.New("job: this job is not queued or running")
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
)

type runningJob struct {
	job         *Job
	run         RunFunc
	cancellable bool
	cancel      context.CancelFunc
	stopCh      chan struct{}
	done        chan struct{}

	mu     sync.Mutex
	reason stopReason
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

	mu          sync.Mutex
	running     map[string]*runningJob
	queue       []*queuedJob
	maintenance bool
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
// itself.
func (s *Scheduler) Submit(ctx context.Context, t Type, resourceIDs []string) (*Job, error) {
	if err := ValidateType(t); err != nil {
		return nil, err
	}
	entry, ok := s.registry.lookup(t)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrJobTypeNotRegistered, t)
	}
	class, _ := ClassOf(t)

	s.mu.Lock()
	if s.maintenance {
		s.mu.Unlock()
		return nil, ErrMaintenanceMode
	}

	now := time.Now().UTC()
	j := &Job{
		ID:          uuid.NewString(),
		Type:        t,
		Class:       class,
		Resumable:   Resumable(t),
		Cancellable: entry.cancellable,
		ResourceIDs: resourceIDs,
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

// Cancel asks a running job's tool to stop, or removes a still-queued job
// outright. A running job whose type honestly can't be cancelled reports
// ErrJobNotCancellable rather than accepting the call and doing nothing
// (doc 01 §4).
func (s *Scheduler) Cancel(ctx context.Context, id string) (*Job, error) {
	s.mu.Lock()
	if rj, ok := s.running[id]; ok {
		if !rj.cancellable {
			s.mu.Unlock()
			return nil, ErrJobNotCancellable
		}
		rj.mu.Lock()
		rj.reason = reasonCancel
		rj.mu.Unlock()
		rj.cancel()
		snapshot := *rj.job
		s.mu.Unlock()
		return &snapshot, nil
	}

	for i, q := range s.queue {
		if q.job.ID != id {
			continue
		}
		s.queue = append(s.queue[:i:i], s.queue[i+1:]...)
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
	s.mu.Unlock()

	existing, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: job %s has status %s", ErrJobNotRunning, id, existing.Status)
}

// Resume restarts an interrupted, resumable job from its last checkpoint
// (Q29) — always an explicit call, never automatic. The job's type must
// still have a RunFunc bound through Registry.Register; a process restart
// loses nothing here since the registry is rebuilt at startup, not
// persisted.
func (s *Scheduler) Resume(ctx context.Context, id string) (*Job, error) {
	existing, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !existing.Resumable {
		return nil, ErrJobNotResumable
	}
	if existing.Status != StatusInterrupted {
		return nil, ErrJobNotInterrupted
	}
	entry, ok := s.registry.lookup(existing.Type)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrJobTypeNotRegistered, existing.Type)
	}

	s.mu.Lock()
	if s.maintenance {
		s.mu.Unlock()
		return nil, ErrMaintenanceMode
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

	if err := s.store.UpdateStatus(ctx, id, existing.Status, existing.Progress, "", "", existing.StartedAt, existing.FinishedAt); err != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("job: resuming job %s: %w", id, err)
	}

	if existing.Status == StatusQueued {
		s.queue = append(s.queue, &queuedJob{job: existing, run: entry.run, cancellable: entry.cancellable})
	} else {
		s.startJobLocked(existing, entry.run, entry.cancellable)
	}
	s.mu.Unlock()

	s.hub.Publish(existing)
	return existing, nil
}

// EnterMaintenance puts the scheduler in maintenance mode (Q70,
// `hoserva array stop`): Submit and Resume refuse from this point on,
// every resumable job currently running is asked to stop at its next
// checkpoint, and every non-resumable running job — and every job that was
// only queued, never started — is marked interrupted immediately, since
// neither has a checkpoint worth preserving.
func (s *Scheduler) EnterMaintenance(ctx context.Context) error {
	s.mu.Lock()
	if s.maintenance {
		s.mu.Unlock()
		return nil
	}
	s.maintenance = true

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
		rj.reason = reasonMaintenance
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
	s.mu.Unlock()
}

// InMaintenance reports whether maintenance mode is currently active.
func (s *Scheduler) InMaintenance() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maintenance
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
// isn't held up behind it.
func (s *Scheduler) dispatch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.maintenance {
		return
	}

	var remaining []*queuedJob
	started := make([]*Job, 0, len(s.queue))
	for _, q := range s.queue {
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
		stopRequested: rj.stopCh,
		out:           out,
		saveCheckpoint: func(data []byte) error {
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
	switch {
	case reason == reasonCancel:
		status = StatusCancelled
	case reason == reasonMaintenance:
		status = StatusInterrupted
	case runErr != nil:
		status = StatusFailed
		errCode = "job_failed"
		errMessage = runErr.Error()
	default:
		status = StatusSucceeded
	}

	if err := s.store.UpdateStatus(context.Background(), rj.job.ID, status, rj.job.Progress, errCode, errMessage, rj.job.StartedAt, &now); err != nil {
		log.Printf("job: recording final status for job %s: %v", rj.job.ID, err)
	}

	s.mu.Lock()
	rj.job.Status = status
	rj.job.ErrorCode = errCode
	rj.job.ErrorMessage = errMessage
	rj.job.FinishedAt = &now
	finished := *rj.job
	delete(s.running, rj.job.ID)
	s.mu.Unlock()

	s.hub.Publish(&finished)
	s.dispatch()
}
