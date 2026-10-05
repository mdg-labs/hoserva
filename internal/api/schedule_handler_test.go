package api_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

func newScheduleHandler(t *testing.T) (*api.Handler, *api.ScheduleService) {
	db := openTestDB(t)
	settingsStore := api.NewSettingsStore(db)
	scheduleStore := api.NewScheduleStore(db)
	svc := api.NewScheduleService(scheduleStore, settingsStore)
	h := &api.Handler{Schedules: svc, Settings: api.NewSettingsService(settingsStore, fakeSettingsCipher{})}
	return h, svc
}

func TestHandlerGetSchedulesSeedsDefaults(t *testing.T) {
	h, _ := newScheduleHandler(t)
	ctx := context.Background()

	got, err := h.GetSchedules(ctx)
	if err != nil {
		t.Fatalf("GetSchedules: %v", err)
	}
	if got.Chain.StartTime != job.DefaultChainStartTime {
		t.Errorf("chain.startTime = %q, want %q", got.Chain.StartTime, job.DefaultChainStartTime)
	}
	if len(got.Chain.Steps) != len(job.ChainOrder()) {
		t.Fatalf("len(chain.steps) = %d, want %d", len(got.Chain.Steps), len(job.ChainOrder()))
	}
	if len(got.OtherJobs) != 4 {
		t.Fatalf("len(otherJobs) = %d, want 4", len(got.OtherJobs))
	}
}

// The nextRun the API reports and the moment ClaimDueChain first claims the
// chain agree for a chain whose settings are created after its start time.
func TestClaimDueChain_NeverRunChainClaimsAtTheNextRunGetSchedulesReports(t *testing.T) {
	h, svc := newScheduleHandler(t)
	ctx := context.Background()
	clock := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	svc.Now = func() time.Time { return clock }

	got, err := h.GetSchedules(ctx)
	if err != nil {
		t.Fatalf("GetSchedules: %v", err)
	}
	next := got.Chain.NextRun
	if want := time.Date(2026, 6, 16, 2, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("chain.nextRun = %v, want %v", next, want)
	}

	clock = clock.Add(20 * time.Second)
	if claimed, err := svc.ClaimDueChain(ctx, time.Time{}); err != nil || claimed != nil {
		t.Fatalf("ClaimDueChain seconds after setup = %v, %v; want nil, nil", claimed, err)
	}
	clock = next.Add(-time.Second)
	if claimed, err := svc.ClaimDueChain(ctx, time.Time{}); err != nil || claimed != nil {
		t.Fatalf("ClaimDueChain just before nextRun = %v, %v; want nil, nil", claimed, err)
	}
	clock = next
	claimed, err := svc.ClaimDueChain(ctx, time.Time{})
	if err != nil || claimed == nil {
		t.Fatalf("ClaimDueChain at nextRun = %v, %v; want a claimed window", claimed, err)
	}
	if claimed, err := svc.ClaimDueChain(ctx, time.Time{}); err != nil || claimed != nil {
		t.Fatalf("second ClaimDueChain in the same window = %v, %v; want nil, nil", claimed, err)
	}
}

// A chain whose settings were created long before the caller could first run
// it is armed by the later instant: windows that passed in between are not
// owed, and the first window is the one nextRun reports when it becomes
// runnable. A chain that has run is not delayed by runnableSince.
func TestClaimDueChain_RunnableSinceHoldsBackWindowsThatPassedEarlier(t *testing.T) {
	h, svc := newScheduleHandler(t)
	ctx := context.Background()
	clock := time.Date(2026, 6, 13, 15, 0, 0, 0, time.UTC)
	svc.Now = func() time.Time { return clock }
	if _, err := h.GetSchedules(ctx); err != nil {
		t.Fatalf("GetSchedules: %v", err)
	}

	clock = time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	runnableSince := clock
	got, err := h.GetSchedules(ctx)
	if err != nil {
		t.Fatalf("GetSchedules: %v", err)
	}
	next := got.Chain.NextRun
	if want := time.Date(2026, 6, 16, 2, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("chain.nextRun = %v, want %v", next, want)
	}

	clock = clock.Add(20 * time.Second)
	if claimed, err := svc.ClaimDueChain(ctx, runnableSince); err != nil || claimed != nil {
		t.Fatalf("ClaimDueChain seconds after becoming runnable = %v, %v; want nil, nil", claimed, err)
	}
	clock = next
	claimed, err := svc.ClaimDueChain(ctx, runnableSince)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimDueChain at nextRun = %v, %v; want a claimed window", claimed, err)
	}

	clock = time.Date(2026, 6, 17, 2, 0, 0, 0, time.UTC)
	later := clock.Add(24 * time.Hour)
	claimed, err = svc.ClaimDueChain(ctx, later)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimDueChain of a chain that has run = %v, %v; want its window to open regardless of runnableSince", claimed, err)
	}
}

func TestHandlerUpdateMaintenanceChainSchedulePersistsDisabledMover(t *testing.T) {
	h, svc := newScheduleHandler(t)
	ctx := context.Background()

	_, err := h.UpdateMaintenanceChainSchedule(ctx, &apiv1.UpdateMaintenanceChainScheduleRequest{
		Steps: []apiv1.MaintenanceChainStep{
			{ID: apiv1.MaintenanceChainStepIdMover, Enabled: false},
		},
	})
	if err != nil {
		t.Fatalf("UpdateMaintenanceChainSchedule: %v", err)
	}

	enabled, err := svc.ChainEnabled(ctx)
	if err != nil {
		t.Fatalf("ChainEnabled: %v", err)
	}
	if enabled[job.StepMover] {
		t.Fatal("mover should be disabled after update")
	}
}

func TestHandlerUpdateScheduledJobPersistsFrequency(t *testing.T) {
	h, _ := newScheduleHandler(t)
	ctx := context.Background()

	got, err := h.UpdateScheduledJob(ctx, &apiv1.UpdateScheduledJobRequest{
		Frequency: apiv1.NewOptScheduleFrequency(apiv1.ScheduleFrequencyMonthly),
		Time:      apiv1.NewOptString("07:30"),
	}, apiv1.UpdateScheduledJobParams{JobId: apiv1.OtherScheduleJobIdSmartSelfTest})
	if err != nil {
		t.Fatalf("UpdateScheduledJob: %v", err)
	}
	var found bool
	for _, j := range got.OtherJobs {
		if j.ID != apiv1.OtherScheduleJobIdSmartSelfTest {
			continue
		}
		found = true
		if j.Frequency != apiv1.ScheduleFrequencyMonthly {
			t.Errorf("frequency = %s, want monthly", j.Frequency)
		}
		if j.Time != "07:30" {
			t.Errorf("time = %s, want 07:30", j.Time)
		}
	}
	if !found {
		t.Fatal("smart_self_test not returned in otherJobs")
	}
}

func TestEnsureDefaults_RepairsMissingDefaultJobs(t *testing.T) {
	db := openTestDB(t)
	s := api.NewScheduleStore(db)
	ctx := context.Background()
	now := "2026-01-01T00:00:00Z"

	if err := s.UpsertJob(ctx, api.ScheduleJobRow{
		JobID:     "smart_self_test",
		Enabled:   true,
		Frequency: "weekly",
		StartTime: "03:00",
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("UpsertJob: %v", err)
	}
	if err := s.EnsureDefaults(ctx, now); err != nil {
		t.Fatalf("EnsureDefaults: %v", err)
	}
	rows, err := s.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("len(jobs) = %d, want 4 default jobs after partial seed repair", len(rows))
	}
	ids := map[string]bool{}
	for _, row := range rows {
		ids[row.JobID] = true
	}
	for _, want := range []string{"smart_self_test", "appdata_backup", "restore_drill", "container_update_check"} {
		if !ids[want] {
			t.Errorf("missing default job %s after EnsureDefaults", want)
		}
	}
}

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := sql.Open("sqlite", store.DSN(dbPath))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: filepath.Join(dir, "snapshots")}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
