package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/notify"
)

// drillPublisher is the one notify.Service method the restore drill alert
// needs; tests use a fake.
type drillPublisher interface {
	Publish(ctx context.Context, event notify.EventType, title, message string) error
}

// wireRestoreDrill is what main.go calls to make the restore drill
// reachable: it gives the backup service the store of the last result,
// which GET and POST /backup/drill read through Handler.Backup
// (wireBackup), and registers the restore_drill job. A failed drill
// publishes notify.EventRestoreDrillFailed. A test calls it too, rather
// than repeating the assignments. With no backup service nothing is
// registered, and the operations answer 501.
func wireRestoreDrill(registry *job.Registry, svc *backup.Service, drills backup.DrillStore, notifier drillPublisher) {
	if svc == nil {
		return
	}
	svc.Drills = drills
	// A cancelled drill concludes nothing: it records no result and alerts
	// on nothing, since the operator stopped it.
	registry.Register(job.TypeRestoreDrill, true, job.RunRestoreDrill(func(ctx context.Context, out io.Writer) error {
		return svc.RunDrill(ctx, out, restoreDrillAlert(notifier))
	}))
}

func restoreDrillAlert(notifier drillPublisher) backup.DrillAlert {
	return func(ctx context.Context, r backup.DrillResult) error {
		if notifier == nil {
			return nil
		}
		return notifier.Publish(ctx, notify.EventRestoreDrillFailed, "Restore drill failed",
			fmt.Sprintf("The newest backup could not be verified, so it may not restore: %s.", r.Failure()))
	}
}

// scheduledRestoreDrill is the schedule's entry for the monthly restore
// drill: it queues the same job POST /backup/drill does. A drill that
// cannot even be queued (maintenance mode, say) is a drill that did not run,
// so it is recorded and alerted like one that failed.
func scheduledRestoreDrill(svc *backup.Service, scheduler *job.Scheduler, notifier drillPublisher) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		_, err := api.SubmitRestoreDrill(ctx, svc, scheduler)
		if err == nil {
			return nil
		}
		failed := svc.FailDrill(ctx, fmt.Errorf("the scheduled drill could not be started: %w", err), restoreDrillAlert(notifier))
		return errors.Join(err, failed)
	}
}

// wireRestoreDrillSchedule makes the monthly restore drill run from the
// schedule loop. Nothing is wired without a service, so the schedule's
// restore_drill window stays unclaimed.
func wireRestoreDrillSchedule(r *scheduleRunner, svc *backup.Service, scheduler *job.Scheduler, notifier drillPublisher) {
	if svc == nil {
		return
	}
	if r.OtherJobs == nil {
		r.OtherJobs = map[string]func(context.Context) error{}
	}
	r.OtherJobs["restore_drill"] = scheduledRestoreDrill(svc, scheduler, notifier)
}
