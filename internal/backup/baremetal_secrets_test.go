package backup

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/mdg-labs/hoserva/internal/disk"
)

// The secrets of the archive the tests below restore, by the (table, column,
// row) secrets.age names them with.
var archivedPlain = map[secretKey]string{
	{"acme_config", "dns_secret", "1"}:       "dns-token",
	{"acme_config", "account_key", "1"}:      "account-key",
	{"ups_config", "monitor_password", "1"}:  "ups-monitor",
	{"backup_destinations", "secrets", "d1"}: `{"access_key":"s3-secret"}`,
	{"notify_channels", "secret", "c1"}:      "gotify-token",
}

const (
	archivePassphrase = "the archive's passphrase"
	envBody           = "TOKEN=from-the-archive\n"
)

type secretsArchive struct {
	tree      string
	recipient *Recipient
}

// archiveWithSecrets is archiveOfAnotherInstallation with the archive's
// manifest, its secrets.age holding archivedPlain and a stack .env sealed
// under secretsPass, and its identity.age sealed under identityPass (no file
// when empty). A second notification channel, c2, holds a sealed credential
// secrets.age carries nothing for. The stack web holds a sealed env that
// secrets.age carries the .env of, and the stack bare one it carries none for.
func archiveWithSecrets(t *testing.T, secretsPass, identityPass string) secretsArchive {
	t.Helper()
	tree := archiveOfAnotherInstallation(t)
	db, err := sql.Open("sqlite", filepath.Join(tree, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO notify_channels (id, name, type, enabled, config, secret, created_at, updated_at) VALUES ('c2', 'ops2', 'gotify', 1, '{}', X'cd', 't', 't')`)
	mustExec(t, db, `INSERT INTO stacks (name, template_source, template_id, template_revision, compose, env, installed_at) VALUES ('web', '', '', '', 'services: {}', X'ef', 't')`)
	mustExec(t, db, `INSERT INTO stacks (name, template_source, template_id, template_revision, compose, env, installed_at) VALUES ('bare', '', '', '', 'services: {}', X'ef', 't')`)
	_ = db.Close()

	if err := writeManifest(filepath.Join(tree, "manifest.json"), buildManifest("host", "1", time.Now(), map[string]string{"stacks/web/docker-compose.yml": "x"})); err != nil {
		t.Fatal(err)
	}
	var dbSecrets []DatabaseSecret
	for k, v := range archivedPlain {
		sealed, _ := FakeSecretCipher{}.Encrypt([]byte(v))
		dbSecrets = append(dbSecrets, DatabaseSecret{Table: k.table, Column: k.column, RowID: k.rowID, Ciphertext: sealed})
	}
	sealed, err := buildSecretsAge(context.Background(), &FakeSecretSource{Passphrase: secretsPass, HasPass: true, Secrets: dbSecrets},
		FakeSecretCipher{}, []StackEnv{{Stack: "web", Body: []byte(envBody)}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "secrets.age"), sealed, 0o600); err != nil {
		t.Fatal(err)
	}

	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	rec := &Recipient{Public: id.Recipient().String(), Identity: id.String()}
	if identityPass != "" {
		sealedID, err := encryptWithPassphrase([]byte(rec.Identity), identityPass)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tree, "identity.age"), sealedID, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return secretsArchive{tree: tree, recipient: rec}
}

func configuredPassphrase(p string) SecretSource {
	if p == "" {
		return nil
	}
	return &FakeSecretSource{Passphrase: p, HasPass: true}
}

func resolve(t *testing.T, tree string, configured string, explicit *string) SecretsOutcome {
	t.Helper()
	out, err := ResolveSecrets(context.Background(), tree, configuredPassphrase(configured), explicit)
	if err != nil {
		t.Fatalf("ResolveSecrets: %v", err)
	}
	return out
}

func stageForSecrets(t *testing.T, tree string) (*BareMetal, *sql.DB, []MappedDisk) {
	t.Helper()
	live := openMigratedDB(t, filepath.Join(t.TempDir(), "live.db"))
	seedInstallation(t, live, "box")
	b, err := StageBareMetal(context.Background(), live, tree)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Discard)
	mapped := b.Map(attached(map[string]disk.Disk{
		"/dev/sdb": {WWN: "wwn-d1", Serial: "ser-d1", FSUUID: "uuid-d1"},
		"/dev/sdc": {WWN: "wwn-d2", Serial: "ser-d2", FSUUID: "uuid-d2"},
	}))
	return b, live, mapped
}

func stagedBlob(t *testing.T, db *sql.DB, q string, args ...any) []byte {
	t.Helper()
	var b []byte
	if err := db.QueryRow(q, args...).Scan(&b); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return b
}

func notRestoredNames(ns []NotRestored) []string {
	var out []string
	for _, n := range ns {
		out = append(out, n.Kind+" "+n.Name+" "+n.Reason)
	}
	return out
}

func TestBareMetalApply_WithThePassphraseSealsEverySecretUnderThisInstallationsKey(t *testing.T) {
	ctx := context.Background()
	arc := archiveWithSecrets(t, archivePassphrase, archivePassphrase)
	b, live, mapped := stageForSecrets(t, arc.tree)
	outcome := resolve(t, arc.tree, "", strPtr(archivePassphrase))

	notRestored, err := b.Apply(ctx, live, mapped, outcome, FakeSecretCipher{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	staged := openStagedRO(t, b.Path())

	for k, want := range archivedPlain {
		col := stagedBlob(t, staged, fmt.Sprintf(`SELECT %s FROM %s WHERE CAST(id AS TEXT) = ?`, k.column, k.table), k.rowID)
		got, err := FakeSecretCipher{}.Decrypt(col)
		if err != nil || string(got) != want {
			t.Errorf("%s.%s row %s = %q (%v), want %q sealed under this installation's key", k.table, k.column, k.rowID, got, err, want)
		}
		if bytes.Equal(col, []byte(want)) {
			t.Errorf("%s.%s row %s holds its plaintext", k.table, k.column, k.rowID)
		}
	}
	if got := stagedBlob(t, staged, `SELECT monitor_password FROM ups_config`); len(got) == 0 {
		t.Error("ups monitor password is empty")
	}
	if n := queryString(t, staged, `SELECT COUNT(*) FROM backup_destinations WHERE id = 'd2' AND length(secrets) > 0`); n != "0" {
		t.Errorf("a destination the archive has no secret for gained one")
	}

	pass, _ := FakeSecretCipher{}.Decrypt(stagedBlob(t, staged, `SELECT backup_passphrase FROM schema_info`))
	if string(pass) != archivePassphrase {
		t.Errorf("backup passphrase = %q, want the one that opened the archive", pass)
	}

	if got := queryString(t, staged, `SELECT public_recipient FROM backup_recipient`); got != arc.recipient.Public {
		t.Errorf("backup_recipient.public_recipient = %q, want the archive's %q", got, arc.recipient.Public)
	}
	identity, _ := FakeSecretCipher{}.Decrypt(stagedBlob(t, staged, `SELECT wrapped_identity FROM backup_recipient`))
	if string(identity) != arc.recipient.Identity {
		t.Error("backup_recipient.wrapped_identity does not unwrap, under this installation's key, to the archive's identity")
	}
	if check := stagedBlob(t, staged, `SELECT check_value FROM backup_recipient`); !bytes.Equal(check, computeRecipientCheckValue(arc.recipient.Identity, arc.recipient.Public)) {
		t.Error("backup_recipient.check_value is not the check LoadOrGenerateRecipient recomputes")
	}
	if got := queryString(t, staged, `SELECT CAST(check_value AS TEXT) FROM machine_key_check`); got != "box-check" {
		t.Errorf("machine_key_check = %q, want this installation's own", got)
	}

	names := notRestoredNames(notRestored)
	for _, w := range []string{
		"database_secret users.totp_secret (alice) sealed_under_other_key",
		"database_secret users.totp_pending_secret (alice) sealed_under_other_key",
		"database_secret notify_channels.secret (ops2) sealed_under_other_key",
		"database_secret stacks.env (bare) sealed_under_other_key",
		"disk parity disk 1 (/mnt/parity1, WWN wwn-p1) disk_absent",
	} {
		if !slices.Contains(names, w) {
			t.Errorf("notRestored lacks %q; has %v", w, names)
		}
	}
	env, err := FakeSecretCipher{}.Decrypt(stagedBlob(t, staged, `SELECT env FROM stacks WHERE name = 'web'`))
	if err != nil || string(env) != envBody {
		t.Errorf("stacks.env of web = %q (%v), want the archive's .env sealed under this installation's key", env, err)
	}
	if n := queryString(t, staged, `SELECT COUNT(*) FROM stacks WHERE name = 'bare' AND length(env) > 0`); n != "0" {
		t.Error("the env of a stack secrets.age carries no .env for was left sealed under the archive's key")
	}
	for _, n := range notRestored {
		if n.Kind == NotRestoredDatabaseSecret && strings.HasPrefix(n.Name, "stacks.env") && strings.Contains(n.Message, "enter it again") {
			t.Errorf("%s: %q tells the user to enter a value no operation takes", n.Name, n.Message)
		}
	}
	for _, n := range names {
		for _, restored := range []string{"acme_config", "ups_config", "backup_destinations", "notify_channels.secret (ops)", "stacks.env (web)", "backup_passphrase", NotRestoredRecipient} {
			if strings.Contains(n, restored) {
				t.Errorf("notRestored lists %q, which was restored", n)
			}
		}
	}
	if n := queryString(t, staged, `SELECT COUNT(*) FROM users WHERE totp_secret IS NOT NULL OR totp_pending_secret IS NOT NULL OR totp_confirmed_at IS NOT NULL OR totp_last_step != 0`); n != "0" {
		t.Error("a user's TOTP enrolment survived")
	}
	if n := queryString(t, staged, `SELECT COUNT(*) FROM notify_channels WHERE id = 'c2' AND secret IS NOT NULL`); n != "0" {
		t.Error("a channel credential the archive carries nothing for was left sealed under the archive's key")
	}
}

// Without a passphrase that opens the archive nothing is restored from its
// passphrase-protected files: every secret is cleared and reported, and this
// installation keeps its own recipient and reports that it did.
func TestBareMetalApply_WithoutAPassphraseClearsEverySecretAndKeepsTheOwnRecipient(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		configured string
		explicit   *string
		reason     string
	}{
		"no passphrase":          {"", nil, NotRestoredNoPassphrase},
		"a configured wrong one": {"not the archive's", nil, NotRestoredPassphraseIncorrect},
	} {
		t.Run(name, func(t *testing.T) {
			arc := archiveWithSecrets(t, archivePassphrase, archivePassphrase)
			b, live, mapped := stageForSecrets(t, arc.tree)
			outcome := resolve(t, arc.tree, tc.configured, tc.explicit)

			notRestored, err := b.Apply(ctx, live, mapped, outcome, FakeSecretCipher{})
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			staged := openStagedRO(t, b.Path())
			for _, q := range []string{
				`SELECT COUNT(*) FROM notify_channels WHERE secret IS NOT NULL`,
				`SELECT COUNT(*) FROM acme_config WHERE length(dns_secret) > 0 OR length(account_key) > 0`,
				`SELECT COUNT(*) FROM ups_config WHERE length(monitor_password) > 0`,
				`SELECT COUNT(*) FROM backup_destinations WHERE length(secrets) > 0`,
				`SELECT COUNT(*) FROM stacks WHERE length(env) > 0`,
				`SELECT COUNT(*) FROM schema_info WHERE backup_passphrase IS NOT NULL`,
			} {
				if got := queryString(t, staged, q); got != "0" {
					t.Errorf("%s = %s, want the secret columns cleared", q, got)
				}
			}
			for col, want := range map[string]string{"public_recipient": "age1box", "CAST(wrapped_identity AS TEXT)": "box-wrapped", "CAST(check_value AS TEXT)": "box-recipient-check"} {
				if got := queryString(t, staged, `SELECT `+col+` FROM backup_recipient`); got != want {
					t.Errorf("backup_recipient %s = %q, want this installation's own %q", col, got, want)
				}
			}
			found := false
			for _, n := range notRestored {
				if n.Kind == NotRestoredRecipient {
					found = n.Reason == tc.reason
				}
			}
			if !found {
				t.Errorf("notRestored = %v, want the backup recipient kept and reported as %s", notRestoredNames(notRestored), tc.reason)
			}
			stacksReported := 0
			for _, n := range notRestored {
				if n.Kind == NotRestoredDatabaseSecret && strings.HasPrefix(n.Name, "stacks.env") {
					stacksReported++
					if strings.Contains(n.Message, "enter it again") {
						t.Errorf("%s: %q tells the user to enter a value no operation takes", n.Name, n.Message)
					}
				}
			}
			if stacksReported != 2 {
				t.Errorf("notRestored = %v, want both stacks' cleared env reported", notRestoredNames(notRestored))
			}
		})
	}
}

// A failure while the secrets are written leaves the staged database exactly
// as it was staged: nothing is half restored.
func TestBareMetalApply_AFailureWhileWritingTheSecretsLeavesTheStagedDatabaseUntouched(t *testing.T) {
	ctx := context.Background()
	for name, trigger := range map[string]string{
		"a destination's secret": `CREATE TRIGGER fail_write BEFORE UPDATE OF secrets ON backup_destinations BEGIN SELECT RAISE(ABORT, 'injected'); END`,
		"the passphrase":         `CREATE TRIGGER fail_write BEFORE UPDATE OF backup_passphrase ON schema_info WHEN NEW.backup_passphrase IS NOT NULL BEGIN SELECT RAISE(ABORT, 'injected'); END`,
		"the recipient":          `CREATE TRIGGER fail_write BEFORE INSERT ON backup_recipient BEGIN SELECT RAISE(ABORT, 'injected'); END`,
	} {
		t.Run(name, func(t *testing.T) {
			arc := archiveWithSecrets(t, archivePassphrase, archivePassphrase)
			b, live, mapped := stageForSecrets(t, arc.tree)
			staged, err := openStaged(b.Path(), "rw")
			if err != nil {
				t.Fatal(err)
			}
			mustExec(t, staged, trigger)
			_ = staged.Close()
			before := fileSum(t, b.Path())

			if _, err := b.Apply(ctx, live, mapped, resolve(t, arc.tree, "", strPtr(archivePassphrase)), FakeSecretCipher{}); err == nil || !strings.Contains(err.Error(), "injected") {
				t.Fatalf("Apply = %v, want the injected failure", err)
			}
			if fileSum(t, b.Path()) != before {
				t.Error("the staged database changed although Apply failed")
			}
		})
	}
}

type failingCipher struct{}

func (failingCipher) Encrypt([]byte) ([]byte, error) { return nil, errors.New("the key is gone") }
func (failingCipher) Decrypt([]byte) ([]byte, error) { return nil, errors.New("the key is gone") }

// What cannot be sealed, or placed, changes nothing: the seals are made before
// the first write and the writes are one transaction.
func TestBareMetalApply_WhatItCannotSealOrPlaceLeavesTheStagedDatabaseUntouched(t *testing.T) {
	ctx := context.Background()
	t.Run("a cipher that cannot seal", func(t *testing.T) {
		arc := archiveWithSecrets(t, archivePassphrase, archivePassphrase)
		b, live, mapped := stageForSecrets(t, arc.tree)
		before := fileSum(t, b.Path())
		if _, err := b.Apply(ctx, live, mapped, resolve(t, arc.tree, "", strPtr(archivePassphrase)), failingCipher{}); err == nil || !strings.Contains(err.Error(), "the key is gone") {
			t.Fatalf("Apply = %v, want the cipher's failure", err)
		}
		if fileSum(t, b.Path()) != before {
			t.Error("the staged database changed")
		}
	})
	t.Run("no cipher at all", func(t *testing.T) {
		arc := archiveWithSecrets(t, archivePassphrase, archivePassphrase)
		b, live, mapped := stageForSecrets(t, arc.tree)
		before := fileSum(t, b.Path())
		if _, err := b.Apply(ctx, live, mapped, resolve(t, arc.tree, "", strPtr(archivePassphrase)), nil); err == nil {
			t.Fatal("Apply sealed the archive's secrets with no machine key")
		}
		if fileSum(t, b.Path()) != before {
			t.Error("the staged database changed")
		}
	})
	t.Run("a secret for a column this Hoserva does not restore", func(t *testing.T) {
		arc := archiveWithSecrets(t, archivePassphrase, archivePassphrase)
		sealed, _ := FakeSecretCipher{}.Encrypt([]byte("x"))
		src := &FakeSecretSource{Passphrase: archivePassphrase, HasPass: true, Secrets: []DatabaseSecret{{Table: "users", Column: "password_hash", RowID: "u1", Ciphertext: sealed}}}
		data, err := buildSecretsAge(ctx, src, FakeSecretCipher{}, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(arc.tree, "secrets.age"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		b, live, mapped := stageForSecrets(t, arc.tree)
		before := fileSum(t, b.Path())
		if _, err := b.Apply(ctx, live, mapped, resolve(t, arc.tree, "", strPtr(archivePassphrase)), FakeSecretCipher{}); err == nil || !strings.Contains(err.Error(), "users.password_hash") {
			t.Fatalf("Apply = %v, want the unplaceable secret named", err)
		}
		if fileSum(t, b.Path()) != before {
			t.Error("the staged database changed")
		}
	})
}

// An archive whose installation never touched its settings has no
// schema_info row: the passphrase still becomes this installation's, in the
// row the settings service would have created.
func TestBareMetalApply_AnArchiveWithNoInstallationRowStillGetsThePassphrase(t *testing.T) {
	arc := archiveWithSecrets(t, archivePassphrase, archivePassphrase)
	db, err := sql.Open("sqlite", filepath.Join(arc.tree, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `DELETE FROM schema_info`)
	_ = db.Close()
	b, live, mapped := stageForSecrets(t, arc.tree)
	if _, err := b.Apply(context.Background(), live, mapped, resolve(t, arc.tree, "", strPtr(archivePassphrase)), FakeSecretCipher{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	staged := openStagedRO(t, b.Path())
	pass, _ := FakeSecretCipher{}.Decrypt(stagedBlob(t, staged, `SELECT backup_passphrase FROM schema_info WHERE id = 1`))
	if string(pass) != archivePassphrase {
		t.Errorf("backup passphrase = %q, want the archive's", pass)
	}
	if queryString(t, staged, `SELECT installation_id FROM schema_info`) == "" {
		t.Error("the installation row has no installation id")
	}
}

func TestResolveSecrets_IdentityOpensWithThePassphraseThatOpensTheSecrets(t *testing.T) {
	both := archiveWithSecrets(t, archivePassphrase, archivePassphrase)
	secretsOnly := archiveWithSecrets(t, archivePassphrase, "")
	otherIdentity := archiveWithSecrets(t, archivePassphrase, "another passphrase")
	identityOnly := archiveWithSecrets(t, archivePassphrase, archivePassphrase)
	if err := os.Remove(filepath.Join(identityOnly.tree, "secrets.age")); err != nil {
		t.Fatal(err)
	}
	neither := archiveWithSecrets(t, archivePassphrase, "")
	if err := os.Remove(filepath.Join(neither.tree, "secrets.age")); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name       string
		tree       string
		configured string
		explicit   *string
		wantErr    error
		secrets    string
		identity   string
		recipient  bool
	}{
		{"explicit, both open", both.tree, "", strPtr(archivePassphrase), nil, SecretsOpened, SecretsOpened, true},
		{"configured, both open", both.tree, archivePassphrase, nil, nil, SecretsOpened, SecretsOpened, true},
		{"nothing configured", both.tree, "", nil, nil, SecretsNoPassphrase, SecretsNoPassphrase, false},
		{"configured is wrong", both.tree, "wrong", nil, nil, SecretsPassphraseIncorrect, SecretsPassphraseIncorrect, false},
		{"explicit is wrong", both.tree, "", strPtr("wrong"), ErrPassphraseIncorrect, "", "", false},
		{"explicit is wrong although the right one is configured", both.tree, archivePassphrase, strPtr("wrong"), ErrPassphraseIncorrect, "", "", false},
		{"no identity.age", secretsOnly.tree, "", strPtr(archivePassphrase), nil, SecretsOpened, SecretsNone, false},
		{"identity.age under another passphrase", otherIdentity.tree, "", strPtr(archivePassphrase), nil, SecretsOpened, SecretsPassphraseIncorrect, false},
		{"identity.age alone, explicit right", identityOnly.tree, "", strPtr(archivePassphrase), nil, SecretsNone, SecretsOpened, true},
		{"identity.age alone, explicit wrong", identityOnly.tree, "", strPtr("wrong"), ErrPassphraseIncorrect, "", "", false},
		{"identity.age alone, configured wrong", identityOnly.tree, "wrong", nil, nil, SecretsNone, SecretsPassphraseIncorrect, false},
		{"neither file, explicit wrong is nothing to refuse", neither.tree, "", strPtr("wrong"), nil, SecretsNone, SecretsNone, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := ResolveSecrets(context.Background(), tc.tree, configuredPassphrase(tc.configured), tc.explicit)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ResolveSecrets = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveSecrets: %v", err)
			}
			if out.Status != tc.secrets || out.Identity != tc.identity {
				t.Errorf("status = %s, identity = %s, want %s, %s", out.Status, out.Identity, tc.secrets, tc.identity)
			}
			if got := out.Recipient() != nil; got != tc.recipient {
				t.Errorf("recipient opened = %v, want %v", got, tc.recipient)
			}
			if r := out.Recipient(); r != nil && (r.Identity != both.recipient.Identity && r.Identity != identityOnly.recipient.Identity) {
				t.Errorf("the recovered identity is not one the archive was given")
			}
		})
	}
}

func TestReadIdentity(t *testing.T) {
	arc := archiveWithSecrets(t, archivePassphrase, archivePassphrase)
	got, err := ReadIdentity(arc.tree, archivePassphrase)
	if err != nil || *got != *arc.recipient {
		t.Fatalf("ReadIdentity = %+v, %v, want the archive's recipient %+v", got, err, arc.recipient)
	}
	if _, err := ReadIdentity(arc.tree, "wrong"); !errors.Is(err, ErrPassphraseIncorrect) {
		t.Fatalf("ReadIdentity(wrong passphrase) = %v, want ErrPassphraseIncorrect", err)
	}
	if _, err := ReadIdentity(archiveWithSecrets(t, archivePassphrase, "").tree, archivePassphrase); !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("ReadIdentity(no file) = %v, want ErrNoIdentity", err)
	}
	notAnIdentity, err := encryptWithPassphrase([]byte("not an age identity"), archivePassphrase)
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{"not an age file": []byte("junk"), "an age file that holds no identity": notAnIdentity} {
		if err := os.WriteFile(filepath.Join(arc.tree, "identity.age"), content, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadIdentity(arc.tree, archivePassphrase); !errors.Is(err, ErrIdentityUnreadable) || errors.Is(err, ErrPassphraseIncorrect) {
			t.Errorf("ReadIdentity(%s) = %v, want ErrIdentityUnreadable", name, err)
		}
	}
}

// age's parser reports a bad character as its code and position ("s[48]=98"),
// which is a byte of the identity.
func TestReadIdentity_MalformedIdentityErrorQuotesNothingOfIt(t *testing.T) {
	arc := archiveWithSecrets(t, archivePassphrase, archivePassphrase)
	for _, plain := range []string{
		"AGE-SECRET-KEY-1QQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQBQQQ",
		"AGE-SECRET-KEY-1QQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQ QQ",
		"a tampered line the parser might quote: hunter2-s3cr3t",
	} {
		sealed, err := encryptWithPassphrase([]byte(plain), archivePassphrase)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(arc.tree, "identity.age"), sealed, 0o600); err != nil {
			t.Fatal(err)
		}
		_, readErr := ReadIdentity(arc.tree, archivePassphrase)
		_, resolveErr := ResolveSecrets(context.Background(), arc.tree, configuredPassphrase(archivePassphrase), nil)
		for name, err := range map[string]error{"ReadIdentity": readErr, "ResolveSecrets": resolveErr} {
			if !errors.Is(err, ErrIdentityUnreadable) {
				t.Fatalf("%s(%q) = %v, want ErrIdentityUnreadable", name, plain, err)
			}
			if !strings.Contains(err.Error(), "identity.age holds no valid identity") {
				t.Errorf("%s(%q) = %q, want the fixed message", name, plain, err)
			}
			for _, part := range []string{"s[", "malformed", "hunter2", "QQQQ"} {
				if strings.Contains(err.Error(), part) {
					t.Errorf("%s(%q) error contains %q of the parser's text or the identity: %v", name, plain, part, err)
				}
			}
		}
	}
}
