package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"path/filepath"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/notify"
	"github.com/mdg-labs/hoserva/internal/store"
)

// appdataJournalName is the file in the state directory naming the
// containers a running appdata backup or restore has stopped.
const appdataJournalName = "appdata-backup-stopped.json"

// appdataPublisher is the one notify.Service method the appdata backup
// alerts need; tests use a fake.
type appdataPublisher interface {
	Publish(ctx context.Context, event notify.EventType, title, message string) error
}

// newAppdataService builds the appdata backup over the daemon's Docker
// lifecycle and backup destinations, or returns nil without a Docker
// client (apps is nil then).
func newAppdataService(apps *appServices, backupSvc *backup.Service, policies backup.AppdataPolicyStore, arrays *store.ArrayStore, stateDir string) *backup.AppdataService {
	if apps == nil {
		return nil
	}
	return &backup.AppdataService{
		Backup:      backupSvc,
		Containers:  backup.LifecycleContainers{Lifecycle: apps.Lifecycle},
		Roots:       appdataRoots(arrays),
		Policies:    policies,
		JournalPath: filepath.Join(stateDir, appdataJournalName),
	}
}

// wireAppdata is what main.go calls to make appdata backup reachable: the
// /appdata/backup operations (Handler.Appdata) and the three job types. A
// failed backup publishes notify.EventAppdataBackupFailed. A test calls it
// too, rather than repeating the assignments. With no service (no Docker)
// nothing is registered, and the operations answer 501.
func wireAppdata(handler *api.Handler, registry *job.Registry, svc *backup.AppdataService, notifier appdataPublisher) {
	if svc == nil {
		return
	}
	handler.Appdata = svc
	// A backup that is cancelled stops copying and starts every container
	// it stopped; a restore is not cancellable, because a cancel between
	// its steps is the one way to leave the appdata half replaced.
	registry.Register(job.TypeAppdataBackup, true, job.RunAppdataBackup(job.AppdataBackupDeps{
		Backup: func(ctx context.Context, requested, resolved []string, out io.Writer) error {
			return svc.Run(ctx, backup.AppdataRunRequest{Containers: requested, Resolved: resolved}, out)
		},
		Failed: func(ctx context.Context, err error) { publishAppdataFailure(ctx, notifier, err) },
	}))
	registry.Register(job.TypeAppdataRestore, false, job.RunAppdataRestore(func(ctx context.Context, p job.AppdataRestoreParams, out io.Writer) error {
		return svc.Restore(ctx, backup.AppdataRestoreRequest{Container: p.Container, Archive: p.Archive, DestinationID: p.DestinationID, Sharers: p.Sharers}, out)
	}))
	// A preview changes nothing, so a cancel is safe at any point.
	registry.Register(job.TypeAppdataRestorePreview, true, job.RunAppdataRestorePreview(func(ctx context.Context, id string, p job.AppdataRestoreParams, out io.Writer) error {
		return svc.RunPreview(ctx, id, backup.AppdataRestoreRequest{Container: p.Container, Archive: p.Archive, DestinationID: p.DestinationID}, out)
	}))
}

func publishAppdataFailure(ctx context.Context, notifier appdataPublisher, err error) {
	if notifier == nil {
		return
	}
	if perr := notifier.Publish(ctx, notify.EventAppdataBackupFailed, "Appdata backup failed", fmt.Sprintf("The appdata backup did not complete: %v", err)); perr != nil {
		log.Printf("hoservad: publishing the appdata backup failure: %v", perr)
	}
}

// scheduledAppdataBackup is the schedule's entry for the weekly appdata
// backup: it queues the same job POST /appdata/backup does. A backup that
// cannot even be queued (the array is stopped, maintenance mode) is a
// backup that did not happen, so it alerts like one that failed.
func scheduledAppdataBackup(svc *backup.AppdataService, scheduler *job.Scheduler, notifier appdataPublisher) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		if _, err := api.SubmitAppdataBackup(ctx, svc, scheduler, nil); err != nil {
			publishAppdataFailure(ctx, notifier, fmt.Errorf("the scheduled backup could not be started: %w", err))
			return err
		}
		return nil
	}
}

// wireAppdataSchedule makes the weekly appdata backup run from the
// schedule loop and has the loop start any container an interrupted backup
// left stopped. Nothing is wired without a service, so the schedule's
// appdata_backup window stays unclaimed.
func wireAppdataSchedule(r *scheduleRunner, svc *backup.AppdataService, scheduler *job.Scheduler, notifier appdataPublisher) {
	if svc == nil {
		return
	}
	if r.OtherJobs == nil {
		r.OtherJobs = map[string]func(context.Context) error{}
	}
	r.OtherJobs["appdata_backup"] = scheduledAppdataBackup(svc, scheduler, notifier)
	r.AppdataRecovery = svc
}
