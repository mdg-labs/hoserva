package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/auth"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/store"
	"github.com/mdg-labs/hoserva/internal/template"

	_ "modernc.org/sqlite"
)

// TestTemplateInstallWiring_InstallsACatalogTemplateUnderTheStateDirectory
// builds the stack and install services the way main.go does
// (wireStacks, then wireTemplateInstall over the same Docker service) and
// installs through the handler: the template is read from the state
// directory's catalog/, the port a running container publishes is moved off,
// and the stack's files and row land under the state directory.
func TestTemplateInstallWiring_InstallsACatalogTemplateUnderTheStateDirectory(t *testing.T) {
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

	fixture := filepath.Join("..", "..", "internal", "template", "testdata", "catalog", "jellyfin", template.ComposeFile)
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	catalog := filepath.Join(stateDir, catalogDirName, "jellyfin")
	if err := os.MkdirAll(catalog, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(catalog, template.ComposeFile), data, 0o644); err != nil {
		t.Fatal(err)
	}

	provider := container.NewFakeProvider()
	provider.AddContainer(container.Container{ID: "c1", Name: "other", State: "running", Ports: []container.Port{{HostPort: 8096, ContainerPort: 80, Protocol: "tcp"}}})
	apps := &appServices{Lifecycle: &container.Lifecycle{Provider: provider}}
	shares := func(context.Context) ([]string, error) { return []string{"media"}, nil }

	h := &api.Handler{}
	wireStacks(h, store.NewStackStore(db), machineKey, container.NewFakeRunner(), stateDir, apps, nil)
	wireTemplateInstall(h, stateDir, apps, shares)
	if h.TemplateInstall == nil {
		t.Fatal("wireTemplateInstall left Handler.TemplateInstall nil, so every /templates operation would 501")
	}
	// The host's own listeners are not part of what is under test here.
	hostPorts, ok := h.TemplateInstall.Ports.(template.HostPorts)
	if !ok || hostPorts.Containers != container.Provider(provider) {
		t.Fatalf("the port source is %T, want the Apps Docker service's published ports", h.TemplateInstall.Ports)
	}
	emptyNet := t.TempDir()
	for _, f := range []string{"tcp", "tcp6", "udp", "udp6"} {
		if err := os.WriteFile(filepath.Join(emptyNet, f), []byte("  sl  local_address rem_address   st\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	hostPorts.ProcNet = emptyNet
	h.TemplateInstall.Ports = hostPorts

	preview, err := h.PreviewTemplateInstall(ctx, &apiv1.TemplateInstallRequest{}, apiv1.PreviewTemplateInstallParams{ID: "jellyfin"})
	if err != nil {
		t.Fatalf("PreviewTemplateInstall: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "stacks", "jellyfin")); err == nil {
		t.Fatal("a preview wrote the stack")
	}
	if len(preview.Privileges) != 0 {
		t.Errorf("privileges = %+v, want none for jellyfin", preview.Privileges)
	}

	res, err := h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{}, apiv1.InstallTemplateParams{ID: "jellyfin"})
	if err != nil {
		t.Fatalf("InstallTemplate: %v", err)
	}
	if res.Stack.Name != "jellyfin" || res.Stack.Template.Source != template.SourceCurated || res.Stack.Template.ID != "jellyfin" || res.Stack.Template.Revision != "1" {
		t.Errorf("stack = %+v", res.Stack)
	}
	var moved bool
	for _, in := range res.Plan.Inputs {
		if in.Name == "WEBUI_PORT" {
			moved = in.Value.Or("") == "8097" && in.RequestedValue.Or("") == "8096"
		}
	}
	if !moved {
		t.Errorf("WEBUI_PORT was not moved off the port another container publishes: %+v", res.Plan.Inputs)
	}
	dir := filepath.Join(stateDir, "stacks", "jellyfin")
	env, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil || !strings.Contains(string(env), "WEBUI_PORT=8097\n") {
		t.Errorf(".env = %q, %v", env, err)
	}
	meta, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil || !strings.Contains(string(meta), `"id": "jellyfin"`) || !strings.Contains(string(meta), `"revision": "1"`) || !strings.Contains(string(meta), `"source": "hoserva"`) {
		t.Errorf("meta.json = %q, %v", meta, err)
	}
	list, err := h.ListStacks(ctx)
	if err != nil || len(list.Stacks) != 1 || list.Stacks[0].Template.ID != "jellyfin" {
		t.Fatalf("ListStacks = %+v, %v", list, err)
	}

	_, err = h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{}, apiv1.InstallTemplateParams{ID: "jellyfin"})
	if st := h.NewError(ctx, err); st.StatusCode != 409 || st.Response.Code != "stack_exists" {
		t.Errorf("a second install of the same name: %d %q, want 409 stack_exists", st.StatusCode, st.Response.Code)
	}
	_, err = h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{}, apiv1.InstallTemplateParams{ID: "nope"})
	if st := h.NewError(ctx, err); st.StatusCode != 404 || st.Response.Code != "template_not_found" {
		t.Errorf("an unknown template: %d %q, want 404 template_not_found", st.StatusCode, st.Response.Code)
	}
}

func TestTemplateInstallWiring_WithNoDockerServiceTheOperationsStay501(t *testing.T) {
	h := &api.Handler{Stacks: &container.StackService{}}
	wireTemplateInstall(h, t.TempDir(), nil, nil)
	if h.TemplateInstall != nil {
		t.Fatal("template install was wired with no Docker service, so it would treat every port as free")
	}
	_, err := h.InstallTemplate(context.Background(), &apiv1.TemplateInstallRequest{}, apiv1.InstallTemplateParams{ID: "jellyfin"})
	if st := h.NewError(context.Background(), err); st.StatusCode != 501 {
		t.Errorf("status = %d, want 501", st.StatusCode)
	}
}
