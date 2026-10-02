package main

import (
	"context"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/store"
)

// wireRegistryCredentials makes the registry credentials reachable
// (Q81): /registry-credentials (Handler.RegistryCredentials), and the
// registry client the update check asks registries through, which logs in
// with the credential saved for a registry's host. It returns that client
// for newUpdateChecker. A test calls it too, rather than repeating the
// assignments.
func wireRegistryCredentials(handler *api.Handler, creds *store.RegistryCredentialStore, cipher container.SecretCipher) *container.HTTPRegistry {
	service := &container.RegistryCredentials{Store: creds, Cipher: cipher}
	handler.RegistryCredentials = service
	return &container.HTTPRegistry{Credentials: service}
}

func registryCredentialSecrets(ctx context.Context, st *store.RegistryCredentialStore) ([]backup.DatabaseSecret, error) {
	if st == nil {
		return nil, nil
	}
	rows, err := st.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]backup.DatabaseSecret, 0, len(rows))
	for _, row := range rows {
		if len(row.Sealed) == 0 {
			continue
		}
		out = append(out, backup.DatabaseSecret{
			Table:      "registry_credentials",
			Column:     "credential",
			RowID:      row.Registry,
			Ciphertext: row.Sealed,
		})
	}
	return out, nil
}
