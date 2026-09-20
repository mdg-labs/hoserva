package acme

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mdg-labs/hoserva/internal/store"
	storedb "github.com/mdg-labs/hoserva/internal/store/db"
)

// Config is the persisted Let's Encrypt DNS-01 setup (D4). Secrets are
// already encrypted when they reach Store.
type Config struct {
	Domain       string
	Provider     string
	ProviderJSON string
	DNSSecret    []byte
	AccountKey   []byte
	Enabled      bool
	LastError    string
	UpdatedAt    time.Time
}

// ProviderSettings is the non-secret JSON in provider_config.
type ProviderSettings struct {
	Nameserver    string `json:"nameserver,omitempty"`
	TSIGKeyName   string `json:"tsigKeyName,omitempty"`
	TSIGAlgorithm string `json:"tsigAlgorithm,omitempty"`
}

// Store persists acme_config.
type Store struct {
	q *storedb.Queries
}

func NewStore(db storedb.DBTX) *Store {
	return &Store{q: storedb.New(db)}
}

func (s *Store) Get(ctx context.Context) (*Config, error) {
	row, err := s.q.GetACMEConfig(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	cfg := &Config{
		Domain:       row.Domain,
		Provider:     row.Provider,
		ProviderJSON: row.ProviderConfig,
		DNSSecret:    row.DnsSecret,
		AccountKey:   row.AccountKey,
		Enabled:      row.Enabled == 1,
		UpdatedAt:    parseTime(row.UpdatedAt),
	}
	if row.LastError.Valid {
		cfg.LastError = row.LastError.String
	}
	return cfg, nil
}

func (s *Store) Upsert(ctx context.Context, cfg *Config) error {
	return s.q.UpsertACMEConfig(ctx, storedb.UpsertACMEConfigParams{
		Domain:         cfg.Domain,
		Provider:       cfg.Provider,
		ProviderConfig: cfg.ProviderJSON,
		DnsSecret:      cfg.DNSSecret,
		AccountKey:     cfg.AccountKey,
		Enabled:        boolToSQL(cfg.Enabled),
		LastError:      stringToNull(cfg.LastError),
		UpdatedAt:      time.Now().UTC().Format(store.TimeFormat),
	})
}

func (s *Store) SetEnabled(ctx context.Context, enabled bool, lastError string) error {
	n, err := s.q.SetACMEEnabled(ctx, storedb.SetACMEEnabledParams{
		Enabled:   boolToSQL(enabled),
		LastError: stringToNull(lastError),
		UpdatedAt: time.Now().UTC().Format(store.TimeFormat),
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) SetLastError(ctx context.Context, lastError string) error {
	return s.q.SetACMELastError(ctx, storedb.SetACMELastErrorParams{
		LastError: stringToNull(lastError),
		UpdatedAt: time.Now().UTC().Format(store.TimeFormat),
	})
}

func (s *Store) SetAccountKey(ctx context.Context, ciphertext []byte) error {
	return s.q.SetACMEAccountKey(ctx, storedb.SetACMEAccountKeyParams{
		AccountKey: ciphertext,
		UpdatedAt:  time.Now().UTC().Format(store.TimeFormat),
	})
}

func (s *Store) Delete(ctx context.Context) error {
	return s.q.DeleteACMEConfig(ctx)
}

func (s *Store) HasEncryptedSecrets(ctx context.Context) (bool, error) {
	return s.q.HasEncryptedACMESecrets(ctx)
}

func (s *Store) ListSecrets(ctx context.Context) ([]struct {
	Column     string
	Ciphertext []byte
}, error) {
	rows, err := s.q.ListACMESecrets(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]struct {
		Column     string
		Ciphertext []byte
	}, 0, len(rows))
	for _, row := range rows {
		out = append(out, struct {
			Column     string
			Ciphertext []byte
		}{Column: row.Col, Ciphertext: row.Ciphertext})
	}
	return out, nil
}

func boolToSQL(v bool) int64 {
	if v {
		return 1
	}
	return 0
}

func stringToNull(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func parseTime(v string) time.Time {
	t, err := time.Parse(store.TimeFormat, v)
	if err != nil {
		return time.Time{}
	}
	return t
}

func encodeProvider(p ProviderSettings) (string, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("acme: encoding provider config: %w", err)
	}
	return string(raw), nil
}

func decodeProvider(raw string) (ProviderSettings, error) {
	if raw == "" || raw == "{}" {
		return ProviderSettings{}, nil
	}
	var p ProviderSettings
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return ProviderSettings{}, fmt.Errorf("acme: decoding provider config: %w", err)
	}
	return p, nil
}
