package main

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

type checklistDaemon struct {
	mu       sync.Mutex
	requests []string
	list     apiv1.MigrationChecklist
	refuse   *apiv1.Error
}

func (d *checklistDaemon) seen() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.requests...)
}

func startChecklistDaemon(t *testing.T) (string, *checklistDaemon) {
	t.Helper()
	dir, err := os.MkdirTemp("", "hsv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	d := &checklistDaemon{}
	at := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := r.Method + " " + r.URL.Path
		d.mu.Lock()
		d.requests = append(d.requests, req)
		list, refuse := d.list, d.refuse
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		var out []byte
		var err error
		switch {
		case refuse != nil:
			w.WriteHeader(http.StatusConflict)
			out, err = refuse.MarshalJSON()
		case req == "GET /api/v1/migrate/checklist":
			out, err = list.MarshalJSON()
		case req == "POST /api/v1/migrate/checklist/restore_drill/acknowledge":
			out, err = (&apiv1.MigrationChecklistItem{
				ID: apiv1.MigrationChecklistItemIdRestoreDrill, Status: apiv1.MigrationChecklistItemStatusDone,
				AcknowledgedBy: apiv1.NewOptString("alice"), AcknowledgedAt: apiv1.NewOptDateTime(at), JobId: apiv1.NewOptString("fix-1"),
			}).MarshalJSON()
		default:
			w.WriteHeader(http.StatusNotFound)
			out, err = (&apiv1.Error{Code: "not_found", Message: "no such thing"}).MarshalJSON()
		}
		if err != nil {
			panic(err)
		}
		_, _ = w.Write(out)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock, d
}

func TestMigrateChecklistSaysItDoesNotApplyBeforeTheMigrationFinished(t *testing.T) {
	sock, d := startChecklistDaemon(t)
	d.list = apiv1.MigrationChecklist{Finished: false, Items: []apiv1.MigrationChecklistItem{}}
	printed, err := runBackupCLI(t, sock, "migrate", "checklist")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(printed, "the migration has not finished") || strings.Contains(printed, "[ ]") {
		t.Errorf("output = %s", printed)
	}
}

func TestMigrateChecklistNeverPrintsAZeroFinishTime(t *testing.T) {
	sock, d := startChecklistDaemon(t)
	d.list = apiv1.MigrationChecklist{Finished: true, Items: []apiv1.MigrationChecklistItem{
		{ID: apiv1.MigrationChecklistItemIdFullScrub, Status: apiv1.MigrationChecklistItemStatusTodo},
	}}
	printed, err := runBackupCLI(t, sock, "migrate", "checklist")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(printed, "0001-01-01") || !strings.Contains(printed, "the migration has not finished") {
		t.Errorf("output = %s", printed)
	}
}

func TestMigrateChecklistPrintsEachItemsStateAndWhatUnraidOffers(t *testing.T) {
	sock, d := startChecklistDaemon(t)
	at := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	d.list = apiv1.MigrationChecklist{Finished: true, FinishedAt: apiv1.NewOptDateTime(at), Items: []apiv1.MigrationChecklistItem{
		{ID: apiv1.MigrationChecklistItemIdAppdataCache, Status: apiv1.MigrationChecklistItemStatusNotApplicable},
		{ID: apiv1.MigrationChecklistItemIdInitialSync, Status: apiv1.MigrationChecklistItemStatusDone, JobId: apiv1.NewOptString("sync-1"), DoneAt: apiv1.NewOptDateTime(at)},
		{ID: apiv1.MigrationChecklistItemIdFullScrub, Status: apiv1.MigrationChecklistItemStatusTodo},
		{ID: apiv1.MigrationChecklistItemIdNotifications, Status: apiv1.MigrationChecklistItemStatusTodo, Notifications: apiv1.NewOptMigrationChecklistNotifications(apiv1.MigrationChecklistNotifications{Channels: 1, Tested: 0, Agents: []string{"Pushover", "Slack"}})},
		{ID: apiv1.MigrationChecklistItemIdSchedules, Status: apiv1.MigrationChecklistItemStatusTodo, Schedules: apiv1.NewOptMigrationChecklistSchedules(apiv1.MigrationChecklistSchedules{
			Mover: true, Sync: false, Scrub: true, ChainStartTime: "02:00",
			Offers: apiv1.MigrationChecklistOffers{
				MoverCron: apiv1.NewOptString("40 3 * * *"), MoverTime: apiv1.NewOptString("03:40"), SpindownDelay: apiv1.NewOptString("30"), ScrubReportOnly: apiv1.NewOptBool(true),
				ParityCheck: apiv1.NewOptMigrationChecklistParityCheck(apiv1.MigrationChecklistParityCheck{Mode: apiv1.NewOptString("1"), Hour: apiv1.NewOptString("0 3"), Correcting: false}),
			},
		})},
		{ID: apiv1.MigrationChecklistItemIdUserScripts, Status: apiv1.MigrationChecklistItemStatusTodo, Acknowledgeable: true, Scripts: []apiv1.MigrationChecklistScript{
			{Name: "nightly-report", Schedule: apiv1.NewOptString("30 2 * * *")}, {Name: "Weekly cleanup"},
		}},
		{ID: apiv1.MigrationChecklistItemIdRestoreDrill, Status: apiv1.MigrationChecklistItemStatusTodo, Acknowledgeable: true, JobId: apiv1.NewOptString("fix-9")},
	}}
	printed, err := runBackupCLI(t, sock, "migrate", "checklist")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[-] Appdata moved back onto the cache", "the source had no cache",
		"[x] Initial sync complete", "Job sync-1",
		"[ ] Full scrub after the initial sync", "--percent 100 --all-blocks",
		"1 notification channel, 0 enabled", "Pushover, Slack", "no secret is carried over",
		"mover on, sync off, scrub on", "Unraid has no sync schedule to carry over", "offered as the chain's start time, 03:40",
		"a scrub that only reports", "spin-down delay was 30",
		`User script "nightly-report", schedule 30 2 * * *`, `User script "Weekly cleanup", schedule none in customSchedule.cron`,
		"checklist ack user_scripts", "Latest fix job: fix-9", "checklist ack restore_drill",
	} {
		if !strings.Contains(printed, want) {
			t.Errorf("output lacks %q:\n%s", want, printed)
		}
	}
	if !strings.Contains(printed, "#619") || strings.Contains(printed, "fix --confirm") {
		t.Errorf("the restore drill hint must point at no whole-array fix and name #619:\n%s", printed)
	}
	for _, line := range strings.Split(printed, "\n") {
		if strings.Contains(line, "Initial sync complete") && !strings.HasPrefix(line, "[x]") {
			t.Errorf("a done item is not marked done: %q", line)
		}
	}
}

func TestMigrateChecklistAckSendsTheItemAndSaysWhoAcknowledged(t *testing.T) {
	sock, d := startChecklistDaemon(t)
	printed, err := runBackupCLI(t, sock, "migrate", "checklist", "ack", "restore_drill")
	if err != nil {
		t.Fatal(err)
	}
	if got := d.seen(); len(got) != 1 || got[0] != "POST /api/v1/migrate/checklist/restore_drill/acknowledge" {
		t.Errorf("requests = %v", got)
	}
	if !strings.Contains(printed, "restore_drill acknowledged by alice") {
		t.Errorf("output = %s", printed)
	}
}

func TestMigrateChecklistAckShowsTheDaemonsRefusal(t *testing.T) {
	sock, d := startChecklistDaemon(t)
	d.refuse = &apiv1.Error{Code: "checklist_item_has_record", Message: "this checklist item is derived from a record and cannot be acknowledged by hand"}
	_, err := runBackupCLI(t, sock, "migrate", "checklist", "ack", "restore_drill")
	if err == nil || !strings.Contains(err.Error(), "derived from a record") {
		t.Fatalf("ack = %v, want the daemon's refusal", err)
	}
	if _, err := runBackupCLI(t, sock, "migrate", "checklist", "ack"); err == nil {
		t.Error("ack with no item succeeded")
	}
}

func TestRootCmdHasMigrateChecklist(t *testing.T) {
	cmd, _, err := rootCmd().Find([]string{"migrate", "checklist", "ack"})
	if err != nil || cmd.Name() != "ack" {
		t.Fatalf("find migrate checklist ack: %v", err)
	}
}
