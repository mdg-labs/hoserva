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
	"testing"
	"time"

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

	if err := h.ImportConfig(ctx, importReq(archive)); err != nil {
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

	err := h.ImportConfig(ctx, importReq(archive))
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
// pre-import safety backup (h.Backup.Run) itself — must still be refused,
// never silently orphaned by a restore that overwrites the jobs table out
// from under it. h.Backup.Now, called exactly once by Run, stands in for
// that submission landing mid-backup.
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

	err := h.ImportConfig(ctx, importReq(archive))
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

	if err := h.ImportConfig(ctx, importReq(archive)); err != nil {
		t.Fatalf("ImportConfig: %v", err)
	}

	restored, err := jobStore.Get(ctx, jobID)
	if err != nil {
		t.Fatalf("getting restored job: %v", err)
	}
	if restored.Status != job.StatusInterrupted {
		t.Fatalf("restored job status = %q, want %q", restored.Status, job.StatusInterrupted)
	}

	if err := h.ImportConfig(ctx, importReq(archive)); err != nil {
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
		importErrCh <- h.ImportConfig(ctx, importReq(archive))
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

	if err := h.ImportConfig(ctx, importReq(archive)); err != nil {
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

	err := h.ImportConfig(ctx, importReq(truncated))
	assertInvalidArchive(t, err)
}

// TestImportConfig_ChecksumMismatchIsRejected covers an archive whose
// state.db bytes were changed after its manifest checksum was computed.
func TestImportConfig_ChecksumMismatchIsRejected(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newImportTestHandler(t)
	archive := exportBytes(t, h)

	staging := t.TempDir()
	if err := unpackTarZst(writeTemp(t, archive), staging); err != nil {
		t.Fatalf("unpacking baseline archive: %v", err)
	}
	if err := tamperStateDB(t, filepath.Join(staging, "state.db")); err != nil {
		t.Fatalf("tampering state.db: %v", err)
	}
	tampered := repackTarZst(t, staging)

	err := h.ImportConfig(ctx, importReq(tampered))
	assertInvalidArchive(t, err)
}

// TestImportConfig_MissingStateDBIsRejected covers an archive whose
// state.db was stripped out after packing.
func TestImportConfig_MissingStateDBIsRejected(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newImportTestHandler(t)
	archive := exportBytes(t, h)

	staging := t.TempDir()
	if err := unpackTarZst(writeTemp(t, archive), staging); err != nil {
		t.Fatalf("unpacking baseline archive: %v", err)
	}
	if err := os.Remove(filepath.Join(staging, "state.db")); err != nil {
		t.Fatalf("removing state.db: %v", err)
	}
	stripped := repackTarZst(t, staging)

	err := h.ImportConfig(ctx, importReq(stripped))
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

	staging := t.TempDir()
	if err := unpackTarZst(writeTemp(t, archive), staging); err != nil {
		t.Fatalf("unpacking baseline archive: %v", err)
	}
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

	err = h.ImportConfig(ctx, importReq(older))
	assertIncompatibleArchive(t, err)
}

func TestImportConfig_NewerSchemaVersionIsIncompatible(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newImportTestHandler(t)
	archive := exportBytes(t, h)

	staging := t.TempDir()
	if err := unpackTarZst(writeTemp(t, archive), staging); err != nil {
		t.Fatalf("unpacking baseline archive: %v", err)
	}
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

	err = h.ImportConfig(ctx, importReq(fromTheFuture))
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

	if err := h.ImportConfig(ctx, importReq(archive)); err != nil {
		t.Fatalf("ImportConfig(archive with secrets.age, no passphrase given anywhere): %v", err)
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
