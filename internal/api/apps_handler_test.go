package api_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/store"
)

type stackListsRows struct{ *stackMemStore }

func (s stackListsRows) List(context.Context) ([]store.Stack, error) {
	out := make([]store.Stack, 0, len(s.rows))
	for _, st := range s.rows {
		out = append(out, st)
	}
	return out, nil
}

type stackListFails struct{ *stackMemStore }

func (stackListFails) List(context.Context) ([]store.Stack, error) {
	return nil, errors.New("database is locked")
}

func composeLabels(project, workingDir string) map[string]string {
	return map[string]string{
		"com.docker.compose.project":             project,
		"com.docker.compose.project.working_dir": workingDir,
	}
}

// newStackedApps is a handler over a Docker with three containers: one an
// installed stack started, one that stack's project name run from another
// directory, and one started by hand.
func newStackedApps(t *testing.T) *api.Handler {
	t.Helper()
	h, root := newStacksHandler(t)
	mem := &stackMemStore{rows: map[string]store.Stack{"immich": {Name: "immich"}}}
	h.Stacks.Store = stackListsRows{mem}
	fake := container.NewFakeProvider()
	fake.AddContainer(container.Container{ID: "c-stack", Name: "immich-server-1", State: "running", Labels: composeLabels("immich", filepath.Join(root, "immich"))})
	fake.AddContainer(container.Container{ID: "c-elsewhere", Name: "immich-by-hand", State: "running", Labels: composeLabels("immich", "/home/me/immich")})
	fake.AddContainer(container.Container{ID: "c-hand", Name: "portainer", State: "running"})
	h.Container = fake
	return h
}

func TestListApps_ReportsTheStackOfTheContainersItManagesAndNoneForTheRest(t *testing.T) {
	h := newStackedApps(t)

	list, err := h.ListApps(context.Background())
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	got := map[string]apiv1.OptString{}
	for _, a := range list.Apps {
		got[a.Name] = a.Stack
	}
	if s, ok := got["immich-server-1"].Get(); !ok || s != "immich" {
		t.Fatalf("immich-server-1 stack = %v, want immich", got["immich-server-1"])
	}
	for _, name := range []string{"immich-by-hand", "portainer"} {
		if s, ok := got[name].Get(); ok {
			t.Fatalf("%s stack = %q, want none: no stack started it", name, s)
		}
	}
}

func TestListApps_AStackReadThatFailsFailsTheRequestInsteadOfReportingEveryContainerUnmanaged(t *testing.T) {
	h := newStackedApps(t)
	h.Stacks.Store = stackListFails{&stackMemStore{rows: map[string]store.Stack{}}}

	list, err := h.ListApps(context.Background())
	if err == nil {
		t.Fatalf("ListApps = %+v, nil; want an error", list)
	}
	if status, _ := statusOf(h, err); status != 500 {
		t.Fatalf("ListApps status = %d, want 500", status)
	}
}

func TestListApps_NoStackServiceMeansNoContainerIsManaged(t *testing.T) {
	h := newStackedApps(t)
	h.Stacks = nil

	list, err := h.ListApps(context.Background())
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	for _, a := range list.Apps {
		if a.Stack.Set {
			t.Fatalf("%s has stack %q with no stack service", a.Name, a.Stack.Value)
		}
	}
}

func TestGetApp_ReportsTheStackAndFailsOnAStackReadThatFails(t *testing.T) {
	h := newStackedApps(t)

	app, err := h.GetApp(context.Background(), apiv1.GetAppParams{ID: "immich-server-1"})
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if s, ok := app.Stack.Get(); !ok || s != "immich" {
		t.Fatalf("GetApp stack = %v, want immich", app.Stack)
	}
	app, err = h.GetApp(context.Background(), apiv1.GetAppParams{ID: "portainer"})
	if err != nil || app.Stack.Set {
		t.Fatalf("GetApp(portainer) = %+v, %v; want no stack", app, err)
	}

	h.Stacks.Store = stackListFails{&stackMemStore{rows: map[string]store.Stack{}}}
	if _, err := h.GetApp(context.Background(), apiv1.GetAppParams{ID: "immich-server-1"}); err == nil {
		t.Fatal("GetApp succeeded although the stacks could not be read")
	}
}
