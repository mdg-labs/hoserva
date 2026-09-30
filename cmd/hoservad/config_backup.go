package main

import (
	"context"
	"fmt"
	"log"

	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/notify"
)

// configBackupPublisher is the one notify.Service method the config backup
// failure alert needs; tests use a fake.
type configBackupPublisher interface {
	Publish(ctx context.Context, event notify.EventType, title, message string) error
}

// wireConfigBackup is what main.go calls to make POST /config/backup
// reachable: it registers the config_backup job the handler queues
// (Handler.Backup, wired by wireBackup, is what refuses a request while no
// destination is enabled). A backup that wrote nowhere publishes
// notify.EventConfigBackupFailed; a cancelled one alerts on nothing. A test
// calls it too, rather than repeating the registration. With no backup
// service nothing is registered, and the operation answers 501.
func wireConfigBackup(registry *job.Registry, svc *backup.Service, notifier configBackupPublisher) {
	if svc == nil {
		return
	}
	registry.Register(job.TypeConfigBackup, true, job.RunConfigBackup(job.ConfigBackupDeps{
		Backup: svc.RunConfigBackup,
		Failed: func(ctx context.Context, err error) { publishConfigBackupFailure(ctx, notifier, err) },
	}))
}

func publishConfigBackupFailure(ctx context.Context, notifier configBackupPublisher, err error) {
	if notifier == nil {
		return
	}
	if perr := notifier.Publish(ctx, notify.EventConfigBackupFailed, "Config backup failed", fmt.Sprintf("The config backup did not complete: %v", err)); perr != nil {
		log.Printf("hoservad: publishing the config backup failure: %v", perr)
	}
}
