package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/auth"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/job"
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
	wireStacks(h, job.NewRegistry(), store.NewStackStore(db), machineKey, docker, stateDir, nil, nil)
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

func (d downRemovesContainers) Run(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	out, err := d.Runner.Run(ctx, env, name, args...)
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
	wireStacks(w.handler, w.registry, store.NewStackStore(w.db), key, engine, w.root, w.apps, arrayActionAdmit(w.scheduler))

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

// composeConfigRejecting stands in for the Docker CLI: it fails
// `docker compose config` for the file named in reject, and records every
// call.
type composeConfigRejecting struct {
	container.Runner
	reject string
	mu     sync.Mutex
	calls  [][]string
}

func (c *composeConfigRejecting) Run(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	c.mu.Lock()
	c.calls = append(c.calls, append([]string(nil), args...))
	c.mu.Unlock()
	for _, a := range args {
		if a == "config" && c.reject != "" {
			if b, err := os.ReadFile(argAfter(args, "--file")); err == nil && string(b) == c.reject {
				return nil, &exec.ExitError{}
			}
		}
	}
	return c.Runner.Run(ctx, env, name, args...)
}

func argAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func (c *composeConfigRejecting) ran(sub string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, args := range c.calls {
		for _, a := range args {
			if a == sub {
				n++
			}
		}
	}
	return n
}

// TestStacksWiring_EditAndStartThroughTheDaemon wires the stack service the
// way main.go does and drives PUT /stacks/{name} and POST
// /stacks/{name}/start over the daemon's real server: the edit is stored
// with the manually-edited flag and regenerated on disk, a refused file
// leaves both as they were, and the start is a stack_start job that runs
// `docker compose up --detach` on the edited file and is refused while the
// array is stopped.
func TestStacksWiring_EditAndStartThroughTheDaemon(t *testing.T) {
	ctx := context.Background()
	w := newContainersWiringHarness(t)
	const oldText = "services:\n  web:\n    image: nginx:1.27\n"
	const newText = "services:\n  web:\n    image: nginx:1.28\n"
	const badText = "services: [\n"
	runner := &composeConfigRejecting{Runner: container.NewFakeRunner(), reject: badText}
	cipher := &auth.FakeMachineKeyStore{}
	key, err := auth.LoadOrGenerateMachineKey(ctx, filepath.Join(w.root, "stacks.key"), cipher)
	if err != nil {
		t.Fatalf("machine key: %v", err)
	}
	wireStacks(w.handler, w.registry, store.NewStackStore(w.db), key, runner, w.root, w.apps, arrayActionAdmit(w.scheduler))
	if _, err := w.handler.CreateStack(ctx, &apiv1.CreateStackRequest{Name: "web", Compose: oldText, Env: apiv1.NewOptString("TOKEN=s3cret\n")}); err != nil {
		t.Fatalf("CreateStack: %v", err)
	}
	composePath := filepath.Join(w.root, "stacks", "web", "docker-compose.yml")

	status, body := w.doBody(t, http.MethodPut, "/stacks/web", `{"compose":"services: [\n"}`)
	if status != http.StatusBadRequest || !bytes.Contains(body, []byte(`"invalid_stack"`)) {
		t.Fatalf("PUT of an invalid file = %d %s, want 400 invalid_stack", status, body)
	}
	if b, _ := os.ReadFile(composePath); string(b) != oldText {
		t.Fatalf("a refused edit changed the file: %q", b)
	}
	status, body = w.do(t, http.MethodGet, "/stacks/web")
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"manuallyEdited":false`)) {
		t.Fatalf("GET after a refused edit = %d %s, want manuallyEdited false", status, body)
	}

	status, body = w.doBody(t, http.MethodPut, "/stacks/web?dryRun=true", `{"compose":"services:\n  web:\n    image: nginx:1.28\n"}`)
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"applied":false`)) {
		t.Fatalf("PUT dryRun = %d %s, want 200 applied false", status, body)
	}
	if b, _ := os.ReadFile(composePath); string(b) != oldText {
		t.Fatalf("a dry run changed the file: %q", b)
	}

	status, body = w.doBody(t, http.MethodPut, "/stacks/web", `{"compose":"services:\n  web:\n    image: nginx:1.28\n"}`)
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"applied":true`)) || !bytes.Contains(body, []byte(`"manuallyEdited":true`)) {
		t.Fatalf("PUT = %d %s, want 200 applied and manually edited", status, body)
	}
	if b, _ := os.ReadFile(composePath); string(b) != newText {
		t.Fatalf("docker-compose.yml = %q after the edit, want the new text", b)
	}
	var sealed []byte
	if err := w.db.QueryRowContext(ctx, `SELECT env FROM stacks WHERE name = 'web'`).Scan(&sealed); err != nil || bytes.Contains(sealed, []byte("s3cret")) {
		t.Fatalf("the stored .env = %q, %v: it must stay sealed", sealed, err)
	}
	if runner.ran("up") != 0 {
		t.Fatal("an edit started the stack")
	}

	w.storageReady.Store(false)
	status, body = w.do(t, http.MethodPost, "/stacks/web/start")
	if status != http.StatusConflict || !bytes.Contains(body, []byte(`"array_stopped"`)) {
		t.Fatalf("POST start on a stopped array = %d %s, want 409 array_stopped", status, body)
	}
	if jobs, err := w.handler.Store.List(ctx, job.ListFilter{}); err != nil || len(jobs) != 0 {
		t.Fatalf("a refused start queued %d jobs (%v)", len(jobs), err)
	}
	w.storageReady.Store(true)

	status, body = w.do(t, http.MethodPost, "/stacks/web/start")
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"stack_start"`)) {
		t.Fatalf("POST start = %d %s, want 200 with a stack_start job", status, body)
	}
	var queued struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &queued); err != nil {
		t.Fatalf("decoding the job: %v", err)
	}
	if done := w.awaitJobByID(t, queued.ID); done.Status != job.StatusSucceeded {
		t.Fatalf("stack_start = %s (%s), want succeeded", done.Status, done.ErrorMessage)
	}
	if runner.ran("up") != 1 {
		t.Fatalf("compose up ran %d times, want once", runner.ran("up"))
	}
}
