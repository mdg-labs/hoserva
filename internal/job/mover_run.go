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
//
// Results, when non-nil, persists the structured run report and the
// cache usage breakdown (#273). UsagePlan, when non-nil, supplies the
// cache mount and every share directory on it for that breakdown (Q87);
// a nil UsagePlan still persists the run result without a breakdown.
type MoverDeps struct {
	Shares    func(ctx context.Context) ([]cache.Share, error)
	Config    cache.Config
	Results   *cache.ResultStore
	UsagePlan func(ctx context.Context) (cacheMount string, shares []cache.UsageShare, err error)
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
			if d.Results != nil {
				var cacheMount string
				var usageShares []cache.UsageShare
				if d.UsagePlan != nil {
					mount, plan, perr := d.UsagePlan(ctx)
					if perr != nil {
						_, _ = fmt.Fprintf(rc.Output(), "mover: resolving usage plan: %v\n", perr)
					} else {
						cacheMount, usageShares = mount, plan
					}
				}
				if perr := d.Results.SaveFromReport(context.WithoutCancel(ctx), report, cacheMount, usageShares); perr != nil {
					_, _ = fmt.Fprintf(rc.Output(), "mover: persisting run result: %v\n", perr)
				}
			}
		}
		if err != nil {
			return err
		}
		return nil
	}
}
