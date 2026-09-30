package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	ht "github.com/ogen-go/ogen/http"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// newImportTestHandler builds a Handler wired the way cmd/hoservad wires
// it for config export/import (#269): a real, WAL-mode SQLite database
// through store.DSN — never the plain, non-WAL DSN internal/api's other
// tests use — because the defect this issue fixes is specific to a
// WAL-mode connection pool staying open across the restore.
func newImportTestHandler(t *testing.T) (*Handler, *sql.DB, string) {
	t.Helper()
	h, _, db, dbPath := newImportTestHandlerWithRegistry(t)
	return h, db, dbPath
}

// newImportTestHandlerWithRegistry is newImportTestHandler with its job
// registry also returned, so a test can register a job type and call
// Scheduler.Submit directly against the very same scheduler ImportConfig
// holds (#402), rather than a second, throwaway scheduler that
// ImportConfig's own BeginDatabaseRestore hold could never affect.
func newImportTestHandlerWithRegistry(t *testing.T) (*Handler, *job.Registry, *sql.DB, string) {
	t.Helper()
	ctx := context.Background()
	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "import-test.db")
	db, err := sql.Open("sqlite", store.DSN(dbPath))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(ctx); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	seedMachineKeyCheck(t, db, []byte("installation-a-check-value"))
	// An installation with no array takes the bare-metal branch, so the
	// in-place tests start from one that has an array.
	seedDisk(t, db, seededDisk{role: "cache", index: 1, uuid: "uuid-c1", wwn: "wwn-c1"})

	jobStore := job.NewStore(db)
	logs := job.NewLogStore(t.TempDir())
	registry := job.NewRegistry()
	scheduler := job.NewScheduler(jobStore, logs, job.NewHub(), registry)

	h := &Handler{
		Scheduler: scheduler,
		Store:     jobStore,
		Logs:      logs,
		Backup: &backup.Service{
			DB:    db,
			Paths: backup.Paths{DBPath: dbPath},
		},
		RegenerateConfig: func(context.Context) error { return nil },
	}
	return h, registry, db, dbPath
}

func exportBytes(t *testing.T, h *Handler) []byte {
	t.Helper()
	exported, err := h.ExportConfig(context.Background())
	if err != nil {
		t.Fatalf("ExportConfig: %v", err)
	}
	data, err := io.ReadAll(exported.Data)
	if err != nil {
		t.Fatalf("reading exported archive: %v", err)
	}
	if c, ok := exported.Data.(io.Closer); ok {
		_ = c.Close()
	}
	return data
}

func importReq(archive []byte) *apiv1.ImportConfigReq {
	return &apiv1.ImportConfigReq{
		Confirm: true,
		Archive: ht.MultipartFile{File: bytes.NewReader(archive)},
	}
}

// TestImportConfig_ReplacesRunningDatabaseWithoutCorruption is #269's own
// safety-critical regression test, reproducing the reported corruption
// end to end through the real handler and confirming the fix:
//
//  1. A WAL-mode database with the real migrations applied.
//  2. Rows left un-checkpointed in the WAL (no checkpoint anywhere in
//     this test, so every write below stays there through the restore).
//  3. Export.
//  4. Mutate: delete the exported share, insert an interim user.
//  5. Import the earlier export.
//  6. One more write on the very same pool.
//  7. Close and reopen fresh (a daemon restart) and check
//     PRAGMA integrity_check and content.
//
// Against the deleted copyFileAtomic file-rename path, this test failed:
// ImportConfig returned success, but the live pool — and the file itself
// once closed and reopened — kept the interim mutation (the delete and
// the insert) instead of the exported snapshot, because copyFileAtomic's
// rename left every already-open WAL-mode connection's *-wal/*-shm
// sidecars paired with data they never actually described (see
// backup.RestoreDatabase's own doc comment). backup.RestoreDatabase (the
// SQLite online backup API) fixes it: every step below passes.
func TestImportConfig_ReplacesRunningDatabaseWithoutCorruption(t *testing.T) {
	ctx := context.Background()
	h, db, dbPath := newImportTestHandler(t)

	shares := store.NewShareStore(db)
	now := time.Now().UTC()
	if err := shares.Insert(ctx, store.Share{
		Name:          "media",
		CacheMode:     "array-only",
		CreatePolicy:  "mfs",
		SMBEnabled:    true,
		SMBBrowseable: true,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("seeding share: %v", err)
	}

	archive := exportBytes(t, h)

	if err := shares.Delete(ctx, "media"); err != nil {
		t.Fatalf("deleting share (interim mutation): %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO users (id, username, password_hash, role, totp_last_step, created_at) VALUES (?, ?, 'x', 'admin', 0, ?)`,
		"interim", "interim-admin", now.Format(time.RFC3339)); err != nil {
		t.Fatalf("inserting interim user: %v", err)
	}

	if _, err := h.ImportConfig(ctx, importReq(archive)); err != nil {
		t.Fatalf("ImportConfig: %v", err)
	}

	// The very same pool must accept a normal write right after import —
	// a pool left desynced by a file-rename-under-open-connections restore
	// can refuse this (a stale view still enforcing constraints against
	// rows the import was supposed to remove).
	if _, err := db.ExecContext(ctx,
		`INSERT INTO users (id, username, password_hash, role, totp_last_step, created_at) VALUES (?, ?, 'x', 'viewer', 0, ?)`,
		"post", "post-import", now.Format(time.RFC3339)); err != nil {
		t.Fatalf("write on the live pool right after import: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("closing pool: %v", err)
	}
	fresh, err := sql.Open("sqlite", store.DSN(dbPath))
	if err != nil {
		t.Fatalf("reopening database: %v", err)
	}
	defer func() { _ = fresh.Close() }()

	var check string
	if err := fresh.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&check); err != nil {
		t.Fatalf("integrity_check after restart: %v", err)
	}
	if check != "ok" {
		t.Fatalf("integrity_check after restart = %q, want ok", check)
	}

	freshShares := store.NewShareStore(fresh)
	list, err := freshShares.List(ctx)
	if err != nil {
		t.Fatalf("listing shares after restart: %v", err)
	}
	if len(list) != 1 || list[0].Name != "media" {
		t.Fatalf("shares after restart = %+v, want just [media] (the exported state, not the interim delete)", list)
	}

	var interimCount int
	if err := fresh.QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE id = 'interim'").Scan(&interimCount); err != nil {
		t.Fatalf("querying interim user after restart: %v", err)
	}
	if interimCount != 0 {
		t.Fatal("the interim user survived the import — the running database was never actually replaced")
	}
	var postCount int
	if err := fresh.QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE id = 'post'").Scan(&postCount); err != nil {
		t.Fatalf("querying post-import user after restart: %v", err)
	}
	if postCount != 1 {
		t.Fatal("the write made on the live pool right after import did not survive to a fresh reopen")
	}
}

// TestImportConfig_RefusesWhileJobActive is doc 01 §4's own exclusion
// applied to import: restoring the database would rewrite the jobs table
// and relocation/mover state underneath a job that is still running or
// queued.
func TestImportConfig_RefusesWhileJobActive(t *testing.T) {
	ctx := context.Background()
	h, db, _ := newImportTestHandler(t)
	archive := exportBytes(t, h)

	jobStore := job.NewStore(db)
	if err := jobStore.Create(ctx, &job.Job{
		ID:        uuid.NewString(),
		Type:      job.TypeSync,
		Class:     job.ClassParity,
		Status:    job.StatusRunning,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seeding an active job: %v", err)
	}

	_, err := h.ImportConfig(ctx, importReq(archive))
	ae, ok := err.(*apiError)
	if !ok {
		t.Fatalf("ImportConfig err = %v (%T), want *apiError", err, err)
	}
	if ae.statusCode != 409 || ae.code != "job_in_progress" {
		t.Fatalf("ImportConfig err = (%d, %q), want (409, job_in_progress)", ae.statusCode, ae.code)
	}
}

// TestImportConfig_RefusesJobSubmittedDuringPreImportBackup is #269's own
// regression test for its second review round: the active-jobs check must
// be the very last thing before backup.RestoreDatabase, not the first
// thing ImportConfig does. A job submitted any time before that point —
// including during the upload, the two verification passes, or the
// pre-import safety backup (h.Backup.RunReason) itself — must still be
// refused, never silently orphaned by a restore that overwrites the jobs
// table out from under it. h.Backup.Now, called exactly once by
// RunReason, stands in for that submission landing mid-backup.
func TestImportConfig_RefusesJobSubmittedDuringPreImportBackup(t *testing.T) {
	ctx := context.Background()
	h, db, _ := newImportTestHandler(t)

	shares := store.NewShareStore(db)
	now := time.Now().UTC()
	if err := shares.Insert(ctx, store.Share{
		Name:          "media",
		CacheMode:     "array-only",
		CreatePolicy:  "mfs",
		SMBEnabled:    true,
		SMBBrowseable: true,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("seeding share: %v", err)
	}
	archive := exportBytes(t, h)

	if err := shares.Delete(ctx, "media"); err != nil {
		t.Fatalf("deleting share (interim mutation): %v", err)
	}

	jobStore := job.NewStore(db)
	h.Backup.Now = func() time.Time {
		if err := jobStore.Create(ctx, &job.Job{
			ID:        uuid.NewString(),
			Type:      job.TypeSync,
			Class:     job.ClassParity,
			Status:    job.StatusRunning,
			CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seeding a job during the pre-import backup: %v", err)
		}
		return time.Now().UTC()
	}

	_, err := h.ImportConfig(ctx, importReq(archive))
	ae, ok := err.(*apiError)
	if !ok {
		t.Fatalf("ImportConfig err = %v (%T), want *apiError", err, err)
	}
	if ae.statusCode != 409 || ae.code != "job_in_progress" {
		t.Fatalf("ImportConfig err = (%d, %q), want (409, job_in_progress)", ae.statusCode, ae.code)
	}

	list, err := shares.List(ctx)
	if err != nil {
		t.Fatalf("listing shares after refused import: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("shares after refused import = %+v, want none (the interim delete, not the export the refused restore never applied)", list)
	}
}

// TestImportConfig_MarksPreImportSafetyBackup proves #401's own wiring:
// ImportConfig's safety backup reaches h.Backup.RunReason with
// backup.ReasonPreImport, not the unmarked Run a second same-day import
// would otherwise let retention prune by taking today's daily-tier slot.
func TestImportConfig_MarksPreImportSafetyBackup(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newImportTestHandler(t)
	destDir := t.TempDir()
	h.Backup.Destinations = []backup.Destination{{
		ID:      "local",
		Path:    destDir,
		Enabled: true,
		Retention: backup.Retention{
			Daily:   backup.DefaultRetentionDaily,
			Weekly:  backup.DefaultRetentionWeekly,
			Monthly: backup.DefaultRetentionMonthly,
		},
	}}

	archive := exportBytes(t, h)
	if _, err := h.ImportConfig(ctx, importReq(archive)); err != nil {
		t.Fatalf("ImportConfig: %v", err)
	}

	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatalf("reading destination: %v", err)
	}
	var found bool
	for _, e := range entries {
		if !e.IsDir() && strings.Contains(e.Name(), ".pre-import.") {
			found = true
		}
	}
	if !found {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("expected an archive marked pre-import in %v, found none", names)
	}
}

// TestImportConfig_RestoredRunningJobIsInterrupted is #269's own
// regression test for its second review round: an archive's jobs table
// can carry a row that was queued or running at export time, but nothing
// on the importing daemon is actually running it. Left as restored, that
// row refuses every later import with job_in_progress forever, since
// nothing ever finishes it. The restore must mark it interrupted the same
// way a daemon restart does.
func TestImportConfig_RestoredRunningJobIsInterrupted(t *testing.T) {
	ctx := context.Background()
	h, db, _ := newImportTestHandler(t)

	jobStore := job.NewStore(db)
	jobID := uuid.NewString()
	if err := jobStore.Create(ctx, &job.Job{
		ID:        jobID,
		Type:      job.TypeSync,
		Class:     job.ClassParity,
		Status:    job.StatusRunning,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seeding a running job to export: %v", err)
	}

	archive := exportBytes(t, h)

	// Finish the job on the live daemon so the check in ImportConfig
	// itself does not refuse this import — what is under test is the
	// running row the archive still carries, not this one.
	if err := jobStore.UpdateStatus(ctx, jobID, job.StatusSucceeded, nil, "", "", nil, nil); err != nil {
		t.Fatalf("finishing the seeded job before import: %v", err)
	}

	if _, err := h.ImportConfig(ctx, importReq(archive)); err != nil {
		t.Fatalf("ImportConfig: %v", err)
	}

	restored, err := jobStore.Get(ctx, jobID)
	if err != nil {
		t.Fatalf("getting restored job: %v", err)
	}
	if restored.Status != job.StatusInterrupted {
		t.Fatalf("restored job status = %q, want %q", restored.Status, job.StatusInterrupted)
	}

	if _, err := h.ImportConfig(ctx, importReq(archive)); err != nil {
		t.Fatalf("second ImportConfig, refused by the first import's own restored job: %v", err)
	}
}

// TestImportConfig_ConcurrentSubmitDuringRestoreIsRefused is #402's own
// regression test for the scheduler admission hold: a job submitted while
// ImportConfig's whole-database restore is in flight must be refused —
// never silently admitted underneath a restore that is about to overwrite
// the jobs table wholesale. importPostRestoreHookForTest lands the
// concurrent Submit deterministically inside the hold's own window
// (between backup.RestoreDatabase returning and the hold's release),
// instead of racing the real clock.
func TestImportConfig_ConcurrentSubmitDuringRestoreIsRefused(t *testing.T) {
	ctx := context.Background()
	h, registry, _, _ := newImportTestHandlerWithRegistry(t)
	archive := exportBytes(t, h)

	registry.Register(job.TypeSync, true, func(ctx context.Context, rc *job.RunContext) error { return nil })

	inRestore := make(chan struct{})
	proceed := make(chan struct{})
	importPostRestoreHookForTest = func() {
		close(inRestore)
		<-proceed
	}
	t.Cleanup(func() { importPostRestoreHookForTest = nil })

	importErrCh := make(chan error, 1)
	go func() {
		_, err := h.ImportConfig(ctx, importReq(archive))
		importErrCh <- err
	}()

	<-inRestore
	_, submitErr := h.Scheduler.Submit(ctx, job.TypeSync, nil, nil)
	close(proceed)

	if err := <-importErrCh; err != nil {
		t.Fatalf("ImportConfig: %v", err)
	}
	if !errors.Is(submitErr, job.ErrDatabaseRestoreInProgress) {
		t.Fatalf("Submit racing the restore's own hold = %v, want ErrDatabaseRestoreInProgress", submitErr)
	}
}

// TestImportConfig_LeavesJobInsertedDuringRestoreWindowUntouched is
// #402's own regression test for the "mirror image" race #269's verifier
// also reported: the post-restore interrupt step must mark interrupted
// only the ids that were queued or running in the archive it restored —
// never every queued/running row the live table happens to hold once the
// restore has finished. importPostRestoreHookForTest inserts a job
// directly into the live database, deterministically inside that exact
// window, standing in for an insert whose own Store.Create call happened
// to land there (#402's own reported InterruptActiveJobs race).
func TestImportConfig_LeavesJobInsertedDuringRestoreWindowUntouched(t *testing.T) {
	ctx := context.Background()
	h, db, _ := newImportTestHandler(t)

	jobStore := job.NewStore(db)
	archivedID := uuid.NewString()
	if err := jobStore.Create(ctx, &job.Job{
		ID:        archivedID,
		Type:      job.TypeSync,
		Class:     job.ClassParity,
		Status:    job.StatusRunning,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seeding a running job to export: %v", err)
	}
	archive := exportBytes(t, h)

	// Finish it on the live daemon so ImportConfig's own active-jobs
	// check does not refuse this import — what is under test is the
	// running row the archive still carries, and the unrelated row
	// inserted below.
	if err := jobStore.UpdateStatus(ctx, archivedID, job.StatusSucceeded, nil, "", "", nil, nil); err != nil {
		t.Fatalf("finishing the seeded job before import: %v", err)
	}

	outsideID := uuid.NewString()
	importPostRestoreHookForTest = func() {
		if err := jobStore.Create(ctx, &job.Job{
			ID:        outsideID,
			Type:      job.TypeScrub,
			Class:     job.ClassParity,
			Status:    job.StatusRunning,
			CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("inserting a job outside the restored archive: %v", err)
		}
	}
	t.Cleanup(func() { importPostRestoreHookForTest = nil })

	if _, err := h.ImportConfig(ctx, importReq(archive)); err != nil {
		t.Fatalf("ImportConfig: %v", err)
	}

	archived, err := jobStore.Get(ctx, archivedID)
	if err != nil {
		t.Fatalf("getting the archived job: %v", err)
	}
	if archived.Status != job.StatusInterrupted {
		t.Fatalf("archived job status = %q, want %q", archived.Status, job.StatusInterrupted)
	}

	outside, err := jobStore.Get(ctx, outsideID)
	if err != nil {
		t.Fatalf("getting the job inserted outside the archive: %v", err)
	}
	if outside.Status != job.StatusRunning {
		t.Fatalf("job inserted during the restore window, never part of the archive, status = %q, want it left untouched at %q", outside.Status, job.StatusRunning)
	}
}

// TestImportConfig_TruncatedArchiveIsRejected covers a body that isn't a
// valid tar.zst stream at all.
func TestImportConfig_TruncatedArchiveIsRejected(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newImportTestHandler(t)
	archive := exportBytes(t, h)
	truncated := archive[:len(archive)/2]

	_, err := h.ImportConfig(ctx, importReq(truncated))
	assertInvalidArchive(t, err)
}

// TestImportConfig_ChecksumMismatchIsRejected covers an archive whose
// state.db bytes were changed after its manifest checksum was computed.
func TestImportConfig_ChecksumMismatchIsRejected(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newImportTestHandler(t)
	archive := exportBytes(t, h)

	staging := extractArchive(t, archive)
	if err := tamperStateDB(t, filepath.Join(staging, "state.db")); err != nil {
		t.Fatalf("tampering state.db: %v", err)
	}
	tampered := repackTarZst(t, staging)

	_, err := h.ImportConfig(ctx, importReq(tampered))
	assertInvalidArchive(t, err)
}

// TestImportConfig_MissingStateDBIsRejected covers an archive whose
// state.db was stripped out after packing.
func TestImportConfig_MissingStateDBIsRejected(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newImportTestHandler(t)
	archive := exportBytes(t, h)

	staging := extractArchive(t, archive)
	if err := os.Remove(filepath.Join(staging, "state.db")); err != nil {
		t.Fatalf("removing state.db: %v", err)
	}
	stripped := repackTarZst(t, staging)

	_, err := h.ImportConfig(ctx, importReq(stripped))
	assertInvalidArchive(t, err)
}

// TestImportConfig_OlderSchemaVersionIsIncompatible and
// TestImportConfig_NewerSchemaVersionIsIncompatible cover a version
// mismatch in either direction — both are refused as incompatible_archive
// rather than restored (#269's own acceptance: "refuses any version
// mismatch"; upgrading an older archive is #276).
func TestImportConfig_OlderSchemaVersionIsIncompatible(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newImportTestHandler(t)
	archive := exportBytes(t, h)

	staging := extractArchive(t, archive)
	stateDBPath := filepath.Join(staging, "state.db")
	adb, err := sql.Open("sqlite", stateDBPath)
	if err != nil {
		t.Fatalf("opening staged state.db: %v", err)
	}
	var maxVersion string
	if err := adb.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&maxVersion); err != nil {
		t.Fatalf("reading max applied version: %v", err)
	}
	if _, err := adb.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version = ?", maxVersion); err != nil {
		t.Fatalf("deleting the newest migration row: %v", err)
	}
	if err := adb.Close(); err != nil {
		t.Fatalf("closing staged state.db: %v", err)
	}
	resyncManifestChecksum(t, staging)
	older := repackTarZst(t, staging)

	_, err = h.ImportConfig(ctx, importReq(older))
	assertIncompatibleArchive(t, err)
}

func TestImportConfig_NewerSchemaVersionIsIncompatible(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newImportTestHandler(t)
	archive := exportBytes(t, h)

	staging := extractArchive(t, archive)
	stateDBPath := filepath.Join(staging, "state.db")
	adb, err := sql.Open("sqlite", stateDBPath)
	if err != nil {
		t.Fatalf("opening staged state.db: %v", err)
	}
	var maxVersion string
	if err := adb.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&maxVersion); err != nil {
		t.Fatalf("reading max applied version: %v", err)
	}
	var n int64
	if _, err := fmt.Sscanf(maxVersion, "%d", &n); err != nil {
		t.Fatalf("parsing max applied version %q: %v", maxVersion, err)
	}
	newer := fmt.Sprintf("%014d", n+1)
	if _, err := adb.ExecContext(ctx,
		"INSERT INTO schema_migrations (version, slug, checksum, applied_at) VALUES (?, 'spike-future-migration', 'x', ?)",
		newer, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("inserting a fabricated future migration row: %v", err)
	}
	if err := adb.Close(); err != nil {
		t.Fatalf("closing staged state.db: %v", err)
	}
	resyncManifestChecksum(t, staging)
	fromTheFuture := repackTarZst(t, staging)

	_, err = h.ImportConfig(ctx, importReq(fromTheFuture))
	assertIncompatibleArchive(t, err)
}

// TestImportConfig_SecretsAgePresentButNoPassphraseStillImports is #269's
// own requirement: config import restores the database only (#62
// restores the rest), so an archive that happens to carry secrets.age
// must not be rejected for lack of a passphrase the import path never
// asks for.
func TestImportConfig_SecretsAgePresentButNoPassphraseStillImports(t *testing.T) {
	ctx := context.Background()
	h, db, _ := newImportTestHandler(t)

	src := &backup.FakeSecretSource{
		Passphrase: "unused-by-import",
		HasPass:    true,
		Secrets: []backup.DatabaseSecret{{
			Table:      "notify_channels",
			Column:     "secret",
			RowID:      "ch1",
			Ciphertext: []byte{0x5a},
		}},
	}
	staging := t.TempDir()
	if _, err := backup.BuildArchive(ctx, db, backup.Paths{}, src, backup.FakeSecretCipher{}, "host", "test", time.Now(), staging); err != nil {
		t.Fatalf("BuildArchive: %v", err)
	}
	archivePath := filepath.Join(t.TempDir(), "with-secrets.tar.zst")
	if err := packTarZst(staging, archivePath); err != nil {
		t.Fatalf("packTarZst: %v", err)
	}
	archive, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatalf("reading packed archive: %v", err)
	}

	if _, err := h.ImportConfig(ctx, importReq(archive)); err != nil {
		t.Fatalf("ImportConfig(archive with secrets.age, no passphrase given anywhere): %v", err)
	}
}

// TestExportConfig_EmbedsIdentityAgeDecryptableWithPassphrase proves
// criterion 3 ("the private identity is embedded in every archive") holds
// through ExportConfig (POST /config/export), the on-demand path. The
// nightly config-backup chain (Service.Run) is proven the same way by
// internal/backup's own
// TestService_RunEmbedsIdentityAgeInArchiveWrittenToDestination, which
// unpacks the archive Run actually writes to a destination rather than
// BuildArchive's output in isolation. A BuildArchive call that dropped
// WithRecipient, or ExportConfig calling it without a recipient, would
// leave identity.age missing and fail the os.ReadFile below before the
// decrypt is ever reached.
func TestExportConfig_EmbedsIdentityAgeDecryptableWithPassphrase(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newImportTestHandler(t)

	recipient, err := backup.LoadOrGenerateRecipient(ctx, backup.FakeSecretCipher{}, &backup.FakeRecipientStore{}, nil)
	if err != nil {
		t.Fatalf("LoadOrGenerateRecipient: %v", err)
	}
	h.Backup.Recipient = recipient
	h.Backup.Secrets = &backup.FakeSecretSource{Passphrase: "export-pass", HasPass: true}

	archive := exportBytes(t, h)

	staging := extractArchive(t, archive)
	identityAge, err := os.ReadFile(filepath.Join(staging, "identity.age"))
	if err != nil {
		t.Fatalf("exported archive has no identity.age: %v", err)
	}

	scryptIdentity, err := age.NewScryptIdentity("export-pass")
	if err != nil {
		t.Fatalf("creating scrypt identity: %v", err)
	}
	r, err := age.Decrypt(bytes.NewReader(identityAge), scryptIdentity)
	if err != nil {
		t.Fatalf("decrypting identity.age with the backup passphrase: %v", err)
	}
	plain, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading decrypted identity.age: %v", err)
	}
	if string(plain) != recipient.Identity {
		t.Fatalf("decrypted identity.age = %q, want %q", plain, recipient.Identity)
	}
}

func assertInvalidArchive(t *testing.T, err error) {
	t.Helper()
	ae, ok := err.(*apiError)
	if !ok {
		t.Fatalf("err = %v (%T), want *apiError", err, err)
	}
	if ae.statusCode != 400 || ae.code != "invalid_archive" {
		t.Fatalf("err = (%d, %q), want (400, invalid_archive)", ae.statusCode, ae.code)
	}
}

func assertIncompatibleArchive(t *testing.T, err error) {
	t.Helper()
	ae, ok := err.(*apiError)
	if !ok {
		t.Fatalf("err = %v (%T), want *apiError", err, err)
	}
	if ae.statusCode != 400 || ae.code != "incompatible_archive" {
		t.Fatalf("err = (%d, %q), want (400, incompatible_archive)", ae.statusCode, ae.code)
	}
}

// extractArchive unpacks a valid archive into a fresh directory the test
// then edits and repacks, through the same extraction ImportConfig verifies
// and restores from.
func extractArchive(t *testing.T, archive []byte) string {
	t.Helper()
	tree, err := backup.ExtractVerifiedArchive(writeTemp(t, archive))
	if err != nil {
		t.Fatalf("unpacking baseline archive: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tree) })
	return tree
}

func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "baseline.tar.zst")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing temp archive: %v", err)
	}
	return path
}

func repackTarZst(t *testing.T, dir string) []byte {
	t.Helper()
	archivePath := filepath.Join(t.TempDir(), "repacked.tar.zst")
	if err := packTarZst(dir, archivePath); err != nil {
		t.Fatalf("repacking archive: %v", err)
	}
	data, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatalf("reading repacked archive: %v", err)
	}
	return data
}

// resyncManifestChecksum recomputes manifest.json's own recorded sha256
// for state.db against dir's current bytes — used only by the schema-
// version tests above, which deliberately edit the staged state.db
// in place and must isolate the version mismatch from an incidental
// checksum mismatch that edit would otherwise also trigger.
func resyncManifestChecksum(t *testing.T, dir string) {
	t.Helper()
	manifestPath := filepath.Join(dir, "manifest.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("reading manifest.json: %v", err)
	}
	var m backup.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parsing manifest.json: %v", err)
	}
	f, err := os.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("opening state.db to rehash: %v", err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hashing state.db: %v", err)
	}
	m.Checksums["state.db"] = hex.EncodeToString(h.Sum(nil))
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("encoding manifest.json: %v", err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(manifestPath, out, 0o600); err != nil {
		t.Fatalf("writing manifest.json: %v", err)
	}
}

// tamperStateDB changes state.db's content after BuildArchive's manifest
// checksum was already computed over the original bytes, without touching
// the manifest — the checksum-mismatch scenario.
func tamperStateDB(t *testing.T, path string) error {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	_, err = db.Exec("UPDATE schema_migrations SET slug = 'tampered' WHERE version = (SELECT MAX(version) FROM schema_migrations)")
	return err
}

func seedMachineKeyCheck(t *testing.T, db *sql.DB, value []byte) {
	t.Helper()
	if _, err := db.Exec(`INSERT OR REPLACE INTO machine_key_check (id, check_value, created_at) VALUES (1, ?, ?)`,
		value, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("seeding machine_key_check: %v", err)
	}
}

type seededDisk struct {
	role    string
	index   int
	uuid    string
	wwn     string
	removal string
}

func seedDisk(t *testing.T, db *sql.DB, d seededDisk) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO array_disks
		(role, role_index, device, filesystem, fs_uuid, wwn, serial, weak_identity, mountpoint, removal_state)
		VALUES (?, ?, ?, 'xfs', ?, ?, ?, 0, ?, NULLIF(?, ''))`,
		d.role, d.index, fmt.Sprintf("/dev/%s%d", d.role, d.index), d.uuid, d.wwn, "serial-"+d.uuid,
		fmt.Sprintf("/mnt/%s%d", d.role, d.index), d.removal); err != nil {
		t.Fatalf("seeding array_disks %s %d: %v", d.role, d.index, err)
	}
}

func seedEvacuation(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, p := range []string{"movies/a.mkv", "movies/b.mkv"} {
		if _, err := db.Exec(`INSERT INTO relocation_manifest (rel_path, size, mtime, source_disk, target_disk)
			VALUES (?, 100, '2026-09-01T00:00:00Z', '/mnt/data2', '/mnt/data1')`, p); err != nil {
			t.Fatalf("seeding relocation_manifest: %v", err)
		}
	}
	if _, err := db.Exec(`INSERT INTO relocation_removing_disks (mountpoint) VALUES ('/mnt/data2')`); err != nil {
		t.Fatalf("seeding relocation_removing_disks: %v", err)
	}
}

func seedArray(t *testing.T, db *sql.DB) {
	t.Helper()
	seedDisk(t, db, seededDisk{role: "parity", index: 1, uuid: "uuid-p1", wwn: "wwn-p1"})
	seedDisk(t, db, seededDisk{role: "data", index: 1, uuid: "uuid-d1", wwn: "wwn-d1"})
}

func insertSentinelShare(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	now := time.Now().UTC()
	if err := store.NewShareStore(db).Insert(context.Background(), store.Share{
		Name: name, CacheMode: "array-only", CreatePolicy: "mfs",
		SMBEnabled: true, SMBBrowseable: true, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("inserting share %q: %v", name, err)
	}
}

// liveFingerprint reads everything an import must leave alone when it
// refuses: the installation and array tables it compares, the jobs table
// and the shares a restore would roll back.
func liveFingerprint(t *testing.T, db *sql.DB) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{
		`SELECT check_value FROM machine_key_check`,
		`SELECT role, role_index, device, fs_uuid, IFNULL(wwn, ''), IFNULL(removal_state, '') FROM array_disks ORDER BY role, role_index`,
		`SELECT rel_path, size, mtime, source_disk, target_disk FROM relocation_manifest ORDER BY id`,
		`SELECT mountpoint FROM relocation_removing_disks ORDER BY mountpoint`,
		`SELECT name FROM shares ORDER BY name`,
		`SELECT id, status FROM jobs ORDER BY id`,
	} {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatalf("fingerprint query %q: %v", q, err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("fingerprint columns: %v", err)
		}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatalf("fingerprint scan: %v", err)
			}
			fmt.Fprintf(&b, "%s: %v\n", q[:20], vals)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("fingerprint rows: %v", err)
		}
		_ = rows.Close()
	}
	return b.String()
}

func rewriteArchive(t *testing.T, archive []byte, edit func(staging string)) []byte {
	t.Helper()
	staging := extractArchive(t, archive)
	edit(staging)
	return repackTarZst(t, staging)
}

func editArchiveDB(t *testing.T, archive []byte, edit func(db *sql.DB)) []byte {
	t.Helper()
	return rewriteArchive(t, archive, func(staging string) {
		adb, err := sql.Open("sqlite", filepath.Join(staging, "state.db"))
		if err != nil {
			t.Fatalf("opening staged state.db: %v", err)
		}
		edit(adb)
		if err := adb.Close(); err != nil {
			t.Fatalf("closing staged state.db: %v", err)
		}
		resyncManifestChecksum(t, staging)
	})
}

func execAll(t *testing.T, db *sql.DB, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// assertRefusedBeforeWriting imports archive and requires the refusal
// (status, code) with nothing written: no pre-import safety backup ran (so
// no retention slot was taken), the restore was never reached, and the live
// database reads exactly as before. RunReason reads Backup.Now once, and
// ImportConfig takes the scheduler's restore hold only after RunReason, so
// a refusal that never reaches Now never reached the hold either.
func assertRefusedBeforeWriting(t *testing.T, h *Handler, db *sql.DB, archive []byte, status int, code string, wantInMessage ...string) {
	t.Helper()
	destDir := t.TempDir()
	h.Backup.Destinations = []backup.Destination{{
		ID:      "local",
		Path:    destDir,
		Enabled: true,
		Retention: backup.Retention{
			Daily:   backup.DefaultRetentionDaily,
			Weekly:  backup.DefaultRetentionWeekly,
			Monthly: backup.DefaultRetentionMonthly,
		},
	}}
	backups := 0
	h.Backup.Now = func() time.Time {
		backups++
		return time.Now().UTC()
	}
	before := liveFingerprint(t, db)

	_, err := h.ImportConfig(context.Background(), importReq(archive))
	ae, ok := err.(*apiError)
	if !ok {
		t.Fatalf("ImportConfig err = %v (%T), want *apiError", err, err)
	}
	if ae.statusCode != status || ae.code != code {
		t.Fatalf("ImportConfig err = (%d, %q: %s), want (%d, %q)", ae.statusCode, ae.code, ae.message, status, code)
	}
	for _, want := range wantInMessage {
		if !strings.Contains(ae.message, want) {
			t.Fatalf("message %q does not contain %q", ae.message, want)
		}
	}
	if backups != 0 {
		t.Fatalf("the pre-import safety backup ran %d time(s) before the refusal", backups)
	}
	if entries, err := os.ReadDir(destDir); err != nil || len(entries) != 0 {
		t.Fatalf("destination after refusal = %v (err %v), want empty", entries, err)
	}
	if after := liveFingerprint(t, db); after != before {
		t.Fatalf("live database changed by a refused import:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	release, err := h.Scheduler.BeginDatabaseRestore(context.Background())
	if code != "job_in_progress" {
		if err != nil {
			t.Fatalf("restore hold still held after the refusal: %v", err)
		}
		release()
	}
}

// TestImportConfig_RefusesAnotherInstallationsArchive reproduces the
// data-loss scenario: a second installation's exported archive imported
// into a first one. Restored, it replaces the first installation's
// machine_key_check, so its next start fails the Q28 check. It must be
// refused and the live check value left as it was.
func TestImportConfig_RefusesAnotherInstallationsArchive(t *testing.T) {
	h, db, _ := newImportTestHandler(t)
	other, otherDB, _ := newImportTestHandler(t)
	seedMachineKeyCheck(t, otherDB, []byte("installation-b-check-value"))
	archive := exportBytes(t, other)

	insertSentinelShare(t, db, "sentinel")
	assertRefusedBeforeWriting(t, h, db, archive, 409, "archive_other_installation", "fresh install")

	var check []byte
	if err := db.QueryRow(`SELECT check_value FROM machine_key_check WHERE id = 1`).Scan(&check); err != nil {
		t.Fatalf("reading live check value: %v", err)
	}
	if string(check) != "installation-a-check-value" {
		t.Fatalf("live check value = %q, want the first installation's own", check)
	}
}

func TestImportConfig_RefusesArchiveWithoutMachineKeyCheck(t *testing.T) {
	h, db, _ := newImportTestHandler(t)
	archive := editArchiveDB(t, exportBytes(t, h), func(adb *sql.DB) {
		execAll(t, adb, `DELETE FROM machine_key_check`)
	})
	assertRefusedBeforeWriting(t, h, db, archive, 409, "archive_other_installation")
}

// A live database with no check value cannot be compared with, so the
// import is refused rather than allowed.
func TestImportConfig_RefusesWhenLiveMachineKeyCheckIsMissing(t *testing.T) {
	ctx := context.Background()
	h, db, _ := newImportTestHandler(t)
	archive := exportBytes(t, h)
	execAll(t, db, `DELETE FROM machine_key_check`)

	_, err := h.ImportConfig(ctx, importReq(archive))
	if err == nil {
		t.Fatal("ImportConfig = nil, want a refusal")
	}
	if ae, ok := err.(*apiError); ok && ae.statusCode < 400 {
		t.Fatalf("ImportConfig err = %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM machine_key_check`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("live machine_key_check rows = %d (err %v), want the live database untouched", n, err)
	}
}

// TestImportConfig_RefusesArchiveWhoseArrayDiffers covers each way an
// archive's view of the array can differ from the live one, and that
// restoring it would put the database out of step with the disks.
func TestImportConfig_RefusesArchiveWhoseArrayDiffers(t *testing.T) {
	t.Run("exported before a disk was added", func(t *testing.T) {
		h, db, _ := newImportTestHandler(t)
		seedArray(t, db)
		archive := exportBytes(t, h)
		seedDisk(t, db, seededDisk{role: "data", index: 2, uuid: "uuid-d2-added", wwn: "wwn-d2"})

		assertRefusedBeforeWriting(t, h, db, archive, 409, "archive_array_mismatch", "uuid-d2-added")
	})

	t.Run("exported during an evacuation, restored after it finished", func(t *testing.T) {
		h, db, _ := newImportTestHandler(t)
		seedArray(t, db)
		seedDisk(t, db, seededDisk{role: "data", index: 2, uuid: "uuid-d2", wwn: "wwn-d2", removal: "evacuating"})
		seedEvacuation(t, db)
		archive := exportBytes(t, h)

		execAll(t, db,
			`UPDATE array_disks SET removal_state = 'unlisted' WHERE role = 'data' AND role_index = 2`,
			`DELETE FROM relocation_manifest`,
			`DELETE FROM relocation_removing_disks`,
		)
		assertRefusedBeforeWriting(t, h, db, archive, 409, "archive_array_mismatch", "removal_state", "relocation manifest", "/mnt/data2")
	})

	t.Run("exported before the rest of the array existed", func(t *testing.T) {
		h, db, _ := newImportTestHandler(t)
		archive := exportBytes(t, h)
		seedArray(t, db)

		assertRefusedBeforeWriting(t, h, db, archive, 409, "archive_array_mismatch", "uuid-d1", "uuid-p1")
	})

	t.Run("a disk replaced by another", func(t *testing.T) {
		h, db, _ := newImportTestHandler(t)
		seedArray(t, db)
		archive := exportBytes(t, h)
		execAll(t, db, `UPDATE array_disks SET fs_uuid = 'uuid-new', wwn = 'wwn-new' WHERE role = 'data'`)

		assertRefusedBeforeWriting(t, h, db, archive, 409, "archive_array_mismatch", "uuid-d1", "uuid-new")
	})

	t.Run("a relocation manifest that differs", func(t *testing.T) {
		h, db, _ := newImportTestHandler(t)
		seedArray(t, db)
		seedEvacuation(t, db)
		archive := exportBytes(t, h)
		execAll(t, db, `UPDATE relocation_manifest SET size = 101 WHERE rel_path = 'movies/b.mkv'`)

		assertRefusedBeforeWriting(t, h, db, archive, 409, "archive_array_mismatch", "movies/b.mkv")
	})
}

// TestImportConfig_RefusesArrayChangeMadeBeforeTheRestoreHold covers a
// topology job that finishes between the first comparison and the
// restore: the comparison runs again under the hold, so the restore never
// puts the database out of step with the disks.
func TestImportConfig_RefusesArrayChangeMadeBeforeTheRestoreHold(t *testing.T) {
	ctx := context.Background()
	h, db, _ := newImportTestHandler(t)
	archive := exportBytes(t, h)
	insertSentinelShare(t, db, "sentinel")
	h.Backup.Now = func() time.Time {
		seedDisk(t, db, seededDisk{role: "data", index: 1, uuid: "uuid-late", wwn: "wwn-late"})
		return time.Now().UTC()
	}

	_, err := h.ImportConfig(ctx, importReq(archive))
	ae, ok := err.(*apiError)
	if !ok || ae.statusCode != 409 || ae.code != "archive_array_mismatch" {
		t.Fatalf("ImportConfig err = %v (%T), want 409 archive_array_mismatch", err, err)
	}
	shares, lerr := store.NewShareStore(db).List(ctx)
	if lerr != nil || len(shares) != 1 {
		t.Fatalf("shares after refusal = %v (err %v), want the sentinel the restore would have removed", shares, lerr)
	}
	release, berr := h.Scheduler.BeginDatabaseRestore(ctx)
	if berr != nil {
		t.Fatalf("restore hold still held after the refusal: %v", berr)
	}
	release()
}

// TestImportConfig_AcceptsArchiveWithIdenticalArray is the same-installation,
// same-array round trip the L3 suite runs, with an array, a relocation in
// flight and a device name that changed across a reboot: what is compared is
// the disks' identities, not the names the kernel gave them.
func TestImportConfig_AcceptsArchiveWithIdenticalArray(t *testing.T) {
	ctx := context.Background()
	h, db, _ := newImportTestHandler(t)
	seedArray(t, db)
	seedDisk(t, db, seededDisk{role: "data", index: 2, uuid: "uuid-d2", wwn: "wwn-d2", removal: "evacuating"})
	seedEvacuation(t, db)
	archive := exportBytes(t, h)

	insertSentinelShare(t, db, "rolled-back")
	execAll(t, db, `UPDATE array_disks SET device = '/dev/moved-after-reboot' WHERE role = 'data' AND role_index = 1`)

	if _, err := h.ImportConfig(ctx, importReq(archive)); err != nil {
		t.Fatalf("ImportConfig of an identical array: %v", err)
	}
	shares, err := store.NewShareStore(db).List(ctx)
	if err != nil {
		t.Fatalf("listing shares: %v", err)
	}
	if len(shares) != 0 {
		t.Fatalf("shares after import = %v, want the export's none (the restore ran)", shares)
	}
}

// TestImportConfig_EveryRefusalHappensBeforeAnythingIsWritten runs each
// refusal ImportConfig has against a live database and requires that none
// of them wrote a pre-import archive (which would take a pre-change
// retention slot, #401), took the restore hold, or changed the database.
func TestImportConfig_EveryRefusalHappensBeforeAnythingIsWritten(t *testing.T) {
	t.Run("invalid_archive: checksum mismatch", func(t *testing.T) {
		h, db, _ := newImportTestHandler(t)
		archive := rewriteArchive(t, exportBytes(t, h), func(staging string) {
			if err := tamperStateDB(t, filepath.Join(staging, "state.db")); err != nil {
				t.Fatalf("tampering state.db: %v", err)
			}
		})
		assertRefusedBeforeWriting(t, h, db, archive, 400, "invalid_archive")
	})

	t.Run("invalid_archive: unlisted file", func(t *testing.T) {
		h, db, _ := newImportTestHandler(t)
		archive := rewriteArchive(t, exportBytes(t, h), func(staging string) {
			if err := os.WriteFile(filepath.Join(staging, "extra.txt"), []byte("x"), 0o600); err != nil {
				t.Fatalf("writing extra file: %v", err)
			}
		})
		assertRefusedBeforeWriting(t, h, db, archive, 400, "invalid_archive")
	})

	t.Run("incompatible_archive", func(t *testing.T) {
		h, db, _ := newImportTestHandler(t)
		archive := editArchiveDB(t, exportBytes(t, h), func(adb *sql.DB) {
			execAll(t, adb, `DELETE FROM schema_migrations WHERE version = (SELECT MAX(version) FROM schema_migrations)`)
		})
		assertRefusedBeforeWriting(t, h, db, archive, 400, "incompatible_archive")
	})

	t.Run("job_in_progress", func(t *testing.T) {
		h, db, _ := newImportTestHandler(t)
		archive := exportBytes(t, h)
		if err := job.NewStore(db).Create(context.Background(), &job.Job{
			ID: uuid.NewString(), Type: job.TypeSync, Class: job.ClassParity,
			Status: job.StatusRunning, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seeding an active job: %v", err)
		}
		assertRefusedBeforeWriting(t, h, db, archive, 409, "job_in_progress")
	})

	t.Run("archive_other_installation", func(t *testing.T) {
		h, db, _ := newImportTestHandler(t)
		other, otherDB, _ := newImportTestHandler(t)
		seedMachineKeyCheck(t, otherDB, []byte("installation-b-check-value"))
		assertRefusedBeforeWriting(t, h, db, exportBytes(t, other), 409, "archive_other_installation")
	})

	t.Run("archive_array_mismatch", func(t *testing.T) {
		h, db, _ := newImportTestHandler(t)
		archive := exportBytes(t, h)
		seedArray(t, db)
		assertRefusedBeforeWriting(t, h, db, archive, 409, "archive_array_mismatch")
	})
}

// TestImportConfig_RefusesAnArchiveHoldingAFileTheManifestDoesNotList: a
// file the manifest does not list is never checksummed, so verification
// used to accept it.
func TestImportConfig_RefusesAnArchiveHoldingAFileTheManifestDoesNotList(t *testing.T) {
	for _, rel := range []string{
		"generated/extra.conf",
		"stacks/media/extra.yml",
		"templates/extra.json",
		"custom/extra.custom.conf",
		"extra.txt",
	} {
		t.Run(rel, func(t *testing.T) {
			h, db, _ := newImportTestHandler(t)
			archive := rewriteArchive(t, exportBytes(t, h), func(staging string) {
				p := filepath.Join(staging, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
					t.Fatalf("creating %s: %v", filepath.Dir(p), err)
				}
				if err := os.WriteFile(p, []byte("not in the manifest"), 0o600); err != nil {
					t.Fatalf("writing %s: %v", p, err)
				}
			})
			assertRefusedBeforeWriting(t, h, db, archive, 400, "invalid_archive")
		})
	}
}
