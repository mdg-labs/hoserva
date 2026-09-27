package backup

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"filippo.io/age"
)

// Recipient is the onboarding age X25519 keypair archives are encrypted to
// (Q80). Public is the recipient string — safe to keep in the clear, and
// the only half the box needs for encryption. Identity is the matching
// private identity: it is never written to disk in the clear, only kept
// in memory for the life of the process and re-wrapped under the backup
// passphrase (buildIdentityAge) each time an archive is built.
type Recipient struct {
	Public   string
	Identity string
}

// RecipientCipher wraps and unwraps the recipient's private identity at
// rest under the machine key (Q28) — the same shape as auth.MachineKey's
// own Encrypt/Decrypt, declared narrowly here so this package doesn't
// import internal/auth directly (mirrors SecretCipher above).
type RecipientCipher interface {
	Encrypt(plaintext []byte) ([]byte, error)
	Decrypt(ciphertext []byte) ([]byte, error)
}

// RecipientStore persists the onboarding recipient's singleton row: the
// public recipient string in the clear, the private identity wrapped
// under the machine key, and a check value recorded the moment it was
// first generated (Q80, Q28-style).
type RecipientStore interface {
	// GetRecipient returns the stored row, and found=false if none has
	// ever been written (a fresh install, before its first recipient
	// exists).
	GetRecipient(ctx context.Context) (publicRecipient string, wrappedIdentity, checkValue []byte, found bool, err error)
	// SetRecipient writes the row the moment a recipient is first
	// generated. LoadOrGenerateRecipient never calls this a second time
	// for the same installation.
	SetRecipient(ctx context.Context, publicRecipient string, wrappedIdentity, checkValue []byte, createdAt time.Time) error
}

// recipientCheckConstant is the fixed plaintext computeRecipientCheckValue
// mixes in alongside the identity — like auth's own keyCheckConstant, its
// value carries no meaning of its own.
const recipientCheckConstant = "hoserva-backup-recipient-check-v1"

// ErrRecipientMismatch is returned by LoadOrGenerateRecipient when the
// stored row's check value does not match what its own public and private
// halves recompute — a fatal startup error, never a silent regeneration:
// a fresh recipient would no longer match the identity already wrapped
// into every past archive (docs/internal/13-open-questions.md's Q80
// entry).
var ErrRecipientMismatch = errors.New("backup: onboarding recipient check failed")

// LoadOrGenerateRecipient reads the onboarding recipient from store,
// generating one — exactly once per installation, at first start — the
// first time no row exists yet. Every call, whether it loads an existing
// row or generates a fresh one, is checked against the recorded check
// value: a row that was corrupted, or a decrypt that succeeds against the
// wrong machine key, must never be quietly treated as valid or silently
// replaced (mirrors auth.LoadOrGenerateMachineKey's own contract, Q28).
func LoadOrGenerateRecipient(ctx context.Context, cipher RecipientCipher, store RecipientStore, now func() time.Time) (*Recipient, error) {
	if cipher == nil {
		return nil, fmt.Errorf("backup: a RecipientCipher is required to load the onboarding recipient")
	}
	if store == nil {
		return nil, fmt.Errorf("backup: a RecipientStore is required to load the onboarding recipient")
	}

	public, wrapped, checkValue, found, err := store.GetRecipient(ctx)
	if err != nil {
		return nil, fmt.Errorf("backup: reading onboarding recipient: %w", err)
	}
	if found {
		plain, err := cipher.Decrypt(wrapped)
		if err != nil {
			return nil, fmt.Errorf("backup: decrypting onboarding recipient identity: %w", err)
		}
		identity := string(plain)
		if !hmac.Equal(checkValue, computeRecipientCheckValue(identity, public)) {
			return nil, fmt.Errorf("%w: the stored onboarding recipient does not match its recorded check value — see docs/internal/13-open-questions.md's Q80 entry before touching the backup_recipient row", ErrRecipientMismatch)
		}
		return &Recipient{Public: public, Identity: identity}, nil
	}

	id, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, fmt.Errorf("backup: generating onboarding recipient: %w", err)
	}
	identity := id.String()
	publicRecipient := id.Recipient().String()
	wrappedIdentity, err := cipher.Encrypt([]byte(identity))
	if err != nil {
		return nil, fmt.Errorf("backup: encrypting onboarding recipient identity: %w", err)
	}
	when := time.Now()
	if now != nil {
		when = now()
	}
	if err := store.SetRecipient(ctx, publicRecipient, wrappedIdentity, computeRecipientCheckValue(identity, publicRecipient), when); err != nil {
		return nil, fmt.Errorf("backup: persisting onboarding recipient: %w", err)
	}
	return &Recipient{Public: publicRecipient, Identity: identity}, nil
}

// computeRecipientCheckValue is an HMAC-SHA256 of the public recipient
// string and a fixed constant, keyed by the private identity string —
// never the identity alone, and not reversible back to it. Ties both
// halves of the row together: a mismatch means either the identity
// decrypted under the wrong key or the public_recipient column no longer
// matches the identity it was recorded alongside.
func computeRecipientCheckValue(identity, public string) []byte {
	mac := hmac.New(sha256.New, []byte(identity))
	mac.Write([]byte(public))
	mac.Write([]byte(recipientCheckConstant))
	return mac.Sum(nil)
}
