package config

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mdg-labs/hoserva/internal/pool"
)

// PoolShare is one configured share, in the shape WritePoolMounts needs:
// pool.Share itself carries no JSON tags (it is pool's own domain type, not
// a serialization shape), so this package defines its own state-shaped
// copy — the same relationship SnapraidState above has to snapraid's own
// directives.
type PoolShare struct {
	Name         string            `json:"name"`
	CacheMode    pool.CacheMode    `json:"cache_mode"`
	CreatePolicy pool.CreatePolicy `json:"create_policy"`
}

// PoolState is the slice of pool state WritePoolMounts needs to build every
// mergerfs mount unit doc 02 §1's topology describes (D1: orchestrate
// mergerfs, never reimplement it). It stands in for the SQLite-backed state
// a future daemon reads (D4) — the same relationship SnapraidState has to
// snapraid.conf. A zero-value Options behaves like pool.DefaultOptions
// (pool.Options.render's own fallback), so a state.json that omits it still
// renders doc 02 §1's defaults.
type PoolState struct {
	DataDisks []string     `json:"data_disks"`
	CachePath string       `json:"cache_path"`
	Shares    []PoolShare  `json:"shares"`
	Options   pool.Options `json:"options"`
	// CreatePolicy, when set, is the catch-all pool's mergerfs create
	// policy from array setup. Empty keeps CatchAllMount's default (Q11).
	CreatePolicy pool.CreatePolicy `json:"create_policy,omitempty"`
	// RemovingDisk, when non-empty, is the one data disk doc 09 §4 step 2
	// marks no-create in every mount this state builds — the catch-all,
	// every share mount and every non-cache-only share's mover write
	// target — via the matching pool.*MountRemoving builder instead of
	// the plain one (#359). Empty keeps every path byte-identical to
	// before this field existed: the plain builders, same as always.
	RemovingDisk string `json:"removing_disk,omitempty"`
}

// unitFileName is pool.UnitFileName — kept as a local alias so this
// package's call sites stay readable next to mountUnitPath.
func unitFileName(where string) string {
	return pool.UnitFileName(where)
}

// poolMountUnitPrefixes are the escaped-name prefixes every unit
// WritePoolMounts can write falls under: the catch-all itself, any
// per-share mount nested under it, and any mover write-target mount
// nested under pool.ArrayRootPath. Reconciliation (below) only ever
// removes a manifest entry whose key falls under one of these, so it can
// never reach a unit some other generator (disk's own data-disk mounts,
// say) wrote into the same systemd/system/ directory.
func poolMountUnitPrefixes() (catchAll, sharePrefix, moverPrefix string) {
	catchAll = unitFileName(pool.CatchAllPath)
	sharePrefix = strings.TrimSuffix(catchAll, ".mount") + "-"
	moverPrefix = strings.TrimSuffix(unitFileName(pool.ArrayRootPath), ".mount") + "-"
	return catchAll, sharePrefix, moverPrefix
}

// isPoolMountUnitKey reports whether key (a Generator manifest key) names
// one of WritePoolMounts's own units.
func isPoolMountUnitKey(key string) bool {
	name := strings.TrimPrefix(key, poolMountUnitDir)
	if name == key {
		return false
	}
	catchAll, sharePrefix, moverPrefix := poolMountUnitPrefixes()
	return name == catchAll || strings.HasPrefix(name, sharePrefix) || strings.HasPrefix(name, moverPrefix)
}

// poolMountUnitDir is where Write puts a pool mount unit — relative to
// Generator.Root in dev/test, /etc/systemd/system/ in production,
// mirroring pool.ServiceDropIn.DropInPath's own convention.
const poolMountUnitDir = "systemd/system/"

func mountUnitPath(where string) string {
	return poolMountUnitDir + unitFileName(where)
}

func poolMounts(state PoolState) ([]pool.Mount, error) {
	catchAll, err := catchAllMount(state)
	if err != nil {
		return nil, err
	}
	mounts := []pool.Mount{catchAll}
	for _, s := range state.Shares {
		share := pool.Share{Name: s.Name, CacheMode: s.CacheMode, CreatePolicy: s.CreatePolicy}
		sm, err := shareMount(share, state)
		if err != nil {
			return nil, fmt.Errorf("config: building share mount for %q: %w", s.Name, err)
		}
		mounts = append(mounts, sm)
		if s.CacheMode == pool.CacheOnly {
			continue
		}
		mm, err := moverTargetMount(share, state)
		if err != nil {
			return nil, fmt.Errorf("config: building mover target mount for %q: %w", s.Name, err)
		}
		mounts = append(mounts, mm)
	}
	return mounts, nil
}

// shareMount and moverTargetMount pick pool's own *MountRemoving builder
// over its plain counterpart exactly when state.RemovingDisk is set
// (#359, doc 09 §4 step 2) — every PoolState-driven mount-generation path
// (WritePoolMounts, share.Service's own live mounts) goes through these
// so the same disk is marked no-create everywhere at once, never in only
// some of them.
func shareMount(share pool.Share, state PoolState) (pool.Mount, error) {
	if state.RemovingDisk == "" {
		return pool.ShareMount(share, state.DataDisks, state.CachePath, state.Options)
	}
	return pool.ShareMountRemoving(share, state.DataDisks, state.CachePath, state.RemovingDisk, state.Options)
}

func moverTargetMount(share pool.Share, state PoolState) (pool.Mount, error) {
	if state.RemovingDisk == "" {
		return pool.MoverTargetMount(share, state.DataDisks, state.Options)
	}
	return pool.MoverTargetMountRemoving(share, state.DataDisks, state.RemovingDisk, state.Options)
}

// CanWriteShareFiles preflights every path a share apply writes: pool
// mount units, smb.conf, and exports. One unmanaged or unimported file
// refuses the whole set so sibling files are not replaced first (Q76).
func (g *Generator) CanWriteShareFiles(ctx context.Context, state PoolState) error {
	mounts, err := poolMounts(state)
	if err != nil {
		return err
	}
	for _, m := range mounts {
		if err := g.CanWrite(ctx, mountUnitPath(m.Where)); err != nil {
			return err
		}
	}
	if err := g.CanWrite(ctx, PathSamba); err != nil {
		return err
	}
	return g.CanWrite(ctx, PathNFS)
}

func catchAllMount(state PoolState) (pool.Mount, error) {
	var catchAll pool.Mount
	var err error
	if state.RemovingDisk == "" {
		catchAll, err = pool.CatchAllMount(state.DataDisks, state.Options)
	} else {
		catchAll, err = pool.CatchAllMountRemoving(state.DataDisks, state.RemovingDisk, state.Options)
	}
	if err != nil {
		return pool.Mount{}, fmt.Errorf("config: building catch-all pool mount: %w", err)
	}
	if state.CreatePolicy != "" {
		catchAll.CreatePolicy = state.CreatePolicy
	}
	return catchAll, nil
}

// WriteCatchAllMount writes only the catch-all's mount unit and removes
// nothing. A disk-topology job knows the array's disks but not its
// shares, so it must not reconcile pool units the way WritePoolMounts
// does — that would delete every share's unit; share.Service rewrites
// those from the share store afterwards.
func (g *Generator) WriteCatchAllMount(ctx context.Context, state PoolState, command string, revision int, now time.Time) error {
	catchAll, err := catchAllMount(state)
	if err != nil {
		return err
	}
	return g.writeMount(ctx, catchAll, command, revision, now, map[string]bool{})
}

// WritePoolMounts builds every mergerfs mount unit doc 02 §1's topology
// describes from state — the catch-all, one per-share mount in the shape
// its own cache mode calls for, and each non-cache-only share's own mover
// write target (pool.CatchAllMount, pool.ShareMount, pool.MoverTargetMount)
// — and writes each one through Write. It never reformats or recomputes
// what internal/pool already computed (doc 09 §2: one placement algorithm,
// mergerfs's own): each file's body is exactly the Mount's own Render()
// output, with only the doc 01 §2 header Write adds around it. command
// names the `hoserva <command>` a user runs to regenerate this state
// instead of hand-editing the unit files, per the doc 01 §2 header.
func (g *Generator) WritePoolMounts(ctx context.Context, state PoolState, command string, revision int, now time.Time) error {
	mounts, err := poolMounts(state)
	if err != nil {
		return err
	}
	desired := make(map[string]bool)
	for _, m := range mounts {
		if err := g.writeMount(ctx, m, command, revision, now, desired); err != nil {
			return err
		}
	}
	return g.reconcilePoolMounts(ctx, desired)
}

func (g *Generator) writeMount(ctx context.Context, m pool.Mount, command string, revision int, now time.Time, desired map[string]bool) error {
	file := File{
		Path:    mountUnitPath(m.Where),
		Command: command,
		Body:    []byte(m.Render()),
	}
	if err := g.Write(ctx, file, revision, now); err != nil {
		return err
	}
	_, key, err := g.resolvePath(file.Path)
	if err != nil {
		return err
	}
	desired[key] = true
	return nil
}

// reconcilePoolMounts removes any pool mount unit Generator has a manifest
// record for that this call didn't just (re)write — a share removed from
// state, or one that moved to CacheOnly and so lost its mover write-target
// unit (doc 02 §1). RemoveManaged only ever deletes a unit that is still
// StatusManaged, so a hand-edited or already-unmanaged unit is left in
// place for a human to resolve, never silently swept away.
func (g *Generator) reconcilePoolMounts(ctx context.Context, desired map[string]bool) error {
	manifest, err := g.loadManifest()
	if err != nil {
		return err
	}
	for key := range manifest {
		if desired[key] || !isPoolMountUnitKey(key) {
			continue
		}
		if _, err := g.RemoveManaged(ctx, key); err != nil {
			return err
		}
	}
	return nil
}
