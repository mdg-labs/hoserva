package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"time"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/store"
)

// updateCheckMaxJitter bounds the random wait the daily update check puts
// between its schedule window opening and its first registry request, so
// installations do not all ask at the same minute (Q81).
const updateCheckMaxJitter = 15 * time.Minute

// updateCheckResource serialises update checks: two never run together.
const updateCheckResource = "container_update_check"

// newUpdateChecker builds the update check over the daemon's Docker
// Provider, or returns nil without a Docker client (apps is nil then).
func newUpdateChecker(apps *appServices, updates *store.UpdateStore, registry container.RegistryClient) *container.UpdateChecker {
	if apps == nil {
		return nil
	}
	return &container.UpdateChecker{
		Provider: apps.Lifecycle.Provider,
		Registry: registry,
		Results:  updates,
	}
}

// wireContainerUpdates is what main.go calls to make update detection
// reachable: GET /apps/updates (Handler.AppUpdates) and the container_update
// job, which only checks. An update-mode run fails with
// job.ErrContainerUpdateNotImplemented rather than report success for an
// update it did not perform. A test calls it too, rather than repeating the
// assignments. The job is registered even with no checker, so a submission
// is refused with a reason instead of as an unknown type.
func wireContainerUpdates(handler *api.Handler, registry *job.Registry, checker *container.UpdateChecker) {
	handler.AppUpdates = checker
	registry.Register(job.TypeContainerUpdate, true, job.RunContainerUpdate(job.ContainerUpdateDeps{
		Check: func(ctx context.Context, out io.Writer) error {
			if checker == nil {
				return errors.New("docker is not configured on this daemon")
			}
			return checker.Run(ctx, out)
		},
	}))
}

// scheduledUpdateCheck is the schedule's entry for the daily update check:
// it queues a check job that first waits a random part of
// updateCheckMaxJitter. A check that cannot even be queued (maintenance
// mode) is not retried before the next window.
func scheduledUpdateCheck(scheduler *job.Scheduler, jitter func() time.Duration) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		body, err := json.Marshal(job.ContainerUpdateParams{
			Mode:          job.ContainerUpdateModeCheck,
			JitterSeconds: int(jitter().Seconds()),
		})
		if err != nil {
			return fmt.Errorf("encoding container_update params: %w", err)
		}
		if _, err := scheduler.Submit(ctx, job.TypeContainerUpdate, []string{updateCheckResource}, body); err != nil {
			return fmt.Errorf("queueing the container update check: %w", err)
		}
		return nil
	}
}

func randomUpdateCheckJitter() time.Duration {
	return time.Duration(rand.Int64N(int64(updateCheckMaxJitter)))
}

// wireContainerUpdateSchedule makes the daily update check run from the
// schedule loop. Nothing is wired without a checker, so the schedule's
// container_update_check window stays unclaimed.
func wireContainerUpdateSchedule(r *scheduleRunner, checker *container.UpdateChecker, scheduler *job.Scheduler, jitter func() time.Duration) {
	if checker == nil {
		return
	}
	if r.OtherJobs == nil {
		r.OtherJobs = map[string]func(context.Context) error{}
	}
	r.OtherJobs["container_update_check"] = scheduledUpdateCheck(scheduler, jitter)
}
