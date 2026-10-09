package api

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/mdg-labs/hoserva/api/gen/go/events"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/notify"
	"github.com/mdg-labs/hoserva/internal/template"
)

// jobToEvent translates a *job.Job into the generated events.JobProgressEvent
// (D18, doc 01 §5, Q63): the events package is generated independently of
// apiv1 (jschemagen, not ogen — Q63), so it has its own, separately
// generated Job/JobClass/JobStatus/JobType types; this is the one place
// that maps internal/job's domain type into them.
func jobToEvent(j *job.Job) (events.Event, error) {
	id, err := uuid.Parse(j.ID)
	if err != nil {
		return events.Event{}, fmt.Errorf("job %s has a non-UUID id: %w", j.ID, err)
	}

	out := events.Job{
		ID:          id,
		Type:        events.JobType(j.Type),
		Class:       events.JobClass(j.Class),
		Status:      events.JobStatus(j.Status),
		Resumable:   j.Resumable,
		Cancellable: j.Cancellable,
		CreatedAt:   j.CreatedAt,
	}
	if j.Progress != nil {
		out.Progress = events.NewOptNilInt32(int32(*j.Progress))
	}
	if j.StartedAt != nil {
		out.StartedAt = events.NewOptNilDateTime(*j.StartedAt)
	}
	if j.FinishedAt != nil {
		out.FinishedAt = events.NewOptNilDateTime(*j.FinishedAt)
	}
	if j.ErrorCode != "" || j.ErrorMessage != "" {
		out.Error = events.NewOptNilError(events.Error{Code: j.ErrorCode, Message: j.ErrorMessage})
	}

	return events.NewJobProgressEventEvent(events.JobProgressEvent{
		Event: string(events.JobProgressEventEvent),
		Data:  out,
	}), nil
}

func alertToEvent(a *notify.Alert) (events.Event, error) {
	return events.NewNotificationEventEvent(events.NotificationEvent{
		Event: string(events.NotificationEventEvent),
		Data: events.NotificationEventData{
			ID:        a.ID,
			EventType: events.NotificationEventType(a.EventType),
			Level:     events.NotificationLevel(a.Severity),
			Title:     a.Title,
			Message:   a.Message,
			CreatedAt: a.CreatedAt,
		},
	}), nil
}

func stateChangeToEvent(sc container.StateChange) (events.Event, error) {
	data := events.ContainerStateEventData{
		ContainerId: sc.ID,
		Name:        sc.Name,
		State:       events.AppState(sc.State),
		At:          sc.At,
	}
	if sc.Health != "" {
		data.Health = events.NewOptAppHealth(events.AppHealth(sc.Health))
	}
	return events.NewContainerStateEventEvent(events.ContainerStateEvent{
		Event: string(events.ContainerStateEventEvent),
		Data:  data,
	}), nil
}

func catalogCheckToEvent(res template.CheckResult) (events.Event, error) {
	data := events.CatalogRefresh{CheckedAt: res.CheckedAt, Outcome: events.CatalogCheckOutcome(res.Outcome)}
	switch res.Outcome {
	case template.OutcomeUpdated:
		data.NewTemplates = events.NewOptInt(res.New)
		data.UpdatedTemplates = events.NewOptInt(res.Updated)
	case template.OutcomeFailed:
		data.Reason = events.NewOptCatalogRefreshReason(events.CatalogRefreshReason(res.Reason))
		data.Message = events.NewOptString(res.Message)
	}
	return events.NewCatalogEventEvent(events.CatalogEvent{
		Event: string(events.CatalogEventEvent),
		Data:  data,
	}), nil
}

// defaultKeepAlive paces the SSE comment lines EventsHandler sends while
// idle, so a client (and any reverse proxy in front of hoservad) sees the
// connection is still alive rather than timing it out.
const defaultKeepAlive = 15 * time.Second

// EventsHandler serves /api/v1/events (doc 01 §5): ogen generates no
// server side for this one operation (Q63 — its SSE support is client-only
// for the whole spec, forcing this exact route out of both the generated
// Handler and client), so it is hand-written here, framing only
// events.Event values decoded through the same generated types
// api/gen/go/events/reader.go's client reads back (D18) — nothing here
// invents its own wire shape.
//
// Authenticate is the seam #22 wires: nil, or an error, refuses the
// request with the same shared Error shape every other operation uses
// (doc 01 §5) — 403 for an error that wraps ErrRoleDenied, 401 for any
// other; #19 leaves every request refused, matching SecurityHandler's own
// default until real session/token validation exists. It runs again on every
// keep-alive tick, and the stream ends when it fails, so a session revoked or
// a role lowered while the stream is open takes effect within one keep-alive
// interval. It must therefore read only what r still holds on a later call.
type EventsHandler struct {
	Hub          *job.Hub
	NotifyHub    *notify.Hub
	ContainerHub *container.Hub
	CatalogHub   *template.CheckHub
	Authenticate func(r *http.Request) error
	KeepAlive    time.Duration
}

func (h *EventsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.Authenticate == nil {
		writeUnauthorized(w)
		return
	}
	if err := h.Authenticate(r); err != nil {
		if errors.Is(err, ErrRoleDenied) {
			writeForbidden(w)
			return
		}
		writeUnauthorized(w)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	jobCh, unsubscribeJobs := h.Hub.Subscribe()
	defer unsubscribeJobs()
	var notifyCh <-chan *notify.Alert
	var unsubscribeNotify func()
	if h.NotifyHub != nil {
		notifyCh, unsubscribeNotify = h.NotifyHub.Subscribe()
		defer unsubscribeNotify()
	}
	var containerCh <-chan container.StateChange
	if h.ContainerHub != nil {
		var unsubscribeContainers func()
		containerCh, unsubscribeContainers = h.ContainerHub.Subscribe()
		defer unsubscribeContainers()
	}

	var catalogCh <-chan template.CheckResult
	if h.CatalogHub != nil {
		var unsubscribeCatalog func()
		catalogCh, unsubscribeCatalog = h.CatalogHub.Subscribe()
		defer unsubscribeCatalog()
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	// A client (and any proxy in front of hoservad) must see this
	// connection open immediately, not whenever the first event or
	// keep-alive happens to fire — Go's server otherwise buffers the
	// status line and headers until the first Write.
	flusher.Flush()

	keepAlive := h.KeepAlive
	if keepAlive <= 0 {
		keepAlive = defaultKeepAlive
	}
	ticker := time.NewTicker(keepAlive)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case j, ok := <-jobCh:
			if !ok {
				return
			}
			if err := writeEvent(w, flusher, func() (events.Event, error) { return jobToEvent(j) }); err != nil {
				return
			}
		case a, ok := <-notifyCh:
			if !ok {
				return
			}
			if err := writeEvent(w, flusher, func() (events.Event, error) { return alertToEvent(a) }); err != nil {
				return
			}
		case sc, ok := <-containerCh:
			if !ok {
				return
			}
			if err := writeEvent(w, flusher, func() (events.Event, error) { return stateChangeToEvent(sc) }); err != nil {
				return
			}
		case res, ok := <-catalogCh:
			if !ok {
				return
			}
			if err := writeEvent(w, flusher, func() (events.Event, error) { return catalogCheckToEvent(res) }); err != nil {
				return
			}
		case <-ticker.C:
			if err := h.Authenticate(r); err != nil {
				return
			}
			// A ":"-prefixed line is an SSE comment: it reaches no
			// "message"/EventSource listener, only keeps the connection
			// from looking idle to a client or an intermediate proxy.
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeEvent(w http.ResponseWriter, flusher http.Flusher, build func() (events.Event, error)) error {
	ev, err := build()
	if err != nil {
		return nil
	}
	frame, err := ev.MarshalJSON()
	if err != nil {
		return nil
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", frame); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// eventsRole is the x-hoserva-role api/openapi.yaml declares for streamEvents.
// ogen generates no operation name for that route (Q63), so operationRoles has
// no entry for it, and events_role_test.go checks this value against the spec.
const eventsRole = RoleViewer

// EnforceEventsRole refuses a principal whose role does not satisfy the
// events stream's declared role, with an error that wraps ErrRoleDenied.
func EnforceEventsRole(role Role) error {
	if !role.Satisfies(eventsRole) {
		return fmt.Errorf("%w: the events stream requires role %q, principal has %q", ErrRoleDenied, eventsRole, role)
	}
	return nil
}

const (
	unauthorizedCode    = "unauthorized"
	unauthorizedMessage = "this request requires a credential"
	forbiddenCode       = "forbidden"
	forbiddenMessage    = "the current account does not have permission for this operation"
)

// writeUnauthorized reports the same Error shape handler.go's NewError
// does, so a client sees one error contract whether it hit a generated
// operation or this hand-written one.
func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = fmt.Fprintf(w, `{"code":%q,"message":%q}`, unauthorizedCode, unauthorizedMessage)
}

// writeForbidden reports the same Error shape for a valid credential whose
// role does not satisfy the stream's.
func writeForbidden(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = fmt.Fprintf(w, `{"code":%q,"message":%q}`, forbiddenCode, forbiddenMessage)
}
