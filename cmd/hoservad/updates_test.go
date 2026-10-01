package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/store"
)

const (
	updDigestOld = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	updDigestNew = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// updateWiring is the update check wired the way main.go wires it, on the
// daemon's real API server, scheduler and schedule loop over a fake Docker
// and a fake registry.
type updateWiring struct {
	w        *containersWiringHarness
	registry *container.FakeRegistry
	runner   *scheduleRunner
	clock    *time.Time
	updates  *store.UpdateStore
	checker  *container.UpdateChecker
	// archives is the appdata backup destination the pre-update snapshots
	// are written to.
	archives string
}

func newUpdateWiring(t *testing.T) *updateWiring {
	t.Helper()
	w := newContainersWiringHarness(t)
	reg := container.NewFakeRegistry()
	updates := store.NewUpdateStore(w.db)
	checker := newUpdateChecker(w.apps, updates, reg)
	archives := filepath.Join(w.root, "pool-backups")
	appdata := appdataBackupService(t, w.apps, w.arrays, w.db, w.root, archives)
	wireContainerUpdates(w.handler, w.registry, checker, newUpdater(w.apps, store.NewImageHistoryStore(w.db), appdata, checker), w.apps.awaitReconciled)
	w.startReconcile()

	// The schedule seeds its window on the first tick, then 06:00 the next
	// day opens it.
	clock := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	schedules := api.NewScheduleService(api.NewScheduleStore(w.db), api.NewSettingsStore(w.db))
	schedules.Now = func() time.Time { return clock }
	runner := &scheduleRunner{Schedules: schedules, Scheduler: w.scheduler}
	wireContainerUpdateSchedule(runner, checker, w.scheduler, func() time.Duration { return 0 })
	return &updateWiring{w: w, registry: reg, runner: runner, clock: &clock, updates: updates, checker: checker, archives: archives}
}

// run starts container name on image repo:tag, pulled as localDigest.
func (u *updateWiring) run(name, repo, tag, localDigest string) {
	id := "sha256:img-" + name
	u.w.fake.AddContainer(container.Container{ID: "id-" + name, Name: name, Image: repo, Tag: tag, ImageID: id, State: "running"})
	u.w.fake.AddImage(container.Image{ID: id, RepoDigests: []string{repo + "@" + localDigest}})
}

func (u *updateWiring) checkJobs(t *testing.T) []*job.Job {
	t.Helper()
	jobs, err := u.w.handler.Store.List(context.Background(), job.ListFilter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var out []*job.Job
	for _, j := range jobs {
		if j.Type == job.TypeContainerUpdate {
			out = append(out, j)
		}
	}
	return out
}

// tickIntoWindow runs the schedule loop's tick before and inside the daily
// window and returns the check job it submitted, finished.
func (u *updateWiring) tickIntoWindow(t *testing.T) *job.Job {
	t.Helper()
	ctx := context.Background()
	if err := u.runner.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if jobs := u.checkJobs(t); len(jobs) != 0 {
		t.Fatalf("a check started before its window opened: %v", jobs)
	}
	*u.clock = time.Date(2026, 9, 30, 6, 1, 0, 0, time.UTC)
	if err := u.runner.tick(ctx); err != nil {
		t.Fatalf("tick in the window: %v", err)
	}
	jobs := u.checkJobs(t)
	if len(jobs) != 1 {
		t.Fatalf("check jobs after the window opened = %d, want one", len(jobs))
	}
	return awaitTerminal(t, u.w.handler.Store, jobs[0].ID)
}

type appUpdatesBody struct {
	Available bool `json:"available"`
	Updates   []struct {
		Container    string `json:"container"`
		Status       string `json:"status"`
		Kind         string `json:"kind"`
		AvailableTag string `json:"availableTag"`
		Message      string `json:"message"`
	} `json:"updates"`
}

func (u *updateWiring) statuses(t *testing.T) map[string]string {
	t.Helper()
	status, body := u.w.do(t, http.MethodGet, "/apps/updates")
	if status != http.StatusOK {
		t.Fatalf("GET /apps/updates = %d %s", status, body)
	}
	var got appUpdatesBody
	if err := json.Unmarshal(body, &got); err != nil || !got.Available {
		t.Fatalf("body = %s (%v), want available", body, err)
	}
	out := map[string]string{}
	for _, up := range got.Updates {
		out[up.Container] = up.Status + "/" + up.Kind + "/" + up.AvailableTag
	}
	return out
}

func TestContainerUpdateWiring_TheDailyCheckRunsFromTheScheduleAndIsReadOverHTTP(t *testing.T) {
	u := newUpdateWiring(t)
	u.run("web", "nginx", "latest", updDigestOld)
	u.registry.SetDigest(container.ImageRef{Registry: "docker.io", Repository: "library/nginx", Tag: "latest"}, updDigestNew)
	u.run("db", "postgres", "16.4", updDigestOld)
	pg := container.ImageRef{Registry: "docker.io", Repository: "library/postgres", Tag: "16.4"}
	u.registry.SetDigest(pg, updDigestOld)
	u.registry.SetTags(pg, []string{"16.4", "16.6"})
	u.run("app", "ghcr.io/acme/app", "2", updDigestOld)
	u.registry.SetDigest(container.ImageRef{Registry: "ghcr.io", Repository: "acme/app", Tag: "2"}, updDigestOld)
	u.registry.SetTags(container.ImageRef{Registry: "ghcr.io", Repository: "acme/app", Tag: "2"}, []string{"2"})

	if got := u.statuses(t)["web"]; got != "not_checked//" {
		t.Fatalf("web before any check = %q, want not_checked", got)
	}
	done := u.tickIntoWindow(t)
	if done.Status != job.StatusSucceeded {
		t.Fatalf("check job = %s %s", done.Status, done.ErrorMessage)
	}
	if done.Class != job.ClassService || !strings.Contains(string(done.Params), `"mode":"check"`) {
		t.Fatalf("check job = class %s params %s, want a service-class check", done.Class, done.Params)
	}
	got := u.statuses(t)
	if got["web"] != "update_available/new_build/" || got["db"] != "update_available/new_version/16.6" || got["app"] != "up_to_date//" {
		t.Fatalf("statuses = %v, want web a new build of latest, db a new version 16.6, app up to date", got)
	}
	for _, c := range u.registry.Calls() {
		if !strings.HasPrefix(c, "manifest ") && !strings.HasPrefix(c, "tags ") {
			t.Errorf("registry call %q is neither a manifest nor a tag listing", c)
		}
	}

	for i := 0; i < 2; i++ {
		if err := u.runner.tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(u.checkJobs(t)); n != 1 {
		t.Fatalf("check jobs after more ticks in the same window = %d, want still one: at most one check a day", n)
	}
}

func TestContainerUpdateWiring_ARateLimitedRegistryReadsAsSkippedOverHTTP(t *testing.T) {
	u := newUpdateWiring(t)
	u.run("web", "nginx", "latest", updDigestOld)
	u.registry.SetRegistryError("docker.io", container.ErrRateLimited)

	if done := u.tickIntoWindow(t); done.Status != job.StatusSucceeded {
		t.Fatalf("check job = %s %s", done.Status, done.ErrorMessage)
	}
	status, body := u.w.do(t, http.MethodGet, "/apps/updates")
	var got appUpdatesBody
	if err := json.Unmarshal(body, &got); status != http.StatusOK || err != nil {
		t.Fatalf("GET /apps/updates = %d %s (%v)", status, body, err)
	}
	for _, up := range got.Updates {
		if up.Container == "web" && (up.Status != "skipped" || !strings.Contains(up.Message, "rate limiting")) {
			t.Fatalf("web = %+v, want skipped saying the registry is rate limiting, never up_to_date", up)
		}
	}
}

func TestContainerUpdateWiring_NothingIsWiredWithoutDockerButTheJobStillRefusesHonestly(t *testing.T) {
	w := newContainersWiringHarness(t)
	if status, body := w.do(t, http.MethodGet, "/apps/updates"); status != http.StatusOK || !strings.Contains(string(body), `"available":false`) {
		t.Fatalf("GET /apps/updates without wiring = %d %s, want available=false", status, body)
	}

	h := newScheduleHarness(t, func() time.Time { return time.Now() }, nil)
	checker := newUpdateChecker(nil, nil, nil)
	wireContainerUpdates(&api.Handler{}, h.registry, checker, newUpdater(nil, nil, nil, checker), func(context.Context) error { return nil })
	wireContainerUpdateSchedule(h.runner, checker, h.runner.Scheduler, randomUpdateCheckJitter)
	if _, ok := h.runner.OtherJobs["container_update_check"]; ok {
		t.Fatal("the schedule entry was wired with no checker; its window must stay unclaimed")
	}
	j, err := h.runner.Scheduler.Submit(context.Background(), job.TypeContainerUpdate, nil, []byte(`{"mode":"check"}`))
	if err != nil {
		t.Fatalf("Submit: %v (container_update must be registered)", err)
	}
	done := awaitTerminal(t, h.jobs, j.ID)
	if done.Status != job.StatusFailed || !strings.Contains(done.ErrorMessage, "docker is not configured") {
		t.Fatalf("job = %s %q, want failed naming the missing Docker", done.Status, done.ErrorMessage)
	}
}

func TestContainerUpdateWiring_UpdateAndRevertJobsWithoutDockerFailHonestly(t *testing.T) {
	h := newScheduleHarness(t, func() time.Time { return time.Now() }, nil)
	wireContainerUpdates(&api.Handler{}, h.registry, nil, newUpdater(nil, nil, nil, nil), func(context.Context) error { return nil })
	for mode, body := range map[string]string{
		"update": `{"mode":"update","containers":["web"]}`,
		"revert": `{"mode":"revert","containers":["web"]}`,
	} {
		j, err := h.runner.Scheduler.Submit(context.Background(), job.TypeContainerUpdate, nil, []byte(body))
		if err != nil {
			t.Fatalf("%s: Submit: %v", mode, err)
		}
		if done := awaitTerminal(t, h.jobs, j.ID); done.Status != job.StatusFailed || !strings.Contains(done.ErrorMessage, "docker is not configured") {
			t.Fatalf("%s: job = %s %q, want failed naming the missing Docker", mode, done.Status, done.ErrorMessage)
		}
	}
}

func TestRandomUpdateCheckJitter_StaysWithinItsBound(t *testing.T) {
	for i := 0; i < 200; i++ {
		if d := randomUpdateCheckJitter(); d < 0 || d >= updateCheckMaxJitter {
			t.Fatalf("jitter = %s, want within [0, %s)", d, updateCheckMaxJitter)
		}
	}
}

// jellyfinOnCache replaces the harness's stopped jellyfin with a running one
// whose appdata sits on the cache disk, on an image that has a newer one to
// pull.
func (u *updateWiring) jellyfinOnCache() {
	u.w.fake.RemoveContainer("a")
	u.w.fake.AddContainer(container.Container{
		ID: "a", Name: "jellyfin", Image: "jf", Tag: "10", ImageID: "sha256:jf-old", State: "running",
		Mounts: []container.Mount{{Source: u.w.appdata, Destination: "/config", ReadWrite: true}},
	})
	u.w.fake.AddImage(container.Image{ID: "sha256:jf-old", RepoTags: []string{"jf:10"}})
	u.w.fake.AddImage(container.Image{ID: "sha256:jf-new"})
	u.w.fake.SetPull("jf:10", "sha256:jf-new")
}

func (u *updateWiring) imageID(t *testing.T, name string) string {
	t.Helper()
	c, err := u.w.fake.Inspect(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return c.ImageID
}

// post sends a request through the daemon's API and returns the job it
// queued, finished.
func (u *updateWiring) postJob(t *testing.T, path, body string) *job.Job {
	t.Helper()
	status, resp := u.w.doBody(t, http.MethodPost, path, body)
	if status != http.StatusOK {
		t.Fatalf("POST %s = %d %s, want 200 with the queued job", path, status, resp)
	}
	var queued struct {
		ID    string `json:"id"`
		Type  string `json:"type"`
		Class string `json:"class"`
		Job   *struct {
			ID string `json:"id"`
		} `json:"job"`
	}
	if err := json.Unmarshal(resp, &queued); err != nil {
		t.Fatal(err)
	}
	id := queued.ID
	if queued.Job != nil {
		id = queued.Job.ID
	} else if queued.Type != "container_update" || queued.Class != "service" {
		t.Fatalf("POST %s queued %s/%s, want a container_update service job", path, queued.Type, queued.Class)
	}
	return u.w.awaitJobByID(t, id)
}

func (u *updateWiring) archivesWithReason(t *testing.T, reason string) []string {
	t.Helper()
	entries, err := os.ReadDir(u.archives)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.Contains(e.Name(), "."+reason+".") {
			out = append(out, e.Name())
		}
	}
	return out
}

type updateHistoryBody struct {
	Available bool `json:"available"`
	Records   []struct {
		Container       string `json:"container"`
		PreviousImageID string `json:"previousImageId"`
		SnapshotArchive string `json:"snapshotArchive"`
		RevertedAt      string `json:"revertedAt"`
		Revertible      bool   `json:"revertible"`
	} `json:"records"`
}

func (u *updateWiring) history(t *testing.T) updateHistoryBody {
	t.Helper()
	status, body := u.w.do(t, http.MethodGet, "/apps/updates/history")
	var got updateHistoryBody
	if err := json.Unmarshal(body, &got); status != http.StatusOK || err != nil || !got.Available {
		t.Fatalf("GET /apps/updates/history = %d %s (%v)", status, body, err)
	}
	return got
}

// The whole feature through the daemon's real entry points: the update is
// queued over HTTP, snapshots the appdata through the real appdata backup,
// swaps the image and keeps the old one; the revert, also over HTTP, restores
// the snapshot over what the new version wrote and puts the old image back.
func TestContainerUpdateWiring_UpdateThenRevertOverHTTP(t *testing.T) {
	u := newUpdateWiring(t)
	u.jellyfinOnCache()
	live := filepath.Join(u.w.appdata, "library.db")

	if done := u.postJob(t, "/apps/jellyfin/update", ``); done.Status != job.StatusSucceeded {
		t.Fatalf("update job = %s %s", done.Status, done.ErrorMessage)
	}
	if got := u.imageID(t, "jellyfin"); got != "sha256:jf-new" {
		t.Fatalf("jellyfin runs %s, want the new image", got)
	}
	snapshots := u.archivesWithReason(t, "pre-update")
	if len(snapshots) != 1 {
		t.Fatalf("pre-update archives = %v, want one written before the update", snapshots)
	}
	hist := u.history(t)
	if len(hist.Records) != 1 || !hist.Records[0].Revertible || hist.Records[0].PreviousImageID != "sha256:jf-old" || hist.Records[0].SnapshotArchive != snapshots[0] {
		t.Fatalf("history = %+v, want one revertible record naming the old image and the snapshot", hist)
	}

	if err := os.WriteFile(live, []byte("migrated by the new version"), 0o644); err != nil {
		t.Fatal(err)
	}
	if done := u.postJob(t, "/apps/jellyfin/revert", ``); done.Status != job.StatusSucceeded {
		t.Fatalf("revert job = %s %s", done.Status, done.ErrorMessage)
	}
	if got, _ := os.ReadFile(live); string(got) != "precious" {
		t.Fatalf("library.db after the revert = %q, want the content the snapshot held", got)
	}
	if got := u.imageID(t, "jellyfin"); got != "sha256:jf-old" {
		t.Fatalf("jellyfin runs %s after the revert, want the old image", got)
	}
	if kept := u.archivesWithReason(t, "pre-restore"); len(kept) != 1 {
		t.Fatalf("pre-restore archives = %v, want what the revert replaced kept in one", kept)
	}
	if hist = u.history(t); hist.Records[0].Revertible || hist.Records[0].RevertedAt == "" {
		t.Fatalf("history after the revert = %+v", hist)
	}
	if status, body := u.w.doBody(t, http.MethodPost, "/apps/jellyfin/revert", ``); status != http.StatusConflict || !strings.Contains(string(body), "nothing_to_revert") {
		t.Fatalf("a second revert = %d %s, want 409 nothing_to_revert", status, body)
	}
}

// The data-loss scenario end to end: an update whose swap fails leaves the
// container on its old image, and a revert afterwards is refused instead of
// restoring the snapshot over the live appdata.
func TestContainerUpdateWiring_AFailedUpdateLeavesNothingToRevertOverHTTP(t *testing.T) {
	u := newUpdateWiring(t)
	u.jellyfinOnCache()
	u.w.fake.FailOn("recreate-local", "jellyfin", errors.New("starting the replacement container: port is already allocated"))
	live := filepath.Join(u.w.appdata, "library.db")

	done := u.postJob(t, "/apps/jellyfin/update", ``)
	if done.Status != job.StatusFailed || !strings.Contains(done.ErrorMessage, "port is already allocated") || !strings.Contains(done.ErrorMessage, "still runs its previous image") {
		t.Fatalf("update job = %s %q, want failed saying why and that the container is unchanged", done.Status, done.ErrorMessage)
	}
	if got := u.imageID(t, "jellyfin"); got != "sha256:jf-old" {
		t.Fatalf("jellyfin runs %s after the failed update, want the old image", got)
	}
	if hist := u.history(t); len(hist.Records) != 0 {
		t.Fatalf("history = %+v, want no record of an update that did not happen", hist)
	}

	if err := os.WriteFile(live, []byte("written after the failed update"), 0o644); err != nil {
		t.Fatal(err)
	}
	if status, body := u.w.doBody(t, http.MethodPost, "/apps/jellyfin/revert", ``); status != http.StatusConflict || !strings.Contains(string(body), "nothing_to_revert") {
		t.Fatalf("revert after a failed update = %d %s, want 409 nothing_to_revert", status, body)
	}
	if got, _ := os.ReadFile(live); string(got) != "written after the failed update" {
		t.Fatalf("library.db = %q: a revert after a failed update overwrote live appdata", got)
	}
}

// A snapshot that cannot be written stops the update before anything changes.
func TestContainerUpdateWiring_AFailedSnapshotStopsTheUpdateOverHTTP(t *testing.T) {
	u := newUpdateWiring(t)
	u.jellyfinOnCache()
	if err := os.WriteFile(u.archives, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	done := u.postJob(t, "/apps/jellyfin/update", ``)
	if done.Status != job.StatusFailed || !strings.Contains(done.ErrorMessage, "so it was not updated") {
		t.Fatalf("update job = %s %q, want failed with the container not updated", done.Status, done.ErrorMessage)
	}
	if got := u.imageID(t, "jellyfin"); got != "sha256:jf-old" {
		t.Fatalf("jellyfin runs %s, want the old image", got)
	}
	for _, c := range u.w.fake.Calls() {
		if c.Op == "recreate" || c.Op == "recreate-local" || strings.HasPrefix(c.ID, "hoserva-previous:") {
			t.Fatalf("the Engine was asked to %s %s after a failed snapshot", c.Op, c.ID)
		}
	}
}

func TestContainerUpdateWiring_BulkUpdateSkipsContainersThatOptedOutOverHTTP(t *testing.T) {
	u := newUpdateWiring(t)
	u.run("web", "nginx", "latest", updDigestOld)
	u.registry.SetDigest(container.ImageRef{Registry: "docker.io", Repository: "library/nginx", Tag: "latest"}, updDigestNew)
	u.run("db", "redis", "7", updDigestOld)
	u.registry.SetDigest(container.ImageRef{Registry: "docker.io", Repository: "library/redis", Tag: "7"}, updDigestNew)
	for _, name := range []string{"web", "db"} {
		u.w.fake.AddImage(container.Image{ID: "sha256:new-" + name})
	}
	u.w.fake.SetPull("nginx:latest", "sha256:new-web")
	u.w.fake.SetPull("redis:7", "sha256:new-db")
	if err := u.checker.Run(context.Background(), io.Discard); err != nil {
		t.Fatal(err)
	}

	if status, body := u.w.doBody(t, http.MethodPut, "/apps/db/update-policy", `{"bulkExcluded":true}`); status != http.StatusOK || !strings.Contains(string(body), `"bulkExcluded":true`) {
		t.Fatalf("PUT update-policy = %d %s", status, body)
	}
	if status, body := u.w.do(t, http.MethodGet, "/apps/updates"); status != http.StatusOK || !strings.Contains(string(body), `"container":"db"`) || !strings.Contains(string(body), `"bulkExcluded":true`) {
		t.Fatalf("GET /apps/updates = %d %s, want db shown as excluded", status, body)
	}

	done := u.postJob(t, "/apps/updates", `{}`)
	if done.Status != job.StatusSucceeded {
		t.Fatalf("bulk job = %s %s", done.Status, done.ErrorMessage)
	}
	if u.imageID(t, "web") != "sha256:new-web" || u.imageID(t, "db") != "sha256:img-db" {
		t.Fatalf("web runs %s, db %s; want only web updated", u.imageID(t, "web"), u.imageID(t, "db"))
	}
	if status, body := u.w.doBody(t, http.MethodPost, "/apps/updates", `{"containers":["nope"]}`); status != http.StatusNotFound {
		t.Fatalf("POST /apps/updates naming an unknown container = %d %s, want 404", status, body)
	}
}

func TestContainerUpdateWiring_ImageKeepDaysSettingOverHTTP(t *testing.T) {
	u := newUpdateWiring(t)
	if status, body := u.w.do(t, http.MethodGet, "/settings/apps"); status != http.StatusOK || !strings.Contains(string(body), `"imageKeepDays":7`) {
		t.Fatalf("GET /settings/apps = %d %s, want the 7 day default", status, body)
	}
	if status, body := u.w.doBody(t, http.MethodPut, "/settings/apps", `{"imageKeepDays":30}`); status != http.StatusOK || !strings.Contains(string(body), `"imageKeepDays":30`) {
		t.Fatalf("PUT /settings/apps = %d %s", status, body)
	}
	u.jellyfinOnCache()
	if done := u.postJob(t, "/apps/jellyfin/update", ``); done.Status != job.StatusSucceeded {
		t.Fatalf("update job = %s %s", done.Status, done.ErrorMessage)
	}
	var rec struct {
		Records []struct {
			UpdatedAt string `json:"updatedAt"`
			KeepUntil string `json:"keepUntil"`
		} `json:"records"`
	}
	_, body := u.w.do(t, http.MethodGet, "/apps/updates/history")
	if err := json.Unmarshal(body, &rec); err != nil || len(rec.Records) != 1 {
		t.Fatalf("history = %s (%v)", body, err)
	}
	updated, err1 := time.Parse(time.RFC3339Nano, rec.Records[0].UpdatedAt)
	until, err2 := time.Parse(time.RFC3339Nano, rec.Records[0].KeepUntil)
	if err1 != nil || err2 != nil || until.Sub(updated) != 30*24*time.Hour {
		t.Fatalf("kept from %s until %s (%v, %v), want the configured 30 days", rec.Records[0].UpdatedAt, rec.Records[0].KeepUntil, err1, err2)
	}
	if status, body := u.w.doBody(t, http.MethodPut, "/settings/apps", `{"imageKeepDays":0}`); status == http.StatusOK {
		t.Fatalf("PUT /settings/apps with 0 days = %d %s, want a refusal", status, body)
	}
}
