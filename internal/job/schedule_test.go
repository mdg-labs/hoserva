package job

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMaintenanceChain_SkipsPersistedDisabledMover(t *testing.T) {
	s := newTestScheduler(t)
	rec := &stepRecorder{}
	registerRecording(s, TypeSync, rec, "sync")

	chain := &MaintenanceChain{
		Scheduler: s,
		Guard:     &fakeGuard{},
		Enabled:   map[Step]bool{StepMover: false},
		Weekly:    false,
	}

	result, err := chain.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := rec.get(); len(got) != 1 || got[0] != "sync" {
		t.Fatalf("recorded steps = %v, want [sync] — disabled mover must be skipped", got)
	}
	if !result.Steps[0].Skipped {
		t.Error("mover step should be reported Skipped when disabled in Enabled map")
	}
}

func TestMaintenanceChain_SkipsUnregisteredMover(t *testing.T) {
	s := newTestScheduler(t)
	rec := &stepRecorder{}
	registerRecording(s, TypeSync, rec, "sync")

	chain := &MaintenanceChain{
		Scheduler: s,
		Guard:     &fakeGuard{},
		Weekly:    false,
	}

	result, err := chain.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v — unregistered mover must be skipped, not a chain failure", err)
	}
	if got := rec.get(); len(got) != 1 || got[0] != "sync" {
		t.Fatalf("recorded steps = %v, want [sync] — unregistered mover must not run and must not stop the chain", got)
	}
	if !result.Steps[0].Skipped {
		t.Error("mover step should be reported Skipped when TypeMover is unregistered")
	}
}

func TestMaintenanceChain_UnregisteredSyncFails(t *testing.T) {
	s := newTestScheduler(t)
	rec := &stepRecorder{}
	registerRecording(s, TypeMover, rec, "mover")

	chain := &MaintenanceChain{
		Scheduler: s,
		Guard:     &fakeGuard{},
		Weekly:    false,
	}

	_, err := chain.Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded with TypeSync unregistered; the chain must fail rather than skip parity")
	}
	if !errors.Is(err, ErrJobTypeNotRegistered) {
		t.Fatalf("Run = %v, want ErrJobTypeNotRegistered", err)
	}
}

func TestMaintenanceChain_UnregisteredScrubFailsOnWeeklyRun(t *testing.T) {
	s := newTestScheduler(t)
	rec := &stepRecorder{}
	registerRecording(s, TypeMover, rec, "mover")
	registerRecording(s, TypeSync, rec, "sync")

	chain := &MaintenanceChain{
		Scheduler: s,
		Guard:     &fakeGuard{},
		Weekly:    true,
	}

	_, err := chain.Run(context.Background())
	if err == nil {
		t.Fatal("weekly Run succeeded with TypeScrub unregistered; the chain must fail rather than skip scrub")
	}
	if !errors.Is(err, ErrJobTypeNotRegistered) {
		t.Fatalf("Run = %v, want ErrJobTypeNotRegistered", err)
	}
}

func TestChainIsDue_UsesInstallationTimezone(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	// 01:30 UTC is 03:30 CEST on 2026-06-15 — after 02:00 Berlin.
	after := time.Date(2026, 6, 15, 1, 30, 0, 0, time.UTC)
	if !ChainIsDue(after, loc, "02:00", nil) {
		t.Fatal("ChainIsDue = false at 03:30 Berlin; today's 02:00 window should be due")
	}
	// 23:30 UTC on the 14th is 01:30 CEST on the 15th — before 02:00 Berlin.
	before := time.Date(2026, 6, 14, 23, 30, 0, 0, time.UTC)
	if ChainIsDue(before, loc, "02:00", nil) {
		t.Fatal("ChainIsDue = true at 01:30 Berlin; today's 02:00 window is not due yet")
	}
}

func TestChainIsDue_LastRunConsumesTodaysWindow(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 6, 15, 3, 0, 0, 0, loc)
	claimed := time.Date(2026, 6, 15, 2, 0, 1, 0, loc)
	if ChainIsDue(now, loc, "02:00", &claimed) {
		t.Fatal("ChainIsDue = true after last-run claimed today's window")
	}
	yesterday := time.Date(2026, 6, 14, 2, 0, 1, 0, loc)
	if !ChainIsDue(now, loc, "02:00", &yesterday) {
		t.Fatal("ChainIsDue = false with last-run yesterday; today's window is still open")
	}
}

func TestScheduleConflict_ParityOverlapReportsConflict(t *testing.T) {
	base := time.Date(2026, 1, 1, 2, 0, 0, 0, time.UTC)
	windows := []NamedWindow{
		{JobID: "maintenance_chain", Window: ScheduledWindow{Class: ClassParity, Start: base, Duration: 2 * time.Hour}},
		{JobID: "parity_job_b", Window: ScheduledWindow{Class: ClassParity, Start: base.Add(30 * time.Minute), Duration: time.Hour}},
	}
	conflicts := DetectScheduleConflicts(windows)
	if len(conflicts) != 1 {
		t.Fatalf("conflicts = %v, want one parity overlap", conflicts)
	}
	if conflicts[0].JobA != "maintenance_chain" || conflicts[0].JobB != "parity_job_b" {
		t.Fatalf("conflicts[0] = %+v, want maintenance_chain vs parity_job_b", conflicts[0])
	}
}

func TestScheduleConflict_ServiceAndParityDoNotConflict(t *testing.T) {
	base := time.Date(2026, 1, 1, 2, 0, 0, 0, time.UTC)
	chain := DefaultChainSettings()
	others := []OtherJobSettings{
		{ID: "appdata_backup", Enabled: true, Frequency: FrequencyDaily, Time: "02:30"},
	}
	now := base.Add(-time.Hour)
	loc := time.UTC
	windows := BuildScheduleWindows(now, loc, chain, others)
	var serviceParity bool
	for _, c := range DetectScheduleConflicts(windows) {
		if (c.JobA == MaintenanceChainJobID && c.JobB == "appdata_backup") ||
			(c.JobB == MaintenanceChainJobID && c.JobA == "appdata_backup") {
			serviceParity = true
		}
	}
	if serviceParity {
		t.Fatal("service appdata_backup overlapping maintenance_chain parity window must not conflict")
	}
}

func TestNextChainRun_UsesLocalTimezone(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	now := time.Date(2026, 6, 15, 1, 0, 0, 0, loc)
	next := NextChainRun(now, loc, "02:00")
	want := time.Date(2026, 6, 15, 2, 0, 0, 0, loc).UTC()
	if !next.Equal(want) {
		t.Fatalf("NextChainRun = %v, want %v", next, want)
	}
}

func TestNextOtherJobRun_WeeklyUsesSundayAcrossWeekdays(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	job := OtherJobSettings{ID: "smart_self_test", Frequency: FrequencyWeekly, Time: "03:00"}
	want := time.Date(2026, 6, 21, 3, 0, 0, 0, loc).UTC() // Sunday
	for _, now := range []time.Time{
		time.Date(2026, 6, 16, 1, 0, 0, 0, loc), // Tuesday
		time.Date(2026, 6, 17, 4, 0, 0, 0, loc), // Wednesday after 03:00
		time.Date(2026, 6, 20, 2, 0, 0, 0, loc), // Saturday
	} {
		got := NextOtherJobRun(now, loc, job)
		if !got.Equal(want) {
			t.Errorf("NextOtherJobRun(%v) = %v, want %v", now, got, want)
		}
	}
}

func TestNextOtherJobRun_MonthlyUsesFirstOfMonth(t *testing.T) {
	loc := time.UTC
	job := OtherJobSettings{ID: "restore_drill", Frequency: FrequencyMonthly, Time: "05:00"}
	got := NextOtherJobRun(time.Date(2026, 6, 15, 1, 0, 0, 0, loc), loc, job)
	want := time.Date(2026, 7, 1, 5, 0, 0, 0, loc).UTC()
	if !got.Equal(want) {
		t.Fatalf("NextOtherJobRun mid-month = %v, want %v", got, want)
	}
	got = NextOtherJobRun(time.Date(2026, 7, 1, 4, 0, 0, 0, loc), loc, job)
	if !got.Equal(want) {
		t.Fatalf("NextOtherJobRun before time on the 1st = %v, want %v", got, want)
	}
	got = NextOtherJobRun(time.Date(2026, 7, 1, 6, 0, 0, 0, loc), loc, job)
	want = time.Date(2026, 8, 1, 5, 0, 0, 0, loc).UTC()
	if !got.Equal(want) {
		t.Fatalf("NextOtherJobRun after time on the 1st = %v, want %v", got, want)
	}
}

func TestChainWindows_ScrubWeekdayUsesInstallationTimezone(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	// Sunday 02:00 JST is Saturday 17:00 UTC. Scrub must still run.
	start := time.Date(2026, 6, 14, 2, 0, 0, 0, loc).UTC()
	windows := ChainWindows(start, loc, int(time.Sunday), nil)
	var scrub bool
	for _, w := range windows {
		if w.Window.Class == ClassParity && w.Window.Duration == stepDurationEstimates[StepScrub] {
			scrub = true
		}
	}
	if !scrub {
		t.Fatal("ChainWindows omitted scrub on Sunday in Asia/Tokyo because start was UTC")
	}
}
