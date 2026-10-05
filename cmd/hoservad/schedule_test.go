package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/notify"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

type recordingEngine struct {
	*parity.FakeEngine

	mu          sync.Mutex
	wroteParity bool
	syncCalls   int
}

func newRecordingEngine() *recordingEngine {
	f := parity.NewFakeEngine()
	f.Sleep = func(time.Duration) {}
	return &recordingEngine{FakeEngine: f}
}

func (r *recordingEngine) Sync(ctx context.Context, opts parity.SyncOpts) (<-chan parity.Progress, error) {
	r.mu.Lock()
	r.syncCalls++
	r.mu.Unlock()
	ch, err := r.FakeEngine.Sync(ctx, opts)
	if err == nil && !opts.DryRun {
		r.mu.Lock()
		r.wroteParity = true
		r.mu.Unlock()
	}
	return ch, err
}

func (r *recordingEngine) snapshot() (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.syncCalls, r.wroteParity
}

type scheduleHarness struct {
	db        *sql.DB
	jobs      *job.Store
	schedules *api.ScheduleService
	settings  *api.SettingsStore
	runner    *scheduleRunner
	registry  *job.Registry
}

// newScheduleHarness builds a harness whose nightly chain was configured, and
// whose parity guard was wired, two days before now, and which has never run,
// so the first window at or after now's start time is genuinely owed. Tests
// of a chain configured or wired at now use newUnseededScheduleHarness.
func newScheduleHarness(t *testing.T, now func() time.Time, guard job.DiffGuard) *scheduleHarness {
	t.Helper()
	h := newUnseededScheduleHarness(t, now, guard)
	err := h.schedules.Schedules.UpsertChain(context.Background(), api.ScheduleChainRow{
		StartTime:           "02:00",
		MoverEnabled:        true,
		DiffGuardEnabled:    true,
		SyncEnabled:         true,
		ScrubEnabled:        true,
		ConfigBackupEnabled: true,
		UpdatedAt:           now().Add(-48 * time.Hour).UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("seeding the chain two days before now: %v", err)
	}
	h.runner.Guard = &diffGuardHolder{guard: guard, setAt: now().Add(-48 * time.Hour), now: now}
	return h
}

func newUnseededScheduleHarness(t *testing.T, now func() time.Time, guard job.DiffGuard) *scheduleHarness {
	t.Helper()
	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "hoservad-schedule-test.db")
	db, err := sql.Open("sqlite", store.DSN(dbPath))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}

	jobStore := job.NewStore(db)
	registry := job.NewRegistry()
	scheduler := job.NewScheduler(jobStore, job.NewLogStore(t.TempDir()), job.NewHub(), registry)
	settingsStore := api.NewSettingsStore(db)
	scheduleService := api.NewScheduleService(api.NewScheduleStore(db), settingsStore)
	scheduleService.Now = now
	holder := &diffGuardHolder{now: now}
	if guard != nil {
		holder.set(guard)
	}

	return &scheduleHarness{
		db:        db,
		jobs:      jobStore,
		schedules: scheduleService,
		settings:  settingsStore,
		registry:  registry,
		runner: &scheduleRunner{
			Schedules: scheduleService,
			Scheduler: scheduler,
			Guard:     holder,
		},
	}
}

func (h *scheduleHarness) setTimezone(t *testing.T, loc string) {
	t.Helper()
	ctx := context.Background()
	if err := h.settings.InsertMeta(ctx, "test-install", time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("InsertMeta: %v", err)
	}
	if err := h.settings.Update(ctx, api.GeneralSettingsRow{
		Timezone: sql.NullString{String: loc, Valid: true},
	}); err != nil {
		t.Fatalf("Update timezone: %v", err)
	}
}

func (h *scheduleHarness) jobTypes(t *testing.T) []job.Type {
	t.Helper()
	jobs, err := h.jobs.List(context.Background(), job.ListFilter{Limit: 50})
	if err != nil {
		t.Fatalf("List jobs: %v", err)
	}
	out := make([]job.Type, len(jobs))
	for i, j := range jobs {
		out[i] = j.Type
	}
	return out
}

func containsType(types []job.Type, want job.Type) bool {
	for _, got := range types {
		if got == want {
			return true
		}
	}
	return false
}

func trippedDiff() parity.DiffReport {
	return parity.DiffReport{
		Removed: 600,
		PerDisk: map[string]parity.DiskDiff{
			"/mnt/disk1": {FilesBefore: 1000, FilesAfter: 400},
		},
	}
}

func utcClock(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

// TestScheduleRunner_BlockedGuardFromDaemonClockDoesNotWriteParity is the
// data-loss scenario: a due nightly chain reached from the daemon clock
// with a tripped threshold guard must stop after diff and never call
// Sync, so parity is not written over the deletions.
func TestScheduleRunner_BlockedGuardFromDaemonClockDoesNotWriteParity(t *testing.T) {
	now := time.Date(2026, 6, 15, 2, 1, 0, 0, time.UTC)
	eng := newRecordingEngine()
	eng.SetDiff(trippedDiff())
	eng.ScriptSync([]parity.Progress{{Phase: "syncing", Percent: 100}}, nil)

	h := newScheduleHarness(t, utcClock(now), job.EngineDiffGuard{Engine: eng, Guard: parity.Guard{}})
	h.registry.Register(job.TypeSync, false, job.RunSync(eng))

	if err := h.runner.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	syncCalls, wrote := eng.snapshot()
	if wrote {
		t.Fatal("sync proceeded past a tripped threshold guard from the daemon clock — parity would have been written over the deletions")
	}
	if syncCalls != 0 {
		t.Fatalf("engine.Sync calls = %d, want 0 — the chain must stop before Submit(TypeSync)", syncCalls)
	}
	if containsType(h.jobTypes(t), job.TypeSync) {
		t.Fatal("a TypeSync job was submitted past a blocked guard")
	}
}

func TestScheduleRunner_DueChainSubmitsSync(t *testing.T) {
	now := time.Date(2026, 6, 15, 2, 1, 0, 0, time.UTC)
	eng := newRecordingEngine()
	eng.SetDiff(parity.DiffReport{Removed: 1, PerDisk: map[string]parity.DiskDiff{
		"/mnt/disk1": {FilesBefore: 1000, FilesAfter: 999},
	}})
	eng.ScriptSync([]parity.Progress{{Phase: "syncing", Percent: 100}}, nil)

	h := newScheduleHarness(t, utcClock(now), job.EngineDiffGuard{Engine: eng, Guard: parity.Guard{}})
	h.registry.Register(job.TypeSync, false, job.RunSync(eng))

	if err := h.runner.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if !containsType(h.jobTypes(t), job.TypeSync) {
		t.Fatal("due chain did not submit TypeSync")
	}
	_, wrote := eng.snapshot()
	if !wrote {
		t.Fatal("due unblocked chain did not write parity")
	}
}

func TestScheduleRunner_UnregisteredMoverIsSkipped(t *testing.T) {
	now := time.Date(2026, 6, 15, 2, 1, 0, 0, time.UTC)
	eng := newRecordingEngine()
	eng.SetDiff(parity.DiffReport{Removed: 1, PerDisk: map[string]parity.DiskDiff{
		"/mnt/disk1": {FilesBefore: 1000, FilesAfter: 999},
	}})
	eng.ScriptSync([]parity.Progress{{Phase: "syncing", Percent: 100}}, nil)

	h := newScheduleHarness(t, utcClock(now), job.EngineDiffGuard{Engine: eng, Guard: parity.Guard{}})
	h.registry.Register(job.TypeSync, false, job.RunSync(eng))

	if err := h.runner.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v — unregistered mover must not fail the chain", err)
	}
	types := h.jobTypes(t)
	if containsType(types, job.TypeMover) {
		t.Fatal("unregistered mover was submitted")
	}
	if !containsType(types, job.TypeSync) {
		t.Fatal("chain did not continue to sync after skipping unregistered mover")
	}
}

func TestScheduleRunner_DisabledMoverIsSkipped(t *testing.T) {
	// Saved at 01:00, before the 02:00 window: a save restarts a never-run
	// chain's wait, so the window must still lie ahead of it.
	clock := time.Date(2026, 6, 15, 1, 0, 0, 0, time.UTC)
	eng := newRecordingEngine()
	eng.SetDiff(parity.DiffReport{Removed: 1, PerDisk: map[string]parity.DiskDiff{
		"/mnt/disk1": {FilesBefore: 1000, FilesAfter: 999},
	}})
	eng.ScriptSync([]parity.Progress{{Phase: "syncing", Percent: 100}}, nil)

	h := newScheduleHarness(t, func() time.Time { return clock }, job.EngineDiffGuard{Engine: eng, Guard: parity.Guard{}})
	h.registry.Register(job.TypeSync, false, job.RunSync(eng))
	moverCalls := 0
	h.registry.Register(job.TypeMover, false, func(context.Context, *job.RunContext) error {
		moverCalls++
		return nil
	})
	disabled := false
	if _, err := h.schedules.UpdateChain(context.Background(), api.UpdateChainInput{
		StepEnabled: map[job.Step]bool{job.StepMover: disabled},
	}); err != nil {
		t.Fatalf("UpdateChain: %v", err)
	}
	clock = time.Date(2026, 6, 15, 2, 1, 0, 0, time.UTC)

	if err := h.runner.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if moverCalls != 0 {
		t.Fatalf("disabled mover ran %d times, want 0", moverCalls)
	}
	if !containsType(h.jobTypes(t), job.TypeSync) {
		t.Fatal("chain did not continue to sync after skipping disabled mover")
	}
}

func TestScheduleRunner_NotYetDueChainIsNotSubmitted(t *testing.T) {
	now := time.Date(2026, 6, 15, 1, 0, 0, 0, time.UTC)
	eng := newRecordingEngine()
	eng.ScriptSync([]parity.Progress{{Phase: "syncing", Percent: 100}}, nil)

	h := newScheduleHarness(t, utcClock(now), job.EngineDiffGuard{Engine: eng, Guard: parity.Guard{}})
	h.registry.Register(job.TypeSync, false, job.RunSync(eng))

	if err := h.runner.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if types := h.jobTypes(t); len(types) != 0 {
		t.Fatalf("not-yet-due chain submitted jobs %v", types)
	}
	syncCalls, wrote := eng.snapshot()
	if syncCalls != 0 || wrote {
		t.Fatalf("not-yet-due chain called Sync (calls=%d wrote=%v)", syncCalls, wrote)
	}
}

// The migration-suite scenario: the daemon sets up after the chain's start
// time and the first tick must not run the mover, a sync or a config backup.
// The same chain is due at its next start time, once, and a restart after
// that window is claimed does not run it again.
func TestScheduleRunner_NeverRunChainSetUpAfterStartWaitsForNextStartTime(t *testing.T) {
	clock := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	eng := newRecordingEngine()
	eng.SetDiff(parity.DiffReport{Removed: 1, PerDisk: map[string]parity.DiskDiff{
		"/mnt/disk1": {FilesBefore: 1000, FilesAfter: 999},
	}})
	eng.ScriptSync([]parity.Progress{{Phase: "syncing", Percent: 100}}, nil)

	h := newUnseededScheduleHarness(t, func() time.Time { return clock }, job.EngineDiffGuard{Engine: eng, Guard: parity.Guard{}})
	h.registry.Register(job.TypeSync, false, job.RunSync(eng))

	if err := h.runner.tick(context.Background()); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	clock = clock.Add(20 * time.Second)
	if err := h.runner.tick(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if types := h.jobTypes(t); len(types) != 0 {
		t.Fatalf("a chain set up at 10:00 with a 02:00 start submitted %v on its first ticks", types)
	}
	if syncCalls, wrote := eng.snapshot(); syncCalls != 0 || wrote {
		t.Fatalf("first ticks called Sync (calls=%d wrote=%v)", syncCalls, wrote)
	}

	clock = time.Date(2026, 6, 16, 2, 0, 30, 0, time.UTC)
	if err := h.runner.tick(context.Background()); err != nil {
		t.Fatalf("tomorrow's tick: %v", err)
	}
	if !containsType(h.jobTypes(t), job.TypeSync) {
		t.Fatal("the chain did not run at its next start time, 02:00 tomorrow")
	}
	if err := h.runner.tick(context.Background()); err != nil {
		t.Fatalf("tick inside the claimed window: %v", err)
	}
	syncs := 0
	for _, typ := range h.jobTypes(t) {
		if typ == job.TypeSync {
			syncs++
		}
	}
	if syncs != 1 {
		t.Fatalf("TypeSync jobs after the first window = %d, want 1", syncs)
	}
}

// A never-run chain does not catch up a window that passed before this
// daemon process could run it: after a restart past today's start time its
// first window is tomorrow's start, because the guard is wired anew then.
func TestScheduleRunner_RestartAfterStartDoesNotRunNeverRunChain(t *testing.T) {
	clock := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	eng := newRecordingEngine()
	eng.ScriptSync([]parity.Progress{{Phase: "syncing", Percent: 100}}, nil)
	guard := job.EngineDiffGuard{Engine: eng, Guard: parity.Guard{}}
	now := func() time.Time { return clock }
	h := newScheduleHarness(t, now, guard)
	h.registry.Register(job.TypeSync, false, job.RunSync(eng))

	restarted := &diffGuardHolder{now: now}
	restarted.set(guard)
	h.runner.Guard = restarted
	if err := h.runner.tick(context.Background()); err != nil {
		t.Fatalf("tick after restart: %v", err)
	}
	clock = clock.Add(time.Minute)
	if err := h.runner.tick(context.Background()); err != nil {
		t.Fatalf("second tick after restart: %v", err)
	}
	if types := h.jobTypes(t); len(types) != 0 {
		t.Fatalf("a restart after the start time ran the never-run chain: %v", types)
	}

	clock = time.Date(2026, 6, 16, 2, 0, 30, 0, time.UTC)
	if err := h.runner.tick(context.Background()); err != nil {
		t.Fatalf("tomorrow's tick: %v", err)
	}
	if !containsType(h.jobTypes(t), job.TypeSync) {
		t.Fatal("the chain did not run at the next start time after the restart")
	}
}

// The daemon is up from day 1 with no array, so the chain guard stays unset
// and no window can be claimed. When parity becomes runnable on day 3 at
// 10:00 (the migration's point of no return), the windows that passed in
// between are not owed: the first run is the next start time, day 4 02:00,
// which is also what nextRun reports then.
func TestScheduleRunner_GuardWiredOnALaterDayWaitsForTheNextStartTime(t *testing.T) {
	clock := time.Date(2026, 6, 13, 15, 0, 0, 0, time.UTC)
	eng := newRecordingEngine()
	eng.ScriptSync([]parity.Progress{{Phase: "syncing", Percent: 100}}, nil)
	h := newUnseededScheduleHarness(t, func() time.Time { return clock }, nil)
	h.registry.Register(job.TypeSync, false, job.RunSync(eng))
	h.runner.OtherJobs = map[string]func(context.Context) error{
		"container_update_check": func(context.Context) error { return nil },
	}
	tick := func(at time.Time) {
		t.Helper()
		clock = at
		if err := h.runner.tick(context.Background()); err != nil {
			t.Fatalf("tick at %v: %v", at, err)
		}
	}

	tick(clock)
	tick(time.Date(2026, 6, 14, 2, 0, 30, 0, time.UTC))
	tick(time.Date(2026, 6, 15, 2, 0, 30, 0, time.UTC))
	if types := h.jobTypes(t); len(types) != 0 {
		t.Fatalf("jobs before parity was wired: %v", types)
	}

	ponr := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	clock = ponr
	h.runner.Guard.set(job.EngineDiffGuard{Engine: eng, Guard: parity.Guard{}})
	firstRun := time.Date(2026, 6, 16, 2, 0, 0, 0, time.UTC)
	if next := job.NextChainRun(ponr, time.UTC, "02:00"); !next.Equal(firstRun) {
		t.Fatalf("nextRun at the point of no return = %v, want %v", next, firstRun)
	}
	tick(ponr.Add(20 * time.Second))
	tick(ponr.Add(time.Minute))
	tick(firstRun.Add(-time.Minute))
	if types := h.jobTypes(t); len(types) != 0 {
		t.Fatalf("jobs after parity was wired but before nextRun %v: %v", firstRun, types)
	}
	if syncCalls, wrote := eng.snapshot(); syncCalls != 0 || wrote {
		t.Fatalf("Sync called before nextRun (calls=%d wrote=%v)", syncCalls, wrote)
	}

	tick(firstRun.Add(30 * time.Second))
	if !containsType(h.jobTypes(t), job.TypeSync) {
		t.Fatalf("the chain did not run at nextRun %v", firstRun)
	}
}

// A fresh install: the daemon starts on day 1 before the chain's start time
// and onboarding creates the array on day 2 after it, in the installation's
// timezone. Onboarding never saves the chain settings, so only the moment
// parity was wired can hold the first run back until day 3's start.
func TestScheduleRunner_ArrayCreatedOnALaterDayAfterTheStartTimeWaits(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	clock := time.Date(2026, 6, 15, 1, 0, 0, 0, loc)
	eng := newRecordingEngine()
	eng.ScriptSync([]parity.Progress{{Phase: "syncing", Percent: 100}}, nil)
	h := newUnseededScheduleHarness(t, func() time.Time { return clock }, nil)
	h.setTimezone(t, "Europe/Berlin")
	h.registry.Register(job.TypeSync, false, job.RunSync(eng))
	h.runner.OtherJobs = map[string]func(context.Context) error{
		"container_update_check": func(context.Context) error { return nil },
	}
	tick := func(at time.Time) {
		t.Helper()
		clock = at
		if err := h.runner.tick(context.Background()); err != nil {
			t.Fatalf("tick at %v: %v", at, err)
		}
	}

	tick(clock)
	onboarded := time.Date(2026, 6, 16, 3, 0, 0, 0, loc)
	clock = onboarded
	h.runner.Guard.set(job.EngineDiffGuard{Engine: eng, Guard: parity.Guard{}})
	tick(onboarded.Add(20 * time.Second))
	tick(onboarded.Add(time.Minute))
	if types := h.jobTypes(t); len(types) != 0 {
		t.Fatalf("the chain ran right after the array was created: %v", types)
	}

	tick(time.Date(2026, 6, 17, 2, 0, 30, 0, loc))
	if !containsType(h.jobTypes(t), job.TypeSync) {
		t.Fatal("the chain did not run at the next start time after the array was created")
	}
}

func TestScheduleRunner_TimezoneIsSettingsTimezone(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	// 00:30 UTC is 02:30 CEST — due in Berlin, not yet due if the runner
	// wrongly used UTC against a 02:00 start.
	now := time.Date(2026, 6, 15, 0, 30, 0, 0, time.UTC)
	eng := newRecordingEngine()
	eng.SetDiff(parity.DiffReport{Removed: 1, PerDisk: map[string]parity.DiskDiff{
		"/mnt/disk1": {FilesBefore: 1000, FilesAfter: 999},
	}})
	eng.ScriptSync([]parity.Progress{{Phase: "syncing", Percent: 100}}, nil)

	h := newScheduleHarness(t, utcClock(now), job.EngineDiffGuard{Engine: eng, Guard: parity.Guard{}})
	h.setTimezone(t, loc.String())
	h.registry.Register(job.TypeSync, false, job.RunSync(eng))

	if err := h.runner.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if !containsType(h.jobTypes(t), job.TypeSync) {
		t.Fatal("chain was not due in Europe/Berlin at 02:30 local — runner ignored the settings timezone")
	}

	utcHarness := newScheduleHarness(t, utcClock(now), job.EngineDiffGuard{Engine: eng, Guard: parity.Guard{}})
	utcHarness.registry.Register(job.TypeSync, false, job.RunSync(eng))
	if err := utcHarness.runner.tick(context.Background()); err != nil {
		t.Fatalf("utc tick: %v", err)
	}
	if containsType(utcHarness.jobTypes(t), job.TypeSync) {
		t.Fatal("00:30 UTC submitted a 02:00 UTC chain; timezone comparison is using UTC when settings are unset, which is correct — this assertion would fire if UTC default also treated 00:30 as due")
	}
}

func TestScheduleRunner_RestartInsideWindowDoesNotStartSecondChain(t *testing.T) {
	now := time.Date(2026, 6, 15, 2, 30, 0, 0, time.UTC)
	eng := newRecordingEngine()
	eng.SetDiff(parity.DiffReport{Removed: 1, PerDisk: map[string]parity.DiskDiff{
		"/mnt/disk1": {FilesBefore: 1000, FilesAfter: 999},
	}})
	eng.ScriptSync([]parity.Progress{{Phase: "syncing", Percent: 100}}, nil)

	h := newScheduleHarness(t, utcClock(now), job.EngineDiffGuard{Engine: eng, Guard: parity.Guard{}})
	h.registry.Register(job.TypeSync, false, job.RunSync(eng))

	if err := h.runner.tick(context.Background()); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	first := h.jobTypes(t)
	if !containsType(first, job.TypeSync) {
		t.Fatal("first tick did not submit TypeSync")
	}

	restart := job.NewScheduler(h.jobs, job.NewLogStore(t.TempDir()), job.NewHub(), h.registry)
	h.runner.Scheduler = restart
	if err := h.runner.tick(context.Background()); err != nil {
		t.Fatalf("restart tick: %v", err)
	}
	second := h.jobTypes(t)
	syncs := 0
	for _, typ := range second {
		if typ == job.TypeSync {
			syncs++
		}
	}
	if syncs != 1 {
		t.Fatalf("TypeSync jobs after restart-in-window = %d, want 1 — last-run must consume the window", syncs)
	}
}

func TestScheduleRunner_WeeklyScrubHonoursSettingsDay(t *testing.T) {
	// 2026-06-14 is a Sunday.
	sunday := time.Date(2026, 6, 14, 2, 1, 0, 0, time.UTC)
	eng := newRecordingEngine()
	eng.SetDiff(parity.DiffReport{Removed: 1, PerDisk: map[string]parity.DiskDiff{
		"/mnt/disk1": {FilesBefore: 1000, FilesAfter: 999},
	}})
	eng.ScriptSync([]parity.Progress{{Phase: "syncing", Percent: 100}}, nil)
	eng.ScriptScrub([]parity.Progress{{Phase: "scrubbing", Percent: 100}}, nil)

	h := newScheduleHarness(t, utcClock(sunday), job.EngineDiffGuard{Engine: eng, Guard: parity.Guard{}})
	h.registry.Register(job.TypeSync, false, job.RunSync(eng))
	h.registry.Register(job.TypeScrub, false, job.RunScrub(eng))

	if err := h.runner.tick(context.Background()); err != nil {
		t.Fatalf("sunday tick: %v", err)
	}
	if !containsType(h.jobTypes(t), job.TypeScrub) {
		t.Fatal("weekly scrub day (Sunday) did not submit TypeScrub")
	}

	monday := time.Date(2026, 6, 15, 2, 1, 0, 0, time.UTC)
	h2 := newScheduleHarness(t, utcClock(monday), job.EngineDiffGuard{Engine: eng, Guard: parity.Guard{}})
	h2.registry.Register(job.TypeSync, false, job.RunSync(eng))
	h2.registry.Register(job.TypeScrub, false, job.RunScrub(eng))
	if err := h2.runner.tick(context.Background()); err != nil {
		t.Fatalf("monday tick: %v", err)
	}
	if containsType(h2.jobTypes(t), job.TypeScrub) {
		t.Fatal("TypeScrub submitted on Monday; weekly scrub day is Sunday")
	}
}

func TestScheduleRunner_SettingsUpdatePreservesLastRun(t *testing.T) {
	now := time.Date(2026, 6, 15, 2, 1, 0, 0, time.UTC)
	eng := newRecordingEngine()
	eng.SetDiff(parity.DiffReport{Removed: 1, PerDisk: map[string]parity.DiskDiff{
		"/mnt/disk1": {FilesBefore: 1000, FilesAfter: 999},
	}})
	eng.ScriptSync([]parity.Progress{{Phase: "syncing", Percent: 100}}, nil)

	h := newScheduleHarness(t, utcClock(now), job.EngineDiffGuard{Engine: eng, Guard: parity.Guard{}})
	h.registry.Register(job.TypeSync, false, job.RunSync(eng))
	if err := h.runner.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	start := "01:00"
	if _, err := h.schedules.UpdateChain(context.Background(), api.UpdateChainInput{StartTime: &start}); err != nil {
		t.Fatalf("UpdateChain: %v", err)
	}
	if err := h.runner.tick(context.Background()); err != nil {
		t.Fatalf("tick after settings update: %v", err)
	}
	syncs := 0
	for _, typ := range h.jobTypes(t) {
		if typ == job.TypeSync {
			syncs++
		}
	}
	if syncs != 1 {
		t.Fatalf("TypeSync jobs after settings update = %d, want 1 — last-run must survive UpsertChain", syncs)
	}
}

func TestRunScheduleLoopStopsOnCancel(t *testing.T) {
	now := time.Date(2026, 6, 15, 1, 0, 0, 0, time.UTC)
	h := newScheduleHarness(t, utcClock(now), &blockingGuard{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runScheduleLoop(ctx, h.runner, 10*time.Millisecond)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runScheduleLoop did not stop after ctx cancellation")
	}
}

type blockingGuard struct{}

func (blockingGuard) Evaluate(context.Context) (bool, error) { return false, nil }

type recordingBackup struct {
	mu   sync.Mutex
	runs int
}

func (b *recordingBackup) Run(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.runs++
	return nil
}

func (b *recordingBackup) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.runs
}

func TestScheduleRunner_DueChainRunsConfigBackup(t *testing.T) {
	now := time.Date(2026, 6, 15, 2, 1, 0, 0, time.UTC)
	eng := newRecordingEngine()
	eng.SetDiff(parity.DiffReport{Removed: 1, PerDisk: map[string]parity.DiskDiff{
		"/mnt/disk1": {FilesBefore: 1000, FilesAfter: 999},
	}})
	eng.ScriptSync([]parity.Progress{{Phase: "syncing", Percent: 100}}, nil)

	backup := &recordingBackup{}
	h := newScheduleHarness(t, utcClock(now), job.EngineDiffGuard{Engine: eng, Guard: parity.Guard{}})
	h.runner.Backup = backup
	h.registry.Register(job.TypeSync, false, job.RunSync(eng))

	if err := h.runner.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if backup.count() != 1 {
		t.Fatalf("ConfigBackup.Run calls = %d, want 1", backup.count())
	}
}

func TestScheduleRunner_NilBackupSkipsConfigBackup(t *testing.T) {
	now := time.Date(2026, 6, 15, 2, 1, 0, 0, time.UTC)
	eng := newRecordingEngine()
	eng.SetDiff(parity.DiffReport{Removed: 1, PerDisk: map[string]parity.DiskDiff{
		"/mnt/disk1": {FilesBefore: 1000, FilesAfter: 999},
	}})
	eng.ScriptSync([]parity.Progress{{Phase: "syncing", Percent: 100}}, nil)

	h := newScheduleHarness(t, utcClock(now), job.EngineDiffGuard{Engine: eng, Guard: parity.Guard{}})
	h.registry.Register(job.TypeSync, false, job.RunSync(eng))

	if err := h.runner.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v — nil backup must not fail the chain", err)
	}
	if h.runner.Backup != nil {
		t.Fatal("harness should leave Backup nil")
	}
}

// nightlyBackupService is a real backup.Service over one destination that
// cannot be written (its parent is a regular file) and, when withGood is
// set, one that can.
func nightlyBackupService(t *testing.T, h *scheduleHarness, withGood bool) *backup.Service {
	t.Helper()
	root := t.TempDir()
	blocker := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	dests := &backup.FakeDestinationStore{}
	retention := backup.Retention{Daily: 7}
	if err := dests.CreateDestination(context.Background(), backup.Destination{
		ID: "bad", Name: "Bad", Type: backup.TypeLocal, Path: filepath.Join(blocker, "backups"), Enabled: true, Retention: retention,
	}); err != nil {
		t.Fatal(err)
	}
	if withGood {
		if err := dests.CreateDestination(context.Background(), backup.Destination{
			ID: "good", Name: "Good", Type: backup.TypeLocal, Path: filepath.Join(root, "good"), Enabled: true, Retention: retention,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return &backup.Service{DB: h.db, Paths: backup.Paths{DBPath: filepath.Join(root, "hoservad.db")}, Store: dests, Hostname: "test-host"}
}

func nightlyChainHarness(t *testing.T, pub *recordingPublisher) *scheduleHarness {
	t.Helper()
	now := time.Date(2026, 6, 15, 2, 1, 0, 0, time.UTC)
	eng := newRecordingEngine()
	eng.SetDiff(parity.DiffReport{Removed: 1, PerDisk: map[string]parity.DiskDiff{
		"/mnt/disk1": {FilesBefore: 1000, FilesAfter: 999},
	}})
	eng.ScriptSync([]parity.Progress{{Phase: "syncing", Percent: 100}}, nil)
	h := newScheduleHarness(t, utcClock(now), job.EngineDiffGuard{Engine: eng, Guard: parity.Guard{}})
	h.registry.Register(job.TypeSync, false, job.RunSync(eng))
	h.runner.Notifier = &scheduleNotifier{svc: pub}
	return h
}

// #477: the chain hoservad schedules publishes config_backup_failed when
// its config backup wrote no destination, and the tick still returns the
// failure.
func TestScheduleRunner_NightlyConfigBackupThatWroteNowhereAlerts(t *testing.T) {
	pub := &recordingPublisher{}
	h := nightlyChainHarness(t, pub)
	h.runner.Backup = nightlyBackupService(t, h, false)

	if err := h.runner.tick(context.Background()); err == nil {
		t.Fatal("tick succeeded, want the failed config backup's error")
	}
	if n := pub.count(notify.EventConfigBackupFailed); n != 1 {
		t.Fatalf("config_backup_failed published %d times, want once", n)
	}
	if len(pub.messages) != 1 || !strings.Contains(pub.messages[0], "The config backup did not complete") {
		t.Fatalf("message = %q, want the on-demand job's payload shape", pub.messages)
	}
}

func TestScheduleRunner_NightlyConfigBackupWithOneDestinationWrittenDoesNotAlert(t *testing.T) {
	pub := &recordingPublisher{}
	h := nightlyChainHarness(t, pub)
	h.runner.Backup = nightlyBackupService(t, h, true)

	if err := h.runner.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v — a partial success is not a failure", err)
	}
	if n := pub.count(notify.EventConfigBackupFailed); n != 0 {
		t.Fatalf("config_backup_failed published %d times for a partial success, want none", n)
	}
}
