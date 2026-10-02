package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

func tableRowCounts(t *testing.T, ctx context.Context, db *sql.DB) map[string]int {
	t.Helper()
	rows, err := db.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name != 'schema_migrations'")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	counts := make(map[string]int, len(names))
	for _, n := range names {
		var c int
		if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM %q", n)).Scan(&c); err != nil {
			t.Fatalf("counting %s: %v", n, err)
		}
		counts[n] = c
	}
	return counts
}

// The catalog sources migration only adds a table: upgrading every released
// fixture database keeps every row of every table it already had, and leaves
// the new table empty (the curated row is written by hoservad at startup).
func TestFixtures_CatalogSourcesMigrationKeepsEveryRow(t *testing.T) {
	ctx := context.Background()
	fixtures, err := filepath.Glob("../../testdata/db/*.db")
	if err != nil || len(fixtures) == 0 {
		t.Fatalf("no fixtures under testdata/db (err %v)", err)
	}
	migrations, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		t.Run(filepath.Base(fixture), func(t *testing.T) {
			ro, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro", fixture))
			if err != nil {
				t.Fatal(err)
			}
			defer closeQuietly(ro)
			before := tableRowCounts(t, ctx, ro)

			work := filepath.Join(t.TempDir(), "upgrade.db")
			copyFile(t, fixture, work)
			db, err := sql.Open("sqlite", work)
			if err != nil {
				t.Fatal(err)
			}
			defer closeQuietly(db)
			r := &Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
			if _, _, err := r.Apply(ctx); err != nil {
				t.Fatalf("upgrading %s: %v", fixture, err)
			}
			after := tableRowCounts(t, ctx, db)
			for table, n := range before {
				if after[table] != n {
					t.Errorf("table %s had %d rows before the upgrade and %d after", table, n, after[table])
				}
			}
			if n, ok := after["catalog_sources"]; !ok || n != 0 {
				t.Errorf("catalog_sources after the upgrade: present=%v rows=%d, want present and empty", ok, n)
			}
		})
	}
}
