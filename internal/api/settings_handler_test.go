package api_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/auth"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

type fakeSettingsCipher struct{}

func (fakeSettingsCipher) Encrypt(plaintext []byte) ([]byte, error) {
	out := make([]byte, len(plaintext))
	for i, b := range plaintext {
		out[i] = b ^ 0x5a
	}
	return out, nil
}

func (fakeSettingsCipher) Decrypt(ciphertext []byte) ([]byte, error) {
	return fakeSettingsCipher{}.Encrypt(ciphertext)
}

func newSettingsTestHandler(t *testing.T) (*api.Handler, *api.SettingsService) {
	t.Helper()

	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "settings-handler-test.db")
	db, err := sql.Open("sqlite", store.DSN(dbPath))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}

	svc := api.NewSettingsService(api.NewSettingsStore(db), fakeSettingsCipher{})
	return &api.Handler{Settings: svc}, svc
}

func TestHandlerGetGeneralSettingsNeverReturnsPassphrase(t *testing.T) {
	ctx := context.Background()
	h, _ := newSettingsTestHandler(t)

	got, err := h.GetGeneralSettings(ctx)
	if err != nil {
		t.Fatalf("GetGeneralSettings: %v", err)
	}
	if got.BackupPassphraseSet {
		t.Fatal("expected backupPassphraseSet false on a fresh install")
	}

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	if string(raw) != `{"backupPassphraseSet":false}` {
		t.Fatalf("GET response = %s, want no passphrase field", raw)
	}
}

func TestHandlerUpdateGeneralSettingsStoresPassphraseForSecretSource(t *testing.T) {
	ctx := context.Background()
	h, svc := newSettingsTestHandler(t)

	updated, err := h.UpdateGeneralSettings(ctx, &apiv1.UpdateGeneralSettingsRequest{
		Hostname:         apiv1.NewOptString("nas"),
		Timezone:         apiv1.NewOptString("Europe/Berlin"),
		BackupPassphrase: apiv1.NewOptString("correct horse battery staple"),
	})
	if err != nil {
		t.Fatalf("UpdateGeneralSettings: %v", err)
	}
	if !updated.BackupPassphraseSet {
		t.Fatal("expected backupPassphraseSet true after setting passphrase")
	}
	if v, ok := updated.Hostname.Get(); !ok || v != "nas" {
		t.Fatalf("hostname = %v", updated.Hostname)
	}

	src := &backup.ServiceSecretSource{BackupPassphraseFn: svc.BackupPassphrase}
	passphrase, ok, err := src.BackupPassphrase(ctx)
	if err != nil {
		t.Fatalf("BackupPassphrase: %v", err)
	}
	if !ok || passphrase != "correct horse battery staple" {
		t.Fatalf("BackupPassphrase = (%q, %v), want (correct horse battery staple, true)", passphrase, ok)
	}

	got, err := h.GetGeneralSettings(ctx)
	if err != nil {
		t.Fatalf("GetGeneralSettings: %v", err)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	if string(raw) != `{"hostname":"nas","timezone":"Europe/Berlin","backupPassphraseSet":true}` {
		t.Fatalf("GET response = %s", raw)
	}
}

func TestHandlerUpdateGeneralSettingsOmitPassphraseLeavesUnset(t *testing.T) {
	ctx := context.Background()
	h, _ := newSettingsTestHandler(t)

	_, err := h.UpdateGeneralSettings(ctx, &apiv1.UpdateGeneralSettingsRequest{
		Timezone: apiv1.NewOptString("UTC"),
	})
	if err != nil {
		t.Fatalf("UpdateGeneralSettings: %v", err)
	}

	got, err := h.GetGeneralSettings(ctx)
	if err != nil {
		t.Fatalf("GetGeneralSettings: %v", err)
	}
	if got.BackupPassphraseSet {
		t.Fatal("expected backupPassphraseSet false when passphrase omitted")
	}
}

func TestHandlerUpdateGeneralSettingsRejectsWhitespaceHostname(t *testing.T) {
	ctx := context.Background()
	h, _ := newSettingsTestHandler(t)

	_, err := h.UpdateGeneralSettings(ctx, &apiv1.UpdateGeneralSettingsRequest{
		Hostname: apiv1.NewOptString("   "),
	})
	if err == nil {
		t.Fatal("expected error for whitespace-only hostname")
	}
}

func TestSettingsServiceEncryptsPassphraseAtRest(t *testing.T) {
	ctx := context.Background()
	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "settings-cipher-test.db")
	db, err := sql.Open("sqlite", store.DSN(dbPath))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}

	key, err := auth.LoadOrGenerateMachineKey(context.Background(), filepath.Join(t.TempDir(), "secret.key"), &auth.FakeMachineKeyStore{})
	if err != nil {
		t.Fatalf("LoadOrGenerateMachineKey: %v", err)
	}
	svc := api.NewSettingsService(api.NewSettingsStore(db), key)
	if _, err := svc.Update(ctx, api.UpdateGeneralSettingsInput{
		BackupPassphrase: strPtr("hunter2"),
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	row, err := api.NewSettingsStore(db).Get(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(row.BackupPassphrase) == 0 {
		t.Fatal("expected encrypted backup passphrase blob")
	}
	if string(row.BackupPassphrase) == "hunter2" {
		t.Fatal("backup passphrase stored in plaintext")
	}

	passphrase, ok, err := svc.BackupPassphrase(ctx)
	if err != nil {
		t.Fatalf("BackupPassphrase: %v", err)
	}
	if !ok || passphrase != "hunter2" {
		t.Fatalf("BackupPassphrase = (%q, %v)", passphrase, ok)
	}
}

func strPtr(s string) *string { return &s }
