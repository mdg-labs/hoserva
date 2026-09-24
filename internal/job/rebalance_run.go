package job

import (
	"context"
	"fmt"

	"github.com/mdg-labs/hoserva/internal/cache"
)

// RebalanceDeps is what RunRebalance needs to run a rebalance job (doc 09
// §3, #55, #274): exactly the cache.Deps.Sync/TrackedFileCount
// cache.RunRebalance itself requires (its own doc comment), wired to the
// daemon's real parity engine.
type RebalanceDeps struct {
	Config           cache.Config
	Sync             cache.SyncFunc
	TrackedFileCount func(ctx context.Context) (int, error)
}

// RunRebalance is the RunFunc hoservad registers for job.TypeRebalance: a
// thin adapter from *RunContext onto cache.RunRebalance, running exactly
// the plan startRebalance most recently computed and submitted with this
// job (RebalanceParams.Plan) — never recomputed here (cache.RebalancePlan's
// own doc comment: "shown to the user before RunRebalance ever executes
// it ... RunRebalance takes it as a value rather than recomputing it, so
// what runs is exactly what was shown"). This is rebalancing's only
// invocation path outside tests (doc 09 §3, D18): nothing else calls
// cache.RunRebalance.
func RunRebalance(d RebalanceDeps) RunFunc {
	return func(ctx context.Context, rc *RunContext) error {
		p, err := decodeRebalanceParams(rc.Params())
		if err != nil {
			return err
		}
		hooks := cache.RunHooks{
			StopRequested:  rc.StopRequested(),
			SaveCheckpoint: rc.SaveCheckpoint,
			SetProgress:    rc.SetProgress,
			Log: func(format string, args ...any) {
				_, _ = fmt.Fprintf(rc.Output(), format+"\n", args...)
			},
		}
		report, err := cache.RunRebalance(ctx, p.Plan, d.Config, cache.Deps{Sync: d.Sync, TrackedFileCount: d.TrackedFileCount}, hooks, rc.InitialCheckpoint())
		if !report.StartedAt.IsZero() {
			_, _ = fmt.Fprintln(rc.Output(), report.Summary())
		}
		return err
	}
}

// RebalanceConfirmation is the exact typed confirmation startRebalance
// requires — fixed, rather than derived from the plan's own content,
// because a rebalance plan is always recomputed fresh at confirm time
// (planRebalance and startRebalance never trust a client-supplied plan,
// doc 09 §3) and its own move count can shift between a preview and a
// confirm from nothing more than ordinary file activity elsewhere in the
// pool — tying the confirmation phrase to that content would make every
// confirm racy against it.
func RebalanceConfirmation() string { return "REBALANCE" }
