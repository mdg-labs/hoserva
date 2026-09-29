package main

import (
	"context"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/backup"
)

// mockDrillRun is a passing drill over the mock's two default
// destinations, finished at ranAt.
func mockDrillRun(ranAt time.Time) apiv1.RestoreDrillRun {
	stamp := ranAt.Format("2006-01-02T15-04-05")
	out := apiv1.RestoreDrillRun{RanAt: ranAt, Passed: true}
	for _, d := range backup.DefaultDestinations() {
		out.Destinations = append(out.Destinations, apiv1.RestoreDrillDestination{
			DestinationId: d.ID, DestinationName: d.Name, Passed: true,
			Archive: apiv1.NewOptNilString("hoserva-config-" + mockAppdataInstallation + "-" + stamp + ".tar.zst"),
		})
	}
	return out
}

func (h *handler) GetRestoreDrill(ctx context.Context) (*apiv1.RestoreDrill, error) {
	h.drillMu.Lock()
	defer h.drillMu.Unlock()
	out := &apiv1.RestoreDrill{}
	if h.drillLast != nil {
		out.LastRun = apiv1.NewOptRestoreDrillRun(*h.drillLast)
	}
	return out, nil
}

// StartRestoreDrill queues the job like every other mock job submission
// (this mock has no scheduler) and, since nothing here can fail, records a
// passing run as its result.
func (h *handler) StartRestoreDrill(ctx context.Context) (*apiv1.Job, error) {
	j, err := h.queueMockJob(apiv1.JobTypeRestoreDrill)
	if err != nil {
		return nil, err
	}
	run := mockDrillRun(time.Now().UTC().Truncate(time.Second))
	h.drillMu.Lock()
	h.drillLast = &run
	h.drillMu.Unlock()
	return j, nil
}

func seededDrillRun() *apiv1.RestoreDrillRun {
	run := mockDrillRun(time.Now().UTC().Add(-3 * 24 * time.Hour).Truncate(time.Second))
	return &run
}
