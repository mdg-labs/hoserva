package backup

import (
	"context"
)

// ServiceSecretSource reads the backup passphrase SettingsService persisted
// (Q28, #178). DatabaseSecrets is not implemented here — callers that need
// the secrets.age bundle supply database ciphertexts through a wrapper or a
// test fake until a later issue collects them from every encrypted column.
type ServiceSecretSource struct {
	BackupPassphraseFn func(ctx context.Context) (string, bool, error)
}

func (s *ServiceSecretSource) BackupPassphrase(ctx context.Context) (string, bool, error) {
	if s == nil || s.BackupPassphraseFn == nil {
		return "", false, nil
	}
	return s.BackupPassphraseFn(ctx)
}

func (s *ServiceSecretSource) DatabaseSecrets(ctx context.Context) ([]DatabaseSecret, error) {
	return nil, nil
}
