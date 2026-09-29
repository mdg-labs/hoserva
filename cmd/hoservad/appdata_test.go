package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/notify"
	"github.com/mdg-labs/hoserva/internal/store"
)

type recordingPublisher struct {
	mu       sync.Mutex
	events   []notify.EventType
	messages []string
}

func (p *recordingPublisher) Publish(_ context.Context, event notify.EventType, _, message string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, event)
	p.messages = append(p.messages, message)
	return nil
}

func (p *recordingPublisher) count(event notify.EventType) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, e := range p.events {
		if e == event {
			n++
		}
	}
	return n
}

// appdataBackupService is the appdata service built the way main.go builds
// it (newAppdataService over the container services and the array's cache
// disk), with one backup destination on a directory of the test's own.
func appdataBackupService(t *testing.T, apps *appServices, arrays *store.ArrayStore, db *sql.DB, stateDir, destDir string) *backup.AppdataService {
	t.Helper()
	dests := &backup.FakeDestinationStore{}
	if err := dests.CreateDestination(context.Background(), backup.Destination{
		ID: backup.DefaultPoolID, Name: "Pool", Type: backup.TypeLocal, Path: destDir, Enabled: true,
		Retention: backup.Retention{Daily: 7, Weekly: 4, Monthly: 6},
	}); err != nil {
		t.Fatal(err)
	}
	svc := &backup.Service{Store: dests, Hostname: "test-host"}
	return newAppdataService(apps, svc, api.NewAppdataPolicyStore(db), arrays, stateDir)
}

func (w *containersWiringHarness) doBody(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, "http://unix"+apiPathPrefix+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.Bytes()
}

func (w *containersWiringHarness) awaitJobByID(t *testing.T, id string) *job.Job {
	t.Helper()
	return awaitTerminal(t, w.handler.Store, id)
}

func TestAppdataWiring_OperationsAnswer501WhenAppdataIsNotWired(t *testing.T) {
	w := newContainersWiringHarness(t)
	if status, body := w.do(t, http.MethodGet, "/appdata/backup"); status != http.StatusNotImplemented {
		t.Fatalf("GET /appdata/backup without wiring = %d %s, want 501", status, body)
	}
}

// POST /appdata/backup reaches the appdata service through the real daemon
// server and its job registry, and archives a container the way the
// schedule would.
func TestAppdataWiring_BackupIsReachableOverHTTP(t *testing.T) {
	w := newContainersWiringHarness(t)
	dest := filepath.Join(w.root, "pool-backups")
	svc := appdataBackupService(t, w.apps, w.arrays, w.db, w.root, dest)
	wireAppdata(w.handler, w.registry, svc, &recordingPublisher{})

	status, body := w.do(t, http.MethodGet, "/appdata/backup")
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"jellyfin"`)) {
		t.Fatalf("GET /appdata/backup = %d %s, want the jellyfin container in scope", status, body)
	}
	status, body = w.doBody(t, http.MethodPut, "/appdata/backup/containers/jellyfin", `{"stop":false,"included":true}`)
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"stop":false`)) {
		t.Fatalf("PUT policy = %d %s", status, body)
	}

	status, body = w.doBody(t, http.MethodPost, "/appdata/backup", ``)
	if status != http.StatusOK {
		t.Fatalf("POST /appdata/backup = %d %s, want 200 with the queued job", status, body)
	}
	var queued struct {
		ID    string `json:"id"`
		Type  string `json:"type"`
		Class string `json:"class"`
	}
	if err := json.Unmarshal(body, &queued); err != nil || queued.Type != "appdata_backup" || queued.Class != "service" {
		t.Fatalf("job = %s (%v), want an appdata_backup service job", body, err)
	}
	done := w.awaitJobByID(t, queued.ID)
	if done.Status != job.StatusSucceeded {
		t.Fatalf("job = %s %s", done.Status, done.ErrorMessage)
	}
	entries, err := os.ReadDir(dest)
	if err != nil || len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "hoserva-appdata-") {
		t.Fatalf("destination = %v, %v; want one appdata archive", entries, err)
	}

	status, body = w.do(t, http.MethodGet, "/appdata/backup/archives?container=jellyfin")
	if status != http.StatusOK || !bytes.Contains(body, []byte(entries[0].Name())) {
		t.Fatalf("GET archives = %d %s", status, body)
	}
}

func TestAppdataWiring_RestoreIsReachableOverHTTP(t *testing.T) {
	w := newContainersWiringHarness(t)
	dest := filepath.Join(w.root, "pool-backups")
	svc := appdataBackupService(t, w.apps, w.arrays, w.db, w.root, dest)
	wireAppdata(w.handler, w.registry, svc, &recordingPublisher{})

	status, body := w.doBody(t, http.MethodPost, "/appdata/backup", `{"containers":["jellyfin"]}`)
	if status != http.StatusOK {
		t.Fatalf("POST /appdata/backup = %d %s", status, body)
	}
	var queued struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &queued)
	if done := w.awaitJobByID(t, queued.ID); done.Status != job.StatusSucceeded {
		t.Fatalf("backup: %s %s", done.Status, done.ErrorMessage)
	}
	entries, _ := os.ReadDir(dest)
	if len(entries) != 1 {
		t.Fatalf("archives = %v", entries)
	}
	live := filepath.Join(w.appdata, "library.db")
	if err := os.WriteFile(live, []byte("corrupted"), 0o644); err != nil {
		t.Fatal(err)
	}
	req := `{"container":"jellyfin","archive":"` + entries[0].Name() + `","destinationId":"pool","confirm":`
	if status, body = w.doBody(t, http.MethodPost, "/appdata/backup/restore", req+`false}`); status != http.StatusBadRequest {
		t.Fatalf("restore without confirm = %d %s, want 400", status, body)
	}
	status, body = w.doBody(t, http.MethodPost, "/appdata/backup/restore", req+`true}`)
	if status != http.StatusOK {
		t.Fatalf("POST restore = %d %s", status, body)
	}
	_ = json.Unmarshal(body, &queued)
	if done := w.awaitJobByID(t, queued.ID); done.Status != job.StatusSucceeded {
		t.Fatalf("restore: %s %s", done.Status, done.ErrorMessage)
	}
	if got, _ := os.ReadFile(live); string(got) != "precious" {
		t.Fatalf("library.db after the restore = %q, want the backed-up content", got)
	}
}

func TestAppdataWiring_AFailedBackupPublishesTheFailureNotification(t *testing.T) {
	w := newContainersWiringHarness(t)
	// A destination that is a file cannot take an archive.
	blocked := filepath.Join(w.root, "not-a-directory")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := appdataBackupService(t, w.apps, w.arrays, w.db, w.root, blocked)
	pub := &recordingPublisher{}
	wireAppdata(w.handler, w.registry, svc, pub)

	status, body := w.doBody(t, http.MethodPost, "/appdata/backup", ``)
	if status != http.StatusOK {
		t.Fatalf("POST /appdata/backup = %d %s", status, body)
	}
	var queued struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &queued)
	if done := w.awaitJobByID(t, queued.ID); done.Status != job.StatusFailed {
		t.Fatalf("job = %s, want failed", done.Status)
	}
	if n := pub.count(notify.EventAppdataBackupFailed); n != 1 {
		t.Fatalf("appdata_backup_failed published %d times, want once", n)
	}
}

type scheduledAppdata struct {
	h         *scheduleHarness
	fake      *container.FakeProvider
	svc       *backup.AppdataService
	pub       *recordingPublisher
	dest      string
	halted    *atomic.Bool
	clock     *time.Time
	stateDir  string
	appdataAt string
}

func newScheduledAppdata(t *testing.T) *scheduledAppdata {
	t.Helper()
	// 2026-09-29 is a Tuesday; the weekly window opens Sunday 2026-10-04 04:00.
	clock := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	h := newScheduleHarness(t, func() time.Time { return clock }, nil)
	root := t.TempDir()
	arrays := store.NewArrayStore(h.db)
	cache := filepath.Join(root, "cache")
	if err := arrays.PutArray(context.Background(), store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "50G", CreatedAt: clock}, []store.ArrayDisk{
		{Role: store.ArrayRoleCache, RoleIndex: 1, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "uuid-c", WWN: "wwn-c", Serial: "CACHE1", ByIDName: "wwn-wwn-c", Mountpoint: cache},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	dir := filepath.Join(cache, "appdata", "sonarr")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sonarr.db"), []byte("series"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := container.NewFakeProvider()
	fake.AddContainer(container.Container{ID: "s", Name: "sonarr", Image: "sonarr", State: "running",
		Mounts: []container.Mount{{Source: dir, Destination: "/config"}}})

	apps := newContainers(fake, root, arrays, nil)
	halted := &atomic.Bool{}
	apps.Lifecycle.Halted = halted.Load
	apps.Lifecycle.StorageReady = func() bool { return true }
	dest := filepath.Join(root, "pool-backups")
	svc := appdataBackupService(t, apps, arrays, h.db, root, dest)
	pub := &recordingPublisher{}
	wireAppdata(&api.Handler{}, h.registry, svc, pub)
	wireAppdataSchedule(h.runner, svc, h.runner.Scheduler, pub)

	t.Cleanup(func() {
		dctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = h.runner.Scheduler.Drain(dctx)
	})
	return &scheduledAppdata{h: h, fake: fake, svc: svc, pub: pub, dest: dest, halted: halted, clock: &clock, stateDir: root, appdataAt: dir}
}

func (s *scheduledAppdata) appdataJobs(t *testing.T) []*job.Job {
	t.Helper()
	jobs, err := s.h.jobs.List(context.Background(), job.ListFilter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var out []*job.Job
	for _, j := range jobs {
		if j.Type == job.TypeAppdataBackup {
			out = append(out, j)
		}
	}
	return out
}

func awaitTerminal(t *testing.T, s *job.Store, id string) *job.Job {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if j, err := s.Get(context.Background(), id); err == nil && j.Status.Terminal() {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s did not finish", id)
	return nil
}

func TestScheduleTick_RunsTheWeeklyAppdataBackupOncePerWindow(t *testing.T) {
	ctx := context.Background()
	s := newScheduledAppdata(t)

	if err := s.h.runner.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if jobs := s.appdataJobs(t); len(jobs) != 0 {
		t.Fatalf("a backup started on the Tuesday the schedule was seeded: %v", jobs)
	}

	*s.clock = time.Date(2026, 10, 4, 4, 1, 0, 0, time.UTC)
	if err := s.h.runner.tick(ctx); err != nil {
		t.Fatalf("tick in the window: %v", err)
	}
	jobs := s.appdataJobs(t)
	if len(jobs) != 1 {
		t.Fatalf("appdata jobs after the window opened = %v, want one", jobs)
	}
	done := awaitTerminal(t, s.h.jobs, jobs[0].ID)
	if done.Status != job.StatusSucceeded {
		t.Fatalf("scheduled backup = %s %s", done.Status, done.ErrorMessage)
	}
	entries, _ := os.ReadDir(s.dest)
	if len(entries) != 1 {
		t.Fatalf("archives after the scheduled backup = %v, want one", entries)
	}

	for i := 0; i < 2; i++ {
		if err := s.h.runner.tick(ctx); err != nil {
			t.Fatalf("second tick: %v", err)
		}
	}
	if got := len(s.appdataJobs(t)); got != 1 {
		t.Fatalf("appdata jobs after more ticks in the same window = %d, want still one", got)
	}

	*s.clock = time.Date(2026, 10, 11, 4, 0, 0, 0, time.UTC)
	if err := s.h.runner.tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(s.appdataJobs(t)); got != 2 {
		t.Fatalf("appdata jobs after the next Sunday = %d, want two", got)
	}
}

func TestScheduleTick_ABackupThatCannotStartAlertsAndIsNotRetriedEveryMinute(t *testing.T) {
	ctx := context.Background()
	s := newScheduledAppdata(t)
	if err := s.h.runner.tick(ctx); err != nil {
		t.Fatalf("seeding tick: %v", err)
	}
	*s.clock = time.Date(2026, 10, 4, 4, 1, 0, 0, time.UTC)
	s.halted.Store(true)

	if err := s.h.runner.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if n := s.pub.count(notify.EventAppdataBackupFailed); n != 1 {
		t.Fatalf("appdata_backup_failed published %d times, want once for the backup that did not start", n)
	}
	if got := len(s.appdataJobs(t)); got != 0 {
		t.Fatalf("a job was queued with the array stopped: %d", got)
	}
	if err := s.h.runner.tick(ctx); err != nil {
		t.Fatal(err)
	}
	if n := s.pub.count(notify.EventAppdataBackupFailed); n != 1 {
		t.Fatalf("the same window alerted %d times, want once", n)
	}
}

func TestScheduleTick_StartsContainersAnInterruptedBackupLeftStopped(t *testing.T) {
	ctx := context.Background()
	s := newScheduledAppdata(t)
	c, err := s.fake.Inspect(ctx, "sonarr")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.fake.Stop(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(s.stateDir, appdataJournalName)
	if err := os.WriteFile(journal, []byte(`{"containers":["sonarr"]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.h.runner.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := containerState(t, s.fake, "sonarr"); got != "running" {
		t.Fatalf("sonarr = %s after the tick, want the interrupted backup's container started again", got)
	}
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatalf("journal survived: %v", err)
	}
}

func TestWireAppdata_WithoutDockerRegistersNothing(t *testing.T) {
	registry := job.NewRegistry()
	handler := &api.Handler{}
	wireAppdata(handler, registry, nil, &recordingPublisher{})
	if handler.Appdata != nil {
		t.Fatal("Handler.Appdata set without a service")
	}
	if newAppdataService(nil, nil, nil, nil, "") != nil {
		t.Fatal("newAppdataService built a service with no Docker client")
	}
	runner := &scheduleRunner{}
	wireAppdataSchedule(runner, nil, nil, nil)
	if len(runner.OtherJobs) != 0 || runner.AppdataRecovery != nil {
		t.Fatal("the schedule was wired to an absent service")
	}
}

// backedUp runs a backup of one container through the daemon's handler.
func backedUp(t *testing.T, w *containersWiringHarness, name string) {
	t.Helper()
	status, body := w.doBody(t, http.MethodPost, "/appdata/backup", `{"containers":["`+name+`"]}`)
	if status != http.StatusOK {
		t.Fatalf("POST /appdata/backup = %d %s", status, body)
	}
	var queued struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &queued)
	if done := w.awaitJobByID(t, queued.ID); done.Status != job.StatusSucceeded {
		t.Fatalf("backup: %s %s", done.Status, done.ErrorMessage)
	}
}

func archiveNamed(t *testing.T, dest, container string) string {
	t.Helper()
	entries, _ := os.ReadDir(dest)
	for _, e := range entries {
		if strings.Contains(e.Name(), "-"+container+"-") {
			return e.Name()
		}
	}
	t.Fatalf("no archive of %s in %s", container, dest)
	return ""
}

// POST /appdata/backup/restore/preview queues an appdata_restore_preview job
// through the real daemon server and its job registry, and the preview it
// reports is read back over HTTP without the appdata being changed.
func TestAppdataWiring_RestorePreviewIsReachableOverHTTP(t *testing.T) {
	w := newContainersWiringHarness(t)
	dest := filepath.Join(w.root, "pool-backups")
	svc := appdataBackupService(t, w.apps, w.arrays, w.db, w.root, dest)
	wireAppdata(w.handler, w.registry, svc, &recordingPublisher{})
	backedUp(t, w, "jellyfin")
	archive := archiveNamed(t, dest, "jellyfin")
	live := filepath.Join(w.appdata, "library.db")
	if err := os.WriteFile(live, []byte("corrupted"), 0o644); err != nil {
		t.Fatal(err)
	}

	req := `{"container":"jellyfin","archive":"` + archive + `","destinationId":"pool"}`
	status, body := w.doBody(t, http.MethodPost, "/appdata/backup/restore/preview", req)
	if status != http.StatusOK {
		t.Fatalf("POST restore/preview = %d %s", status, body)
	}
	var queued struct {
		ID    string `json:"id"`
		Type  string `json:"type"`
		Class string `json:"class"`
	}
	if err := json.Unmarshal(body, &queued); err != nil || queued.Type != "appdata_restore_preview" || queued.Class != "service" {
		t.Fatalf("job = %s (%v), want an appdata_restore_preview service job", body, err)
	}
	if done := w.awaitJobByID(t, queued.ID); done.Status != job.StatusSucceeded {
		t.Fatalf("preview job: %s %s", done.Status, done.ErrorMessage)
	}

	status, body = w.do(t, http.MethodGet, "/appdata/backup/restore/preview/"+queued.ID)
	if status != http.StatusOK {
		t.Fatalf("GET restore/preview/%s = %d %s", queued.ID, status, body)
	}
	var preview struct {
		Directories []struct {
			Replaced struct {
				Files  int      `json:"files"`
				Sample []string `json:"sample"`
			} `json:"replaced"`
		} `json:"directories"`
	}
	if err := json.Unmarshal(body, &preview); err != nil || len(preview.Directories) == 0 ||
		preview.Directories[0].Replaced.Files != 1 || preview.Directories[0].Replaced.Sample[0] != "library.db" {
		t.Fatalf("preview = %s (%v), want library.db replaced", body, err)
	}
	if got, _ := os.ReadFile(live); string(got) != "corrupted" {
		t.Fatalf("library.db after the preview = %q, want it untouched", got)
	}
	if left, _ := os.ReadDir(filepath.Dir(filepath.Dir(w.appdata))); len(left) != 1 {
		t.Fatalf("the preview left %v next to the appdata location, want only appdata", left)
	}
	if status, body = w.do(t, http.MethodGet, "/appdata/backup/restore/preview/00000000-0000-4000-8000-000000000000"); status != http.StatusNotFound {
		t.Fatalf("GET of an unknown preview = %d %s, want 404", status, body)
	}
}

// gatedContainers holds one RequireArrayRunning call until released, so a
// test can keep a preview job running while it submits other jobs.
type gatedContainers struct {
	backup.AppdataContainers
	mu      sync.Mutex
	armed   bool
	skip    int
	entered chan struct{}
	release chan struct{}
}

func (g *gatedContainers) arm(skip int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.armed, g.skip = true, skip
}

func (g *gatedContainers) RequireArrayRunning() error {
	g.mu.Lock()
	hold := false
	if g.armed {
		if g.skip == 0 {
			hold, g.armed = true, false
		} else {
			g.skip--
		}
	}
	g.mu.Unlock()
	if hold {
		close(g.entered)
		<-g.release
	}
	return g.AppdataContainers.RequireArrayRunning()
}

// A backup submitted while a preview job is running is never failed by it:
// the same container's backup queues behind the preview, another
// container's runs beside it, and neither raises the backup-failed alert.
func TestAppdataWiring_ABackupNeverFailsBecauseAPreviewIsRunning(t *testing.T) {
	w := newContainersWiringHarness(t)
	sonarrDir := filepath.Join(w.root, "cache", "appdata", "sonarr")
	if err := os.MkdirAll(sonarrDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sonarrDir, "sonarr.db"), []byte("series"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.fake.AddContainer(container.Container{
		ID: "b", Name: "sonarr", Image: "sonarr", Tag: "4", State: "exited",
		Mounts: []container.Mount{{Source: sonarrDir, Destination: "/config", ReadWrite: true}},
	})
	dest := filepath.Join(w.root, "pool-backups")
	svc := appdataBackupService(t, w.apps, w.arrays, w.db, w.root, dest)
	gate := &gatedContainers{AppdataContainers: svc.Containers, entered: make(chan struct{}), release: make(chan struct{})}
	svc.Containers = gate
	pub := &recordingPublisher{}
	wireAppdata(w.handler, w.registry, svc, pub)
	backedUp(t, w, "jellyfin")
	archive := archiveNamed(t, dest, "jellyfin")

	// The handler's own array check is the first call, the job's is the
	// second, which is the one held.
	gate.arm(1)
	req := `{"container":"jellyfin","archive":"` + archive + `","destinationId":"pool"}`
	status, body := w.doBody(t, http.MethodPost, "/appdata/backup/restore/preview", req)
	if status != http.StatusOK {
		t.Fatalf("POST restore/preview = %d %s", status, body)
	}
	var preview struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &preview)
	select {
	case <-gate.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the preview job never started")
	}

	status, body = w.doBody(t, http.MethodPost, "/appdata/backup", `{"containers":["sonarr"]}`)
	if status != http.StatusOK {
		t.Fatalf("POST /appdata/backup for another container = %d %s", status, body)
	}
	var other struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &other)
	if done := w.awaitJobByID(t, other.ID); done.Status != job.StatusSucceeded {
		t.Fatalf("a backup of another container beside the preview: %s %s", done.Status, done.ErrorMessage)
	}

	status, body = w.doBody(t, http.MethodPost, "/appdata/backup", `{"containers":["jellyfin"]}`)
	if status != http.StatusOK {
		t.Fatalf("POST /appdata/backup for the previewed container = %d %s", status, body)
	}
	var same struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	_ = json.Unmarshal(body, &same)
	if same.Status != "queued" {
		t.Fatalf("a backup of the container being previewed is %q, want it queued behind the preview", same.Status)
	}

	close(gate.release)
	if done := w.awaitJobByID(t, preview.ID); done.Status != job.StatusSucceeded {
		t.Fatalf("preview: %s %s", done.Status, done.ErrorMessage)
	}
	if done := w.awaitJobByID(t, same.ID); done.Status != job.StatusSucceeded {
		t.Fatalf("the queued backup: %s %s", done.Status, done.ErrorMessage)
	}
	if n := pub.count(notify.EventAppdataBackupFailed); n != 0 {
		t.Fatalf("appdata_backup_failed published %d times, want none", n)
	}
}
