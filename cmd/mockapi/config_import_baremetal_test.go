package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	ht "github.com/ogen-go/ogen/http"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// mockBareMetalArchive is a real config archive from another installation
// whose array has one data disk with the serial of the fresh-install
// scenario's first disk and one with a serial no scenario disk has; extra
// rows the archive's database gets are run first.
func mockBareMetalArchive(t *testing.T, extra ...string) []byte {
	t.Helper()
	b, err := buildMockBareMetalArchive(extra...)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// buildMockBareMetalArchive is mockBareMetalArchive for a contract case,
// which has no *testing.T to build one with.
func buildMockBareMetalArchive(extra ...string) ([]byte, error) {
	return buildMockArchive(false, extra...)
}

// contractArchivePassphrase is the backup passphrase the sealed archives of
// buildMockSealedArchive were built under.
const contractArchivePassphrase = "the archive's contract passphrase"

// buildMockSealedArchive is buildMockBareMetalArchive with the archive's
// backup passphrase set, so it carries a secrets.age holding the notification
// channel's credential and an identity.age holding its backup recipient.
func buildMockSealedArchive(extra ...string) ([]byte, error) {
	return buildMockArchive(true, extra...)
}

func buildMockArchive(sealed bool, extra ...string) ([]byte, error) {
	migrations, err := store.Load()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "mockapi-archive-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	snapshots := filepath.Join(dir, "snapshots")
	db, err := sql.Open("sqlite", filepath.Join(dir, "live.db"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	if _, _, err := (&store.Runner{DB: db, Migrations: migrations, SnapshotDir: snapshots}).Apply(context.Background()); err != nil {
		return nil, err
	}
	for _, stmt := range append([]string{
		`INSERT INTO machine_key_check (id, check_value, created_at) VALUES (1, x'01', 't')`,
		`INSERT INTO array_settings (id, create_policy, min_free_space, created_at) VALUES (1, 'mfs', '20G', '2026-09-01T00:00:00Z')`,
		`INSERT INTO array_disks (role, role_index, device, filesystem, fs_uuid, serial, weak_identity, mountpoint) VALUES
			('data', 1, '/dev/sda', 'xfs', 'u1', 'WD-WCC4E1234567', 0, '/mnt/disk1'),
			('data', 2, '/dev/sdz', 'xfs', 'u2', 'WD-NOT-ATTACHED', 0, '/mnt/disk2')`,
		`INSERT INTO notify_channels (id, name, type, enabled, config, secret, created_at, updated_at) VALUES ('c1', 'ops', 'gotify', 1, '{}', x'cc', 't', 't')`,
	}, extra...) {
		if _, err := db.Exec(stmt); err != nil {
			return nil, fmt.Errorf("%s: %w", stmt, err)
		}
	}
	staging := filepath.Join(dir, "staging")
	if err := os.Mkdir(staging, 0o700); err != nil {
		return nil, err
	}
	var (
		src    backup.SecretSource
		opts   []backup.ArchiveOption
		cipher backup.SecretCipher
	)
	if sealed {
		id, err := age.GenerateX25519Identity()
		if err != nil {
			return nil, err
		}
		credential, _ := backup.FakeSecretCipher{}.Encrypt([]byte("gotify-token"))
		src = &backup.FakeSecretSource{Passphrase: contractArchivePassphrase, HasPass: true,
			Secrets: []backup.DatabaseSecret{{Table: "notify_channels", Column: "secret", RowID: "c1", Ciphertext: credential}}}
		cipher = backup.FakeSecretCipher{}
		opts = append(opts, backup.WithRecipient(&backup.Recipient{Public: id.Recipient().String(), Identity: id.String()}))
	}
	if _, err := backup.BuildArchive(context.Background(), db, backup.Paths{}, src, cipher, "host", "test", time.Now(), staging, opts...); err != nil {
		return nil, err
	}
	return packArchive(staging)
}

func mockErr(t *testing.T, err error, status int, code string) {
	t.Helper()
	var me *mockError
	if !errors.As(err, &me) || me.statusCode != status || me.code != code {
		t.Fatalf("err = %v, want %d %s", err, status, code)
	}
}

func mockImport(archive []byte, mapping string) *apiv1.ImportConfigReq {
	req := &apiv1.ImportConfigReq{Confirm: true, Archive: ht.MultipartFile{File: bytes.NewReader(archive)}}
	if mapping != "" {
		req.DiskMapping = apiv1.NewOptString(mapping)
	}
	return req
}

// The fresh-install scenario has no array, so the mock takes the bare-metal
// branch on the production code's own staging and mapping, and answers as
// production does: the mapping to confirm, 409 without it, 409 for a stale
// one, and a report that names what was not restored.
func TestMockConfigImport_FreshInstallTakesTheBareMetalBranch(t *testing.T) {
	ctx := context.Background()
	archive := mockBareMetalArchive(t)
	h, err := newHandler("fresh-install")
	if err != nil {
		t.Fatal(err)
	}

	p, err := h.PreviewConfigImport(ctx, &apiv1.PreviewConfigImportReq{Archive: ht.MultipartFile{File: bytes.NewReader(archive)}})
	if err != nil {
		t.Fatalf("PreviewConfigImport: %v", err)
	}
	bm, ok := p.BareMetal.Get()
	if !ok || len(bm.Disks) != 2 || len(p.Blockers) != 0 {
		t.Fatalf("preview = %+v, want a bareMetal block with the archive's two disks and no blocker", p)
	}
	states := map[apiv1.ConfigImportDiskState]string{}
	for _, d := range bm.Disks {
		states[d.State] = d.Device.Or("")
	}
	if states[apiv1.ConfigImportDiskStateMatched] != "/dev/sdb" || states[apiv1.ConfigImportDiskStateAbsent] != "" {
		t.Fatalf("disk states = %v, want data 1 matched on /dev/sdb and data 2 absent", states)
	}
	if len(bm.DiskMapping.Disks) != 1 || bm.DiskMapping.Disks[0].Device != "/dev/sdb" {
		t.Fatalf("the mapping to confirm = %+v, want only the matched disk", bm.DiskMapping)
	}

	_, err = h.ImportConfig(ctx, mockImport(archive, ""))
	mockErr(t, err, 409, "disk_mapping_required")
	_, err = h.ImportConfig(ctx, mockImport(archive, `{"disks":[{"role":"data","roleIndex":1,"device":"/dev/sdc"}]}`))
	mockErr(t, err, 409, "disk_mapping_stale")
	_, err = h.ImportConfig(ctx, mockImport(archive, `{"disks":[]}`))
	mockErr(t, err, 409, "disk_mapping_stale")
	_, err = h.ImportConfig(ctx, mockImport(archive, `not a mapping`))
	mockErr(t, err, 400, "invalid_disk_mapping")

	report, err := h.ImportConfig(ctx, mockImport(archive, `{"disks":[{"role":"data","roleIndex":1,"device":"/dev/sdb"}]}`))
	if err != nil {
		t.Fatalf("ImportConfig with the confirmed mapping: %v", err)
	}
	var reasons []string
	for _, n := range report.NotRestored {
		reasons = append(reasons, string(n.Kind)+" "+n.Name+" "+string(n.Reason))
	}
	got := strings.Join(reasons, "\n")
	for _, want := range []string{"disk data disk 2 (/mnt/disk2, serial WD-NOT-ATTACHED) disk_absent", "database_secret notify_channels.secret (ops) sealed_under_other_key"} {
		if !strings.Contains(got, want) {
			t.Errorf("notRestored lacks %q:\n%s", want, got)
		}
	}
}

func TestMockConfigImport_ANewerArchiveIsRefusedByNameOnAFreshInstall(t *testing.T) {
	ctx := context.Background()
	archive := mockBareMetalArchive(t, `INSERT INTO schema_migrations (version, slug, checksum, applied_at) VALUES ('99999999999999', 'from_the_future', 'x', '2099-01-01T00:00:00Z')`)
	h, err := newHandler("fresh-install")
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.ImportConfig(ctx, mockImport(archive, `{"disks":[]}`))
	mockErr(t, err, 409, "archive_newer_version")
	p, err := h.PreviewConfigImport(ctx, &apiv1.PreviewConfigImportReq{Archive: ht.MultipartFile{File: bytes.NewReader(archive)}})
	if err != nil {
		t.Fatal(err)
	}
	if p.BareMetal.Set || len(p.Blockers) != 1 || p.Blockers[0].Code != apiv1.ConfigImportBlockerCodeArchiveNewerVersion {
		t.Fatalf("preview = blockers %+v, bareMetal %v, want the one archive_newer_version blocker", p.Blockers, p.BareMetal.Set)
	}
}

// A scenario with an array takes no mapping, and shows no bareMetal block.
func TestMockConfigImport_AnInstallationWithAnArrayRefusesAMappingAndShowsNoBareMetalBlock(t *testing.T) {
	ctx := context.Background()
	archive := mockBareMetalArchive(t)
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.ImportConfig(ctx, mockImport(archive, `{"disks":[]}`))
	mockErr(t, err, 409, "disk_mapping_not_applicable")
	p, err := h.PreviewConfigImport(ctx, &apiv1.PreviewConfigImportReq{Archive: ht.MultipartFile{File: bytes.NewReader(archive)}})
	if err != nil || p.BareMetal.Set {
		t.Fatalf("preview = %+v (err %v), want no bareMetal block", p, err)
	}
}

// The preview's host_files_replaced note promises a saved copy, so with every
// backup destination removed the mock refuses the restore as production does.
func TestMockConfigImport_FreshInstallWithoutABackupDestinationRefusesToReplaceHostFiles(t *testing.T) {
	ctx := context.Background()
	archive := mockBareMetalArchive(t)
	mapping := `{"disks":[{"role":"data","roleIndex":1,"device":"/dev/sdb"}]}`
	h, err := newHandler("fresh-install")
	if err != nil {
		t.Fatal(err)
	}
	dests, err := h.ListBackupDestinations(ctx)
	if err != nil || len(dests.Destinations) == 0 {
		t.Fatalf("ListBackupDestinations = %+v, %v, want the default destinations", dests, err)
	}
	for _, d := range dests.Destinations {
		if err := h.DeleteBackupDestination(ctx, apiv1.DeleteBackupDestinationParams{DestinationId: d.ID}); err != nil {
			t.Fatalf("DeleteBackupDestination(%s): %v", d.ID, err)
		}
	}
	_, err = h.ImportConfig(ctx, mockImport(archive, mapping))
	mockErr(t, err, 409, "host_files_not_saved")
}

// A bare-metal restore with the archive's passphrase seals the archive's
// secrets under this installation's key, so they are not reported as not
// restored, and adopts its recipient; without it, or with one that does not
// open the archive, the mock answers as production does.
func TestMockConfigImport_TheArchivesPassphraseRestoresItsSecretsOnAFreshInstall(t *testing.T) {
	ctx := context.Background()
	archive, err := buildMockSealedArchive()
	if err != nil {
		t.Fatal(err)
	}
	mapping := `{"disks":[{"role":"data","roleIndex":1,"device":"/dev/sdb"}]}`
	h, err := newHandler("fresh-install")
	if err != nil {
		t.Fatal(err)
	}
	withPass := func(p string) *apiv1.ImportConfigReq {
		req := mockImport(archive, mapping)
		req.Passphrase = apiv1.NewOptString(p)
		return req
	}

	_, err = h.ImportConfig(ctx, withPass("not the passphrase"))
	mockErr(t, err, 400, "backup_passphrase_incorrect")

	p, err := h.PreviewConfigImport(ctx, &apiv1.PreviewConfigImportReq{Archive: ht.MultipartFile{File: bytes.NewReader(archive)}, Passphrase: apiv1.NewOptString(contractArchivePassphrase)})
	if err != nil {
		t.Fatalf("PreviewConfigImport with the archive's passphrase: %v", err)
	}
	if p.Secrets.Status != apiv1.ConfigImportSecretsStatusOpened || p.Secrets.Identity.Or("") != apiv1.ConfigImportSecretsStatusOpened {
		t.Fatalf("preview secrets = %+v, want secrets.age and identity.age opened", p.Secrets)
	}
	p, err = h.PreviewConfigImport(ctx, &apiv1.PreviewConfigImportReq{Archive: ht.MultipartFile{File: bytes.NewReader(archive)}})
	if err != nil {
		t.Fatalf("PreviewConfigImport without a passphrase: %v", err)
	}
	if p.Secrets.Status != apiv1.ConfigImportSecretsStatusNoPassphrase || p.Secrets.Identity.Or("") != apiv1.ConfigImportSecretsStatusNoPassphrase {
		t.Fatalf("preview secrets without a passphrase = %+v, want no_passphrase for both", p.Secrets)
	}

	report, err := h.ImportConfig(ctx, withPass(contractArchivePassphrase))
	if err != nil {
		t.Fatalf("ImportConfig with the archive's passphrase: %v", err)
	}
	for _, n := range report.NotRestored {
		if n.Kind == apiv1.ConfigImportNotRestoredKindDatabaseSecret || n.Kind == apiv1.ConfigImportNotRestoredKindBackupRecipient {
			t.Errorf("the report lists %s %s as not restored", n.Kind, n.Name)
		}
	}

	// The passphrase is now the mock's configured one, as production's is.
	report, err = h.ImportConfig(ctx, mockImport(archive, mapping))
	if err != nil || report.Secrets != apiv1.ConfigImportSecretsStatusOpened {
		t.Fatalf("ImportConfig with the now configured passphrase = %+v (err %v), want the secrets opened", report, err)
	}

	fresh, err := newHandler("fresh-install")
	if err != nil {
		t.Fatal(err)
	}
	report, err = fresh.ImportConfig(ctx, mockImport(archive, mapping))
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[apiv1.ConfigImportNotRestoredKind]bool{}
	for _, n := range report.NotRestored {
		kinds[n.Kind] = true
		if n.Kind == apiv1.ConfigImportNotRestoredKindBackupRecipient && n.Reason != apiv1.ConfigImportNotRestoredReasonNoPassphrase {
			t.Errorf("recipient reason = %s, want no_passphrase", n.Reason)
		}
	}
	if !kinds[apiv1.ConfigImportNotRestoredKindBackupRecipient] || !kinds[apiv1.ConfigImportNotRestoredKindDatabaseSecret] {
		t.Errorf("without a passphrase the report lists %v, want the cleared secret and the kept recipient", report.NotRestored)
	}
}
