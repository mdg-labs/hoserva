package api

import (
	"context"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/store"
)

type stubUpdateResults struct{ rows []store.ImageUpdateCheck }

func (s stubUpdateResults) PutImageUpdateCheck(context.Context, store.ImageUpdateCheck) error {
	return nil
}
func (s stubUpdateResults) ListImageUpdateChecks(context.Context) ([]store.ImageUpdateCheck, error) {
	return s.rows, nil
}
func (s stubUpdateResults) DeleteImageUpdateCheck(context.Context, string) error { return nil }

func TestListAppUpdates_ReportsWhatTheCheckFoundAndNeverUpToDateForWhatItDidNot(t *testing.T) {
	at := time.Date(2026, 9, 30, 6, 10, 0, 0, time.UTC)
	fake := container.NewFakeProvider()
	fake.AddContainer(container.Container{ID: "1", Name: "db", Image: "postgres", Tag: "16.4"})
	fake.AddContainer(container.Container{ID: "2", Name: "web", Image: "nginx", Tag: "latest", ImageID: "sha256:web"})
	fake.AddImage(container.Image{ID: "sha256:web", RepoDigests: []string{"nginx@sha256:1111111111111111111111111111111111111111111111111111111111111111"}})
	fake.AddContainer(container.Container{ID: "3", Name: "new", Image: "redis", Tag: "7"})
	h := &Handler{AppUpdates: &container.UpdateChecker{Provider: fake, Results: stubUpdateResults{rows: []store.ImageUpdateCheck{
		{Image: "postgres:16.4", CheckedAt: at, Status: store.UpdateAvailable, Kind: store.UpdateKindNewVersion, AvailableTag: "16.6"},
		{Image: "nginx:latest", CheckedAt: at, Status: store.UpdateAvailable, Kind: store.UpdateKindNewBuild},
	}}}}

	got, err := h.ListAppUpdates(context.Background())
	if err != nil || !got.Available || len(got.Updates) != 3 {
		t.Fatalf("ListAppUpdates = %+v, %v, want three containers", got, err)
	}
	by := map[string]apiv1.AppUpdate{}
	for _, u := range got.Updates {
		by[u.Container] = u
	}
	if u := by["db"]; u.Status != apiv1.AppUpdateStatusUpdateAvailable || u.Kind.Value != apiv1.AppUpdateKindNewVersion || u.AvailableTag.Value != "16.6" || !u.CheckedAt.Value.Equal(at) {
		t.Errorf("db = %+v, want a new version 16.6", u)
	}
	if u := by["web"]; u.Kind.Value != apiv1.AppUpdateKindNewBuild || u.AvailableTag.Set {
		t.Errorf("web = %+v, want a new build with no tag", u)
	}
	if u := by["new"]; u.Status != apiv1.AppUpdateStatusNotChecked || u.CheckedAt.Set {
		t.Errorf("new = %+v, want not_checked", u)
	}
}

func TestListAppUpdates_DockerUnreachableIsAvailableFalseNotAnError(t *testing.T) {
	fake := container.NewFakeProvider()
	fake.SetUnavailable(nil)
	h := &Handler{AppUpdates: &container.UpdateChecker{Provider: fake, Results: stubUpdateResults{}}}
	got, err := h.ListAppUpdates(context.Background())
	if err != nil || got.Available || !got.Message.Set {
		t.Fatalf("ListAppUpdates = %+v, %v, want available=false with a message", got, err)
	}
	if got, err := (&Handler{}).ListAppUpdates(context.Background()); err != nil || got.Available {
		t.Fatalf("unwired ListAppUpdates = %+v, %v, want available=false", got, err)
	}
}
