package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/ogen-go/ogen/ogenerrors"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/web/fixtures"
)

// unauthorizedCode and unauthorizedMessage are the honest 401 the spec's
// shared Error schema carries when a request has no credential at all
// (neither an Authorization: Bearer … header nor a hoserva_session cookie).
// The spec declares no dedicated 401 response (every operation only lists a
// "default" Error), so this code is the mock's own, consistent with the
// job_* codes above; /api/v1/events (events.go) uses the same values so a
// client sees one shape everywhere.
const (
	unauthorizedCode    = "unauthorized"
	unauthorizedMessage = "this request requires a credential: an Authorization: Bearer <token> header or a hoserva_session cookie (any value is accepted by this mock)"
)

// handler implements apiv1.Handler from a scenario's fixture jobs (doc 06
// §8, doc 12 §2): the same jobs.json a backend test decodes, so the mock
// cannot show a state the fixtures don't also cover. Deleting a method
// below, or changing its signature so it no longer matches the generated
// interface, is a compile failure by way of the assertion, not a runtime
// one (D18).
type handler struct {
	scenario string

	mu   sync.Mutex
	jobs map[uuid.UUID]apiv1.Job

	// notifyMu guards the in-memory notification state below (#35) —
	// separate from mu (jobs) since neither ever needs the other's lock.
	// None of this comes from a fixture: no scenario encodes notification
	// channels yet, so every mock instance starts with none configured,
	// exactly like a fresh Hoserva install.
	notifyMu        sync.Mutex
	channels        map[uuid.UUID]apiv1.NotificationChannel
	routing         map[apiv1.NotificationEventType]apiv1.NotificationRoutingEntry
	quietHours      apiv1.NotificationQuietHours
	inboxAlerts     []apiv1.NotificationAlert
	generalSettings apiv1.GeneralSettings
	schedules       apiv1.Schedules
	updateStatus    apiv1.UpdateStatus
	network         apiv1.NetworkSettings
	certSerial      int64

	// maintenance is Q70's maintenance mode for this mock instance:
	// StopArray sets it, StartArray clears it, GetStatus reports it.
	maintenance bool

	shares map[string]apiv1.Share

	externalMu sync.Mutex
	external   map[string]apiv1.ExternalDisk
}

var _ apiv1.Handler = (*handler)(nil)

// newHandler loads scenario's jobs fixture into an in-memory store. Cancel
// and Resume mutate this copy so a client sees the effect of its own call
// on a subsequent ListJobs/GetJob, which is the point of exercising these
// against the generated client rather than serving static responses.
func newHandler(scenario string) (*handler, error) {
	if !fixtures.Valid(scenario) {
		return nil, fmt.Errorf("unknown scenario %q", scenario)
	}
	raw, err := fixtures.JobsJSON(scenario)
	if err != nil {
		return nil, fmt.Errorf("load jobs fixture: %w", err)
	}
	var listJobsOK apiv1.ListJobsOK
	if err := listJobsOK.UnmarshalJSON(raw); err != nil {
		return nil, fmt.Errorf("decode jobs fixture: %w", err)
	}

	jobs := make(map[uuid.UUID]apiv1.Job, len(listJobsOK.Jobs))
	for _, job := range listJobsOK.Jobs {
		if err := job.Validate(); err != nil {
			return nil, fmt.Errorf("fixture job %s fails validation: %w", job.ID, err)
		}
		jobs[job.ID] = job
	}

	return &handler{
		scenario:     scenario,
		jobs:         jobs,
		channels:     make(map[uuid.UUID]apiv1.NotificationChannel),
		routing:      defaultNotificationRouting(),
		quietHours:   apiv1.NotificationQuietHours{Enabled: false, Start: "22:00", End: "07:00", CriticalAlwaysDelivers: true},
		inboxAlerts:  defaultInboxAlerts(scenario),
		schedules:    defaultMockSchedules(),
		updateStatus: defaultMockUpdateStatus(),
		network:      defaultMockNetwork(),
		shares:       make(map[string]apiv1.Share),
		external:     make(map[string]apiv1.ExternalDisk),
	}, nil
}

// errJobNotFound and friends are sentinels handed to NewError, keeping the
// classification in one place (doc 01 §5's shared Error model) instead of
// scattering *apiv1.ErrorStatusCode construction across every method.
type mockError struct {
	code       string
	statusCode int
	message    string
}

func (e *mockError) Error() string { return e.message }

func errJobNotFound(id uuid.UUID) error {
	return &mockError{code: "job_not_found", statusCode: 404, message: fmt.Sprintf("no job with id %s", id)}
}

func errJobNotCancellable(id uuid.UUID) error {
	return &mockError{code: "job_not_cancellable", statusCode: 409, message: fmt.Sprintf("job %s does not support cancellation", id)}
}

func errJobNotResumable(id uuid.UUID) error {
	return &mockError{code: "job_not_resumable", statusCode: 409, message: fmt.Sprintf("job %s is not a resumable job type (Q29)", id)}
}

func (h *handler) ListJobs(ctx context.Context, params apiv1.ListJobsParams) (*apiv1.ListJobsOK, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	class, hasClass := params.Class.Get()
	status, hasStatus := params.Status.Get()
	limit := int(params.Limit.Or(50))

	matched := make([]apiv1.Job, 0, len(h.jobs))
	for _, job := range h.jobs {
		if hasClass && job.Class != class {
			continue
		}
		if hasStatus && job.Status != status {
			continue
		}
		matched = append(matched, job)
	}
	sort.Slice(matched, func(i, j int) bool {
		return matched[i].CreatedAt.After(matched[j].CreatedAt)
	})
	if limit >= 0 && len(matched) > limit {
		matched = matched[:limit]
	}

	return &apiv1.ListJobsOK{Jobs: matched}, nil
}

func (h *handler) GetJob(ctx context.Context, params apiv1.GetJobParams) (*apiv1.Job, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	job, ok := h.jobs[params.JobId]
	if !ok {
		return nil, errJobNotFound(params.JobId)
	}
	return &job, nil
}

func (h *handler) CancelJob(ctx context.Context, params apiv1.CancelJobParams) (*apiv1.Job, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	job, ok := h.jobs[params.JobId]
	if !ok {
		return nil, errJobNotFound(params.JobId)
	}
	if !job.Cancellable {
		return nil, errJobNotCancellable(params.JobId)
	}

	job.Status = apiv1.JobStatusCancelled
	job.FinishedAt = apiv1.NewOptNilDateTime(time.Now().UTC())
	h.jobs[params.JobId] = job
	return &job, nil
}

func (h *handler) ResumeJob(ctx context.Context, params apiv1.ResumeJobParams) (*apiv1.Job, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	job, ok := h.jobs[params.JobId]
	if !ok {
		return nil, errJobNotFound(params.JobId)
	}
	if !job.Resumable {
		return nil, errJobNotResumable(params.JobId)
	}

	job.Status = apiv1.JobStatusRunning
	h.jobs[params.JobId] = job
	return &job, nil
}

func (h *handler) GetJobLog(ctx context.Context, params apiv1.GetJobLogParams) (apiv1.GetJobLogOK, error) {
	h.mu.Lock()
	_, ok := h.jobs[params.JobId]
	h.mu.Unlock()
	if !ok {
		return apiv1.GetJobLogOK{}, errJobNotFound(params.JobId)
	}

	raw, err := fixtures.JobLog()
	if err != nil {
		return apiv1.GetJobLogOK{}, fmt.Errorf("load job-log fixture: %w", err)
	}

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(raw); err != nil {
		return apiv1.GetJobLogOK{}, fmt.Errorf("compress job log: %w", err)
	}
	if err := gw.Close(); err != nil {
		return apiv1.GetJobLogOK{}, fmt.Errorf("compress job log: %w", err)
	}

	return apiv1.GetJobLogOK{Data: bytes.NewReader(buf.Bytes())}, nil
}

// NewError maps a handler error to the spec's shared Error schema (doc 01
// §5). Every deliberate error the mock returns is one of the sentinels
// above. The generated router also calls this for a request the security
// layer itself rejects (api/gen/go/oas_handlers_gen.go), which happens for
// every request carrying neither an Authorization: Bearer … header nor a
// hoserva_session cookie — securityHandler (security.go) never runs in that
// case, since the router only invokes it once some credential is present.
// That is reported as a real 401, not the fallback 500: the mock's whole
// point (doc 06 §8) is every screen reachable without first standing up
// real auth, and a bare 500 with no route to recovery defeats that.
//
// Anything else (a fixture load failure at startup would panic before this
// ever runs) is logged with its detail server-side and reported as an
// opaque 500 — no internal error text reaches the response body.
func (h *handler) NewError(ctx context.Context, err error) *apiv1.ErrorStatusCode {
	if me, ok := err.(*mockError); ok {
		return &apiv1.ErrorStatusCode{
			StatusCode: me.statusCode,
			Response:   apiv1.Error{Code: me.code, Message: me.message},
		}
	}

	var secErr *ogenerrors.SecurityError
	if errors.As(err, &secErr) {
		return &apiv1.ErrorStatusCode{
			StatusCode: http.StatusUnauthorized,
			Response:   apiv1.Error{Code: unauthorizedCode, Message: unauthorizedMessage},
		}
	}

	log.Printf("mockapi: internal error: %v", err)
	return &apiv1.ErrorStatusCode{
		StatusCode: 500,
		Response:   apiv1.Error{Code: "internal", Message: "an internal error occurred"},
	}
}
