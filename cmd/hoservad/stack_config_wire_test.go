package main

import (
	"context"
	"database/sql"
	"encoding/json"
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

	_ "modernc.org/sqlite"
)

// TestStackConfigWiring_ReadsAndChangesAnInstalledStacksInputs builds the
// stack and install services the way main.go does and goes through the
// handler: the inputs come from the stored Compose text, the secret stays in
// the sealed row and the .env, a port a running container publishes is
// refused, and an accepted change reaches both the row and the file while
// the Compose file stays as it was.
func TestStackConfigWiring_ReadsAndChangesAnInstalledStacksInputs(t *testing.T) {
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

	data, err := os.ReadFile(filepath.Join("..", "..", "internal", "template", "testdata", "catalog", "aio-notes", template.ComposeFile))
	if err != nil {
		t.Fatal(err)
	}
	catalog := filepath.Join(stateDir, catalogDirName, "aio-notes")
	if err := os.MkdirAll(catalog, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(catalog, template.ComposeFile), data, 0o644); err != nil {
		t.Fatal(err)
	}

	provider := container.NewFakeProvider()
	provider.AddContainer(container.Container{ID: "c1", Name: "other", State: "running", Ports: []container.Port{{HostPort: 4000, ContainerPort: 80, Protocol: "tcp"}}})
	apps := &appServices{Lifecycle: &container.Lifecycle{Provider: provider}}
	h := &api.Handler{}
	wireStacks(h, job.NewRegistry(), store.NewStackStore(db), machineKey, &resolvingRunner{}, stateDir, apps, nil)
	wireTemplateInstall(h, stateDir, apps, func(context.Context) ([]string, error) { return nil, nil })
	if h.TemplateInstall == nil {
		t.Fatal("wireTemplateInstall left Handler.TemplateInstall nil, so the config operations would 501")
	}
	hostPorts := h.TemplateInstall.Ports.(template.HostPorts)
	emptyNet := t.TempDir()
	for _, f := range []string{"tcp", "tcp6", "udp", "udp6"} {
		if err := os.WriteFile(filepath.Join(emptyNet, f), []byte("  sl  local_address rem_address   st\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	hostPorts.ProcNet = emptyNet
	h.TemplateInstall.Ports = hostPorts

	if _, err := h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{}, apiv1.InstallTemplateParams{ID: "aio-notes"}); err != nil {
		t.Fatalf("InstallTemplate: %v", err)
	}
	dir := filepath.Join(stateDir, "stacks", "aio-notes")
	envBefore, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	var secret string
	for _, l := range strings.Split(string(envBefore), "\n") {
		if v, ok := strings.CutPrefix(l, "DB_PASSWORD="); ok {
			secret = v
		}
	}
	if secret == "" {
		t.Fatalf("setup: .env = %q", envBefore)
	}
	composeBefore, err := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := h.GetStackConfig(ctx, apiv1.GetStackConfigParams{Name: "aio-notes"})
	if err != nil {
		t.Fatalf("GetStackConfig: %v", err)
	}
	byName := map[string]apiv1.StackConfigInput{}
	for _, in := range cfg.Inputs {
		byName[in.Name] = in
	}
	if byName["WEBUI_PORT"].Value.Or("") != "3000" || byName["SITE_NAME"].Value.Or("") != "Notes" {
		t.Errorf("inputs = %+v", cfg.Inputs)
	}
	if pw := byName["DB_PASSWORD"]; !pw.Set.Or(false) || pw.Value.Set {
		t.Errorf("DB_PASSWORD = %+v, want set and no value", pw)
	}

	_, err = h.UpdateStackConfig(ctx, &apiv1.UpdateStackConfigRequest{Values: apiv1.NewOptUpdateStackConfigRequestValues(map[string]string{"WEBUI_PORT": "4000"})}, apiv1.UpdateStackConfigParams{Name: "aio-notes"})
	if st := h.NewError(ctx, err); st.StatusCode != 409 || st.Response.Code != "no_free_port" {
		t.Fatalf("a port another container publishes: %d %q, want 409 no_free_port", st.StatusCode, st.Response.Code)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, ".env")); string(got) != string(envBefore) {
		t.Errorf("a refused update changed the .env:\n%s", got)
	}
	if row, err := h.Stacks.Env(ctx, "aio-notes"); err != nil || row != string(envBefore) {
		t.Errorf("a refused update changed the row's .env: %q, %v", row, err)
	}

	updated, err := h.UpdateStackConfig(ctx, &apiv1.UpdateStackConfigRequest{Values: apiv1.NewOptUpdateStackConfigRequestValues(map[string]string{"WEBUI_PORT": "3100", "SITE_NAME": "Team"})}, apiv1.UpdateStackConfigParams{Name: "aio-notes"})
	if err != nil {
		t.Fatalf("UpdateStackConfig: %v", err)
	}
	if body, _ := json.Marshal(updated); strings.Contains(string(body), secret) {
		t.Error("the response carries the secret")
	}
	envAfter, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil || !strings.Contains(string(envAfter), "WEBUI_PORT=3100\n") || !strings.Contains(string(envAfter), "SITE_NAME=Team\n") || !strings.Contains(string(envAfter), "DB_PASSWORD="+secret+"\n") {
		t.Errorf(".env = %q, %v", envAfter, err)
	}
	if row, err := h.Stacks.Env(ctx, "aio-notes"); err != nil || row != string(envAfter) {
		t.Errorf("the row's .env = %q, %v; want the file's", row, err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "docker-compose.yml")); string(got) != string(composeBefore) {
		t.Error("an update rewrote the Compose file")
	}
}
