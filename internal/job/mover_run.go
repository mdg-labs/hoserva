package job

import (
	"context"
	"fmt"

	"github.com/mdg-labs/hoserva/internal/cache"
)

// MoverDeps is what RunMover needs to actually run a pass (doc 09 §2,
// #53). Shares resolves the current cache-then-move shares to sweep, in
// order, from the daemon's own configured state (D4) — the mover has no
// job-params payload of its own, the same way DiffGuard and ConfigBackup
// read directly from configured state rather than from a submitted
// request body.
type MoverDeps struct {
	Shares func(ctx context.Context) ([]cache.Share, error)
	Config cache.Config
}

// RunMover is the RunFunc hoservad registers for TypeMover: a thin
// adapter from *RunContext onto cache.Run, passing StopRequested,
// SaveCheckpoint and SetProgress straight through as cache.RunHooks — the
// shape internal/cache's own doc.go describes this wiring as built for.
func RunMover(d MoverDeps) RunFunc {
	return func(ctx context.Context, rc *RunContext) error {
		shares, err := d.Shares(ctx)
		if err != nil {
			return fmt.Errorf("job: mover: resolving shares: %w", err)
		}
		hooks := cache.RunHooks{
			StopRequested:  rc.StopRequested(),
			SaveCheckpoint: rc.SaveCheckpoint,
			SetProgress:    rc.SetProgress,
			Log: func(format string, args ...any) {
				_, _ = fmt.Fprintf(rc.Output(), format+"\n", args...)
			},
		}
		report, err := cache.Run(ctx, shares, d.Config, cache.Deps{}, hooks, rc.InitialCheckpoint())
		if !report.StartedAt.IsZero() {
			_, _ = fmt.Fprintln(rc.Output(), report.Summary())
		}
		if err != nil {
			return err
		}
		return nil
	}
}
