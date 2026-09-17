package transforms_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	sqlitemigrate "github.com/mdg-labs/sqlite-migrate"
	_ "modernc.org/sqlite"

	"github.com/mdg-labs/hoserva/internal/store"
	"github.com/mdg-labs/hoserva/internal/store/transforms"
)

// This file is the worked example doc 01 §4 asks for: "size_mb becomes
// size_bytes" is the design's own illustration of when a transform is
// needed at all — a schema diff can add the size_bytes column, but only a
// transform knows the factor of 1024*1024 to fill it from size_mb.
//
// It is deliberately test-only, per this issue's own scope note: no real
// table in schema.sql has a size_mb column yet (schema.sql's only table,
// schema_info, has none — see internal/store/schema/schema.sql's own
// comment on why), so shipping this migration set to users would be
// exercising the mechanism against a schema change nobody asked for. This
// test builds its own tiny two-version fixture instead, to prove the
// mechanism — a transform bound to a version, run inside that migration's
// transaction, tested against a fixture — end to end.
func TestSizeMBToSizeBytes_Transform(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file::memory:?cache=private")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	// v1: the "old home" of the data.
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE disk (id INTEGER PRIMARY KEY, size_mb INTEGER NOT NULL);
		INSERT INTO disk (id, size_mb) VALUES (1, 4000), (2, 8000000);
	`); err != nil {
		t.Fatalf("seeding the v1 fixture: %v", err)
	}

	// v2 (expand): add the new column, still nullable — the schema diff
	// alone can do this much.
	if _, err := db.ExecContext(ctx, "ALTER TABLE disk ADD COLUMN size_bytes INTEGER"); err != nil {
		t.Fatalf("expand step: %v", err)
	}

	// The transform itself: bound to v2, run in the same transaction as
	// the ALTER TABLE above would be in a real migration.
	transform := transforms.Transform{
		Version: "20260101000002",
		Name:    "backfill disk.size_bytes from disk.size_mb",
		Fn: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "UPDATE disk SET size_bytes = size_mb * 1024 * 1024")
			return err
		},
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := transform.Fn(ctx, tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("running the transform: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	rows, err := db.QueryContext(ctx, "SELECT id, size_mb, size_bytes FROM disk ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	want := map[int64]int64{1: 4000 * 1024 * 1024, 2: 8000000 * 1024 * 1024}
	got := map[int64]int64{}
	for rows.Next() {
		var id, mb, bytes int64
		if err := rows.Scan(&id, &mb, &bytes); err != nil {
			t.Fatal(err)
		}
		got[id] = bytes
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	for id, wantBytes := range want {
		if got[id] != wantBytes {
			t.Errorf("disk %d: size_bytes = %d, want %d", id, got[id], wantBytes)
		}
	}
}

// TestSizeMBToSizeBytes_TransformAgainstFixtures is this issue's own
// acceptance criterion: "a data-transform example is tested against that
// fixture" (doc 06 §2). The test above proves the mechanism in isolation;
// this proves it end to end, through the real Runner, against every
// released fixture database under testdata/db. seed and expand are a
// test-only migration pair layered on top of the real, embedded
// migrations — never written under internal/store/migrations/, so this
// never ships as a schema change nobody asked for (schema.sql's own
// comment on why schema_info is its only table still holds).
func TestSizeMBToSizeBytes_TransformAgainstFixtures(t *testing.T) {
	fixtures, err := filepath.Glob("../../../testdata/db/*.db")
	if err != nil {
		t.Fatal(err)
	}
	if len(fixtures) == 0 {
		t.Fatal("no fixtures under testdata/db")
	}

	migrations, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	var head int64
	for _, m := range migrations {
		n, err := strconv.ParseInt(m.Version, 10, 64)
		if err != nil {
			t.Fatalf("migration %q has a non-numeric version %q: %v", m.Filename, m.Version, err)
		}
		if n > head {
			head = n
		}
	}
	seedVersion := fmt.Sprintf("%014d", head+1)
	expandVersion := fmt.Sprintf("%014d", head+2)

	seedSQL := "CREATE TABLE disk (id INTEGER PRIMARY KEY, size_mb INTEGER NOT NULL);"
	seed := store.Migration{
		Version:  seedVersion,
		Slug:     "test_only_disk_size_mb",
		Filename: seedVersion + "_test_only_disk_size_mb.sql",
		SQL:      seedSQL,
		Checksum: sqlitemigrate.Checksum(seedSQL),
	}
	seedRows := transforms.Transform{
		Version: seedVersion,
		Name:    "test-only: seed disk.size_mb rows",
		Fn: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "INSERT INTO disk (id, size_mb) VALUES (1, 4000), (2, 8000000)")
			return err
		},
	}
	expandSQL := "ALTER TABLE disk ADD COLUMN size_bytes INTEGER;"
	expand := store.Migration{
		Version:  expandVersion,
		Slug:     "test_only_disk_size_bytes",
		Filename: expandVersion + "_test_only_disk_size_bytes.sql",
		SQL:      expandSQL,
		Checksum: sqlitemigrate.Checksum(expandSQL),
	}
	transform := transforms.Transform{
		Version: expandVersion,
		Name:    "backfill disk.size_bytes from disk.size_mb",
		Fn: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "UPDATE disk SET size_bytes = size_mb * 1024 * 1024")
			return err
		},
	}

	ctx := context.Background()
	for _, fixture := range fixtures {
		fixture := fixture
		t.Run(filepath.Base(fixture), func(t *testing.T) {
			data, err := os.ReadFile(fixture)
			if err != nil {
				t.Fatal(err)
			}
			work := filepath.Join(t.TempDir(), "transform.db")
			if err := os.WriteFile(work, data, 0o644); err != nil {
				t.Fatal(err)
			}

			db, err := sql.Open("sqlite", work)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()

			r := &store.Runner{
				DB:          db,
				Migrations:  append(append([]store.Migration{}, migrations...), seed, expand),
				Transforms:  []transforms.Transform{seedRows, transform},
				SnapshotDir: t.TempDir(),
			}
			if _, _, err := r.Apply(ctx); err != nil {
				t.Fatalf("applying the test-only transform migrations to %s: %v", fixture, err)
			}

			rows, err := db.QueryContext(ctx, "SELECT id, size_mb, size_bytes FROM disk ORDER BY id")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = rows.Close() }()

			want := map[int64]int64{1: 4000 * 1024 * 1024, 2: 8000000 * 1024 * 1024}
			got := map[int64]int64{}
			for rows.Next() {
				var id, mb, bytes int64
				if err := rows.Scan(&id, &mb, &bytes); err != nil {
					t.Fatal(err)
				}
				got[id] = bytes
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			for id, wantBytes := range want {
				if got[id] != wantBytes {
					t.Errorf("disk %d: size_bytes = %d, want %d", id, got[id], wantBytes)
				}
			}
		})
	}
}
