package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"filippo.io/age"
)

// SecretCipher decrypts machine-key-encrypted secret columns (Q28).
type SecretCipher interface {
	Decrypt(ciphertext []byte) ([]byte, error)
}

// DatabaseSecret is one encrypted secret column row from the main database.
type DatabaseSecret struct {
	Table      string
	Column     string
	RowID      string
	Ciphertext []byte
}

// StackEnv is one stack's .env file (Q80) — never written into the archive
// in plain text.
type StackEnv struct {
	Stack string
	Body  []byte
}

// SecretSource supplies secrets for the passphrase-protected section.
type SecretSource interface {
	BackupPassphrase(ctx context.Context) (string, bool, error)
	DatabaseSecrets(ctx context.Context) ([]DatabaseSecret, error)
}

// secretsPayload is the JSON cleartext encrypted into secrets.age.
type secretsPayload struct {
	Database []databaseSecretEntry `json:"database"`
	Stacks   []stackEnvEntry       `json:"stacks"`
}

type databaseSecretEntry struct {
	Table  string `json:"table"`
	Column string `json:"column"`
	RowID  string `json:"row_id"`
	Value  []byte `json:"value"`
}

type stackEnvEntry struct {
	Stack string `json:"stack"`
	Body  []byte `json:"body"`
}

// buildSecretsAge decrypts machine-key ciphertexts, collects stack .env
// files, and re-encrypts the bundle under the backup passphrase with age
// scrypt (Q28, Q80). When no passphrase is configured, secrets.age is
// omitted entirely — a restore without it restores everything except
// secrets, as doc 10 §1 describes.
func buildSecretsAge(ctx context.Context, src SecretSource, cipher SecretCipher, stackEnvs []StackEnv) ([]byte, error) {
	if src == nil {
		return nil, nil
	}
	passphrase, ok, err := src.BackupPassphrase(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading backup passphrase: %w", err)
	}
	if !ok || passphrase == "" {
		return nil, nil
	}

	dbSecrets, err := src.DatabaseSecrets(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing database secrets: %w", err)
	}

	payload := secretsPayload{}
	for _, s := range dbSecrets {
		if len(s.Ciphertext) == 0 {
			continue
		}
		plain, err := cipher.Decrypt(s.Ciphertext)
		if err != nil {
			return nil, fmt.Errorf("decrypting %s.%s row %s: %w", s.Table, s.Column, s.RowID, err)
		}
		payload.Database = append(payload.Database, databaseSecretEntry{
			Table:  s.Table,
			Column: s.Column,
			RowID:  s.RowID,
			Value:  plain,
		})
	}
	for _, env := range stackEnvs {
		if len(env.Body) == 0 {
			continue
		}
		payload.Stacks = append(payload.Stacks, stackEnvEntry(env))
	}
	if len(payload.Database) == 0 && len(payload.Stacks) == 0 {
		return nil, nil
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encoding secrets payload: %w", err)
	}
	return encryptWithPassphrase(raw, passphrase)
}

func encryptWithPassphrase(plain []byte, passphrase string) ([]byte, error) {
	recipient, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return nil, fmt.Errorf("creating age scrypt recipient: %w", err)
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, recipient)
	if err != nil {
		return nil, fmt.Errorf("starting age encryption: %w", err)
	}
	if _, err := io.Copy(w, bytes.NewReader(plain)); err != nil {
		return nil, fmt.Errorf("encrypting secrets payload: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("finalizing age encryption: %w", err)
	}
	return buf.Bytes(), nil
}

func decryptSecretsAge(data []byte, passphrase string) (*secretsPayload, error) {
	identity, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return nil, fmt.Errorf("creating age scrypt identity: %w", err)
	}
	r, err := age.Decrypt(bytes.NewReader(data), identity)
	if err != nil {
		return nil, fmt.Errorf("decrypting secrets.age: %w", err)
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("reading decrypted secrets payload: %w", err)
	}
	var payload secretsPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("parsing secrets payload: %w", err)
	}
	return &payload, nil
}
