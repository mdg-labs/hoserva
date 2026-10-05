package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/migrate"
	"github.com/mdg-labs/hoserva/internal/notify"
	"github.com/mdg-labs/hoserva/internal/store"
)

type scriptedSender struct{ err error }

func (s *scriptedSender) Send(context.Context, notify.ChannelConfig, string, notify.Message) error {
	return s.err
}

type checklistRig struct {
	*containersWiringHarness
	sender *scriptedSender
}

// newChecklistRig is the daemon's real API server with the migrator, the
// notification service and the schedule service wired as main.go wires them,
// then wireMigrationChecklist, over a real database.
func newChecklistRig(t *testing.T) *checklistRig {
	t.Helper()
	w := newContainersWiringHarness(t)
	disks := disk.NewFakeProvider()
	disks.AddDisk("/dev/sdb", disk.Disk{Serial: "WIREDSERIAL", Size: 1 << 40})
	if err := wireMigration(context.Background(), w.handler, w.registry, disks, disk.NewFakeReadOnlyMounter(), disk.NewFakeRunner(), store.NewMigrationSessionStore(w.db), w.root); err != nil {
		t.Fatal(err)
	}
	r := &checklistRig{containersWiringHarness: w, sender: &scriptedSender{}}
	w.handler.Notify = notify.NewService(notify.NewStore(w.db), nil, notify.Senders{notify.ChannelNtfy: r.sender})
	w.handler.Schedules = api.NewScheduleService(api.NewScheduleStore(w.db), api.NewSettingsStore(w.db))
	return r
}

func (r *checklistRig) wire(t *testing.T) {
	t.Helper()
	if err := wireMigrationChecklist(r.handler, r.handler.Store, store.NewArrayStore(r.db)); err != nil {
		t.Fatal(err)
	}
}

func (r *checklistRig) checklist(t *testing.T) apiv1.MigrationChecklist {
	t.Helper()
	status, body := r.do(t, http.MethodGet, "/migrate/checklist")
	var c apiv1.MigrationChecklist
	if err := json.Unmarshal(body, &c); status != http.StatusOK || err != nil {
		t.Fatalf("GET /migrate/checklist = %d %s (%v)", status, body, err)
	}
	return c
}

func (r *checklistRig) item(t *testing.T, id apiv1.MigrationChecklistItemId) apiv1.MigrationChecklistItem {
	t.Helper()
	for _, it := range r.checklist(t).Items {
		if it.ID == id {
			return it
		}
	}
	t.Fatalf("the checklist has no %s item", id)
	return apiv1.MigrationChecklistItem{}
}

var checklistClock = time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)

// finishMigration ends the migration of the rig's array record through the
// store's own last step, FinishMigration, then pins the stamp it wrote onto the
// tests' fixed job clock (their jobs are created at checklistClock plus hours).
func (r *checklistRig) finishMigration(t *testing.T, hour int) {
	t.Helper()
	ctx := context.Background()
	if _, err := r.db.ExecContext(ctx, "UPDATE array_settings SET migration_recorded = '[]'"); err != nil {
		t.Fatal(err)
	}
	if err := store.NewArrayStore(r.db).FinishMigration(ctx); err != nil {
		t.Fatal(err)
	}
	stamp := checklistClock.Add(time.Duration(hour) * time.Hour).Format(time.RFC3339)
	if _, err := r.db.ExecContext(ctx, "UPDATE array_settings SET migration_finished_at = ?", stamp); err != nil {
		t.Fatal(err)
	}
}

func (r *checklistRig) addJob(t *testing.T, id string, typ job.Type, class job.Class, params any, startH, endH int) {
	t.Helper()
	r.addJobWithStatus(t, id, typ, class, job.StatusSucceeded, params, startH, endH)
}

func (r *checklistRig) addJobWithStatus(t *testing.T, id string, typ job.Type, class job.Class, status job.Status, params any, startH, endH int) {
	t.Helper()
	j := &job.Job{
		ID: id, Type: typ, Class: class, Status: status,
		CreatedAt: checklistClock.Add(time.Duration(startH) * time.Hour),
	}
	started, finished := checklistClock.Add(time.Duration(startH)*time.Hour), checklistClock.Add(time.Duration(endH)*time.Hour)
	j.StartedAt, j.FinishedAt = &started, &finished
	if params != nil {
		data, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		j.Params = data
	}
	if err := r.handler.Store.Create(context.Background(), j); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationChecklistWiring_JobTypesAreTheDaemonsOwn(t *testing.T) {
	for got, want := range map[string]job.Type{
		migrate.JobTypeSync: job.TypeSync, migrate.JobTypeScrub: job.TypeScrub,
		migrate.JobTypeFix: job.TypeFix, migrate.JobTypeShareRelocation: job.TypeShareRelocation,
	} {
		if got != string(want) {
			t.Errorf("the checklist reads %q jobs, the daemon's type is %q", got, want)
		}
	}
	if migrate.FullScrubPercent != 100 {
		t.Errorf("FullScrubPercent = %d", migrate.FullScrubPercent)
	}
}

func TestMigrationChecklistWiring_AnswersNotConfiguredUntilWiredAndNotYetBeforeTheMigrationFinished(t *testing.T) {
	r := newChecklistRig(t)
	if status, body := r.do(t, http.MethodGet, "/migrate/checklist"); status != http.StatusNotImplemented {
		t.Fatalf("GET /migrate/checklist before wireMigrationChecklist = %d %s, want 501", status, body)
	}
	r.wire(t)
	if c := r.checklist(t); c.Finished || len(c.Items) != 0 {
		t.Fatalf("checklist on an array whose migration never finished = %+v, want not finished with no items", c)
	}
	status, body := r.do(t, http.MethodPost, "/migrate/checklist/user_scripts/acknowledge")
	if status != http.StatusConflict || !strings.Contains(string(body), "migration_not_finished") {
		t.Fatalf("acknowledging before the migration finished = %d %s, want 409 migration_not_finished", status, body)
	}
	r.addJob(t, "sync-early", job.TypeSync, job.ClassParity, job.SyncParams{}, 1, 2)
	r.addJob(t, "parity", job.TypeMigrationParity, job.ClassTopology, nil, 3, 4)
	if c := r.checklist(t); c.Finished {
		t.Errorf("a succeeded sync and a succeeded migration_parity job finished the checklist with no stamp on the array record: %+v", c)
	}
	status, body = r.do(t, http.MethodPost, "/migrate/checklist/user_scripts/acknowledge")
	if status != http.StatusConflict || !strings.Contains(string(body), "migration_not_finished") {
		t.Errorf("acknowledging with a succeeded migration_parity job but no stamp = %d %s, want 409 migration_not_finished", status, body)
	}
}

func TestMigrationChecklistWiring_EachItemIsDerivedFromTheDaemonsRecords(t *testing.T) {
	r := newChecklistRig(t)
	r.wire(t)

	zipData := flashBackupZipWith(t, "7.3.2", map[string]string{
		"config/share.cfg": "shareMoverSchedule=\"40 3 * * *\"\n",
		"config/plugins/dynamix/notifications/agents/Slack.xml": "<x/>",
		"config/plugins/user.scripts/scripts/nightly/script":    "#!/bin/bash\necho SECRET\n",
		"config/plugins/user.scripts/customSchedule.cron":       "30 2 * * * /usr/local/emhttp/plugins/user.scripts/startCustom.php /boot/config/plugins/user.scripts/scripts/nightly/script > /dev/null 2>&1\n",
	})
	status, body := r.uploadScan(t, zipData, false)
	if status != http.StatusOK {
		t.Fatalf("POST /migrate/scan = %d %s", status, body)
	}
	var queued struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &queued); err != nil {
		t.Fatal(err)
	}
	if done := r.awaitJobByID(t, queued.ID); done.Status != job.StatusSucceeded {
		t.Fatalf("scan job = %s %s", done.Status, done.ErrorMessage)
	}

	r.finishMigration(t, 10)
	c := r.checklist(t)
	if !c.Finished || len(c.Items) != 7 || !c.FinishedAt.Value.Equal(checklistClock.Add(10*time.Hour)) {
		t.Fatalf("checklist = %+v, want finished with 7 items", c)
	}

	appdata := r.item(t, apiv1.MigrationChecklistItemIdAppdataCache)
	if appdata.Status != apiv1.MigrationChecklistItemStatusNotApplicable {
		t.Errorf("appdata = %+v, want not applicable: the scanned source had only a data disk", appdata)
	}
	r.addJob(t, "move-media", job.TypeShareRelocation, job.ClassArrayWrite, job.ShareRelocationParams{Share: "media", To: "cache"}, 12, 13)
	if got := r.item(t, apiv1.MigrationChecklistItemIdAppdataCache); got.Status != apiv1.MigrationChecklistItemStatusNotApplicable {
		t.Errorf("appdata after relocating another share = %+v", got)
	}
	r.addJob(t, "move-appdata", job.TypeShareRelocation, job.ClassArrayWrite, job.ShareRelocationParams{Share: "appdata", To: "cache"}, 14, 15)
	if got := r.item(t, apiv1.MigrationChecklistItemIdAppdataCache); got.Status != apiv1.MigrationChecklistItemStatusDone || got.JobId.Or("") != "move-appdata" {
		t.Errorf("appdata after its relocation = %+v, want done from move-appdata", got)
	}

	r.addJob(t, "sync-dry", job.TypeSync, job.ClassParity, job.SyncParams{DryRun: true}, 11, 12)
	if got := r.item(t, apiv1.MigrationChecklistItemIdInitialSync); got.Status != apiv1.MigrationChecklistItemStatusTodo {
		t.Errorf("initial sync after a dry run = %+v, want todo", got)
	}
	r.addJob(t, "sync-1", job.TypeSync, job.ClassParity, job.SyncParams{}, 16, 20)
	if got := r.item(t, apiv1.MigrationChecklistItemIdInitialSync); got.Status != apiv1.MigrationChecklistItemStatusDone || got.JobId.Or("") != "sync-1" {
		t.Errorf("initial sync = %+v, want done from sync-1", got)
	}

	eight := 8
	r.addJob(t, "scrub-default", job.TypeScrub, job.ClassParity, nil, 21, 22)
	r.addJob(t, "scrub-8", job.TypeScrub, job.ClassParity, job.ScrubParams{Percent: &eight}, 23, 24)
	if got := r.item(t, apiv1.MigrationChecklistItemIdFullScrub); got.Status != apiv1.MigrationChecklistItemStatusTodo {
		t.Errorf("full scrub after partial scrubs = %+v, want todo", got)
	}
	hundred := 100
	r.addJob(t, "scrub-100-recent-skipped", job.TypeScrub, job.ClassParity, job.ScrubParams{Percent: &hundred}, 25, 26)
	if got := r.item(t, apiv1.MigrationChecklistItemIdFullScrub); got.Status != apiv1.MigrationChecklistItemStatusTodo {
		t.Errorf("full scrub after a 100 percent scrub that skips recent blocks = %+v, want todo", got)
	}
	r.addJob(t, "scrub-100", job.TypeScrub, job.ClassParity, job.ScrubParams{Percent: &hundred, AllBlocks: true}, 27, 30)
	if got := r.item(t, apiv1.MigrationChecklistItemIdFullScrub); got.Status != apiv1.MigrationChecklistItemStatusDone || got.JobId.Or("") != "scrub-100" {
		t.Errorf("full scrub = %+v, want done from scrub-100", got)
	}

	// Notifications: a channel exists, its failing test does not count, its
	// succeeding one does.
	ch, err := r.handler.Notify.CreateChannel(context.Background(), notify.ChannelInput{
		Name: "phone", Type: notify.ChannelNtfy, Enabled: true, Config: notify.ChannelConfig{NtfyURL: "https://ntfy.example", NtfyTopic: "hoserva"},
	})
	if err != nil {
		t.Fatal(err)
	}
	n := r.item(t, apiv1.MigrationChecklistItemIdNotifications)
	if n.Status != apiv1.MigrationChecklistItemStatusTodo || n.Notifications.Value.Channels != 1 || n.Notifications.Value.Tested != 0 || strings.Join(n.Notifications.Value.Agents, ",") != "Slack" {
		t.Errorf("notifications with an untested channel = %+v %+v, want todo naming the Slack agent", n, n.Notifications.Value)
	}
	r.sender.err = context.DeadlineExceeded
	if status, body := r.do(t, http.MethodPost, "/notifications/channels/"+ch.ID+"/test"); status != http.StatusOK || !strings.Contains(string(body), `"success":false`) {
		t.Fatalf("a failing test = %d %s", status, body)
	}
	if got := r.item(t, apiv1.MigrationChecklistItemIdNotifications); got.Status != apiv1.MigrationChecklistItemStatusTodo {
		t.Errorf("notifications after a failed test = %+v, want todo", got)
	}
	r.sender.err = nil
	// A channel's updated_at is stored to the second, so a test counts only from
	// the next second on.
	time.Sleep(1100 * time.Millisecond)
	if status, body := r.do(t, http.MethodPost, "/notifications/channels/"+ch.ID+"/test"); status != http.StatusOK || !strings.Contains(string(body), `"success":true`) {
		t.Fatalf("a succeeding test = %d %s", status, body)
	}
	if got := r.item(t, apiv1.MigrationChecklistItemIdNotifications); got.Status != apiv1.MigrationChecklistItemStatusDone || got.Notifications.Value.Tested != 1 {
		t.Errorf("notifications after a successful test = %+v %+v, want done", got, got.Notifications.Value)
	}

	// Schedules: the nightly chain's three steps, and what the scan offers.
	sch := r.item(t, apiv1.MigrationChecklistItemIdSchedules)
	if sch.Status != apiv1.MigrationChecklistItemStatusDone || sch.Schedules.Value.Offers.MoverTime.Or("") != "03:40" || sch.Schedules.Value.Offers.MoverCron.Or("") != "40 3 * * *" {
		t.Errorf("schedules = %+v %+v, want done with the mover time offered", sch, sch.Schedules.Value)
	}
	off := false
	if _, err := r.handler.Schedules.UpdateChain(context.Background(), api.UpdateChainInput{StepEnabled: map[job.Step]bool{job.StepSync: off}}); err != nil {
		t.Fatal(err)
	}
	if got := r.item(t, apiv1.MigrationChecklistItemIdSchedules); got.Status != apiv1.MigrationChecklistItemStatusTodo || got.Schedules.Value.Sync {
		t.Errorf("schedules with the sync step off = %+v %+v, want todo", got, got.Schedules.Value)
	}

	// The two acknowledged items, and the refusal for every other.
	scripts := r.item(t, apiv1.MigrationChecklistItemIdUserScripts)
	if scripts.Status != apiv1.MigrationChecklistItemStatusTodo || !scripts.Acknowledgeable || len(scripts.Scripts) != 1 || scripts.Scripts[0].Name != "nightly" || scripts.Scripts[0].Schedule.Or("") != "30 2 * * *" {
		t.Errorf("user scripts = %+v, want todo, acknowledgeable, listing nightly", scripts)
	}
	r.addJob(t, "fix-1", job.TypeFix, job.ClassParity, job.FixParams{Confirm: true}, 31, 32)
	if got := r.item(t, apiv1.MigrationChecklistItemIdRestoreDrill); got.Status != apiv1.MigrationChecklistItemStatusTodo || got.JobId.Or("") != "fix-1" || !got.Acknowledgeable {
		t.Errorf("restore drill = %+v, want todo showing fix-1", got)
	}
	status, body = r.do(t, http.MethodPost, "/migrate/checklist/restore_drill/acknowledge")
	var acked apiv1.MigrationChecklistItem
	if err := json.Unmarshal(body, &acked); status != http.StatusOK || err != nil {
		t.Fatalf("acknowledging the restore drill = %d %s (%v)", status, body, err)
	}
	if acked.Status != apiv1.MigrationChecklistItemStatusDone || acked.AcknowledgedBy.Or("") != "local" || acked.JobId.Or("") != "fix-1" || acked.AcknowledgedAt.IsSet() == false {
		t.Errorf("acknowledged restore drill = %+v, want done, by local, recording fix-1", acked)
	}
	for _, id := range []string{"appdata_cache", "initial_sync", "full_scrub", "notifications", "schedules"} {
		status, body = r.do(t, http.MethodPost, "/migrate/checklist/"+id+"/acknowledge")
		if status != http.StatusConflict || !strings.Contains(string(body), "checklist_item_has_record") {
			t.Errorf("acknowledging %s = %d %s, want 409 checklist_item_has_record", id, status, body)
		}
	}
	if got := r.item(t, apiv1.MigrationChecklistItemIdInitialSync); got.AcknowledgedBy.IsSet() {
		t.Errorf("a refused acknowledgement was recorded: %+v", got)
	}

	// The acknowledgement is in the database, so it outlives forgetting the scan.
	if status, body := r.do(t, http.MethodDelete, "/migrate"); status != http.StatusNoContent && status != http.StatusOK {
		t.Fatalf("DELETE /migrate = %d %s", status, body)
	}
	if got := r.item(t, apiv1.MigrationChecklistItemIdRestoreDrill); got.Status != apiv1.MigrationChecklistItemStatusDone || got.AcknowledgedBy.Or("") != "local" {
		t.Errorf("restore drill after forgetting the session = %+v, want the acknowledgement kept", got)
	}
}

func TestMigrationChecklistWiring_AParityJobThatFailedAfterTheMigrationFinishedStillLeavesTheChecklistApplying(t *testing.T) {
	r := newChecklistRig(t)
	r.wire(t)
	r.addJobWithStatus(t, "parity", job.TypeMigrationParity, job.ClassTopology, job.StatusFailed, nil, 10, 11)
	r.finishMigration(t, 10)
	if c := r.checklist(t); !c.Finished || len(c.Items) != 7 {
		t.Fatalf("checklist after a failed parity job on a finished array record = %+v, want finished with 7 items", c)
	}
	if status, body := r.do(t, http.MethodPost, "/migrate/checklist/user_scripts/acknowledge"); status != http.StatusOK {
		t.Fatalf("acknowledging after a failed parity job = %d %s, want 200", status, body)
	}
}

// A first run that failed and a later one that finished the migration: the
// checklist's finishedAt is the later one's, and a sync created between the
// two is not the initial sync.
func TestMigrationChecklistWiring_FinishedAtIsWhenTheMigrationFinishedNotWhenTheFirstParityJobEnded(t *testing.T) {
	r := newChecklistRig(t)
	r.wire(t)
	r.addJobWithStatus(t, "parity-1", job.TypeMigrationParity, job.ClassTopology, job.StatusFailed, nil, 1, 2)
	r.addJob(t, "sync-between", job.TypeSync, job.ClassParity, job.SyncParams{}, 10, 11)
	r.addJob(t, "parity-2", job.TypeMigrationParity, job.ClassTopology, nil, 31, 32)
	r.finishMigration(t, 32)
	c := r.checklist(t)
	if !c.Finished || !c.FinishedAt.Value.Equal(checklistClock.Add(32*time.Hour)) {
		t.Fatalf("finishedAt = %v (finished %v), want the finish at hour 32", c.FinishedAt, c.Finished)
	}
	if got := r.item(t, apiv1.MigrationChecklistItemIdInitialSync); got.Status != apiv1.MigrationChecklistItemStatusTodo {
		t.Errorf("initial sync = %+v, want todo: the only sync was created before the migration finished", got)
	}
}

func (r *checklistRig) acknowledgeUserScripts(t *testing.T) (int, string) {
	t.Helper()
	status, body := r.do(t, http.MethodPost, "/migrate/checklist/user_scripts/acknowledge")
	return status, string(body)
}

// An adoption whose point of no return failed before it recorded anything stays
// pending; backing out of it deletes the array record, and what is left is not a
// migration that finished, even once a new array is created by hand.
func TestMigrationChecklistWiring_AnAdoptionUndoneAfterAFailedParityInitialisationIsNotAFinishedMigration(t *testing.T) {
	pw := wireParity(t)
	w := pw.w
	ctx := context.Background()
	r := &checklistRig{containersWiringHarness: w, sender: &scriptedSender{}}
	w.handler.Notify = notify.NewService(notify.NewStore(w.db), nil, notify.Senders{notify.ChannelNtfy: r.sender})
	w.handler.Schedules = api.NewScheduleService(api.NewScheduleStore(w.db), api.NewSettingsStore(w.db))
	r.wire(t)

	pw.scanAndImport(t)
	pw.setVerify(t, migrate.VerifyPassed)
	// A former parity disk whose size is not the one the import recorded fails the
	// job before it erases or records anything, which leaves the adoption pending.
	if _, err := w.db.ExecContext(ctx, "UPDATE array_settings SET migration_recorded = json_set(migration_recorded, '$[0].size', 1)"); err != nil {
		t.Fatal(err)
	}
	status, body := w.doBody(t, http.MethodPost, "/migrate/initialize-parity", `{"confirmation":"ERASE /dev/sdb"}`)
	if status != http.StatusOK {
		t.Fatalf("POST /migrate/initialize-parity = %d %s", status, body)
	}
	var queued struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &queued)
	if done := w.awaitJobByID(t, queued.ID); done.Status != job.StatusFailed || !strings.Contains(done.ErrorMessage, "the size of") {
		t.Fatalf("migration_parity job = %s %q, want it failed on the changed size before the point of no return", done.Status, done.ErrorMessage)
	}
	pw.assertNothingFormatted(t, "the failed parity initialisation")
	if pending, err := w.arrays.MigrationPending(ctx); err != nil || !pending {
		t.Fatalf("MigrationPending = %v, %v after the failed job, want the adoption still pending", pending, err)
	}
	if c := r.checklist(t); c.Finished || len(c.Items) != 0 {
		t.Fatalf("checklist while the adoption is pending = %+v", c)
	}

	status, body = w.doBody(t, http.MethodPost, "/migrate/import", undoBody)
	if status != http.StatusOK {
		t.Fatalf("POST /migrate/import with undo = %d %s", status, body)
	}
	_ = json.Unmarshal(body, &queued)
	if done := w.awaitJobByID(t, queued.ID); done.Status != job.StatusSucceeded {
		t.Fatalf("undo job = %s %s", done.Status, done.ErrorMessage)
	}
	if exists, err := w.arrays.Exists(ctx); err != nil || exists {
		t.Fatalf("array exists = %v, %v after the undo", exists, err)
	}
	if c := r.checklist(t); c.Finished || len(c.Items) != 0 {
		t.Fatalf("checklist after the undo = %+v, want not finished with no items", c)
	}
	if status, body := r.acknowledgeUserScripts(t); status != http.StatusConflict || !strings.Contains(body, "migration_not_finished") {
		t.Fatalf("acknowledging after the undo = %d %s, want 409 migration_not_finished", status, body)
	}
	if got, found, err := store.NewMigrationSessionStore(w.db).Get(ctx); err != nil || (found && got.Checklist != nil) {
		t.Errorf("a refused acknowledgement was stored: %+v %v %v", got, found, err)
	}

	if err := w.arrays.PutArray(ctx, store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "50G", CreatedAt: time.Now().UTC()}, []store.ArrayDisk{
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-new", Mountpoint: "/mnt/disk1"},
	}); err != nil {
		t.Fatal(err)
	}
	if c := r.checklist(t); c.Finished || len(c.Items) != 0 {
		t.Fatalf("checklist for an array created by hand after the undo = %+v, want not finished", c)
	}
	if status, body := r.acknowledgeUserScripts(t); status != http.StatusConflict {
		t.Fatalf("acknowledging for an array created by hand = %d %s, want 409", status, body)
	}
}

// The real point of no return is what finishes the checklist: its last step
// stamps the array record, and the checklist's finishedAt is that stamp.
func TestMigrationChecklistWiring_ThePointOfNoReturnStampsTheFinishThatTheChecklistReads(t *testing.T) {
	pw := wireParity(t)
	w := pw.w
	ctx := context.Background()
	r := &checklistRig{containersWiringHarness: w, sender: &scriptedSender{}}
	w.handler.Notify = notify.NewService(notify.NewStore(w.db), nil, notify.Senders{notify.ChannelNtfy: r.sender})
	w.handler.Schedules = api.NewScheduleService(api.NewScheduleStore(w.db), api.NewSettingsStore(w.db))
	r.wire(t)

	pw.scanAndImport(t)
	pw.setVerify(t, migrate.VerifyPassed)
	if c := r.checklist(t); c.Finished {
		t.Fatalf("checklist for a verified, pending adoption = %+v", c)
	}
	before := time.Now().UTC().Truncate(time.Second)
	status, body := w.doBody(t, http.MethodPost, "/migrate/initialize-parity", `{"confirmation":"ERASE /dev/sdb"}`)
	if status != http.StatusOK {
		t.Fatalf("POST /migrate/initialize-parity = %d %s", status, body)
	}
	var queued struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &queued)
	if done := w.awaitJobByID(t, queued.ID); done.Status != job.StatusSucceeded {
		t.Fatalf("migration_parity job = %s %s", done.Status, done.ErrorMessage)
	}
	stamp, found, err := w.arrays.MigrationFinishedAt(ctx)
	if err != nil || !found || stamp.Before(before) || stamp.After(time.Now()) {
		t.Fatalf("MigrationFinishedAt = %v %v %v, want a time since %v", stamp, found, err, before)
	}
	c := r.checklist(t)
	if !c.Finished || len(c.Items) != 7 || !c.FinishedAt.Value.Equal(stamp) {
		t.Fatalf("checklist after the point of no return = %+v, want finished at %v with 7 items", c, stamp)
	}
	if status, body := r.acknowledgeUserScripts(t); status != http.StatusOK {
		t.Fatalf("acknowledging after the point of no return = %d %s, want 200", status, body)
	}
}

func TestMigrationChecklistWiring_AChannelChangedInTheSecondOfItsTestIsNotTested(t *testing.T) {
	r := newChecklistRig(t)
	r.wire(t)
	r.finishMigration(t, 10)
	in := notify.ChannelInput{Name: "phone", Type: notify.ChannelNtfy, Enabled: true, Config: notify.ChannelConfig{NtfyURL: "https://ntfy.example", NtfyTopic: "hoserva"}}
	ch, err := r.handler.Notify.CreateChannel(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if status, body := r.do(t, http.MethodPost, "/notifications/channels/"+ch.ID+"/test"); status != http.StatusOK || !strings.Contains(string(body), `"success":true`) {
		t.Fatalf("a succeeding test = %d %s", status, body)
	}
	if got := r.item(t, apiv1.MigrationChecklistItemIdNotifications); got.Status != apiv1.MigrationChecklistItemStatusDone {
		t.Fatalf("notifications after a test = %+v, want done", got)
	}
	in.Name = "phone, renamed"
	if _, err := r.handler.Notify.UpdateChannel(context.Background(), ch.ID, in); err != nil {
		t.Fatal(err)
	}
	if got := r.item(t, apiv1.MigrationChecklistItemIdNotifications); got.Status != apiv1.MigrationChecklistItemStatusTodo || got.Notifications.Value.Tested != 0 {
		t.Errorf("notifications after the channel changed in the second of its test = %+v %+v, want todo", got, got.Notifications.Value)
	}
}

func TestMigrationChecklistWiring_ANonFinishedArrayRecordKeepsTheChecklistFromApplying(t *testing.T) {
	r := newChecklistRig(t)
	r.wire(t)
	r.addJob(t, "parity", job.TypeMigrationParity, job.ClassTopology, nil, 10, 11)
	if _, err := r.db.ExecContext(context.Background(), "UPDATE array_settings SET migration_pending = 1"); err != nil {
		t.Fatal(err)
	}
	if c := r.checklist(t); c.Finished || len(c.Items) != 0 {
		t.Fatalf("checklist while the array record has the migration pending = %+v, want not finished with no items", c)
	}
	if status, body := r.do(t, http.MethodPost, "/migrate/checklist/user_scripts/acknowledge"); status != http.StatusConflict || !strings.Contains(string(body), "migration_not_finished") {
		t.Fatalf("acknowledging while the migration is pending = %d %s, want 409 migration_not_finished", status, body)
	}
}
