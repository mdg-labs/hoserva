package job

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"
	storedb "github.com/mdg-labs/hoserva/internal/store/db"
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
	// ErrJobResumeInProgress refuses a Cancel or a second Resume of a job
	// whose Resume is still repairing its log outside s.mu. It is not an
	// ErrJobAbortInProgress: the job is being resumed, not aborted.
	ErrJobResumeInProgress = errors.New("job: a resume of this job is already running")
	// ErrCancelRequested is what a data-disk upgrade's save of its
	// releasing checkpoint returns when a Cancel won the race for it
	// (doc 02 §4 E3, UR7): the checkpoint is not saved and Release never
	// runs.
	ErrCancelRequested = errors.New("job: cancel requested before the release decision was saved")
	// ErrEvacuationPending refuses a new evacuation while another one
	// is queued, running or interrupted (#359). Callers wrap it with the
	// pending job's id.
	ErrEvacuationPending = errors.New("job: an evacuation is already pending")
	// ErrJobNeedsRetry is a RunFunc's own signal that it stopped cleanly
	// at a resumable checkpoint on its own decision — never through a
	// Cancel or EnterMaintenance stop — and wants the scheduler to record
	// it exactly as it would a maintenance-mode interruption: resumable
	// through an explicit Resume, never a plain terminal failure with no
	// way back. A RunFunc wraps this with fmt.Errorf's %w.
	ErrJobNeedsRetry = errors.New("job: stopped at a resumable checkpoint; resume once the condition that stopped it clears")
	// ErrDatabaseRestoreInProgress refuses Submit and Resume while a
	// whole-database restore holds job admission (BeginDatabaseRestore):
	// a config import (doc 10 §1) that overwrites the jobs table
	// underneath a job whose runner is still going (#402).
	ErrDatabaseRestoreInProgress = errors.New("job: a database restore is in progress — jobs are refused until it completes")
	// ErrJobsActiveForRestore refuses BeginDatabaseRestore while any job
	// is queued or running, while any job's Cancel abort is still
	// running, or while a Resume is still repairing a job's log: a whole-database restore must never overwrite a live job's
	// row while its runner is still going, and must never start while
	// abortAndCancel's own writes (manifest clearing, removal-state
	// release, final status) could still land against the table it is
	// about to replace (#402).
	ErrJobsActiveForRestore = errors.New("job: a job is queued, running or being aborted — refuse the database restore until it finishes")
	// ErrJobAlreadyRunning refuses Resume of a job whose runner is still
	// alive in this scheduler's own s.running, whatever its stored status
	// says — a row a restore overwrote or mislabelled while the runner
	// kept going must never start a second, concurrent run of the same
	// job (#402).
	ErrJobAlreadyRunning = errors.New("job: this job is already running")
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

	// mu guards cancellable, reason, stopSignalled and finished. Lock
	// order is Scheduler.mu, then mu.
	mu sync.Mutex
	// cancellable is decided with a cancel under mu: runJob's
	// saveCheckpoint clears it, and never sets it again, when a
	// TypeDiskUpgradeData run saves its releasing checkpoint (doc 02 §4
	// UR7).
	cancellable   bool
	reason        stopReason
	stopSignalled bool
	// finished is set true by runJob, under mu, the instant rj.run
	// returns — before runJob does anything else, including reading
	// reason for its own status decision. A job still appears in
	// s.running until runJob later deletes it, so without this a Cancel
	// racing in after rj.run has already returned (but before that
	// delete) could still find the job here and treat it as live: it
	// would mutate reason to reasonCancel too late to change what the
	// run actually did, silently misreporting the job's real,
	// already-decided outcome as cancelled and never invoking the job
	// type's AbortFunc, orphaning whatever job-owned state that outcome
	// legitimately kept (#364). Cancel checks finished under the same
	// lock and refuses once it is set, rather than racing to overwrite
	// reason.
	finished bool
}

type queuedJob struct {
	job         *Job
	run         RunFunc
	cancellable bool
	// cancelRequested is set by Cancel, under s.mu, for a ClassTopology
	// job dispatch() has already moved into s.dispatching (#408): the
	// backup running for it outside s.mu cannot be interrupted, but
	// startQueuedTopologyJob checks this once it returns and ends the job
	// cancelled instead of starting it, whatever the backup itself
	// decided.
	cancelRequested bool
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
	// resuming holds the id of every job whose Resume is repairing its log
	// outside s.mu, so no second Resume or Cancel of it runs meanwhile. It
	// is separate from aborting because BeginDatabaseRestore refuses on
	// either, but only aborting is about a Cancel's abort.
	resuming map[string]bool
	// batteryHold is Q77's own on-battery hold: lighter than maintenance
	// mode — it refuses only TypeMover and TypeSync (Submit and dispatch
	// both check it) and never touches a job of any other type, unlike
	// EnterMaintenance's own refusal of everything.
	batteryHold bool
	// databaseRestore is BeginDatabaseRestore's own hold (#402): while
	// true, Submit and Resume refuse every job, of every type, and Cancel
	// refuses outright, with ErrDatabaseRestoreInProgress — a
	// whole-database restore (ImportConfig) is about to overwrite the
	// jobs table wholesale, and nothing may be admitted, resumed or
	// cancelled underneath it.
	databaseRestore bool
	// shareMutations counts share create/update/delete calls admitted
	// before maintenance mode. ArraySequence.Stop waits for it to drain
	// before unmounting, so a mutation that passed the maintenance check
	// cannot mkdir on a disk after that disk is gone.
	shareMutations int
	shareWaiters   []chan struct{}
	// appActions counts /apps start, restart and remove-with-appdata calls
	// admitted before maintenance mode, for the same reason: array stop
	// waits for them so a container started, or appdata deleted, by a
	// call that passed the array check cannot land after the containers
	// were listed and stopped or after the cache was unmounted.
	appActions int
	appWaiters []chan struct{}
	// settleHook, when non-nil, is called synchronously by runJob once a
	// job's rj.finished has just been set true — after its RunFunc has
	// returned, but before the job is removed from s.running — so a test
	// can deterministically land a Cancel call inside that exact window
	// instead of racing the real clock (#364). Never set outside a test.
	settleHook func(jobID string)
	// resumableDecisionHook, when non-nil, is called synchronously by the
	// scheduler-owned half of RunContext.KeepForResume — after a RunFunc
	// (RunEvacuation's own d.run) has decided every other condition for
	// ending resumable already holds, but before KeepForResume takes
	// rj.mu to commit that decision against a racing Cancel — so a test
	// can land a Cancel call deterministically inside that exact window
	// (#378) instead of racing the real clock. It is never called when a
	// RunFunc's own preliminary check already decided against resumable;
	// KeepForResume is never reached at all in that case. Never set
	// outside a test.
	resumableDecisionHook func(jobID string)
	// beforeFinishedHook, when non-nil, is called synchronously by runJob
	// right after a job's RunFunc has returned but before runJob takes
	// rj.mu to set rj.finished and read rj.reason — the window #379
	// reports: a Cancel landing here is still accepted (rj.finished is
	// not yet set, so Scheduler.Cancel's own finished check does not
	// refuse it), even though the RunFunc has already committed to
	// whatever runErr it returned. A test can land a Cancel call
	// deterministically inside that exact window instead of racing the
	// real clock. Never set outside a test.
	beforeFinishedHook func(jobID string)
	// topologyBackup, when set, is run once a job whose class is
	// ClassTopology actually starts (doc 10 §1, #406, #408) — in Submit
	// when it starts immediately, or in dispatch() when a queued one's
	// turn comes: a disk add, remove, replace, data or parity upgrade,
	// format, or pool remount is never entered without a config snapshot
	// from just before it, including one that waited behind another
	// topology job in the queue first. Set through SetTopologyBackup,
	// never directly — every other Scheduler hook that isn't a test-only
	// field is reached the same way.
	topologyBackup ConfigBackup
	// dispatching holds, for the window between dispatch() removing a
	// queued ClassTopology job from s.queue and its outcome being decided
	// — its start-time backup, and, if it was cancelled or that backup
	// failed and its type registered an AbortFunc, that abort too (#408,
	// doc 02 §4 invariant 3) — the queuedJob dispatch() is running that
	// work for. It is outside s.mu the whole time, so it is in neither
	// s.queue nor s.running, and Cancel needs its own way to recognize it
	// instead of mis-reporting why it refuses.
	dispatching map[string]*queuedJob
	// beforeSealHook, when non-nil, is called by Resume outside s.mu right
	// before it repairs the job's log, so a test can hold that step open
	// deterministically. Never set outside a test.
	beforeSealHook func(jobID string)
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
		resuming: make(map[string]bool),
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

// SetTopologyBackup wires the pre-topology config backup doc 10 §1
// promises (#406, #408): from this point on, a job whose class is
// ClassTopology (disk add/remove/replace/upgrade, format, or pool
// remount) runs it the moment it actually starts — in Submit for one
// starting immediately, in dispatch() for one that waited its turn in the
// queue first. cmd/hoservad's main.go calls this once, right after
// building the daemon's backup.Service. Unset (the zero value, nil), no
// such backup ever runs — the same "not configured, skip" behaviour
// update.Engine.backup uses when its own Backup field is nil.
func (s *Scheduler) SetTopologyBackup(b ConfigBackup) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.topologyBackup = b
}

// takesTopologyBackup reports whether a job of type t, in class, starts with the
// pre-topology config backup. A migration scan is in the Topology class so no
// storage job runs beside it, but it only reads: it changes no topology, and its
// repeated runs must not use the bounded retention the real changes' backups
// rely on.
func takesTopologyBackup(t Type, class Class) bool {
	return class == ClassTopology && t != TypeMigrationScan
}

// runTopologyBackup runs the pre-topology config backup (doc 10 §1, #406,
// #408), if one is wired, at the moment a ClassTopology job actually
// starts — called by Submit for one starting immediately, and by
// dispatch() for a queued one whose turn has come — always outside s.mu,
// so a slow backup never holds the lock every other Submit and Cancel
// needs.
func (s *Scheduler) runTopologyBackup(ctx context.Context) error {
	s.mu.Lock()
	backup := s.topologyBackup
	s.mu.Unlock()
	if backup == nil {
		return nil
	}
	if err := backup.Run(ctx); err != nil {
		return fmt.Errorf("job: pre-topology config backup: %w", err)
	}
	return nil
}

// admitLocked runs every admission check Submit performs before creating
// a job of type t — the database-restore hold, the data-disk-upgrade or
// maintenance-mode check, the on-battery hold, and evacuation admission.
// It reports no conflict-queueing decision at all: doc 01 §4's
// class-conflict check is Submit's own hasConflictWithRunningLocked call,
// kept separate so it can be evaluated on its own, before these checks,
// to decide whether a ClassTopology job's start-time backup is needed
// (#408). Callers must hold s.mu.
func (s *Scheduler) admitLocked(ctx context.Context, t Type) error {
	if s.databaseRestore {
		return ErrDatabaseRestoreInProgress
	}
	if t == TypeDiskUpgradeData {
		if err := s.admitDiskUpgradeDataLocked(ctx); err != nil {
			return err
		}
	} else if s.maintenance {
		return ErrMaintenanceMode
	}
	if s.batteryHold && isBatteryHeldType(t) {
		return ErrOnBattery
	}
	if t == TypeEvacuation {
		if err := s.admitEvacuationLocked(ctx); err != nil {
			return err
		}
	}
	return nil
}

// finishSubmitLocked builds, persists and queues-or-starts a new job of
// type t/class/resourceIDs/params, queued rather than started immediately
// exactly when it conflicts with a job already running (doc 01 §4).
// backupRan reports whether Submit already ran this ClassTopology job's
// start-time backup before taking s.mu here — decided by a check that
// released s.mu in between (#408), so this function's own conflict check,
// taken under one unbroken lock hold with the job's creation, is the only
// one authoritative for whether it may actually start immediately. If
// Submit's own earlier check found a conflict and skipped the backup on
// that basis, but whatever it conflicted with has since finished, this
// function queues the job anyway instead of starting it with no backup at
// all — exactly as if it still conflicted — and reports that back to
// Submit (its second return value) so Submit can call dispatch() itself
// right away rather than leaving the job stuck until some unrelated job's
// completion happens to trigger one. Callers must hold s.mu.
func (s *Scheduler) finishSubmitLocked(ctx context.Context, t Type, class Class, resourceIDs []string, params []byte, entry registryEntry, backupRan bool) (*Job, bool, error) {
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

	deferredForBackup := false
	switch {
	case s.hasConflictWithRunningLocked(class, resourceIDs):
		j.Status = StatusQueued
	case takesTopologyBackup(t, class) && !backupRan:
		j.Status = StatusQueued
		deferredForBackup = true
	default:
		j.Status = StatusRunning
		j.StartedAt = &now
	}

	if err := s.store.Create(ctx, j); err != nil {
		return nil, false, fmt.Errorf("job: persisting new job: %w", err)
	}

	if j.Status == StatusQueued {
		s.queue = append(s.queue, &queuedJob{job: j, run: entry.run, cancellable: entry.cancellable})
	} else {
		s.startJobLocked(j, entry.run, entry.cancellable)
	}
	return j, deferredForBackup, nil
}

// Submit persists a new job of type t and starts it immediately unless its
// class conflicts with a job already running (doc 01 §4), in which case it
// is queued until dispatch() finds it a slot. t must have a RunFunc bound
// through Registry.Register — nothing here knows how to run any job type
// itself. params is the JSON request payload for types that have one
// (validated here against t); nil or empty for types that have none.
//
// A ClassTopology job that admitLocked admits and that does not conflict
// with anything currently running is about to start immediately — so its
// start-time backup (doc 10 §1, #406, #408) runs here, before anything is
// persisted. A submission admission refuses gets no backup: each archive
// takes one of the pre-change retention slots (internal/backup), and a
// refused change must not push out a real pre-import or pre-update one.
// admitLocked runs again after the backup, in the lock hold that creates
// the job, since state can change while the backup runs. A
// ClassTopology job that does conflict gets no backup here at all — one
// taken now would describe the configuration from before whatever is
// currently running finishes, not from just before this job's own change
// — dispatch() gives it one instead, at the moment it actually starts
// (#408). The conflict is re-checked once more, in one unbroken lock hold
// with the job's own creation, by finishSubmitLocked — it was released
// for the backup call itself, so that second check, never this one, is
// authoritative for whether the job may actually start. If it now finds
// no conflict despite this check having found one moments earlier (#408:
// whatever it conflicted with finished in between), finishSubmitLocked
// queues the job rather than starting it with no backup at all, and
// reports that back here so dispatch() — called directly below, before
// this function returns — gives it a start-time backup right away rather
// than leaving it stuck behind whatever else happens to finish next.
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

	backupRan := false
	if takesTopologyBackup(t, class) {
		s.mu.Lock()
		if err := s.admitLocked(ctx, t); err != nil {
			s.mu.Unlock()
			return nil, err
		}
		willQueue := s.hasConflictWithRunningLocked(class, resourceIDs)
		s.mu.Unlock()
		if !willQueue {
			if err := s.runTopologyBackup(ctx); err != nil {
				return nil, err
			}
			backupRan = true
		}
	}

	s.mu.Lock()
	if err := s.admitLocked(ctx, t); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	j, deferredForBackup, err := s.finishSubmitLocked(ctx, t, class, resourceIDs, params, entry, backupRan)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}

	s.hub.Publish(j)
	if deferredForBackup {
		s.dispatch()
	}
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

// admitEvacuationLocked refuses a new evacuation while another one is
// pending — queued, running or interrupted (#359). Only one disk is in
// removal at a time, and an evacuation's removal state belongs to the
// job that set it: a second evacuation of the same disk alongside an
// interrupted one would hold the state the first one's cancel releases.
// Callers must hold s.mu, so the check and the job's creation are one
// step.
func (s *Scheduler) admitEvacuationLocked(ctx context.Context) error {
	pending, err := s.store.ListPending(ctx, TypeEvacuation)
	if err != nil {
		return fmt.Errorf("job: checking for a pending evacuation: %w", err)
	}
	if len(pending) > 0 {
		return fmt.Errorf("%w: job %s", ErrEvacuationPending, pending[0].ID)
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
//
// A job still appears in s.running for a short window after its RunFunc
// has already returned — runJob only deletes it once its own outcome is
// fully decided (#364). A Cancel landing in that window finds rj.finished
// already set and is refused with ErrJobNotRunning rather than mutating
// reason: the run's real outcome is already fixed by then, so silently
// relabelling it cancelled would both misreport what actually happened
// and skip the AbortFunc a genuinely queued-or-interrupted cancel would
// have run, orphaning whatever job-owned state that outcome legitimately
// kept (e.g. #274/#359's evacuation manifest and no-create exemption for
// a resumable stop).
//
// While BeginDatabaseRestore's hold is held, Cancel is refused outright
// with ErrDatabaseRestoreInProgress, before any of the branches below run
// (#402): abortAndCancel's own writes — clearing an evacuation's owned
// manifest, releasing its removal state, recording the job's final status
// — must never race backup.RestoreDatabase overwriting the same tables.
// BeginDatabaseRestore itself refuses to start while an abort is already
// running, so once the hold is granted no abort can be in flight for it
// to race in the first place.
//
// A queued ClassTopology job whose turn dispatch() has just taken is in
// neither s.queue nor s.running while its start-time backup (#408) runs
// outside s.mu — dispatch() has already removed it from the former and its
// own RunFunc has not started to put it in the latter. Cancel cannot
// interrupt that backup, and its outcome is not decided yet either, so
// Cancel only records the request (queuedJob.cancelRequested) and returns
// the job's current snapshot without error, the same way cancelling an
// ordinary queued job that has no AbortFunc does. startQueuedTopologyJob
// checks the request once the backup returns: if the job's type registered
// an AbortFunc (TypeDiskUpgradeData's own Unwind), that runs next, and the
// job ends cancelled only once it succeeds — interrupted, with the abort's
// own error, if it does not (doc 02 §4 invariant 3) — the same outcome a
// plainly queued job's own failed-abort Cancel already records below. A
// type with no AbortFunc ends cancelled directly, whatever the backup
// itself decided. Either way RunFunc is never called.
func (s *Scheduler) Cancel(ctx context.Context, id string) (*Job, error) {
	s.mu.Lock()
	if s.databaseRestore {
		s.mu.Unlock()
		return nil, ErrDatabaseRestoreInProgress
	}
	if q, ok := s.dispatching[id]; ok {
		if !q.cancellable && q.job.Type == TypeDiskUpgradeData {
			s.mu.Unlock()
			return nil, ErrDiskUpgradePastRelease
		}
		q.cancelRequested = true
		snapshot := *q.job
		s.mu.Unlock()
		return &snapshot, nil
	}
	if rj, ok := s.running[id]; ok {
		rj.mu.Lock()
		if rj.finished {
			rj.mu.Unlock()
			s.mu.Unlock()
			return nil, fmt.Errorf("%w: job %s has already finished running", ErrJobNotRunning, id)
		}
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
	if s.resuming[id] {
		s.mu.Unlock()
		return nil, ErrJobResumeInProgress
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
		if err := abort(ctx, j.ID, j.Params); err != nil {
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
// persisted. The job is read and started under s.mu, and while its log is
// repaired outside s.mu it is held in s.resuming, so a concurrent Cancel's
// abort of the same job and this never both run (doc 02 §4 UR7). A
// data-disk upgrade resumed at releasing is not cancellable, decided and
// persisted before the job is visible to Cancel.
//
// Once the stored row itself says resumable and interrupted, it also
// refuses with ErrJobAlreadyRunning if id's runner is still alive in
// s.running (#402): a whole-database restore (ImportConfig) can
// overwrite or mislabel that row back to interrupted while the runner it
// actually describes is still going, and the class/resource-scope
// conflict check below is blind to that on its own, since a restored
// row's resource scope need not still overlap what is actually running
// under the same id.
func (s *Scheduler) Resume(ctx context.Context, id string) (*Job, error) {
	s.mu.Lock()
	if s.aborting[id] {
		s.mu.Unlock()
		return nil, ErrJobAbortInProgress
	}
	if s.resuming[id] {
		s.mu.Unlock()
		return nil, ErrJobResumeInProgress
	}
	existing, entry, err := s.resumableJobLocked(ctx, id)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}

	// An interrupted job is terminal, so getJobLog does not follow it; once
	// the job is queued or running it does. A run that never closed its log
	// (the daemon stopped mid-job) is repaired here, before that, because
	// the repair swaps the file a follower would already have open. The
	// repair decodes the whole log and may rewrite it, so it runs outside
	// s.mu with id in s.resuming (which refuses a second Resume, a Cancel
	// and a database restore), and every precondition is checked again
	// once s.mu is back. This is the only path from interrupted back to
	// queued or running.
	if s.logs != nil {
		s.resuming[id] = true
		s.mu.Unlock()
		s.sealLogForResume(id)
		s.mu.Lock()
		delete(s.resuming, id)
		existing, entry, err = s.resumableJobLocked(ctx, id)
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
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

// resumableJobLocked reads id's row and decides whether Resume may start
// it now: every refusal Resume owns except the per-job in-flight markers.
// The caller holds s.mu.
func (s *Scheduler) resumableJobLocked(ctx context.Context, id string) (*Job, registryEntry, error) {
	if s.databaseRestore {
		return nil, registryEntry{}, ErrDatabaseRestoreInProgress
	}
	existing, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, registryEntry{}, err
	}
	if !existing.Resumable {
		return nil, registryEntry{}, ErrJobNotResumable
	}
	if existing.Status != StatusInterrupted {
		return nil, registryEntry{}, ErrJobNotInterrupted
	}
	if _, ok := s.running[id]; ok {
		return nil, registryEntry{}, fmt.Errorf("%w: job %s", ErrJobAlreadyRunning, id)
	}
	entry, ok := s.registry.lookup(existing.Type)
	if !ok {
		return nil, registryEntry{}, fmt.Errorf("%w: %s", ErrJobTypeNotRegistered, existing.Type)
	}
	if existing.Type == TypeDiskUpgradeData {
		if !s.maintenance {
			return nil, registryEntry{}, ErrArrayNotStopped
		}
	} else if s.maintenance {
		return nil, registryEntry{}, ErrMaintenanceMode
	}
	if s.batteryHold && isBatteryHeldType(existing.Type) {
		return nil, registryEntry{}, ErrOnBattery
	}
	return existing, entry, nil
}

// sealLogForResume repairs id's log for Resume, outside s.mu. A repair that
// fails is logged and Resume continues, as before. Should Seal panic, the
// caller never gets to clear s.resuming[id], so it is cleared here.
func (s *Scheduler) sealLogForResume(id string) {
	sealed := false
	defer func() {
		if !sealed {
			s.mu.Lock()
			delete(s.resuming, id)
			s.mu.Unlock()
		}
	}()
	if s.beforeSealHook != nil {
		s.beforeSealHook(id)
	}
	if err := s.logs.Seal(id); err != nil {
		log.Printf("job: repairing the log of job %s before it resumes: %v", id, err)
	}
	sealed = true
}

// persistMaintenanceLocked writes maintenance/arrayStopped to the
// array_maintenance singleton row (D16, #387, doc 02 §4, Q70), so `array
// stop` survives a hoservad restart or reboot — held only in this
// struct's own fields before this, a crash or restart while the array
// was stopped silently returned to normal operation. Both fields are
// always written together, so a restart can never restore one without
// the other. Callers must hold s.mu.
func (s *Scheduler) persistMaintenanceLocked(ctx context.Context, maintenance, arrayStopped bool) error {
	if s.store == nil {
		// A Scheduler built without a Store (NewScheduler(nil, ...), this
		// package's own tests that exercise nothing but in-memory state)
		// has nothing to persist to and nothing a restart could restore
		// either — every production caller (cmd/hoservad's main.go) always
		// passes a real one.
		return nil
	}
	return s.store.q.UpsertArrayMaintenance(ctx, storedb.UpsertArrayMaintenanceParams{
		Maintenance:  boolToSQL(maintenance),
		ArrayStopped: boolToSQL(arrayStopped),
		UpdatedAt:    time.Now().UTC().Format(store.TimeFormat),
	})
}

// RestorePersistedMaintenance reads the maintenance state `array stop`
// last persisted and restores it into this scheduler (#387, doc 02 §4,
// Q70). cmd/hoservad calls this once, immediately after
// RecoverFromRestart and before building the array's stop/start sequence
// or evaluating the storage-target gate: a crash or restart while the
// array was stopped must never admit a job, remount the pool, or start
// Samba/NFS/Docker/libvirt, because nothing survived in memory to say
// otherwise. No persisted row — a fresh install, or one that has never
// run `array stop` — restores to normal operation, and so does a
// Scheduler built without a Store at all (NewScheduler(nil, ...), this
// package's own tests that exercise nothing but in-memory state): there
// is nothing for it to have restored from.
func (s *Scheduler) RestorePersistedMaintenance(ctx context.Context) error {
	if s.store == nil {
		return nil
	}
	row, err := s.store.q.GetArrayMaintenance(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("job: reading persisted maintenance state: %w", err)
	}
	s.mu.Lock()
	s.maintenance = row.Maintenance != 0
	s.arrayStopped = row.ArrayStopped != 0
	s.mu.Unlock()
	return nil
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
//
// The new state is persisted (#387) before anything else changes: a
// failure to persist refuses the whole call, leaving every job and every
// service exactly as it was, rather than entering maintenance mode in
// memory only and repeating the bug this exists to fix.
func (s *Scheduler) EnterMaintenance(ctx context.Context) error {
	return s.enterMaintenance(ctx, true)
}

// EnterMaintenanceTransient is EnterMaintenance's own shutdown-sequence
// half (#387): a reboot or a UPS low-battery shutdown needs the
// exact same in-memory admission refusal and job-stopping ArraySequence
// .Stop already gets from EnterMaintenance, but neither is a user asking
// the array to stay stopped after the box comes back — persisting a new
// "stopped" row here would leave RestorePersistedMaintenance holding the
// array offline after the next ordinary boot (a plain Reboot, or a UPS
// shutdown, would otherwise silently turn into a stuck maintenance mode).
// It never writes to array_maintenance, so it
// never fails and never disturbs a persisted user `array stop` already in
// force either — that row is simply left exactly as it is.
func (s *Scheduler) EnterMaintenanceTransient() {
	// persist=false never touches the store, so this can never return an
	// error — see enterMaintenance below.
	_ = s.enterMaintenance(context.Background(), false)
}

func (s *Scheduler) enterMaintenance(ctx context.Context, persist bool) error {
	s.mu.Lock()
	if persist {
		if err := s.persistMaintenanceLocked(ctx, true, false); err != nil {
			s.mu.Unlock()
			return fmt.Errorf("job: persisting maintenance mode: %w", err)
		}
	}
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

// ExitMaintenance reverses EnterMaintenance (`hoserva array start`, Q70)
// without reporting a failed persistence write to its caller. It exists
// only for this package's own tests (and internal/api's), which call it
// as a bare statement and never exercise that failure path; a failed
// write is only logged. ArraySequence.Start, the only production caller,
// uses ExitMaintenanceChecked instead (#387) — kept separate
// rather than changing this signature, since every other call site would
// need a matching fix outside this issue's own declared scope. Never
// called by Scheduler.Cancel or Resume — neither touches maintenance
// mode.
func (s *Scheduler) ExitMaintenance() {
	if err := s.ExitMaintenanceChecked(); err != nil {
		log.Printf("job: %v — a caller ignoring ExitMaintenance's own returned error leaves maintenance mode entered", err)
	}
}

// ExitMaintenanceChecked is ExitMaintenance with its persisted-write
// failure reported to the caller (#387): the cleared state is
// persisted before it takes effect in memory, so ArraySequence.Start
// returns this error and `array start` itself fails and the user
// retries, rather than reporting success while SQLite still holds the
// previous, stopped state — a restart before that is fixed would put the
// daemon straight back into maintenance mode (RestorePersistedMaintenance)
// over an array the user was just told had started, with GetStatus
// reporting it stopped while it is actually live and serving clients.
func (s *Scheduler) ExitMaintenanceChecked() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.persistMaintenanceLocked(context.Background(), false, false); err != nil {
		return fmt.Errorf("job: persisting the exited-maintenance state: %w", err)
	}
	s.maintenance = false
	s.arrayStopped = false
	return nil
}

// MarkArrayStopped records that ArraySequence.Stop completed every step
// (doc 02 §4 UR3): the only state that admits a data-disk upgrade.
func (s *Scheduler) MarkArrayStopped() {
	s.markArrayStopped(true)
}

// MarkArrayStoppedTransient is MarkArrayStopped's own shutdown-sequence
// half (#387, ArraySequence.StopForShutdown): completes the
// in-memory "stop sequence completed" bookkeeping a reboot or UPS
// shutdown's own stop sequence needs, without writing a new persisted
// stop the user never asked for.
func (s *Scheduler) MarkArrayStoppedTransient() {
	s.markArrayStopped(false)
}

func (s *Scheduler) markArrayStopped(persist bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stopped := s.maintenance
	if persist {
		// A failure here is only logged, not returned — by the time this
		// runs every unmount has already succeeded, and the process's own
		// in-memory state (which already governs Submit's admission check)
		// is authoritative for as long as this process keeps running. Only
		// a restart before this write is retried would restore the stale,
		// unstopped state — refusing a data-disk upgrade until `array
		// stop` runs again, never admitting one a restart could not
		// confirm actually happened.
		if err := s.persistMaintenanceLocked(context.Background(), s.maintenance, stopped); err != nil {
			log.Printf("job: persisting the array-stopped state: %v — a restart before this is retried would refuse a data-disk upgrade until `array stop` runs again, never admit one unsafely", err)
		}
	}
	s.arrayStopped = stopped
}

// BeginArrayStart is ArraySequence.Start's first step: it refuses while a
// data-disk upgrade is pending (doc 02 §4 E6), and otherwise clears the
// "stop sequence completed" state under the same lock Submit admits a
// data-disk upgrade under, so no upgrade is admitted once a start begins.
// It returns that state as it was, for RestoreArrayStopped. The cleared
// state is persisted (#387) before it takes effect: a restart mid-start
// must never restore a "stop sequence completed" state that a since-begun
// start has already invalidated.
func (s *Scheduler) BeginArrayStart(ctx context.Context) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending, err := s.store.ListPending(ctx, TypeDiskUpgradeData)
	if err != nil {
		return false, fmt.Errorf("job: checking for a pending data-disk upgrade: %w", err)
	}
	if len(pending) > 0 {
		return false, fmt.Errorf("%w: job %s — resume it, or cancel it to abort back to the old disk", ErrDiskUpgradeDataPending, pending[0].ID)
	}
	was := s.arrayStopped
	if err := s.persistMaintenanceLocked(ctx, s.maintenance, false); err != nil {
		return false, fmt.Errorf("job: persisting array-start state: %w", err)
	}
	s.arrayStopped = false
	return was, nil
}

// RestoreArrayStopped puts back the state BeginArrayStart returned, for a
// start that failed without leaving anything mounted. The restored state
// is persisted (#387) before it takes effect in memory: a failure here
// leaves the persisted state at "not stopped", which only ever refuses a
// data-disk upgrade a restart could not confirm is actually safe — never
// the reverse.
func (s *Scheduler) RestoreArrayStopped(was bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	restored := was && s.maintenance
	if err := s.persistMaintenanceLocked(context.Background(), s.maintenance, restored); err != nil {
		return fmt.Errorf("job: persisting the restored array-stopped state: %w", err)
	}
	s.arrayStopped = restored
	return nil
}

// InMaintenance reports whether maintenance mode is currently active.
func (s *Scheduler) InMaintenance() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maintenance
}

// BeginDatabaseRestore admits a whole-database restore (ImportConfig,
// doc 10 §1, #402): it refuses with ErrDatabaseRestoreInProgress if
// another restore already holds it, and with ErrJobsActiveForRestore if
// any job is currently queued or running, or if any job's Cancel abort is
// still running (s.aborting non-empty) or any Resume is still repairing
// its job's log (s.resuming non-empty) — checked under s.mu, so a job
// whose Submit already completed (its row persisted) cannot be missed,
// and no further Submit, Resume or Cancel can land once this returns,
// since each takes the same lock and checks the same flag. Refusing while
// an abort is running closes the race abortAndCancel would otherwise have
// against RestoreDatabase: an abort already past this check keeps running
// to completion, exactly as a Cancel racing a maintenance-mode entry
// already does elsewhere in this package (#402). On success, every
// Submit, Resume and Cancel is refused with ErrDatabaseRestoreInProgress
// until the returned release func is called; the caller must call it
// exactly once, on every path — including error and panic (defer).
func (s *Scheduler) BeginDatabaseRestore(ctx context.Context) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.databaseRestore {
		return nil, ErrDatabaseRestoreInProgress
	}
	if len(s.aborting) > 0 || len(s.resuming) > 0 {
		return nil, ErrJobsActiveForRestore
	}
	active, err := s.store.ListActive(ctx)
	if err != nil {
		return nil, fmt.Errorf("job: checking for active jobs before a database restore: %w", err)
	}
	if len(active) > 0 {
		return nil, ErrJobsActiveForRestore
	}
	s.databaseRestore = true
	return s.endDatabaseRestore, nil
}

// WithArrayStateHeld runs fn with the maintenance and array-stopped state
// unable to change: every transition takes s.mu, which is held until fn
// returns. ImportConfig reads the live array_maintenance row and restores the
// database inside it, so a concurrent `array stop` cannot land between the
// two and be overwritten by the archive's state. fn must not call back into
// the Scheduler.
func (s *Scheduler) WithArrayStateHeld(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn()
}

func (s *Scheduler) endDatabaseRestore() {
	s.mu.Lock()
	s.databaseRestore = false
	s.mu.Unlock()
}

// BeginShareMutation admits one share create, update, or delete. It
// fails once maintenance mode is active, under the same lock that
// EnterMaintenance sets that flag, so a mutation cannot start after
// array stop has decided to unmount. FinishShareMutation must be called
// when the mutation returns, including on error.
func (s *Scheduler) BeginShareMutation() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.maintenance {
		return ErrMaintenanceMode
	}
	s.shareMutations++
	return nil
}

// FinishShareMutation records that a mutation admitted by
// BeginShareMutation has finished.
func (s *Scheduler) FinishShareMutation() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shareMutations == 0 {
		return
	}
	s.shareMutations--
	if s.shareMutations == 0 {
		for _, ch := range s.shareWaiters {
			close(ch)
		}
		s.shareWaiters = nil
	}
}

// DrainShareMutations blocks until every mutation admitted before
// maintenance mode has finished, or until ctx is done. ArraySequence.Stop
// calls it after EnterMaintenance and before unmounting.
func (s *Scheduler) DrainShareMutations(ctx context.Context) error {
	s.mu.Lock()
	if s.shareMutations == 0 {
		s.mu.Unlock()
		return nil
	}
	ch := make(chan struct{})
	s.shareWaiters = append(s.shareWaiters, ch)
	s.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("job: waiting for share updates to finish: %w", ctx.Err())
	}
}

// BeginAppAction admits one container start, restart, or remove that
// deletes appdata. It fails once maintenance mode is active, under the
// same lock that EnterMaintenance sets that flag, so an action cannot
// begin after array stop has decided to list and stop the containers.
// FinishAppAction must be called when the action returns, including on
// error.
func (s *Scheduler) BeginAppAction() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.maintenance {
		return ErrMaintenanceMode
	}
	s.appActions++
	return nil
}

// FinishAppAction records that an action admitted by BeginAppAction has
// finished.
func (s *Scheduler) FinishAppAction() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.appActions == 0 {
		return
	}
	s.appActions--
	if s.appActions == 0 {
		for _, ch := range s.appWaiters {
			close(ch)
		}
		s.appWaiters = nil
	}
}

// DrainAppActions blocks until every action admitted before maintenance
// mode has finished, or until ctx is done. ArraySequence.Stop calls it
// after EnterMaintenance and before any service stops.
func (s *Scheduler) DrainAppActions(ctx context.Context) error {
	s.mu.Lock()
	if s.appActions == 0 {
		s.mu.Unlock()
		return nil
	}
	ch := make(chan struct{})
	s.appWaiters = append(s.appWaiters, ch)
	s.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("job: waiting for app start, restart and remove calls to finish: %w", ctx.Err())
	}
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
// Topology job, if any — including a queued Topology job whose start-time
// backup dispatch() is already running outside s.mu (s.dispatching, #408):
// it is committed to starting and is every bit as blocking as one already
// in s.running. Self-update and rollback refuse while one is running and
// name it (Q67); reboot waits for it instead (Q68).
func (s *Scheduler) BlockingStorageJob() *Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rj := range s.running {
		if IsStorageClass(rj.job.Class) {
			cp := *rj.job
			return &cp
		}
	}
	for _, q := range s.dispatching {
		if IsStorageClass(q.job.Class) {
			cp := *q.job
			return &cp
		}
	}
	return nil
}

// WaitForStorageJobs blocks until no Parity, Array-write or Topology job
// is running or dispatching (s.dispatching, #408), or ctx is done. Reboot
// uses this before the Q70 sequence (Q68) — it waits rather than refusing.
// A dispatching job has no done channel of its own to wait on — its
// start-time backup runs outside s.mu, and dispatch() only tracks it in a
// plain map — so this polls at the same interval Await falls back to
// rather than blocking on a channel for that case.
func (s *Scheduler) WaitForStorageJobs(ctx context.Context) error {
	for {
		s.mu.Lock()
		var done <-chan struct{}
		dispatching := false
		for _, rj := range s.running {
			if IsStorageClass(rj.job.Class) {
				done = rj.done
				break
			}
		}
		if done == nil {
			for _, q := range s.dispatching {
				if IsStorageClass(q.job.Class) {
					dispatching = true
					break
				}
			}
		}
		s.mu.Unlock()
		if done == nil && !dispatching {
			return nil
		}
		if done == nil {
			select {
			case <-time.After(awaitPollInterval):
			case <-ctx.Done():
				return fmt.Errorf("job: waiting for storage jobs: %w", ctx.Err())
			}
			continue
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
// conflicts with any currently running job (doc 01 §4), or with a queued
// ClassTopology job dispatch() has already committed to starting but whose
// start-time backup is still running outside s.mu (s.dispatching, #408):
// that job is no longer in s.queue and has not yet been added to
// s.running, but it is not idle either — mutual exclusion must hold for it
// exactly as if it were already running, or a second Submit, Resume, or
// dispatch() pass landing in that window could start something doc 01 §4
// says must never run alongside it. Callers must hold s.mu.
func (s *Scheduler) hasConflictWithRunningLocked(class Class, resourceIDs []string) bool {
	for _, rj := range s.running {
		if conflicts(class, rj.job.Class, resourceIDs, rj.job.ResourceIDs) {
			return true
		}
	}
	for _, q := range s.dispatching {
		if conflicts(class, q.job.Class, resourceIDs, q.job.ResourceIDs) {
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
//
// A queued ClassTopology job's turn is decided under s.mu exactly like any
// other job's, but starting it is not: its start-time pre-topology backup
// (doc 10 §1, #406, #408), and, if it is cancelled or that backup fails,
// its type's own AbortFunc (doc 02 §4 invariant 3), all have to run first,
// and never under this lock — neither a slow backup nor a slow abort may
// ever hold up an unrelated Submit or Cancel. This function only decides,
// under s.mu, that such a job is otherwise eligible to start, removes it
// from s.queue, and records it in s.dispatching so Cancel recognizes it;
// startQueuedTopologyJob (called below, after this function has released
// s.mu) is what actually runs that work and starts the job, or records its
// outcome. doc 01 §4's own conflict rule (Topology excludes every
// storage-class job globally) means at most one queued topology job is
// ever deferred to it in a single dispatch() pass.
func (s *Scheduler) dispatch() {
	s.mu.Lock()

	var remaining []*queuedJob
	decided := make([]*Job, 0, len(s.queue))
	var toStart []*queuedJob
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
			for _, sj := range decided {
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

		if takesTopologyBackup(q.job.Type, q.job.Class) {
			if s.dispatching == nil {
				s.dispatching = make(map[string]*queuedJob)
			}
			s.dispatching[q.job.ID] = q
			toStart = append(toStart, q)
			decided = append(decided, q.job)
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
		decided = append(decided, q.job)
		s.hub.Publish(q.job)
	}
	s.queue = remaining
	s.mu.Unlock()

	for _, q := range toStart {
		s.startQueuedTopologyJob(q)
	}
}

// recordTerminalOutcomeWithRetry persists j's already-decided terminal
// status — set on j by the caller before calling this — the same way
// runJob's own final-status write does: up to finalStatusWriteRetries
// attempts, and a terminal snapshot remembered (rememberTerminalSnapshot)
// if every attempt still fails, so Await can still resolve j correctly
// from memory even though the store row is left non-terminal (#408: a
// queued topology job failed or interrupted by startQueuedTopologyJob has
// no RunFunc goroutine of its own behind it to retry this write the way
// runJob's did already). Called without s.mu held.
func (s *Scheduler) recordTerminalOutcomeWithRetry(j Job) {
	storeErr := s.store.UpdateStatus(context.Background(), j.ID, j.Status, j.Progress, j.ErrorCode, j.ErrorMessage, j.StartedAt, j.FinishedAt)
	for attempt := 1; storeErr != nil && attempt < finalStatusWriteRetries; attempt++ {
		time.Sleep(finalStatusWriteRetryDelay)
		storeErr = s.store.UpdateStatus(context.Background(), j.ID, j.Status, j.Progress, j.ErrorCode, j.ErrorMessage, j.StartedAt, j.FinishedAt)
	}
	if storeErr != nil {
		log.Printf("job: recording job %s as %s: %v", j.ID, j.Status, storeErr)
		s.rememberTerminalSnapshot(j)
	}
}

// startQueuedTopologyJob runs a queued ClassTopology job's start-time
// pre-topology backup (doc 10 §1, #406, #408), always called without s.mu
// held — dispatch() has already removed q from s.queue and recorded it in
// s.dispatching before calling this, and it stays there — still excluding
// every other storage-class job exactly as if it were running
// (hasConflictWithRunningLocked, BlockingStorageJob, WaitForStorageJobs) —
// until this function records its outcome, below.
//
// If Cancel recorded a request for q while the backup ran
// (queuedJob.cancelRequested, #408), or the backup itself failed, and q's
// type registered an AbortFunc (TypeDiskUpgradeData's own Unwind), that
// abort runs next — also outside s.mu, while s.aborting[q.job.ID] is held
// the same way a plainly queued or interrupted job's own Cancel already
// holds it around abortAndCancel. doc 02 §4 invariant 3 means cancelled
// and failed both promise the type's own abort already succeeded, so a
// type with one never reaches either status without it having run and
// succeeded first: a failed abort leaves q interrupted instead, with the
// abort's own error recorded — the same outcome abortAndCancel's own
// failed-abort branch already records for a plainly queued job — so the
// upgrade stays pending, reachable by the user's next Cancel or Resume,
// rather than being reported settled over a checkpoint nothing actually
// unwound. A type with no AbortFunc skips straight to the outcome its
// trigger already decided, exactly as before this fix.
//
// A cancel request takes precedence over a failed backup when both are
// true. Cancel could not interrupt the backup itself, but it never has to
// start what it was asked to cancel. dispatch() runs again immediately
// after every terminal outcome below (cancelled, failed or interrupted),
// since a job dispatch() skipped only because it conflicted with q while
// q's own turn was still being decided deserves its own chance now rather
// than waiting for some unrelated job to finish and trigger it.
//
// A data-disk upgrade resumed at releasing never reaches the cancel or
// AbortFunc branches above at all: Cancel already refuses it there with
// ErrDiskUpgradePastRelease (q.cancellable is false), and doc 02 §4
// invariant 4 makes succeeded the only outcome past that decision — there
// is nothing for Unwind to undo. So a failed backup for one of these skips
// the AbortFunc entirely and ends the job interrupted, at its unchanged
// checkpoint, with the backup's own error: still resumable, never failed,
// since failed would claim the release already committed over a
// configuration this backup never actually captured.
//
// If the backup succeeds and no cancel was requested, q starts exactly as
// dispatch() itself already starts any other queued job — unless
// EnterMaintenance ran while the backup was in flight: it already
// interrupted every job still in s.queue at that moment, but q had
// already been removed from it, so q gets the same outcome now instead of
// starting a Topology job during maintenance mode (the one class it never
// admits except a data-disk upgrade, which never reaches this branch
// through a cancel request or a failed backup — TypeDiskUpgradeData is the
// only ClassTopology type with an AbortFunc, and dispatch() only ever
// defers one to this path while maintenance mode is already active) or
// being left stuck queued with nothing left to dispatch it. This branch
// does not call dispatch() itself: EnterMaintenance already emptied
// s.queue of everything it could reach.
func (s *Scheduler) startQueuedTopologyJob(q *queuedJob) {
	backupErr := s.runTopologyBackup(context.Background())
	abort, hasAbort := s.registry.lookupAbort(q.job.Type)

	// The cancel check and the start below are one lock hold: a Cancel
	// landing between them would return success for a job that then starts.
	s.mu.Lock()
	if q.cancelRequested {
		s.mu.Unlock()
		if hasAbort {
			if code, message, ok := s.runQueuedTopologyAbort(q, abort); !ok {
				s.finishQueuedTopologyJobInterrupted(q, code, message)
				return
			}
		}
		s.finishQueuedTopologyJobCancelled(q)
		return
	}

	if backupErr != nil {
		s.mu.Unlock()
		if !q.cancellable && q.job.Type == TypeDiskUpgradeData {
			s.finishQueuedTopologyJobInterrupted(q, "pre_topology_backup_failed", backupErr.Error())
			return
		}
		if hasAbort {
			if code, message, ok := s.runQueuedTopologyAbort(q, abort); !ok {
				s.finishQueuedTopologyJobInterrupted(q, code, message)
				return
			}
		}
		s.finishQueuedTopologyJobFailed(q, backupErr)
		return
	}

	delete(s.dispatching, q.job.ID)

	if s.maintenance && q.job.Type != TypeDiskUpgradeData {
		now := time.Now().UTC()
		q.job.Status = StatusInterrupted
		q.job.FinishedAt = &now
		snapshot := *q.job
		s.mu.Unlock()

		s.recordTerminalOutcomeWithRetry(snapshot)
		s.hub.Publish(&snapshot)
		return
	}

	now := time.Now().UTC()
	if err := s.store.UpdateStatus(context.Background(), q.job.ID, StatusRunning, q.job.Progress, "", "", &now, nil); err != nil {
		log.Printf("job: starting queued job %s after its pre-topology backup: %v", q.job.ID, err)
		s.queue = append(s.queue, q)
		s.mu.Unlock()
		return
	}
	q.job.Status = StatusRunning
	q.job.StartedAt = &now
	s.startJobLocked(q.job, q.run, q.cancellable)
	s.mu.Unlock()
	s.hub.Publish(q.job)
}

// runQueuedTopologyAbort runs abort for a dispatching queued topology job
// whose cancel request or failed start-time backup means its type's own
// cleanup must run before any terminal outcome is recorded (doc 02 §4
// invariant 3), holding s.aborting[q.job.ID] the whole time — the same
// guard a plainly queued or interrupted job's own Cancel already holds
// around abortAndCancel — so no Resume or second Cancel of the same id can
// run while it is in flight; both already refuse outright with
// ErrJobAbortInProgress while their own such check finds this held. On
// success it reports ok true. On failure it reports the error code and
// message abortAndCancel itself would record — an *OutcomeError's Code, or
// job_abort_failed — and ok false, for the caller to record the job
// interrupted with instead of the outcome it was about to record.
func (s *Scheduler) runQueuedTopologyAbort(q *queuedJob, abort AbortFunc) (code, message string, ok bool) {
	s.mu.Lock()
	s.aborting[q.job.ID] = true
	s.mu.Unlock()
	defer s.releaseAbort(q.job.ID)

	if err := abort(context.Background(), q.job.ID, q.job.Params); err != nil {
		code = "job_abort_failed"
		var oe *OutcomeError
		if errors.As(err, &oe) && oe.Code != "" {
			code = oe.Code
		}
		return code, err.Error(), false
	}
	return "", "", true
}

// finishQueuedTopologyJobCancelled records q cancelled and removes it from
// s.dispatching — reached only once its type's AbortFunc, if any, has
// already succeeded (doc 02 §4 invariant 3).
func (s *Scheduler) finishQueuedTopologyJobCancelled(q *queuedJob) {
	now := time.Now().UTC()
	s.mu.Lock()
	delete(s.dispatching, q.job.ID)
	q.job.Status = StatusCancelled
	q.job.ErrorCode = ""
	q.job.ErrorMessage = ""
	q.job.FinishedAt = &now
	snapshot := *q.job
	s.mu.Unlock()

	s.recordTerminalOutcomeWithRetry(snapshot)
	s.hub.Publish(&snapshot)
	s.dispatch()
}

// finishQueuedTopologyJobFailed records q failed with backupErr and
// removes it from s.dispatching — reached only once its type's AbortFunc,
// if any, has already succeeded (doc 02 §4 invariant 3).
func (s *Scheduler) finishQueuedTopologyJobFailed(q *queuedJob, backupErr error) {
	now := time.Now().UTC()
	s.mu.Lock()
	delete(s.dispatching, q.job.ID)
	q.job.Status = StatusFailed
	q.job.ErrorCode = "pre_topology_backup_failed"
	q.job.ErrorMessage = backupErr.Error()
	q.job.FinishedAt = &now
	snapshot := *q.job
	s.mu.Unlock()

	s.recordTerminalOutcomeWithRetry(snapshot)
	s.hub.Publish(&snapshot)
	s.dispatch()
}

// finishQueuedTopologyJobInterrupted records q interrupted with code/
// message and removes it from s.dispatching. Called either with its own
// type's AbortFunc having just failed — neither cancelled nor failed would
// be true then (doc 02 §4 invariant 3): nothing confirms the type's own
// cleanup, Unwind for a data-disk upgrade, actually succeeded, so the job
// stays pending instead, reachable by the user's next Cancel or Resume,
// exactly as abortAndCancel's own failed-abort branch already leaves a
// plainly queued or interrupted job — or with a data-disk upgrade's own
// start-time backup having failed past its release decision, where no
// AbortFunc ever runs at all (doc 02 §4 invariant 4: nothing for Unwind to
// undo once the release has committed) and q's checkpoint is left exactly
// as it was, so the same resume completes the release once the backup
// destination is fixed.
func (s *Scheduler) finishQueuedTopologyJobInterrupted(q *queuedJob, code, message string) {
	now := time.Now().UTC()
	s.mu.Lock()
	delete(s.dispatching, q.job.ID)
	q.job.Status = StatusInterrupted
	q.job.ErrorCode = code
	q.job.ErrorMessage = message
	q.job.FinishedAt = &now
	snapshot := *q.job
	s.mu.Unlock()

	s.recordTerminalOutcomeWithRetry(snapshot)
	s.hub.Publish(&snapshot)
	s.dispatch()
}

// isCancellationDerived reports whether err is itself a RunFunc's own
// reaction to observing ctx's cancellation — never a genuine, unrelated
// failure that merely happened to return after a raced Cancel (#379).
// context.Canceled and context.DeadlineExceeded cover a RunFunc that
// returns ctx.Err() itself, or blocks on ctx.Done() until a killed
// subprocess's own wrapped exit error comes back (runStream,
// internal/parity/snapraid_engine.go). ErrCancelRequested and
// ErrJobNeedsRetry cover TypeDiskUpgradeData's own releasing-save/
// resumable-checkpoint race (#364). runJob's reasonCancel branch silences
// only these; every other plain error is recorded rather than erased,
// since there is no race-free way to tell "the RunFunc reacted to this
// cancel" from "an unrelated failure happened to return microseconds
// after one landed" by timing alone.
func isCancellationDerived(err error) bool {
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrCancelRequested) ||
		errors.Is(err, ErrJobNeedsRetry)
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
		id:            rj.job.ID,
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
		keepForResume: func() bool {
			// Called with no lock held: the test hook, when set, may
			// itself call s.Cancel, which takes s.mu then rj.mu — taking
			// rj.mu here first would deadlock a hook that lands the
			// Cancel this call is meant to race against (#378). Because
			// the hook call is synchronous, by the time it returns any
			// Cancel it made is already fully committed (reason and the
			// underlying context cancellation both set) before this
			// function goes on to read rj.reason below.
			if s.resumableDecisionHook != nil {
				s.resumableDecisionHook(rj.job.ID)
			}
			rj.mu.Lock()
			defer rj.mu.Unlock()
			if rj.reason == reasonCancel {
				return false
			}
			// Committing here refuses every Cancel for the rest of this
			// run the same way #364's own post-return settle window
			// does — rj.finished already means exactly "no further
			// Cancel may change this job's outcome" to Cancel itself,
			// and that is exactly the guarantee this commit needs too.
			rj.finished = true
			return true
		},
	}

	runErr := rj.run(ctx, rc)

	if s.beforeFinishedHook != nil {
		s.beforeFinishedHook(rj.job.ID)
	}

	// rj.finished is set, and reason captured, the instant run returns —
	// ahead of everything else below, including closing the log — so a
	// Cancel racing in from here on (the job is still in s.running; the
	// delete below hasn't run yet) finds finished already true and
	// refuses instead of mutating reason too late to affect an outcome
	// that has already, in fact, happened (#364). Whichever of this lock
	// or Cancel's own rj.mu.Lock happens to land first is what settles
	// the linearized order between "this run finished" and "a Cancel
	// call arrived" — there is no other way to order two truly
	// independent goroutines — but once it is set, no later Cancel can
	// ever again change what gets recorded.
	rj.mu.Lock()
	rj.finished = true
	reason := rj.reason
	rj.mu.Unlock()

	if s.settleHook != nil {
		s.settleHook(rj.job.ID)
	}

	if closer != nil {
		if err := closer.Close(); err != nil {
			log.Printf("job: closing log for job %s: %v", rj.job.ID, err)
		}
	}

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
		// Cancel is the definitive status here regardless of what runErr
		// otherwise says — but the status is the only thing a Cancel is
		// ever allowed to override. An explicit *CancelCleanupError or
		// *OutcomeError is always surfaced — a RunFunc that returns either
		// has already, itself, decided this needs reporting beyond the
		// cancel. A plain error is classified by identity, not by timing
		// (#379): isCancellationDerived silences only an error that is
		// itself the RunFunc's own reaction to observing ctx's
		// cancellation, so nothing further is reported beyond the cancel
		// itself. Every other plain error survived its own last
		// cancellation check and returned a genuine, unrelated failure — a
		// raced Cancel landing microseconds later must not erase it, so it
		// is named through a generic code instead of silently dropped
		// under a bare, errorless Cancelled.
		status = StatusCancelled
		var cleanup *CancelCleanupError
		switch {
		case runErr == nil:
		case errors.As(runErr, &cleanup):
			errCode = cleanup.Code
			errMessage = cleanup.Error()
		case hasOutcome:
			errCode = outcome.Code
			errMessage = runErr.Error()
		case isCancellationDerived(runErr):
		default:
			errCode = "job_failed_before_cancel"
			errMessage = runErr.Error()
		}
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

// CancelCleanupError lets a RunFunc report that a Cancel it accepted
// (runJob's own reason == reasonCancel) went through — the job still ends
// StatusCancelled, exactly as a plain return would record it — but its own
// cleanup afterward hit a problem that must be recorded, not silently
// dropped (#364). Unlike OutcomeError, this never changes the recorded
// status: Cancel stays the definitive, deliberate outcome; Code and the
// wrapped error's message are only carried alongside it.
type CancelCleanupError struct {
	Code string
	Err  error
}

func (e *CancelCleanupError) Error() string { return e.Err.Error() }

func (e *CancelCleanupError) Unwrap() error { return e.Err }
