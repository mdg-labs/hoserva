package notify

import "testing"

func TestFakeSecretCipherRoundTrips(t *testing.T) {
	c := FakeSecretCipher{}
	ciphertext, err := c.Encrypt([]byte("hunter2"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if string(ciphertext) == "hunter2" {
		t.Fatal("Encrypt returned the plaintext unchanged")
	}
	plaintext, err := c.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(plaintext) != "hunter2" {
		t.Fatalf("Decrypt = %q, want hunter2", plaintext)
	}
}

func TestEncryptDecryptSecretRequireCipher(t *testing.T) {
	if _, err := encryptSecret(nil, "x"); err != ErrSecretCipherRequired {
		t.Fatalf("encryptSecret(nil, ...): err = %v, want ErrSecretCipherRequired", err)
	}
	if _, err := decryptSecret(nil, []byte("x")); err != ErrSecretCipherRequired {
		t.Fatalf("decryptSecret(nil, ...): err = %v, want ErrSecretCipherRequired", err)
	}
}
