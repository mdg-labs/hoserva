package job

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLogStore_WriteAndOpenRoundTrip(t *testing.T) {
	dir := t.TempDir()
	l := NewLogStore(dir)

	w, err := l.Create("job-1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := w.Write([]byte("hello stdout\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r, err := l.Open("job-1")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = r.Close() }()

	// getJobLog serves these bytes as-is (application/gzip) — confirm
	// what's on disk really is a valid gzip stream containing what was
	// written, not a decompressed passthrough.
	gz, err := gzip.NewReader(r)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	got, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("reading decompressed log: %v", err)
	}
	if string(got) != "hello stdout\n" {
		t.Fatalf("log content = %q, want %q", got, "hello stdout\n")
	}
}

func TestLogStore_OpenMissingReturnsErrLogNotFound(t *testing.T) {
	l := NewLogStore(t.TempDir())
	if _, err := l.Open("never-existed"); err != ErrLogNotFound {
		t.Fatalf("Open(missing) = %v, want ErrLogNotFound", err)
	}
}

func writeLogFile(t *testing.T, dir, id string, size int, modTime time.Time) string {
	t.Helper()
	path := filepath.Join(dir, id+".log.gz")
	if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, size), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLogStore_PruneDeletesExpiredLogs(t *testing.T) {
	dir := t.TempDir()
	l := NewLogStore(dir)
	now := time.Now()

	old := writeLogFile(t, dir, "old-job", 10, now.Add(-100*24*time.Hour))
	fresh := writeLogFile(t, dir, "fresh-job", 10, now.Add(-1*time.Hour))

	if err := l.Prune(now, nil); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("expired log %s still exists (err=%v), want deleted", old, err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh log %s was deleted: %v", fresh, err)
	}
}

func TestLogStore_PruneNeverDeletesARunningJobsLog(t *testing.T) {
	dir := t.TempDir()
	l := NewLogStore(dir)
	now := time.Now()

	// Expired by age, and it's the only file over the cap too — every
	// reason Prune has to delete something, except that the job is still
	// running.
	running := writeLogFile(t, dir, "still-running", LogRetentionCap+1, now.Add(-200*24*time.Hour))

	if err := l.Prune(now, map[string]bool{"still-running": true}); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	if _, err := os.Stat(running); err != nil {
		t.Fatalf("Prune deleted a running job's log (Q74 forbids this): %v", err)
	}
}

func TestLogStore_PruneEnforcesSizeCapOldestFirst(t *testing.T) {
	dir := t.TempDir()
	l := NewLogStore(dir)
	now := time.Now()

	const each = LogRetentionCap/2 + 1024
	oldest := writeLogFile(t, dir, "oldest", each, now.Add(-3*time.Hour))
	middle := writeLogFile(t, dir, "middle", each, now.Add(-2*time.Hour))
	newest := writeLogFile(t, dir, "newest", each, now.Add(-1*time.Hour))

	if err := l.Prune(now, nil); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	if _, err := os.Stat(oldest); !os.IsNotExist(err) {
		t.Errorf("oldest log should have been pruned first to bring the directory under the cap, err=%v", err)
	}
	if _, err := os.Stat(newest); err != nil {
		t.Errorf("newest log was pruned, want kept: %v", err)
	}
	_ = middle // may or may not survive depending on exact sizing; not asserted
}

func TestLogStore_PruneNeverTouchesAForeignFile(t *testing.T) {
	dir := t.TempDir()
	l := NewLogStore(dir)
	now := time.Now()

	foreign := filepath.Join(dir, "not-a-job-log.txt")
	if err := os.WriteFile(foreign, []byte("unrelated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(foreign, now.Add(-365*24*time.Hour), now.Add(-365*24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	if err := l.Prune(now, nil); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("Prune deleted a file it never wrote itself: %v", err)
	}
}

func TestLogStore_PruneOnEmptyDirectoryIsANoOp(t *testing.T) {
	l := NewLogStore(filepath.Join(t.TempDir(), "does-not-exist-yet"))
	if err := l.Prune(time.Now(), nil); err != nil {
		t.Fatalf("Prune on a directory that was never created: %v", err)
	}
}
