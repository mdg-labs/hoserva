package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/mdg-labs/hoserva/internal/api"
	cfggen "github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"
)

// newArraySequence constructs the daemon's single job.ArraySequence from
// persisted topology (doc 02 §4, Q70, Q69). No array yet returns (nil,
// nil) so stop/start stay 501 rather than unmounting an empty path.
// Forgetting Evaluate here would leave StorageGate unready and refuse
// every Start even with every disk present; skipping the gate would
// mount a degraded array.
//
// shares populates ShareMounts with every persisted share's own mount
// and, for every non-cache-only share, its mover write target (#268):
// without this, Stop never unmounts a share's own mergerfs mount before
// the catch-all, so the catch-all unmount fails EBUSY the moment any
// share exists, and Start never remounts a share at all — a share that
// survives to a reboot loses its own mount and its mover write target
// until something other than array start remounts it by hand.
//
// Services always carries Samba and NFS (doc 02 §4's "Samba and NFS
// stop"/"start" step, #309) — even when there are no data mounts yet —
// so ArraySequence.Stop always stops them before any unmount can run,
// and every caller (array stop, the UPS low-battery shutdown, the
// update reboot) gets the same ordering because they all run this one
// sequence. Running containers come first in that slice when the daemon
// has a Docker client: they stop before Samba and NFS, and a container
// that will not stop holds the array stop up before any unmount, the same
// way a Samba or NFS that will not stop does. containers is one instance
// shared by every rebuild, since it remembers across an array stop which
// containers array start still owes a start.
//
// While an Unraid import is pending (settings.MigrationPending) the data
// disks' units are read-only and bound to each disk's own device, the
// catch-all is read-only and the only pool mount (a share is a directory of
// it), and Start confirms each disk against that device as well as its UUID;
// parity and cache are not in the sequence at all, being only recorded.
//
// A data disk that has left the pool (unpooled, #358) is in no pool
// mount's branch list; an unlisted one is not part of the sequence at all.
//
// The readiness gate also reports not ready while a data-disk upgrade is
// pending (doc 02 §4 UR2), and Start confirms every mounted array disk
// against the filesystem UUID SQLite names before anything above the
// disks starts (UR9).
func newArraySequence(ctx context.Context, scheduler *job.Scheduler, arrays *store.ArrayStore, shares *store.ShareStore, disks disk.Provider, runner disk.Runner, containers job.ArrayService) (*job.ArraySequence, error) {
	settings, assigned, err := arrays.GetArray(ctx)
	if err != nil {
		if errors.Is(err, store.ErrNoArray) {
			return nil, nil
		}
		return nil, fmt.Errorf("loading array topology: %w", err)
	}

	expected := make([]disk.ExpectedDisk, 0, len(assigned))
	diskMounts := make([]job.ArrayMount, 0, len(assigned))
	var dataMounts []string
	var cachePath string
	var removingDisk string
	var checked []disk.MountUnit
	for _, d := range assigned {
		if d.Mountpoint == "" {
			return nil, fmt.Errorf("array disk %s has no mountpoint", d.Device)
		}
		// An unlisted disk is out of every pool mount and out of
		// snapraid.conf (#358, doc 09 §4 steps 7-8); only its own
		// unmount and row deletion are left. The array neither waits
		// for it nor mounts it, so pulling it early never blocks a start.
		if d.RemovalState == store.RemovalStateUnlisted {
			continue
		}
		checked = append(checked, disk.MountUnit{Where: d.Mountpoint, UUID: d.FSUUID, What: d.MountSource})
		// A cache on a boot-disk partition is matched by its parent
		// disk's identity, and the inventory's cached filesystem for that
		// disk is its first partition's, never this partition's — so the
		// recorded UUID is not compared here; ArrayDiskUUIDCheck above
		// still confirms the mounted partition's filesystem at Start.
		expectedFSUUID := d.FSUUID
		if disk.IsPartition(d.Device, d.ByIDName) {
			expectedFSUUID = ""
		}
		expected = append(expected, disk.ExpectedDisk{
			Identity: disk.Identity{
				WWN:          d.WWN,
				Serial:       d.Serial,
				WeakIdentity: d.WeakIdentity,
				ByIDName:     d.ByIDName,
				// FSUUID is the filesystem SQLite recorded for this slot
				// (#388): a replacement disk matched by identity alone but
				// carrying a different filesystem must not be reported
				// present, since Startup would then try to mount it by a
				// UUID that never appears as a device.
				FSUUID: expectedFSUUID,
			},
			Role:    d.Role,
			MountAt: d.Mountpoint,
		})
		diskMounts = append(diskMounts, guardedSlotMount{
			inner: disk.MountUnitController{
				Unit: disk.MountUnit{
					Where:      d.Mountpoint,
					UUID:       d.FSUUID,
					Filesystem: disk.FilesystemType(d.Filesystem),
					ReadOnly:   settings.MigrationPending,
					What:       d.MountSource,
				},
				Runner: runner,
			},
			runner: runner,
		})
		switch d.Role {
		case store.ArrayRoleData:
			if d.LeftPool() {
				continue
			}
			dataMounts = append(dataMounts, d.Mountpoint)
			if d.RemovalState == store.RemovalStateEvacuating || d.RemovalState == store.RemovalStateEvacuated {
				removingDisk = d.Mountpoint
			}
		case store.ArrayRoleCache:
			cachePath = d.Mountpoint
		}
	}

	gate := disk.NewStorageGate(expected)
	listed, err := disks.List(ctx)
	if err != nil {
		// Leave the gate unevaluated (Ready is false until Evaluate).
		// Returning the error would abort daemon startup and take the API,
		// diagnostics, and Stop with it; Start already refuses with
		// storage_not_ready while the gate is unready.
		log.Printf("hoservad: listing disks for storage gate: %v — Start will refuse until inventory can be evaluated", err)
	} else {
		present := make([]disk.Identity, 0, len(listed))
		for _, d := range listed {
			present = append(present, disk.Identity{
				WWN:          d.WWN,
				Serial:       d.Serial,
				WeakIdentity: d.WeakIdentity,
				ByIDName:     d.ByIDName,
				// FSUUID is read from udev's own cache (disk.Lister.List),
				// never by opening the device (#388) — the same source
				// disk.Disk.FSUUID everywhere else in this file already
				// trusts.
				FSUUID: d.FSUUID,
			})
		}
		gate.Evaluate(present)
	}

	// Stop order (doc 02 §4): containers stop before Samba and NFS, and
	// Start runs the slice in reverse. VMs, once they exist, go before
	// containers. A nil containers means no Docker client was built.
	var services []job.ArrayService
	if containers != nil {
		services = append(services, containers)
	}
	services = append(services,
		disk.ServiceUnitController{ServiceName: "Samba", Unit: cfggen.SambaServiceUnit, Runner: runner},
		disk.ServiceUnitController{ServiceName: "NFS", Unit: cfggen.NFSServiceUnit, Runner: runner},
	)

	seq := &job.ArraySequence{
		Scheduler: scheduler,
		Gate:      job.PendingUpgradeGate{Gate: gate, Scheduler: scheduler},
		DiskCheck: job.ArrayDiskUUIDCheck{Mounts: disk.KernelMounts{Runner: runner}, Disks: checked},
		Services:  services,
		Disks:     diskMounts,
	}
	if len(dataMounts) == 0 {
		return seq, nil
	}

	var catchAll pool.Mount
	if settings.MigrationPending {
		catchAll, err = pool.CatchAllMountReadOnly(dataMounts, pool.Options{MinFreeSpace: settings.MinFreeSpace})
	} else if removingDisk == "" {
		catchAll, err = pool.CatchAllMount(dataMounts, pool.Options{MinFreeSpace: settings.MinFreeSpace})
	} else {
		catchAll, err = pool.CatchAllMountRemoving(dataMounts, removingDisk, pool.Options{MinFreeSpace: settings.MinFreeSpace})
	}
	if err != nil {
		return nil, fmt.Errorf("building catch-all pool mount: %w", err)
	}
	if settings.CreatePolicy != "" {
		catchAll.CreatePolicy = pool.CreatePolicy(settings.CreatePolicy)
	}
	// Production mounts the catch-all and every share through their
	// generated systemd .mount units (SystemdMounter, #335) so mergerfs
	// lives outside hoserva.service's cgroup — a restart must not tear
	// the pool down. Lab tests keep using pool.Mounter (direct exec).
	seq.CatchAll = pool.MountController{
		Mnt:     catchAll,
		Mounter: guardedCatchAllMounter{inner: pool.SystemdMounter{Runner: runner}, runner: runner},
	}

	// A pending migration has the catch-all only: /mnt/user/<share> is a
	// directory of the read-only pool, and a share's directory may exist on no
	// adopted disk.
	if settings.MigrationPending {
		return seq, nil
	}
	rows, err := shares.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading shares: %w", err)
	}
	opts := pool.Options{MinFreeSpace: settings.MinFreeSpace}
	for _, row := range rows {
		sh := pool.Share{
			Name:         row.Name,
			CacheMode:    pool.CacheMode(row.CacheMode),
			CreatePolicy: pool.CreatePolicy(row.CreatePolicy),
			MinFreeSpace: row.MinFreeSpace,
		}
		var shareMount pool.Mount
		if removingDisk == "" {
			shareMount, err = pool.ShareMount(sh, dataMounts, cachePath, opts)
		} else {
			shareMount, err = pool.ShareMountRemoving(sh, dataMounts, cachePath, removingDisk, opts)
		}
		if err != nil {
			return nil, fmt.Errorf("building share mount for %q: %w", sh.Name, err)
		}
		seq.ShareMounts = append(seq.ShareMounts, pool.MountController{
			Mnt:     shareMount,
			Mounter: pool.SystemdMounter{Runner: runner},
		})
		if sh.CacheMode == pool.CacheOnly {
			continue
		}
		var moverMount pool.Mount
		if removingDisk == "" {
			moverMount, err = pool.MoverTargetMount(sh, dataMounts, opts)
		} else {
			moverMount, err = pool.MoverTargetMountRemoving(sh, dataMounts, removingDisk, opts)
		}
		if err != nil {
			return nil, fmt.Errorf("building mover target mount for %q: %w", sh.Name, err)
		}
		seq.ShareMounts = append(seq.ShareMounts, pool.MountController{
			Mnt:     moverMount,
			Mounter: pool.SystemdMounter{Runner: runner},
		})
	}
	return seq, nil
}

// storageGateOf unwraps newArraySequence's gate: the storage readiness
// gate inside job.PendingUpgradeGate (doc 02 §4 UR2). Acknowledging a
// degraded array (#385) needs the concrete *disk.StorageGate — Acknowledge
// is not part of the job.ReadinessGate interface newArraySequence's own
// Gate field carries — and this is the one place production reconstructs
// it, the same way this package's own tests already did before this
// wiring needed it too.
func storageGateOf(g job.ReadinessGate) (*disk.StorageGate, bool) {
	wrapped, ok := g.(job.PendingUpgradeGate)
	if !ok {
		return nil, false
	}
	inner, ok := wrapped.Gate.(*disk.StorageGate)
	return inner, ok
}

// acknowledgedDegraded remembers, for cmd/hoservad's whole lifetime, which
// disks the user has explicitly acknowledged — missing ones (#385 finding
// 1) and, identically, ones present by identity but carrying the wrong
// filesystem (#388 finding 2, disk.StorageGate.WrongFilesystem): both are
// exactly as degraded as the other, and the gate treats them the same way
// in Ready(). newArraySequence's own disk.StorageGate is rebuilt from
// scratch on every share create/update/delete (shareService.PostCommit),
// every disk-topology job's ArrayReady hook, and every SIGHUP —
// hoserva-storage.rules sends the latter on every udev block `add` event,
// including an unrelated USB stick — a freshly built gate has never
// itself been acknowledged, so without this, any one of those
// re-evaluates the same still-degraded slot as newly degraded and undoes
// the acknowledgement the moment it runs. reapply only re-acknowledges a
// freshly evaluated gate when every slot it currently reports missing or
// wrong-filesystem is one this holder already recorded
// (disk.Identity.Matches, which does not distinguish the two categories —
// the same identity flipping from missing to wrong-filesystem or back is
// still the same acknowledged slot) — a disk arriving, or a different
// slot going missing or wrong-filesystem, is a new degraded state and
// gets its own banner, never a blanket "stay quiet forever".
type acknowledgedDegraded struct {
	mu         sync.Mutex
	identities []disk.Identity
	// seqMu serializes wireAcknowledgeDegraded's hook against
	// newRebuildArraySequence's closure: otherwise a rebuild can reapply
	// onto its fresh gate before the hook records, then publish that
	// still-unacknowledged gate after the hook already opened the storage
	// target, closing it again under an acknowledgement reported as
	// successful. Always taken before storageTargetSync.mu.
	seqMu sync.Mutex
}

// record captures the identities behind an acknowledgement that has just
// succeeded — gate.Missing() and gate.WrongFilesystem() together (#388
// finding 2), read against the same gate.Acknowledge() call that just
// succeeded: a wrong-filesystem slot is exactly as degraded as a missing
// one, and reapply must be able to tell it was already acknowledged too.
func (a *acknowledgedDegraded) record(missing, wrongFilesystem []disk.ExpectedDisk) {
	ids := make([]disk.Identity, 0, len(missing)+len(wrongFilesystem))
	for _, m := range missing {
		ids = append(ids, m.Identity)
	}
	for _, m := range wrongFilesystem {
		ids = append(ids, m.Identity)
	}
	a.mu.Lock()
	a.identities = ids
	a.mu.Unlock()
}

// reapply re-acknowledges gate — freshly built and already Evaluate'd by
// newArraySequence — when every slot it now reports missing or wrong-
// filesystem (#388 finding 2) is already in this holder's own recorded
// set. It clears that set once gate reports nothing degraded at all, so
// disk.StorageGate.Evaluate's own "all present clears the
// acknowledgement" rule is never contradicted by a stale holder outliving
// the state it was about.
func (a *acknowledgedDegraded) reapply(gate *disk.StorageGate) {
	degraded := append(gate.Missing(), gate.WrongFilesystem()...)
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(degraded) == 0 {
		a.identities = nil
		return
	}
	for _, d := range degraded {
		found := false
		for _, id := range a.identities {
			if d.Identity.Matches(id) {
				found = true
				break
			}
		}
		if !found {
			return
		}
	}
	_ = gate.Acknowledge()
}

// wireAcknowledgeDegraded installs Handler.AcknowledgeDegraded (#385, doc
// 02 §1, Q69): cmd/hoservad's own main.go and this package's own tests
// both call this one function, so a test built the way main.go builds the
// handler fails the moment this wiring is skipped, instead of passing
// against dead production code the way a hand-copied closure could.
//
// It calls disk.StorageGate.Acknowledge on CurrentArray()'s own live gate
// — never a rebuild through newArraySequence, which would construct a
// fresh, unacknowledged gate and undo the call this hook just made —
// records the degraded identities behind it — missing and wrong-
// filesystem alike (ack.record, #388 finding 2) — so a later rebuild's
// own reapply can re-acknowledge the same degraded state (finding 1), and
// finally runs storageTarget's own not-ready→ready
// transition (UpdateOrError) against that same sequence. A transition
// that does not actually start anything — the array is in maintenance
// mode, or the pool fails to mount or confirm — is reported to the
// caller as api.ErrDegradedServicesNotStarted (finding 3) rather than
// silently reported as success: the acknowledgement itself still stands,
// but the caller must never tell the user services are running when they
// are not.
//
// It also wires Handler.StorageServicesReleased to storageTarget.Ready
// (#385 finding 2): GetStatus's own storageServicesReleased field must
// read this same live gate state, never be derived from the
// acknowledgement alone, since the two can disagree exactly in the
// maintenance-mode and mount-failure cases above.
func wireAcknowledgeDegraded(handler *api.Handler, storageTarget *storageTargetSync, ack *acknowledgedDegraded) {
	handler.StorageServicesReleased = storageTarget.Ready
	handler.AcknowledgeDegraded = func(ctx context.Context) error {
		ack.seqMu.Lock()
		defer ack.seqMu.Unlock()
		seq := handler.CurrentArray()
		if seq == nil {
			return disk.ErrNothingToAcknowledge
		}
		gate, ok := storageGateOf(seq.Gate)
		if !ok {
			return disk.ErrNothingToAcknowledge
		}
		if err := gate.Acknowledge(); err != nil {
			return err
		}
		ack.record(gate.Missing(), gate.WrongFilesystem())
		if err := storageTarget.UpdateOrError(ctx, seq); err != nil {
			return fmt.Errorf("%w: %v", api.ErrDegradedServicesNotStarted, err)
		}
		return nil
	}
}

// newRebuildArraySequence builds the rebuild closure run() installs as
// shareService.PostCommit, hands to wireTopologyHooks for the
// disk-topology jobs' ArrayReady hook, and re-runs on every SIGHUP
// (installReloadHandler) — the single place a disk arriving/leaving or a
// live share change re-evaluates disk.StorageGate (doc 02 §1, Q69). It
// re-applies any acknowledgement of a still-missing or still-wrong-
// filesystem slot the user already gave (ack.reapply, #385 finding 1,
// #388 finding 2) to the freshly built gate before syncing the storage-
// target units — without this, a fresh, unevaluated gate is never
// acknowledged, so any one of this closure's own callers (a share edit, a
// disk-topology change, a SIGHUP) would undo the user's acknowledgement
// the moment it ran.
func newRebuildArraySequence(scheduler *job.Scheduler, arrayStore *store.ArrayStore, shareStore *store.ShareStore, disks disk.Provider, runner disk.Runner, storageTarget *storageTargetSync, poolWriteGate job.PoolWriteGate, handler *api.Handler, ack *acknowledgedDegraded, containers job.ArrayService, afterStart func(context.Context)) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		ack.seqMu.Lock()
		defer ack.seqMu.Unlock()
		seq, err := newArraySequence(ctx, scheduler, arrayStore, shareStore, disks, runner, containers)
		if err != nil {
			return err
		}
		if seq != nil {
			seq.StorageTarget = storageTarget
			// The same instance main.go's own arraySeq and backupService
			// share (#409): newArraySequence always builds a fresh
			// job.ArraySequence with a zero PoolWriteGate, so every rebuild
			// (a share change, a disk-topology change, a SIGHUP) must
			// re-wire it here or Stop's own coordination with backupService
			// would silently stop after the very first one.
			seq.PoolWriteGate = poolWriteGate
			// Every rebuild is a fresh sequence too, so the hook that queues an
			// owed initial sync on `array start` is re-wired here as well.
			seq.AfterStart = afterStart
			if gate, ok := storageGateOf(seq.Gate); ok {
				ack.reapply(gate)
			}
		}
		handler.SetArray(seq)
		// A disk arriving, leaving or a live topology change is exactly
		// the "storage gate's inputs changed" doc 02 §1 and Q69 describe —
		// the boot-ordering units must reflect it now, not only at the
		// next reboot. Update only ever runs after notifySystemdReady has
		// already sent READY=1, so hoserva.service's own start job is long
		// finished; it also only touches systemd on an actual transition
		// (storageTargetSync's own doc comment), so a share create with an
		// unchanged gate never restarts anything already running.
		storageTarget.Update(ctx, seq)
		return nil
	}
}
