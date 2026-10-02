package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	ht "github.com/ogen-go/ogen/http"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/acme"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/auth"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/notify"
	"github.com/mdg-labs/hoserva/internal/store"
)

const (
	secretsPassphrase = "the archive's backup passphrase"
	secretsPassword   = "correct horse battery staple"
)

// sourceSecrets is what the source installation sealed under its own machine
// key, by "table.column.row", as plaintext.
type sourceSecrets map[string]string

func sealedRow(t *testing.T, key *auth.MachineKey, plain string) []byte {
	t.Helper()
	sealed, err := key.Encrypt([]byte(plain))
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

// newSourceWithSecrets is another installation with a backup passphrase set
// and one of every secret the archive carries, each sealed under its own
// machine key: ACME, UPS, backup-destination and notification-channel
// credentials, an enrolled TOTP, and stacks with an .env each. It returns the
// archive it exports.
func newSourceWithSecrets(t *testing.T, stacks []string) (*wiredInstall, []byte, sourceSecrets) {
	t.Helper()
	ctx := context.Background()
	src := newWiredInstall(t)
	populateSourceInstall(t, src)
	pass := secretsPassphrase
	if _, err := src.settings.Update(ctx, api.UpdateGeneralSettingsInput{BackupPassphrase: &pass}); err != nil {
		t.Fatal(err)
	}

	plain := sourceSecrets{
		"acme_config.dns_secret.1":       "dns-token",
		"acme_config.account_key.1":      "account-key",
		"ups_config.monitor_password.1":  "ups-monitor",
		"backup_destinations.secrets.d1": `{"access_key":"s3-secret"}`,
	}
	if _, err := src.db.ExecContext(ctx, `INSERT INTO acme_config (id, domain, provider, provider_config, dns_secret, account_key, enabled, updated_at)
		VALUES (1, 'nas.example.org', 'cloudflare', '{}', ?, ?, 1, '2026-09-01T00:00:00Z')`,
		sealedRow(t, src.key, plain["acme_config.dns_secret.1"]), sealedRow(t, src.key, plain["acme_config.account_key.1"])); err != nil {
		t.Fatal(err)
	}
	if _, err := src.db.ExecContext(ctx, `INSERT INTO ups_config (id, connection, driver, port, monitor_password, network_host, network_port, network_ups_name, network_username, network_password, low_battery_percent, runtime_seconds, updated_at)
		VALUES (1, 'usb', 'usbhid-ups', 'auto', ?, '', 0, '', '', X'', 20, 300, '2026-09-01T00:00:00Z')`, sealedRow(t, src.key, plain["ups_config.monitor_password.1"])); err != nil {
		t.Fatal(err)
	}
	if _, err := src.db.ExecContext(ctx, `INSERT INTO backup_destinations (id, name, type, path, options, secrets, enabled, encrypt, retention_daily, retention_weekly, retention_monthly, created_at)
		VALUES ('d1', 'offsite', 's3', 'bucket', '{}', ?, 1, 1, 7, 4, 6, '2026-09-01T00:00:00Z')`, sealedRow(t, src.key, plain["backup_destinations.secrets.d1"])); err != nil {
		t.Fatal(err)
	}
	channel, err := notify.NewService(notify.NewStore(src.db), src.key, nil).CreateChannel(ctx, notify.ChannelInput{
		Name: "ops", Type: notify.ChannelDiscord, Enabled: true, Secret: &notify.SecretInput{Op: notify.SecretSet, Value: "webhook-credential"},
	})
	if err != nil {
		t.Fatal(err)
	}
	plain["notify_channels.secret."+channel.ID] = "webhook-credential"
	if err := (&container.RegistryCredentials{Store: store.NewRegistryCredentialStore(src.db), Cipher: src.key}).
		PutCredential(ctx, "ghcr.io", container.Credential{Username: "me", Password: "registry-password"}); err != nil {
		t.Fatal(err)
	}
	plain["registry_credentials.credential.ghcr.io"] = `{"username":"me","password":"registry-password"}`
	if _, err := src.db.ExecContext(ctx, `UPDATE users SET totp_secret = ?, totp_pending_secret = ?, totp_confirmed_at = '2026-09-01T00:00:00Z', totp_last_step = 5 WHERE username = 'alice'`,
		sealedRow(t, src.key, "JBSWY3DPEHPK3PXP"), sealedRow(t, src.key, "PENDINGPENDING")); err != nil {
		t.Fatal(err)
	}
	for _, name := range stacks {
		dir := filepath.Join(src.stateDir, "stacks", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("TOKEN="+name+"-from-the-archive\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return src, exportArchive(t, src.handler), plain
}

// attachSourceDisks attaches, on other device names, the disks the source's
// array records.
func (w *wiredInstall) attachSourceDisks() {
	w.attach("/dev/sdx", "uuid-p1", "wwn-p1")
	w.attach("/dev/sdy", "uuid-d1", "wwn-d1")
	w.attach("/dev/sdz", "uuid-d2", "wwn-d2")
}

// bareMetalImport is what the CLI does: preview to get the mapping to
// confirm, then import with it, through the generated client over the Unix
// socket. passphrase is nil to send none.
func bareMetalImport(t *testing.T, srv *wireClient, archive []byte, passphrase *string) (*apiv1.ConfigImportReport, *apiv1.ConfigImportPreview, error) {
	t.Helper()
	ctx := context.Background()
	client := srv.generated()
	file := func() ht.MultipartFile { return ht.MultipartFile{Name: "a.tar.zst", File: bytes.NewReader(archive)} }
	opt := apiv1.OptString{}
	if passphrase != nil {
		opt = apiv1.NewOptString(*passphrase)
	}
	preview, err := client.PreviewConfigImport(ctx, &apiv1.PreviewConfigImportReq{Archive: file(), Passphrase: opt})
	if err != nil {
		return nil, nil, err
	}
	bm, ok := preview.BareMetal.Get()
	if !ok {
		t.Fatalf("the preview has no bareMetal block: %+v", preview)
	}
	mapping, err := json.Marshal(bm.DiskMapping)
	if err != nil {
		t.Fatal(err)
	}
	report, err := client.ImportConfig(ctx, &apiv1.ImportConfigReq{Confirm: true, Archive: file(), DiskMapping: apiv1.NewOptString(string(mapping)), Passphrase: opt})
	return report, preview, err
}

// restart is the daemon's next start on the box's database: the migrations,
// this box's own machine key file against the database's check value, and the
// backup recipient against its check value. Either refusing is fatal to the
// daemon, so it is a test failure.
func restart(t *testing.T, box *wiredInstall) (*sql.DB, *auth.MachineKey, *backup.Recipient) {
	t.Helper()
	ctx := context.Background()
	if err := box.db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", store.DSN(box.dbPath))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := applyMigrations(ctx, db, box.stateDir); err != nil {
		t.Fatalf("the next start refuses the restored database: applying migrations: %v", err)
	}
	key, err := auth.LoadOrGenerateMachineKey(ctx, box.keyPath, api.NewAuthStore(db))
	if err != nil {
		t.Fatalf("the next start refuses the restored database: the machine key check: %v", err)
	}
	recipient, err := backup.LoadOrGenerateRecipient(ctx, key, api.NewBackupRecipientStore(db), time.Now)
	if err != nil {
		t.Fatalf("the next start refuses the restored database: the recipient check: %v", err)
	}
	return db, key, recipient
}

// secretsOf reads every database secret the way a config backup does
// (backupSecretSource) and decrypts each with key, so a secret that is not
// sealed under key is a failure.
func secretsOf(t *testing.T, db *sql.DB, key *auth.MachineKey) sourceSecrets {
	t.Helper()
	src := backupSecretSource(api.NewSettingsService(api.NewSettingsStore(db), key), acme.NewStore(db), api.NewUPSStore(db), api.NewBackupDestinationStore(db), notify.NewStore(db), store.NewRegistryCredentialStore(db))
	secrets, err := src.DatabaseSecrets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := sourceSecrets{}
	for _, s := range secrets {
		plain, err := key.Decrypt(s.Ciphertext)
		if err != nil {
			t.Fatalf("%s.%s row %s does not decrypt with this installation's machine key: %v", s.Table, s.Column, s.RowID, err)
		}
		out[s.Table+"."+s.Column+"."+s.RowID] = string(plain)
	}
	return out
}

func notRestoredHas(r *apiv1.ConfigImportReport, kind apiv1.ConfigImportNotRestoredKind, nameContains string) bool {
	for _, n := range r.NotRestored {
		if n.Kind == kind && strings.Contains(n.Name, nameContains) {
			return true
		}
	}
	return false
}

func strp(s string) *string { return &s }

// The passphrase that opens the archive restores everything it protects onto
// the fresh install: each database secret sealed under this box's key in its
// own row, the .env files, the backup passphrase and the backup recipient. The
// daemon starts on the result, and the archive the box writes next is
// encrypted to the archive's recipient.
func TestBareMetalRestore_TheArchivesPassphraseRestoresItsSecretsUnderThisBoxsKey(t *testing.T) {
	for name, tc := range map[string]struct {
		explicit   *string
		configured string
	}{
		"the passphrase given with the request": {explicit: strp(secretsPassphrase)},
		"the passphrase configured on the box":  {configured: secretsPassphrase},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			src, archive, want := newSourceWithSecrets(t, []string{"web"})
			box := newWiredInstall(t)
			box.attachSourceDisks()
			boxRecipient := box.recipient.Public
			if tc.configured != "" {
				if _, err := box.settings.Update(ctx, api.UpdateGeneralSettingsInput{BackupPassphrase: &tc.configured}); err != nil {
					t.Fatal(err)
				}
			}
			srv := box.serve(t)

			preview, _ := srv.generated().PreviewConfigImport(ctx, &apiv1.PreviewConfigImportReq{Archive: ht.MultipartFile{Name: "a", File: bytes.NewReader(archive)}, Passphrase: optOf(tc.explicit)})
			if preview == nil || preview.Secrets.Status != apiv1.ConfigImportSecretsStatusOpened || preview.Secrets.Identity.Or("") != apiv1.ConfigImportSecretsStatusOpened {
				t.Fatalf("preview secrets = %+v, want secrets.age and identity.age both reported opened", preview)
			}

			report, _, err := bareMetalImport(t, srv, archive, tc.explicit)
			if err != nil {
				t.Fatalf("import: %v", err)
			}
			if report.Secrets != apiv1.ConfigImportSecretsStatusOpened {
				t.Errorf("report secrets = %s, want opened", report.Secrets)
			}

			// The next archive the box writes, on the running daemon, is
			// encrypted to the archive's recipient and keeps its
			// installation id in its name.
			dest := filepath.Join(box.root, "encrypted")
			box.handler.Backup.Destinations = []backup.Destination{{ID: "enc", Name: "Encrypted", Path: dest, Enabled: true, Encrypt: true,
				Retention: backup.Retention{Daily: 7, Weekly: 4, Monthly: 6}}}
			box.handler.Backup.Now = func() time.Time { return time.Date(2026, 9, 28, 4, 17, 0, 0, time.UTC) }
			if err := box.handler.Backup.Run(ctx); err != nil {
				t.Fatalf("the box's next backup: %v", err)
			}
			archives, _ := filepath.Glob(filepath.Join(dest, "*.tar.zst.age"))
			if len(archives) != 1 {
				t.Fatalf("archives written = %v, want one", archives)
			}
			id, err := age.ParseX25519Identity(src.recipient.Identity)
			if err != nil {
				t.Fatal(err)
			}
			f, err := os.Open(archives[0])
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			r, err := age.Decrypt(f, id)
			if err != nil {
				t.Fatalf("the box's next archive does not open with the archive's identity: %v", err)
			}
			if _, err := io.Copy(io.Discard, r); err != nil {
				t.Fatal(err)
			}
			if got, want := archiveInstallationID(filepath.Base(archives[0])), sourceInstallationID(t, src); got != want {
				t.Errorf("the box's archive carries installation id %s, want the archive's %s", got, want)
			}

			// Each secret is the source's, sealed under this box's key.
			db2, key2, recipient2 := restart(t, box)
			got := secretsOf(t, db2, key2)
			for k, plain := range want {
				if got[k] != plain {
					t.Errorf("%s = %q on the restored box, want %q", k, got[k], plain)
				}
			}
			if len(got) != len(want) {
				t.Errorf("the box holds %d database secrets, want the archive's %d", len(got), len(want))
			}
			for k := range want {
				var ct []byte
				table, rest, _ := strings.Cut(k, ".")
				column, row, _ := strings.Cut(rest, ".")
				keyColumn := "id"
				if table == "registry_credentials" {
					keyColumn = "registry"
				}
				if err := db2.QueryRow(`SELECT `+column+` FROM `+table+` WHERE CAST(`+keyColumn+` AS TEXT) = ?`, row).Scan(&ct); err != nil {
					t.Fatalf("%s: %v", k, err)
				}
				if _, err := src.key.Decrypt(ct); err == nil {
					t.Errorf("%s is still sealed under the source's machine key", k)
				}
			}
			for _, n := range report.NotRestored {
				if n.Kind == apiv1.ConfigImportNotRestoredKindDatabaseSecret && !strings.HasPrefix(n.Name, "users.totp") {
					t.Errorf("the report lists %s as not restored", n.Name)
				}
				if n.Kind == apiv1.ConfigImportNotRestoredKindBackupRecipient {
					t.Errorf("the report lists the backup recipient as not restored: %+v", n)
				}
			}

			// The passphrase that opened the archive is the box's own.
			if p, ok, err := api.NewSettingsService(api.NewSettingsStore(db2), key2).BackupPassphrase(ctx); err != nil || !ok || p != secretsPassphrase {
				t.Errorf("backup passphrase = %q, %v, %v, want the archive's", p, ok, err)
			}

			// The .env file.
			envPath := filepath.Join(box.stateDir, "stacks", "web", ".env")
			if b, _ := os.ReadFile(envPath); string(b) != "TOKEN=web-from-the-archive\n" {
				t.Errorf(".env = %q, want the archive's", b)
			}
			if fi, err := os.Stat(envPath); err != nil || fi.Mode().Perm() != 0o600 {
				t.Errorf(".env mode = %v (%v), want 0600", fi.Mode().Perm(), err)
			}

			// The archive's recipient, its identity wrapped under this box's key.
			if recipient2.Public != src.recipient.Public || recipient2.Identity != src.recipient.Identity {
				t.Errorf("recipient after the restart = %s, want the archive's %s (this box's own was %s)", recipient2.Public, src.recipient.Public, boxRecipient)
			}
			if box.handler.Backup.Recipient.Public != src.recipient.Public {
				t.Errorf("the running daemon still encrypts to %s, want the archive's recipient", box.handler.Backup.Recipient.Public)
			}
			// TOTP is cleared: the next sign-in enrols again, not an internal error.
			authService := api.NewAuthService(api.NewAuthStore(db2), key2)
			u, _, err := authService.Login(ctx, "alice", secretsPassword, "", "")
			if err != nil {
				t.Fatalf("login after the restore = %v, want a sign-in that prompts to enrol TOTP again", err)
			}
			if u.TOTPEnrolled() {
				t.Error("alice's TOTP is still enrolled")
			}
			if !notRestoredHas(report, apiv1.ConfigImportNotRestoredKindDatabaseSecret, "users.totp_secret (alice)") {
				t.Errorf("the report does not list alice's TOTP: %+v", report.NotRestored)
			}
		})
	}
}

// archiveInstallationID is the installation id in an archive's name,
// hoserva-config-<id>-<timestamp>....
func archiveInstallationID(name string) string {
	parts := strings.Split(name, "-")
	if len(parts) < 3 {
		return ""
	}
	return parts[2]
}

// sourceInstallationID is the id the source's own archives carry.
func sourceInstallationID(t *testing.T, src *wiredInstall) string {
	t.Helper()
	src.handler.Backup.Now = func() time.Time { return time.Date(2026, 9, 28, 4, 11, 0, 0, time.UTC) }
	if err := src.handler.Backup.Run(context.Background()); err != nil {
		t.Fatalf("the source's backup: %v", err)
	}
	archives, _ := filepath.Glob(filepath.Join(src.root, "backups", "hoserva-config-*.tar.zst"))
	if len(archives) == 0 {
		t.Fatal("the source wrote no archive")
	}
	return archiveInstallationID(filepath.Base(archives[0]))
}

func optOf(p *string) apiv1.OptString {
	if p == nil {
		return apiv1.OptString{}
	}
	return apiv1.NewOptString(*p)
}

// A passphrase that does not open the archive is refused before anything is
// written; no passphrase, or a configured one that does not open it, restores
// everything else and clears and reports the secrets. In every case the next
// start accepts the database.
func TestBareMetalRestore_NeverLeavesADatabaseTheNextStartRefuses(t *testing.T) {
	ctx := context.Background()
	type outcome struct {
		refused                bool
		recipientKept          bool
		reportedRecipientCause apiv1.ConfigImportNotRestoredReason
	}
	for name, tc := range map[string]struct {
		explicit   *string
		configured string
		want       outcome
	}{
		"the correct passphrase":                {explicit: strp(secretsPassphrase)},
		"a wrong passphrase given":              {explicit: strp("not the passphrase"), want: outcome{refused: true}},
		"no passphrase":                         {want: outcome{recipientKept: true, reportedRecipientCause: apiv1.ConfigImportNotRestoredReasonNoPassphrase}},
		"a configured passphrase that is wrong": {configured: "not the passphrase", want: outcome{recipientKept: true, reportedRecipientCause: apiv1.ConfigImportNotRestoredReasonPassphraseIncorrect}},
	} {
		t.Run(name, func(t *testing.T) {
			_, archive, _ := newSourceWithSecrets(t, []string{"web"})
			box := newWiredInstall(t)
			box.attachSourceDisks()
			boxRecipient := box.recipient.Public
			if tc.configured != "" {
				if _, err := box.settings.Update(ctx, api.UpdateGeneralSettingsInput{BackupPassphrase: &tc.configured}); err != nil {
					t.Fatal(err)
				}
			}
			srv := box.serve(t)

			report, _, err := bareMetalImport(t, srv, archive, tc.explicit)
			if tc.want.refused {
				var status *apiv1.ErrorStatusCode
				if !errors.As(err, &status) || status.StatusCode != http.StatusBadRequest || status.Response.Code != "backup_passphrase_incorrect" {
					t.Fatalf("import = %v, want 400 backup_passphrase_incorrect", err)
				}
				if n := countRows(t, box.db, `SELECT COUNT(*) FROM shares`) + countRows(t, box.db, `SELECT COUNT(*) FROM users`); n != 0 || len(box.hooks) != 0 {
					t.Fatalf("a refused import wrote %d rows and ran hooks %v", n, box.hooks)
				}
				if _, err := os.Stat(filepath.Join(box.stateDir, "stacks", "web")); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("a refused import wrote a stack: %v", err)
				}
			} else if err != nil {
				t.Fatalf("import: %v", err)
			}

			db2, key2, recipient2 := restart(t, box)
			if tc.want.recipientKept || tc.want.refused {
				if recipient2.Public != boxRecipient {
					t.Errorf("recipient = %s, want this box's own %s", recipient2.Public, boxRecipient)
				}
			}
			if tc.want.recipientKept {
				if !notRestoredHas(report, apiv1.ConfigImportNotRestoredKindBackupRecipient, "backup_recipient") {
					t.Errorf("the report does not say the box kept its own recipient: %+v", report.NotRestored)
				}
				for _, n := range report.NotRestored {
					if n.Kind == apiv1.ConfigImportNotRestoredKindBackupRecipient && n.Reason != tc.want.reportedRecipientCause {
						t.Errorf("recipient reason = %s, want %s", n.Reason, tc.want.reportedRecipientCause)
					}
				}
				if got := secretsOf(t, db2, key2); len(got) != 0 {
					t.Errorf("the box holds secrets %v, want every one cleared", got)
				}
				if b, _ := os.ReadFile(filepath.Join(box.stateDir, "stacks", "web", ".env")); len(b) != 0 {
					t.Errorf(".env = %q, want none restored", b)
				}
				for _, k := range []string{"acme_config.dns_secret", "ups_config.monitor_password", "notify_channels.secret", "backup_destinations.secrets", "registry_credentials.credential"} {
					table, column, _ := strings.Cut(k, ".")
					if !notRestoredHas(report, apiv1.ConfigImportNotRestoredKindDatabaseSecret, table+"."+column) {
						t.Errorf("the report does not list %s: %+v", k, report.NotRestored)
					}
				}
			}
			if tc.want.recipientKept {
				// The registry stays listed, and the update check fails its
				// images with a reason rather than asking it anonymously.
				service := &container.RegistryCredentials{Store: store.NewRegistryCredentialStore(db2), Cipher: key2}
				if hosts, err := service.ListCredentialRegistries(ctx); err != nil || len(hosts) != 1 || hosts[0] != "ghcr.io" {
					t.Errorf("registries with a credential after the restore = %v, %v, want ghcr.io still listed", hosts, err)
				}
				if _, found, err := service.Credential(ctx, "ghcr.io"); found || !errors.Is(err, container.ErrCredentialUnusable) {
					t.Errorf("the cleared credential = found %v, %v, want container.ErrCredentialUnusable", found, err)
				}
			}
			if !tc.want.refused {
				// Whatever the passphrase did, the box starts and its admin can sign in.
				if _, _, err := api.NewAuthService(api.NewAuthStore(db2), key2).Login(ctx, "alice", secretsPassword, "", ""); err != nil {
					t.Fatalf("login after the restore: %v", err)
				}
			}
		})
	}
}

// The data-loss scenario: a failure while the stack .env files are written,
// after the database was restored. Every stack's files, .env included, are
// left wholly as they were, and the report says the import did not finish.
func TestBareMetalRestore_AFailureWhileTheEnvFilesAreWrittenLeavesEveryStackAsItWas(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a read-only directory does not stop root writing to it")
	}
	_, archive, _ := newSourceWithSecrets(t, []string{"aaa", "zzz"})
	box := newWiredInstall(t)
	box.attachSourceDisks()
	stacks := filepath.Join(box.stateDir, "stacks")
	for _, name := range []string{"aaa", "zzz"} {
		dir := filepath.Join(stacks, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for file, body := range map[string]string{"docker-compose.yml": "services: {old: {}}\n", ".env": "TOKEN=old-" + name + "\n"} {
			if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The last stack cannot be written to, so the failure comes after the
	// first stack's files were already put in place.
	locked := filepath.Join(stacks, "zzz")
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	before := stackTreeOf(t, stacks)
	srv := box.serve(t)

	_, _, err := bareMetalImport(t, srv, archive, strp(secretsPassphrase))
	var status *apiv1.ErrorStatusCode
	if !errors.As(err, &status) || status.StatusCode != http.StatusInternalServerError || status.Response.Code != "import_failed" ||
		!strings.Contains(status.Response.Message, "the database was restored but the import did not finish") {
		t.Fatalf("import = %v, want 500 import_failed after the database was restored", err)
	}
	if after := stackTreeOf(t, stacks); !equalMaps(before, after) {
		t.Errorf("the stacks are not wholly as they were:\nbefore: %v\nafter:  %v", before, after)
	}
	// The database is the restored one, and the next start accepts it.
	restart(t, box)
}

func stackTreeOf(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() {
			out[rel+"/"] = ""
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		out[rel] = fi.Mode().String() + " " + string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
