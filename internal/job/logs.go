package job

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// LogRetention is Q74's default: 90 days, capped at 1 GiB, oldest first.
const (
	LogRetentionAge = 90 * 24 * time.Hour
	LogRetentionCap = 1 << 30 // 1 GiB
)

// logFilePattern is the only shape Prune ever considers its own — a
// filename it did not write itself (any other extension, any character
// outside a job id's own alphabet) is left alone, never removed (Q74:
// "never deletes anything that isn't its own log file").
var logFilePattern = regexp.MustCompile(`^([A-Za-z0-9_-]+)\.log\.gz$`)

// LogStore captures each job's stdout/stderr to its own compressed file
// under Dir (Q74) — a constructor parameter, never a hardcoded path, so a
// test or dev daemon points it at a t.TempDir() and production points it
// at /var/lib/hoserva/jobs/. Dir is on the boot SSD, not a data disk, so
// Prune walking it does not violate "nothing on a timer walks a data
// disk" (doc 01 §4). Like metrics.db (package
// github.com/mdg-labs/hoserva/internal/store/metrics), everything under
// Dir is excluded from config backups (doc 10 §1: "Not included:
// metrics.db and job logs — history, not configuration") — a job's
// summary row in the jobs table is what a config backup restores; its
// captured log output is not.
type LogStore struct {
	Dir string
}

// NewLogStore returns a LogStore writing under dir, creating it if it
// doesn't exist yet.
func NewLogStore(dir string) *LogStore {
	return &LogStore{Dir: dir}
}

func (l *LogStore) path(id string) string {
	return filepath.Join(l.Dir, id+".log.gz")
}

// Create opens id's log file for writing, gzip-compressed. The returned
// WriteCloser's Close both flushes the gzip stream and closes the
// underlying file — a caller that forgets to Close leaves a truncated,
// unreadable gzip member, exactly like any other buffered writer.
func (l *LogStore) Create(id string) (io.WriteCloser, error) {
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("job log store: creating %s: %w", l.Dir, err)
	}
	f, err := os.Create(l.path(id))
	if err != nil {
		return nil, fmt.Errorf("job log store: creating log for job %s: %w", id, err)
	}
	return &gzipWriteCloser{gz: gzip.NewWriter(f), f: f}, nil
}

type gzipWriteCloser struct {
	gz *gzip.Writer
	f  *os.File
}

func (w *gzipWriteCloser) Write(p []byte) (int, error) { return w.gz.Write(p) }

func (w *gzipWriteCloser) Close() error {
	if err := w.gz.Close(); err != nil {
		_ = w.f.Close()
		return err
	}
	return w.f.Close()
}

// ErrLogNotFound is returned by Open when id has no captured log — either
// the job never wrote one, or its log has already been pruned.
var ErrLogNotFound = fmt.Errorf("job log store: no log for this job")

// Open returns id's captured log, still gzip-compressed — getJobLog (doc
// 01 §4, api/openapi.yaml's `application/gzip` response) serves these
// bytes as-is, never decompressing and recompressing them.
func (l *LogStore) Open(id string) (io.ReadCloser, error) {
	f, err := os.Open(l.path(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrLogNotFound
		}
		return nil, fmt.Errorf("job log store: opening log for job %s: %w", id, err)
	}
	return f, nil
}

// Prune enforces Q74's retention: files older than LogRetentionAge, then
// the oldest remaining files once the directory's total size exceeds
// LogRetentionCap, are deleted — oldest first. A file for one of
// activeIDs is never deleted, no matter its age or the directory's total
// size (Q74: "never deletes a running job's log"), and any file this
// store didn't itself write (logFilePattern doesn't match its name) is
// left untouched.
func (l *LogStore) Prune(now time.Time, activeIDs map[string]bool) error {
	entries, err := os.ReadDir(l.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("job log store: reading %s: %w", l.Dir, err)
	}

	type ownedFile struct {
		id      string
		path    string
		size    int64
		modTime time.Time
	}
	var files []ownedFile
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := logFilePattern.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		f := ownedFile{id: m[1], path: filepath.Join(l.Dir, e.Name()), size: info.Size(), modTime: info.ModTime()}
		files = append(files, f)
		total += f.size
	}

	sort.Slice(files, func(i, j int) bool { return files[i].modTime.Before(files[j].modTime) })

	for _, f := range files {
		if activeIDs[f.id] {
			continue
		}
		expired := now.Sub(f.modTime) > LogRetentionAge
		overCap := total > LogRetentionCap
		if !expired && !overCap {
			continue
		}
		if err := os.Remove(f.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("job log store: pruning %s: %w", f.path, err)
		}
		total -= f.size
	}
	return nil
}
