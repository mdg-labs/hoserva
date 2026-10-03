package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/migrate"
	"github.com/mdg-labs/hoserva/internal/store"
)

// wireMigration is what main.go calls to make the migrator's scan reachable: it
// gives the handler the migration session (the migration_session row, and the
// uploaded Flash Backup zip it names under <state dir>/migrate) and registers
// the migration_scan job. mounter mounts the Unraid USB stick read-only for a
// scan of it (Q25); the array's disks, which are never offered as the stick,
// come from the handler's array store. A scan is read-only and short, so it is
// not cancellable. A test calls it too, rather than repeating the assignments. An
// error leaves the operations answering 501 instead of serving a session whose
// directory cannot be trusted.
func wireMigration(ctx context.Context, handler *api.Handler, registry *job.Registry, disks disk.Provider, mounter disk.ReadOnlyMounter, sessions *store.MigrationSessionStore, stateDir string) error {
	svc := &migrate.Service{
		Dir:      filepath.Join(stateDir, "migrate"),
		Scanner:  &migrate.Scanner{Disks: disks},
		Sessions: sessions,
		Mounter:  mounter,
		// The handler's array store is read when asked: it is set before this
		// is called, but a live array creation can change what it holds.
		ArrayDevices: handler.ArrayDevices,
		JobEnded: func(ctx context.Context, id string) (string, bool, error) {
			j, err := handler.Store.Get(ctx, id)
			if errors.Is(err, job.ErrNotFound) {
				return "missing", true, nil
			}
			if err != nil {
				return "", false, err
			}
			return string(j.Status), j.Status.Terminal(), nil
		},
	}
	// Any scan recorded as running belongs to the previous process: the
	// scheduler marks its job interrupted, and a scan is re-run, never resumed.
	if err := svc.Recover(ctx); err != nil {
		return fmt.Errorf("preparing the migration session directory: %w", err)
	}
	registry.Register(job.TypeMigrationScan, false, job.RunMigrationScan(svc.RunScan))
	handler.Migration = svc
	return nil
}
