// Package api implements the server interfaces ogen generates from
// api/openapi.yaml (D18). Handler below implements every method
// apiv1.Handler declares, deliberately not by embedding
// apiv1.UnimplementedHandler: an explicit method set is what makes the
// compile-time assertion below mean something. Deleting a method, or
// changing its signature so it no longer matches the spec, is a build
// failure, not a test failure.
package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/acme"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/notify"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/share"
	"github.com/mdg-labs/hoserva/internal/store"
	"github.com/mdg-labs/hoserva/internal/store/metrics"
	"github.com/mdg-labs/hoserva/internal/update"
)

// Handler implements apiv1.Handler against the job system (#19): no
// business logic lives here (CLAUDE.md's "no business logic in API
// handlers") — every method below only translates between apiv1's
// generated types and job.Scheduler/job.Store/job.LogStore's own, and maps
// the errors they return to the spec's shared Error schema.
type Handler struct {
	Scheduler *job.Scheduler
	Store     *job.Store
	Logs      *job.LogStore
	// Auth is #22's setup/login/session/TOTP business logic — nil is only
	// valid in tests that exercise none of those operations.
	Auth *AuthService
	// Notify is #35's channel/routing/quiet-hours business logic — nil is
	// only valid in tests that exercise none of those operations.
	Notify *notify.Service
	// Disks is the disk provider for list/doctor/status — nil returns empty
	// inventory rather than an error.
	Disks disk.Provider
	// Parity is the SnapRAID engine for doctor freshness — nil skips that
	// check with a warning.
	Parity parity.Engine
	// ParityGuard evaluates threshold-guard state for run-diff (doc 02 §2).
	ParityGuard parity.Guard
	// RelocationManifest is Q15's own persisted current relocation
	// manifest and removing-disks set (doc 09 §3-4) — RunParityDiff loads
	// it and runs it through parity.ConfirmManifestTargets before
	// evaluating ParityGuard, so a preview reflects an in-progress
	// relocation — including a Q14 two-phase relocation's trailing sync
	// (#252) — the same way a production Sync call does. Nil evaluates
	// against nil manifest/removingDisks, matching this handler's own
	// pre-#194 behaviour.
	RelocationManifest *parity.RelocationManifestStore
	paritySnap         *paritySnapshotStore
	parityOnce         sync.Once
	// Backup is the config archive builder for export/import — nil returns
	// 501 from those operations.
	Backup *backup.Service
	// Settings is #178's hostname/timezone/backup-passphrase business
	// logic — nil returns an internal error from those operations.
	Settings *SettingsService
	// Schedules is #197's recurring-job schedule business logic — nil
	// returns an internal error from those operations.
	Schedules *ScheduleService
	// Array is Q70's stop/start sequence. Nil returns 501 from those
	// operations — the handler never duplicates the sequence itself.
	Array *job.ArraySequence
	// History is spin-state and audit-log persistence (Q32, Q74). Nil
	// returns an empty wake-events list rather than an error.
	History *store.History
	// Metrics is metrics.db (Q74). Nil returns empty series rather than an
	// error — losing graphs must not look like array failure (#186).
	Metrics *metrics.Store
	// Updates is self-update, rollback and reboot (Q67, Q68). Nil returns
	// 501 from those operations.
	Updates *update.Engine
	// Generator writes managed files under a caller-supplied root (Q76).
	// Nil skips host-config doctor checks and returns 501 from apply.
	Generator *config.Generator
	// HostConfig persists Q76 import/leave choices. Nil returns 501 from apply.
	HostConfig *store.HostConfigStore
	// Docker lists Engine containers and images for Q76. Nil means Docker
	// is treated as not installed for host-config checks.
	Docker config.DockerInventory
	// ArrayStore is create-array topology, used to decide whether a Docker
	// data-root move to cache is even possible (Q62). Nil means no cache.
	ArrayStore *store.ArrayStore
	// DiskMounter mounts and unmounts external disks by filesystem UUID
	// (Q72). Nil uses DirectMounter over DiskRunner.
	DiskMounter disk.UnitMounter
	// DiskRunner is the argv runner for blkid/mount/umount on external
	// disks. Nil uses CommandRunner.
	DiskRunner disk.Runner
	// Shares is the share model (#46). Nil returns 501 from share operations.
	Shares *share.Service
	// Network is host ifupdown settings with confirm-or-revert (Q75, #114).
	// Nil returns 501 from those operations.
	Network *config.NetworkService
	// HTTPS is certificate, access-scope and listen-port controls for the
	// network settings page. Nil omits live values (tests).
	HTTPS HTTPSControl
	// ACME is Let's Encrypt DNS-01 (#211). Nil omits status and returns 501
	// from configure/disable.
	ACME *acme.Service
}

var _ apiv1.Handler = (*Handler)(nil)

func (h *Handler) ListJobs(ctx context.Context, params apiv1.ListJobsParams) (*apiv1.ListJobsOK, error) {
	filter := job.ListFilter{}
	if class, ok := params.Class.Get(); ok {
		c := job.Class(class)
		filter.Class = &c
	}
	if status, ok := params.Status.Get(); ok {
		s := job.Status(status)
		filter.Status = &s
	}
	if limit, ok := params.Limit.Get(); ok {
		filter.Limit = int(limit)
	}

	jobs, err := h.Store.List(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("listing jobs: %w", err)
	}

	out := make([]apiv1.Job, 0, len(jobs))
	for _, j := range jobs {
		apiJob, err := jobToAPI(j)
		if err != nil {
			return nil, err
		}
		out = append(out, *apiJob)
	}
	return &apiv1.ListJobsOK{Jobs: out}, nil
}

func (h *Handler) GetJob(ctx context.Context, params apiv1.GetJobParams) (*apiv1.Job, error) {
	j, err := h.Store.Get(ctx, params.JobId.String())
	if err != nil {
		return nil, mapStoreError(params.JobId, err)
	}
	return jobToAPI(j)
}

func (h *Handler) CancelJob(ctx context.Context, params apiv1.CancelJobParams) (*apiv1.Job, error) {
	j, err := h.Scheduler.Cancel(ctx, params.JobId.String())
	if err != nil {
		return nil, mapSchedulerError(params.JobId, err)
	}
	return jobToAPI(j)
}

func (h *Handler) ResumeJob(ctx context.Context, params apiv1.ResumeJobParams) (*apiv1.Job, error) {
	j, err := h.Scheduler.Resume(ctx, params.JobId.String())
	if err != nil {
		return nil, mapSchedulerError(params.JobId, err)
	}
	return jobToAPI(j)
}

func (h *Handler) GetJobLog(ctx context.Context, params apiv1.GetJobLogParams) (apiv1.GetJobLogOK, error) {
	if _, err := h.Store.Get(ctx, params.JobId.String()); err != nil {
		return apiv1.GetJobLogOK{}, mapStoreError(params.JobId, err)
	}
	r, err := h.Logs.Open(params.JobId.String())
	if err != nil {
		if errors.Is(err, job.ErrLogNotFound) {
			return apiv1.GetJobLogOK{}, &apiError{code: "job_log_not_found", statusCode: 404, message: fmt.Sprintf("job %s has no captured log", params.JobId)}
		}
		return apiv1.GetJobLogOK{}, fmt.Errorf("opening log for job %s: %w", params.JobId, err)
	}
	return apiv1.GetJobLogOK{Data: r}, nil
}

// apiError is a handler error already classified against the spec's
// shared Error schema (doc 01 §5) — mapStoreError and mapSchedulerError
// build these so NewError below has one place to render them, instead of
// constructing *apiv1.ErrorStatusCode inline at every call site.
type apiError struct {
	code       string
	statusCode int
	message    string
}

func (e *apiError) Error() string { return e.message }

func mapStoreError(id uuid.UUID, err error) error {
	if errors.Is(err, job.ErrNotFound) {
		return &apiError{code: "job_not_found", statusCode: 404, message: fmt.Sprintf("no job with id %s", id)}
	}
	return fmt.Errorf("job %s: %w", id, err)
}

func mapSchedulerError(id uuid.UUID, err error) error {
	switch {
	case errors.Is(err, job.ErrNotFound):
		return &apiError{code: "job_not_found", statusCode: 404, message: fmt.Sprintf("no job with id %s", id)}
	case errors.Is(err, job.ErrJobNotCancellable):
		return &apiError{code: "job_not_cancellable", statusCode: 409, message: fmt.Sprintf("job %s does not support cancellation", id)}
	case errors.Is(err, job.ErrJobNotResumable):
		return &apiError{code: "job_not_resumable", statusCode: 409, message: fmt.Sprintf("job %s is not a resumable job type (Q29)", id)}
	case errors.Is(err, job.ErrJobNotInterrupted):
		return &apiError{code: "job_not_interrupted", statusCode: 409, message: fmt.Sprintf("job %s can only be resumed while interrupted", id)}
	case errors.Is(err, job.ErrJobNotRunning):
		return &apiError{code: "job_not_running", statusCode: 409, message: fmt.Sprintf("job %s is not queued or running", id)}
	case errors.Is(err, job.ErrMaintenanceMode):
		return &apiError{code: "maintenance_mode", statusCode: 409, message: "maintenance mode is active — no new jobs are accepted"}
	case errors.Is(err, job.ErrJobTypeNotRegistered):
		return &apiError{code: "job_type_not_registered", statusCode: 501, message: fmt.Sprintf("job %s's type has no registered implementation yet", id)}
	default:
		return fmt.Errorf("job %s: %w", id, err)
	}
}

// NewError maps a handler error to the spec's shared Error schema
// (doc 01 §5). Every deliberate error this Handler returns is an
// *apiError built above; anything else is an unclassified internal error,
// logged server-side with its detail and reported as an opaque 500 with
// no internal detail in the response body.
func (*Handler) NewError(ctx context.Context, err error) *apiv1.ErrorStatusCode {
	var ae *apiError
	if !errors.As(err, &ae) {
		err = mapAuthError(err)
		errors.As(err, &ae)
	}
	if ae != nil {
		return &apiv1.ErrorStatusCode{
			StatusCode: ae.statusCode,
			Response:   apiv1.Error{Code: ae.code, Message: ae.message},
		}
	}
	log.Printf("hoservad: internal error: %v", err)
	return &apiv1.ErrorStatusCode{
		StatusCode: 500,
		Response: apiv1.Error{
			Code:    "internal",
			Message: "an internal error occurred",
		},
	}
}
