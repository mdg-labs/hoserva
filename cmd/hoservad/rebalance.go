package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/mdg-labs/hoserva/internal/cache"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/store"
)

// rebalanceSharesFromStore builds Handler.RebalanceShares and
// job.RebalanceDeps/EvacuationDeps' own share list (doc 09 §3-4, #274):
// every configured share resolved fresh from SQLite (D4) on each call —
// moverSharesFromStore's own array-topology lookup, without its
// cache-then-move filter or its cache-disk requirement, since rebalancing
// and evacuation only ever move files between array branches, regardless
// of a share's cache mode, and need no cache disk to exist at all. No
// array yet (store.ErrNoArray) resolves to no shares, the same "nothing
// to do yet" moverSharesFromStore treats it as.
func rebalanceSharesFromStore(shares *store.ShareStore, arrays *store.ArrayStore) func(ctx context.Context) ([]cache.Share, error) {
	return func(ctx context.Context) ([]cache.Share, error) {
		settings, disks, err := arrays.GetArray(ctx)
		if err != nil {
			if errors.Is(err, store.ErrNoArray) {
				return nil, nil
			}
			return nil, fmt.Errorf("rebalance: loading array topology: %w", err)
		}

		var dataDisks []string
		for _, d := range disks {
			if d.Role == store.ArrayRoleData {
				dataDisks = append(dataDisks, d.Mountpoint)
			}
		}
		if len(dataDisks) == 0 {
			return nil, nil
		}

		minFree, err := parseMinFreeSpaceBytes(settings.MinFreeSpace)
		if err != nil {
			return nil, fmt.Errorf("rebalance: parsing array minfreespace %q: %w", settings.MinFreeSpace, err)
		}

		all, err := shares.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("rebalance: listing shares: %w", err)
		}

		out := make([]cache.Share, 0, len(all))
		for _, s := range all {
			out = append(out, cache.Share{
				Name:         s.Name,
				Branches:     shareBranchDirs(dataDisks, s.Name),
				MinFreeSpace: minFree,
			})
		}
		return out, nil
	}
}

// evacuationSyncFunc adapts eng.Sync into job.EvacuationDeps.Sync
// (job.EvacuationSyncFunc's own shape): the real dependency
// job.RunEvacuation needs so every batch sync it makes carries
// SyncOpts.RemovingDisks (doc 09 §4 step 2, Q15) — unlike
// shareRelocationSyncFunc/rebalanceSync above, which never empty a disk
// on purpose and so never need it. It runs the exact same
// eng.Sync(ctx, parity.SyncOpts{Manifest: manifest, RemovingDisks:
// removingDisks}) call, with Confirm left at its zero value, that this
// package's own evacuation lab test scripts against a real
// parity.SnapraidEngine — so the real adapter here goes through that same
// guard, never around it.
func evacuationSyncFunc(eng parity.Engine) job.EvacuationSyncFunc {
	return func(ctx context.Context, manifest []parity.ManifestEntry, removingDisks map[string]bool) error {
		ch, err := eng.Sync(ctx, parity.SyncOpts{Manifest: manifest, RemovingDisks: removingDisks})
		if err != nil {
			return err
		}
		for p := range ch {
			if p.Err != nil {
				return p.Err
			}
		}
		return nil
	}
}

// rebalanceTrackedFileCount adapts eng.Diff into job.RebalanceDeps/
// EvacuationDeps.TrackedFileCount (cache.Deps.TrackedFileCount's own
// required shape, internal/cache/mover.go): the sum of
// DiffReport.PerDisk[mount].FilesBefore across every disk in a fresh
// diff, exactly the "totalBefore" the threshold guard's own next
// Evaluate call will divide by (parity/guard.go's Evaluate) — never an
// independent filesystem walk, which cache.Deps.TrackedFileCount's own
// doc comment forbids.
func rebalanceTrackedFileCount(eng parity.Engine) func(ctx context.Context) (int, error) {
	return func(ctx context.Context) (int, error) {
		diff, err := eng.Diff(ctx)
		if err != nil {
			return 0, err
		}
		var total int
		for _, dd := range diff.PerDisk {
			total += dd.FilesBefore
		}
		return total, nil
	}
}
