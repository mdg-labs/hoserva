package backup

import (
	"context"
	"testing"
	"time"

	"filippo.io/age"
)

func TestLoadOrGenerateRecipient_GeneratesOnceAndPersists(t *testing.T) {
	ctx := context.Background()
	store := &FakeRecipientStore{}
	cipher := FakeSecretCipher{}
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

	first, err := LoadOrGenerateRecipient(ctx, cipher, store, func() time.Time { return now })
	if err != nil {
		t.Fatalf("first LoadOrGenerateRecipient: %v", err)
	}
	if first.Public == "" || first.Identity == "" {
		t.Fatalf("expected a generated public/identity pair, got %+v", first)
	}
	if _, err := age.ParseX25519Recipient(first.Public); err != nil {
		t.Fatalf("generated public recipient does not parse: %v", err)
	}
	identity, err := age.ParseX25519Identity(first.Identity)
	if err != nil {
		t.Fatalf("generated identity does not parse: %v", err)
	}
	if identity.Recipient().String() != first.Public {
		t.Fatalf("identity's own recipient %q does not match stored public %q", identity.Recipient().String(), first.Public)
	}
	if !store.Found {
		t.Fatal("expected the row to be persisted after first generation")
	}

	// A second call against the same store (simulating a daemon restart)
	// must load the same keypair, never generate a new one.
	second, err := LoadOrGenerateRecipient(ctx, cipher, store, func() time.Time { return now })
	if err != nil {
		t.Fatalf("second LoadOrGenerateRecipient: %v", err)
	}
	if second.Public != first.Public || second.Identity != first.Identity {
		t.Fatalf("second call regenerated a different keypair: got %+v, want %+v", second, first)
	}
}

func TestLoadOrGenerateRecipient_CorruptedCheckValueIsFatal(t *testing.T) {
	ctx := context.Background()
	store := &FakeRecipientStore{}
	cipher := FakeSecretCipher{}

	if _, err := LoadOrGenerateRecipient(ctx, cipher, store, nil); err != nil {
		t.Fatalf("generating recipient: %v", err)
	}

	// Tamper with the stored check value, as if the row (or the machine
	// key that produced wrapped_identity) had been corrupted or swapped.
	store.CheckValue = append([]byte(nil), store.CheckValue...)
	store.CheckValue[0] ^= 0xff

	if _, err := LoadOrGenerateRecipient(ctx, cipher, store, nil); err == nil {
		t.Fatal("expected a fatal mismatch error for a corrupted check value, got nil")
	}
}

func TestLoadOrGenerateRecipient_TamperedPublicRecipientIsFatal(t *testing.T) {
	ctx := context.Background()
	store := &FakeRecipientStore{}
	cipher := FakeSecretCipher{}

	if _, err := LoadOrGenerateRecipient(ctx, cipher, store, nil); err != nil {
		t.Fatalf("generating recipient: %v", err)
	}

	// Swap in a different, syntactically valid recipient string without
	// touching wrapped_identity or check_value — the check value ties the
	// two together, so this must still be caught rather than silently
	// encrypting future archives to the wrong public key.
	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generating unrelated identity: %v", err)
	}
	store.Public = other.Recipient().String()

	if _, err := LoadOrGenerateRecipient(ctx, cipher, store, nil); err == nil {
		t.Fatal("expected a fatal mismatch error for a tampered public recipient, got nil")
	}
}

func TestLoadOrGenerateRecipient_RequiresCipherAndStore(t *testing.T) {
	ctx := context.Background()
	if _, err := LoadOrGenerateRecipient(ctx, nil, &FakeRecipientStore{}, nil); err == nil {
		t.Fatal("expected an error with a nil cipher")
	}
	if _, err := LoadOrGenerateRecipient(ctx, FakeSecretCipher{}, nil, nil); err == nil {
		t.Fatal("expected an error with a nil store")
	}
}
