package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestFixtures_UpgradeToHead is the fixture-upgrade harness (doc 06 §2):
// every released schema's fixture database, upgraded through the real
// runner, must come out at head with integrity_check clean and its
// representative data intact.
func TestFixtures_UpgradeToHead(t *testing.T) {
	fixtures, err := filepath.Glob("../../testdata/db/*.db")
	if err != nil {
		t.Fatal(err)
	}
	if len(fixtures) == 0 {
		t.Fatal("no fixtures under testdata/db — this issue's acceptance criteria calls for a first one")
	}

	migrations, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	head := maxVersion(migrations)

	for _, fixture := range fixtures {
		fixture := fixture
		t.Run(filepath.Base(fixture), func(t *testing.T) {
			ctx := context.Background()

			work := filepath.Join(t.TempDir(), "upgrade.db")
			copyFile(t, fixture, work)

			db, err := sql.Open("sqlite", work)
			if err != nil {
				t.Fatal(err)
			}
			defer closeQuietly(db)

			r := &Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
			if _, _, err := r.Apply(ctx); err != nil {
				t.Fatalf("upgrading %s to head: %v", fixture, err)
			}

			version, err := r.CurrentVersion(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if version != head {
				t.Fatalf("CurrentVersion after upgrade = %q, want head (%q)", version, head)
			}
			assertIntegrityOK(t, ctx, db)

			before := readSchemaInfo(t, ctx, fixture)
			after := readSchemaInfo(t, ctx, work)
			if before != after {
				t.Fatalf("schema_info changed across the upgrade — doc 06 §2 requires every row and every value to survive:\nbefore: %+v\nafter:  %+v", before, after)
			}
		})
	}
}

// schemaInfoRow is schema_info's one row, compared value-for-value against
// its own pre-upgrade content — doc 06 §2's "every row and every value
// survives", not merely "some non-empty value is still there".
type schemaInfoRow struct {
	id             int
	installationID string
	createdAt      string
}

func readSchemaInfo(t *testing.T, ctx context.Context, path string) schemaInfoRow {
	t.Helper()
	// mode=ro: path here is sometimes the committed golden fixture itself
	// (testdata/db/*.db) — opening it read-write, even without writing any
	// row, is enough for SQLite to create a -journal/-wal sidecar file
	// right next to a checked-in golden file. mode=ro refuses that.
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro", path))
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(db)

	var row schemaInfoRow
	err = db.QueryRowContext(ctx, "SELECT id, installation_id, created_at FROM schema_info WHERE id = 1").
		Scan(&row.id, &row.installationID, &row.createdAt)
	if err != nil {
		t.Fatalf("reading schema_info from %s: %v", path, err)
	}
	return row
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
