package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/migrate"
)

func TestChecklistActor_IsTheSignedInUserAndLocalForTheSocket(t *testing.T) {
	if got := ChecklistActor(context.Background()); got != "local" {
		t.Errorf("no principal = %q, want local", got)
	}
	if got := ChecklistActor(withPrincipal(context.Background(), Principal{Role: RoleAdmin, Local: true})); got != "local" {
		t.Errorf("the socket's principal = %q, want local", got)
	}
	if got := ChecklistActor(withPrincipal(context.Background(), Principal{Username: "alice", Role: RoleAdmin})); got != "alice" {
		t.Errorf("a signed-in user = %q, want alice", got)
	}
}

func TestMigrateError_MapsTheChecklistRefusals(t *testing.T) {
	for _, c := range []struct {
		err    error
		code   string
		status int
	}{
		{migrate.ErrMigrationNotFinished, "migration_not_finished", 409},
		{migrate.ErrChecklistItemHasRecord, "checklist_item_has_record", 409},
		{migrate.ErrChecklistItemNotFound, "checklist_item_not_found", 404},
	} {
		var ae *apiError
		if !errors.As(migrateError(c.err), &ae) || ae.code != c.code || ae.statusCode != c.status {
			t.Errorf("migrateError(%v) = %+v, want %d %s", c.err, ae, c.status, c.code)
		}
	}
	var ae *apiError
	if !errors.As(migrateError(migrate.ErrChecklistNotConfigured), &ae) || ae.statusCode != 501 {
		t.Errorf("an unconfigured checklist = %+v, want 501", ae)
	}
}

func TestMigrationChecklistToAPI_KeepsTheEvidenceAndNeverEmitsNullLists(t *testing.T) {
	at := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	c := migrate.Checklist{Finished: true, FinishedAt: at, Items: []migrate.ChecklistItem{
		{ID: migrate.ItemNotifications, Status: migrate.ItemTodo, Notifications: &migrate.NotificationsDetail{Channels: 2}},
		{ID: migrate.ItemSchedules, Status: migrate.ItemTodo, Schedules: &migrate.SchedulesDetail{
			Sync: false, Mover: true, Scrub: true, ChainStartTime: "02:00",
			Offers: migrate.ScheduleOffers{MoverCron: "40 3 * * *", MoverTime: "03:40", ParityCheck: &migrate.ParityCheckSchedule{Found: true, Mode: "1"}, ScrubReportOnly: true, SpindownDelay: "30"},
		}},
		{ID: migrate.ItemUserScripts, Status: migrate.ItemDone, Acknowledgeable: false, DoneAt: at, Ack: &migrate.ChecklistAck{By: "alice", At: at}},
		{ID: migrate.ItemRestoreDrill, Status: migrate.ItemTodo, Acknowledgeable: true, JobID: "fix-1"},
	}}
	out := MigrationChecklistToAPI(c)
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"agents":[]`, `"scripts":[]`, `"moverTime":"03:40"`, `"scrubReportOnly":true`, `"acknowledgedBy":"alice"`, `"jobId":"fix-1"`, `"spindownDelay":"30"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("the answer lacks %s: %s", want, data)
		}
	}
	if !out.Finished || out.Items[1].Schedules.Value.Sync || out.Items[3].Status != apiv1.MigrationChecklistItemStatusTodo {
		t.Errorf("answer = %+v", out)
	}
	if empty := MigrationChecklistToAPI(migrate.Checklist{}); !strings.Contains(mustJSON(t, empty), `"items":[]`) {
		t.Errorf("an empty checklist = %s, want an empty items list", mustJSON(t, empty))
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
