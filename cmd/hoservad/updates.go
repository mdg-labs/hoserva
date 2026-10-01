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
	"github.com/mdg-labs/hoserva/internal/backup"
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

// newUpdater builds the update execution over the daemon's container
// services, the appdata backup's snapshot mechanism and the update history,
// or, without a Docker client (apps and the appdata service are nil then),
// one that serves only the settings kept in history.
func newUpdater(apps *appServices, history *store.ImageHistoryStore, appdata *backup.AppdataService, checker *container.UpdateChecker) *container.Updater {
	u := &container.Updater{History: history}
	if apps == nil || appdata == nil {
		return u
	}
	u.Lifecycle = apps.Lifecycle
	u.Snapshots = backup.UpdateSnapshots{Appdata: appdata}
	u.Statuses = checker
	return u
}

// wireContainerUpdates is what main.go calls to make container updates
// reachable: GET /apps/updates and the bulk, single, revert, opt-out,
// history and settings operations (Handler.AppUpdates, Handler.AppUpdater),
// and the container_update job in all three modes: the daily check, which
// also removes the kept images whose keep period has ended, updating, and
// reverting. Updating and reverting wait for the start-up reconciliation of
// interrupted recreates, as the recreate job does, so they never start while
// its leftovers are still being sorted out. A test calls it too, rather than
// repeating the assignments. The job is registered even with no Docker
// client, so a submission is refused with a reason instead of as an unknown
// type.
func wireContainerUpdates(handler *api.Handler, registry *job.Registry, checker *container.UpdateChecker, updater *container.Updater, awaitReconciled func(context.Context) error) {
	handler.AppUpdates = checker
	handler.AppUpdater = updater
	errNoDocker := errors.New("docker is not configured on this daemon")
	registry.Register(job.TypeContainerUpdate, true, job.RunContainerUpdate(job.ContainerUpdateDeps{
		Check: func(ctx context.Context, out io.Writer) error {
			if checker == nil {
				return errNoDocker
			}
			checkErr := checker.Run(ctx, out)
			if updater == nil || updater.Lifecycle == nil {
				return checkErr
			}
			return errors.Join(checkErr, updater.Prune(ctx, out))
		},
		Update: func(ctx context.Context, name string, out io.Writer) error {
			if updater == nil || updater.Lifecycle == nil {
				return errNoDocker
			}
			if err := awaitReconciled(ctx); err != nil {
				return err
			}
			return updater.Update(ctx, name, out)
		},
		Revert: func(ctx context.Context, name string, sharers []string, out io.Writer) error {
			if updater == nil || updater.Lifecycle == nil {
				return errNoDocker
			}
			if err := awaitReconciled(ctx); err != nil {
				return err
			}
			return updater.Revert(ctx, name, sharers, out)
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
