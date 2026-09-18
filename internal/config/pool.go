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
}

// unitFileName mirrors disk.UnitFileName's own systemd-escape rule
// (CLAUDE.md: kept consistent in style, not shared code, since these are
// different packages rendering different kinds of units) — the same rule
// internal/pool's own golden test names its fixtures with.
func unitFileName(where string) string {
	return strings.ReplaceAll(strings.Trim(where, "/"), "/", "-") + ".mount"
}

// mountUnitPath is where Write puts a pool mount unit — a path relative to
// Generator.Root in dev/test, /etc/systemd/system/ in production, mirroring
// pool.ServiceDropIn.DropInPath's own convention.
func mountUnitPath(where string) string {
	return "systemd/system/" + unitFileName(where)
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
	catchAll, err := pool.CatchAllMount(state.DataDisks, state.Options)
	if err != nil {
		return fmt.Errorf("config: building catch-all pool mount: %w", err)
	}
	if err := g.writeMount(ctx, catchAll, command, revision, now); err != nil {
		return err
	}

	for _, s := range state.Shares {
		share := pool.Share{Name: s.Name, CacheMode: s.CacheMode, CreatePolicy: s.CreatePolicy}

		shareMount, err := pool.ShareMount(share, state.DataDisks, state.CachePath, state.Options)
		if err != nil {
			return fmt.Errorf("config: building share mount for %q: %w", s.Name, err)
		}
		if err := g.writeMount(ctx, shareMount, command, revision, now); err != nil {
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
		if err := g.writeMount(ctx, moverMount, command, revision, now); err != nil {
			return err
		}
	}

	return nil
}

func (g *Generator) writeMount(ctx context.Context, m pool.Mount, command string, revision int, now time.Time) error {
	file := File{
		Path:    mountUnitPath(m.Where),
		Command: command,
		Body:    []byte(m.Render()),
	}
	return g.Write(ctx, file, revision, now)
}
