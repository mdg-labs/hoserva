package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/auth"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// TestListAppsWiring_NamesTheStackOfAContainerAnInstalledStackStarted builds
// the handler the way main.go does (Docker service, then wireStacks over the
// state directory) and lists the containers: the one Compose started from
// the stack's directory under the state directory names its stack, one of
// the same project run from elsewhere and one started by hand do not.
func TestListAppsWiring_NamesTheStackOfAContainerAnInstalledStackStarted(t *testing.T) {
	ctx := context.Background()
	stateDir := t.TempDir()
	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	db, err := sql.Open("sqlite", store.DSN(filepath.Join(stateDir, "hoservad.db")))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(ctx); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	machineKey, err := auth.LoadOrGenerateMachineKey(ctx, filepath.Join(stateDir, "secret.key"), api.NewAuthStore(db))
	if err != nil {
		t.Fatalf("machine key: %v", err)
	}

	provider := container.NewFakeProvider()
	apps := &appServices{Lifecycle: &container.Lifecycle{Provider: provider}}
	h := &api.Handler{Container: provider}
	wireStacks(h, job.NewRegistry(), store.NewStackStore(db), machineKey, &resolvingRunner{}, stateDir, apps, nil)
	if _, err := h.Stacks.Create(ctx, container.NewStack{Name: "nginx", Compose: "services:\n  web:\n    image: nginx:1.27\n"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	label := func(dir string) map[string]string {
		return map[string]string{
			"com.docker.compose.project":             "nginx",
			"com.docker.compose.project.working_dir": dir,
		}
	}
	provider.AddContainer(container.Container{ID: "c1", Name: "nginx-web-1", State: "running", Labels: label(filepath.Join(stateDir, "stacks", "nginx"))})
	provider.AddContainer(container.Container{ID: "c2", Name: "nginx-by-hand", State: "running", Labels: label("/home/me/nginx")})
	provider.AddContainer(container.Container{ID: "c3", Name: "portainer", State: "running"})

	list, err := h.ListApps(ctx)
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	got := map[string]apiv1.OptString{}
	for _, a := range list.Apps {
		got[a.Name] = a.Stack
	}
	if s, ok := got["nginx-web-1"].Get(); !ok || s != "nginx" {
		t.Errorf("nginx-web-1 stack = %v, want nginx", got["nginx-web-1"])
	}
	for _, name := range []string{"nginx-by-hand", "portainer"} {
		if got[name].Set {
			t.Errorf("%s has stack %q, want none", name, got[name].Value)
		}
	}
}
