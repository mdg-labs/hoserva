package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/notify"
)

func TestConfigBackupWiring_OperationAnswers501WhenNothingIsWired(t *testing.T) {
	w := newContainersWiringHarness(t)
	if status, body := w.doBody(t, http.MethodPost, "/config/backup", ``); status != http.StatusNotImplemented {
		t.Fatalf("POST /config/backup without wiring = %d %s, want 501", status, body)
	}
}

// POST /config/backup reaches the backup through the real daemon server and
// its job registry: the archive lands in the enabled destination, the job
// succeeds, and nothing is alerted.
func TestConfigBackupWiring_RunNowIsReachableOverHTTPAndWritesTheDestination(t *testing.T) {
	w := newContainersWiringHarness(t)
	svc, dest, _ := drillBackupService(t, w.db, w.root, time.Date(2026, 9, 29, 3, 0, 0, 0, time.UTC))
	pub := &recordingPublisher{}
	wireBackup(w.handler, svc)
	wireConfigBackup(w.registry, svc, pub)
	before, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}

	status, body := w.doBody(t, http.MethodPost, "/config/backup", ``)
	if status != http.StatusOK {
		t.Fatalf("POST /config/backup = %d %s", status, body)
	}
	var queued struct {
		ID    string `json:"id"`
		Type  string `json:"type"`
		Class string `json:"class"`
	}
	if err := json.Unmarshal(body, &queued); err != nil || queued.Type != "config_backup" || queued.Class != "service" {
		t.Fatalf("job = %s (%v), want a config_backup service job", body, err)
	}
	if done := w.awaitJobByID(t, queued.ID); done.Status != job.StatusSucceeded {
		t.Fatalf("config backup job = %s %s", done.Status, done.ErrorMessage)
	}
	after, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}
	// Retention keeps the newest archive of the day, so the archive the
	// drill helper wrote earlier is replaced by the new one.
	if len(after) != 1 || after[0].Name() == before[0].Name() {
		t.Fatalf("destination = %v, want the new archive in place of %s", after, before[0].Name())
	}
	if n := pub.count(notify.EventConfigBackupFailed); n != 0 {
		t.Fatalf("config_backup_failed published %d times for a successful backup", n)
	}
}

func TestConfigBackupWiring_ARefusedRequestQueuesNothingWhenNoDestinationIsEnabled(t *testing.T) {
	w := newContainersWiringHarness(t)
	svc, _, _ := drillBackupService(t, w.db, w.root, time.Date(2026, 9, 29, 3, 0, 0, 0, time.UTC))
	off := false
	if _, err := svc.UpdateDestination(context.Background(), backup.DefaultPoolID, backup.DestinationUpdate{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	wireBackup(w.handler, svc)
	wireConfigBackup(w.registry, svc, &recordingPublisher{})

	status, body := w.doBody(t, http.MethodPost, "/config/backup", ``)
	if status != http.StatusConflict || !strings.Contains(string(body), "backup_no_destination") {
		t.Fatalf("POST /config/backup with nothing enabled = %d %s, want 409 backup_no_destination", status, body)
	}
	jobs, err := w.handler.Store.List(context.Background(), job.ListFilter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.Type == job.TypeConfigBackup {
			t.Fatalf("a refused request queued %v", j)
		}
	}
}

func TestConfigBackupWiring_AFailedBackupEndsTheJobFailedAndPublishesTheAlert(t *testing.T) {
	w := newContainersWiringHarness(t)
	blocker := filepath.Join(w.root, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	dests := &backup.FakeDestinationStore{}
	if err := dests.CreateDestination(context.Background(), backup.Destination{
		ID: backup.DefaultPoolID, Name: "Pool", Type: backup.TypeLocal, Path: filepath.Join(blocker, "backups"), Enabled: true,
		Retention: backup.Retention{Daily: 7},
	}); err != nil {
		t.Fatal(err)
	}
	svc := &backup.Service{DB: w.db, Paths: backup.Paths{DBPath: filepath.Join(w.root, "hoservad.db")}, Store: dests, Hostname: "test-host"}
	pub := &recordingPublisher{}
	wireBackup(w.handler, svc)
	wireConfigBackup(w.registry, svc, pub)

	status, body := w.doBody(t, http.MethodPost, "/config/backup", ``)
	if status != http.StatusOK {
		t.Fatalf("POST /config/backup = %d %s", status, body)
	}
	var queued struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &queued); err != nil {
		t.Fatal(err)
	}
	done := w.awaitJobByID(t, queued.ID)
	if done.Status != job.StatusFailed || !strings.Contains(done.ErrorMessage, "Pool") && !strings.Contains(done.ErrorMessage, backup.DefaultPoolID) {
		t.Fatalf("job = %s %q, want failed with the destination named", done.Status, done.ErrorMessage)
	}
	if n := pub.count(notify.EventConfigBackupFailed); n != 1 {
		t.Fatalf("config_backup_failed published %d times, want once", n)
	}
}
