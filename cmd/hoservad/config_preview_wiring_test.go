package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/auth"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/notify"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// TestConfigImportPreview_ServedByTheDaemonsHandler builds the handler the
// way main.go does for config export and import (wireBackup, Scheduler) and
// drives POST /config/export and POST /config/import/preview through
// buildUnixServer's generated API server over a real Unix socket, so the
// preview is reachable the way the CLI reaches it and is neither a 404 nor
// a 501.
func TestConfigImportPreview_ServedByTheDaemonsHandler(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	dbPath := filepath.Join(root, "hoservad.db")
	db, err := sql.Open("sqlite", store.DSN(dbPath))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	defer func() { _ = db.Close() }()
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(ctx); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}

	authStore := api.NewAuthStore(db)
	machineKey, err := auth.LoadOrGenerateMachineKey(ctx, filepath.Join(root, "secret.key"), authStore)
	if err != nil {
		t.Fatalf("machine key: %v", err)
	}
	authService := api.NewAuthService(authStore, machineKey)
	if _, _, err := authService.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	handler := &api.Handler{
		Scheduler: job.NewScheduler(job.NewStore(db), job.NewLogStore(t.TempDir()), job.NewHub(), job.NewRegistry()),
	}
	wireBackup(handler, &backup.Service{DB: db, Paths: backup.Paths{DBPath: dbPath}})

	unixServer, err := buildUnixServer(handler, authStore, job.NewHub(), notify.NewHub())
	if err != nil {
		t.Fatalf("buildUnixServer: %v", err)
	}
	sockPath := filepath.Join(root, "hoserva.sock")
	ln, err := setupUnixListener(sockPath)
	if err != nil {
		t.Fatalf("setupUnixListener: %v", err)
	}
	go func() { _ = unixServer.Serve(ln) }()
	t.Cleanup(func() { _ = unixServer.Close() })

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sockPath)
		},
	}}
	const base = "http://unix" + apiPathPrefix

	exported, err := client.Post(base+"/config/export", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /config/export: %v", err)
	}
	archive, _ := io.ReadAll(exported.Body)
	_ = exported.Body.Close()
	if exported.StatusCode != http.StatusOK {
		t.Fatalf("POST /config/export status = %d: %s", exported.StatusCode, archive)
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("archive", "hoserva-config.tar.zst")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(archive)
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	resp, err := client.Post(base+"/config/import/preview", mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatalf("POST /config/import/preview: %v", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /config/import/preview status = %d, want 200 (501 means the handler is not wired): %s", resp.StatusCode, respBody)
	}
	var preview struct {
		Blockers []json.RawMessage `json:"blockers"`
		Groups   []struct {
			Category string `json:"category"`
		} `json:"groups"`
		LiveSchemaVersion string `json:"liveSchemaVersion"`
	}
	if err := json.Unmarshal(respBody, &preview); err != nil {
		t.Fatalf("decoding the preview: %v\n%s", err, respBody)
	}
	if len(preview.Blockers) != 0 || len(preview.Groups) != 6 || preview.LiveSchemaVersion == "" {
		t.Fatalf("preview of the daemon's own export = %s, want no blockers, six categories and a schema version", respBody)
	}
}
