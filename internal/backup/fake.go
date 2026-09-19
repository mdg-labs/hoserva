package backup

import "context"

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
