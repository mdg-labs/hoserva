package job

import (
	"context"
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
