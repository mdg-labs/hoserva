package config

import (
	"context"
	"fmt"
	"time"

	"github.com/mdg-labs/hoserva/internal/pool"
)

// storageTargetUnitDir mirrors poolMountUnitDir's own convention: a path
// relative to Generator.Root in dev/test, /etc/systemd/system/ in
// production.
const storageTargetUnitDir = poolMountUnitDir

// WriteStorageTarget writes hoserva-storage.target, hoserva-storage-
// ready.service and one managed drop-in per pool.DependentServiceUnits
// (doc 02 §1, Q69), through Write — the same manifest and hand-edit
// protection WritePoolMounts already gives the pool's own mount units.
// diskMountUnits is every physical data/parity/cache mount unit's file
// name the target's own Wants=/After= should carry. Every file this
// writes is independent of disk.StorageGate.Ready(): cmd/hoservad reflects
// that live in pool.StorageReadyFlagPath instead, so a caller re-running
// this after nothing but readiness changed writes byte-identical content.
// command names the `hoserva <command>` a user runs instead of
// hand-editing these units, per the doc 01 §2 header.
func (g *Generator) WriteStorageTarget(ctx context.Context, diskMountUnits []string, command string, revision int, now time.Time) error {
	readyUnit := pool.StorageReadyUnit{}
	if err := g.Write(ctx, File{
		Path:    storageTargetUnitDir + pool.StorageReadyUnitName,
		Command: command,
		Body:    []byte(readyUnit.Render()),
	}, revision, now); err != nil {
		return fmt.Errorf("config: writing %s: %w", pool.StorageReadyUnitName, err)
	}

	targetUnit := pool.StorageTargetUnit{DiskMountUnits: diskMountUnits}
	if err := g.Write(ctx, File{
		Path:    storageTargetUnitDir + pool.StorageTargetUnitName,
		Command: command,
		Body:    []byte(targetUnit.Render()),
	}, revision, now); err != nil {
		return fmt.Errorf("config: writing %s: %w", pool.StorageTargetUnitName, err)
	}

	for _, svc := range pool.DependentServiceUnits {
		dropIn := pool.ServiceDropIn{Unit: svc}
		if err := g.Write(ctx, File{
			Path:    dropIn.DropInPath(),
			Command: command,
			Body:    []byte(dropIn.Render()),
		}, revision, now); err != nil {
			return fmt.Errorf("config: writing %s drop-in: %w", svc, err)
		}
	}

	return nil
}
