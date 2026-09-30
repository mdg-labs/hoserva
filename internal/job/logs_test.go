package job

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
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

func readGzipPrefix(t *testing.T, r io.Reader) string {
	t.Helper()
	gz, err := gzip.NewReader(r)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	got, err := io.ReadAll(gz)
	if err != nil && err != io.ErrUnexpectedEOF {
		t.Fatalf("reading decompressed log: %v", err)
	}
	return string(got)
}

func TestLogStore_OpenSeesEveryWriteBeforeClose(t *testing.T) {
	l := NewLogStore(t.TempDir())

	w, err := l.Create("running")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = w.Close() }()

	var want string
	for _, line := range []string{"first line\n", "second line\n", "third line\n"} {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatalf("Write: %v", err)
		}
		want += line

		r, err := l.Open("running")
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		got := readGzipPrefix(t, r)
		_ = r.Close()
		if got != want {
			t.Fatalf("log of a still-running job = %q, want %q", got, want)
		}
	}
}

func TestLogStore_CloseAfterFlushedWritesLeavesCompleteGzip(t *testing.T) {
	l := NewLogStore(t.TempDir())

	w, err := l.Create("finished")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, line := range []string{"a\n", "b\n"} {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r, err := l.Open("finished")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = r.Close() }()
	gz, err := gzip.NewReader(r)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	got, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("a closed log must be a complete gzip stream, got: %v", err)
	}
	if string(got) != "a\nb\n" {
		t.Fatalf("log content = %q, want %q", got, "a\nb\n")
	}
}

type followFlag struct{ done atomic.Bool }

func (f *followFlag) finished(context.Context) (bool, error) { return f.done.Load(), nil }

func readWithin(t *testing.T, r io.Reader, n int) string {
	t.Helper()
	got := make(chan string, 1)
	go func() {
		buf := make([]byte, n)
		_, _ = io.ReadFull(r, buf)
		got <- string(buf)
	}()
	select {
	case s := <-got:
		return s
	case <-time.After(10 * time.Second):
		t.Fatal("no output arrived while the job was still running")
		return ""
	}
}

func TestLogStore_FollowDeliversOutputWhileTheJobRunsAndEndsWithTheTrailer(t *testing.T) {
	l := NewLogStore(t.TempDir())
	w, err := l.Create("j")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var flag followFlag
	f := l.Follow(context.Background(), "j", flag.finished, 5*time.Millisecond)
	defer func() { _ = f.Close() }()
	if _, err := io.WriteString(w, "one\n"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip.NewReader on a running job's log: %v", err)
	}
	if got := readWithin(t, gz, 4); got != "one\n" {
		t.Fatalf("first chunk = %q, want %q", got, "one\n")
	}
	if _, err := io.WriteString(w, "two\n"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := readWithin(t, gz, 4); got != "two\n" {
		t.Fatalf("second chunk = %q, want %q", got, "two\n")
	}
	if _, err := io.WriteString(w, "three\n"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	flag.done.Store(true)
	rest, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("a finished job's followed log must end with a clean gzip trailer: %v", err)
	}
	if string(rest) != "three\n" {
		t.Fatalf("rest = %q, want %q", rest, "three\n")
	}
}

func TestLogStore_FollowWaitsForALogThatDoesNotExistYet(t *testing.T) {
	l := NewLogStore(t.TempDir())
	var flag followFlag
	f := l.Follow(context.Background(), "queued", flag.finished, 5*time.Millisecond)
	defer func() { _ = f.Close() }()

	got := make(chan []byte, 1)
	go func() {
		gz, err := gzip.NewReader(f)
		if err != nil {
			got <- nil
			return
		}
		b, _ := io.ReadAll(gz)
		got <- b
	}()
	time.Sleep(50 * time.Millisecond)
	w, err := l.Create("queued")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := io.WriteString(w, "late\n"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	flag.done.Store(true)
	select {
	case b := <-got:
		if string(b) != "late\n" {
			t.Fatalf("output = %q, want %q", b, "late\n")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the follower never ended")
	}
}

func TestLogStore_FollowOfAFinishedJobWithNoLogIsEmpty(t *testing.T) {
	l := NewLogStore(t.TempDir())
	var flag followFlag
	flag.done.Store(true)
	f := l.Follow(context.Background(), "gone", flag.finished, 5*time.Millisecond)
	defer func() { _ = f.Close() }()
	if b, err := io.ReadAll(f); err != nil || len(b) != 0 {
		t.Fatalf("ReadAll = %q, %v; want empty and nil", b, err)
	}
}

func TestLogStore_FollowEndsWhenTheContextEnds(t *testing.T) {
	l := NewLogStore(t.TempDir())
	w, err := l.Create("j")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = w.Close() }()
	var flag followFlag
	ctx, cancel := context.WithCancel(context.Background())
	f := l.Follow(ctx, "j", flag.finished, 5*time.Millisecond)
	defer func() { _ = f.Close() }()

	errc := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(f)
		errc <- err
	}()
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Read after cancel = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a cancelled follower kept waiting")
	}
}

func TestLogStore_FollowSurfacesAFinishedCheckError(t *testing.T) {
	l := NewLogStore(t.TempDir())
	boom := errors.New("store unavailable")
	f := l.Follow(context.Background(), "j", func(context.Context) (bool, error) { return false, boom }, 5*time.Millisecond)
	defer func() { _ = f.Close() }()
	if _, err := io.ReadAll(f); !errors.Is(err, boom) {
		t.Fatalf("Read = %v, want the finished-check error", err)
	}
}

func writeLogRun(t *testing.T, l *LogStore, id, text string, closeLog bool) {
	t.Helper()
	w, err := l.Create(id)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := io.WriteString(w, text); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if closeLog {
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		return
	}
	if err := w.(*gzipWriteCloser).f.Close(); err != nil {
		t.Fatalf("closing the file without a gzip trailer: %v", err)
	}
}

func readWholeLog(t *testing.T, l *LogStore, id string) string {
	t.Helper()
	r, err := l.Open(id)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = r.Close() }()
	gz, err := gzip.NewReader(r)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	got, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("a resumed job's log must be one complete gzip stream: %v", err)
	}
	return string(got)
}

func TestLogStore_CreateOfAnExistingLogKeepsEarlierOutput(t *testing.T) {
	l := NewLogStore(t.TempDir())
	writeLogRun(t, l, "j", "run one\n", true)
	writeLogRun(t, l, "j", "run two\n", true)
	writeLogRun(t, l, "j", "run three\n", true)
	if got := readWholeLog(t, l, "j"); got != "run one\nrun two\nrun three\n" {
		t.Fatalf("log = %q, want the output of all three runs in order", got)
	}
}

func TestLogStore_SealThenCreateAfterARunThatNeverClosedItsLog(t *testing.T) {
	l := NewLogStore(t.TempDir())
	writeLogRun(t, l, "j", "run one\n", false)
	if err := l.Seal("j"); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	writeLogRun(t, l, "j", "run two\n", true)
	if got := readWholeLog(t, l, "j"); got != "run one\nrun two\n" {
		t.Fatalf("log = %q, want run one's output kept although its gzip trailer was never written", got)
	}
}

func TestLogStore_SealThenCreateAfterARunCutOffMidWrite(t *testing.T) {
	l := NewLogStore(t.TempDir())
	writeLogRun(t, l, "j", "run one\n", false)
	f, err := os.OpenFile(l.path("j"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{0x05, 0xc3, 0x11}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Seal("j"); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	writeLogRun(t, l, "j", "run two\n", true)
	if got := readWholeLog(t, l, "j"); got != "run one\nrun two\n" {
		t.Fatalf("log = %q, want the output decoded before the damage, then run two", got)
	}
	entries, err := os.ReadDir(l.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("log directory holds %d entries after the repair, want only the log", len(entries))
	}
}

func TestLogStore_SealThenCreateOverAnEmptyLogFile(t *testing.T) {
	l := NewLogStore(t.TempDir())
	if err := os.WriteFile(l.path("j"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.Seal("j"); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	writeLogRun(t, l, "j", "run\n", true)
	if got := readWholeLog(t, l, "j"); got != "run\n" {
		t.Fatalf("log = %q, want %q", got, "run\n")
	}
}

func TestLogStore_FollowCrossesTheMemberBoundaryOfAResumedJob(t *testing.T) {
	l := NewLogStore(t.TempDir())
	writeLogRun(t, l, "j", "run one\n", true)

	var flag followFlag
	f := l.Follow(context.Background(), "j", flag.finished, 5*time.Millisecond)
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	if got := readWithin(t, gz, len("run one\n")); got != "run one\n" {
		t.Fatalf("first run = %q, want %q", got, "run one\n")
	}

	w, err := l.Create("j")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := io.WriteString(w, "run two\n"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := readWithin(t, gz, len("run two\n")); got != "run two\n" {
		t.Fatalf("resumed run = %q, want %q while it is still running", got, "run two\n")
	}
	if _, err := io.WriteString(w, "end\n"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	flag.done.Store(true)
	rest, err := io.ReadAll(gz)
	if err != nil || string(rest) != "end\n" {
		t.Fatalf("rest = %q, %v; want %q and a clean end", rest, err, "end\n")
	}
}
