package backup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// openPlainDB opens a new database at path, in the journal mode a VACUUM INTO
// leaves an archive's state.db in, with every embedded migration applied.
func openPlainDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	migrations, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, _, err := (&store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}).Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	return db
}

func fileSum(t *testing.T, path string) [sha256.Size]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

func copyForTest(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// seedInstallation gives db what a running installation always has: its
// machine key check and its backup recipient.
func seedInstallation(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	mustExec(t, db,
		fmt.Sprintf(`INSERT OR REPLACE INTO machine_key_check (id, check_value, created_at) VALUES (1, CAST('%s-check' AS BLOB), '2026-09-01T00:00:00Z')`, name),
		fmt.Sprintf(`INSERT OR REPLACE INTO backup_recipient (id, public_recipient, wrapped_identity, check_value, created_at)
			VALUES (1, 'age1%s', CAST('%s-wrapped' AS BLOB), CAST('%s-recipient-check' AS BLOB), '2026-09-01T00:00:00Z')`, name, name, name),
	)
}

func queryString(t *testing.T, db *sql.DB, q string, args ...any) string {
	t.Helper()
	var s sql.NullString
	if err := db.QueryRow(q, args...).Scan(&s); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return s.String
}

func openStagedRO(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := openStaged(path, "ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// Every database in testdata/db is a released schema's fixture. An archive
// of one is upgraded to the running schema on a copy: the archive's own file
// is never written, and the copy is at head with its data intact.
func TestStageBareMetal_UpgradesEveryReleasedFixtureOnACopy(t *testing.T) {
	ctx := context.Background()
	fixtures, err := filepath.Glob("../../testdata/db/*.db")
	if err != nil || len(fixtures) == 0 {
		t.Fatalf("no fixtures under testdata/db (%v)", err)
	}
	migrations, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	head := migrations[len(migrations)-1].Version

	for _, fixture := range fixtures {
		t.Run(filepath.Base(fixture), func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			tree := t.TempDir()
			copyForTest(t, fixture, filepath.Join(tree, "state.db"))
			before := fileSum(t, filepath.Join(tree, "state.db"))
			live := openMigratedDB(t, filepath.Join(t.TempDir(), "live.db"))

			b, err := StageBareMetal(ctx, live, tree)
			if err != nil {
				t.Fatalf("StageBareMetal: %v", err)
			}
			if !b.SchemaUpgrade() {
				t.Error("SchemaUpgrade = false for a fixture older than head")
			}
			staged := openStagedRO(t, b.Path())
			if v, err := (&store.Runner{DB: staged}).CurrentVersion(ctx); err != nil || v != head {
				t.Fatalf("staged version = %q (err %v), want head %q", v, err, head)
			}
			if got := queryString(t, staged, `PRAGMA integrity_check`); got != "ok" {
				t.Fatalf("integrity_check = %q", got)
			}
			absFixture, err := filepath.Abs(fixture)
			if err != nil {
				t.Fatal(err)
			}
			archived := openStagedRO(t, absFixture)
			if want, got := queryString(t, archived, `SELECT installation_id FROM schema_info`), queryString(t, staged, `SELECT installation_id FROM schema_info`); want != got {
				t.Errorf("installation_id after the upgrade = %q, want %q", got, want)
			}
			if after := fileSum(t, filepath.Join(tree, "state.db")); after != before {
				t.Fatal("the archive's own state.db was written")
			}
			b.Discard()
			if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
				t.Errorf("Discard left %v", entries)
			}
		})
	}
}

func TestStageBareMetal_RefusesANewerArchiveBeforeWritingAnything(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	tree := t.TempDir()
	arc := openPlainDB(t, filepath.Join(tree, "state.db"))
	mustExec(t, arc, `INSERT INTO schema_migrations (version, slug, checksum, applied_at) VALUES ('99999999999999', 'from_the_future', 'x', '2099-01-01T00:00:00Z')`)
	before := fileSum(t, filepath.Join(tree, "state.db"))
	live := openMigratedDB(t, filepath.Join(t.TempDir(), "live.db"))

	_, err := StageBareMetal(ctx, live, tree)
	var newer *NewerArchiveError
	if !errors.As(err, &newer) || newer.Archive != "99999999999999" {
		t.Fatalf("StageBareMetal = %v, want a *NewerArchiveError for 99999999999999", err)
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Errorf("a refused archive left %v", entries)
	}
	if fileSum(t, filepath.Join(tree, "state.db")) != before {
		t.Error("the archive's state.db was written")
	}
}

func TestStageBareMetal_AnArchiveTheRunnerCannotUpgradeIsRefusedAndLeavesNothing(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	tree := t.TempDir()
	copyForTest(t, "../../testdata/db/v1.db", filepath.Join(tree, "state.db"))
	arc, err := sql.Open("sqlite", filepath.Join(tree, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	// A table a later migration creates, of the wrong shape: that migration
	// fails, and the runner rolls the whole upgrade back.
	mustExec(t, arc, `CREATE TABLE shares (x TEXT)`)
	_ = arc.Close()
	live := openMigratedDB(t, filepath.Join(t.TempDir(), "live.db"))

	_, err = StageBareMetal(ctx, live, tree)
	var upgrade *ArchiveUpgradeError
	if !errors.As(err, &upgrade) {
		t.Fatalf("StageBareMetal = %v, want an *ArchiveUpgradeError", err)
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Errorf("a failed upgrade left %v", entries)
	}
}

// A failure of the machine doing the upgrade is not a verdict on the archive:
// the pre-upgrade snapshot cannot be written here, so the error is an
// ordinary one, not an *ArchiveUpgradeError (which the API answers 400
// incompatible_archive and the preview lists as a blocker).
func TestStageBareMetal_ASnapshotThatCannotBeWrittenIsNotAnIncompatibleArchive(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	notADir := filepath.Join(dir, "file")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	b := &BareMetal{
		dir:         dir,
		path:        filepath.Join(dir, "state.db"),
		snapshotDir: filepath.Join(notADir, "pre-upgrade"),
		from:        "00000000000001",
		to:          "99999999999999",
	}

	err := b.stage(ctx, "../../testdata/db/v1.db")
	if err == nil {
		t.Fatal("stage succeeded with no place to write the pre-upgrade snapshot")
	}
	var upgrade *ArchiveUpgradeError
	if errors.As(err, &upgrade) {
		t.Fatalf("stage = %v, an *ArchiveUpgradeError for a snapshot that could not be written", err)
	}
	if !strings.Contains(err.Error(), "pre-migration snapshot") {
		t.Errorf("stage = %v, want the failed snapshot named", err)
	}
}

func TestIsEnvironmentFailure(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"path error":        {fmt.Errorf("a: %w", &fs.PathError{Op: "mkdir", Path: "/tmp/x", Err: syscall.ENOSPC}), true},
		"cancelled":         {fmt.Errorf("a: %w", context.Canceled), true},
		"deadline":          {fmt.Errorf("a: %w", context.DeadlineExceeded), true},
		"plain content":     {errors.New(`applying migration "x.sql": no such column`), false},
		"checksum mismatch": {errors.New("checking pending migrations: checksum differs"), false},
	} {
		if got := isEnvironmentFailure(tc.err); got != tc.want {
			t.Errorf("%s: isEnvironmentFailure = %v, want %v", name, got, tc.want)
		}
	}
}

func TestIsFreshInstall_MeansNoArrayWhateverElseIsConfigured(t *testing.T) {
	ctx := context.Background()
	db := openMigratedDB(t, filepath.Join(t.TempDir(), "live.db"))
	mustExec(t, db, `INSERT INTO users (id, username, password_hash, role, totp_last_step, created_at) VALUES ('a', 'admin', 'x', 'admin', 0, '2026-09-01T00:00:00Z')`)
	if fresh, err := IsFreshInstall(ctx, db); err != nil || !fresh {
		t.Fatalf("IsFreshInstall with an admin and no array = %v, %v, want true", fresh, err)
	}
	mustExec(t, db, `INSERT INTO array_disks (role, role_index, device, filesystem, fs_uuid, mountpoint) VALUES ('data', 1, '/dev/sdb', 'xfs', 'u', '/mnt/disk1')`)
	if fresh, err := IsFreshInstall(ctx, db); err != nil || fresh {
		t.Fatalf("IsFreshInstall with an array = %v, %v, want false", fresh, err)
	}
}

// archiveOfAnotherInstallation is a state.db in a tree, from an installation
// that has a three-disk array and every kind of secret sealed under its own
// machine key.
func archiveOfAnotherInstallation(t *testing.T) string {
	t.Helper()
	tree := t.TempDir()
	db := openPlainDB(t, filepath.Join(tree, "state.db"))
	seedInstallation(t, db, "other")
	mustExec(t, db,
		`INSERT INTO array_settings (id, create_policy, min_free_space, created_at) VALUES (1, 'mfs', '20G', '2026-09-01T00:00:00Z')`,
		`INSERT INTO array_disks (role, role_index, device, filesystem, fs_uuid, wwn, serial, weak_identity, mountpoint) VALUES
			('parity', 1, '/dev/sda', 'xfs', 'uuid-p1', 'wwn-p1', 'ser-p1', 0, '/mnt/parity1'),
			('data', 1, '/dev/sdb', 'xfs', 'uuid-d1', 'wwn-d1', 'ser-d1', 0, '/mnt/disk1'),
			('data', 2, '/dev/sdc', 'xfs', 'uuid-d2', 'wwn-d2', 'ser-d2', 0, '/mnt/disk2')`,
		`INSERT INTO users (id, username, password_hash, role, totp_secret, totp_pending_secret, totp_confirmed_at, totp_last_step, created_at)
			VALUES ('u1', 'alice', 'hash', 'admin', X'aa', X'bb', '2026-09-01T00:00:00Z', 5, '2026-09-01T00:00:00Z'),
			       ('u2', 'bob', 'hash', 'viewer', NULL, NULL, NULL, 0, '2026-09-01T00:00:00Z')`,
		`INSERT INTO notify_channels (id, name, type, enabled, config, secret, created_at, updated_at) VALUES ('c1', 'ops', 'gotify', 1, '{}', X'cc', 't', 't')`,
		`INSERT INTO acme_config (id, domain, provider, provider_config, dns_secret, account_key, enabled, updated_at) VALUES (1, 'nas.example.org', 'cloudflare', '{}', X'dd', X'ee', 1, 't')`,
		`INSERT INTO ups_config (id, connection, driver, port, monitor_password, network_host, network_port, network_ups_name, network_username, network_password, low_battery_percent, runtime_seconds, updated_at)
			VALUES (1, 'usb', 'usbhid-ups', 'auto', X'ff', '', 0, '', '', X'', 20, 300, 't')`,
		`INSERT INTO backup_destinations (id, name, type, path, options, secrets, enabled, encrypt, retention_daily, retention_weekly, retention_monthly, created_at)
			VALUES ('d1', 'offsite', 's3', 'bucket', '{}', X'11', 1, 1, 7, 4, 6, 't'),
			       ('d2', 'boot', 'local', '/var/backups', '{}', X'', 1, 0, 7, 4, 6, 't')`,
	)
	if n := queryString(t, db, `SELECT COUNT(*) FROM schema_info`); n == "0" {
		mustExec(t, db, `INSERT INTO schema_info (id, installation_id, created_at) VALUES (1, 'other-install', 't')`)
	}
	mustExec(t, db, `UPDATE schema_info SET backup_passphrase = X'99'`)
	return tree
}

func attached(devices map[string]disk.Disk) []disk.Disk {
	var out []disk.Disk
	for dev, d := range devices {
		d.Device = dev
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b disk.Disk) int { return strings.Compare(a.Device, b.Device) })
	return out
}

func TestBareMetalApply_MakesTheStagedDatabaseThisInstallations(t *testing.T) {
	ctx := context.Background()
	tree := archiveOfAnotherInstallation(t)
	live := openMigratedDB(t, filepath.Join(t.TempDir(), "live.db"))
	seedInstallation(t, live, "box")
	archiveBefore := fileSum(t, filepath.Join(tree, "state.db"))

	b, err := StageBareMetal(ctx, live, tree)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Discard()
	// The disks came back on other device names, data 1 and data 2 swapped,
	// the parity disk is not attached.
	mapped := b.Map(attached(map[string]disk.Disk{
		"/dev/sdb": {WWN: "wwn-d2", Serial: "ser-d2", FSUUID: "uuid-d2"},
		"/dev/sdc": {WWN: "wwn-d1", Serial: "ser-d1", FSUUID: "uuid-d1"},
	}))
	notRestored, err := b.Apply(ctx, live, mapped, SecretsOutcome{}, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	staged := openStagedRO(t, b.Path())
	if got := queryString(t, staged, `SELECT CAST(check_value AS TEXT) FROM machine_key_check`); got != "box-check" {
		t.Errorf("machine_key_check = %q, want this installation's own", got)
	}
	for col, want := range map[string]string{"public_recipient": "age1box", "CAST(wrapped_identity AS TEXT)": "box-wrapped", "CAST(check_value AS TEXT)": "box-recipient-check"} {
		if got := queryString(t, staged, `SELECT `+col+` FROM backup_recipient`); got != want {
			t.Errorf("backup_recipient %s = %q, want %q", col, got, want)
		}
	}
	devices := map[string]string{}
	rows, err := staged.Query(`SELECT role || role_index, device FROM array_disks`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var slot, dev string
		if err := rows.Scan(&slot, &dev); err != nil {
			t.Fatal(err)
		}
		devices[slot] = dev
	}
	_ = rows.Close()
	want := map[string]string{"data1": "/dev/sdc", "data2": "/dev/sdb", "parity1": "/dev/sda"}
	for slot, dev := range want {
		if devices[slot] != dev {
			t.Errorf("%s device = %q, want %q (devices: %v)", slot, devices[slot], dev, devices)
		}
	}

	for _, q := range []string{
		`SELECT COUNT(*) FROM users WHERE totp_secret IS NOT NULL OR totp_pending_secret IS NOT NULL OR totp_confirmed_at IS NOT NULL OR totp_last_step != 0`,
		`SELECT COUNT(*) FROM notify_channels WHERE secret IS NOT NULL`,
		`SELECT COUNT(*) FROM acme_config WHERE length(dns_secret) > 0 OR length(account_key) > 0`,
		`SELECT COUNT(*) FROM ups_config WHERE length(monitor_password) > 0`,
		`SELECT COUNT(*) FROM backup_destinations WHERE length(secrets) > 0`,
		`SELECT COUNT(*) FROM schema_info WHERE backup_passphrase IS NOT NULL`,
	} {
		if got := queryString(t, staged, q); got != "0" {
			t.Errorf("%s = %s, want every sealed column cleared", q, got)
		}
	}
	if got := queryString(t, staged, `SELECT username FROM users WHERE role = 'admin'`); got != "alice" {
		t.Errorf("the admin = %q, want the archive's user kept", got)
	}
	if got := queryString(t, staged, `SELECT domain FROM acme_config`); got != "nas.example.org" {
		t.Errorf("the ACME row's other columns did not survive: domain = %q", got)
	}

	var names []string
	for _, n := range notRestored {
		names = append(names, n.Kind+" "+n.Name+" "+n.Reason)
	}
	for _, w := range []string{
		"disk parity disk 1 (/mnt/parity1, WWN wwn-p1) disk_absent",
		"database_secret users.totp_secret (alice) sealed_under_other_key",
		"database_secret users.totp_pending_secret (alice) sealed_under_other_key",
		"database_secret notify_channels.secret (ops) sealed_under_other_key",
		"database_secret acme_config.dns_secret (nas.example.org) sealed_under_other_key",
		"database_secret acme_config.account_key (nas.example.org) sealed_under_other_key",
		"database_secret ups_config.monitor_password (usb) sealed_under_other_key",
		"database_secret backup_destinations.secrets (offsite) sealed_under_other_key",
		"database_secret schema_info.backup_passphrase sealed_under_other_key",
	} {
		if !slices.Contains(names, w) {
			t.Errorf("notRestored lacks %q; has %v", w, names)
		}
	}
	for _, n := range names {
		if strings.Contains(n, "boot") || strings.Contains(n, "bob") || strings.Contains(n, "network_password") {
			t.Errorf("notRestored lists %q, which held nothing sealed", n)
		}
	}
	if fileSum(t, filepath.Join(tree, "state.db")) != archiveBefore {
		t.Error("the archive's own state.db was written")
	}
}

// A failure while making the staged database this installation's leaves it as
// it was: the whole preparation is one transaction.
func TestBareMetalApply_AFailureLeavesTheStagedDatabaseUntouched(t *testing.T) {
	ctx := context.Background()
	tree := archiveOfAnotherInstallation(t)
	live := openMigratedDB(t, filepath.Join(t.TempDir(), "live.db"))
	seedInstallation(t, live, "box")
	b, err := StageBareMetal(ctx, live, tree)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Discard()

	mapped := b.Map(attached(map[string]disk.Disk{"/dev/sdb": {WWN: "wwn-d1", FSUUID: "uuid-d1"}}))
	// A CHECK the schema does not have makes one of the clearing statements
	// fail once the earlier ones have already run.
	staged, err := openStaged(b.Path(), "rw")
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, staged, `CREATE TRIGGER fail_clear BEFORE UPDATE OF secrets ON backup_destinations BEGIN SELECT RAISE(ABORT, 'injected'); END`)
	_ = staged.Close()

	if _, err := b.Apply(ctx, live, mapped, SecretsOutcome{}, nil); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("Apply = %v, want the injected failure", err)
	}
	db := openStagedRO(t, b.Path())
	for q, want := range map[string]string{
		`SELECT CAST(check_value AS TEXT) FROM machine_key_check`:               "other-check",
		`SELECT public_recipient FROM backup_recipient`:                         "age1other",
		`SELECT device FROM array_disks WHERE role = 'data' AND role_index = 1`: "/dev/sdb",
		`SELECT COUNT(*) FROM users WHERE totp_secret IS NOT NULL`:              "1",
		`SELECT COUNT(*) FROM notify_channels WHERE secret IS NOT NULL`:         "1",
	} {
		if got := queryString(t, db, q); got != want {
			t.Errorf("%s = %q after a failed Apply, want %q: the archive's database as it was", q, got, want)
		}
	}
}

func TestBareMetalApply_RefusesWhenThisInstallationHasNoKeyRowsToKeep(t *testing.T) {
	ctx := context.Background()
	for name, stmt := range map[string]string{
		"machine key check": `DELETE FROM machine_key_check`,
		"backup recipient":  `DELETE FROM backup_recipient`,
	} {
		t.Run(name, func(t *testing.T) {
			tree := archiveOfAnotherInstallation(t)
			live := openMigratedDB(t, filepath.Join(t.TempDir(), "live.db"))
			seedInstallation(t, live, "box")
			mustExec(t, live, stmt)
			b, err := StageBareMetal(ctx, live, tree)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Discard()
			before := fileSum(t, b.Path())
			if _, err := b.Apply(ctx, live, b.Map(nil), SecretsOutcome{}, nil); err == nil {
				t.Fatal("Apply succeeded without this installation's own row to keep")
			}
			if fileSum(t, b.Path()) != before {
				t.Error("the staged database changed")
			}
		})
	}
}

// An unmatched disk keeps its recorded device unless a matched disk now has
// it, since UNIQUE(device) would refuse two rows; it then gets a name no
// device has, and a swap of two matched devices goes through.
func TestBareMetalApply_DevicesNeverCollide(t *testing.T) {
	ctx := context.Background()
	tree := archiveOfAnotherInstallation(t)
	live := openMigratedDB(t, filepath.Join(t.TempDir(), "live.db"))
	seedInstallation(t, live, "box")
	b, err := StageBareMetal(ctx, live, tree)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Discard()
	// Data 2 is matched and now sits where the absent parity disk was.
	mapped := b.Map(attached(map[string]disk.Disk{
		"/dev/sda": {WWN: "wwn-d2", FSUUID: "uuid-d2"},
		"/dev/sdb": {WWN: "wwn-d1", FSUUID: "uuid-d1"},
	}))
	if _, err := b.Apply(ctx, live, mapped, SecretsOutcome{}, nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	staged := openStagedRO(t, b.Path())
	if got := queryString(t, staged, `SELECT device FROM array_disks WHERE role = 'data' AND role_index = 2`); got != "/dev/sda" {
		t.Errorf("data 2 device = %q, want /dev/sda", got)
	}
	got := queryString(t, staged, `SELECT device FROM array_disks WHERE role = 'parity'`)
	if got == "/dev/sda" || !strings.HasPrefix(got, "unmapped:") {
		t.Errorf("the absent parity disk's device = %q, want a name no device has", got)
	}
}

// Every BLOB column in the schema is either sealed under the machine key, and
// so cleared by a bare-metal restore, or listed with why it is not: a new
// sealed column cannot slip past the restore.
func TestSealedColumns_CoverEveryBlobColumnOfTheSchema(t *testing.T) {
	db := openMigratedDB(t, filepath.Join(t.TempDir(), "live.db"))
	rows, err := db.Query(`SELECT m.name || '.' || p.name FROM sqlite_master m, pragma_table_info(m.name) p WHERE m.type = 'table' AND p.type = 'BLOB' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	known := map[string]bool{}
	for _, c := range sealedColumns {
		known[c.table+"."+c.column] = true
	}
	for c := range unsealedBlobColumns {
		known[c] = true
	}
	found := map[string]bool{}
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			t.Fatal(err)
		}
		found[col] = true
		if !known[col] {
			t.Errorf("BLOB column %s is neither in sealedColumns nor in unsealedBlobColumns", col)
		}
	}
	for col := range known {
		if !found[col] {
			t.Errorf("%s is listed but the schema has no such BLOB column", col)
		}
	}
}

func TestBareMetalHostFileDecisions_AreTheArchivesSambaAndNFSRowsOnly(t *testing.T) {
	ctx := context.Background()
	tree := archiveOfAnotherInstallation(t)
	db, err := sql.Open("sqlite", filepath.Join(tree, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db,
		`INSERT INTO host_config (kind, decision, facts, applied_at) VALUES
			('samba', 'import', '{}', '2026-09-01T00:00:00Z'),
			('nfs', 'leave', '{}', '2026-09-01T00:00:00Z'),
			('fstab', 'import', '{}', '2026-09-01T00:00:00Z'),
			('docker_images', 'import', '{}', '2026-09-01T00:00:00Z')`)
	_ = db.Close()
	live := openMigratedDB(t, filepath.Join(t.TempDir(), "live.db"))
	seedInstallation(t, live, "box")

	b, err := StageBareMetal(ctx, live, tree)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Discard()
	got, err := b.HostFileDecisions(ctx)
	if err != nil {
		t.Fatalf("HostFileDecisions: %v", err)
	}
	want := []config.HostFileDecision{
		{Path: config.PathNFS, Decision: config.DecisionLeave},
		{Path: config.PathSamba, Decision: config.DecisionImport},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("HostFileDecisions = %+v, want %+v", got, want)
	}
}

func TestHostFilesNote_NamesEachFileAtItsPathOnThisHost(t *testing.T) {
	if _, ok := HostFilesNote("/etc", nil); ok {
		t.Fatal("a restore that replaces nothing has a note")
	}
	note, ok := HostFilesNote("/etc", []config.HostFileDecision{
		{Path: config.PathSamba, Decision: config.DecisionImport},
		{Path: config.PathNFS, Decision: config.DecisionImport},
	})
	if !ok || note.Code != NoteHostFilesReplaced {
		t.Fatalf("note = %+v, %v", note, ok)
	}
	for _, want := range []string{"Samba configuration (/etc/samba/smb.conf)", "NFS exports (/etc/exports)"} {
		if !strings.Contains(note.Message, want) {
			t.Errorf("message %q lacks %q", note.Message, want)
		}
	}
}
