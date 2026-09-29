package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/notify"
)

// drillBackupService is a backup service over the test's real database
// with one local destination, and the archive a backup wrote to it.
func drillBackupService(t *testing.T, db *sql.DB, root string, at time.Time) (svc *backup.Service, dest, archive string) {
	t.Helper()
	dest = filepath.Join(root, "drill-backups")
	dests := &backup.FakeDestinationStore{}
	if err := dests.CreateDestination(context.Background(), backup.Destination{
		ID: backup.DefaultPoolID, Name: "Pool", Type: backup.TypeLocal, Path: dest, Enabled: true,
		Retention: backup.Retention{Daily: 7, Weekly: 4, Monthly: 6},
	}); err != nil {
		t.Fatal(err)
	}
	svc = &backup.Service{DB: db, Paths: backup.Paths{DBPath: filepath.Join(root, "hoservad.db")}, Store: dests, Hostname: "test-host",
		Now: func() time.Time { return at }}
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("writing the archive the drill will test: %v", err)
	}
	entries, err := os.ReadDir(dest)
	if err != nil || len(entries) != 1 {
		t.Fatalf("destination = %v, %v; want one archive", entries, err)
	}
	return svc, dest, entries[0].Name()
}

func corruptArchive(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data[:len(data)/2], 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDrillWiring_OperationsAnswer501WhenNothingIsWired(t *testing.T) {
	w := newContainersWiringHarness(t)
	if status, body := w.do(t, http.MethodGet, "/backup/drill"); status != http.StatusNotImplemented {
		t.Fatalf("GET /backup/drill without wiring = %d %s, want 501", status, body)
	}
	if status, body := w.doBody(t, http.MethodPost, "/backup/drill", ``); status != http.StatusNotImplemented {
		t.Fatalf("POST /backup/drill without wiring = %d %s, want 501", status, body)
	}
}

// POST /backup/drill reaches the drill through the real daemon server and
// its job registry, GET /backup/drill reads the persisted result, and a
// failed drill publishes the notification.
func TestDrillWiring_RunNowIsReachableOverHTTPAndAFailureAlerts(t *testing.T) {
	w := newContainersWiringHarness(t)
	svc, dest, archive := drillBackupService(t, w.db, w.root, time.Date(2026, 9, 29, 3, 0, 0, 0, time.UTC))
	pub := &recordingPublisher{}
	wireBackup(w.handler, svc)
	wireRestoreDrill(w.registry, svc, api.NewDrillStore(w.db), pub)

	status, body := w.do(t, http.MethodGet, "/backup/drill")
	if status != http.StatusOK || bytes.Contains(body, []byte("lastRun")) {
		t.Fatalf("GET before any drill = %d %s, want 200 without lastRun", status, body)
	}

	run := func() *job.Job {
		status, body := w.doBody(t, http.MethodPost, "/backup/drill", ``)
		if status != http.StatusOK {
			t.Fatalf("POST /backup/drill = %d %s", status, body)
		}
		var queued struct {
			ID    string `json:"id"`
			Type  string `json:"type"`
			Class string `json:"class"`
		}
		if err := json.Unmarshal(body, &queued); err != nil || queued.Type != "restore_drill" || queued.Class != "service" {
			t.Fatalf("job = %s (%v), want a restore_drill service job", body, err)
		}
		return w.awaitJobByID(t, queued.ID)
	}
	lastRun := func() (passed bool, archive string) {
		status, body := w.do(t, http.MethodGet, "/backup/drill")
		if status != http.StatusOK {
			t.Fatalf("GET /backup/drill = %d %s", status, body)
		}
		var got struct {
			LastRun struct {
				Passed       bool `json:"passed"`
				Destinations []struct {
					Archive string `json:"archive"`
				} `json:"destinations"`
			} `json:"lastRun"`
		}
		if err := json.Unmarshal(body, &got); err != nil || len(got.LastRun.Destinations) != 1 {
			t.Fatalf("GET /backup/drill = %s (%v)", body, err)
		}
		return got.LastRun.Passed, got.LastRun.Destinations[0].Archive
	}

	if done := run(); done.Status != job.StatusSucceeded {
		t.Fatalf("drill job = %s %s", done.Status, done.ErrorMessage)
	}
	if passed, tested := lastRun(); !passed || tested != archive {
		t.Fatalf("last run = passed %v on %q, want a pass on %q", passed, tested, archive)
	}
	if n := pub.count(notify.EventRestoreDrillFailed); n != 0 {
		t.Fatalf("restore_drill_failed published %d times for a passing drill", n)
	}

	corruptArchive(t, filepath.Join(dest, archive))
	if done := run(); done.Status != job.StatusFailed {
		t.Fatalf("drill job over a corrupt archive = %s, want failed", done.Status)
	}
	if passed, _ := lastRun(); passed {
		t.Fatal("the last run still reads as passed after a failed drill")
	}
	if n := pub.count(notify.EventRestoreDrillFailed); n != 1 {
		t.Fatalf("restore_drill_failed published %d times, want once", n)
	}
}

type scheduledDrill struct {
	h       *scheduleHarness
	svc     *backup.Service
	pub     *recordingPublisher
	dest    string
	archive string
	clock   *time.Time
}

func newScheduledDrill(t *testing.T) *scheduledDrill {
	t.Helper()
	// 2026-09-29 is a Tuesday; the monthly window opens 2026-10-01 05:00.
	clock := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	h := newScheduleHarness(t, func() time.Time { return clock }, nil)
	root := t.TempDir()
	svc, dest, archive := drillBackupService(t, h.db, root, clock)
	pub := &recordingPublisher{}
	wireRestoreDrill(h.registry, svc, api.NewDrillStore(h.db), pub)
	wireRestoreDrillSchedule(h.runner, svc, h.runner.Scheduler, pub)
	t.Cleanup(func() {
		dctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = h.runner.Scheduler.Drain(dctx)
	})
	return &scheduledDrill{h: h, svc: svc, pub: pub, dest: dest, archive: archive, clock: &clock}
}

func (s *scheduledDrill) drillJobs(t *testing.T) []*job.Job {
	t.Helper()
	jobs, err := s.h.jobs.List(context.Background(), job.ListFilter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var out []*job.Job
	for _, j := range jobs {
		if j.Type == job.TypeRestoreDrill {
			out = append(out, j)
		}
	}
	return out
}

func TestScheduleTick_RunsTheMonthlyRestoreDrillOncePerWindow(t *testing.T) {
	ctx := context.Background()
	s := newScheduledDrill(t)

	if err := s.h.runner.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if jobs := s.drillJobs(t); len(jobs) != 0 {
		t.Fatalf("a drill started on the day the schedule was seeded: %v", jobs)
	}

	*s.clock = time.Date(2026, 10, 1, 5, 1, 0, 0, time.UTC)
	if err := s.h.runner.tick(ctx); err != nil {
		t.Fatalf("tick in the window: %v", err)
	}
	jobs := s.drillJobs(t)
	if len(jobs) != 1 {
		t.Fatalf("drill jobs after the window opened = %v, want one", jobs)
	}
	if done := awaitTerminal(t, s.h.jobs, jobs[0].ID); done.Status != job.StatusSucceeded {
		t.Fatalf("scheduled drill = %s %s", done.Status, done.ErrorMessage)
	}
	last, err := s.svc.LastDrill(ctx)
	if err != nil || last == nil || !last.Passed || last.Destinations[0].Archive != s.archive {
		t.Fatalf("last drill = %+v, %v; want a pass on %s", last, err, s.archive)
	}

	for i := 0; i < 2; i++ {
		if err := s.h.runner.tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(s.drillJobs(t)); got != 1 {
		t.Fatalf("drill jobs after more ticks in the same window = %d, want still one", got)
	}

	*s.clock = time.Date(2026, 11, 1, 5, 0, 0, 0, time.UTC)
	if err := s.h.runner.tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(s.drillJobs(t)); got != 2 {
		t.Fatalf("drill jobs after the next month = %d, want two", got)
	}
}

func TestScheduleTick_AScheduledDrillOverACorruptArchiveFailsRecordsAndAlerts(t *testing.T) {
	ctx := context.Background()
	s := newScheduledDrill(t)
	corruptArchive(t, filepath.Join(s.dest, s.archive))
	if err := s.h.runner.tick(ctx); err != nil {
		t.Fatal(err)
	}
	*s.clock = time.Date(2026, 10, 1, 5, 1, 0, 0, time.UTC)
	if err := s.h.runner.tick(ctx); err != nil {
		t.Fatal(err)
	}
	jobs := s.drillJobs(t)
	if len(jobs) != 1 {
		t.Fatalf("drill jobs = %v, want one", jobs)
	}
	if done := awaitTerminal(t, s.h.jobs, jobs[0].ID); done.Status != job.StatusFailed {
		t.Fatalf("drill = %s, want failed", done.Status)
	}
	if last, _ := s.svc.LastDrill(ctx); last == nil || last.Passed {
		t.Fatalf("last drill = %+v, want the failure recorded", last)
	}
	if n := s.pub.count(notify.EventRestoreDrillFailed); n != 1 {
		t.Fatalf("restore_drill_failed published %d times, want once", n)
	}
}

func TestScheduleTick_ADrillThatCannotBeQueuedIsRecordedAlertedAndNotRetriedEveryMinute(t *testing.T) {
	ctx := context.Background()
	s := newScheduledDrill(t)
	if err := s.h.runner.tick(ctx); err != nil {
		t.Fatalf("seeding tick: %v", err)
	}
	*s.clock = time.Date(2026, 10, 1, 5, 1, 0, 0, time.UTC)
	if err := s.h.runner.Scheduler.EnterMaintenance(ctx); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}

	if err := s.h.runner.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := len(s.drillJobs(t)); got != 0 {
		t.Fatalf("a job was queued in maintenance mode: %d", got)
	}
	if n := s.pub.count(notify.EventRestoreDrillFailed); n != 1 {
		t.Fatalf("restore_drill_failed published %d times, want once for the drill that never ran", n)
	}
	if last, _ := s.svc.LastDrill(ctx); last == nil || last.Passed || last.Error == "" {
		t.Fatalf("last drill = %+v, want the unstarted drill recorded as failed", last)
	}
	if err := s.h.runner.tick(ctx); err != nil {
		t.Fatal(err)
	}
	if n := s.pub.count(notify.EventRestoreDrillFailed); n != 1 {
		t.Fatalf("the same window alerted %d times, want once", n)
	}
}

func TestWireRestoreDrill_WithoutABackupServiceRegistersNothing(t *testing.T) {
	registry := job.NewRegistry()
	wireRestoreDrill(registry, nil, nil, &recordingPublisher{})
	runner := &scheduleRunner{}
	wireRestoreDrillSchedule(runner, nil, nil, nil)
	if len(runner.OtherJobs) != 0 {
		t.Fatal("the schedule was wired to an absent service")
	}
}
