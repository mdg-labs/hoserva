package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"

	"github.com/mdg-labs/hoserva/internal/acme"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/notify"
)

const notifyCredential = "webhook-credential-for-the-archive-test"

type archivedSecret struct {
	Table  string `json:"table"`
	Column string `json:"column"`
	RowID  string `json:"row_id"`
	Value  []byte `json:"value"`
}

// useProductionSecretSource points rig.svc at the secret source main.go
// builds, over the rig's own database.
func useProductionSecretSource(rig *backupRig) *notify.Service {
	rig.svc.PoolMounted = func(string) (bool, error) { return false, nil }
	rig.svc.Log = func(string, ...any) {}
	notifyStore := notify.NewStore(rig.db)
	rig.svc.Secrets = backupSecretSource(rig.settings, acme.NewStore(rig.db), api.NewUPSStore(rig.db), api.NewBackupDestinationStore(rig.db), notifyStore)
	return notify.NewService(notifyStore, rig.svc.Cipher.(notify.SecretCipher), nil)
}

func addChannel(t *testing.T, svc *notify.Service, name string, secret *notify.SecretInput) string {
	t.Helper()
	ch, err := svc.CreateChannel(context.Background(), notify.ChannelInput{
		Name: name, Type: notify.ChannelDiscord, Enabled: true, Secret: secret,
	})
	if err != nil {
		t.Fatalf("CreateChannel %s: %v", name, err)
	}
	return ch.ID
}

// stagedSecrets builds an archive's staging directory the way
// backup.Service does and returns its secrets.age, or nil when it has none.
func stagedSecrets(t *testing.T, rig *backupRig) []byte {
	t.Helper()
	staging := t.TempDir()
	if _, err := backup.BuildArchive(context.Background(), rig.db, rig.svc.Paths, rig.svc.Secrets, rig.svc.Cipher, "host", "0.0.0-test", rig.svc.Now(), staging); err != nil {
		t.Fatalf("BuildArchive: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(staging, "secrets.age"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func openSecrets(t *testing.T, data []byte, passphrase string) []archivedSecret {
	t.Helper()
	identity, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		t.Fatal(err)
	}
	r, err := age.Decrypt(bytes.NewReader(data), identity)
	if err != nil {
		t.Fatalf("decrypting secrets.age with the passphrase: %v", err)
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Database []archivedSecret `json:"database"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	return payload.Database
}

func TestBackupSecretSource_SecretsAgeCarriesEachNotificationChannelCredential(t *testing.T) {
	ctx := context.Background()
	rig := newBackupRig(t, "correct horse")
	notifySvc := useProductionSecretSource(rig)
	withCredential := addChannel(t, notifySvc, "with credential", &notify.SecretInput{Op: notify.SecretSet, Value: notifyCredential})
	addChannel(t, notifySvc, "without credential", nil)

	var found []archivedSecret
	for _, s := range openSecrets(t, stagedSecrets(t, rig), "correct horse") {
		if s.Table == "notify_channels" {
			found = append(found, s)
		}
	}
	if len(found) != 1 || found[0].Column != "secret" || found[0].RowID != withCredential {
		t.Fatalf("notify_channels entries in secrets.age = %d, want one for the channel that has a credential", len(found))
	}
	if string(found[0].Value) != notifyCredential {
		t.Fatal("the archived credential is not the one the channel was given")
	}

	if err := rig.svc.Run(ctx); err != nil {
		t.Fatalf("Run with a channel credential in the archive: %v", err)
	}
}

func TestBackupSecretSource_AChannelWithoutACredentialAddsNothingAndTheArchiveStillOpens(t *testing.T) {
	rig := newBackupRig(t, "correct horse")
	notifySvc := useProductionSecretSource(rig)
	addChannel(t, notifySvc, "without credential", nil)

	if got := stagedSecrets(t, rig); got != nil {
		t.Fatal("secrets.age is written although the database holds no credential")
	}
	if err := rig.svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestBackupSecretSource_NoPassphraseMeansNoSecretsAge(t *testing.T) {
	rig := newBackupRig(t, "")
	notifySvc := useProductionSecretSource(rig)
	addChannel(t, notifySvc, "with credential", &notify.SecretInput{Op: notify.SecretSet, Value: notifyCredential})

	if got := stagedSecrets(t, rig); got != nil {
		t.Fatal("an archive built without a backup passphrase carries a secrets.age")
	}
}
