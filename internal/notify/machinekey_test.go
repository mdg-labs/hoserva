package notify

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/mdg-labs/hoserva/internal/auth"
)

// TestMachineKeySatisfiesSecretCipher confirms the real Q28 encryption
// primitive — internal/auth.MachineKey — needs no adapter to satisfy
// SecretCipher: internal/api wires the same *auth.MachineKey instance
// AuthService already uses for TOTP secrets into a Service's Cipher
// field directly, never a second encryption mechanism.
func TestMachineKeySatisfiesSecretCipher(t *testing.T) {
	store := &auth.FakeMachineKeyStore{}
	key, err := auth.LoadOrGenerateMachineKey(context.Background(), filepath.Join(t.TempDir(), "secret.key"), store)
	if err != nil {
		t.Fatalf("LoadOrGenerateMachineKey: %v", err)
	}

	var cipher SecretCipher = key
	ciphertext, err := cipher.Encrypt([]byte("smtp-app-password"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if string(ciphertext) == "smtp-app-password" {
		t.Fatal("Encrypt returned the plaintext unchanged")
	}
	plaintext, err := cipher.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(plaintext) != "smtp-app-password" {
		t.Fatalf("Decrypt = %q, want smtp-app-password", plaintext)
	}
}
