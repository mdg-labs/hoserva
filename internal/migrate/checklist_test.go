package migrate

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)

func at(h int) time.Time { return t0.Add(time.Duration(h) * time.Hour) }

// fakeRecords is the checklist's record sources as a test scripts them: the
// succeeded jobs by type, oldest first, the array record's finished-migration
// stamp (zero while there is none), the channels and the schedule.
type fakeRecords struct {
	jobs       map[string][]JobRecord
	finishedAt time.Time
	channels   []ChannelRecord
	schedule   ScheduleRecord

	jobReads, channelReads, scheduleReads int
	failJobs                              string
	failFinished                          bool
}

func (f *fakeRecords) sources() ChecklistSources {
	return ChecklistSources{
		Jobs: func(_ context.Context, jobType string, visit func(JobRecord) bool) error {
			f.jobReads++
			if f.failJobs == jobType {
				return errors.New("the job store is gone")
			}
			for _, j := range f.jobs[jobType] {
				if visit(j) {
					return nil
				}
			}
			return nil
		},
		Finished: func(context.Context) (time.Time, bool, error) {
			if f.failFinished {
				return time.Time{}, false, errors.New("the array record is gone")
			}
			return f.finishedAt, !f.finishedAt.IsZero(), nil
		},
		Channels: func(context.Context) ([]ChannelRecord, error) { f.channelReads++; return f.channels, nil },
		Schedule: func(context.Context) (ScheduleRecord, error) { f.scheduleReads++; return f.schedule, nil },
	}
}

func finishedRecords() *fakeRecords {
	return &fakeRecords{jobs: map[string][]JobRecord{}, finishedAt: at(0)}
}

func buildFrom(t *testing.T, f *fakeRecords, rec ChecklistRecord, r *Report) Checklist {
	t.Helper()
	facts, err := f.sources().Facts(ctx0)
	if err != nil {
		t.Fatal(err)
	}
	return BuildChecklist(facts, rec, r)
}

func itemOf(t *testing.T, c Checklist, id ChecklistItemID) ChecklistItem {
	t.Helper()
	it := c.Item(id)
	if it == nil {
		t.Fatalf("the checklist has no %s item: %+v", id, c.Items)
	}
	return *it
}

func TestChecklist_AppliesExactlyWhenTheArrayRecordHasAFinishedStamp(t *testing.T) {
	f := &fakeRecords{jobs: map[string][]JobRecord{
		JobTypeSync:  {{ID: "s", CreatedAt: at(2), FinishedAt: at(3)}},
		JobTypeScrub: {{ID: "c", ScrubPercent: 100, AllBlocks: true, StartedAt: at(4), FinishedAt: at(5)}},
	}}
	c := buildFrom(t, f, ChecklistRecord{}, nil)
	if c.Finished || !c.FinishedAt.IsZero() || len(c.Items) != 0 {
		t.Fatalf("checklist = %+v, want not finished and no items", c)
	}
	if f.channelReads != 0 || f.scheduleReads != 0 || f.jobReads != 0 {
		t.Errorf("jobs read %d times, channels %d and the schedule %d before the migration finished", f.jobReads, f.channelReads, f.scheduleReads)
	}

	f.finishedAt = at(1)
	c = buildFrom(t, f, ChecklistRecord{}, nil)
	if !c.Finished || !c.FinishedAt.Equal(at(1)) {
		t.Fatalf("checklist = %+v, want finished at the stamp", c)
	}
	var ids []ChecklistItemID
	for _, it := range c.Items {
		ids = append(ids, it.ID)
	}
	if !reflect.DeepEqual(ids, ChecklistItemIDs) {
		t.Errorf("items = %v, want %v", ids, ChecklistItemIDs)
	}

	s := newChecklistService(t, f)
	if _, err := s.AcknowledgeChecklistItem(ctx0, ItemUserScripts, "alice"); err != nil {
		t.Errorf("acknowledging once the stamp exists = %v, want it accepted", err)
	}
}

func TestChecklist_AnUnreadableFinishedStampIsAnErrorNeverAFinishedMigration(t *testing.T) {
	f := finishedRecords()
	f.failFinished = true
	if _, err := f.sources().Facts(ctx0); err == nil {
		t.Fatal("an unreadable stamp did not fail the checklist")
	}
	s := newChecklistService(t, f)
	if _, err := s.AcknowledgeChecklistItem(ctx0, ItemUserScripts, "alice"); err == nil || errors.Is(err, ErrMigrationNotFinished) {
		t.Errorf("acknowledging with an unreadable stamp = %v, want the read error", err)
	}
}

func TestChecklist_AnUnreadableJobRecordIsAnErrorNeverAnEmptyChecklist(t *testing.T) {
	for _, jobType := range []string{JobTypeSync, JobTypeShareRelocation, JobTypeFix} {
		f := finishedRecords()
		f.failJobs = jobType
		if _, err := f.sources().Facts(ctx0); err == nil || !strings.Contains(err.Error(), jobType) {
			t.Errorf("a failing %s read = %v, want an error naming it", jobType, err)
		}
	}
	f := finishedRecords()
	f.jobs[JobTypeSync] = []JobRecord{{ID: "s", CreatedAt: at(2), FinishedAt: at(3)}}
	f.failJobs = JobTypeScrub
	if _, err := f.sources().Facts(ctx0); err == nil {
		t.Error("a failing scrub read did not fail the checklist")
	}
	if _, err := (ChecklistSources{}).Facts(ctx0); !errors.Is(err, ErrChecklistNotConfigured) {
		t.Errorf("unconfigured sources = %v, want ErrChecklistNotConfigured", err)
	}
}

func TestChecklist_InitialSyncIsTheFirstRealSyncSinceTheMigrationFinished(t *testing.T) {
	f := finishedRecords()
	f.jobs[JobTypeSync] = []JobRecord{
		{ID: "before", CreatedAt: at(-5), FinishedAt: at(-4)},
		{ID: "dry-run", CreatedAt: at(1), FinishedAt: at(2), DryRun: true},
		{ID: "unfinished", CreatedAt: at(2)},
		{ID: "initial", CreatedAt: at(3), FinishedAt: at(4)},
		{ID: "later", CreatedAt: at(30), FinishedAt: at(31)},
	}
	it := itemOf(t, buildFrom(t, f, ChecklistRecord{}, nil), ItemInitialSync)
	if it.Status != ItemDone || it.JobID != "initial" || !it.DoneAt.Equal(at(4)) || it.Acknowledgeable {
		t.Errorf("initial sync = %+v, want done from job initial, finished at 4h, not acknowledgeable", it)
	}

	f.jobs[JobTypeSync] = f.jobs[JobTypeSync][:3]
	it = itemOf(t, buildFrom(t, f, ChecklistRecord{}, nil), ItemInitialSync)
	if it.Status != ItemTodo || it.JobID != "" || it.Acknowledgeable {
		t.Errorf("initial sync with only a dry run, an older sync and an unfinished one = %+v, want todo", it)
	}
}

func TestChecklist_FullScrubIsAScrubOfEveryBlockAt100PercentStartedAfterTheInitialSyncEnded(t *testing.T) {
	f := finishedRecords()
	f.jobs[JobTypeSync] = []JobRecord{{ID: "initial", CreatedAt: at(2), FinishedAt: at(5)}}
	f.jobs[JobTypeScrub] = []JobRecord{
		{ID: "partial", ScrubPercent: 8, AllBlocks: true, CreatedAt: at(6), StartedAt: at(6), FinishedAt: at(7)},
		{ID: "before-sync-ended", ScrubPercent: 100, AllBlocks: true, CreatedAt: at(1), StartedAt: at(4), FinishedAt: at(4)},
		{ID: "not-started", ScrubPercent: 100, AllBlocks: true, CreatedAt: at(6)},
		{ID: "skips-recent-blocks", ScrubPercent: 100, CreatedAt: at(6), StartedAt: at(6), FinishedAt: at(7)},
	}
	it := itemOf(t, buildFrom(t, f, ChecklistRecord{}, nil), ItemFullScrub)
	if it.Status != ItemTodo {
		t.Errorf("full scrub = %+v, want todo: a partial scrub, one that started before the sync ended, one that never started and a 100 percent one that skips recent blocks do not count", it)
	}

	f.jobs[JobTypeScrub] = append(f.jobs[JobTypeScrub], JobRecord{ID: "full", ScrubPercent: 100, AllBlocks: true, CreatedAt: at(8), StartedAt: at(9), FinishedAt: at(12)})
	it = itemOf(t, buildFrom(t, f, ChecklistRecord{}, nil), ItemFullScrub)
	if it.Status != ItemDone || it.JobID != "full" || !it.DoneAt.Equal(at(12)) || it.Acknowledgeable {
		t.Errorf("full scrub = %+v, want done from job full", it)
	}

	f.jobs[JobTypeSync] = nil
	it = itemOf(t, buildFrom(t, f, ChecklistRecord{}, nil), ItemFullScrub)
	if it.Status != ItemTodo {
		t.Errorf("full scrub without a succeeded initial sync = %+v, want todo", it)
	}
}

func reviewWithRoles(roles ...UnraidRole) *Report {
	r := &Report{Review: &Review{}}
	for _, role := range roles {
		r.Review.Disks = append(r.Review.Disks, ReviewDisk{UnraidRole: role})
	}
	return r
}

func TestChecklist_AppdataIsDoneByARelocationOfAppdataToCacheAndNotApplicableWhenTheSourceHadNoCache(t *testing.T) {
	f := finishedRecords()
	f.jobs[JobTypeShareRelocation] = []JobRecord{
		{ID: "media", Share: "media", To: "cache", FinishedAt: at(2)},
		{ID: "back", Share: "appdata", To: "array", FinishedAt: at(3)},
	}
	withCache := reviewWithRoles(UnraidParity, UnraidData, UnraidCache)
	noCache := reviewWithRoles(UnraidParity, UnraidData)
	unknown := reviewWithRoles("", "")

	for name, c := range map[string]struct {
		report *Report
		want   ItemStatus
	}{
		"a cache in the capture":                   {withCache, ItemTodo},
		"no cache in the capture":                  {noCache, ItemNotApplicable},
		"a capture with no roles":                  {unknown, ItemTodo},
		"no review (a scan older than the review)": {&Report{}, ItemTodo},
		"no report": {nil, ItemTodo},
	} {
		it := itemOf(t, buildFrom(t, f, ChecklistRecord{}, c.report), ItemAppdataCache)
		if it.Status != c.want || it.JobID != "" || it.Acknowledgeable {
			t.Errorf("%s: appdata = %+v, want %s from no job", name, it, c.want)
		}
	}

	f.jobs[JobTypeShareRelocation] = append(f.jobs[JobTypeShareRelocation], JobRecord{ID: "appdata-to-cache", Share: "appdata", To: "cache", FinishedAt: at(4)})
	for name, r := range map[string]*Report{"a cache": withCache, "no cache": noCache} {
		it := itemOf(t, buildFrom(t, f, ChecklistRecord{}, r), ItemAppdataCache)
		if it.Status != ItemDone || it.JobID != "appdata-to-cache" || !it.DoneAt.Equal(at(4)) {
			t.Errorf("%s: appdata = %+v, want done from the relocation: a record is never overridden", name, it)
		}
	}
}

func TestChecklist_NotificationsNeedAnEnabledChannelWhoseTestSucceededSinceItLastChanged(t *testing.T) {
	report := &Report{}
	report.Import.Schedules.NotifyAgents = []string{"Pushover", "Slack"}
	f := finishedRecords()

	it := itemOf(t, buildFrom(t, f, ChecklistRecord{}, report), ItemNotifications)
	if it.Status != ItemTodo || it.Notifications.Channels != 0 || !reflect.DeepEqual(it.Notifications.Agents, []string{"Pushover", "Slack"}) {
		t.Errorf("no channels = %+v %+v, want todo, naming the Unraid agents", it, it.Notifications)
	}

	f.channels = []ChannelRecord{
		{ID: "untested", Enabled: true, UpdatedAt: at(1)},
		{ID: "disabled", Enabled: false, UpdatedAt: at(1)},
		{ID: "edited-after", Enabled: true, UpdatedAt: at(5)},
	}
	rec := ChecklistRecord{ChannelTests: map[string]time.Time{"disabled": at(3), "edited-after": at(4), "deleted": at(3)}}
	it = itemOf(t, buildFrom(t, f, rec, report), ItemNotifications)
	if it.Status != ItemTodo || it.Notifications.Channels != 3 || it.Notifications.Tested != 0 || it.Acknowledgeable {
		t.Errorf("channels untested, disabled or edited after their test = %+v %+v, want todo with 3 channels and none tested", it, it.Notifications)
	}

	f.channels = []ChannelRecord{
		{ID: "same-second", Enabled: true, UpdatedAt: at(5).Truncate(time.Second)},
	}
	rec = ChecklistRecord{ChannelTests: map[string]time.Time{"same-second": at(5).Add(300 * time.Millisecond)}}
	it = itemOf(t, buildFrom(t, f, rec, report), ItemNotifications)
	if it.Status != ItemTodo || it.Notifications.Tested != 0 {
		t.Errorf("a channel changed in the same second as its test = %+v %+v, want todo: its updated_at is stored to the second", it, it.Notifications)
	}
	rec.ChannelTests["same-second"] = at(5).Add(1100 * time.Millisecond)
	if it = itemOf(t, buildFrom(t, f, rec, report), ItemNotifications); it.Status != ItemDone {
		t.Errorf("a channel tested in the second after it changed = %+v, want done", it)
	}

	f.channels = []ChannelRecord{
		{ID: "untested", Enabled: true, UpdatedAt: at(1)},
		{ID: "disabled", Enabled: false, UpdatedAt: at(1)},
		{ID: "edited-after", Enabled: true, UpdatedAt: at(5)},
		{ID: "good", Enabled: true, UpdatedAt: at(2)},
	}
	rec = ChecklistRecord{ChannelTests: map[string]time.Time{"disabled": at(3), "edited-after": at(4), "deleted": at(3), "good": at(3)}}
	it = itemOf(t, buildFrom(t, f, rec, report), ItemNotifications)
	if it.Status != ItemDone || it.Notifications.Tested != 1 || !it.DoneAt.Equal(at(3)) {
		t.Errorf("one tested channel = %+v %+v, want done at its test", it, it.Notifications)
	}
	for _, a := range it.Notifications.Agents {
		if strings.ContainsAny(a, ":/@") {
			t.Errorf("agent %q looks like more than a name", a)
		}
	}
}

func TestChecklist_SchedulesNeedMoverSyncAndScrubAndOfferWhatUnraidHad(t *testing.T) {
	report := &Report{}
	report.Import.Schedules = Schedules{
		MoverCron: "40 3 * * *", SpindownDelay: "30",
		ParityCheck: ParityCheckSchedule{Found: true, Mode: "1", Hour: "0 3", Correcting: false},
	}
	f := finishedRecords()
	f.schedule = ScheduleRecord{ChainStartTime: "02:00", WeeklyScrubDay: 0, Mover: true, Sync: false, Scrub: true}

	it := itemOf(t, buildFrom(t, f, ChecklistRecord{}, report), ItemSchedules)
	if it.Status != ItemTodo || it.Acknowledgeable {
		t.Errorf("schedules with the sync step off = %+v, want todo", it)
	}
	d := it.Schedules
	if d.Sync || !d.Mover || !d.Scrub || d.ChainStartTime != "02:00" {
		t.Errorf("detail = %+v", d)
	}
	o := d.Offers
	if o.MoverCron != "40 3 * * *" || o.MoverTime != "03:40" || o.SpindownDelay != "30" || o.ParityCheck == nil || o.ParityCheck.Mode != "1" || !o.ScrubReportOnly {
		t.Errorf("offers = %+v, want the mover cron and time, the spin-down delay and a report-only scrub from a non-correcting check", o)
	}

	for _, step := range []string{"mover", "sync", "scrub"} {
		g := f.schedule
		g.Mover, g.Sync, g.Scrub = true, true, true
		switch step {
		case "mover":
			g.Mover = false
		case "sync":
			g.Sync = false
		case "scrub":
			g.Scrub = false
		}
		f2 := finishedRecords()
		f2.schedule = g
		if it := itemOf(t, buildFrom(t, f2, ChecklistRecord{}, report), ItemSchedules); it.Status != ItemTodo {
			t.Errorf("with the %s step off, schedules = %v, want todo", step, it.Status)
		}
	}

	f.schedule.Sync = true
	if it := itemOf(t, buildFrom(t, f, ChecklistRecord{}, report), ItemSchedules); it.Status != ItemDone {
		t.Errorf("with all three steps on, schedules = %+v, want done", it)
	}

	correcting := &Report{}
	correcting.Import.Schedules.ParityCheck = ParityCheckSchedule{Found: true, Correcting: true}
	if o := itemOf(t, buildFrom(t, f, ChecklistRecord{}, correcting), ItemSchedules).Schedules.Offers; o.ScrubReportOnly || o.ParityCheck == nil {
		t.Errorf("a correcting check's offers = %+v, want a scrub that is not report-only", o)
	}
	if o := itemOf(t, buildFrom(t, f, ChecklistRecord{}, nil), ItemSchedules).Schedules.Offers; o != (ScheduleOffers{}) {
		t.Errorf("offers without a report = %+v, want none", o)
	}
}

func TestDailyTime(t *testing.T) {
	for cron, want := range map[string]string{
		"40 3 * * *":   "03:40",
		"0 0 * * *":    "00:00",
		"5 23 * * *":   "23:05",
		"*/5 * * * *":  "",
		"0 3 * * 1":    "",
		"0 3 1 * *":    "",
		"60 3 * * *":   "",
		"0 24 * * *":   "",
		"@daily":       "",
		"":             "",
		"0 3 * * * *":  "",
		"0 3,15 * * *": "",
	} {
		if got := dailyTime(cron); got != want {
			t.Errorf("dailyTime(%q) = %q, want %q", cron, got, want)
		}
	}
}

func TestChecklist_UserScriptsAndTheRestoreDrillAreTheOnlyItemsAUserAcknowledges(t *testing.T) {
	report := &Report{}
	report.Import.UserScripts = []UserScript{{Name: "nightly-report", Schedule: "30 2 * * *"}, {Name: "Weekly cleanup"}}
	f := finishedRecords()
	f.jobs[JobTypeFix] = []JobRecord{{ID: "fix-1", FinishedAt: at(8)}, {ID: "fix-2", FinishedAt: at(9)}}

	c := buildFrom(t, f, ChecklistRecord{}, report)
	scripts := itemOf(t, c, ItemUserScripts)
	if scripts.Status != ItemTodo || !scripts.Acknowledgeable || !reflect.DeepEqual(scripts.Scripts, report.Import.UserScripts) {
		t.Errorf("user scripts = %+v, want todo, acknowledgeable, listing the scripts", scripts)
	}
	drill := itemOf(t, c, ItemRestoreDrill)
	if drill.Status != ItemTodo || !drill.Acknowledgeable || drill.JobID != "fix-2" || drill.Ack != nil {
		t.Errorf("restore drill = %+v, want todo, acknowledgeable, showing the latest fix job", drill)
	}
	for _, it := range c.Items {
		if it.ID != ItemUserScripts && it.ID != ItemRestoreDrill && it.Acknowledgeable {
			t.Errorf("%s is acknowledgeable although a record decides it", it.ID)
		}
	}

	facts, _ := f.sources().Facts(ctx0)
	for _, id := range []ChecklistItemID{ItemAppdataCache, ItemInitialSync, ItemFullScrub, ItemNotifications, ItemSchedules} {
		if _, _, err := c.Acknowledge(id, "alice", at(10), facts); !errors.Is(err, ErrChecklistItemHasRecord) {
			t.Errorf("acknowledging %s = %v, want ErrChecklistItemHasRecord", id, err)
		}
	}
	if _, _, err := c.Acknowledge("no-such-item", "alice", at(10), facts); !errors.Is(err, ErrChecklistItemNotFound) {
		t.Errorf("acknowledging an unknown item = %v, want ErrChecklistItemNotFound", err)
	}
	if _, _, err := (Checklist{}).Acknowledge(ItemUserScripts, "alice", at(10), ChecklistFacts{}); !errors.Is(err, ErrMigrationNotFinished) {
		t.Errorf("acknowledging before the migration finished = %v, want ErrMigrationNotFinished", err)
	}

	ack, isNew, err := c.Acknowledge(ItemRestoreDrill, "alice", at(10), facts)
	if err != nil || !isNew || ack.By != "alice" || !ack.At.Equal(at(10)) || ack.JobID != "fix-2" {
		t.Fatalf("acknowledging the restore drill = %+v %v %v, want alice's, at 10h, recording fix-2", ack, isNew, err)
	}
	rec := ChecklistRecord{Acks: map[ChecklistItemID]ChecklistAck{ItemRestoreDrill: ack}}
	c = buildFrom(t, f, rec, report)
	drill = itemOf(t, c, ItemRestoreDrill)
	if drill.Status != ItemDone || drill.Acknowledgeable || drill.Ack == nil || drill.Ack.By != "alice" || drill.JobID != "fix-2" || !drill.DoneAt.Equal(at(10)) {
		t.Errorf("restore drill after the acknowledgement = %+v", drill)
	}
	again, isNew, err := c.Acknowledge(ItemRestoreDrill, "bob", at(11), facts)
	if err != nil || isNew || again.By != "alice" {
		t.Errorf("a second acknowledgement = %+v %v %v, want the first kept", again, isNew, err)
	}

	f.jobs[JobTypeFix] = nil
	facts, _ = f.sources().Facts(ctx0)
	ack, _, err = buildFrom(t, f, ChecklistRecord{}, report).Acknowledge(ItemRestoreDrill, "alice", at(10), facts)
	if err != nil || ack.JobID != "" {
		t.Errorf("acknowledging with no fix job = %+v %v, want no job recorded", ack, err)
	}
}

func newChecklistService(t *testing.T, f *fakeRecords) *Service {
	t.Helper()
	s := &Service{Dir: filepath.Join(t.TempDir(), "migrate"), Sessions: newSessions(t), ChecklistRecords: f.sources()}
	now := at(20)
	s.Now = func() time.Time { return now }
	return s
}

func TestServiceChecklist_AcknowledgementsAreStoredWithWhoAndWhenAndSurviveForgettingTheSession(t *testing.T) {
	f := finishedRecords()
	s := newChecklistService(t, f)

	if _, err := s.AcknowledgeChecklistItem(ctx0, ItemUserScripts, "alice"); err != nil {
		t.Fatal(err)
	}
	it, err := s.AcknowledgeChecklistItem(ctx0, ItemUserScripts, "bob")
	if err != nil || it.Ack == nil || it.Ack.By != "alice" || !it.Ack.At.Equal(at(20)) {
		t.Fatalf("second acknowledgement = %+v %v, want alice's, at 20h", it, err)
	}
	if _, err := s.AcknowledgeChecklistItem(ctx0, ItemInitialSync, "alice"); !errors.Is(err, ErrChecklistItemHasRecord) {
		t.Errorf("acknowledging a record-derived item = %v", err)
	}

	row, found, err := s.Sessions.Get(ctx0)
	if err != nil || !found || !strings.Contains(string(row.Checklist), `"by":"alice"`) {
		t.Fatalf("the stored row = %+v %v %v, want the acknowledgement in it", row, found, err)
	}

	if err := s.Forget(ctx0); err != nil {
		t.Fatalf("Forget = %v", err)
	}
	c, err := s.Checklist(ctx0)
	if err != nil {
		t.Fatal(err)
	}
	if got := itemOf(t, c, ItemUserScripts); got.Status != ItemDone || got.Ack == nil || got.Ack.By != "alice" {
		t.Errorf("after the session was forgotten, user scripts = %+v, want the acknowledgement kept", got)
	}
	if st, err := s.State(ctx0); err != nil || st.Phase != PhaseNone || st.Report != nil || st.Source != nil {
		t.Errorf("State after Forget = %+v %v, want an empty session", st, err)
	}
}

func TestServiceChecklist_RefusesAcknowledgingBeforeTheMigrationFinishedAndStoresNothing(t *testing.T) {
	f := &fakeRecords{}
	s := newChecklistService(t, f)
	if _, err := s.AcknowledgeChecklistItem(ctx0, ItemUserScripts, "alice"); !errors.Is(err, ErrMigrationNotFinished) {
		t.Fatalf("AcknowledgeChecklistItem = %v, want ErrMigrationNotFinished", err)
	}
	if _, found, err := s.Sessions.Get(ctx0); err != nil || found {
		t.Errorf("a refused acknowledgement left a row: %v %v", found, err)
	}
	c, err := s.Checklist(ctx0)
	if err != nil || c.Finished || len(c.Items) != 0 {
		t.Errorf("Checklist = %+v %v, want none yet", c, err)
	}
}

func TestServiceChecklist_AChannelTestIsRecordedAndCountsUntilTheChannelChanges(t *testing.T) {
	f := finishedRecords()
	f.channels = []ChannelRecord{{ID: "ch1", Enabled: true, UpdatedAt: at(2)}}
	s := newChecklistService(t, f)

	c, err := s.Checklist(ctx0)
	if err != nil || itemOf(t, c, ItemNotifications).Status != ItemTodo {
		t.Fatalf("before any test: %+v %v", c, err)
	}
	if err := s.RecordChannelTest(ctx0, "ch1"); err != nil {
		t.Fatal(err)
	}
	c, err = s.Checklist(ctx0)
	if got := itemOf(t, c, ItemNotifications); err != nil || got.Status != ItemDone || !got.DoneAt.Equal(at(20)) {
		t.Fatalf("after a recorded test: %+v %v", got, err)
	}

	f.channels[0].UpdatedAt = at(21)
	c, _ = s.Checklist(ctx0)
	if got := itemOf(t, c, ItemNotifications); got.Status != ItemTodo {
		t.Errorf("after the channel changed, notifications = %+v, want todo", got)
	}
}

func TestServiceChecklist_ARecordWrittenByAScanSaveOrAnAcknowledgementDoesNotErase(t *testing.T) {
	f := finishedRecords()
	s := newChecklistService(t, f)
	if err := s.RecordChannelTest(ctx0, "ch1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcknowledgeChecklistItem(ctx0, ItemRestoreDrill, "alice"); err != nil {
		t.Fatal(err)
	}
	sess, err := s.load(ctx0)
	if err != nil {
		t.Fatal(err)
	}
	sess.Source = &SourceInfo{File: "upload-a.zip", Size: 1, ReceivedAt: at(1)}
	if err := s.save(ctx0, sess); err != nil {
		t.Fatal(err)
	}
	rec, err := decodeChecklistRecord(func() []byte { r, _, _ := s.Sessions.Get(ctx0); return r.Checklist }())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rec.Acks[ItemRestoreDrill]; !ok || len(rec.ChannelTests) != 1 {
		t.Errorf("record after a session save = %+v, want the acknowledgement and the channel test kept", rec)
	}
}
