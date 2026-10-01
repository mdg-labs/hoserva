package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newImageHistoryStoreForTest(t *testing.T) *ImageHistoryStore {
	t.Helper()
	migrations, err := Load()
	if err != nil {
		t.Fatalf("loading migrations: %v", err)
	}
	db, err := sql.Open("sqlite", DSN(filepath.Join(t.TempDir(), "image-history-test.db")))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, _, err := (&Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}).Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	return NewImageHistoryStore(db)
}

func TestImageHistoryStore_RecordsAndRevertsAnUpdate(t *testing.T) {
	ctx := context.Background()
	s := newImageHistoryStoreForTest(t)
	at := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)

	if _, ok, err := s.LatestImageHistory(ctx, "jellyfin"); err != nil || ok {
		t.Fatalf("Latest with no rows = %v, %v, want none", ok, err)
	}
	first, err := s.InsertImageHistory(ctx, ImageHistory{Container: "jellyfin", Image: "jellyfin:10", PreviousImageID: "sha256:a", UpdatedAt: at, KeepUntil: at.Add(24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.InsertImageHistory(ctx, ImageHistory{
		Container: "jellyfin", Image: "jellyfin:10", PreviousImageID: "sha256:b", SnapshotArchive: "hoserva-appdata-x.tar.zst", SnapshotDestination: "pool",
		UpdatedAt: at.Add(time.Hour), KeepUntil: at.Add(25 * time.Hour),
	})
	if err != nil || second.ID <= first.ID {
		t.Fatalf("second = %+v, %v, want an id after %d", second, err, first.ID)
	}
	if _, err := s.InsertImageHistory(ctx, ImageHistory{Container: "postgres", Image: "postgres:16", PreviousImageID: "sha256:c", UpdatedAt: at, KeepUntil: at}); err != nil {
		t.Fatal(err)
	}

	latest, ok, err := s.LatestImageHistory(ctx, "jellyfin")
	if err != nil || !ok || !reflect.DeepEqual(latest, second) {
		t.Fatalf("Latest = %+v, %v, %v, want %+v", latest, ok, err, second)
	}
	all, err := s.ListImageHistory(ctx)
	if err != nil || len(all) != 3 || all[0].Container != "postgres" || all[2].ID != first.ID {
		t.Fatalf("List = %+v, %v, want three rows newest first", all, err)
	}

	if err := s.MarkImageHistoryReverted(ctx, second.ID, at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkImageHistoryReverted(ctx, second.ID, at.Add(3*time.Hour)); !errors.Is(err, ErrImageHistoryNotFound) {
		t.Fatalf("a second revert mark = %v, want ErrImageHistoryNotFound", err)
	}
	latest, _, _ = s.LatestImageHistory(ctx, "jellyfin")
	if !latest.RevertedAt.Equal(at.Add(2 * time.Hour)) {
		t.Fatalf("RevertedAt = %v, want the first mark", latest.RevertedAt)
	}

	if err := s.DeleteImageHistory(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteImageHistory(ctx, second.ID); err != nil {
		t.Fatalf("deleting a missing row: %v", err)
	}
	if latest, _, _ := s.LatestImageHistory(ctx, "jellyfin"); latest.ID != first.ID {
		t.Fatalf("Latest after deleting the newest = %+v, want the first", latest)
	}
}

func TestImageHistoryStore_BulkExclusion(t *testing.T) {
	ctx := context.Background()
	s := newImageHistoryStoreForTest(t)
	if got, err := s.BulkExcluded(ctx); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("BulkExcluded = %v, %v, want an empty list", got, err)
	}
	for name, excluded := range map[string]bool{"postgres": true, "jellyfin": true, "nginx": true} {
		if err := s.SetBulkExcluded(ctx, name, excluded); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetBulkExcluded(ctx, "nginx", false); err != nil {
		t.Fatal(err)
	}
	if got, err := s.BulkExcluded(ctx); err != nil || !reflect.DeepEqual(got, []string{"jellyfin", "postgres"}) {
		t.Fatalf("BulkExcluded = %v, %v, want jellyfin and postgres", got, err)
	}
}

func TestImageHistoryStore_KeepDays(t *testing.T) {
	ctx := context.Background()
	s := newImageHistoryStoreForTest(t)
	if got, err := s.ImageKeepDays(ctx); err != nil || got != DefaultImageKeepDays {
		t.Fatalf("ImageKeepDays with nothing stored = %d, %v, want the default", got, err)
	}
	if err := s.SetImageKeepDays(ctx, 30); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ImageKeepDays(ctx); got != 30 {
		t.Fatalf("ImageKeepDays = %d, want 30", got)
	}
	for _, days := range []int{0, -1, MaxImageKeepDays + 1} {
		if err := s.SetImageKeepDays(ctx, days); !errors.Is(err, ErrImageKeepDays) {
			t.Errorf("SetImageKeepDays(%d) = %v, want ErrImageKeepDays", days, err)
		}
	}
	if got, _ := s.ImageKeepDays(ctx); got != 30 {
		t.Fatalf("a refused period changed the stored one to %d", got)
	}
}
