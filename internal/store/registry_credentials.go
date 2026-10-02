package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	storedb "github.com/mdg-labs/hoserva/internal/store/db"
)

// RegistryCredential is one row of registry_credentials (#488): the
// credential for a registry host, still sealed under the machine key. Sealed
// is empty for a row a restore without the backup passphrase cleared.
type RegistryCredential struct {
	Registry  string
	Sealed    []byte
	UpdatedAt time.Time
}

// RegistryCredentialStore persists registry credentials in the central
// SQLite database (D4). It never opens a credential: sealing belongs to
// container.RegistryCredentials.
type RegistryCredentialStore struct {
	q *storedb.Queries
}

// NewRegistryCredentialStore wraps db for registry credential persistence.
func NewRegistryCredentialStore(db storedb.DBTX) *RegistryCredentialStore {
	return &RegistryCredentialStore{q: storedb.New(db)}
}

// Put creates or replaces the credential for registry.
func (s *RegistryCredentialStore) Put(ctx context.Context, registry string, sealed []byte, at time.Time) error {
	err := s.q.UpsertRegistryCredential(ctx, storedb.UpsertRegistryCredentialParams{
		Registry:   registry,
		Credential: sealed,
		UpdatedAt:  at.UTC().Format(TimeFormat),
	})
	if err != nil {
		return fmt.Errorf("store: saving the credential of registry %s: %w", registry, err)
	}
	return nil
}

// Get returns the credential for registry, and false when there is none.
func (s *RegistryCredentialStore) Get(ctx context.Context, registry string) (RegistryCredential, bool, error) {
	row, err := s.q.GetRegistryCredential(ctx, registry)
	if errors.Is(err, sql.ErrNoRows) {
		return RegistryCredential{}, false, nil
	}
	if err != nil {
		return RegistryCredential{}, false, fmt.Errorf("store: reading the credential of registry %s: %w", registry, err)
	}
	c, err := registryCredentialFromRow(row)
	return c, err == nil, err
}

// List returns every credential, sorted by registry.
func (s *RegistryCredentialStore) List(ctx context.Context) ([]RegistryCredential, error) {
	rows, err := s.q.ListRegistryCredentials(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: listing registry credentials: %w", err)
	}
	out := make([]RegistryCredential, 0, len(rows))
	for _, row := range rows {
		c, err := registryCredentialFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// Delete removes the credential for registry and reports whether there was
// one.
func (s *RegistryCredentialStore) Delete(ctx context.Context, registry string) (bool, error) {
	n, err := s.q.DeleteRegistryCredential(ctx, registry)
	if err != nil {
		return false, fmt.Errorf("store: deleting the credential of registry %s: %w", registry, err)
	}
	return n > 0, nil
}

func registryCredentialFromRow(row *storedb.RegistryCredential) (RegistryCredential, error) {
	at, err := time.Parse(TimeFormat, row.UpdatedAt)
	if err != nil {
		return RegistryCredential{}, fmt.Errorf("store: parsing the credential of registry %s updated_at: %w", row.Registry, err)
	}
	return RegistryCredential{Registry: row.Registry, Sealed: row.Credential, UpdatedAt: at}, nil
}
