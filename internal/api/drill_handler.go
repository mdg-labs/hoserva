package api

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/job"
)

func errDrillNotConfigured() error {
	return &apiError{code: "not_configured", statusCode: 501, message: "restore drills are not configured on this daemon"}
}

func (h *Handler) GetRestoreDrill(ctx context.Context) (*apiv1.RestoreDrill, error) {
	if h.Backup == nil || h.Backup.Drills == nil {
		return nil, errDrillNotConfigured()
	}
	last, err := h.Backup.LastDrill(ctx)
	if err != nil {
		return nil, err
	}
	out := &apiv1.RestoreDrill{}
	if last != nil {
		out.LastRun = apiv1.NewOptRestoreDrillRun(drillRunToAPI(*last))
	}
	return out, nil
}

func drillRunToAPI(r backup.DrillResult) apiv1.RestoreDrillRun {
	out := apiv1.RestoreDrillRun{RanAt: r.RanAt, Passed: r.Passed, Destinations: make([]apiv1.RestoreDrillDestination, 0, len(r.Destinations))}
	if r.Error != "" {
		out.Error = apiv1.NewOptNilString(r.Error)
	}
	for _, d := range r.Destinations {
		item := apiv1.RestoreDrillDestination{DestinationId: d.DestinationID, DestinationName: d.DestinationName, Passed: d.Passed}
		if d.Archive != "" {
			item.Archive = apiv1.NewOptNilString(d.Archive)
		}
		if d.Error != "" {
			item.Error = apiv1.NewOptNilString(d.Error)
		}
		out.Destinations = append(out.Destinations, item)
	}
	return out
}

// SubmitRestoreDrill queues a restore_drill job behind any other drill. It
// is the one entry both the API and the schedule use.
func SubmitRestoreDrill(ctx context.Context, svc *backup.Service, sched *job.Scheduler) (*job.Job, error) {
	if svc == nil || svc.Drills == nil {
		return nil, errDrillNotConfigured()
	}
	if sched == nil {
		return nil, fmt.Errorf("job scheduler not configured")
	}
	j, err := sched.Submit(ctx, job.TypeRestoreDrill, []string{backup.DrillJobResource}, nil)
	if err != nil {
		return nil, mapSchedulerError(uuid.Nil, err)
	}
	return j, nil
}

func (h *Handler) StartRestoreDrill(ctx context.Context) (*apiv1.Job, error) {
	j, err := SubmitRestoreDrill(ctx, h.Backup, h.Scheduler)
	if err != nil {
		return nil, err
	}
	return jobToAPI(j)
}
