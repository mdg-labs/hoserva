package migrate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

var ctx0 = context.Background()

func newSessions(t *testing.T) *store.MigrationSessionStore {
	t.Helper()
	st, _ := newSessionsDB(t)
	return st
}

func newSessionsDB(t *testing.T) (*store.MigrationSessionStore, *sql.DB) {
	t.Helper()
	migrations, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "migrate.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, _, err := (&store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}).Apply(ctx0); err != nil {
		t.Fatal(err)
	}
	return store.NewMigrationSessionStore(db), db
}

func newService(t *testing.T, variant string) (*Service, fixtureSpec, map[string][]byte) {
	t.Helper()
	return newServiceWithJobs(t, variant, fakeJobs{})
}

func newServiceWithJobs(t *testing.T, variant string, jobs fakeJobs) (*Service, fixtureSpec, map[string][]byte) {
	t.Helper()
	files, spec := flashTree(t, variant)
	return &Service{Dir: filepath.Join(t.TempDir(), "migrate"), Scanner: scanner(fixtureDisks(spec)), Sessions: newSessions(t), JobEnded: jobs.ended}, spec, files
}

// fakeJobs stands in for the scheduler's job store: a job has ended once a
// test says so.
type fakeJobs map[string]string

func (j fakeJobs) ended(_ context.Context, id string) (string, bool, error) {
	status, ended := j[id]
	return status, ended, nil
}

// scanNow starts a scan and then runs the job it submitted, as the scheduler
// does: in its own goroutine, once StartScan has released the session.
func scanNow(s *Service, upload []byte, opts ScanOptions) error {
	var p string
	if err := s.StartScan(context.Background(), bytes.NewReader(upload), opts, func(_ context.Context, got string) (string, error) {
		p = got
		return "job-1", nil
	}); err != nil {
		return err
	}
	return s.RunScan(context.Background(), io.Discard, p)
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestService_ScanKeepsTheUploadedZipUnmodifiedAndPrivate(t *testing.T) {
	s, _, files := newService(t, "unraid-7x-xfs-single-parity")
	data := zipOf(t, files, false)

	if err := scanNow(s, data, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	st, err := s.State(ctx0)
	if err != nil || st.Phase != PhaseScanned || st.Report == nil || st.Source == nil {
		t.Fatalf("State = %+v, %v; want a scanned session with its report", st, err)
	}

	stored, err := os.ReadFile(filepath.Join(s.Dir, st.Source.File))
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(stored) != sha256.Sum256(data) {
		t.Error("the stored zip is not the uploaded one, byte for byte")
	}
	for _, e := range dirEntries(t, s.Dir) {
		info, _ := os.Stat(filepath.Join(s.Dir, e))
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is %v: it holds secrets", e, info.Mode().Perm())
		}
	}
	if info, _ := os.Stat(s.Dir); info.Mode().Perm() != 0o700 {
		t.Errorf("state directory mode = %v, want 0700", info.Mode().Perm())
	}
	if names := dirEntries(t, s.Dir); len(names) != 1 {
		t.Errorf("session directory holds %v, want the one zip", names)
	}
	if names := dirEntries(t, filepath.Dir(s.Dir)); len(names) != 1 || names[0] != "migrate" {
		t.Errorf("the scan wrote %v beside its directory", names)
	}
}

// The report persists beyond the process: a second Service over the same
// directory reads it, and the Markdown document is what a download returns.
func TestService_TheReportIsPersistedAndDownloadable(t *testing.T) {
	s, spec, files := newService(t, "unraid-7x-xfs-single-parity")
	if err := scanNow(s, zipOf(t, files, false), ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	again := &Service{Dir: s.Dir, Scanner: scanner(fixtureDisks(spec)), Sessions: s.Sessions}
	md, err := again.ReportMarkdown(ctx0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(md, "# Hoserva migration scan report") || !strings.Contains(md, "Disk serial mapping") || !strings.Contains(md, "disk3-hoserva-test") {
		t.Errorf("document:\n%s", md)
	}
}

func TestService_NoReportBeforeAScan(t *testing.T) {
	s, _, _ := newService(t, "unraid-7x-xfs-single-parity")
	if st, err := s.State(ctx0); err != nil || st.Phase != PhaseNone || st.Report != nil {
		t.Errorf("State = %+v, %v", st, err)
	}
	if _, err := s.ReportMarkdown(ctx0); !errors.Is(err, ErrNoReport) {
		t.Errorf("ReportMarkdown = %v, want ErrNoReport", err)
	}
}

func TestService_RefusedUploadsLeaveNothingBehind(t *testing.T) {
	files, _ := flashTree(t, "unraid-7x-xfs-single-parity")
	delete(files, "config/disk.cfg")
	badLayout, _ := flashTree(t, "unraid-7x-xfs-single-parity")
	badLayout["changes.txt"] = []byte("# Version 6.9.2 2021-01-01\n")
	traversal, _ := flashTree(t, "unraid-7x-xfs-single-parity")
	traversal["../x"] = []byte("x")

	for name, tc := range map[string]struct {
		upload []byte
		want   error
	}{
		"not a zip":         {[]byte("hello"), ErrInvalidZip},
		"empty":             {nil, ErrInvalidZip},
		"no disk.cfg":       {zipOf(t, files, false), ErrNoDiskCfg},
		"unknown layout":    {zipOf(t, badLayout, false), ErrUnsupportedLayout},
		"entry leaves root": {zipOf(t, traversal, false), ErrInvalidZip},
	} {
		t.Run(name, func(t *testing.T) {
			s, _, _ := newService(t, "unraid-7x-xfs-single-parity")
			submitted := false
			err := s.StartScan(context.Background(), bytes.NewReader(tc.upload), ScanOptions{}, func(context.Context, string) (string, error) {
				submitted = true
				return "job-1", nil
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("StartScan = %v, want %v", err, tc.want)
			}
			if submitted {
				t.Error("a refused upload queued a job")
			}
			if names := dirEntries(t, s.Dir); len(names) != 0 {
				t.Errorf("a refused upload left %v behind", names)
			}
			if st, _ := s.State(ctx0); st.Phase != PhaseNone {
				t.Errorf("phase = %s after a refusal", st.Phase)
			}
		})
	}
}

func TestService_UnknownLayoutIsAcceptedWithTheOverride(t *testing.T) {
	s, _, files := newService(t, "unraid-7x-xfs-single-parity")
	files["changes.txt"] = []byte("# Version 6.9.2 2021-01-01\n")
	if err := scanNow(s, zipOf(t, files, false), ScanOptions{UnverifiedLayout: true}); err != nil {
		t.Fatal(err)
	}
	st, _ := s.State(ctx0)
	if st.Report == nil || !st.Report.UnverifiedLayout {
		t.Fatalf("the override is not recorded in the persisted report: %+v", st.Report)
	}
	md, _ := s.ReportMarkdown(ctx0)
	if !strings.Contains(md, "Unverified layout") {
		t.Error("the downloaded document does not say the override was used")
	}
}

func TestService_SizeLimit(t *testing.T) {
	s, _, _ := newService(t, "unraid-7x-xfs-single-parity")
	defer func(old int64) { maxZipBytes = old }(maxZipBytes)
	maxZipBytes = 1 << 10
	huge := zeroReader{}
	err := s.StartScan(context.Background(), huge, ScanOptions{}, func(context.Context, string) (string, error) { return "job-1", nil })
	if !errors.Is(err, ErrZipTooLarge) {
		t.Fatalf("StartScan = %v, want ErrZipTooLarge", err)
	}
	if names := dirEntries(t, s.Dir); len(names) != 0 {
		t.Errorf("an oversized upload left %v behind", names)
	}
}

type zeroReader struct{}

func (z zeroReader) Read(p []byte) (int, error) { return len(p), nil }

func TestService_ASecondScanWhileOneRunsIsRefused(t *testing.T) {
	s, _, files := newService(t, "unraid-7x-xfs-single-parity")
	data := zipOf(t, files, false)
	queued := func(context.Context, string) (string, error) { return "job-1", nil }
	if err := s.StartScan(context.Background(), bytes.NewReader(data), ScanOptions{}, queued); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.State(ctx0); st.Phase != PhaseScanning {
		t.Fatalf("phase = %s, want scanning", st.Phase)
	}
	if err := s.StartScan(context.Background(), bytes.NewReader(data), ScanOptions{}, queued); !errors.Is(err, ErrScanInProgress) {
		t.Errorf("second StartScan = %v, want ErrScanInProgress", err)
	}
	if err := s.Forget(ctx0); !errors.Is(err, ErrScanInProgress) {
		t.Errorf("Forget while scanning = %v, want ErrScanInProgress", err)
	}
	if len(dirEntries(t, s.Dir)) != 1 {
		t.Errorf("session directory = %v", dirEntries(t, s.Dir))
	}
}

// A queued scan whose job ends without ever calling RunScan (cancelled while
// queued, or dropped when the array stops) is a failed scan, not a running one:
// the zip it holds can be forgotten and a new scan can start.
func TestService_AScanWhoseJobEndedWithoutRunningIsFailedNotRunning(t *testing.T) {
	for _, status := range []string{"cancelled", "interrupted", "failed"} {
		t.Run(status, func(t *testing.T) {
			jobs := fakeJobs{}
			s, _, files := newServiceWithJobs(t, "unraid-7x-xfs-single-parity", jobs)
			data := zipOf(t, files, false)
			queued := func(context.Context, string) (string, error) { return "job-1", nil }
			if err := s.StartScan(ctx0, bytes.NewReader(data), ScanOptions{}, queued); err != nil {
				t.Fatal(err)
			}
			if st, _ := s.State(ctx0); st.Phase != PhaseScanning {
				t.Fatalf("phase with the job queued = %s, want scanning", st.Phase)
			}

			jobs["job-1"] = status
			st, err := s.State(ctx0)
			if err != nil || st.Phase != PhaseScanFailed || !strings.Contains(st.ScanError, status) {
				t.Fatalf("State after the job ended %s = %+v (%v), want scan_failed naming it", status, st, err)
			}

			if err := s.Forget(ctx0); err != nil {
				t.Fatalf("Forget = %v, want the zip forgotten", err)
			}
			if names := dirEntries(t, s.Dir); len(names) != 0 {
				t.Errorf("Forget left %v", names)
			}
			if st, _ := s.State(ctx0); st.Phase != PhaseNone {
				t.Errorf("phase after Forget = %s, want none", st.Phase)
			}

			if err := s.StartScan(ctx0, bytes.NewReader(data), ScanOptions{}, func(context.Context, string) (string, error) { return "job-2", nil }); err != nil {
				t.Fatalf("a new scan after the job ended = %v", err)
			}
			if st, _ := s.State(ctx0); st.Phase != PhaseScanning {
				t.Errorf("phase of the new scan = %s, want scanning", st.Phase)
			}
		})
	}
}

// A new scan replaces a scan whose job ended without running, and removes its zip.
func TestService_ANewScanReplacesAScanWhoseJobEnded(t *testing.T) {
	jobs := fakeJobs{}
	s, _, files := newServiceWithJobs(t, "unraid-7x-xfs-single-parity", jobs)
	data := zipOf(t, files, false)
	if err := s.StartScan(ctx0, bytes.NewReader(data), ScanOptions{}, func(context.Context, string) (string, error) { return "job-1", nil }); err != nil {
		t.Fatal(err)
	}
	jobs["job-1"] = "cancelled"
	if err := scanNow(s, data, ScanOptions{}); err != nil {
		t.Fatalf("scan after a cancelled one = %v", err)
	}
	if st, _ := s.State(ctx0); st.Phase != PhaseScanned {
		t.Errorf("phase = %s, want scanned", st.Phase)
	}
	if names := dirEntries(t, s.Dir); len(names) != 1 {
		t.Errorf("directory = %v, want the new source only", names)
	}
}

// A job lookup that fails is not a scan that has ended: the session is not
// guessed at, so a zip a running scan holds is never deleted on a read error.
func TestService_AJobLookupFailureIsAnErrorNotAnEndedScan(t *testing.T) {
	s, _, files := newService(t, "unraid-7x-xfs-single-parity")
	if err := s.StartScan(ctx0, bytes.NewReader(zipOf(t, files, false)), ScanOptions{}, func(context.Context, string) (string, error) { return "job-1", nil }); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("store unavailable")
	s.JobEnded = func(context.Context, string) (string, bool, error) { return "", false, boom }
	if _, err := s.State(ctx0); !errors.Is(err, boom) {
		t.Errorf("State = %v, want the lookup error", err)
	}
	if err := s.Forget(ctx0); !errors.Is(err, boom) {
		t.Errorf("Forget = %v, want the lookup error", err)
	}
	if len(dirEntries(t, s.Dir)) != 1 {
		t.Errorf("the zip was removed: %v", dirEntries(t, s.Dir))
	}
}

// A submit that fails leaves the previous session exactly as it was, and the
// staged upload is gone.
func TestService_FailedSubmitRestoresThePreviousSession(t *testing.T) {
	s, _, files := newService(t, "unraid-7x-xfs-single-parity")
	data := zipOf(t, files, false)
	if err := scanNow(s, data, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.State(ctx0)
	names := dirEntries(t, s.Dir)

	boom := errors.New("maintenance mode")
	if err := s.StartScan(context.Background(), bytes.NewReader(data), ScanOptions{}, func(context.Context, string) (string, error) { return "", boom }); !errors.Is(err, boom) {
		t.Fatalf("StartScan = %v, want the submit error", err)
	}
	after, _ := s.State(ctx0)
	if after.Phase != PhaseScanned || after.Source.File != before.Source.File || after.Report == nil {
		t.Errorf("session after a failed submit = %+v, want %+v", after, before)
	}
	if got := dirEntries(t, s.Dir); strings.Join(got, ",") != strings.Join(names, ",") {
		t.Errorf("directory = %v, want %v", got, names)
	}
}

func TestService_RescanReplacesTheSourceAndDeletesTheOldZip(t *testing.T) {
	s, _, files := newService(t, "unraid-7x-xfs-single-parity")
	data := zipOf(t, files, false)
	if err := scanNow(s, data, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	first, _ := s.State(ctx0)
	if err := scanNow(s, data, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	second, _ := s.State(ctx0)
	if first.Source.File == second.Source.File {
		t.Error("a rescan reused the previous source")
	}
	if _, err := os.Stat(filepath.Join(s.Dir, first.Source.File)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the replaced zip is still on disk: %v", err)
	}
}

func TestService_AFailedScanIsRecordedAndKeepsThePreviousReport(t *testing.T) {
	s, spec, files := newService(t, "unraid-7x-xfs-single-parity")
	data := zipOf(t, files, false)
	if err := scanNow(s, data, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	s.Scanner = scanner(listFails{fixtureDisks(spec)})
	err := scanNow(s, data, ScanOptions{})
	if err == nil || !strings.Contains(err.Error(), "udev gone") {
		t.Fatalf("StartScan = %v, want the scan's failure", err)
	}
	st, _ := s.State(ctx0)
	if st.Phase != PhaseScanFailed || !strings.Contains(st.ScanError, "udev gone") || st.Report == nil {
		t.Errorf("State = %+v, want scan_failed with the previous report kept", st)
	}
	// A failed scan does not block a new one.
	s.Scanner = scanner(fixtureDisks(spec))
	if err := scanNow(s, data, ScanOptions{}); err != nil {
		t.Fatalf("scan after a failed one: %v", err)
	}
	if st, _ := s.State(ctx0); st.Phase != PhaseScanned {
		t.Errorf("phase = %s", st.Phase)
	}
	if names := dirEntries(t, s.Dir); len(names) != 1 {
		t.Errorf("directory = %v, want the one zip", names)
	}
}

type cancelsOnList struct {
	*disk.FakeProvider
	cancel func()
}

func (c cancelsOnList) List(ctx context.Context) ([]disk.Disk, error) {
	c.cancel()
	return c.FakeProvider.List(ctx)
}

func TestService_ACancelledScanIsRecordedWithAContextThatSurvives(t *testing.T) {
	s, _, files := newService(t, "unraid-7x-xfs-single-parity")
	var p string
	if err := s.StartScan(context.Background(), bytes.NewReader(zipOf(t, files, false)), ScanOptions{}, func(_ context.Context, got string) (string, error) {
		p = got
		return "job-1", nil
	}); err != nil {
		t.Fatal(err)
	}
	// The job is cancelled while it scans, as maintenance mode does.
	ctx, cancel := context.WithCancel(context.Background())
	s.Scanner = scanner(cancelsOnList{FakeProvider: s.Scanner.Disks.(*disk.FakeProvider), cancel: cancel})
	if err := s.RunScan(ctx, io.Discard, p); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunScan = %v, want context.Canceled", err)
	}
	if st, _ := s.State(ctx0); st.Phase != PhaseScanFailed {
		t.Errorf("phase = %s, want scan_failed", st.Phase)
	}
}

func TestService_RecoverMarksAnInterruptedScanFailed(t *testing.T) {
	s, _, files := newService(t, "unraid-7x-xfs-single-parity")
	if err := s.StartScan(context.Background(), bytes.NewReader(zipOf(t, files, false)), ScanOptions{}, func(context.Context, string) (string, error) { return "job-1", nil }); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir, "upload-orphan.zip"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Recover(ctx0); err != nil {
		t.Fatal(err)
	}
	st, _ := s.State(ctx0)
	if st.Phase != PhaseScanFailed || !strings.Contains(st.ScanError, "restart") {
		t.Errorf("State = %+v", st)
	}
	if names := dirEntries(t, s.Dir); len(names) != 1 {
		t.Errorf("directory = %v, want the interrupted upload only", names)
	}
}

func TestService_RunScanRefusesAReplacedUpload(t *testing.T) {
	s, _, _ := newService(t, "unraid-7x-xfs-single-parity")
	if err := s.ensureDir(); err != nil {
		t.Fatal(err)
	}
	if err := s.RunScan(context.Background(), io.Discard, "upload-gone.zip"); err == nil {
		t.Error("a scan of an upload the session does not name ran")
	}
}

func TestService_ForgetDeletesTheSessionAndEveryZip(t *testing.T) {
	s, _, files := newService(t, "unraid-7x-xfs-single-parity")
	if err := scanNow(s, zipOf(t, files, false), ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Forget(ctx0); err != nil {
		t.Fatal(err)
	}
	if names := dirEntries(t, s.Dir); len(names) != 0 {
		t.Errorf("Forget left %v behind", names)
	}
	if st, _ := s.State(ctx0); st.Phase != PhaseNone || st.Report != nil {
		t.Errorf("State after Forget = %+v", st)
	}
	if err := s.Forget(ctx0); err != nil {
		t.Errorf("Forget of nothing = %v, want success", err)
	}
}

// The report never quotes a file's content: a secret planted in files the
// scan reads does not reach the document.
func TestService_TheReportNeverQuotesFileContent(t *testing.T) {
	s, _, files := newService(t, "unraid-7x-xfs-single-parity")
	files["config/shadow"] = []byte("root:$6$SECRETHASH$abcdef:19000:0:99999:7:::\n")
	files["config/ident.cfg"] = []byte("NAME=\"tower\"\nPASSWORD=\"hunter2-SECRET\"\n")
	files["config/network.cfg"] = []byte("SECRETKEY=\"wg-private-SECRET\"\n")
	files["config/plugins/user.scripts/customSchedule.cron"] = append(files["config/plugins/user.scripts/customSchedule.cron"],
		[]byte("0 * * * * curl -fsS -u admin:cron-SECRET https://hc-ping.example/ping\n")...)
	if err := scanNow(s, zipOf(t, files, false), ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	md, _ := s.ReportMarkdown(ctx0)
	row, _, _ := s.Sessions.Get(ctx0)
	if strings.Contains(md, "SECRET") || strings.Contains(string(row.Report), "SECRET") {
		t.Error("a secret from the zip reached the report")
	}
}

// failWrites makes every write of the session row fail, as a full disk or a
// locked database would, while reads keep working.
func failWrites(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, trigger := range []string{
		`CREATE TRIGGER no_insert BEFORE INSERT ON migration_session BEGIN SELECT RAISE(ABORT, 'disk full'); END`,
		`CREATE TRIGGER no_update BEFORE UPDATE ON migration_session BEGIN SELECT RAISE(ABORT, 'disk full'); END`,
	} {
		if _, err := db.Exec(trigger); err != nil {
			t.Fatal(err)
		}
	}
}

func TestService_AFailedRecordQueuesNothingAndKeepsNothing(t *testing.T) {
	files, spec := flashTree(t, "unraid-7x-xfs-single-parity")
	sessions, db := newSessionsDB(t)
	s := &Service{Dir: filepath.Join(t.TempDir(), "migrate"), Scanner: scanner(fixtureDisks(spec)), Sessions: sessions}
	data := zipOf(t, files, false)
	if err := scanNow(s, data, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.State(ctx0)
	failWrites(t, db)

	submitted := false
	err := s.StartScan(ctx0, bytes.NewReader(data), ScanOptions{}, func(context.Context, string) (string, error) { submitted = true; return "job-1", nil })
	if err == nil || !strings.Contains(err.Error(), "disk full") || submitted {
		t.Fatalf("StartScan = %v (submitted %v), want the write failure and no job", err, submitted)
	}
	if names := dirEntries(t, s.Dir); len(names) != 1 || names[0] != before.Source.File {
		t.Errorf("directory = %v, want only the first scan's zip", names)
	}
}

// The zip a scan replaces is deleted only after the row naming the new one is
// saved: when the save fails the old source and its report stay.
func TestService_AFailedCommitKeepsThePreviousSourceZip(t *testing.T) {
	files, spec := flashTree(t, "unraid-7x-xfs-single-parity")
	sessions, db := newSessionsDB(t)
	s := &Service{Dir: filepath.Join(t.TempDir(), "migrate"), Scanner: scanner(fixtureDisks(spec)), Sessions: sessions}
	data := zipOf(t, files, false)
	if err := scanNow(s, data, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	first, _ := s.State(ctx0)

	var upload string
	if err := s.StartScan(ctx0, bytes.NewReader(data), ScanOptions{}, func(_ context.Context, u string) (string, error) { upload = u; return "job-1", nil }); err != nil {
		t.Fatal(err)
	}
	failWrites(t, db)
	if err := s.RunScan(ctx0, io.Discard, upload); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("RunScan = %v, want the write failure", err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, first.Source.File)); err != nil {
		t.Errorf("the previous source zip was deleted although the new report was not saved: %v", err)
	}
	if got, _ := s.State(ctx0); got.Source.File != first.Source.File || got.Report == nil {
		t.Errorf("State = %+v, want the first source and report", got)
	}
}

// A row restored from another installation's archive can name zips this one
// does not have: the source is forgotten at start, and a scan whose upload is
// gone reads as failed rather than running forever.
func TestService_RowsNamingMissingZipsAreRepaired(t *testing.T) {
	s, _, files := newService(t, "unraid-7x-xfs-single-parity")
	data := zipOf(t, files, false)
	if err := scanNow(s, data, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	st, _ := s.State(ctx0)
	if err := os.Remove(filepath.Join(s.Dir, st.Source.File)); err != nil {
		t.Fatal(err)
	}
	if err := s.Recover(ctx0); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.State(ctx0); got.Source != nil || got.Report == nil || got.Phase != PhaseScanned {
		t.Errorf("State after Recover = %+v, want the report kept and no source", got)
	}

	row, _, _ := s.Sessions.Get(ctx0)
	row.ScanFile, row.ScanSize = "upload-notthere.zip", 1
	if err := s.Sessions.Put(ctx0, row); err != nil {
		t.Fatal(err)
	}
	got, _ := s.State(ctx0)
	if got.Phase != PhaseScanFailed || got.ScanError == "" {
		t.Errorf("a scan whose upload is gone = %+v, want scan_failed", got)
	}
	if err := scanNow(s, data, ScanOptions{}); err != nil {
		t.Errorf("a new scan after that: %v", err)
	}
}

func TestService_TheRowNamesTheStoredZip(t *testing.T) {
	s, _, files := newService(t, "unraid-7x-xfs-single-parity")
	if err := scanNow(s, zipOf(t, files, false), ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	row, found, err := s.Sessions.Get(ctx0)
	names := dirEntries(t, s.Dir)
	if err != nil || !found || len(names) != 1 || row.SourceFile != names[0] || row.ScanFile != "" || len(row.Report) == 0 {
		t.Fatalf("row = %+v, %v, %v; directory %v", row, found, err, names)
	}
	if err := s.Forget(ctx0); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.Sessions.Get(ctx0); found {
		t.Error("Forget left the row")
	}
}
