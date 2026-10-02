package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newCatalogSourceTestStore(t *testing.T) (*CatalogSourceStore, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	migrations, err := Load()
	if err != nil {
		t.Fatalf("loading migrations: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "sources.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(ctx); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	return NewCatalogSourceStore(db), db
}

func TestCatalogSourceStore_CuratedRowIsEnsuredOnceAndNeverRemoved(t *testing.T) {
	ctx := context.Background()
	st, _ := newCatalogSourceTestStore(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	if err := st.EnsureCurated(ctx, "hoserva", "https://catalog.hoserva.dev", at); err != nil {
		t.Fatalf("EnsureCurated: %v", err)
	}
	if err := st.RecordRefresh(ctx, "hoserva", true, at.Add(time.Hour)); err != nil {
		t.Fatalf("RecordRefresh: %v", err)
	}
	if err := st.EnsureCurated(ctx, "hoserva", "https://catalog.hoserva.dev", at.Add(2*time.Hour)); err != nil {
		t.Fatalf("second EnsureCurated: %v", err)
	}
	got, err := st.Get(ctx, "hoserva")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Kind != CatalogSourceCurated || !got.SignatureVerified || !got.AddedAt.Equal(at) || !got.LastRefreshedAt.Equal(at.Add(time.Hour)) {
		t.Fatalf("the second EnsureCurated changed the row: %+v", got)
	}

	if err := st.DeleteUserAdded(ctx, "hoserva"); !errors.Is(err, ErrCatalogSourceCurated) {
		t.Fatalf("DeleteUserAdded(curated) = %v, want ErrCatalogSourceCurated", err)
	}
	if _, err := st.Get(ctx, "hoserva"); err != nil {
		t.Fatalf("the curated row is gone after a refused delete: %v", err)
	}
}

func TestCatalogSourceStore_DeleteRemovesOnlyThatSource(t *testing.T) {
	ctx := context.Background()
	st, _ := newCatalogSourceTestStore(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if err := st.EnsureCurated(ctx, "hoserva", "https://catalog.hoserva.dev", at); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"src-aaaaaaaaaa", "src-bbbbbbbbbb", "src-cccccccccc"} {
		err := st.Insert(ctx, CatalogSource{ID: id, URL: "https://example.com/" + id, Kind: CatalogSourceUserAdded, AddedAt: at.Add(time.Duration(i+1) * time.Minute)})
		if err != nil {
			t.Fatalf("Insert %s: %v", id, err)
		}
	}

	if err := st.DeleteUserAdded(ctx, "src-bbbbbbbbbb"); err != nil {
		t.Fatalf("DeleteUserAdded: %v", err)
	}
	list, err := st.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range list {
		ids = append(ids, s.ID)
	}
	want := []string{"hoserva", "src-aaaaaaaaaa", "src-cccccccccc"}
	if len(ids) != len(want) || ids[0] != want[0] || ids[1] != want[1] || ids[2] != want[2] {
		t.Fatalf("List after deleting one source = %v, want %v", ids, want)
	}
	if err := st.DeleteUserAdded(ctx, "src-bbbbbbbbbb"); !errors.Is(err, ErrCatalogSourceNotFound) {
		t.Fatalf("second DeleteUserAdded = %v, want ErrCatalogSourceNotFound", err)
	}
}

func TestCatalogSourceStore_InsertRefusesADuplicateURLOrID(t *testing.T) {
	ctx := context.Background()
	st, _ := newCatalogSourceTestStore(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	src := CatalogSource{ID: "src-aaaaaaaaaa", URL: "https://example.com/c", Kind: CatalogSourceUserAdded, AddedAt: at}
	if err := st.Insert(ctx, src); err != nil {
		t.Fatal(err)
	}
	same := src
	same.ID = "src-bbbbbbbbbb"
	if err := st.Insert(ctx, same); !errors.Is(err, ErrCatalogSourceExists) {
		t.Fatalf("Insert with a used URL = %v, want ErrCatalogSourceExists", err)
	}
	same = src
	same.URL = "https://example.com/other"
	if err := st.Insert(ctx, same); !errors.Is(err, ErrCatalogSourceExists) {
		t.Fatalf("Insert with a used id = %v, want ErrCatalogSourceExists", err)
	}
}

func TestCatalogSourceStore_AnUnsignedSourceCanNeverBeMarkedVerified(t *testing.T) {
	ctx := context.Background()
	st, _ := newCatalogSourceTestStore(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	err := st.Insert(ctx, CatalogSource{ID: "src-aaaaaaaaaa", URL: "https://example.com/a", Kind: CatalogSourceUserAdded, SignatureVerified: true, AddedAt: at})
	if err == nil {
		t.Fatal("Insert accepted a user-added source with no public key marked signature-verified")
	}

	if err := st.Insert(ctx, CatalogSource{ID: "src-bbbbbbbbbb", URL: "https://example.com/b", Kind: CatalogSourceUserAdded, AddedAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordRefresh(ctx, "src-bbbbbbbbbb", true, at); err == nil {
		t.Fatal("RecordRefresh marked an unsigned user-added source signature-verified")
	}
	got, err := st.Get(ctx, "src-bbbbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	if got.SignatureVerified {
		t.Fatalf("an unsigned source reads back as signature-verified: %+v", got)
	}

	if err := st.Insert(ctx, CatalogSource{ID: "src-cccccccccc", URL: "https://example.com/c", Kind: CatalogSourceUserAdded, PublicKey: "a2V5", SignatureVerified: true, AddedAt: at}); err != nil {
		t.Fatalf("a keyed source marked verified was refused: %v", err)
	}
}

func TestCatalogSourceStore_RecordRefreshKeepsEveryOtherColumn(t *testing.T) {
	ctx := context.Background()
	st, _ := newCatalogSourceTestStore(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	src := CatalogSource{ID: "src-aaaaaaaaaa", URL: "https://example.com/a", Kind: CatalogSourceUserAdded, PublicKey: "a2V5", SignatureVerified: true, AddedAt: at}
	if err := st.Insert(ctx, src); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordRefresh(ctx, "src-aaaaaaaaaa", true, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(ctx, "src-aaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != src.URL || got.PublicKey != "a2V5" || !got.AddedAt.Equal(at) || !got.LastRefreshedAt.Equal(at.Add(time.Hour)) || !got.SignatureVerified {
		t.Fatalf("RecordRefresh changed more than the refresh time: %+v", got)
	}
	if err := st.RecordRefresh(ctx, "src-missing0000", true, at); !errors.Is(err, ErrCatalogSourceNotFound) {
		t.Fatalf("RecordRefresh of a missing source = %v, want ErrCatalogSourceNotFound", err)
	}
}
