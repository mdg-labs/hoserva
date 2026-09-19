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
}

// unitFileName turns where into the systemd unit name a mount at that
// path must use: "/" between path segments becomes "-", and any literal
// "-" already in a segment (ValidateShareName allows one in a share name)
// is escaped as \x2d so it can't collide with a separator — the same
// systemd-escape --path rule mkunitfiles-generator-style tooling follows.
// Every byte here is one ValidateShareName already restricted to
// [A-Za-z0-9_-] plus the "/" this function itself consumes, so no other
// escaping is needed.
func unitFileName(where string) string {
	trimmed := strings.Trim(where, "/")
	var b strings.Builder
	b.Grow(len(trimmed) + 6)
	for _, r := range trimmed {
		switch r {
		case '/':
			b.WriteByte('-')
		case '-':
			b.WriteString(`\x2d`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteString(".mount")
	return b.String()
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
	desired := make(map[string]bool)

	catchAll, err := pool.CatchAllMount(state.DataDisks, state.Options)
	if err != nil {
		return fmt.Errorf("config: building catch-all pool mount: %w", err)
	}
	if state.CreatePolicy != "" {
		catchAll.CreatePolicy = state.CreatePolicy
	}
	if err := g.writeMount(ctx, catchAll, command, revision, now, desired); err != nil {
		return err
	}

	for _, s := range state.Shares {
		share := pool.Share{Name: s.Name, CacheMode: s.CacheMode, CreatePolicy: s.CreatePolicy}

		shareMount, err := pool.ShareMount(share, state.DataDisks, state.CachePath, state.Options)
		if err != nil {
			return fmt.Errorf("config: building share mount for %q: %w", s.Name, err)
		}
		if err := g.writeMount(ctx, shareMount, command, revision, now, desired); err != nil {
			return err
		}

		if s.CacheMode == pool.CacheOnly {
			// CacheOnly data lives on cache permanently and is never
			// moved (pool/share.go) — no mover write-target mount
			// exists for it.
			continue
		}

		moverMount, err := pool.MoverTargetMount(share, state.DataDisks, state.Options)
		if err != nil {
			return fmt.Errorf("config: building mover target mount for %q: %w", s.Name, err)
		}
		if err := g.writeMount(ctx, moverMount, command, revision, now, desired); err != nil {
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
