package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Phase D's closing steps (doc 05 §4 steps 18 and 21 to 25) as a checklist. An
// item is done only when a record says so: a succeeded job, a channel whose
// test succeeded, a schedule that is enabled. The two items no record can show
// (the User Scripts inventory was read, and a restore drill was done) are done
// by the user's acknowledgement, which stores who made it and when; an item with
// a record is never acknowledged by hand.

var (
	// ErrChecklistNotConfigured is returned when this daemon has no record
	// sources for the checklist.
	ErrChecklistNotConfigured = errors.New("this daemon cannot build the migration checklist")
	// ErrMigrationNotFinished is returned for an acknowledgement while the array
	// record has no finished-migration stamp: the checklist applies only to an
	// array whose migration passed its point of no return.
	ErrMigrationNotFinished = errors.New("the migration has not finished: the checklist applies once the migration is past its point of no return")
	// ErrChecklistItemNotFound is returned for an item the checklist does not have.
	ErrChecklistItemNotFound = errors.New("the migration checklist has no such item")
	// ErrChecklistItemHasRecord is returned for an acknowledgement of an item whose
	// state comes from a record.
	ErrChecklistItemHasRecord = errors.New("this checklist item is derived from a record and cannot be acknowledged by hand")
)

// ChecklistItemID names a checklist item.
type ChecklistItemID string

const (
	ItemAppdataCache  ChecklistItemID = "appdata_cache"
	ItemInitialSync   ChecklistItemID = "initial_sync"
	ItemFullScrub     ChecklistItemID = "full_scrub"
	ItemNotifications ChecklistItemID = "notifications"
	ItemSchedules     ChecklistItemID = "schedules"
	ItemUserScripts   ChecklistItemID = "user_scripts"
	ItemRestoreDrill  ChecklistItemID = "restore_drill"
)

// ChecklistItemIDs are the items in the order the checklist lists them.
var ChecklistItemIDs = []ChecklistItemID{ItemAppdataCache, ItemInitialSync, ItemFullScrub, ItemNotifications, ItemSchedules, ItemUserScripts, ItemRestoreDrill}

// ItemStatus is where one item stands.
type ItemStatus string

const (
	ItemTodo          ItemStatus = "todo"
	ItemDone          ItemStatus = "done"
	ItemNotApplicable ItemStatus = "not_applicable"
)

// The job types the checklist reads and the share and direction the appdata
// item looks for; they are internal/job's Type values and the share
// relocation's own, which a test in cmd/hoservad checks against the real ones.
const (
	JobTypeSync            = "sync"
	JobTypeScrub           = "scrub"
	JobTypeFix             = "fix"
	JobTypeShareRelocation = "share_relocation"

	appdataShare      = "appdata"
	relocationToCache = "cache"

	// FullScrubPercent is the scrub percentage of a full scrub. Only a scrub
	// that also asked for blocks of every age (job.ScrubParams.AllBlocks) checks
	// the whole array: any other skips blocks synced within the last days.
	FullScrubPercent = 100
)

// JobRecord is a succeeded job as the checklist reads it. A time that is not
// recorded is the zero time. DryRun is a sync's, ScrubPercent and AllBlocks a
// scrub's, Share and To a share relocation's and Path a fix's, the pool path of
// the one file it restored (empty for a fix of the whole array or of one disk);
// the others leave them zero.
type JobRecord struct {
	ID         string
	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time

	DryRun       bool
	ScrubPercent int
	AllBlocks    bool
	Share, To    string
	Path         string
}

// ChannelRecord is one notification channel. UpdatedAt is when its
// configuration last changed, stored to the second: a test vouches for the
// channel only when its time, truncated to the second, is after it.
type ChannelRecord struct {
	ID        string
	Enabled   bool
	UpdatedAt time.Time
}

// ScheduleRecord is the nightly maintenance chain: which of the steps the
// checklist asks about are enabled.
type ScheduleRecord struct {
	ChainStartTime string
	WeeklyScrubDay int
	Mover          bool
	Sync           bool
	Scrub          bool
}

// ChecklistSources are where the checklist reads its records. Jobs calls visit
// for the succeeded jobs of a type, oldest first, until visit returns true.
// Finished returns when the migration finished and false while there is no
// record of it (store.ArrayStore.MigrationFinishedAt): an error is never
// "finished".
type ChecklistSources struct {
	Jobs     func(ctx context.Context, jobType string, visit func(JobRecord) (stop bool)) error
	Finished func(ctx context.Context) (at time.Time, finished bool, err error)
	Channels func(ctx context.Context) ([]ChannelRecord, error)
	Schedule func(ctx context.Context) (ScheduleRecord, error)
}

func (c ChecklistSources) configured() bool {
	return c.Jobs != nil && c.Finished != nil && c.Channels != nil && c.Schedule != nil
}

// ChecklistFacts is what the records show. Finished is false until the array
// record says the migration finished, and then nothing else is read.
type ChecklistFacts struct {
	Finished          bool
	FinishedAt        time.Time
	AppdataRelocation *JobRecord
	InitialSync       *JobRecord
	FullScrub         *JobRecord
	LatestFix         *JobRecord
	Channels          []ChannelRecord
	Schedule          ScheduleRecord
}

func firstJob(ctx context.Context, src ChecklistSources, jobType string, match func(JobRecord) bool) (*JobRecord, error) {
	var found *JobRecord
	err := src.Jobs(ctx, jobType, func(j JobRecord) bool {
		if match(j) {
			found = &j
			return true
		}
		return false
	})
	if err != nil {
		return nil, fmt.Errorf("reading the succeeded %s jobs: %w", jobType, err)
	}
	return found, nil
}

// Facts reads the records. The migration is finished exactly when the array
// record carries the stamp FinishMigration writes in the statement that ends the
// point of no return (store.ArrayStore.MigrationFinishedAt); a migration that is
// pending, part-way through, or was undone has none, and then nothing else is
// read. The initial sync is the first real (not dry-run) sync that succeeded
// after that moment, and the full scrub the first 100 percent scrub of blocks of
// every age that started after the initial sync ended.
func (c ChecklistSources) Facts(ctx context.Context) (ChecklistFacts, error) {
	if !c.configured() {
		return ChecklistFacts{}, ErrChecklistNotConfigured
	}
	var f ChecklistFacts
	at, finished, err := c.Finished(ctx)
	if err != nil {
		return f, fmt.Errorf("reading when the migration finished: %w", err)
	}
	if !finished {
		return f, nil
	}
	f.Finished, f.FinishedAt = true, at
	if f.AppdataRelocation, err = firstJob(ctx, c, JobTypeShareRelocation, func(j JobRecord) bool {
		return j.Share == appdataShare && j.To == relocationToCache
	}); err != nil {
		return f, err
	}
	if f.InitialSync, err = firstJob(ctx, c, JobTypeSync, func(j JobRecord) bool {
		return !j.DryRun && !j.FinishedAt.IsZero() && !j.CreatedAt.Before(f.FinishedAt)
	}); err != nil {
		return f, err
	}
	if f.InitialSync != nil {
		if f.FullScrub, err = firstJob(ctx, c, JobTypeScrub, func(j JobRecord) bool {
			return j.ScrubPercent >= FullScrubPercent && j.AllBlocks && !j.StartedAt.IsZero() && !j.StartedAt.Before(f.InitialSync.FinishedAt)
		}); err != nil {
			return f, err
		}
	}
	if err := c.Jobs(ctx, JobTypeFix, func(j JobRecord) bool {
		if j.Path != "" {
			latest := j
			f.LatestFix = &latest
		}
		return false
	}); err != nil {
		return f, fmt.Errorf("reading the succeeded %s jobs: %w", JobTypeFix, err)
	}
	if f.Channels, err = c.Channels(ctx); err != nil {
		return f, fmt.Errorf("reading the notification channels: %w", err)
	}
	if f.Schedule, err = c.Schedule(ctx); err != nil {
		return f, fmt.Errorf("reading the schedules: %w", err)
	}
	return f, nil
}

// ChecklistAck is a user's acknowledgement of one item.
type ChecklistAck struct {
	By string    `json:"by"`
	At time.Time `json:"at"`
	// JobID is the latest succeeded fix job of one file when the restore drill
	// was acknowledged, empty when there was none or for any other item.
	JobID string `json:"jobId,omitempty"`
}

// ChecklistRecord is what the checklist keeps itself, in the session's row
// (D4): the acknowledgements, and when each notification channel's test last
// succeeded.
type ChecklistRecord struct {
	Acks         map[ChecklistItemID]ChecklistAck `json:"acks,omitempty"`
	ChannelTests map[string]time.Time             `json:"channelTests,omitempty"`
}

// NotificationsDetail is the notifications item's evidence: how many channels
// exist, how many are enabled with a test that succeeded in a later second than
// the one they last changed in, and the Unraid notification agents the scan found, by name only.
type NotificationsDetail struct {
	Channels int
	Tested   int
	Agents   []string
}

// ScheduleOffers are the values read from Unraid that the schedules item offers
// to carry over; each is empty when the flash did not have it.
type ScheduleOffers struct {
	// MoverCron is Unraid's mover schedule, and MoverTime the time of day it
	// reads as when it is a daily "minute hour * * *" line.
	MoverCron string
	MoverTime string
	// ParityCheck is Unraid's parity-check schedule, offered as the scrub
	// schedule; ScrubReportOnly is true when the check wrote no corrections,
	// which maps to a scrub that only reports.
	ParityCheck     *ParityCheckSchedule
	ScrubReportOnly bool
	// SpindownDelay is Unraid's global spin-down delay, offered as Hoserva's
	// default.
	SpindownDelay string
}

// SchedulesDetail is the schedules item's evidence: which of the three steps
// are enabled in the nightly chain, and what Unraid had. The sync schedule has
// no Unraid counterpart, so nothing is offered for it.
type SchedulesDetail struct {
	Mover          bool
	Sync           bool
	Scrub          bool
	ChainStartTime string
	WeeklyScrubDay int
	Offers         ScheduleOffers
}

// ChecklistItem is one step of the checklist. Acknowledgeable is true only for
// an item that has no record to derive from and has not been acknowledged yet.
type ChecklistItem struct {
	ID              ChecklistItemID
	Status          ItemStatus
	Acknowledgeable bool
	// DoneAt is when the record says the item was done, or when it was
	// acknowledged; zero while it is not done.
	DoneAt time.Time
	// JobID is the job the item derives from; for the restore drill, the latest
	// succeeded fix job of one file.
	JobID string
	Ack   *ChecklistAck

	Notifications *NotificationsDetail
	Schedules     *SchedulesDetail
	Scripts       []UserScript
}

// Checklist is the post-migration checklist. Items is empty until Finished.
type Checklist struct {
	Finished   bool
	FinishedAt time.Time
	Items      []ChecklistItem
}

// Item returns the item with id, or nil when the checklist has none.
func (c Checklist) Item(id ChecklistItemID) *ChecklistItem {
	for i := range c.Items {
		if c.Items[i].ID == id {
			return &c.Items[i]
		}
	}
	return nil
}

func recordItem(id ChecklistItemID, j *JobRecord) ChecklistItem {
	it := ChecklistItem{ID: id, Status: ItemTodo}
	if j != nil {
		it.Status, it.JobID, it.DoneAt = ItemDone, j.ID, j.FinishedAt
	}
	return it
}

// sourceHadNoCache is true only when the scan read the capture's disk roles and
// none of them is a cache: a scan without that information never decides it.
func sourceHadNoCache(r *Report) bool {
	if r == nil || r.Review == nil {
		return false
	}
	known := false
	for _, d := range r.Review.Disks {
		if d.UnraidRole == "" {
			continue
		}
		known = true
		if d.UnraidRole == UnraidCache {
			return false
		}
	}
	return known
}

// dailyTime reads a "minute hour * * *" cron line as HH:MM.
func dailyTime(cron string) string {
	f := strings.Fields(cron)
	if len(f) != 5 || f[2] != "*" || f[3] != "*" || f[4] != "*" {
		return ""
	}
	minute, err1 := strconv.Atoi(f[0])
	hour, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil || minute < 0 || minute > 59 || hour < 0 || hour > 23 {
		return ""
	}
	return fmt.Sprintf("%02d:%02d", hour, minute)
}

func scheduleOffers(r *Report) ScheduleOffers {
	if r == nil {
		return ScheduleOffers{}
	}
	s := r.Import.Schedules
	o := ScheduleOffers{MoverCron: s.MoverCron, MoverTime: dailyTime(s.MoverCron), SpindownDelay: s.SpindownDelay}
	if s.ParityCheck.Found {
		p := s.ParityCheck
		o.ParityCheck = &p
		o.ScrubReportOnly = !p.Correcting
	}
	return o
}

func ackedItem(it ChecklistItem, ack *ChecklistAck, fix *JobRecord) ChecklistItem {
	it.Status = ItemTodo
	it.Acknowledgeable = true
	if ack != nil {
		it.Status, it.Acknowledgeable, it.Ack, it.DoneAt, it.JobID = ItemDone, false, ack, ack.At, ack.JobID
	} else if fix != nil && it.ID == ItemRestoreDrill {
		it.JobID = fix.ID
	}
	return it
}

// BuildChecklist derives every item from the facts, the checklist's own record
// and the scan's report (which may be nil: the offers are then empty). It does
// no IO, so the daemon and the mock API answer the same.
func BuildChecklist(f ChecklistFacts, rec ChecklistRecord, report *Report) Checklist {
	if !f.Finished {
		return Checklist{}
	}
	c := Checklist{Finished: true, FinishedAt: f.FinishedAt}

	appdata := recordItem(ItemAppdataCache, f.AppdataRelocation)
	if f.AppdataRelocation == nil && sourceHadNoCache(report) {
		appdata.Status = ItemNotApplicable
	}
	c.Items = append(c.Items, appdata, recordItem(ItemInitialSync, f.InitialSync), recordItem(ItemFullScrub, f.FullScrub))

	n := NotificationsDetail{Channels: len(f.Channels)}
	var testedAt time.Time
	for _, ch := range f.Channels {
		at, ok := rec.ChannelTests[ch.ID]
		if !ch.Enabled || !ok || !at.Truncate(time.Second).After(ch.UpdatedAt) {
			continue
		}
		n.Tested++
		if at.After(testedAt) {
			testedAt = at
		}
	}
	if report != nil {
		n.Agents = append(n.Agents, report.Import.Schedules.NotifyAgents...)
	}
	notifications := ChecklistItem{ID: ItemNotifications, Status: ItemTodo, Notifications: &n}
	if n.Tested > 0 {
		notifications.Status, notifications.DoneAt = ItemDone, testedAt
	}
	c.Items = append(c.Items, notifications)

	sd := SchedulesDetail{
		Mover: f.Schedule.Mover, Sync: f.Schedule.Sync, Scrub: f.Schedule.Scrub,
		ChainStartTime: f.Schedule.ChainStartTime, WeeklyScrubDay: f.Schedule.WeeklyScrubDay, Offers: scheduleOffers(report),
	}
	schedules := ChecklistItem{ID: ItemSchedules, Status: ItemTodo, Schedules: &sd}
	if sd.Mover && sd.Sync && sd.Scrub {
		schedules.Status = ItemDone
	}
	c.Items = append(c.Items, schedules)

	scripts := ChecklistItem{ID: ItemUserScripts}
	if report != nil {
		scripts.Scripts = append(scripts.Scripts, report.Import.UserScripts...)
	}
	drill := ChecklistItem{ID: ItemRestoreDrill}
	var scriptsAck, drillAck *ChecklistAck
	if a, ok := rec.Acks[ItemUserScripts]; ok {
		scriptsAck = &a
	}
	if a, ok := rec.Acks[ItemRestoreDrill]; ok {
		drillAck = &a
	}
	c.Items = append(c.Items, ackedItem(scripts, scriptsAck, nil), ackedItem(drill, drillAck, f.LatestFix))
	return c
}

// Acknowledge makes the acknowledgement of item id by the user by at, or
// refuses it: before the migration has finished, for an item that is not on the
// checklist, and for an item whose state comes from a record. An item that is
// already acknowledged keeps its first acknowledgement: it is returned with
// isNew false and nothing is to be written.
func (c Checklist) Acknowledge(id ChecklistItemID, by string, at time.Time, f ChecklistFacts) (ack ChecklistAck, isNew bool, err error) {
	if !c.Finished {
		return ChecklistAck{}, false, ErrMigrationNotFinished
	}
	it := c.Item(id)
	if it == nil {
		return ChecklistAck{}, false, fmt.Errorf("%w: %q", ErrChecklistItemNotFound, id)
	}
	if id != ItemUserScripts && id != ItemRestoreDrill {
		return ChecklistAck{}, false, fmt.Errorf("%w: %s", ErrChecklistItemHasRecord, id)
	}
	if it.Ack != nil {
		return *it.Ack, false, nil
	}
	ack = ChecklistAck{By: by, At: at}
	if id == ItemRestoreDrill && f.LatestFix != nil {
		ack.JobID = f.LatestFix.ID
	}
	return ack, true, nil
}

func decodeChecklistRecord(data []byte) (ChecklistRecord, error) {
	var rec ChecklistRecord
	if len(data) == 0 {
		return rec, nil
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return ChecklistRecord{}, fmt.Errorf("decoding the migration checklist record: %w", err)
	}
	return rec, nil
}

// Checklist reads the records and derives the checklist.
func (s *Service) Checklist(ctx context.Context) (Checklist, error) {
	s.checklistMu.Lock()
	defer s.checklistMu.Unlock()
	c, _, _, _, err := s.checklistLocked(ctx)
	return c, err
}

func (s *Service) checklistLocked(ctx context.Context) (Checklist, ChecklistFacts, ChecklistRecord, *session, error) {
	facts, err := s.ChecklistRecords.Facts(ctx)
	if err != nil {
		return Checklist{}, facts, ChecklistRecord{}, nil, err
	}
	sess, err := s.load(ctx)
	if err != nil {
		return Checklist{}, facts, ChecklistRecord{}, nil, err
	}
	rec, err := decodeChecklistRecord(sess.checklist)
	if err != nil {
		return Checklist{}, facts, ChecklistRecord{}, nil, err
	}
	return BuildChecklist(facts, rec, sess.Report), facts, rec, sess, nil
}

// AcknowledgeChecklistItem records the acknowledgement of item id by the user
// named by and returns the item as it stands afterwards.
func (s *Service) AcknowledgeChecklistItem(ctx context.Context, id ChecklistItemID, by string) (ChecklistItem, error) {
	s.checklistMu.Lock()
	defer s.checklistMu.Unlock()
	c, facts, rec, sess, err := s.checklistLocked(ctx)
	if err != nil {
		return ChecklistItem{}, err
	}
	ack, isNew, err := c.Acknowledge(id, by, s.now(), facts)
	if err != nil {
		return ChecklistItem{}, err
	}
	if isNew {
		if rec.Acks == nil {
			rec.Acks = map[ChecklistItemID]ChecklistAck{}
		}
		rec.Acks[id] = ack
		if err := s.saveChecklist(ctx, rec); err != nil {
			return ChecklistItem{}, err
		}
	}
	return *BuildChecklist(facts, rec, sess.Report).Item(id), nil
}

// RecordChannelTest records that the test of the channel succeeded now. A test
// that failed is not recorded.
func (s *Service) RecordChannelTest(ctx context.Context, channelID string) error {
	s.checklistMu.Lock()
	defer s.checklistMu.Unlock()
	sess, err := s.load(ctx)
	if err != nil {
		return err
	}
	rec, err := decodeChecklistRecord(sess.checklist)
	if err != nil {
		return err
	}
	if rec.ChannelTests == nil {
		rec.ChannelTests = map[string]time.Time{}
	}
	rec.ChannelTests[channelID] = s.now()
	return s.saveChecklist(ctx, rec)
}

func (s *Service) saveChecklist(ctx context.Context, rec ChecklistRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encoding the migration checklist record: %w", err)
	}
	return s.Sessions.SetChecklist(ctx, data)
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
