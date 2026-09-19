package notify

import (
	"errors"
	"fmt"
)

// SecretCipher encrypts a channel credential before Store persists it, and
// decrypts it again before Service sends through it (Q28). Its shape is
// exactly *auth.MachineKey's own Encrypt/Decrypt methods — this package
// declares its own minimal interface instead of importing internal/auth
// directly, so internal/notify stays independent of internal/api's own
// auth wiring (mirroring internal/auth.MachineKeyStore's own reasoning for
// not depending on internal/store): internal/api constructs the real
// *auth.MachineKey once and passes it here as this interface, the same
// instance AuthService already uses for TOTP secrets, rather than this
// package inventing a second encryption path.
type SecretCipher interface {
	Encrypt(plaintext []byte) ([]byte, error)
	Decrypt(ciphertext []byte) ([]byte, error)
}

// ErrSecretCipherRequired is returned by Service methods that touch a
// channel credential when no SecretCipher was configured — a programming
// error (NewService was called without one), never a runtime condition a
// caller should retry.
var ErrSecretCipherRequired = errors.New("notify: a SecretCipher is required to read or write a channel credential")

// encryptSecret encrypts value under cipher, wrapping any failure with
// context. A nil cipher is ErrSecretCipherRequired, not a panic — every
// caller reaches this only when a caller actually supplied a secret to
// store.
func encryptSecret(cipher SecretCipher, value string) ([]byte, error) {
	if cipher == nil {
		return nil, ErrSecretCipherRequired
	}
	ciphertext, err := cipher.Encrypt([]byte(value))
	if err != nil {
		return nil, fmt.Errorf("notify: encrypting channel credential: %w", err)
	}
	return ciphertext, nil
}

// decryptSecret reverses encryptSecret.
func decryptSecret(cipher SecretCipher, ciphertext []byte) (string, error) {
	if cipher == nil {
		return "", ErrSecretCipherRequired
	}
	plaintext, err := cipher.Decrypt(ciphertext)
	if err != nil {
		return "", fmt.Errorf("notify: decrypting channel credential: %w", err)
	}
	return string(plaintext), nil
}

// FakeSecretCipher is a scriptable SecretCipher for tests (CLAUDE.md) —
// a fixed-width XOR, not real cryptography, reversible without a key file
// so tests never need internal/auth.MachineKey just to round-trip a
// channel credential. Tests that actually need to confirm ciphertext at
// rest is unreadable (Q28) use the real *auth.MachineKey instead, which
// already satisfies this interface.
type FakeSecretCipher struct{}

var _ SecretCipher = FakeSecretCipher{}

const fakeSecretCipherKey = 0x5a

func (FakeSecretCipher) Encrypt(plaintext []byte) ([]byte, error) {
	return xorBytes(plaintext), nil
}

func (FakeSecretCipher) Decrypt(ciphertext []byte) ([]byte, error) {
	return xorBytes(ciphertext), nil
}

func xorBytes(in []byte) []byte {
	out := make([]byte, len(in))
	for i, b := range in {
		out[i] = b ^ fakeSecretCipherKey
	}
	return out
}
