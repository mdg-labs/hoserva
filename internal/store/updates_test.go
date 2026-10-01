package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newUpdateStoreForTest(t *testing.T) *UpdateStore {
	t.Helper()
	migrations, err := Load()
	if err != nil {
		t.Fatalf("loading migrations: %v", err)
	}
	db, err := sql.Open("sqlite", DSN(filepath.Join(t.TempDir(), "updates-test.db")))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, _, err := (&Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}).Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	return NewUpdateStore(db)
}

func TestUpdateStore_ImageUpdateChecks(t *testing.T) {
	ctx := context.Background()
	s := newUpdateStoreForTest(t)
	at := time.Date(2026, 9, 30, 6, 10, 0, 0, time.UTC)

	for _, c := range []ImageUpdateCheck{
		{Image: "postgres:16.4", CheckedAt: at, Status: UpdateAvailable, Kind: UpdateKindNewVersion, AvailableTag: "16.6"},
		{Image: "nginx:latest", CheckedAt: at, Status: UpdateUpToDate},
		{Image: "acme/private:latest", CheckedAt: at, Status: UpdateNotChecked, Message: "the registry wants a login"},
		{Image: "nginx:latest", CheckedAt: at.Add(time.Hour), Status: UpdateSkipped, Message: "rate limited"},
	} {
		if err := s.PutImageUpdateCheck(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListImageUpdateChecks(ctx)
	if err != nil || len(got) != 3 || got[0].Image != "acme/private:latest" || got[0].Status != UpdateNotChecked || got[1].Image != "nginx:latest" || got[1].Status != UpdateSkipped || got[1].Message != "rate limited" || !got[1].CheckedAt.Equal(at.Add(time.Hour)) || got[2].AvailableTag != "16.6" {
		t.Fatalf("List = %+v, %v, want the skipped result to have replaced the up_to_date one", got, err)
	}
	if err := s.DeleteImageUpdateCheck(ctx, "nginx:latest"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ListImageUpdateChecks(ctx); len(got) != 2 {
		t.Fatalf("after Delete = %+v", got)
	}
	if err := s.PutImageUpdateCheck(ctx, ImageUpdateCheck{Image: "x:1", CheckedAt: at, Status: "maybe"}); err == nil {
		t.Fatal("a status outside the schema's CHECK was accepted")
	}
}
