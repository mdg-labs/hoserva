package api_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/job"
)

func newClaimService(t *testing.T, now *time.Time) (*api.ScheduleService, *api.ScheduleStore) {
	t.Helper()
	db := openTestDB(t)
	store := api.NewScheduleStore(db)
	svc := api.NewScheduleService(store, api.NewSettingsStore(db))
	svc.Now = func() time.Time { return *now }
	return svc, store
}

func TestClaimDueOtherJobs_AppdataBackupIsSeededWeeklyAndClaimedOncePerWindow(t *testing.T) {
	ctx := context.Background()
	// 2026-09-29 is a Tuesday; the next Sunday is 2026-10-04.
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	svc, store := newClaimService(t, &now)
	ids := []string{"appdata_backup"}

	got, err := svc.ClaimDueOtherJobs(ctx, ids)
	if err != nil || len(got) != 0 {
		t.Fatalf("on the day it was seeded: claimed %v, err %v; want nothing until the first window", got, err)
	}
	rows, _ := store.ListJobs(ctx)
	for _, r := range rows {
		if r.JobID == "appdata_backup" && (!r.Enabled || r.Frequency != "weekly") {
			t.Fatalf("seeded appdata_backup = %+v, want enabled and weekly", r)
		}
	}

	now = time.Date(2026, 10, 4, 3, 59, 0, 0, time.UTC)
	if got, _ := svc.ClaimDueOtherJobs(ctx, ids); len(got) != 0 {
		t.Fatalf("a minute before the window: claimed %v", got)
	}
	now = time.Date(2026, 10, 4, 4, 1, 0, 0, time.UTC)
	got, err = svc.ClaimDueOtherJobs(ctx, ids)
	if err != nil || len(got) != 1 || got[0] != "appdata_backup" {
		t.Fatalf("in the window: claimed %v, err %v; want appdata_backup", got, err)
	}
	if got, _ := svc.ClaimDueOtherJobs(ctx, ids); len(got) != 0 {
		t.Fatalf("the same window claimed twice: %v", got)
	}
	now = time.Date(2026, 10, 4, 23, 0, 0, 0, time.UTC)
	if got, _ := svc.ClaimDueOtherJobs(ctx, ids); len(got) != 0 {
		t.Fatalf("later the same Sunday: claimed %v", got)
	}
	now = time.Date(2026, 10, 11, 4, 0, 0, 0, time.UTC)
	if got, _ := svc.ClaimDueOtherJobs(ctx, ids); len(got) != 1 {
		t.Fatalf("the next Sunday: claimed %v, want one", got)
	}
}

func TestClaimDueOtherJobs_OnlyClaimsJobsTheCallerCanRun(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	svc, store := newClaimService(t, &now)
	if _, err := svc.ClaimDueOtherJobs(ctx, nil); err != nil {
		t.Fatal(err)
	}
	// Both the weekly appdata backup and the monthly restore drill have a
	// window that has opened by now; only the one the caller names is
	// claimed.
	now = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	if got, err := svc.ClaimDueOtherJobs(ctx, []string{"appdata_backup"}); err != nil || len(got) != 1 || got[0] != "appdata_backup" {
		t.Fatalf("claimed %v, err %v; want only appdata_backup", got, err)
	}
	rows, _ := store.ListJobs(ctx)
	for _, r := range rows {
		if r.JobID != "appdata_backup" && r.LastRunAt != "" {
			t.Fatalf("%s has last_run_at %q although the caller could not run it", r.JobID, r.LastRunAt)
		}
	}
}

func TestClaimDueOtherJobs_RestoreDrillIsSeededMonthlyAndClaimedOncePerWindow(t *testing.T) {
	ctx := context.Background()
	// 2026-09-29: the next monthly window is 2026-10-01 05:00.
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	svc, store := newClaimService(t, &now)
	ids := []string{"restore_drill"}

	if got, err := svc.ClaimDueOtherJobs(ctx, ids); err != nil || len(got) != 0 {
		t.Fatalf("on the day it was seeded: claimed %v, err %v; want nothing until the first window", got, err)
	}
	rows, _ := store.ListJobs(ctx)
	for _, r := range rows {
		if r.JobID == "restore_drill" && (!r.Enabled || r.Frequency != "monthly" || r.StartTime != "05:00") {
			t.Fatalf("seeded restore_drill = %+v, want enabled, monthly at 05:00 (doc 10 §4)", r)
		}
	}
	now = time.Date(2026, 10, 1, 5, 1, 0, 0, time.UTC)
	if got, err := svc.ClaimDueOtherJobs(ctx, ids); err != nil || len(got) != 1 {
		t.Fatalf("in the window: claimed %v, err %v; want restore_drill", got, err)
	}
	if got, _ := svc.ClaimDueOtherJobs(ctx, ids); len(got) != 0 {
		t.Fatalf("the same window claimed twice: %v", got)
	}
	now = time.Date(2026, 10, 20, 5, 0, 0, 0, time.UTC)
	if got, _ := svc.ClaimDueOtherJobs(ctx, ids); len(got) != 0 {
		t.Fatalf("mid-month: claimed %v", got)
	}
	now = time.Date(2026, 11, 1, 5, 0, 0, 0, time.UTC)
	if got, _ := svc.ClaimDueOtherJobs(ctx, ids); len(got) != 1 {
		t.Fatalf("the next month: claimed %v, want one", got)
	}
}

func TestClaimDueOtherJobs_ASavedScheduleWaitsForItsNextWindow(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	svc, _ := newClaimService(t, &now)
	if _, err := svc.ClaimDueOtherJobs(ctx, nil); err != nil {
		t.Fatal(err)
	}
	// Sunday evening: the 04:00 window has passed and was never claimed
	// (say, the daemon was down). Editing the schedule now must not fire
	// that stale window.
	now = time.Date(2026, 10, 4, 20, 0, 0, 0, time.UTC)
	daily := job.FrequencyDaily
	if _, err := svc.UpdateOtherJob(ctx, "appdata_backup", api.UpdateOtherJobInput{Frequency: &daily}); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.ClaimDueOtherJobs(ctx, []string{"appdata_backup"}); len(got) != 0 {
		t.Fatalf("claimed %v right after saving the schedule", got)
	}
	now = time.Date(2026, 10, 5, 4, 0, 0, 0, time.UTC)
	if got, _ := svc.ClaimDueOtherJobs(ctx, []string{"appdata_backup"}); len(got) != 1 {
		t.Fatalf("the next daily window: claimed %v, want one", got)
	}
}

func TestClaimDueOtherJobs_TwoDaemonsClaimAWindowOnce(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	db := openTestDB(t)
	store := api.NewScheduleStore(db)
	settings := api.NewSettingsStore(db)
	a, b := api.NewScheduleService(store, settings), api.NewScheduleService(store, settings)
	a.Now, b.Now = func() time.Time { return now }, func() time.Time { return now }
	if _, err := a.ClaimDueOtherJobs(ctx, nil); err != nil {
		t.Fatal(err)
	}
	now = time.Date(2026, 10, 4, 5, 0, 0, 0, time.UTC)

	var wg sync.WaitGroup
	var mu sync.Mutex
	total := 0
	for _, svc := range []*api.ScheduleService{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := svc.ClaimDueOtherJobs(ctx, []string{"appdata_backup"})
			if err != nil {
				t.Errorf("ClaimDueOtherJobs: %v", err)
			}
			mu.Lock()
			total += len(got)
			mu.Unlock()
		}()
	}
	wg.Wait()
	if total != 1 {
		t.Fatalf("the window was claimed %d times, want exactly once", total)
	}
}

func TestAppdataPolicyStore_RoundTrip(t *testing.T) {
	ctx := context.Background()
	s := api.NewAppdataPolicyStore(openTestDB(t))
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	if got, err := s.ListAppdataPolicies(ctx); err != nil || len(got) != 0 {
		t.Fatalf("fresh store = %v, %v", got, err)
	}
	for _, p := range []backup.AppdataPolicy{
		{Container: "db", Stop: false, Included: true},
		{Container: "web", Stop: true, Included: false},
		{Container: "db", Stop: true, Included: true},
	} {
		if err := s.SetAppdataPolicy(ctx, p, at); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListAppdataPolicies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []backup.AppdataPolicy{{Container: "db", Stop: true, Included: true}, {Container: "web", Stop: true, Included: false}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("policies = %v, want %v", got, want)
	}
}
