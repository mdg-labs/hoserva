package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func openCheckDB(t *testing.T, key string) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, s := range []string{
		`CREATE TABLE machine_key_check (id INTEGER PRIMARY KEY, check_value BLOB NOT NULL)`,
		`CREATE TABLE array_disks (role TEXT, role_index INTEGER, fs_uuid TEXT, wwn TEXT, serial TEXT, removal_state TEXT)`,
		`CREATE TABLE relocation_manifest (id INTEGER PRIMARY KEY AUTOINCREMENT, rel_path TEXT, size INTEGER, mtime TEXT, source_disk TEXT, target_disk TEXT)`,
		`CREATE TABLE relocation_removing_disks (mountpoint TEXT PRIMARY KEY)`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if key != "" {
		if _, err := db.Exec(`INSERT INTO machine_key_check VALUES (1, ?)`, []byte(key)); err != nil {
			t.Fatalf("seeding key: %v", err)
		}
	}
	return db, path
}

func addManifest(t *testing.T, db *sql.DB, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := db.Exec(`INSERT INTO relocation_manifest (rel_path, size, mtime, source_disk, target_disk) VALUES (?, 1, 't', '/mnt/a', '/mnt/b')`, p); err != nil {
			t.Fatalf("seeding manifest: %v", err)
		}
	}
}

func TestCheckRestorable_ComparesRelocationManifestsAsMultisets(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name          string
		live, archive []string
		want          []string
	}{
		{"identical", []string{"a", "b", "b", "c"}, []string{"c", "b", "a", "b"}, nil},
		{"both empty", nil, nil, nil},
		{"a repeated entry", []string{"a", "b", "b"}, []string{"a", "b"}, []string{"live relocation manifest has 1 entries"}},
		{"only in the archive", []string{"a"}, []string{"a", "z"}, []string{"archive's relocation manifest has 1 entries", `"z"`}},
		{
			"differences on both sides, interleaved",
			[]string{"a", "c", "e", "g", "h"},
			[]string{"b", "c", "d", "g"},
			[]string{"live relocation manifest has 3 entries", "archive's relocation manifest has 2 entries"},
		},
		{"live empty", nil, []string{"a"}, []string{"archive's relocation manifest has 1 entries"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live, _ := openCheckDB(t, "k")
			arc, arcPath := openCheckDB(t, "k")
			addManifest(t, arc, tc.archive...)
			addManifest(t, live, tc.live...)

			err := CheckRestorable(ctx, live, arcPath)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("CheckRestorable = %v, want nil", err)
				}
				return
			}
			var mismatch *ArrayMismatchError
			if !errors.As(err, &mismatch) {
				t.Fatalf("CheckRestorable = %v, want *ArrayMismatchError", err)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Fatalf("error %q does not contain %q", err, w)
				}
			}
		})
	}
}

func TestCheckRestorable_SamplesAtMostAFewEntries(t *testing.T) {
	live, _ := openCheckDB(t, "k")
	_, arcPath := openCheckDB(t, "k")
	for i := 0; i < 50; i++ {
		addManifest(t, live, fmt.Sprintf("f%02d", i))
	}
	err := CheckRestorable(context.Background(), live, arcPath)
	if err == nil || !strings.Contains(err.Error(), "50 entries") {
		t.Fatalf("CheckRestorable = %v, want the count of 50", err)
	}
	if strings.Count(err.Error(), `"f`) != manifestSampleSize {
		t.Fatalf("error %q names %d entries, want %d", err, strings.Count(err.Error(), `"f`), manifestSampleSize)
	}
}

func TestCheckRestorable_InstallationIsComparedByCheckValue(t *testing.T) {
	ctx := context.Background()
	live, _ := openCheckDB(t, "k")
	_, same := openCheckDB(t, "k")
	_, other := openCheckDB(t, "other")
	_, none := openCheckDB(t, "")

	if err := CheckRestorable(ctx, live, same); err != nil {
		t.Fatalf("same installation: %v", err)
	}
	for name, p := range map[string]string{"different check value": other, "no check value": none} {
		if err := CheckRestorable(ctx, live, p); !errors.Is(err, ErrArchiveOtherInstallation) {
			t.Fatalf("%s: CheckRestorable = %v, want ErrArchiveOtherInstallation", name, err)
		}
	}
}

// A database that cannot be read is not the same as one that matches.
func TestCheckRestorable_UnreadableDatabasesRefuse(t *testing.T) {
	ctx := context.Background()
	live, _ := openCheckDB(t, "k")
	if err := CheckRestorable(ctx, live, filepath.Join(t.TempDir(), "missing.db")); err == nil {
		t.Fatal("CheckRestorable with a missing archive database = nil, want an error")
	}

	noKeyLive, _ := openCheckDB(t, "")
	_, arc := openCheckDB(t, "k")
	if err := CheckRestorable(ctx, noKeyLive, arc); err == nil || errors.Is(err, ErrArchiveOtherInstallation) {
		t.Fatalf("CheckRestorable with no live check value = %v, want a plain error", err)
	}

	broken, _ := openCheckDB(t, "k")
	if _, err := broken.Exec(`DROP TABLE array_disks`); err != nil {
		t.Fatalf("dropping table: %v", err)
	}
	_, arc2 := openCheckDB(t, "k")
	if err := CheckRestorable(ctx, broken, arc2); err == nil {
		t.Fatal("CheckRestorable with an unreadable live array_disks = nil, want an error")
	}
}

func TestCheckRestorable_IgnoresDeviceNamesAndOtherDiskColumns(t *testing.T) {
	ctx := context.Background()
	live, _ := openCheckDB(t, "k")
	arc, arcPath := openCheckDB(t, "k")
	for _, db := range []*sql.DB{live, arc} {
		if _, err := db.Exec(`INSERT INTO array_disks VALUES ('data', 1, 'u1', 'w1', 's1', NULL)`); err != nil {
			t.Fatalf("seeding disk: %v", err)
		}
	}
	if err := CheckRestorable(ctx, live, arcPath); err != nil {
		t.Fatalf("identical disks: %v", err)
	}
	if _, err := arc.Exec(`UPDATE array_disks SET serial = 's2', removal_state = 'evacuating'`); err != nil {
		t.Fatalf("editing archive disk: %v", err)
	}
	err := CheckRestorable(ctx, live, arcPath)
	var mismatch *ArrayMismatchError
	if !errors.As(err, &mismatch) || len(mismatch.Differences) != 2 {
		t.Fatalf("CheckRestorable = %v, want a mismatch naming the serial and the removal_state", err)
	}
}
