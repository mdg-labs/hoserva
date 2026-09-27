package main

import (
	"context"
	"database/sql"
	"io"
	"path/filepath"
	"testing"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// TestWireBackup_HandlerServesExportConfigInsteadOf501 reproduces #269's
// first finding: before this fix, main.go's handler.* assignment block
// never set Backup, so ExportConfig/ImportConfig (POST /config/export,
// POST /config/import) 501'd unconditionally regardless of array or
// onboarding state. This builds the handler through wireBackup — the
// exact function main.go calls, never a hand copy of the assignment —
// and confirms a real ExportConfig call reaches backup.Service instead
// of the 501 a nil Handler.Backup would still produce.
func TestWireBackup_HandlerServesExportConfigInsteadOf501(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	dbPath := filepath.Join(dir, "hoservad.db")
	db, err := sql.Open("sqlite", store.DSN(dbPath))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	defer func() { _ = db.Close() }()
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(ctx); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}

	backupService := &backup.Service{
		DB:    db,
		Paths: backup.Paths{DBPath: dbPath},
	}

	h := &api.Handler{}
	wireBackup(h, backupService)

	got, err := h.ExportConfig(ctx)
	if err != nil {
		t.Fatalf("ExportConfig: %v", err)
	}
	data, err := io.ReadAll(got.Data)
	if err != nil {
		t.Fatalf("reading exported archive: %v", err)
	}
	if c, ok := got.Data.(io.Closer); ok {
		_ = c.Close()
	}
	if len(data) == 0 {
		t.Fatal("ExportConfig returned an empty archive")
	}
}
