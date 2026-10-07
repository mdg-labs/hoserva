package main

import (
	"context"
	"database/sql"
	"encoding/base64"
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

const laravelKeyTemplate = `services:
  app:
    image: example/app:1
    environment:
      APP_KEY: ${APP_KEY}
x-hoserva:
  schema: 1
  id: laravel-app
  revision: 1
  title: Laravel app
  categories: [system]
  icon: icon.svg
  docs: https://example.com
  inputs:
    APP_KEY: { kind: secret, format: laravel-key }
`

// TestTemplateInstallWiring_ALaravelKeySecretIsGeneratedInTheStacksEnv builds
// the install services the way main.go does and installs a catalog template
// whose secret asks for the laravel-key format: the stack's .env holds
// base64: followed by 32 random bytes, a typed key of another length is
// refused with 400 invalid_template_input, and a valid typed key is kept.
func TestTemplateInstallWiring_ALaravelKeySecretIsGeneratedInTheStacksEnv(t *testing.T) {
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
	catalog := filepath.Join(stateDir, catalogDirName, "laravel-app")
	if err := os.MkdirAll(catalog, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(catalog, template.ComposeFile), []byte(laravelKeyTemplate), 0o644); err != nil {
		t.Fatal(err)
	}

	apps := &appServices{Lifecycle: &container.Lifecycle{Provider: container.NewFakeProvider()}}
	h := &api.Handler{}
	wireStacks(h, job.NewRegistry(), store.NewStackStore(db), machineKey, &resolvingRunner{}, stateDir, apps, nil)
	wireTemplateInstall(h, stateDir, apps, func(context.Context) ([]string, error) { return nil, nil })
	if h.TemplateInstall == nil {
		t.Fatal("wireTemplateInstall left Handler.TemplateInstall nil, so every /templates operation would 501")
	}
	appKey := func(name string) string {
		env, err := os.ReadFile(filepath.Join(stateDir, "stacks", name, ".env"))
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range strings.Split(string(env), "\n") {
			if v, ok := strings.CutPrefix(l, "APP_KEY="); ok {
				return v
			}
		}
		t.Fatalf(".env = %q has no APP_KEY", env)
		return ""
	}

	if _, err := h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{}, apiv1.InstallTemplateParams{ID: "laravel-app"}); err != nil {
		t.Fatalf("InstallTemplate: %v", err)
	}
	rest, ok := strings.CutPrefix(appKey("laravel-app"), "base64:")
	raw, err := base64.StdEncoding.DecodeString(rest)
	if !ok || err != nil || len(raw) != 32 {
		t.Errorf("generated APP_KEY = %q, want base64: and 32 random bytes", appKey("laravel-app"))
	}

	short := "base64:" + base64.StdEncoding.EncodeToString(make([]byte, 16))
	_, err = h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{Name: apiv1.NewOptString("short"), Values: apiv1.NewOptTemplateInstallRequestValues(apiv1.TemplateInstallRequestValues{"APP_KEY": short})}, apiv1.InstallTemplateParams{ID: "laravel-app"})
	if st := h.NewError(ctx, err); st.StatusCode != 400 || st.Response.Code != "invalid_template_input" {
		t.Fatalf("a 16-byte key: %d %q, want 400 invalid_template_input", st.StatusCode, st.Response.Code)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "stacks", "short")); err == nil {
		t.Error("an install with a refused key was written anyway")
	}

	typed := "base64:" + base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	if _, err := h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{Name: apiv1.NewOptString("typed"), Values: apiv1.NewOptTemplateInstallRequestValues(apiv1.TemplateInstallRequestValues{"APP_KEY": typed})}, apiv1.InstallTemplateParams{ID: "laravel-app"}); err != nil {
		t.Fatalf("a valid typed key: %v", err)
	}
	if got := appKey("typed"); got != typed && got != "'"+typed+"'" {
		t.Errorf("typed APP_KEY = %q, want %q kept", got, typed)
	}
}
