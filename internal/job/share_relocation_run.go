package job

import (
	"context"
	"fmt"

	"github.com/mdg-labs/hoserva/internal/cache"
)

// ShareRelocationDeps is what RunShareRelocation needs to relocate one
// share (doc 09 §2, #54, #239). Share resolves a share name into the
// cache.Share RelocateToArray/RelocateToCache expect — the daemon's own
// configured state (D4), the same way MoverDeps.Shares resolves the
// mover's own sweep, scoped here to a single share the request named.
// Sync is RelocateToCache's own required dependency (cache.Deps.Sync's
// doc comment); RelocateToArray never calls it.
type ShareRelocationDeps struct {
	Share  func(ctx context.Context, name string) (cache.Share, error)
	Config cache.Config
	Sync   cache.SyncFunc
}

// RunShareRelocation is the RunFunc hoservad registers for
// TypeShareRelocation: a thin adapter from *RunContext onto #54's
// cache.RelocateToArray/RelocateToCache, the same shape RunMover adapts
// cache.Run — decoding ShareRelocationParams to pick the share and
// direction a startShareRelocation request named, since (unlike the
// mover's own scheduled sweep) a relocation job always has one behind
// it. This is share relocation's only invocation path outside tests
// (doc 09 §2, D18): nothing else calls RelocateToArray or
// RelocateToCache.
func RunShareRelocation(d ShareRelocationDeps) RunFunc {
	return func(ctx context.Context, rc *RunContext) error {
		p, err := decodeShareRelocationParams(rc.Params())
		if err != nil {
			return err
		}
		share, err := d.Share(ctx, p.Share)
		if err != nil {
			return fmt.Errorf("job: share relocation: resolving share %q: %w", p.Share, err)
		}
		hooks := cache.RunHooks{
			StopRequested:  rc.StopRequested(),
			SaveCheckpoint: rc.SaveCheckpoint,
			SetProgress:    rc.SetProgress,
			Log: func(format string, args ...any) {
				_, _ = fmt.Fprintf(rc.Output(), format+"\n", args...)
			},
		}

		var report cache.Report
		if p.To == shareRelocationToArray {
			report, err = cache.RelocateToArray(ctx, share, d.Config, cache.Deps{}, hooks, rc.InitialCheckpoint())
		} else {
			report, err = cache.RelocateToCache(ctx, share, d.Config, cache.Deps{Sync: d.Sync}, hooks, rc.InitialCheckpoint())
		}
		if !report.StartedAt.IsZero() {
			_, _ = fmt.Fprintln(rc.Output(), report.Summary())
		}
		return err
	}
}
