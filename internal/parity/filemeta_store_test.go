package parity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestFileMetaStore_ReplaceKeepsThePreviousRowsWhenProduceFails(t *testing.T) {
	ctx := context.Background()
	s := NewFileMetaStore(newTestDB(t))
	old := FileMeta{Disk: "/mnt/disk1", RelPath: "docs/a.txt", UID: 99, GID: 100, Mode: 0o664}
	if err := s.Replace(ctx, func(emit func(FileMeta) error) error { return emit(old) }); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	boom := errors.New("boom")
	err := s.Replace(ctx, func(emit func(FileMeta) error) error {
		if err := emit(FileMeta{Disk: "/mnt/disk1", RelPath: "docs/b.txt", UID: 1, GID: 1, Mode: 0o600}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Replace = %v, want boom", err)
	}
	if got, ok, err := s.Get(ctx, old.Disk, old.RelPath); err != nil || !ok || got != old {
		t.Fatalf("Get(old) = %+v, %v, %v, want %+v", got, ok, err, old)
	}
	if _, ok, _ := s.Get(ctx, old.Disk, "docs/b.txt"); ok {
		t.Fatal("a row from the failed Replace was committed")
	}
}

func TestFileMetaStore_ReplaceDropsWhatIsNoLongerEmitted(t *testing.T) {
	ctx := context.Background()
	s := NewFileMetaStore(newTestDB(t))
	if recorded, err := s.Recorded(ctx); err != nil || recorded {
		t.Fatalf("Recorded on an empty table = %v, %v", recorded, err)
	}
	a := FileMeta{Disk: "/mnt/disk1", RelPath: "docs", Dir: true, UID: 99, GID: 100, Mode: 0o2775}
	if err := s.Replace(ctx, func(emit func(FileMeta) error) error { return emit(a) }); err != nil {
		t.Fatal(err)
	}
	if recorded, _ := s.Recorded(ctx); !recorded {
		t.Fatal("Recorded = false after a Replace that wrote a row")
	}
	if err := s.Replace(ctx, func(func(FileMeta) error) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get(ctx, a.Disk, a.RelPath); ok {
		t.Fatal("a row survived a Replace that did not emit it")
	}
}

func TestWalkTrackedMetadata_RecordsFilesAndEachDirectoryOnceAndSkipsVanishedFiles(t *testing.T) {
	mount := t.TempDir()
	for _, rel := range []string{"docs/Reports/a.txt", "docs/Reports/b.txt", "top.txt"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(mount, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(mount, rel), nil, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	list := ListReport{
		DataMounts: map[string]string{"d1": mount + "/"},
		Files: []ListFile{
			{Disk: "d1", RelPath: "docs/Reports/a.txt"},
			{Disk: "d1", RelPath: "docs/Reports/b.txt"},
			{Disk: "d1", RelPath: "docs/Reports/gone.txt"},
			{Disk: "d1", RelPath: "top.txt"},
			{Disk: "d9", RelPath: "unmapped.txt"},
		},
	}

	got := map[string]FileMeta{}
	count := 0
	err := walkTrackedMetadata(context.Background(), list, func(m FileMeta) error {
		got[m.RelPath] = m
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("walkTrackedMetadata: %v", err)
	}
	if count != 5 || len(got) != 5 {
		t.Fatalf("emitted %d rows for %d paths, want each of 3 files and 2 directories once: %+v", count, len(got), got)
	}
	for _, rel := range []string{"docs", "docs/Reports"} {
		if m, ok := got[rel]; !ok || !m.Dir || m.Disk != mount {
			t.Errorf("%s = %+v, want a directory row keyed by the cleaned mount", rel, m)
		}
	}
	if m := got["top.txt"]; m.Dir || m.Mode != 0o640 {
		t.Errorf("top.txt = %+v, want a file with mode 0640", m)
	}
}

func TestWalkTrackedMetadata_StopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	list := ListReport{DataMounts: map[string]string{"d1": t.TempDir()}, Files: []ListFile{{Disk: "d1", RelPath: "a"}}}
	if err := walkTrackedMetadata(ctx, list, func(FileMeta) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("walkTrackedMetadata = %v, want context.Canceled", err)
	}
}

func TestFileMetaStore_ReplaceKeepsAnEmptyRecordWhenALaterRewriteFails(t *testing.T) {
	ctx := context.Background()
	s := NewFileMetaStore(newTestDB(t))
	if err := s.Replace(ctx, func(func(FileMeta) error) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if recorded, _ := s.Recorded(ctx); !recorded {
		t.Fatal("a record of an array with no files is not recorded")
	}
	boom := errors.New("boom")
	err := s.Replace(ctx, func(emit func(FileMeta) error) error {
		_ = emit(FileMeta{Disk: "/mnt/disk1", RelPath: "x", UID: 1, GID: 1, Mode: 0o600})
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Replace = %v, want boom", err)
	}
	if _, ok, _ := s.Get(ctx, "/mnt/disk1", "x"); ok {
		t.Fatal("rows of a failed rewrite are visible")
	}
}

func dbFile(t *testing.T, db *sql.DB) string {
	t.Helper()
	var seq int
	var name, file string
	if err := db.QueryRow("PRAGMA database_list").Scan(&seq, &name, &file); err != nil {
		t.Fatal(err)
	}
	return file
}

// Replace's walk must not hold the database's write lock: another writer
// with a short busy timeout gets in while produce is still running, however
// many rows it has emitted, and the rows already written are not yet the
// record.
func TestFileMetaStore_ReplaceHoldsNoWriteLockWhileProducing(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewFileMetaStore(db)
	old := FileMeta{Disk: "/mnt/disk1", RelPath: "old", UID: 99, GID: 100, Mode: 0o664}
	if err := s.Replace(ctx, func(emit func(FileMeta) error) error { return emit(old) }); err != nil {
		t.Fatal(err)
	}
	other, err := sql.Open("sqlite", "file:"+dbFile(t, db)+"?_pragma=busy_timeout(100)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()

	const rows = 3*fileMetaBatch + 5
	err = s.Replace(ctx, func(emit func(FileMeta) error) error {
		for i := 0; i < rows; i++ {
			if err := emit(FileMeta{Disk: "/mnt/disk1", RelPath: fmt.Sprintf("f%05d", i), UID: 99, GID: 100, Mode: 0o664}); err != nil {
				return err
			}
			if i == 2*fileMetaBatch+1 || i == 0 {
				if _, err := other.Exec("INSERT INTO share_usage (share_name, disk_mountpoint, bytes) VALUES (?, '/mnt/disk1', 1)", fmt.Sprintf("s%d", i)); err != nil {
					t.Errorf("another writer was blocked while Replace was producing row %d: %v", i, err)
				}
				if got, ok, _ := s.Get(ctx, old.Disk, old.RelPath); !ok || got != old {
					t.Errorf("while producing row %d the record is %+v, %v, want the previous one", i, got, ok)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if _, ok, _ := s.Get(ctx, old.Disk, old.RelPath); ok {
		t.Fatal("the previous record outlived the Replace that superseded it")
	}
	if _, ok, _ := s.Get(ctx, "/mnt/disk1", fmt.Sprintf("f%05d", rows-1)); !ok {
		t.Fatal("the last row of the new record is missing")
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM file_metadata").Scan(&n); err != nil || n != rows {
		t.Fatalf("file_metadata holds %d rows (%v), want only the new record's %d", n, err, rows)
	}
}

func TestFileMetaStore_ReplaceFailingAfterManyBatchesKeepsThePreviousRecordAndLeavesNoRows(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewFileMetaStore(db)
	old := FileMeta{Disk: "/mnt/disk1", RelPath: "old", UID: 99, GID: 100, Mode: 0o664}
	if err := s.Replace(ctx, func(emit func(FileMeta) error) error { return emit(old) }); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	err := s.Replace(ctx, func(emit func(FileMeta) error) error {
		for i := 0; i < 2*fileMetaBatch+7; i++ {
			if err := emit(FileMeta{Disk: "/mnt/disk1", RelPath: fmt.Sprintf("n%05d", i), UID: 1, GID: 1, Mode: 0o600}); err != nil {
				return err
			}
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Replace = %v, want boom", err)
	}
	if got, ok, _ := s.Get(ctx, old.Disk, old.RelPath); !ok || got != old {
		t.Fatalf("previous record = %+v, %v, want %+v", got, ok, old)
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM file_metadata").Scan(&n); err != nil || n != 1 {
		t.Fatalf("file_metadata holds %d rows (%v) after a failed rewrite, want the previous record's 1", n, err)
	}
}

func TestWalkTrackedMetadata_NeverReadsThroughASymlinkedDirectory(t *testing.T) {
	mount, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(mount, "docs")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(mount, "link.txt")); err != nil {
		t.Fatal(err)
	}
	list := ListReport{
		DataMounts: map[string]string{"d1": mount},
		Files:      []ListFile{{Disk: "d1", RelPath: "docs/secret"}, {Disk: "d1", RelPath: "link.txt"}},
	}
	var got []FileMeta
	if err := walkTrackedMetadata(context.Background(), list, func(m FileMeta) error { got = append(got, m); return nil }); err != nil {
		t.Fatalf("walkTrackedMetadata: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("recorded %+v from paths that are symlinks", got)
	}
}
