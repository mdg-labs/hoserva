package main

import (
	"context"
	"errors"
	"fmt"
	"io"
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
// the migration_scan job. mounter mounts the Unraid USB stick (Q25) and the
// Unraid data disks read-only; runner runs the data disks' read-only filesystem
// checks; the array's disks, which are never offered as the stick, come from the
// handler's array store. The scan reads every data disk, which takes as long as
// the disks are large, so the job reports its progress and can be cancelled:
// a cancelled scan unmounts what it mounted. A test calls it too, rather than
// repeating the assignments. An error leaves the operations answering 501
// instead of serving a session whose directory cannot be trusted.
func wireMigration(ctx context.Context, handler *api.Handler, registry *job.Registry, disks disk.Provider, mounter disk.ReadOnlyMounter, runner disk.Runner, sessions *store.MigrationSessionStore, stateDir string) error {
	dir := filepath.Join(stateDir, "migrate")
	svc := &migrate.Service{
		Dir: dir,
		Scanner: &migrate.Scanner{
			Disks:   disks,
			Runner:  runner,
			Mounter: mounter,
			Dir:     dir,
			Dirs:    &migrate.DiskReader{Mounter: mounter, Runner: runner, Dir: dir},
		},
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
	registry.Register(job.TypeMigrationScan, true, func(ctx context.Context, rc *job.RunContext) error {
		scan := job.RunMigrationScan(func(ctx context.Context, out io.Writer, upload string) error {
			return svc.RunScan(migrate.WithProgress(ctx, rc.SetProgress), out, upload)
		})
		return scan(ctx, rc)
	})
	handler.Migration = svc
	return nil
}
