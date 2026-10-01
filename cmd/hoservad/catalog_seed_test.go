package main

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
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

// startedTemplates builds the stack and template services the way main.go
// does (wireStacks, then startTemplates over the same Docker service) over
// stateDir.
func startedTemplates(t *testing.T, stateDir string) *api.Handler {
	t.Helper()
	return startedTemplatesWith(t, stateDir, nil)
}

// startedTemplatesWith is startedTemplates with the notifier main.go hands
// startTemplates.
func startedTemplatesWith(t *testing.T, stateDir string, notifier catalogPublisher) *api.Handler {
	t.Helper()
	return startedTemplatesIn(t, stateDir, notifier, false)
}

// startedTemplatesWithSettings is startedTemplatesWith with the catalog
// settings store main.go hands startTemplates, so the automatic checks and
// /settings/catalog are wired.
func startedTemplatesWithSettings(t *testing.T, stateDir string, notifier catalogPublisher) *api.Handler {
	t.Helper()
	return startedTemplatesIn(t, stateDir, notifier, true)
}

func startedTemplatesIn(t *testing.T, stateDir string, notifier catalogPublisher, withSettings bool) *api.Handler {
	t.Helper()
	ctx := context.Background()
	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	db, err := sql.Open("sqlite", store.DSN(filepath.Join(t.TempDir(), "hoservad.db")))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(ctx); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	machineKey, err := auth.LoadOrGenerateMachineKey(ctx, filepath.Join(t.TempDir(), "secret.key"), api.NewAuthStore(db))
	if err != nil {
		t.Fatalf("machine key: %v", err)
	}
	apps := &appServices{Lifecycle: &container.Lifecycle{Provider: container.NewFakeProvider()}}
	shares := func(context.Context) ([]string, error) { return []string{"media"}, nil }

	h := &api.Handler{}
	wireStacks(h, job.NewRegistry(), store.NewStackStore(db), machineKey, container.NewFakeRunner(), stateDir, apps, nil)
	var settings api.CatalogSettingsStore
	if withSettings {
		settings = store.NewCatalogSettingsStore(db)
	}
	startTemplates(h, stateDir, apps, shares, notifier, settings)
	if h.TemplateInstall == nil {
		t.Fatal("startTemplates left Handler.TemplateInstall nil, so every /templates operation would 501")
	}
	// The host's own listeners are not part of what is under test here.
	hostPorts := h.TemplateInstall.Ports.(template.HostPorts)
	emptyNet := t.TempDir()
	for _, f := range []string{"tcp", "tcp6", "udp", "udp6"} {
		if err := os.WriteFile(filepath.Join(emptyNet, f), []byte("  sl  local_address rem_address   st\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	hostPorts.ProcNet = emptyNet
	h.TemplateInstall.Ports = hostPorts
	return h
}

// TestStartTemplates_AFreshStateDirectoryResolvesTheEmbeddedCuratedCatalog
// starts with an empty state directory, no refresh ever having run and no
// network use anywhere: the catalog directory is installed from the snapshot
// embedded in the binary, and a preview of jellyfin resolves the curated
// template from it.
func TestStartTemplates_AFreshStateDirectoryResolvesTheEmbeddedCuratedCatalog(t *testing.T) {
	if _, _, err := template.EmbeddedSnapshot(); err != nil {
		t.Fatalf("%v — run `make catalog-snapshot` first", err)
	}
	stateDir := t.TempDir()
	h := startedTemplates(t, stateDir)

	catalogDir := filepath.Join(stateDir, catalogDirName)
	if _, err := os.Stat(filepath.Join(catalogDir, "index.json")); err != nil {
		t.Fatalf("startup did not install the embedded catalog: %v", err)
	}
	plan, err := h.PreviewTemplateInstall(context.Background(), &apiv1.TemplateInstallRequest{}, apiv1.PreviewTemplateInstallParams{ID: "jellyfin"})
	if err != nil {
		t.Fatalf("PreviewTemplateInstall(jellyfin) from a fresh state directory: %v", err)
	}
	if plan.Template.ID != "jellyfin" || plan.Template.Source != template.SourceCurated {
		t.Errorf("template = %+v, want jellyfin from the %q source", plan.Template, template.SourceCurated)
	}
	if !strings.Contains(plan.Compose, "lscr.io/linuxserver/jellyfin:") {
		t.Errorf("compose = %q, want the curated jellyfin image", plan.Compose)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "stacks", "jellyfin")); err == nil {
		t.Error("a preview wrote the stack")
	}
}

func TestSeedCatalog_LeavesANewerOnDiskCatalogAlone(t *testing.T) {
	if _, _, err := template.EmbeddedSnapshot(); err != nil {
		t.Fatalf("%v — run `make catalog-snapshot` first", err)
	}
	stateDir := t.TempDir()
	dir := filepath.Join(stateDir, catalogDirName)
	if err := os.MkdirAll(filepath.Join(dir, "jellyfin"), 0o755); err != nil {
		t.Fatal(err)
	}
	const index = `{"schema":1,"serial":9999999999,"templates":[]}`
	const compose = "name: refreshed\n"
	if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte(index), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "jellyfin", template.ComposeFile), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := seedCatalog(stateDir); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "jellyfin", template.ComposeFile))
	if err != nil || string(got) != compose {
		t.Fatalf("the newer catalog was replaced: %q, %v", got, err)
	}
	if idx, err := os.ReadFile(filepath.Join(dir, "index.json")); err != nil || string(idx) != index {
		t.Fatalf("index.json = %q, %v", idx, err)
	}
}

func TestSeedCatalog_RestoresACatalogAnInterruptedSwapMovedAside(t *testing.T) {
	if _, _, err := template.EmbeddedSnapshot(); err != nil {
		t.Fatalf("%v — run `make catalog-snapshot` first", err)
	}
	stateDir := t.TempDir()
	if err := seedCatalog(stateDir); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(stateDir, catalogDirName)
	marker := filepath.Join(dir, "marker")
	if err := os.WriteFile(marker, []byte("the previous copy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, dir+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir+".new", 0o755); err != nil {
		t.Fatal(err)
	}

	if err := seedCatalog(stateDir); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "the previous copy" {
		t.Fatalf("the previous copy was not restored: %q, %v", got, err)
	}
	for _, leftover := range []string{dir + ".old", dir + ".new"} {
		if _, err := os.Stat(leftover); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s was left behind: %v", leftover, err)
		}
	}
}
