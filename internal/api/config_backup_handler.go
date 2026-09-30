package api

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/job"
)

// SubmitConfigBackup queues a config_backup job behind any other one. It
// refuses, before anything is queued, while no destination is enabled.
func SubmitConfigBackup(ctx context.Context, svc *backup.Service, sched *job.Scheduler) (*job.Job, error) {
	if svc == nil {
		return nil, &apiError{code: "not_configured", statusCode: 501, message: "config backups are not configured on this daemon"}
	}
	if sched == nil {
		return nil, fmt.Errorf("job scheduler not configured")
	}
	if err := svc.RequireEnabledDestination(ctx); err != nil {
		return nil, mapBackupDestinationError(err)
	}
	j, err := sched.Submit(ctx, job.TypeConfigBackup, []string{backup.ConfigBackupJobResource}, nil)
	if err != nil {
		return nil, mapSchedulerError(uuid.Nil, err)
	}
	return j, nil
}

func (h *Handler) RunConfigBackup(ctx context.Context) (*apiv1.Job, error) {
	j, err := SubmitConfigBackup(ctx, h.Backup, h.Scheduler)
	if err != nil {
		return nil, err
	}
	return jobToAPI(j)
}
