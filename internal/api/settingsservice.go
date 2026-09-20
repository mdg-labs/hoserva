package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrInvalidSettingsInput is returned by SettingsService when a caller
// supplies a value the API rejects (whitespace-only hostname, unknown
// timezone).
var ErrInvalidSettingsInput = errors.New("settings: invalid input")

// SettingsCipher encrypts the backup passphrase before SettingsStore
// persists it, and decrypts it again for backup.SecretSource (Q28). Its
// shape is exactly *auth.MachineKey's own Encrypt/Decrypt methods — this
// package declares its own minimal interface instead of importing
// internal/auth directly, so internal/api wires the same *auth.MachineKey
// instance AuthService and notify.Service already use.
type SettingsCipher interface {
	Encrypt(plaintext []byte) ([]byte, error)
	Decrypt(ciphertext []byte) ([]byte, error)
}

// SettingsService is general-settings business logic (#178): hostname and
// timezone persistence and the write-only backup passphrase (Q28).
// internal/api's handlers call only this type, never SettingsStore
// directly.
type SettingsService struct {
	Store  *SettingsStore
	Cipher SettingsCipher
	Now    func() time.Time
	NewID  func() string
}

// NewSettingsService wires a SettingsService with the real clock and
// uuid.NewString.
func NewSettingsService(store *SettingsStore, cipher SettingsCipher) *SettingsService {
	return &SettingsService{
		Store:  store,
		Cipher: cipher,
		Now:    time.Now,
		NewID:  uuid.NewString,
	}
}

func (s *SettingsService) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *SettingsService) newID() string {
	if s.NewID != nil {
		return s.NewID()
	}
	return uuid.NewString()
}

// GeneralSettings is the API-facing view of schema_info's settings fields.
type GeneralSettings struct {
	Hostname            *string
	Timezone            *string
	BackupPassphraseSet bool
}

// Get returns the current general settings. A missing schema_info row is
// not an error — every field is simply unset.
func (s *SettingsService) Get(ctx context.Context) (GeneralSettings, error) {
	row, err := s.loadRow(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return GeneralSettings{}, nil
		}
		return GeneralSettings{}, fmt.Errorf("settings: loading general settings: %w", err)
	}
	return generalSettingsFromRow(row), nil
}

// UpdateInput carries the optional fields an updateGeneralSettings request
// may change. A nil pointer means "leave unchanged"; Hostname may point to
// an empty string to clear a previously set value.
type UpdateGeneralSettingsInput struct {
	Hostname         *string
	Timezone         *string
	BackupPassphrase *string
}

// Update merges input into the stored row and persists it.
func (s *SettingsService) Update(ctx context.Context, input UpdateGeneralSettingsInput) (GeneralSettings, error) {
	row, err := s.ensureRow(ctx)
	if err != nil {
		return GeneralSettings{}, err
	}

	if input.Hostname != nil {
		trimmed := strings.TrimSpace(*input.Hostname)
		if *input.Hostname != "" && trimmed == "" {
			return GeneralSettings{}, fmt.Errorf("%w: hostname must not be whitespace only", ErrInvalidSettingsInput)
		}
		if trimmed == "" {
			row.Hostname = sql.NullString{}
		} else {
			row.Hostname = sql.NullString{String: trimmed, Valid: true}
		}
	}

	if input.Timezone != nil {
		trimmed := strings.TrimSpace(*input.Timezone)
		if trimmed == "" {
			return GeneralSettings{}, fmt.Errorf("%w: timezone must not be empty", ErrInvalidSettingsInput)
		}
		if _, err := time.LoadLocation(trimmed); err != nil {
			return GeneralSettings{}, fmt.Errorf("%w: unknown timezone %q", ErrInvalidSettingsInput, trimmed)
		}
		row.Timezone = sql.NullString{String: trimmed, Valid: true}
	}

	if input.BackupPassphrase != nil {
		if s.Cipher == nil {
			return GeneralSettings{}, fmt.Errorf("settings: a SettingsCipher is required to store a backup passphrase")
		}
		ciphertext, err := s.Cipher.Encrypt([]byte(*input.BackupPassphrase))
		if err != nil {
			return GeneralSettings{}, fmt.Errorf("settings: encrypting backup passphrase: %w", err)
		}
		row.BackupPassphrase = ciphertext
	}

	if err := s.Store.Update(ctx, *row); err != nil {
		return GeneralSettings{}, fmt.Errorf("settings: saving general settings: %w", err)
	}
	return generalSettingsFromRow(row), nil
}

// UpdateSettings is schema_info's Q67 fields.
type UpdateSettings struct {
	Channel         string
	CheckEnabled    bool
	PreviousVersion string
}

func (s *SettingsService) GetUpdateSettings(ctx context.Context) (UpdateSettings, error) {
	row, err := s.loadRow(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return UpdateSettings{Channel: "stable", CheckEnabled: true}, nil
		}
		return UpdateSettings{}, err
	}
	channel := row.UpdateChannel
	if channel == "" {
		channel = "stable"
	}
	prev := ""
	if row.PreviousVersion.Valid {
		prev = row.PreviousVersion.String
	}
	return UpdateSettings{
		Channel:         channel,
		CheckEnabled:    row.UpdateCheckEnabled,
		PreviousVersion: prev,
	}, nil
}

func (s *SettingsService) SetUpdateSettings(ctx context.Context, channel string, checkEnabled bool) error {
	if _, err := s.ensureRow(ctx); err != nil {
		return err
	}
	return s.Store.UpdateUpdateSettings(ctx, channel, checkEnabled)
}

func (s *SettingsService) SetPreviousVersion(ctx context.Context, version string) error {
	if _, err := s.ensureRow(ctx); err != nil {
		return err
	}
	return s.Store.UpdatePreviousVersion(ctx, version)
}

// BackupPassphrase decrypts the stored backup passphrase for
// backup.SecretSource (Q28). It never logs the value.
func (s *SettingsService) BackupPassphrase(ctx context.Context) (string, bool, error) {
	row, err := s.loadRow(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	if len(row.BackupPassphrase) == 0 {
		return "", false, nil
	}
	if s.Cipher == nil {
		return "", false, fmt.Errorf("settings: a SettingsCipher is required to read a backup passphrase")
	}
	plain, err := s.Cipher.Decrypt(row.BackupPassphrase)
	if err != nil {
		return "", false, fmt.Errorf("settings: decrypting backup passphrase: %w", err)
	}
	return string(plain), true, nil
}

func (s *SettingsService) loadRow(ctx context.Context) (*GeneralSettingsRow, error) {
	return s.Store.Get(ctx)
}

func (s *SettingsService) ensureRow(ctx context.Context) (*GeneralSettingsRow, error) {
	row, err := s.loadRow(ctx)
	if err == nil {
		return row, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("settings: loading general settings: %w", err)
	}
	now := s.now().UTC().Format(timeFormat)
	if err := s.Store.InsertMeta(ctx, s.newID(), now); err != nil {
		return nil, fmt.Errorf("settings: creating schema_info row: %w", err)
	}
	return s.loadRow(ctx)
}

func generalSettingsFromRow(row *GeneralSettingsRow) GeneralSettings {
	out := GeneralSettings{
		BackupPassphraseSet: len(row.BackupPassphrase) > 0,
	}
	if row.Hostname.Valid {
		out.Hostname = &row.Hostname.String
	}
	if row.Timezone.Valid {
		out.Timezone = &row.Timezone.String
	}
	return out
}
