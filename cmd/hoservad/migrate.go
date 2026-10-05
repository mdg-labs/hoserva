package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"sort"

	"github.com/google/uuid"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/auth"
	cfggen "github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/migrate"
	"github.com/mdg-labs/hoserva/internal/notify"
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
	handler.Migration.Finishing = arrays.MigrationFinishing
	handler.Migration.Record = func(ctx context.Context) ([]store.ArrayDisk, []store.RecordedDisk, error) {
		_, disks, err := arrays.GetArray(ctx)
		if err != nil {
			return nil, nil, err
		}
		recorded, err := arrays.RecordedDisks(ctx)
		if err != nil {
			return nil, nil, err
		}
		return disks, recorded, nil
	}
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
		// A pass recorded before an import runs again says nothing about what is
		// mounted after it: the import forgets it before it changes anything.
		InvalidateVerify: svc.InvalidateVerify,
	}))
	return nil
}

// wireMigrationParity is what main.go calls, after wireMigrationImport, to make
// the point of no return reachable (doc 05 §4 step 17): it registers the
// migration_parity job, which formats the former parity disks and the cache
// through disks (the real provider, whose format guard re-resolves each target
// by identity) and nothing else, mounts the data disks read-write and generates
// snapraid.conf. arrayReady is the disk-topology jobs' strict hook
// (parityReg.callArrayReady in main.go): it regenerates the pool's mounts and
// smb.conf from the share rows, rebuilds the array sequence and wires the parity
// engine for the snapraid.conf just written, so sync, scrub and fix are
// registered without a restart (#265), which the initial sync the job queues
// through scheduler needs. shares applies what the import deferred. It fails
// when wireMigration has not run, as wireMigrationImport does.
func wireMigrationParity(handler *api.Handler, registry *job.Registry, scheduler *job.Scheduler, disks disk.Provider, arrays *store.ArrayStore, generator *cfggen.Generator, runner disk.Runner, mounter disk.UnitMounter, arrayReady func(ctx context.Context) error, shares *share.Service) error {
	if handler.Migration == nil {
		return errors.New("the migration session is not wired")
	}
	if handler.ArrayStore != arrays {
		return errors.New("the handler's array store is not the one the point of no return records the array in")
	}
	svc := handler.Migration
	registry.Register(job.TypeMigrationParity, false, job.RunMigrationParity(job.MigrationParityDeps{
		Plan:       svc.PlanParityInit,
		Provider:   disks,
		Runner:     runner,
		Store:      arrays,
		Generator:  generator,
		Mounter:    mounter,
		ArrayReady: arrayReady,
		Array:      handler.CurrentArray,
		Shares: func(ctx context.Context, out io.Writer) error {
			return completeMigrationShares(ctx, out, shares)
		},
		QueueSync: job.QueueInitialSync(scheduler),
	}))
	return nil
}

// wireMigrationContainers is what main.go calls, after wireMigrationParity, to
// make Phase D's container steps reachable (doc 05 §4 steps 19 and 20): the
// migrator creates the user's stacks, starts them one at a time and checks that
// each sees its data through the Compose stack layer wireStacks gave the
// handler, and only once the migration is past its point of no return. The stack
// start itself is the stack_start job wireStacks registered. It fails when
// wireMigration or wireStacks has not run, so a missing piece is an error
// instead of operations that answer 501.
func wireMigrationContainers(handler *api.Handler, arrays *store.ArrayStore) error {
	if handler.Migration == nil {
		return errors.New("the migration session is not wired")
	}
	if handler.Stacks == nil {
		return errors.New("the Compose stack layer is not wired")
	}
	if handler.ArrayStore != arrays {
		return errors.New("the handler's array store is not the one the migration records the array in")
	}
	handler.Migration.Stacks = handler.Stacks
	handler.Migration.Initialized = migrationInitialized(arrays)
	return nil
}

// migrationInitialized is whether the parity initialisation has been confirmed:
// an array exists and no migration is pending or part-way through its point of
// no return (store.ArrayStore.MigrationUnfinished), the same question the
// scheduler asks before it admits a parity, array-write or topology job. A
// record that cannot be read is an error, not an answer.
func migrationInitialized(arrays *store.ArrayStore) func(context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		if _, _, err := arrays.GetArray(ctx); err != nil {
			if errors.Is(err, store.ErrNoArray) {
				return false, nil
			}
			return false, fmt.Errorf("reading the array: %w", err)
		}
		unfinished, err := arrays.MigrationUnfinished(ctx)
		if err != nil {
			return false, err
		}
		return !unfinished, nil
	}
}

// eventPublisher is the notification service's one publisher this file uses.
type eventPublisher interface {
	Publish(ctx context.Context, event notify.EventType, title, message string) error
}

// queueOwedInitialSync is what main.go calls once the parity engine is wired,
// and what every successful array start calls again: a migration that finished
// its point of no return but stopped before the initial sync was queued owes
// that sync (store.ArrayStore.InitialSyncOwed), and the array has no parity
// until it has run (doc 05 §5). The sync goes through the scheduler like any
// other and runs through the engine's threshold guard. A sync that cannot be
// queued (a persisted `array stop` at daemon start, the sync job not registered)
// is logged and published as an alert, and stays owed for the array start or
// daemon start after; it never stops the daemon starting.
func queueOwedInitialSync(ctx context.Context, arrays *store.ArrayStore, scheduler *job.Scheduler, notifier eventPublisher) {
	id, err := job.QueueOwedInitialSync(ctx, arrays, scheduler)
	if err != nil {
		log.Printf("hoservad: the initial sync the Unraid migration owes was not queued: %v", err)
		if perr := notifier.Publish(ctx, notify.EventSyncFailed, "The first parity sync could not be started",
			fmt.Sprintf("The migration finished, but the first sync that builds parity could not be queued (%v). The array has no parity until a sync has run: it is queued again when the array is started or Hoserva restarts, or start one yourself.", err)); perr != nil {
			log.Printf("hoservad: publishing the alert for the owed initial sync: %v", perr)
		}
		return
	}
	if id != "" {
		log.Printf("hoservad: queued the initial sync the Unraid migration owed (job %s)", id)
	}
}

// owedInitialSyncAfterStart is the ArraySequence.AfterStart hook of every
// sequence the daemon builds: an array started after the daemon came up with a
// persisted `array stop` queues the initial sync that start could not.
func owedInitialSyncAfterStart(arrays *store.ArrayStore, scheduler *job.Scheduler, notifier eventPublisher) func(context.Context) {
	return func(ctx context.Context) {
		queueOwedInitialSync(ctx, arrays, scheduler, notifier)
	}
}

// completeMigrationShares applies what the import deferred through the share
// service and tells the job's log what it did.
func completeMigrationShares(ctx context.Context, out io.Writer, shares *share.Service) error {
	res, err := shares.CompleteMigration(ctx)
	for _, line := range res.Applied {
		_, _ = fmt.Fprintf(out, "share %s\n", line)
	}
	for _, line := range res.Kept {
		_, _ = fmt.Fprintf(out, "share %s: it stays array-only with its Unraid cache mode recorded; change it when the array has a cache\n", line)
	}
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "%d share directories brought to the shared group and setgid mode (top level only)\n", res.Prepared)
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
