package backup

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"
)

func testRecipient(t *testing.T) *Recipient {
	t.Helper()
	r, err := LoadOrGenerateRecipient(context.Background(), FakeSecretCipher{}, &FakeRecipientStore{}, nil)
	if err != nil {
		t.Fatalf("generating test recipient: %v", err)
	}
	return r
}

func TestBuildEncryptedArtifacts_RoundTripsWithPassphraseOrIdentity(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "hoserva-config-2026-09-27T03-00.tar.zst")
	plaintext := []byte("plaintext archive contents")
	if err := os.WriteFile(archivePath, plaintext, 0o600); err != nil {
		t.Fatal(err)
	}

	recipient := testRecipient(t)
	passphrase := "correct horse battery staple"
	sidecar, err := encryptWithPassphrase([]byte(recipient.Identity), passphrase)
	if err != nil {
		t.Fatalf("encryptWithPassphrase: %v", err)
	}

	artifacts, err := buildEncryptedArtifacts(archivePath, recipient.Public, sidecar)
	if err != nil {
		t.Fatalf("buildEncryptedArtifacts: %v", err)
	}
	if artifacts.ArchivePath != archivePath+".age" {
		t.Fatalf("unexpected encrypted archive path %q", artifacts.ArchivePath)
	}
	if artifacts.SidecarPath != artifacts.ArchivePath+identitySidecarSuffix {
		t.Fatalf("unexpected sidecar path %q", artifacts.SidecarPath)
	}
	raw, err := os.ReadFile(artifacts.ArchivePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(raw, plaintext) {
		t.Fatal("encrypted archive must not equal the plaintext archive")
	}

	// Readable with the passphrase alone (Q80's "a restore needs only the
	// passphrase") via the sidecar, without ever touching
	// recipient.Identity directly.
	gotByPassphrase, err := decryptArchiveWithPassphrase(artifacts.ArchivePath, artifacts.SidecarPath, passphrase)
	if err != nil {
		t.Fatalf("decryptArchiveWithPassphrase: %v", err)
	}
	if !bytes.Equal(gotByPassphrase, plaintext) {
		t.Fatalf("decrypted (by passphrase) = %q, want %q", gotByPassphrase, plaintext)
	}

	// Also readable with the onboarding recipient's own private identity
	// — the box's own route back into an archive it wrote, with no
	// passphrase or sidecar involved.
	gotByIdentity, err := decryptArchiveWithRecipient(artifacts.ArchivePath, recipient)
	if err != nil {
		t.Fatalf("decryptArchiveWithRecipient: %v", err)
	}
	if !bytes.Equal(gotByIdentity, plaintext) {
		t.Fatalf("decrypted (by identity) = %q, want %q", gotByIdentity, plaintext)
	}

	// The wrong passphrase must not recover the identity or decrypt
	// anything.
	if _, err := decryptArchiveWithPassphrase(artifacts.ArchivePath, artifacts.SidecarPath, "wrong passphrase"); err == nil {
		t.Fatal("expected decryption to fail with the wrong passphrase")
	}
}

func TestBuildIdentityAge_OmittedWithoutRecipientOrPassphrase(t *testing.T) {
	ctx := context.Background()
	recipient := testRecipient(t)

	// No SecretSource at all.
	data, err := buildIdentityAge(ctx, nil, recipient)
	if err != nil || data != nil {
		t.Fatalf("expected nil, nil with no SecretSource; got %v, %v", data, err)
	}

	// A SecretSource with no passphrase configured.
	src := &FakeSecretSource{HasPass: false}
	data, err = buildIdentityAge(ctx, src, recipient)
	if err != nil || data != nil {
		t.Fatalf("expected nil, nil with no passphrase configured; got %v, %v", data, err)
	}

	// No recipient yet.
	src = &FakeSecretSource{Passphrase: "pw", HasPass: true}
	data, err = buildIdentityAge(ctx, src, nil)
	if err != nil || data != nil {
		t.Fatalf("expected nil, nil with no recipient; got %v, %v", data, err)
	}
}

func TestBuildIdentityAge_RoundTripsThroughTheSecretsAgeScryptPath(t *testing.T) {
	ctx := context.Background()
	recipient := testRecipient(t)
	src := &FakeSecretSource{Passphrase: "identity-pass", HasPass: true}

	data, err := buildIdentityAge(ctx, src, recipient)
	if err != nil {
		t.Fatalf("buildIdentityAge: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("expected identity.age content")
	}

	// identity.age is wrapped through the exact same scrypt path
	// (encryptWithPassphrase) buildSecretsAge uses for secrets.age, so
	// plain age.NewScryptIdentity decryption — the same primitive
	// decryptIdentitySidecar uses underneath — must reverse it directly.
	scryptIdentity, err := age.NewScryptIdentity("identity-pass")
	if err != nil {
		t.Fatalf("creating scrypt identity: %v", err)
	}
	r, err := age.Decrypt(bytes.NewReader(data), scryptIdentity)
	if err != nil {
		t.Fatalf("decrypting identity.age: %v", err)
	}
	plain, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading decrypted identity.age: %v", err)
	}
	if string(plain) != recipient.Identity {
		t.Fatalf("decrypted identity.age = %q, want %q", plain, recipient.Identity)
	}
}
