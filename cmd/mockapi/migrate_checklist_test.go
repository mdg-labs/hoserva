package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/migrate"
)

func checklistItem(t *testing.T, h *handler, id apiv1.MigrationChecklistItemId) apiv1.MigrationChecklistItem {
	t.Helper()
	c, err := h.GetMigrationChecklist(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range c.Items {
		if it.ID == id {
			return it
		}
	}
	t.Fatalf("the checklist has no %s item: %+v", id, c)
	return apiv1.MigrationChecklistItem{}
}

func TestMockChecklist_DoesNotApplyWhileTheMigrationIsPendingAndRefusesAcknowledgements(t *testing.T) {
	ctx := context.Background()
	h, _ := newHandler("migration-pending")
	c, err := h.GetMigrationChecklist(ctx)
	if err != nil || c.Finished || len(c.Items) != 0 {
		t.Fatalf("checklist of a seeded pending migration = %+v %v, want not finished and empty", c, err)
	}
	_, err = h.AcknowledgeMigrationChecklistItem(ctx, apiv1.AcknowledgeMigrationChecklistItemParams{Item: apiv1.MigrationChecklistItemIdUserScripts})
	if status, code := mockErrCode(t, err); status != 409 || code != "migration_not_finished" {
		t.Fatalf("acknowledging while pending = %d %s, want 409 migration_not_finished", status, code)
	}

	mockVerifiedMigration(t, h)
	if c, _ := h.GetMigrationChecklist(ctx); c.Finished {
		t.Fatal("an imported and verified migration is finished before its point of no return")
	}
}

func TestMockChecklist_DerivesEachItemFromTheMocksOwnRecords(t *testing.T) {
	ctx := context.Background()
	h := mockCrossedMigration(t)

	c, err := h.GetMigrationChecklist(ctx)
	if err != nil || !c.Finished || len(c.Items) != 7 {
		t.Fatalf("checklist after the point of no return = %+v %v, want finished with 7 items", c, err)
	}
	if got := checklistItem(t, h, apiv1.MigrationChecklistItemIdAppdataCache); got.Status != apiv1.MigrationChecklistItemStatusTodo {
		t.Errorf("appdata = %+v, want todo: the mock's source had a cache", got)
	}
	if got := checklistItem(t, h, apiv1.MigrationChecklistItemIdInitialSync); got.Status != apiv1.MigrationChecklistItemStatusTodo {
		t.Errorf("initial sync before its job succeeded = %+v, want todo", got)
	}

	// The initial sync the parity initialisation queued is a job like any
	// other: once it has succeeded it is the record.
	h.mu.Lock()
	var syncID uuid.UUID
	for id, j := range h.jobs {
		if j.Type == apiv1.JobTypeSync {
			syncID = id
		}
	}
	now := time.Now().UTC().Truncate(time.Second)
	j := h.jobs[syncID]
	j.Status, j.StartedAt, j.FinishedAt = apiv1.JobStatusSucceeded, apiv1.NewOptNilDateTime(now), apiv1.NewOptNilDateTime(now)
	h.jobs[syncID] = j
	h.mu.Unlock()
	if got := checklistItem(t, h, apiv1.MigrationChecklistItemIdInitialSync); got.Status != apiv1.MigrationChecklistItemStatusDone || got.JobId.Or("") != syncID.String() {
		t.Errorf("initial sync after its job succeeded = %+v, want done from %s", got, syncID)
	}

	// Schedules: the nightly chain is on by default, with Unraid's values offered.
	sch := checklistItem(t, h, apiv1.MigrationChecklistItemIdSchedules)
	o := sch.Schedules.Value.Offers
	if sch.Status != apiv1.MigrationChecklistItemStatusDone || o.MoverTime.Or("") != "03:40" || !o.ScrubReportOnly.Or(false) || o.SpindownDelay.Or("") != "30" {
		t.Errorf("schedules = %+v %+v", sch, o)
	}
	if _, err := h.UpdateMaintenanceChainSchedule(ctx, &apiv1.UpdateMaintenanceChainScheduleRequest{Steps: []apiv1.MaintenanceChainStep{{ID: apiv1.MaintenanceChainStepIdSync, Enabled: false}}}); err != nil {
		t.Fatal(err)
	}
	if got := checklistItem(t, h, apiv1.MigrationChecklistItemIdSchedules); got.Status != apiv1.MigrationChecklistItemStatusTodo || got.Schedules.Value.Sync {
		t.Errorf("schedules with the sync step off = %+v", got)
	}

	// Notifications: a channel's successful test is the record.
	ch := apiv1.NotificationChannel{ID: uuid.New(), Name: "phone", Type: apiv1.NotificationChannelTypeNtfy, Enabled: true, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)}
	h.notifyMu.Lock()
	h.channels[ch.ID] = ch
	h.notifyMu.Unlock()
	n := checklistItem(t, h, apiv1.MigrationChecklistItemIdNotifications)
	if n.Status != apiv1.MigrationChecklistItemStatusTodo || n.Notifications.Value.Channels != 1 || strings.Join(n.Notifications.Value.Agents, ",") != "Pushover,Slack" {
		t.Errorf("notifications with an untested channel = %+v %+v", n, n.Notifications.Value)
	}
	if _, err := h.SendTestNotification(ctx, apiv1.SendTestNotificationParams{ChannelId: ch.ID}); err != nil {
		t.Fatal(err)
	}
	if got := checklistItem(t, h, apiv1.MigrationChecklistItemIdNotifications); got.Status != apiv1.MigrationChecklistItemStatusDone || got.Notifications.Value.Tested != 1 {
		t.Errorf("notifications after a test = %+v", got)
	}
}

func TestMockChecklist_AcknowledgesOnlyTheTwoItemsNoRecordShowsAndKeepsThemAfterAForget(t *testing.T) {
	ctx := context.Background()
	h := mockCrossedMigration(t)

	scripts := checklistItem(t, h, apiv1.MigrationChecklistItemIdUserScripts)
	if !scripts.Acknowledgeable || len(scripts.Scripts) != 2 || scripts.Scripts[0].Name != "nightly-report" || scripts.Scripts[0].Schedule.Or("") != "30 2 * * *" {
		t.Errorf("user scripts = %+v, want acknowledgeable, listing the two scripts", scripts)
	}
	for _, id := range []apiv1.MigrationChecklistItemId{
		apiv1.MigrationChecklistItemIdAppdataCache, apiv1.MigrationChecklistItemIdInitialSync, apiv1.MigrationChecklistItemIdFullScrub,
		apiv1.MigrationChecklistItemIdNotifications, apiv1.MigrationChecklistItemIdSchedules,
	} {
		_, err := h.AcknowledgeMigrationChecklistItem(ctx, apiv1.AcknowledgeMigrationChecklistItemParams{Item: id})
		if status, code := mockErrCode(t, err); status != 409 || code != "checklist_item_has_record" {
			t.Errorf("acknowledging %s = %d %s, want 409 checklist_item_has_record", id, status, code)
		}
		if got := checklistItem(t, h, id); got.AcknowledgedBy.IsSet() || got.Acknowledgeable {
			t.Errorf("%s after a refused acknowledgement = %+v", id, got)
		}
	}

	// A fix of the whole array is not a restore drill and is not shown.
	now := time.Now().UTC().Truncate(time.Second)
	whole := apiv1.Job{ID: uuid.New(), Type: apiv1.JobTypeFix, Class: apiv1.JobClassParity, Status: apiv1.JobStatusSucceeded, CreatedAt: now, StartedAt: apiv1.NewOptNilDateTime(now), FinishedAt: apiv1.NewOptNilDateTime(now)}
	h.mu.Lock()
	h.jobs[whole.ID] = whole
	h.mu.Unlock()
	if got := checklistItem(t, h, apiv1.MigrationChecklistItemIdRestoreDrill); got.JobId.IsSet() {
		t.Errorf("restore drill with only a whole-array fix = %+v, want no job shown", got)
	}

	// A fix job of one file that succeeded is shown, and the acknowledgement records it.
	fix := apiv1.Job{ID: uuid.New(), Type: apiv1.JobTypeFix, Class: apiv1.JobClassParity, Status: apiv1.JobStatusSucceeded, CreatedAt: now, StartedAt: apiv1.NewOptNilDateTime(now), FinishedAt: apiv1.NewOptNilDateTime(now)}
	h.mu.Lock()
	h.jobs[fix.ID] = fix
	h.fixPaths[fix.ID] = "/mnt/user/documents/tax.pdf"
	h.mu.Unlock()
	if got := checklistItem(t, h, apiv1.MigrationChecklistItemIdRestoreDrill); got.JobId.Or("") != fix.ID.String() || got.Status != apiv1.MigrationChecklistItemStatusTodo {
		t.Errorf("restore drill with a succeeded fix = %+v, want todo showing the job", got)
	}
	got, err := h.AcknowledgeMigrationChecklistItem(ctx, apiv1.AcknowledgeMigrationChecklistItemParams{Item: apiv1.MigrationChecklistItemIdRestoreDrill})
	if err != nil || got.Status != apiv1.MigrationChecklistItemStatusDone || got.AcknowledgedBy.Or("") != "local" || got.JobId.Or("") != fix.ID.String() || !got.AcknowledgedAt.IsSet() {
		t.Fatalf("acknowledged restore drill = %+v %v", got, err)
	}

	if err := h.ForgetMigration(ctx); err != nil {
		t.Fatal(err)
	}
	if got := checklistItem(t, h, apiv1.MigrationChecklistItemIdRestoreDrill); got.Status != apiv1.MigrationChecklistItemStatusDone || got.AcknowledgedBy.Or("") != "local" {
		t.Errorf("restore drill after forgetting the session = %+v, want the acknowledgement kept", got)
	}
	if got := checklistItem(t, h, apiv1.MigrationChecklistItemIdUserScripts); len(got.Scripts) != 0 {
		t.Errorf("user scripts after forgetting the scan = %+v, want none listed", got)
	}
}

func TestMockChecklist_RestoreDrillShowsTheFixStartFixQueuedForAPath(t *testing.T) {
	ctx := context.Background()
	h := mockCrossedMigration(t)

	whole, err := h.StartFix(ctx, &apiv1.StartFixRequest{Confirm: true})
	if err != nil {
		t.Fatal(err)
	}
	req := &apiv1.StartFixRequest{Confirm: true}
	req.SetPath(apiv1.NewOptString("/mnt/user/documents/tax.pdf"))
	one, err := h.StartFix(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	h.mu.Lock()
	for _, id := range []uuid.UUID{whole.ID, one.ID} {
		j := h.jobs[id]
		j.Status, j.StartedAt, j.FinishedAt = apiv1.JobStatusSucceeded, apiv1.NewOptNilDateTime(now), apiv1.NewOptNilDateTime(now)
		h.jobs[id] = j
	}
	h.mu.Unlock()
	if got := checklistItem(t, h, apiv1.MigrationChecklistItemIdRestoreDrill); got.JobId.Or("") != one.ID.String() {
		t.Errorf("restore drill = %+v, want the fix of one file %s, not the whole-array fix %s", got, one.ID, whole.ID)
	}
}

func requireChecklistNotApplying(t *testing.T, h *handler, when string) {
	t.Helper()
	ctx := context.Background()
	c, err := h.GetMigrationChecklist(ctx)
	if err != nil || c.Finished || c.FinishedAt.IsSet() || len(c.Items) != 0 {
		t.Fatalf("checklist %s = %+v %v, want not finished with no items", when, c, err)
	}
	_, err = h.AcknowledgeMigrationChecklistItem(ctx, apiv1.AcknowledgeMigrationChecklistItemParams{Item: apiv1.MigrationChecklistItemIdUserScripts})
	if status, code := mockErrCode(t, err); status != 409 || code != "migration_not_finished" {
		t.Fatalf("acknowledging %s = %d %s, want 409 migration_not_finished", when, status, code)
	}
}

// The checklist reads "finished" only from the stamp the point of no return
// leaves, so a migration that is pending, was undone, or stopped part-way
// through is never finished, and a migration that finished after a run that
// failed is finished at the later run.
func TestMockChecklist_FinishedIsTheStampOfThePointOfNoReturnAndNothingElse(t *testing.T) {
	ctx := context.Background()

	undone, _ := newHandler("migration-pending")
	mockVerifiedMigration(t, undone)
	requireChecklistNotApplying(t, undone, "while the verified migration is pending")
	if _, err := undone.StartMigrationImport(ctx, &apiv1.MigrationImportRequest{Confirm: true, Undo: apiv1.NewOptBool(true)}); err != nil {
		t.Fatal(err)
	}
	requireChecklistNotApplying(t, undone, "after the import was undone")

	h, _ := newHandler("migration-pending")
	h.migration.stopParityInit = true
	mockVerifiedMigration(t, h)
	m, _ := h.GetMigration(ctx)
	first, err := h.InitializeMigrationParity(ctx, &apiv1.MigrationInitializeParityRequest{Confirmation: m.ParityInit.Value.Confirmation.Value})
	if err != nil {
		t.Fatal(err)
	}
	requireChecklistNotApplying(t, h, "after a run that failed part-way through the point of no return")

	longAgo := time.Now().UTC().Add(-30 * time.Hour).Truncate(time.Second)
	h.mu.Lock()
	j := h.jobs[first.ID]
	j.CreatedAt, j.StartedAt, j.FinishedAt = longAgo, apiv1.NewOptNilDateTime(longAgo), apiv1.NewOptNilDateTime(longAgo)
	h.jobs[first.ID] = j
	h.mu.Unlock()

	before := time.Now().UTC().Truncate(time.Second)
	if _, err := h.InitializeMigrationParity(ctx, &apiv1.MigrationInitializeParityRequest{Confirmation: disk.ParityInitFinishConfirmation}); err != nil {
		t.Fatal(err)
	}
	c, err := h.GetMigrationChecklist(ctx)
	if err != nil || !c.Finished || len(c.Items) != len(migrate.ChecklistItemIDs) {
		t.Fatalf("checklist after the run that finished = %+v %v", c, err)
	}
	if at := c.FinishedAt.Value; at.Before(before) || at.After(time.Now()) {
		t.Errorf("finishedAt = %v, want the time of the run that finished (since %v), not the first run's %v", at, before, longAgo)
	}
	if _, err := h.AcknowledgeMigrationChecklistItem(ctx, apiv1.AcknowledgeMigrationChecklistItemParams{Item: apiv1.MigrationChecklistItemIdUserScripts}); err != nil {
		t.Errorf("acknowledging once finished: %v", err)
	}
}
