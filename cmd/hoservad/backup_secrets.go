package main

import (
	"context"

	"github.com/mdg-labs/hoserva/internal/acme"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/notify"
)

// backupSecretSource is what a config archive's secrets.age is built from
// (Q28): the backup passphrase, and every sealed credential the database
// holds — ACME, UPS, backup destinations and notification channels.
func backupSecretSource(settings *api.SettingsService, acmeStore *acme.Store, upsStore *api.UPSStore, destinations *api.BackupDestinationStore, notifyStore *notify.Store) *backup.ServiceSecretSource {
	return &backup.ServiceSecretSource{
		BackupPassphraseFn: settings.BackupPassphrase,
		DatabaseSecretsFn: func(ctx context.Context) ([]backup.DatabaseSecret, error) {
			acmeSecrets, err := acmeDatabaseSecrets(ctx, acmeStore)
			if err != nil {
				return nil, err
			}
			upsSecrets, err := upsDatabaseSecrets(ctx, upsStore)
			if err != nil {
				return nil, err
			}
			destinationSecrets, err := destinations.BackupDestinationSecrets(ctx)
			if err != nil {
				return nil, err
			}
			notifySecrets, err := notifyDatabaseSecrets(ctx, notifyStore)
			if err != nil {
				return nil, err
			}
			out := append(acmeSecrets, upsSecrets...)
			out = append(out, destinationSecrets...)
			return append(out, notifySecrets...), nil
		},
	}
}

func notifyDatabaseSecrets(ctx context.Context, st *notify.Store) ([]backup.DatabaseSecret, error) {
	if st == nil {
		return nil, nil
	}
	rows, err := st.ListChannelSecrets(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]backup.DatabaseSecret, 0, len(rows))
	for _, row := range rows {
		out = append(out, backup.DatabaseSecret{
			Table:      "notify_channels",
			Column:     "secret",
			RowID:      row.ChannelID,
			Ciphertext: row.Ciphertext,
		})
	}
	return out, nil
}
