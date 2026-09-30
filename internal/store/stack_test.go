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

func TestStackStore_InsertGetListDelete(t *testing.T) {
	ctx := context.Background()
	migrations, err := Load()
	if err != nil {
		t.Fatalf("loading migrations: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "stack-test.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(ctx); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	st := NewStackStore(db)

	at := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	rec := Stack{Name: "nginx", TemplateSource: "catalog", TemplateID: "nginx", TemplateRevision: "3", Compose: "services: {}\n", SealedEnv: []byte{1, 2, 3}, InstalledAt: at}
	if err := st.Insert(ctx, rec); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := st.Insert(ctx, rec); !errors.Is(err, ErrStackExists) {
		t.Fatalf("second Insert = %v, want ErrStackExists", err)
	}

	got, err := st.Get(ctx, "nginx")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "nginx" || got.TemplateRevision != "3" || got.Compose != "services: {}\n" || string(got.SealedEnv) != "\x01\x02\x03" || !got.InstalledAt.Equal(at) {
		t.Fatalf("Get = %+v", got)
	}
	if _, err := st.Get(ctx, "missing"); !errors.Is(err, ErrStackNotFound) {
		t.Fatalf("Get(missing) = %v, want ErrStackNotFound", err)
	}

	rec2 := rec
	rec2.Name = "abc"
	if err := st.Insert(ctx, rec2); err != nil {
		t.Fatalf("Insert abc: %v", err)
	}
	list, err := st.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].Name != "abc" || list[1].Name != "nginx" {
		t.Fatalf("List = %+v, want abc then nginx", list)
	}

	rec3 := rec
	rec3.Name = "empty-env"
	rec3.SealedEnv = nil
	if err := st.Insert(ctx, rec3); err != nil {
		t.Fatalf("Insert with a nil sealed env: %v", err)
	}
	if err := st.Delete(ctx, "empty-env"); err != nil {
		t.Fatalf("Delete empty-env: %v", err)
	}

	if err := st.Delete(ctx, "nginx"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := st.Delete(ctx, "nginx"); !errors.Is(err, ErrStackNotFound) {
		t.Fatalf("second Delete = %v, want ErrStackNotFound", err)
	}
}
