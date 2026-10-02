package migrate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mdg-labs/hoserva/internal/store"
)

// JobResource is the resource id a scan job is submitted under, so two scans
// never run at once.
const JobResource = "migration"

var (
	// ErrZipTooLarge is returned for an upload larger than maxZipBytes.
	ErrZipTooLarge = errors.New("the Flash Backup zip is larger than the limit")
	// ErrScanInProgress is returned for a new scan, or a forget, while a scan runs.
	ErrScanInProgress = errors.New("a migration scan is already running")
	// ErrNoReport is returned when no scan has produced a report.
	ErrNoReport = errors.New("there is no migration report")
)

// Phase is where the migration session stands.
type Phase string

const (
	PhaseNone       Phase = "none"
	PhaseScanning   Phase = "scanning"
	PhaseScanFailed Phase = "scan_failed"
	PhaseScanned    Phase = "scanned"
)

// SourceInfo describes the zip a report was made from.
type SourceInfo struct {
	File       string    `json:"file"`
	Size       int64     `json:"size"`
	ReceivedAt time.Time `json:"receivedAt"`
}

type scanRecord struct {
	File             string
	Size             int64
	ReceivedAt       time.Time
	UnverifiedLayout bool
	Error            string
}

// session is the migration_session row in the form the service works with.
type session struct {
	Source *SourceInfo
	Report *Report
	Scan   *scanRecord
}

// State is the session as the API shows it. Source is the zip the Report was
// made from, whose name is never shown: it is a file in the state directory.
type State struct {
	Phase     Phase
	ScanError string
	Source    *SourceInfo
	Report    *Report
}

// Service keeps the one migration session: its record in the migration_session
// table (D4) and, beside it in Dir, the uploaded zip the record names. The zip
// holds secrets (password hashes, SSH host keys, WireGuard and rclone config, the
// licence key, containers' environment), so Dir is 0700 and every file in it
// 0600; it is never in the database or in a config backup. The report quotes
// names and counts only.
//
// The row is always written before a file it names is relied on and before a
// file it no longer names is deleted, so a crash leaves at worst an unreferenced
// file, which the next prune removes, and never a row naming a file that was
// deleted first.
type Service struct {
	Dir      string
	Scanner  *Scanner
	Sessions *store.MigrationSessionStore
	// JobEnded reports whether the job with this id has reached a terminal
	// status, and which. A scan whose job ended without the scan recording its
	// own outcome (a queued job cancelled, or dropped when the array stops) is
	// failed, not running.
	JobEnded func(ctx context.Context, id string) (status string, ended bool, err error)

	mu sync.Mutex
	// scanJob is the job queued for the staged upload named here. It lives in
	// memory only: a scan the previous process left unfinished is failed by
	// Recover, so no persisted scan outlives the job that was to run it.
	scanJob struct{ upload, id string }
}

func (s *Service) path(name string) string { return filepath.Join(s.Dir, name) }

func (s *Service) fileExists(name string) bool {
	_, err := os.Stat(s.path(name))
	return err == nil
}

func (s *Service) load(ctx context.Context) (*session, error) {
	row, found, err := s.Sessions.Get(ctx)
	if err != nil {
		return nil, err
	}
	sess := &session{}
	if !found {
		return sess, nil
	}
	if row.SourceFile != "" {
		sess.Source = &SourceInfo{File: row.SourceFile, Size: row.SourceSize, ReceivedAt: row.SourceReceivedAt}
	}
	if row.Report != nil {
		sess.Report = &Report{}
		if err := json.Unmarshal(row.Report, sess.Report); err != nil {
			return nil, fmt.Errorf("decoding the migration report: %w", err)
		}
	}
	if row.ScanFile != "" {
		sess.Scan = &scanRecord{File: row.ScanFile, Size: row.ScanSize, ReceivedAt: row.ScanReceivedAt, UnverifiedLayout: row.ScanUnverifiedLayout, Error: row.ScanError}
	}
	return sess, nil
}

// save writes sess as the session's row, or deletes the row when sess is empty.
func (s *Service) save(ctx context.Context, sess *session) error {
	if sess.Source == nil && sess.Report == nil && sess.Scan == nil {
		return s.Sessions.Delete(ctx)
	}
	var row store.MigrationSession
	if sess.Source != nil {
		row.SourceFile, row.SourceSize, row.SourceReceivedAt = sess.Source.File, sess.Source.Size, sess.Source.ReceivedAt
	}
	if sess.Report != nil {
		data, err := json.Marshal(sess.Report)
		if err != nil {
			return fmt.Errorf("encoding the migration report: %w", err)
		}
		row.Report = data
	}
	if sc := sess.Scan; sc != nil {
		row.ScanFile, row.ScanSize, row.ScanReceivedAt, row.ScanUnverifiedLayout, row.ScanError = sc.File, sc.Size, sc.ReceivedAt, sc.UnverifiedLayout, sc.Error
	}
	return s.Sessions.Put(ctx, row)
}

// scanOutcome says whether the session's recorded scan can still finish. When it
// cannot, failure says why: it recorded an error, its staged upload is not here
// (a row restored from another installation's archive can name an upload this
// one never had), or its job ended without running the scan.
func (s *Service) scanOutcome(ctx context.Context, sc *scanRecord) (running bool, failure string, err error) {
	switch {
	case sc == nil:
		return false, "", nil
	case sc.Error != "":
		return false, sc.Error, nil
	case !s.fileExists(sc.File):
		return false, "the scan's upload is no longer on this machine; scan again", nil
	case s.JobEnded == nil || s.scanJob.upload != sc.File:
		return false, "this scan's job is not known to this daemon; scan again", nil
	}
	status, ended, err := s.JobEnded(ctx, s.scanJob.id)
	if err != nil {
		return false, "", fmt.Errorf("reading the scan's job: %w", err)
	}
	if ended {
		return false, fmt.Sprintf("the scan's job ended (%s) without finishing the scan; scan again", status), nil
	}
	return true, "", nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

func (s *Service) ensureDir() error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(s.Dir, 0o700)
}

// prune removes every zip in Dir the session does not name: an upload that was
// refused, a source a newer scan replaced, or one a crash left behind.
func (s *Service) prune(sess *session) error {
	keep := map[string]bool{}
	if sess.Source != nil {
		keep[sess.Source.File] = true
	}
	if sess.Scan != nil {
		keep[sess.Scan.File] = true
	}
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "upload-") && !keep[e.Name()] {
			if err := os.Remove(s.path(e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// pruneLogged prunes after the session has already changed: a file that cannot
// be removed now is removed by the next prune, and failing the operation that
// just succeeded would report a half-applied change that is not one.
func (s *Service) pruneLogged(sess *session) {
	if err := s.prune(sess); err != nil {
		log.Printf("migrate: removing superseded uploads: %v", err)
	}
}

// Recover runs once at start, before the scheduler accepts jobs. A scan the
// previous process left unfinished is marked failed: its job was interrupted,
// and a scan is re-run, never resumed. A source the row names but this machine
// does not have (a row restored from another installation's archive) is
// forgotten, so the row never names a zip that is not here; its report stays.
func (s *Service) Recover(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureDir(); err != nil {
		return err
	}
	sess, err := s.load(ctx)
	if err != nil {
		return err
	}
	changed := false
	if sess.Scan != nil && sess.Scan.Error == "" {
		sess.Scan.Error = "the scan was interrupted by a restart; scan again"
		changed = true
	}
	if sess.Source != nil && !s.fileExists(sess.Source.File) {
		sess.Source = nil
		changed = true
	}
	if changed {
		if err := s.save(ctx, sess); err != nil {
			return err
		}
	}
	return s.prune(sess)
}

// StartScan stages the upload, refuses it unless it is a usable Flash Backup,
// records the scan in the session and calls submit, with the name the staged file
// was given (never one a client chose), to queue the job; submit returns the id
// of the job it queued. A refusal, or a submit
// that fails, leaves the session as it was and no staged file behind. Nothing
// about the uploaded zip is ever modified.
func (s *Service) StartScan(ctx context.Context, upload io.Reader, opts ScanOptions, submit func(ctx context.Context, upload string) (jobID string, err error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureDir(); err != nil {
		return err
	}
	sess, err := s.load(ctx)
	if err != nil {
		return err
	}
	if running, _, err := s.scanOutcome(ctx, sess.Scan); err != nil {
		return err
	} else if running {
		return ErrScanInProgress
	}

	name, size, err := s.stage(upload)
	if err != nil {
		return err
	}
	staged := true
	defer func() {
		if staged {
			_ = os.Remove(s.path(name))
		}
	}()
	src, f, err := OpenZipFile(s.path(name))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := Inspect(src, opts); err != nil {
		return err
	}

	previous := *sess
	sess.Scan = &scanRecord{File: name, Size: size, ReceivedAt: time.Now().UTC(), UnverifiedLayout: opts.UnverifiedLayout}
	if err := s.save(ctx, sess); err != nil {
		return fmt.Errorf("recording the scan: %w", err)
	}
	jobID, err := submit(ctx, name)
	if err != nil {
		// A request that was cancelled is a reason submit can fail; the previous
		// session is put back all the same.
		if rerr := s.save(context.WithoutCancel(ctx), &previous); rerr != nil {
			return errors.Join(err, fmt.Errorf("restoring the previous session: %w", rerr))
		}
		return err
	}
	staged = false
	s.scanJob.upload, s.scanJob.id = name, jobID
	s.pruneLogged(sess)
	return nil
}

// stage copies the upload to a new 0600 file in Dir, bounded by maxZipBytes.
func (s *Service) stage(upload io.Reader) (name string, size int64, err error) {
	var rnd [12]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", 0, err
	}
	name = "upload-" + hex.EncodeToString(rnd[:]) + ".zip"
	f, err := os.OpenFile(s.path(name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", 0, err
	}
	size, err = io.Copy(f, io.LimitReader(upload, maxZipBytes+1))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && size > maxZipBytes {
		err = ErrZipTooLarge
	}
	if err != nil {
		_ = os.Remove(s.path(name))
		return "", 0, fmt.Errorf("staging the upload: %w", err)
	}
	if err := syncDir(s.Dir); err != nil {
		_ = os.Remove(s.path(name))
		return "", 0, fmt.Errorf("staging the upload: %w", err)
	}
	return name, size, nil
}

// RunScan is the migration_scan job: it scans the staged upload and, on success,
// makes it the session's source with the report. A failure, including a
// cancelled run, records itself in the session and keeps the previous report.
func (s *Service) RunScan(ctx context.Context, out io.Writer, upload string) error {
	s.mu.Lock()
	sess, err := s.load(ctx)
	if err == nil && (sess.Scan == nil || sess.Scan.File != upload || sess.Scan.Error != "") {
		err = errors.New("this scan was replaced or forgotten")
	}
	var rec scanRecord
	if err == nil {
		rec = *sess.Scan
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}

	report, err := s.scan(ctx, rec)
	if err != nil {
		if rerr := s.recordFailure(ctx, upload, err); rerr != nil {
			return errors.Join(err, fmt.Errorf("recording the failure: %w", rerr))
		}
		return err
	}
	_, _ = fmt.Fprintf(out, "migration scan finished: %s\n", report.Verdict)
	if err := s.commit(ctx, upload, rec, report); err != nil {
		if rerr := s.recordFailure(ctx, upload, err); rerr != nil {
			return errors.Join(err, fmt.Errorf("recording the failure: %w", rerr))
		}
		return err
	}
	return nil
}

func (s *Service) scan(ctx context.Context, rec scanRecord) (*Report, error) {
	src, f, err := OpenZipFile(s.path(rec.File))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return s.Scanner.Scan(ctx, src, ScanOptions{UnverifiedLayout: rec.UnverifiedLayout})
}

// commit makes the finished scan the session's source in one write of the row;
// only then is the zip it replaced deleted.
func (s *Service) commit(ctx context.Context, upload string, rec scanRecord, report *Report) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx = context.WithoutCancel(ctx)
	sess, err := s.load(ctx)
	if err != nil {
		return err
	}
	if sess.Scan == nil || sess.Scan.File != upload {
		return errors.New("this scan was replaced or forgotten")
	}
	sess.Source = &SourceInfo{File: rec.File, Size: rec.Size, ReceivedAt: rec.ReceivedAt}
	sess.Report = report
	sess.Scan = nil
	if err := s.save(ctx, sess); err != nil {
		return fmt.Errorf("saving the report: %w", err)
	}
	s.pruneLogged(sess)
	return nil
}

// recordFailure notes a failed scan in the session. A cancelled run has lost its
// own context, so the write is made without its cancellation.
func (s *Service) recordFailure(ctx context.Context, upload string, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, err := s.load(ctx)
	if err != nil {
		return err
	}
	if sess.Scan == nil || sess.Scan.File != upload {
		return nil
	}
	sess.Scan.Error = cause.Error()
	return s.save(ctx, sess)
}

// State returns the session's phase and, when one exists, its report.
func (s *Service) State(ctx context.Context) (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	running, failure, err := s.scanOutcome(ctx, sess.Scan)
	if err != nil {
		return nil, err
	}
	st := &State{Phase: PhaseNone, Source: sess.Source, Report: sess.Report}
	switch {
	case running:
		st.Phase = PhaseScanning
	case sess.Scan != nil:
		st.Phase, st.ScanError = PhaseScanFailed, failure
	case sess.Report != nil:
		st.Phase = PhaseScanned
	}
	return st, nil
}

// ReportMarkdown is the persisted report as a document.
func (s *Service) ReportMarkdown(ctx context.Context) (string, error) {
	st, err := s.State(ctx)
	if err != nil {
		return "", err
	}
	if st.Report == nil {
		return "", ErrNoReport
	}
	return st.Report.Markdown(), nil
}

// Forget deletes the session's row and then every zip in Dir, the uploaded zip
// included. It is refused while a scan runs, which has the zip open. A zip that
// cannot be removed is reported, the row already being gone, and removed by the
// next Forget or start.
func (s *Service) Forget(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureDir(); err != nil {
		return err
	}
	sess, err := s.load(ctx)
	if err != nil {
		return err
	}
	if running, _, err := s.scanOutcome(ctx, sess.Scan); err != nil {
		return err
	} else if running {
		return ErrScanInProgress
	}
	if err := s.Sessions.Delete(ctx); err != nil {
		return err
	}
	return s.prune(&session{})
}
