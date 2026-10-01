package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	_ "modernc.org/sqlite"
)

func newCatalogSettingsStoreForTest(t *testing.T) (*CatalogSettingsStore, *sql.DB) {
	t.Helper()
	migrations, err := Load()
	if err != nil {
		t.Fatalf("loading migrations: %v", err)
	}
	db, err := sql.Open("sqlite", DSN(filepath.Join(t.TempDir(), "catalog-settings-test.db")))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, _, err := (&Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}).Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	return NewCatalogSettingsStore(db), db
}

func strp(s string) *string { return &s }
func boolp(b bool) *bool    { return &b }

func TestCatalogSettingsStore_DefaultsToDailyAndOnWithNoInstallationRow(t *testing.T) {
	s, db := newCatalogSettingsStoreForTest(t)
	got, err := s.CatalogSettings(context.Background())
	if err != nil || got != DefaultCatalogSettings {
		t.Fatalf("settings = %+v, %v, want %+v", got, err, DefaultCatalogSettings)
	}
	if got.RefreshInterval != "24h" || !got.CheckOnOpen {
		t.Fatalf("defaults = %+v, want 24h and on", got)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM schema_info`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("reading created %d installation rows, %v", n, err)
	}
}

func TestCatalogSettingsStore_ChangesOneFieldAndKeepsTheOther(t *testing.T) {
	ctx := context.Background()
	s, _ := newCatalogSettingsStoreForTest(t)

	got, err := s.UpdateCatalogSettings(ctx, CatalogSettingsUpdate{RefreshInterval: strp(CatalogInterval6h)})
	if err != nil || got.RefreshInterval != "6h" || !got.CheckOnOpen {
		t.Fatalf("interval only = %+v, %v", got, err)
	}
	got, err = s.UpdateCatalogSettings(ctx, CatalogSettingsUpdate{CheckOnOpen: boolp(false)})
	if err != nil || got.RefreshInterval != "6h" || got.CheckOnOpen {
		t.Fatalf("check-on-open only = %+v, %v", got, err)
	}
	got, err = s.UpdateCatalogSettings(ctx, CatalogSettingsUpdate{})
	if err != nil || got.RefreshInterval != "6h" || got.CheckOnOpen {
		t.Fatalf("an empty update = %+v, %v", got, err)
	}
	for _, in := range []string{CatalogIntervalOff, CatalogInterval1h, CatalogInterval12h, CatalogInterval24h} {
		if got, err := s.UpdateCatalogSettings(ctx, CatalogSettingsUpdate{RefreshInterval: strp(in)}); err != nil || got.RefreshInterval != in {
			t.Fatalf("interval %q = %+v, %v", in, got, err)
		}
	}
}

func TestCatalogSettingsStore_RefusesAnUnknownIntervalAndWritesNothing(t *testing.T) {
	ctx := context.Background()
	s, db := newCatalogSettingsStoreForTest(t)
	for _, in := range []string{"", "2h", "48h", "OFF", "1h; DROP TABLE schema_info"} {
		_, err := s.UpdateCatalogSettings(ctx, CatalogSettingsUpdate{RefreshInterval: strp(in), CheckOnOpen: boolp(false)})
		if !errors.Is(err, ErrCatalogInterval) {
			t.Fatalf("interval %q: err = %v, want ErrCatalogInterval", in, err)
		}
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM schema_info`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a refused update wrote %d installation rows, %v", n, err)
	}
	if got, _ := s.CatalogSettings(ctx); got != DefaultCatalogSettings {
		t.Fatalf("a refused update changed the settings: %+v", got)
	}
}

func TestCatalogSettingsStore_AnUpdateLeavesTheInstallationRowItFoundAlone(t *testing.T) {
	ctx := context.Background()
	s, db := newCatalogSettingsStoreForTest(t)
	if _, err := db.Exec(`INSERT INTO schema_info (id, installation_id, created_at, hostname, update_channel) VALUES (1, 'install-1', '2026-10-01T00:00:00Z', 'nas', 'beta')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateCatalogSettings(ctx, CatalogSettingsUpdate{RefreshInterval: strp(CatalogIntervalOff)}); err != nil {
		t.Fatal(err)
	}
	var id, host, channel string
	if err := db.QueryRow(`SELECT installation_id, hostname, update_channel FROM schema_info WHERE id = 1`).Scan(&id, &host, &channel); err != nil {
		t.Fatal(err)
	}
	if id != "install-1" || host != "nas" || channel != "beta" {
		t.Fatalf("row = %q %q %q, want the original installation id, hostname and channel", id, host, channel)
	}
}

func TestCatalogSettingsStore_ConcurrentPartialUpdatesDoNotOverwriteEachOther(t *testing.T) {
	ctx := context.Background()
	s, _ := newCatalogSettingsStoreForTest(t)
	for i := 0; i < 20; i++ {
		if _, err := s.UpdateCatalogSettings(ctx, CatalogSettingsUpdate{RefreshInterval: strp(CatalogInterval24h), CheckOnOpen: boolp(true)}); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := s.UpdateCatalogSettings(ctx, CatalogSettingsUpdate{RefreshInterval: strp(CatalogInterval1h)}); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := s.UpdateCatalogSettings(ctx, CatalogSettingsUpdate{CheckOnOpen: boolp(false)}); err != nil {
				t.Error(err)
			}
		}()
		wg.Wait()
		got, err := s.CatalogSettings(ctx)
		if err != nil || got.RefreshInterval != "1h" || got.CheckOnOpen {
			t.Fatalf("round %d: %+v, %v, want both updates applied", i, got, err)
		}
	}
}

func TestCatalogSettingsColumns_RefuseAValueOutsideTheirChecks(t *testing.T) {
	_, db := newCatalogSettingsStoreForTest(t)
	if _, err := db.Exec(`INSERT INTO schema_info (id, installation_id, created_at, catalog_refresh_interval) VALUES (1, 'i', '2026-10-01T00:00:00Z', '2h')`); err == nil {
		t.Fatal("the database accepted a catalog interval outside the allowed set")
	}
	if _, err := db.Exec(`INSERT INTO schema_info (id, installation_id, created_at, catalog_check_on_open) VALUES (1, 'i', '2026-10-01T00:00:00Z', 2)`); err == nil {
		t.Fatal("the database accepted a check-on-open value other than 0 and 1")
	}
}
