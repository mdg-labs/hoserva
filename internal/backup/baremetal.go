package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"

	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"

	sqlite "modernc.org/sqlite"
)

// RefusalNewerArchive is the code importConfig refuses an archive from a
// newer schema version with (a bare-metal restore, doc 10 §1 step 1).
const RefusalNewerArchive = "archive_newer_version"

// NewerArchiveError is returned by StageBareMetal for an archive whose
// database is from a newer Hoserva than the running one: a database is never
// downgraded, so nothing is written.
type NewerArchiveError struct{ Archive, Live string }

func (e *NewerArchiveError) Error() string {
	return fmt.Sprintf("archive schema version %s is newer than the running database's %s; install a Hoserva at least as new as the one that wrote the archive", e.Archive, e.Live)
}

// ArchiveUpgradeError is returned by StageBareMetal when the migration
// runner fails on the archive's own content: a migration or transform that
// does not apply to it, a failed foreign-key or integrity check, or a
// migration history that does not match this build's. A failure of the
// machine doing the upgrade (a full or unwritable temporary directory, a
// cancelled request) is an ordinary error instead.
type ArchiveUpgradeError struct {
	From, To string
	Err      error
}

func (e *ArchiveUpgradeError) Error() string {
	return fmt.Sprintf("upgrading the archive's database from schema version %s to %s: %v", e.From, e.To, e.Err)
}

func (e *ArchiveUpgradeError) Unwrap() error { return e.Err }

// IsFreshInstall reports whether live has no array configured, which is
// what makes an import the bare-metal restore: there is no array for the
// archive's to differ from. It says nothing about admin accounts, so it
// holds after onboarding created one.
func IsFreshInstall(ctx context.Context, live *sql.DB) (bool, error) {
	var n int64
	if err := live.QueryRowContext(ctx, `SELECT COUNT(*) FROM array_disks`).Scan(&n); err != nil {
		return false, fmt.Errorf("counting the live array's disks: %w", err)
	}
	return n == 0, nil
}

// BareMetal is an archive's database staged for a bare-metal restore onto a
// fresh install (doc 10 §1): a copy of the archive's state.db, brought up to
// the running schema by the migration runner, in a directory of its own. The
// archive's own file is never written, and nothing here touches the live
// database or any disk until Apply writes into the staged copy.
type BareMetal struct {
	dir         string
	path        string
	snapshotDir string
	from, to    string
	recorded    []store.ArrayDisk
}

// StageBareMetal copies the state.db of the verified archive tree, upgrades
// the copy with store.Runner.Apply, the runner a normal daemon upgrade uses,
// and reads the array disks it records. An archive from a newer schema
// version is a *NewerArchiveError, an unreadable one an
// *UnreadableArchiveError, and one the runner cannot upgrade an
// *ArchiveUpgradeError; a failure of the upgrade that is not the archive's
// own (see ArchiveUpgradeError) is neither. The caller calls Discard.
func StageBareMetal(ctx context.Context, live *sql.DB, tree string) (*BareMetal, error) {
	src := filepath.Join(tree, "state.db")
	arc, err := openArchiveDB(src)
	if err != nil {
		return nil, err
	}
	from, err := (&store.Runner{DB: arc}).CurrentVersion(ctx)
	_ = arc.Close()
	if err != nil {
		return nil, &UnreadableArchiveError{Err: err}
	}
	to, err := (&store.Runner{DB: live}).CurrentVersion(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading live database schema version: %w", err)
	}
	if from > to {
		return nil, &NewerArchiveError{Archive: from, Live: to}
	}

	dir, err := os.MkdirTemp("", "hoserva-bare-metal-*")
	if err != nil {
		return nil, fmt.Errorf("creating the staging directory: %w", err)
	}
	b := &BareMetal{dir: dir, path: filepath.Join(dir, "state.db"), snapshotDir: filepath.Join(dir, "pre-upgrade"), from: from, to: to}
	if err := b.stage(ctx, src); err != nil {
		b.Discard()
		return nil, err
	}
	return b, nil
}

func (b *BareMetal) stage(ctx context.Context, src string) error {
	if err := copyPath(b.path, src); err != nil {
		return fmt.Errorf("copying the archive's database: %w", err)
	}
	staged, err := openStaged(b.path, "rw")
	if err != nil {
		return err
	}
	defer func() { _ = staged.Close() }()

	if b.from != b.to {
		migrations, err := store.Load()
		if err != nil {
			return fmt.Errorf("loading embedded migrations: %w", err)
		}
		runner := &store.Runner{DB: staged, Migrations: migrations, SnapshotDir: b.snapshotDir}
		if _, _, err := runner.Apply(ctx); err != nil {
			if ctx.Err() != nil || isEnvironmentFailure(err) {
				return fmt.Errorf("upgrading the archive's database from schema version %s to %s: %w", b.from, b.to, err)
			}
			return &ArchiveUpgradeError{From: b.from, To: b.to, Err: err}
		}
		got, err := runner.CurrentVersion(ctx)
		if err != nil {
			return &ArchiveUpgradeError{From: b.from, To: b.to, Err: err}
		}
		if got != b.to {
			return &ArchiveUpgradeError{From: b.from, To: b.to, Err: fmt.Errorf("the database is at %s after the upgrade", got)}
		}
	}

	_, disks, err := store.NewArrayStore(staged).GetArray(ctx)
	if err != nil && !errors.Is(err, store.ErrNoArray) {
		return fmt.Errorf("reading the archive's array disks: %w", err)
	}
	for _, d := range disks {
		if d.RemovalState != store.RemovalStateUnlisted {
			b.recorded = append(b.recorded, d)
		}
	}
	return nil
}

// isEnvironmentFailure reports an error that comes from the file system or
// SQLite's own I/O rather than from what the archive's database contains, so
// it is not evidence the archive cannot be upgraded.
func isEnvironmentFailure(err error) bool {
	var (
		pathErr *fs.PathError
		linkErr *os.LinkError
		sysErr  *os.SyscallError
		sqlErr  *sqlite.Error
	)
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return true
	case errors.As(err, &pathErr), errors.As(err, &linkErr), errors.As(err, &sysErr):
		return true
	case errors.As(err, &sqlErr):
		switch sqlErr.Code() & 0xff {
		case sqliteNoMem, sqliteReadOnly, sqliteIOErr, sqliteFull, sqliteCantOpen:
			return true
		}
	}
	return false
}

// SQLite's primary result codes for a failure of the environment.
const (
	sqliteNoMem    = 7
	sqliteReadOnly = 8
	sqliteIOErr    = 10
	sqliteFull     = 13
	sqliteCantOpen = 14
)

func openStaged(path, mode string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=" + mode}).String())
	if err != nil {
		return nil, fmt.Errorf("opening the staged archive database: %w", err)
	}
	return db, nil
}

// Path is the staged database, which everything after staging reads and
// which the restore is made from.
func (b *BareMetal) Path() string { return b.path }

// SchemaUpgrade reports whether the archive's database was older than the
// running schema and so upgraded.
func (b *BareMetal) SchemaUpgrade() bool { return b.from != b.to }

// Discard removes the staged copy and everything the upgrade wrote beside it.
func (b *BareMetal) Discard() { _ = os.RemoveAll(b.dir) }

// Map is MapArrayDisks for the disks this archive records.
func (b *BareMetal) Map(attached []disk.Disk) []MappedDisk {
	return MapArrayDisks(b.recorded, attached)
}

// Confirm maps the recorded disks against the attached ones as they are now
// and checks the mapping the user confirmed is exactly it. A nil mapping is
// ErrDiskMappingRequired and one that differs a *DiskMappingStaleError; the
// mapping it returns is what Apply writes.
func (b *BareMetal) Confirm(attached []disk.Disk, confirmed *DiskMapping) ([]MappedDisk, error) {
	if confirmed == nil {
		return nil, ErrDiskMappingRequired
	}
	mapped := b.Map(attached)
	if err := checkConfirmed(mapped, *confirmed); err != nil {
		return nil, err
	}
	return mapped, nil
}

// BareMetalPreview is what a bare-metal restore of an archive would do
// beyond what ImportPreview lists: whether its database is upgraded, and each
// array disk it records with what it matched.
type BareMetalPreview struct {
	SchemaUpgrade bool
	Disks         []MappedDisk
}

// PreviewBareMetal is PreviewImport for a fresh install: the archive's
// database is staged and prepared exactly as the restore prepares it
// (StageBareMetal, then Apply), so the groups are what the restore brings
// in, and the disks are mapped against attached. Only the staged copy in a
// temporary directory of its own is written; the live database, the archive
// tree and every disk are only read. A newer archive is a blocker with no
// BareMetalPreview, as is one that cannot be upgraded.
func PreviewBareMetal(ctx context.Context, live *sql.DB, paths Paths, stagingDir string, attached []disk.Disk, opts ...FilesOption) (ImportPreview, *BareMetalPreview, error) {
	manifest, err := readManifest(filepath.Join(stagingDir, "manifest.json"))
	if err != nil {
		return ImportPreview{}, nil, fmt.Errorf("reading the archive's manifest: %w", err)
	}
	b, err := StageBareMetal(ctx, live, stagingDir)
	var (
		newer   *NewerArchiveError
		upgrade *ArchiveUpgradeError
	)
	switch {
	case errors.As(err, &newer):
		return blockedBareMetalPreview(manifest, newer.Archive, newer.Live, ImportRefusal{Code: RefusalNewerArchive, Message: err.Error()}, paths, stagingDir, opts)
	case errors.As(err, &upgrade):
		return blockedBareMetalPreview(manifest, upgrade.From, upgrade.To, ImportRefusal{Code: RefusalIncompatibleArchive, Message: err.Error()}, paths, stagingDir, opts)
	case err != nil:
		return ImportPreview{}, nil, err
	}
	defer b.Discard()

	p := newImportPreview(manifest, b.from, b.to)
	files, err := p.planFiles(stagingDir, paths, opts...)
	if err != nil {
		return ImportPreview{}, nil, err
	}
	mapped := b.Map(attached)
	if _, err := b.Apply(ctx, live, mapped); err != nil {
		return ImportPreview{}, nil, err
	}
	staged, err := openStaged(b.path, "ro")
	if err != nil {
		return ImportPreview{}, nil, err
	}
	defer func() { _ = staged.Close() }()
	if err := p.compare(ctx, live, staged, files); err != nil {
		return ImportPreview{}, nil, err
	}
	return p, &BareMetalPreview{SchemaUpgrade: b.SchemaUpgrade(), Disks: mapped}, nil
}

func blockedBareMetalPreview(manifest Manifest, archiveSchema, liveSchema string, refusal ImportRefusal, paths Paths, stagingDir string, opts []FilesOption) (ImportPreview, *BareMetalPreview, error) {
	p := newImportPreview(manifest, archiveSchema, liveSchema)
	p.Blockers = append(p.Blockers, refusal)
	if _, err := p.planFiles(stagingDir, paths, opts...); err != nil {
		return ImportPreview{}, nil, err
	}
	return p, nil, nil
}

// The reasons a bare-metal restore does not restore a disk or a secret.
const (
	NotRestoredDiskAbsent          = "disk_absent"
	NotRestoredDiskReplaced        = "disk_replaced"
	NotRestoredDiskAmbiguous       = "disk_ambiguous"
	NotRestoredSealedUnderOtherKey = "sealed_under_other_key"
)

// The kinds of NotRestored a bare-metal restore adds to NotRestoredStackEnv.
const (
	NotRestoredDisk           = "disk"
	NotRestoredDatabaseSecret = "database_secret"
)

// sealedColumn is a database column sealed under the machine key of the
// installation that wrote the archive. label is an SQL expression naming
// the row, and empty what a cleared column holds: NULL, or an empty blob
// for a column declared NOT NULL.
type sealedColumn struct {
	table, column, label, empty string
}

// sealedColumns is every column the schema seals under the machine key (Q28).
// A restore onto another machine key cannot read them, so each is cleared and
// reported; a test lists the schema's BLOB columns and fails on one that is
// neither here nor in unsealedBlobColumns.
var sealedColumns = []sealedColumn{
	{"users", "totp_secret", "username", "NULL"},
	{"users", "totp_pending_secret", "username", "NULL"},
	{"notify_channels", "secret", "name", "NULL"},
	{"acme_config", "dns_secret", "domain", "X''"},
	{"acme_config", "account_key", "domain", "X''"},
	{"ups_config", "monitor_password", "connection", "X''"},
	{"ups_config", "network_password", "connection", "X''"},
	{"backup_destinations", "secrets", "name", "X''"},
	{"schema_info", "backup_passphrase", "''", "NULL"},
}

// unsealedBlobColumns are the schema's BLOB columns that are not sealed
// under the machine key, each with why.
var unsealedBlobColumns = map[string]string{
	"jobs.checkpoint":                   "a resumable job's state",
	"machine_key_check.check_value":     "kept: this installation's own, written into the staged database",
	"backup_recipient.wrapped_identity": "kept: this installation's own, written into the staged database",
	"backup_recipient.check_value":      "kept: this installation's own, written into the staged database",
}

// Apply makes the staged database one this installation can start on and
// describes what it did not restore. Everything is written into the staged
// copy in one transaction, so a failure leaves no partial change and the
// live database is never touched:
//
//   - this installation's own machine_key_check and backup_recipient rows
//     replace the archive's, since auth.LoadOrGenerateMachineKey treats a
//     check value that does not match the key file as fatal (Q28);
//   - each matched disk's device is the attached disk's; every other row
//     keeps its recorded device unless a matched disk now has it;
//   - every column in sealedColumns that holds something is cleared, and
//     users' TOTP enrolment with it so the next login enrols again.
//
// A disk that is not matched stays a row of the array, unmounted, and is
// reported: the storage gate then reports the array degraded and nothing
// mounts until the user acknowledges it or the replace flow (doc 09 §4) runs.
func (b *BareMetal) Apply(ctx context.Context, live *sql.DB, mapped []MappedDisk) ([]NotRestored, error) {
	var (
		keyValue, recIdentity, recCheck   []byte
		keyCreated, recipient, recCreated string
	)
	err := live.QueryRowContext(ctx, `SELECT check_value, created_at FROM machine_key_check WHERE id = 1`).Scan(&keyValue, &keyCreated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("the live database has no machine key check value to keep")
	}
	if err != nil {
		return nil, fmt.Errorf("reading the live machine key check value: %w", err)
	}
	err = live.QueryRowContext(ctx, `SELECT public_recipient, wrapped_identity, check_value, created_at FROM backup_recipient WHERE id = 1`).
		Scan(&recipient, &recIdentity, &recCheck, &recCreated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("the live database has no backup recipient to keep")
	}
	if err != nil {
		return nil, fmt.Errorf("reading the live backup recipient: %w", err)
	}

	staged, err := openStaged(b.path, "rw")
	if err != nil {
		return nil, err
	}
	defer func() { _ = staged.Close() }()
	tx, err := staged.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginning the staged archive update: %w", err)
	}
	notRestored, err := func() ([]NotRestored, error) {
		if _, err := tx.ExecContext(ctx, `DELETE FROM machine_key_check`); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO machine_key_check (id, check_value, created_at) VALUES (1, ?, ?)`, keyValue, keyCreated); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM backup_recipient`); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO backup_recipient (id, public_recipient, wrapped_identity, check_value, created_at) VALUES (1, ?, ?, ?, ?)`,
			recipient, recIdentity, recCheck, recCreated); err != nil {
			return nil, err
		}
		if err := rewriteDevices(ctx, tx, mapped); err != nil {
			return nil, fmt.Errorf("writing the attached disks' devices: %w", err)
		}
		notRestored := disksNotRestored(mapped)
		secrets, err := clearSealedColumns(ctx, tx)
		if err != nil {
			return nil, fmt.Errorf("clearing the secrets sealed under the archive's machine key: %w", err)
		}
		return append(notRestored, secrets...), nil
	}()
	if err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("preparing the archive's database for this installation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("preparing the archive's database for this installation: %w", err)
	}
	return notRestored, nil
}

// rewriteDevices gives each matched array_disks row the attached disk's
// device. UNIQUE(device) is checked row by row, so a swap of two devices
// goes through placeholders no device can be named.
func rewriteDevices(ctx context.Context, tx *sql.Tx, mapped []MappedDisk) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, role, role_index, device, mountpoint FROM array_disks`)
	if err != nil {
		return err
	}
	type row struct {
		id         int64
		role       string
		index      int
		device     string
		mountpoint string
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.role, &r.index, &r.device, &r.mountpoint); err != nil {
			_ = rows.Close()
			return err
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	assigned := map[string]bool{}
	final := make(map[int64]string, len(all))
	for _, r := range all {
		for _, m := range mapped {
			if m.State == DiskMatched && m.Recorded.Role == r.role && m.Recorded.RoleIndex == r.index {
				final[r.id] = m.Attached.Device
				assigned[m.Attached.Device] = true
			}
		}
	}
	for _, r := range all {
		if _, ok := final[r.id]; ok {
			continue
		}
		if assigned[r.device] {
			final[r.id] = "unmapped:" + r.mountpoint
		} else {
			final[r.id] = r.device
		}
		assigned[final[r.id]] = true
	}

	if _, err := tx.ExecContext(ctx, `UPDATE array_disks SET device = 'pending:' || id`); err != nil {
		return err
	}
	for _, r := range all {
		if _, err := tx.ExecContext(ctx, `UPDATE array_disks SET device = ? WHERE id = ?`, final[r.id], r.id); err != nil {
			return err
		}
	}
	return nil
}

func disksNotRestored(mapped []MappedDisk) []NotRestored {
	var out []NotRestored
	for _, m := range mapped {
		var reason, why string
		switch m.State {
		case DiskAbsent:
			reason, why = NotRestoredDiskAbsent, "no attached disk is it"
		case DiskReplaced:
			reason, why = NotRestoredDiskReplaced, "its identity or its filesystem differs from the one recorded"
		case DiskAmbiguous:
			reason, why = NotRestoredDiskAmbiguous, "more than one attached disk matches it"
		default:
			continue
		}
		out = append(out, NotRestored{Kind: NotRestoredDisk, Name: m.Name(), Reason: reason,
			Message: fmt.Sprintf("the %s is not mounted: %s; the replace flow adopts a replacement disk", m.Name(), why)})
	}
	return out
}

func clearSealedColumns(ctx context.Context, tx *sql.Tx) ([]NotRestored, error) {
	var out []NotRestored
	for _, c := range sealedColumns {
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT %s FROM %s WHERE length(%s) > 0 ORDER BY 1`, c.label, c.table, c.column))
		if err != nil {
			return nil, fmt.Errorf("reading %s.%s: %w", c.table, c.column, err)
		}
		var labels []string
		for rows.Next() {
			var l string
			if err := rows.Scan(&l); err != nil {
				_ = rows.Close()
				return nil, err
			}
			labels = append(labels, l)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET %s = %s WHERE %s IS NOT NULL`, c.table, c.column, c.empty, c.column)); err != nil {
			return nil, fmt.Errorf("clearing %s.%s: %w", c.table, c.column, err)
		}
		for _, l := range labels {
			name := c.table + "." + c.column
			if l != "" {
				name += " (" + l + ")"
			}
			out = append(out, NotRestored{Kind: NotRestoredDatabaseSecret, Name: name, Reason: NotRestoredSealedUnderOtherKey,
				Message: fmt.Sprintf("%s was sealed under the machine key of the installation the archive came from and is cleared; enter it again", name)})
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET totp_confirmed_at = NULL, totp_last_step = 0`); err != nil {
		return nil, fmt.Errorf("clearing the TOTP enrolment: %w", err)
	}
	return out, nil
}
