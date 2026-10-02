package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/auth"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/store"
	"github.com/mdg-labs/hoserva/internal/template"

	"gopkg.in/yaml.v3"

	_ "modernc.org/sqlite"
)

// resolvingRunner stands in for `docker compose`: `config --quiet` succeeds,
// and `config --format json` prints each service's `ports` entries with the
// stack's own .env substituted, as Compose resolves them.
type resolvingRunner struct {
	fail error
}

func (r *resolvingRunner) Run(_ context.Context, _ []string, _ string, args ...string) ([]byte, error) {
	if r.fail != nil {
		return nil, r.fail
	}
	flag := func(name string) string {
		for i, a := range args {
			if a == name && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	if flag("--format") != "json" {
		return nil, nil
	}
	compose, err := os.ReadFile(flag("--file"))
	if err != nil {
		return nil, err
	}
	envFile, err := os.ReadFile(flag("--env-file"))
	if err != nil {
		return nil, err
	}
	env := map[string]string{}
	for _, l := range strings.Split(string(envFile), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			env[k] = strings.Trim(v, `"'`)
		}
	}
	var doc struct {
		Services map[string]struct {
			Ports []string `yaml:"ports"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(compose, &doc); err != nil {
		return nil, err
	}
	type port struct {
		Published string `json:"published,omitempty"`
	}
	type service struct {
		Ports []port `json:"ports"`
	}
	out := struct {
		Services map[string]service `json:"services"`
	}{map[string]service{}}
	for name, s := range doc.Services {
		svc := service{}
		for _, spec := range s.Ports {
			parts := strings.Split(os.Expand(spec, func(k string) string { return env[k] }), ":")
			if len(parts) >= 2 {
				svc.Ports = append(svc.Ports, port{Published: parts[len(parts)-2]})
			}
		}
		out.Services[name] = svc
	}
	return json.Marshal(out)
}

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
	compose := &resolvingRunner{}
	wireStacks(h, job.NewRegistry(), store.NewStackStore(db), machineKey, compose, stateDir, apps, nil)
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
	second, err := h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{Name: apiv1.NewOptString("jellyfin-2")}, apiv1.InstallTemplateParams{ID: "jellyfin"})
	if err != nil {
		t.Fatalf("a second jellyfin under another name: %v", err)
	}
	for _, in := range second.Plan.Inputs {
		if in.Name == "WEBUI_PORT" && (in.Value.Or("") != "8098" || in.RequestedValue.Or("") != "8096") {
			t.Errorf("the second stack's WEBUI_PORT = %q (requested %q), want 8098: 8096 is another container's and 8097 the first stack's, which was never started", in.Value.Or(""), in.RequestedValue.Or(""))
		}
	}

	// The network mode is checked against the same Docker service's networks,
	// and listing them is served by the handler main.go builds.
	h.Container = provider
	provider.SetNetworks(container.Network{Name: "bridge", Driver: "bridge"}, container.Network{Name: "lan", Driver: "macvlan"})
	listed, err := h.ListDockerNetworks(ctx)
	if err != nil || !listed.Available || len(listed.Networks) != 2 || listed.Networks[1].Name != "lan" {
		t.Fatalf("ListDockerNetworks = %+v, %v, want the provider's networks", listed, err)
	}
	missing, err := h.PreviewTemplateInstall(ctx, &apiv1.TemplateInstallRequest{NetworkMode: apiv1.NewOptString("iot")}, apiv1.PreviewTemplateInstallParams{ID: "jellyfin"})
	if err != nil || len(missing.Warnings) != 1 || missing.Warnings[0].Command.Or("") != "docker network create iot" {
		t.Errorf("a network the provider lacks: %+v, %v, want a missing_network warning with its command", missing, err)
	}
	_, err = h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{Name: apiv1.NewOptString("jellyfin-iot"), NetworkMode: apiv1.NewOptString("iot")}, apiv1.InstallTemplateParams{ID: "jellyfin"})
	if st := h.NewError(ctx, err); st.StatusCode != 409 || st.Response.Code != "network_missing" {
		t.Errorf("an install on a missing network: %d %q, want 409 network_missing", st.StatusCode, st.Response.Code)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "stacks", "jellyfin-iot")); err == nil {
		t.Error("an install on a missing network was written anyway")
	}
	onLAN, err := h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{Name: apiv1.NewOptString("jellyfin-lan"), NetworkMode: apiv1.NewOptString("lan"), Restart: apiv1.NewOptTemplateInstallRequestRestart(apiv1.TemplateInstallRequestRestartNo)}, apiv1.InstallTemplateParams{ID: "jellyfin"})
	if err != nil {
		t.Fatalf("an install on an existing network: %v", err)
	}
	written, err := os.ReadFile(filepath.Join(stateDir, "stacks", "jellyfin-lan", "docker-compose.yml"))
	if err != nil || !strings.Contains(string(written), "external: true") || !strings.Contains(string(written), `restart: "no"`) || onLAN.Plan.Compose != string(written) {
		t.Errorf("docker-compose.yml = %q, %v, want the network and the restart policy", written, err)
	}

	compose.fail = errors.New("compose cannot resolve the stack")
	_, err = h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{Name: apiv1.NewOptString("jellyfin-3")}, apiv1.InstallTemplateParams{ID: "jellyfin"})
	if st := h.NewError(ctx, err); st.StatusCode != 502 || st.Response.Code != "stack_action_failed" {
		t.Errorf("stacks whose ports cannot be read: %d %q, want 502 stack_action_failed", st.StatusCode, st.Response.Code)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "stacks", "jellyfin-3")); err == nil {
		t.Error("an install that could not check the ports was written anyway")
	}
	compose.fail = nil

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
