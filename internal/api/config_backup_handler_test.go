package api_test

import (
	"context"
	"io"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/job"
)

func configBackupService(t *testing.T, enabled bool) *backup.Service {
	t.Helper()
	store := &backup.FakeDestinationStore{}
	if err := store.CreateDestination(context.Background(), backup.Destination{
		ID: "pool", Name: "Pool", Type: backup.TypeLocal, Path: t.TempDir(), Enabled: enabled,
		Retention: backup.Retention{Daily: 7},
	}); err != nil {
		t.Fatal(err)
	}
	return &backup.Service{Store: store}
}

func TestHandler_RunConfigBackup_NotConfiguredIs501(t *testing.T) {
	h, _, _ := newTestHandler(t)
	_, err := h.RunConfigBackup(context.Background())
	if ae := apiError(t, h, err); ae.StatusCode != 501 || ae.Response.Code != "not_configured" {
		t.Fatalf("status = %d %q, want 501 not_configured", ae.StatusCode, ae.Response.Code)
	}
}

func TestHandler_RunConfigBackup_WithNoEnabledDestinationIs409AndQueuesNothing(t *testing.T) {
	h, _, reg := newTestHandler(t)
	ctx := context.Background()
	h.Backup = configBackupService(t, false)
	reg.Register(job.TypeConfigBackup, true, job.RunConfigBackup(job.ConfigBackupDeps{
		Backup: func(context.Context, io.Writer) error { return nil },
	}))

	_, err := h.RunConfigBackup(ctx)
	if ae := apiError(t, h, err); ae.StatusCode != 409 || ae.Response.Code != "backup_no_destination" {
		t.Fatalf("status = %d %q, want 409 backup_no_destination", ae.StatusCode, ae.Response.Code)
	}
	jobs, err := h.Store.List(ctx, job.ListFilter{Limit: 10})
	if err != nil || len(jobs) != 0 {
		t.Fatalf("jobs = %v, %v; want none queued by a refused request", jobs, err)
	}
}

func TestHandler_RunConfigBackup_QueuesAServiceJobBehindAnotherConfigBackup(t *testing.T) {
	h, sched, reg := newTestHandler(t)
	ctx := context.Background()
	h.Backup = configBackupService(t, true)
	gate := make(chan struct{})
	started := make(chan struct{}, 2)
	reg.Register(job.TypeConfigBackup, true, job.RunConfigBackup(job.ConfigBackupDeps{
		Backup: func(context.Context, io.Writer) error {
			started <- struct{}{}
			<-gate
			return nil
		},
	}))

	first, err := h.RunConfigBackup(ctx)
	if err != nil {
		t.Fatalf("RunConfigBackup: %v", err)
	}
	if first.Type != apiv1.JobTypeConfigBackup || first.Class != apiv1.JobClassService {
		t.Fatalf("job = %s/%s, want config_backup in the service class", first.Type, first.Class)
	}
	<-started
	second, err := h.RunConfigBackup(ctx)
	if err != nil {
		t.Fatalf("a second config backup was refused instead of queued: %v", err)
	}
	if got, _ := h.Store.Get(ctx, second.ID.String()); got == nil || got.Status != job.StatusQueued {
		t.Fatalf("second config backup = %+v, want it queued behind the first", got)
	}
	close(gate)
	for _, j := range []*apiv1.Job{first, second} {
		if done := awaitJob(t, sched, j.ID.String()); done.Status != job.StatusSucceeded {
			t.Fatalf("config backup %s = %s %s", j.ID, done.Status, done.ErrorMessage)
		}
	}
}
