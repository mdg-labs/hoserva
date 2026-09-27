package api_test

import (
	"context"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/backup"
)

// TestBackupRecipientStore_RoundTripsThroughRealDatabase proves
// BackupRecipientStore (#275, Q80) persists and reads back the
// backup_recipient singleton row through the real, migrated schema — not
// just the sqlc-generated query shapes in isolation.
func TestBackupRecipientStore_RoundTripsThroughRealDatabase(t *testing.T) {
	db := openTestDB(t)
	store := api.NewBackupRecipientStore(db)
	ctx := context.Background()

	_, _, _, found, err := store.GetRecipient(ctx)
	if err != nil {
		t.Fatalf("GetRecipient before any row exists: %v", err)
	}
	if found {
		t.Fatal("expected found=false before SetRecipient")
	}

	createdAt := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	wrapped := []byte{0x01, 0x02, 0x03}
	checkValue := []byte{0xaa, 0xbb}
	if err := store.SetRecipient(ctx, "age1exampleexampleexample", wrapped, checkValue, createdAt); err != nil {
		t.Fatalf("SetRecipient: %v", err)
	}

	public, gotWrapped, gotCheck, found, err := store.GetRecipient(ctx)
	if err != nil {
		t.Fatalf("GetRecipient after SetRecipient: %v", err)
	}
	if !found {
		t.Fatal("expected found=true after SetRecipient")
	}
	if public != "age1exampleexampleexample" {
		t.Fatalf("public_recipient = %q", public)
	}
	if string(gotWrapped) != string(wrapped) {
		t.Fatalf("wrapped_identity = %v, want %v", gotWrapped, wrapped)
	}
	if string(gotCheck) != string(checkValue) {
		t.Fatalf("check_value = %v, want %v", gotCheck, checkValue)
	}
}

// TestBackupRecipientStore_ImplementsRecipientCycle exercises the store
// through backup.LoadOrGenerateRecipient itself — the entry point that
// actually matters — against the real database, proving generation,
// persistence and a simulated restart (a second load against the same
// store) all agree.
func TestBackupRecipientStore_ImplementsRecipientCycle(t *testing.T) {
	db := openTestDB(t)
	store := api.NewBackupRecipientStore(db)
	ctx := context.Background()
	cipher := backup.FakeSecretCipher{}

	first, err := backup.LoadOrGenerateRecipient(ctx, cipher, store, nil)
	if err != nil {
		t.Fatalf("first LoadOrGenerateRecipient: %v", err)
	}
	if first.Public == "" || first.Identity == "" {
		t.Fatalf("expected a generated keypair, got %+v", first)
	}

	second, err := backup.LoadOrGenerateRecipient(ctx, cipher, store, nil)
	if err != nil {
		t.Fatalf("second LoadOrGenerateRecipient: %v", err)
	}
	if second.Public != first.Public || second.Identity != first.Identity {
		t.Fatalf("second load through the real database regenerated a different keypair: got %+v, want %+v", second, first)
	}
}
