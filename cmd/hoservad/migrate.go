package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"

	"github.com/google/uuid"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/auth"
	cfggen "github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/migrate"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/share"
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

// wireMigrationImport is what main.go calls, after wireMigration, to make the
// import's adoption and the verify phase reachable (doc 05 §4 steps 14-16): it
// registers the migration_import job and the migration_verify job.
// migration_import adopts the Unraid data disks read-only through mounter (a
// unit mounter bound to the array's own mount units, never a read-write mount
// of a source disk). migration_verify only reads: it takes the adopted disks'
// paths from the array store and checks through runner (findmnt) that each
// mount and the pool are read-only before it walks them. The function also
// tells the migration session how to ask whether an adoption is pending, so it
// reports `imported` and refuses a forget. main.go gives the scheduler the same
// question before any job can be submitted, so every parity, array-write and
// topology job is refused beside a pending adoption. arrayReady rebuilds the array sequence from the stored
// topology (main.go passes rebuildArraySequence, not the disk-topology jobs'
// wider hook, which would also rewrite the pool's units from the share
// service's own state); the pool is mounted through the sequence it leaves in
// handler. shares is the one share service the HTTP handler reads: once the
// disks are adopted the import job seeds the scan's shares and accounts through
// it (seedMigration), so they show in listShares and listUsers. It fails when
// wireMigration has not run (there is no session for the job to read its report
// from) or the handler's array store is not arrays, which it checks before
// queueing.
func wireMigrationImport(handler *api.Handler, registry *job.Registry, arrays *store.ArrayStore, generator *cfggen.Generator, runner disk.Runner, mounter disk.UnitMounter, arrayReady func(ctx context.Context) error, shares *share.Service) error {
	if handler.Migration == nil {
		return errors.New("the migration session is not wired")
	}
	if handler.ArrayStore != arrays {
		return errors.New("the handler's array store is not the one the import records the array in")
	}
	handler.Migration.Pending = arrays.MigrationPending
	handler.Migration.Adopted = func(ctx context.Context) (migrate.Adoption, error) {
		_, disks, err := arrays.GetArray(ctx)
		if err != nil {
			return migrate.Adoption{}, err
		}
		sort.SliceStable(disks, func(i, j int) bool { return disks[i].RoleIndex < disks[j].RoleIndex })
		ad := migrate.Adoption{Pool: pool.CatchAllPath}
		for _, d := range disks {
			if d.Role == store.ArrayRoleData {
				ad.Disks = append(ad.Disks, migrate.AdoptedDisk{Serial: d.Serial, WWN: d.WWN, Mountpoint: d.Mountpoint})
			}
		}
		return ad, nil
	}
	handler.Migration.ConfirmReadOnly = func(ctx context.Context, where string) error {
		return disk.ConfirmMountedReadOnly(ctx, runner, where)
	}
	svc := handler.Migration
	registry.Register(job.TypeMigrationVerify, true, func(ctx context.Context, rc *job.RunContext) error {
		verify := job.RunMigrationVerify(func(ctx context.Context, out io.Writer) error {
			return svc.RunVerify(migrate.WithProgress(ctx, rc.SetProgress), out)
		})
		return verify(ctx, rc)
	})
	registry.Register(job.TypeMigrationImport, false, job.RunMigrationImport(job.MigrationImportDeps{
		Plan: func(ctx context.Context, assignments []disk.AdoptionAssignment) (disk.AdoptionPlan, error) {
			p, err := handler.Migration.PlanImport(ctx, assignments)
			if err != nil {
				return disk.AdoptionPlan{}, err
			}
			return p.Plan, nil
		},
		Runner:     runner,
		Store:      arrays,
		Generator:  generator,
		Mounter:    mounter,
		ArrayReady: arrayReady,
		Array:      handler.CurrentArray,
		Seed: func(ctx context.Context, out io.Writer) error {
			return seedMigration(ctx, out, svc.SeedPlan, shares)
		},
	}))
	return nil
}

// seedMigration creates the shares and accounts of the session's report through
// the share service and tells the job's log what it did: every account with the
// reminder to set its password, which is never read from the flash, and every
// share it did not create with the reason. Account passwords are random values
// nobody knows until the user sets one.
func seedMigration(ctx context.Context, out io.Writer, plan func(context.Context) (migrate.SeedPlan, error), shares *share.Service) error {
	p, err := plan(ctx)
	if err != nil {
		return err
	}
	in := share.SeedInput{Shares: p.Shares}
	for _, name := range p.Users {
		hash, err := auth.HashPasswordContext(ctx, uuid.NewString())
		if err != nil {
			return fmt.Errorf("hashing the placeholder password of %s: %w", name, err)
		}
		in.Users = append(in.Users, share.SeedUser{Username: name, PasswordHash: hash})
	}
	res, err := shares.SeedMigration(ctx, in)
	if err != nil {
		return err
	}
	byName := map[string]share.SeedShare{}
	for _, sh := range p.Shares {
		byName[sh.Name] = sh
	}
	for _, name := range res.Shares {
		sh := byName[name]
		_, _ = fmt.Fprintf(out, "share %s created: create policy %s, read-only over SMB until the point of no return\n", name, sh.CreatePolicy)
		if sh.TargetCacheMode != "" {
			_, _ = fmt.Fprintf(out, "share %s is array-only for now; its cache mode %s is applied once the cache exists\n", name, sh.TargetCacheMode)
		}
		for _, note := range sh.Notes {
			_, _ = fmt.Fprintf(out, "share %s: %s\n", name, note)
		}
	}
	for _, name := range res.ExistingShares {
		_, _ = fmt.Fprintf(out, "share %s already exists and is left as it is\n", name)
	}
	for _, sk := range p.SkippedShares {
		_, _ = fmt.Fprintf(out, "share %q was not created: %s\n", sk.Name, sk.Reason)
	}
	for _, name := range res.Users {
		_, _ = fmt.Fprintf(out, "account %s created without a password: set one in the web UI (Users) before its SMB clients reconnect; passwords are never read from Unraid\n", name)
	}
	for _, name := range res.ExistingUsers {
		_, _ = fmt.Fprintf(out, "account %s already exists and is left as it is\n", name)
	}
	for _, sk := range p.SkippedUsers {
		_, _ = fmt.Fprintf(out, "account %q was not created: %s\n", sk.Name, sk.Reason)
	}
	return nil
}
