package main

import (
	"context"
	"fmt"

	"github.com/mdg-labs/hoserva/internal/cache"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"
)

// shareRelocationShareFromStore builds job.ShareRelocationDeps.Share (doc
// 09 §2, #239, #245): the one share a startShareRelocation request named,
// resolved fresh from SQLite (D4) the same way moverSharesFromStore
// resolves the mover's whole cache-then-move sweep — same cache disk,
// data disks and minfreespace lookup, same cache/array path shape — but
// for exactly one share, and without moverSharesFromStore's
// cache-then-move filter, since relocation moves any one share between
// cache and array regardless of its current mode (doc 09 §2: "when a
// share's cache mode changes"). A data disk leaving the array
// (store.ArrayDisk.LeavingArray, #366) is left out of Branches, except
// one still "evacuating": a relocation to cache reads the share's files
// from Branches, and an evacuating disk can still hold some of them. A
// disk only gets past "evacuating" once the evacuation's post-check has
// found nothing but empty directories in its share branches. The cost
// is that a relocation to the array counts an evacuating disk's free
// space in its room pre-check, although mergerfs creates nothing there.
func shareRelocationShareFromStore(shares *store.ShareStore, arrays *store.ArrayStore) func(ctx context.Context, name string) (cache.Share, error) {
	return func(ctx context.Context, name string) (cache.Share, error) {
		rec, err := shares.Get(ctx, name)
		if err != nil {
			return cache.Share{}, fmt.Errorf("share relocation: loading share %q: %w", name, err)
		}

		settings, disks, err := arrays.GetArray(ctx)
		if err != nil {
			return cache.Share{}, fmt.Errorf("share relocation: loading array topology: %w", err)
		}

		var cachePath string
		var dataDisks []string
		for _, d := range disks {
			switch d.Role {
			case store.ArrayRoleCache:
				cachePath = d.Mountpoint
			case store.ArrayRoleData:
				if !d.LeavingArray() || d.RemovalState == store.RemovalStateEvacuating {
					dataDisks = append(dataDisks, d.Mountpoint)
				}
			}
		}
		if cachePath == "" {
			return cache.Share{}, fmt.Errorf("share relocation: share %q needs a cache disk, array has none", rec.Name)
		}
		if len(dataDisks) == 0 {
			return cache.Share{}, fmt.Errorf("share relocation: share %q needs data disks, array has none", rec.Name)
		}

		minFree, err := parseMinFreeSpaceBytes(settings.MinFreeSpace)
		if err != nil {
			return cache.Share{}, fmt.Errorf("share relocation: parsing array minfreespace %q: %w", settings.MinFreeSpace, err)
		}

		return cache.Share{
			Name:         rec.Name,
			CachePath:    cachePath + "/" + rec.Name,
			ArrayPath:    pool.MoverTargetPath(rec.Name),
			Branches:     shareBranchDirs(dataDisks, rec.Name),
			MinFreeSpace: minFree,
		}, nil
	}
}

// shareRelocationSyncFunc adapts eng.Sync into job.ShareRelocationDeps.Sync
// (cache.SyncFunc's shape) — the real dependency RelocateToCache needs to
// sync parity before deleting a relocated file's array original (Q14).
// It runs the exact same eng.Sync(ctx, parity.SyncOpts{Manifest:
// manifest}) call, with Confirm left at its zero value, that
// internal/job's own test helper (syncFuncFromEngine,
// share_relocation_run_test.go) scripts against a fake engine to prove
// the threshold guard still blocks an unconfirmed sync — so the real
// adapter here goes through that same guard, never around it.
func shareRelocationSyncFunc(eng parity.Engine) cache.SyncFunc {
	return func(ctx context.Context, manifest []parity.ManifestEntry) error {
		ch, err := eng.Sync(ctx, parity.SyncOpts{Manifest: manifest})
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
