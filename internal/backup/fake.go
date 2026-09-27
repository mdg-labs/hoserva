package backup

import (
	"context"
	"time"
)

// FakeSecretSource is a scriptable SecretSource for tests (CLAUDE.md).
type FakeSecretSource struct {
	Passphrase string
	HasPass    bool
	Secrets    []DatabaseSecret
}

func (f *FakeSecretSource) BackupPassphrase(ctx context.Context) (string, bool, error) {
	return f.Passphrase, f.HasPass, nil
}

func (f *FakeSecretSource) DatabaseSecrets(ctx context.Context) ([]DatabaseSecret, error) {
	return f.Secrets, nil
}

// FakeSecretCipher is a reversible XOR cipher for tests — not real crypto.
type FakeSecretCipher struct{}

func (FakeSecretCipher) Decrypt(ciphertext []byte) ([]byte, error) {
	out := make([]byte, len(ciphertext))
	for i, b := range ciphertext {
		out[i] = b ^ 0x5a
	}
	return out, nil
}

// Encrypt reverses Decrypt's XOR (it is its own inverse), so
// FakeSecretCipher also satisfies RecipientCipher for tests that need
// both directions.
func (FakeSecretCipher) Encrypt(plaintext []byte) ([]byte, error) {
	return FakeSecretCipher{}.Decrypt(plaintext)
}

// FakeRecipientStore is a scriptable RecipientStore for tests (CLAUDE.md)
// — no database involved. A caller shares one instance across two
// LoadOrGenerateRecipient calls to simulate the row persisting across a
// daemon restart, the same way a real database would.
type FakeRecipientStore struct {
	Public          string
	WrappedIdentity []byte
	CheckValue      []byte
	Found           bool
}

var _ RecipientStore = (*FakeRecipientStore)(nil)

func (f *FakeRecipientStore) GetRecipient(ctx context.Context) (string, []byte, []byte, bool, error) {
	return f.Public, f.WrappedIdentity, f.CheckValue, f.Found, nil
}

func (f *FakeRecipientStore) SetRecipient(ctx context.Context, publicRecipient string, wrappedIdentity, checkValue []byte, createdAt time.Time) error {
	f.Public = publicRecipient
	f.WrappedIdentity = wrappedIdentity
	f.CheckValue = checkValue
	f.Found = true
	return nil
}
