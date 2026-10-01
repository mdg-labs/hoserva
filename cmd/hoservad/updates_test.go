package main

import (
	"context"
	"encoding/json"
	"net/http"
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
}

func newUpdateWiring(t *testing.T) *updateWiring {
	t.Helper()
	w := newContainersWiringHarness(t)
	reg := container.NewFakeRegistry()
	updates := store.NewUpdateStore(w.db)
	checker := newUpdateChecker(w.apps, updates, reg)
	wireContainerUpdates(w.handler, w.registry, checker)

	// The schedule seeds its window on the first tick, then 06:00 the next
	// day opens it.
	clock := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	schedules := api.NewScheduleService(api.NewScheduleStore(w.db), api.NewSettingsStore(w.db))
	schedules.Now = func() time.Time { return clock }
	runner := &scheduleRunner{Schedules: schedules, Scheduler: w.scheduler}
	wireContainerUpdateSchedule(runner, checker, w.scheduler, func() time.Duration { return 0 })
	return &updateWiring{w: w, registry: reg, runner: runner, clock: &clock, updates: updates}
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
	wireContainerUpdates(&api.Handler{}, h.registry, checker)
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

func TestContainerUpdateWiring_AnUpdateModeJobFailsAndUpdatesNothing(t *testing.T) {
	u := newUpdateWiring(t)
	u.run("web", "nginx", "latest", updDigestOld)
	j, err := u.w.scheduler.Submit(context.Background(), job.TypeContainerUpdate, nil, []byte(`{"mode":"update"}`))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	done := awaitTerminal(t, u.w.handler.Store, j.ID)
	if done.Status != job.StatusFailed || !strings.Contains(done.ErrorMessage, "not implemented") {
		t.Fatalf("job = %s %q, want failed as not implemented", done.Status, done.ErrorMessage)
	}
	if calls := u.w.fake.Calls(); len(calls) != 0 {
		t.Fatalf("the Engine was called: %v", calls)
	}
}

func TestRandomUpdateCheckJitter_StaysWithinItsBound(t *testing.T) {
	for i := 0; i < 200; i++ {
		if d := randomUpdateCheckJitter(); d < 0 || d >= updateCheckMaxJitter {
			t.Fatalf("jitter = %s, want within [0, %s)", d, updateCheckMaxJitter)
		}
	}
}
