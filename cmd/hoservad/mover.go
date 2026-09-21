package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/mdg-labs/hoserva/internal/cache"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"
)

// moverSharesFromStore builds job.MoverDeps.Shares (#53, doc 09 §2): every
// currently configured cache-then-move share, resolved fresh from SQLite
// (D4) on each mover run rather than captured once at daemon startup, so a
// share or array topology change before the next nightly chain is picked
// up without a restart. No array yet (store.ErrNoArray) resolves to no
// shares, the same "nothing to do yet" the array sequence itself treats it
// as (newArraySequence, array.go) — the mover has nothing to relocate
// before create-array has ever run.
func moverSharesFromStore(shares *store.ShareStore, arrays *store.ArrayStore) func(ctx context.Context) ([]cache.Share, error) {
	return func(ctx context.Context) ([]cache.Share, error) {
		settings, disks, err := arrays.GetArray(ctx)
		if err != nil {
			if errors.Is(err, store.ErrNoArray) {
				return nil, nil
			}
			return nil, fmt.Errorf("mover: loading array topology: %w", err)
		}

		var cachePath string
		var dataDisks []string
		for _, d := range disks {
			switch d.Role {
			case store.ArrayRoleCache:
				cachePath = d.Mountpoint
			case store.ArrayRoleData:
				dataDisks = append(dataDisks, d.Mountpoint)
			}
		}

		minFree, err := parseMinFreeSpaceBytes(settings.MinFreeSpace)
		if err != nil {
			return nil, fmt.Errorf("mover: parsing array minfreespace %q: %w", settings.MinFreeSpace, err)
		}

		all, err := shares.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("mover: listing shares: %w", err)
		}

		var out []cache.Share
		for _, s := range all {
			if s.CacheMode != string(pool.CacheThenMove) {
				continue
			}
			if cachePath == "" {
				return nil, fmt.Errorf("mover: share %q is cache-then-move but the array has no cache disk", s.Name)
			}
			if len(dataDisks) == 0 {
				return nil, fmt.Errorf("mover: share %q is cache-then-move but the array has no data disks", s.Name)
			}
			out = append(out, cache.Share{
				Name:         s.Name,
				CachePath:    cachePath + "/" + s.Name,
				ArrayPath:    pool.MoverTargetPath(s.Name),
				Branches:     shareBranchDirs(dataDisks, s.Name),
				MinFreeSpace: minFree,
			})
		}
		return out, nil
	}
}

// shareBranchDirs mirrors the per-share directories the pool package's own
// MoverTargetMount builds its branch list from (internal/pool/topology.go's
// unexported shareBranches) — one directory per data disk, without the
// mergerfs "=RW" mode suffix that only the mount option string itself
// needs.
func shareBranchDirs(dataDisks []string, share string) []string {
	branches := make([]string, len(dataDisks))
	for i, d := range dataDisks {
		branches[i] = d + "/" + share
	}
	return branches
}

// parseMinFreeSpaceBytes parses mergerfs's own minfreespace size-suffix
// syntax (K/M/G/T/P, optionally followed by "B", powers of 1024, e.g.
// "50G") into a byte count. An empty value falls back to
// pool.DefaultOptions' own default — the same fallback pool.Options.render
// applies to the mount option itself, so an unset array setting and an
// explicitly-default one behave identically here too.
func parseMinFreeSpaceBytes(s string) (int64, error) {
	if strings.TrimSpace(s) == "" {
		s = pool.DefaultOptions().MinFreeSpace
	}
	upper := strings.TrimSuffix(strings.ToUpper(strings.TrimSpace(s)), "B")
	if upper == "" {
		return 0, fmt.Errorf("empty value")
	}
	multipliers := map[byte]int64{'K': 1 << 10, 'M': 1 << 20, 'G': 1 << 30, 'T': 1 << 40, 'P': 1 << 50}
	last := upper[len(upper)-1]
	if mult, ok := multipliers[last]; ok {
		n, err := strconv.ParseInt(upper[:len(upper)-1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%q: %w", s, err)
		}
		return n * mult, nil
	}
	n, err := strconv.ParseInt(upper, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q: %w", s, err)
	}
	return n, nil
}
