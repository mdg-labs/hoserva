package api_test

import (
	"context"
	"io"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/job"
)

func TestDrillStore_RoundTripKeepsOnlyTheLastResult(t *testing.T) {
	ctx := context.Background()
	s := api.NewDrillStore(openTestDB(t))
	if got, err := s.LastDrill(ctx); err != nil || got != nil {
		t.Fatalf("fresh store = %v, %v; want nil, nil", got, err)
	}
	first := backup.DrillResult{RanAt: time.Date(2026, 10, 1, 5, 0, 0, 0, time.UTC), Passed: true,
		Destinations: []backup.DrillDestination{{DestinationID: "boot", DestinationName: "Boot", Archive: "a.tar.zst", Passed: true}}}
	if err := s.RecordDrill(ctx, first); err != nil {
		t.Fatal(err)
	}
	failed := backup.DrillResult{RanAt: time.Date(2026, 11, 1, 5, 0, 0, 0, time.UTC), Error: "no backup destination is enabled",
		Destinations: []backup.DrillDestination{
			{DestinationID: "boot", DestinationName: "Boot", Archive: "b.tar.zst", Passed: true},
			{DestinationID: "pool", DestinationName: "Pool", Error: "the destination holds no config archive"},
		}}
	if err := s.RecordDrill(ctx, failed); err != nil {
		t.Fatal(err)
	}
	got, err := s.LastDrill(ctx)
	if err != nil || got == nil {
		t.Fatalf("LastDrill = %v, %v", got, err)
	}
	if !got.RanAt.Equal(failed.RanAt) || got.Passed || got.Error != failed.Error || len(got.Destinations) != 2 ||
		got.Destinations[0] != failed.Destinations[0] || got.Destinations[1] != failed.Destinations[1] {
		t.Fatalf("LastDrill = %+v, want %+v", got, failed)
	}
}

func TestHandler_RestoreDrillOperations_Return501WithoutABackupService(t *testing.T) {
	h, _, _ := newTestHandler(t)
	ctx := context.Background()
	if _, err := h.GetRestoreDrill(ctx); err == nil {
		t.Fatal("GetRestoreDrill = nil without a backup service")
	} else if st, code := statusOf(h, err); st != 501 || code != "not_configured" {
		t.Fatalf("GetRestoreDrill = %d %s, want 501 not_configured", st, code)
	}
	if _, err := h.StartRestoreDrill(ctx); err == nil {
		t.Fatal("StartRestoreDrill = nil without a backup service")
	} else if st, _ := statusOf(h, err); st != 501 {
		t.Fatalf("StartRestoreDrill = %d, want 501", st)
	}
}

func TestHandler_GetRestoreDrill_ReportsTheLastResult(t *testing.T) {
	h, _, _ := newTestHandler(t)
	ctx := context.Background()
	drills := &backup.FakeDrillStore{}
	h.Backup = &backup.Service{Drills: drills}

	got, err := h.GetRestoreDrill(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.LastRun.Get(); ok {
		t.Fatal("lastRun present before any drill ran")
	}

	ranAt := time.Date(2026, 10, 1, 5, 0, 0, 0, time.UTC)
	if err := drills.RecordDrill(ctx, backup.DrillResult{RanAt: ranAt, Destinations: []backup.DrillDestination{
		{DestinationID: "boot", DestinationName: "Boot", Archive: "a.tar.zst", Passed: true},
		{DestinationID: "pool", DestinationName: "Pool", Error: "no archive"},
	}}); err != nil {
		t.Fatal(err)
	}
	got, err = h.GetRestoreDrill(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, ok := got.LastRun.Get()
	if !ok || run.Passed || !run.RanAt.Equal(ranAt) || len(run.Destinations) != 2 {
		t.Fatalf("lastRun = %+v, %v", run, ok)
	}
	if a, _ := run.Destinations[0].Archive.Get(); a != "a.tar.zst" || !run.Destinations[0].Passed {
		t.Fatalf("destination 0 = %+v", run.Destinations[0])
	}
	if _, has := run.Destinations[1].Archive.Get(); has {
		t.Fatalf("a destination with no archive reported one: %+v", run.Destinations[1])
	}
	if e, _ := run.Destinations[1].Error.Get(); e != "no archive" {
		t.Fatalf("destination 1 = %+v", run.Destinations[1])
	}
}

func TestHandler_StartRestoreDrill_QueuesAServiceJobBehindAnotherDrill(t *testing.T) {
	h, sched, reg := newTestHandler(t)
	ctx := context.Background()
	h.Backup = &backup.Service{Drills: &backup.FakeDrillStore{}}
	gate := make(chan struct{})
	started := make(chan struct{}, 2)
	reg.Register(job.TypeRestoreDrill, true, job.RunRestoreDrill(func(context.Context, io.Writer) error {
		started <- struct{}{}
		<-gate
		return nil
	}))

	first, err := h.StartRestoreDrill(ctx)
	if err != nil {
		t.Fatalf("StartRestoreDrill: %v", err)
	}
	if first.Type != apiv1.JobTypeRestoreDrill || first.Class != apiv1.JobClassService {
		t.Fatalf("job = %s/%s, want restore_drill in the service class", first.Type, first.Class)
	}
	<-started
	second, err := h.StartRestoreDrill(ctx)
	if err != nil {
		t.Fatalf("a second drill was refused instead of queued: %v", err)
	}
	if got, _ := h.Store.Get(ctx, second.ID.String()); got == nil || got.Status != job.StatusQueued {
		t.Fatalf("second drill = %+v, want it queued behind the first", got)
	}
	close(gate)
	for _, j := range []*apiv1.Job{first, second} {
		if done := awaitJob(t, sched, j.ID.String()); done.Status != job.StatusSucceeded {
			t.Fatalf("drill %s = %s %s", j.ID, done.Status, done.ErrorMessage)
		}
	}
}
