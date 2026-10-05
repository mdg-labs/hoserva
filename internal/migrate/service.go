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

	"github.com/mdg-labs/hoserva/internal/disk"
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
	// ErrImportPending is returned for a forget while an import's adoption is
	// waiting for its point of no return, or part-way through it: the session's
	// baseline is what the verify step compares the adopted disks against.
	ErrImportPending = errors.New("an Unraid import is pending its point of no return: its scan baseline is kept until then")
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
	// PhaseImported is the import's adoption: the data disks are mounted
	// read-only and parity and cache are untouched, until the point of no
	// return.
	PhaseImported Phase = "imported"
	// PhaseVerifying, PhaseVerifyFailed and PhaseVerified are the imported
	// array's verify phase: running, finished with a mismatch or without
	// finishing, and finished with every comparison passing.
	PhaseVerifying    Phase = "verifying"
	PhaseVerifyFailed Phase = "verify_failed"
	PhaseVerified     Phase = "verified"
	// PhaseInitializing is the point of no return that stopped after the former
	// parity and cache disks were formatted and recorded: the rest of it is
	// finished by running it again, which erases nothing.
	PhaseInitializing Phase = "initializing"
)

// SourceInfo describes what a report was made from: an uploaded zip, or a flash
// device (IsDevice), for which the zip's size is 0 and ReceivedAt is when it was
// scanned.
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
	FullChecksums    bool
	Error            string
}

// session is the migration_session row in the form the service works with.
type session struct {
	Source *SourceInfo
	Report *Report
	Scan   *scanRecord
	Verify *VerifyResult
	// checklist is the post-migration checklist's record as stored. save never
	// writes it: checklist.go replaces it on its own.
	checklist []byte
}

// State is the session as the API shows it. Source is what the Report was made
// from; an uploaded zip's name is never shown, it is a file in the state
// directory.
type State struct {
	Phase     Phase
	ScanError string
	Source    *SourceInfo
	Report    *Report
	// Verify is the verify phase's result, present only while an import is
	// pending its point of no return.
	Verify *VerifyResult
}

// An upload is written under stagingPrefix while it arrives and is inspected,
// and renamed to uploadPrefix once StartScan accepts it. Only uploadPrefix
// files are pruned.
const (
	stagingPrefix = "staging-"
	uploadPrefix  = "upload-"
)

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
	// Mounter mounts the Unraid flash device read-only for a scan of it. Nil
	// means this daemon reads zips only.
	Mounter disk.ReadOnlyMounter
	// Pending reports whether an import's adoption is waiting for its point of
	// no return (store.ArrayStore.MigrationPending). Nil means it never is.
	Pending func(ctx context.Context) (bool, error)
	// ArrayDevices returns the devices in this machine's array, which are never
	// offered as the flash. Nil means none are known.
	ArrayDevices func(ctx context.Context) (map[string]struct{}, error)
	// Adopted returns the adopted array's data disks, in the pool's branch order,
	// and where the pool is mounted, for the verify phase. ConfirmReadOnly
	// returns nil only when the mount at where is read-only, from the kernel's
	// mount table. Verify refuses to run while either is nil.
	Adopted         func(ctx context.Context) (Adoption, error)
	ConfirmReadOnly func(ctx context.Context, where string) error
	// Record returns what the adoption recorded: its data disks, and the former
	// parity and cache disks it left unformatted. The point of no return resolves
	// them again from a fresh inventory (PlanParityInit).
	Record func(ctx context.Context) ([]store.ArrayDisk, []store.RecordedDisk, error)
	// Finishing reports whether a parity initialisation stopped after the former
	// parity and cache disks were formatted and recorded, with the rest of it left
	// (store.ArrayStore.MigrationFinishing). Nil means it never is.
	Finishing func(ctx context.Context) (bool, error)
	// Initialized reports whether the parity initialisation has been confirmed:
	// an array exists and no migration is pending or part-way through its point
	// of no return. Phase D (containers.go) creates and starts nothing until it
	// is true; nil means it never is.
	Initialized func(ctx context.Context) (bool, error)
	// Stacks is the Compose stack layer the migrated containers are created
	// through. Nil means this daemon creates none.
	Stacks StackLayer
	// ChecklistRecords are where the post-migration checklist reads its records
	// (checklist.go). Zero means this daemon cannot build it.
	ChecklistRecords ChecklistSources
	// Now returns the current time; nil means the system clock.
	Now func() time.Time
	// DataRoots are the host paths a migrated container's data lives under,
	// which the data check reads; nil means /mnt/user and /mnt/cache.
	DataRoots []string

	// flowMu serialises the Phase D operations that read the record of the
	// created stacks, decide, and then act (create, start, confirm). It is taken
	// before mu, never after.
	flowMu sync.Mutex
	// checklistMu serialises the read-modify-write of the checklist's record.
	// It is taken before mu, never after.
	checklistMu sync.Mutex
	// stickMu serialises the one private mountpoint a flash device is read at.
	// It is taken after mu, never before.
	stickMu sync.Mutex
	mu      sync.Mutex
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
	sess.checklist = row.Checklist
	if row.SourceFile != "" {
		sess.Source = &SourceInfo{File: row.SourceFile, Size: row.SourceSize, ReceivedAt: row.SourceReceivedAt}
	}
	if row.Report != nil {
		sess.Report = &Report{}
		if err := json.Unmarshal(row.Report, sess.Report); err != nil {
			return nil, fmt.Errorf("decoding the migration report: %w", err)
		}
	}
	if len(row.Verify) > 0 {
		sess.Verify = &VerifyResult{}
		if err := json.Unmarshal(row.Verify, sess.Verify); err != nil {
			sess.Verify = &VerifyResult{Status: VerifyFailed, Error: "the stored verify result could not be read: run verify again"}
		}
	}
	if row.ScanFile != "" {
		sess.Scan = &scanRecord{File: row.ScanFile, Size: row.ScanSize, ReceivedAt: row.ScanReceivedAt, UnverifiedLayout: row.ScanUnverifiedLayout, FullChecksums: row.ScanFullChecksums, Error: row.ScanError}
	}
	return sess, nil
}

// save writes sess as the session's row, or deletes the row when sess is empty.
func (s *Service) save(ctx context.Context, sess *session) error {
	if sess.Source == nil && sess.Report == nil && sess.Scan == nil && sess.Verify == nil {
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
	if sess.Verify != nil {
		data, err := json.Marshal(sess.Verify)
		if err != nil {
			return fmt.Errorf("encoding the verify result: %w", err)
		}
		row.Verify = data
	}
	if sc := sess.Scan; sc != nil {
		row.ScanFile, row.ScanSize, row.ScanReceivedAt, row.ScanUnverifiedLayout, row.ScanFullChecksums, row.ScanError = sc.File, sc.Size, sc.ReceivedAt, sc.UnverifiedLayout, sc.FullChecksums, sc.Error
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
	case !isDeviceScan(sc.File) && !s.fileExists(sc.File):
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

// prune removes every zip and baseline file in Dir the session does not name: an
// upload that was refused, a source or baseline a newer scan replaced, or one a
// crash left behind.
func (s *Service) prune(sess *session) error {
	keep := map[string]bool{}
	if sess.Source != nil {
		keep[sess.Source.File] = true
	}
	if sess.Scan != nil {
		keep[sess.Scan.File] = true
	}
	if sess.Report != nil && sess.Report.Baseline != nil {
		keep[sess.Report.Baseline.File] = true
	}
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if (strings.HasPrefix(e.Name(), uploadPrefix) || strings.HasPrefix(e.Name(), baselinePrefix)) && !keep[e.Name()] {
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
	if sess.Source != nil && !isDeviceScan(sess.Source.File) && !s.fileExists(sess.Source.File) {
		sess.Source = nil
		changed = true
	}
	if sess.Verify != nil && sess.Verify.Status == VerifyRunning {
		sess.Verify = &VerifyResult{Status: VerifyFailed, StartedAt: sess.Verify.StartedAt, Error: "the verify was interrupted by a restart: run it again"}
		changed = true
	}
	if changed {
		if err := s.save(ctx, sess); err != nil {
			return err
		}
	}
	s.recoverStick(ctx)
	s.recoverDiskMounts(ctx)
	if err := s.removeStaging(); err != nil {
		return err
	}
	return s.prune(sess)
}

// recoverDiskMounts runs at start: a source disk mounted at one of the private
// mountpoints is a mount of an earlier process, and is released. One that stays
// does not stop the zip scans, only the next disk read, which refuses until it is
// gone.
func (s *Service) recoverDiskMounts(ctx context.Context) {
	if s.Mounter == nil {
		return
	}
	if err := releaseMounts(ctx, s.Mounter, s.Dir); err != nil {
		log.Printf("migrate: %v", err)
	}
}

// removeStaging removes the uploads a previous process was still receiving and
// the scratch space and baseline file of a scan it was still running. Only
// Recover calls it: at any other time they belong to a live upload or scan. A
// scratch directory never holds a mount: the mountpoints are under mnt/.
func (s *Service) removeStaging() error {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		switch {
		case strings.HasPrefix(e.Name(), stagingPrefix):
			if err := os.Remove(s.path(e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
		case strings.HasPrefix(e.Name(), tmpPrefix):
			if err := os.RemoveAll(s.path(e.Name())); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// OpenBaseline opens the baseline the session's latest report was made with, for
// the verify phase. It returns ErrNoBaseline when the session has none, or when
// its file is not on this machine, as after a config import of another
// installation's archive.
func (s *Service) OpenBaseline(ctx context.Context) (*BaselineReader, error) {
	name, err := s.baselineName(ctx)
	if err != nil {
		return nil, err
	}
	// Reading the whole file to check it is slow on a large array, so s.mu is
	// not held for it. A baseline's name is random and never reused, and the row
	// stops naming a file before prune removes it: a prune or a new scan that
	// lands now leaves this file or removes it, which reads as ErrNoBaseline.
	return OpenBaseline(s.path(name))
}

func (s *Service) baselineName(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, err := s.load(ctx)
	if err != nil {
		return "", err
	}
	if sess.Report == nil || sess.Report.Baseline == nil {
		return "", ErrNoBaseline
	}
	name := sess.Report.Baseline.File
	if name == "" || filepath.Base(name) != name || !strings.HasPrefix(name, baselinePrefix) {
		return "", fmt.Errorf("%w: the session names %q", ErrNoBaseline, name)
	}
	return name, nil
}

// StartScan stages the upload, refuses it unless it is a usable Flash Backup,
// records the scan in the session and calls submit, with the name the staged file
// was given (never one a client chose), to queue the job; submit returns the id
// of the job it queued. A refusal, or a submit
// that fails, leaves the session as it was and no staged file behind. Nothing
// about the uploaded zip is ever modified.
//
// The body arrives and is inspected without s.mu held: a slow client would
// otherwise stall every session operation, a running scan's commit among them,
// for as long as its upload takes. Until it is accepted the file carries the
// staging prefix, which prune never matches.
func (s *Service) StartScan(ctx context.Context, upload io.Reader, opts ScanOptions, submit func(ctx context.Context, upload string) (jobID string, err error)) error {
	if err := s.refuseWhileScanning(ctx); err != nil {
		return err
	}
	name, size, err := s.stage(upload)
	if err != nil {
		return err
	}
	kept := false
	defer func() {
		if !kept {
			_ = os.Remove(s.path(name))
		}
	}()
	if err := s.inspectStaged(name, opts); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	sess, err := s.load(ctx)
	if err != nil {
		return err
	}
	if running, _, err := s.scanOutcome(ctx, sess.Scan); err != nil {
		return err
	} else if running {
		return ErrScanInProgress
	}
	accepted := uploadPrefix + strings.TrimPrefix(name, stagingPrefix)
	if err := os.Rename(s.path(name), s.path(accepted)); err != nil {
		return fmt.Errorf("staging the upload: %w", err)
	}
	name = accepted
	if err := syncDir(s.Dir); err != nil {
		return fmt.Errorf("staging the upload: %w", err)
	}

	rec := &scanRecord{File: name, Size: size, ReceivedAt: time.Now().UTC(), UnverifiedLayout: opts.UnverifiedLayout, FullChecksums: opts.FullChecksums}
	if err := s.queue(ctx, sess, rec, submit); err != nil {
		return err
	}
	kept = true
	return nil
}

// refuseWhileScanning refuses an upload before its body is read when a scan is
// already running; StartScan checks again once the upload has arrived.
func (s *Service) refuseWhileScanning(ctx context.Context) error {
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
	return nil
}

func (s *Service) inspectStaged(name string, opts ScanOptions) error {
	src, f, err := OpenZipFile(s.path(name))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = Inspect(src, opts)
	return err
}

// queue records rec as the session's scan and calls submit, which queues the
// job. The caller holds s.mu. A submit that fails puts the previous session
// back.
func (s *Service) queue(ctx context.Context, sess *session, rec *scanRecord, submit func(ctx context.Context, scan string) (jobID string, err error)) error {
	previous := *sess
	sess.Scan = rec
	if err := s.save(ctx, sess); err != nil {
		return fmt.Errorf("recording the scan: %w", err)
	}
	jobID, err := submit(ctx, rec.File)
	if err != nil {
		// A request that was cancelled is a reason submit can fail; the previous
		// session is put back all the same.
		if rerr := s.save(context.WithoutCancel(ctx), &previous); rerr != nil {
			return errors.Join(err, fmt.Errorf("restoring the previous session: %w", rerr))
		}
		return err
	}
	s.scanJob.upload, s.scanJob.id = rec.File, jobID
	s.pruneLogged(sess)
	return nil
}

// stage copies the upload to a new 0600 staging file in Dir, bounded by
// maxZipBytes.
func (s *Service) stage(upload io.Reader) (name string, size int64, err error) {
	var rnd [12]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", 0, err
	}
	name = stagingPrefix + hex.EncodeToString(rnd[:]) + ".zip"
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

	report, err := s.scan(withLog(ctx, out), rec)
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

type progressKey struct{}
type logKey struct{}

// WithProgress returns a context in which a scan run through it reports its
// progress, as a percentage of the whole, to fn. It is called when the
// percentage changes.
func WithProgress(ctx context.Context, fn func(pct int)) context.Context {
	return context.WithValue(ctx, progressKey{}, fn)
}

func withLog(ctx context.Context, out io.Writer) context.Context {
	return context.WithValue(ctx, logKey{}, out)
}

// options are the scan options the record stands for, with the progress hook the
// context carries: each line goes to the job's log and each new percentage to the
// job.
func (rec scanRecord) options(ctx context.Context) ScanOptions {
	opts := ScanOptions{UnverifiedLayout: rec.UnverifiedLayout, FullChecksums: rec.FullChecksums}
	out, _ := ctx.Value(logKey{}).(io.Writer)
	pct, _ := ctx.Value(progressKey{}).(func(int))
	last := -1
	opts.Progress = func(p int, line string) {
		if out != nil && line != "" {
			_, _ = fmt.Fprintln(out, line)
		}
		if pct != nil && p != last {
			last = p
			pct(p)
		}
	}
	return opts
}

func (s *Service) scan(ctx context.Context, rec scanRecord) (*Report, error) {
	if isDeviceScan(rec.File) {
		return s.scanDevice(ctx, rec)
	}
	src, f, err := OpenZipFile(s.path(rec.File))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return s.Scanner.Scan(ctx, src, rec.options(ctx))
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
	if sess.Report != nil {
		report.Containers = sess.Report.Containers
	}
	sess.Report = report
	sess.Scan = nil
	sess.Verify = nil
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
	finishing, err := s.finishing(ctx)
	if err != nil {
		return nil, err
	}
	if finishing {
		st.Phase = PhaseInitializing
	}
	if s.Pending != nil {
		pending, err := s.Pending(ctx)
		if err != nil {
			return nil, fmt.Errorf("reading whether an import is pending: %w", err)
		}
		if pending {
			st.Phase = PhaseImported
			if v := sess.Verify; v != nil {
				st.Verify = v
				switch v.Status {
				case VerifyRunning:
					st.Phase = PhaseVerifying
				case VerifyPassed:
					st.Phase = PhaseVerified
				default:
					st.Phase = PhaseVerifyFailed
				}
			}
		}
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
// next Forget or start. It is refused with ErrImportPending while an import's
// adoption is pending.
func (s *Service) Forget(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureDir(); err != nil {
		return err
	}
	if s.Pending != nil {
		pending, err := s.Pending(ctx)
		if err != nil {
			return fmt.Errorf("reading whether an import is pending: %w", err)
		}
		if pending {
			return ErrImportPending
		}
	}
	if finishing, err := s.finishing(ctx); err != nil {
		return err
	} else if finishing {
		return ErrImportPending
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
