package backup

import (
	"context"
)

// ServiceSecretSource reads the backup passphrase SettingsService persisted
// (Q28, #178) and optional encrypted database columns (ACME keys, Q28).
type ServiceSecretSource struct {
	BackupPassphraseFn func(ctx context.Context) (string, bool, error)
	DatabaseSecretsFn  func(ctx context.Context) ([]DatabaseSecret, error)
}

func (s *ServiceSecretSource) BackupPassphrase(ctx context.Context) (string, bool, error) {
	if s == nil || s.BackupPassphraseFn == nil {
		return "", false, nil
	}
	return s.BackupPassphraseFn(ctx)
}

func (s *ServiceSecretSource) DatabaseSecrets(ctx context.Context) ([]DatabaseSecret, error) {
	if s == nil || s.DatabaseSecretsFn == nil {
		return nil, nil
	}
	return s.DatabaseSecretsFn(ctx)
}
