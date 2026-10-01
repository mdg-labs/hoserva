package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/auth"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// TestStacksWiring_HandlerCreatesAndRemovesUnderTheStateDirectory builds the
// Compose stack service the way main.go does (wireStacks, over a real
// migrated database and the real machine key) and drives the /stacks
// operations through the handler: every file lands under the state
// directory's stacks/, the .env is sealed in the database, and appdata is
// deleted only when removal asks for it.
func TestStacksWiring_HandlerCreatesAndRemovesUnderTheStateDirectory(t *testing.T) {
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

	h := &api.Handler{}
	docker := container.NewFakeRunner()
	wireStacks(h, store.NewStackStore(db), machineKey, docker, stateDir, nil, nil)
	if h.Stacks == nil {
		t.Fatal("wireStacks left Handler.Stacks nil, so every /stacks operation would 501")
	}

	if _, err := h.CreateStack(ctx, &apiv1.CreateStackRequest{
		Name:    "nginx",
		Compose: "services:\n  web:\n    image: nginx:1.27\n",
		Env:     apiv1.NewOptString("TOKEN=s3cret\n"),
	}); err != nil {
		t.Fatalf("CreateStack: %v", err)
	}
	dir := filepath.Join(stateDir, "stacks", "nginx")
	if got, err := os.ReadFile(filepath.Join(dir, ".env")); err != nil || string(got) != "TOKEN=s3cret\n" {
		t.Fatalf(".env = %q, %v", got, err)
	}
	var sealed []byte
	if err := db.QueryRowContext(ctx, `SELECT env FROM stacks WHERE name = 'nginx'`).Scan(&sealed); err != nil {
		t.Fatalf("reading the stored .env: %v", err)
	}
	if bytes.Contains(sealed, []byte("s3cret")) {
		t.Fatal("the .env is stored in the database in the clear")
	}

	list, err := h.ListStacks(ctx)
	if err != nil || len(list.Stacks) != 1 || list.Stacks[0].Name != "nginx" {
		t.Fatalf("ListStacks = %+v, %v", list, err)
	}

	// Every remove lists the project's containers first, so with no Docker
	// service it is refused before anything is removed.
	if _, err := h.RemoveStack(ctx, apiv1.RemoveStackParams{Name: "nginx"}); err == nil {
		t.Fatal("RemoveStack succeeded with no Docker service to list the project's containers")
	}
	if _, err := os.Stat(filepath.Join(dir, "docker-compose.yml")); err != nil {
		t.Fatalf("a refused remove deleted the stack's files: %v", err)
	}
	h.Stacks.Provider = container.NewFakeProvider()

	if _, err := h.RemoveStack(ctx, apiv1.RemoveStackParams{Name: "nginx"}); err != nil {
		t.Fatalf("RemoveStack: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a removed stack's generated files were kept: %v", err)
	}
	if _, err := h.CreateStack(ctx, &apiv1.CreateStackRequest{Name: "nginx", Compose: "services: {}\n"}); err != nil {
		t.Fatalf("installing the removed stack again: %v", err)
	}

	// With no array state readable, appdata deletion is refused and nothing
	// is removed.
	_, err = h.RemoveStack(ctx, apiv1.RemoveStackParams{Name: "nginx", DeleteAppdata: apiv1.NewOptBool(true)})
	if err == nil {
		t.Fatal("RemoveStack with deleteAppdata succeeded although no array state is readable")
	}
	if _, err := os.Stat(filepath.Join(dir, "docker-compose.yml")); err != nil {
		t.Fatalf("a refused remove deleted the stack's files: %v", err)
	}
}

// downRemovesContainers stands in for the Docker Engine: `compose down`
// deletes the containers with the IDs from the fake.
type downRemovesContainers struct {
	container.Runner
	fake *container.FakeProvider
	ids  []string
}

func (d downRemovesContainers) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := d.Runner.Run(ctx, name, args...)
	for _, a := range args {
		if err == nil && a == "down" {
			for _, id := range d.ids {
				d.fake.RemoveContainer(id)
			}
		}
	}
	return out, err
}

// TestStacksWiring_RemoveWithAppdataThroughTheDaemon wires the stack service
// the way main.go does on top of the container wiring: DELETE
// /stacks/{name}?deleteAppdata=true deletes the stack's appdata, found by its
// Compose project label and the stored cache disk, only with the array
// running, and never the appdata of a container that is not the stack's.
func TestStacksWiring_RemoveWithAppdataThroughTheDaemon(t *testing.T) {
	ctx := context.Background()
	w := newContainersWiringHarness(t)
	runner := container.NewFakeRunner()
	engine := downRemovesContainers{Runner: runner, fake: w.fake, ids: []string{"s1"}}
	cipher := &auth.FakeMachineKeyStore{}
	key, err := auth.LoadOrGenerateMachineKey(ctx, filepath.Join(w.root, "stacks.key"), cipher)
	if err != nil {
		t.Fatalf("machine key: %v", err)
	}
	wireStacks(w.handler, store.NewStackStore(w.db), key, engine, w.root, w.apps, arrayActionAdmit(w.scheduler))

	appdataRoot := filepath.Join(w.root, "cache", "appdata")
	mine := filepath.Join(appdataRoot, "web")
	if err := os.MkdirAll(mine, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mine, "data"), []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.fake.AddContainer(container.Container{
		ID: "s1", Name: "web-web-1", State: "running",
		Labels: map[string]string{
			"com.docker.compose.project":             "web",
			"com.docker.compose.project.working_dir": filepath.Join(w.root, "stacks", "web"),
		},
		Mounts: []container.Mount{{Source: mine, Destination: "/data", ReadWrite: true}},
	})
	create := func() {
		t.Helper()
		if _, err := w.handler.CreateStack(ctx, &apiv1.CreateStackRequest{Name: "web", Compose: "services:\n  web:\n    image: nginx\n"}); err != nil {
			t.Fatalf("CreateStack: %v", err)
		}
	}
	create()
	stackDir := filepath.Join(w.root, "stacks", "web")

	w.storageReady.Store(false)
	before := len(runner.Calls())
	status, body := w.do(t, http.MethodDelete, "/stacks/web?deleteAppdata=true")
	if status != http.StatusConflict || !bytes.Contains(body, []byte(`"array_stopped"`)) {
		t.Fatalf("DELETE on a stopped array = %d %s, want 409 array_stopped", status, body)
	}
	if len(runner.Calls()) != before {
		t.Fatalf("docker ran although the remove was refused: %v", runner.Calls()[before:])
	}
	if _, err := os.Stat(filepath.Join(mine, "data")); err != nil {
		t.Fatalf("appdata touched by a refused remove: %v", err)
	}

	w.storageReady.Store(true)
	if status, body := w.do(t, http.MethodDelete, "/stacks/web?deleteAppdata=true"); status != http.StatusOK {
		t.Fatalf("DELETE ?deleteAppdata=true = %d %s, want 200", status, body)
	}
	for _, p := range []string{mine, stackDir} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s still exists after an explicit request to delete the stack's appdata: %v", p, err)
		}
	}
	if _, err := os.Stat(w.appdata); err != nil {
		t.Fatalf("another container's appdata was deleted: %v", err)
	}

	create()
	if status, body := w.do(t, http.MethodDelete, "/stacks/web"); status != http.StatusOK {
		t.Fatalf("DELETE = %d %s, want 200", status, body)
	}
	create()
}
