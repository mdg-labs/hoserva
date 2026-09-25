package job

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/mdg-labs/hoserva/internal/cache"
	"github.com/mdg-labs/hoserva/internal/store"
)

// RebalanceDeps is what RunRebalance needs to run a rebalance job (doc 09
// §3, #55, #274): exactly the cache.Deps.Sync/TrackedFileCount
// cache.RunRebalance itself requires (its own doc comment), wired to the
// daemon's real parity engine.
type RebalanceDeps struct {
	Config           cache.Config
	Sync             cache.SyncFunc
	TrackedFileCount func(ctx context.Context) (int, error)
	// Store is read before every run, a resume included, to refuse a plan
	// that touches a disk leaving the array (#366): the plan was computed
	// before that disk entered removal. Required.
	Store *store.ArrayStore
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
		if d.Store == nil {
			return fmt.Errorf("job: rebalance: Deps.Store is required")
		}
		if err := refuseLeavingDiskMoves(ctx, d.Store, p.Plan); err != nil {
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

// refuseLeavingDiskMoves fails a rebalance whose plan moves a file from
// or to a data disk leaving the array. A plan is only ever computed
// without leaving disks (api.Handler.sharesOffLeavingDisks), but a
// rebalance interrupted before an evacuation started resumes its old
// plan. A move's disk is filepath.Dir of its branch, the "<disk>/<share>"
// shape every cache.Share branch has.
func refuseLeavingDiskMoves(ctx context.Context, arrays *store.ArrayStore, plan cache.RebalancePlan) error {
	_, disks, err := arrays.GetArray(ctx)
	if err != nil {
		if errors.Is(err, store.ErrNoArray) {
			return nil
		}
		return fmt.Errorf("job: rebalance: reading the array's disks: %w", err)
	}
	leaving := make(map[string]string)
	for _, disk := range disks {
		if disk.Role == store.ArrayRoleData && disk.LeavingArray() {
			leaving[disk.Mountpoint] = disk.RemovalState
		}
	}
	for _, mv := range plan.Moves {
		for _, branch := range []string{mv.SourceBranch, mv.TargetBranch} {
			mp := filepath.Dir(branch)
			if state, ok := leaving[mp]; ok {
				return fmt.Errorf("job: rebalance: the plan moves %s via %s, which is being removed from the array (%s) — cancel this rebalance and plan a new one", mv.RelPath, mp, state)
			}
		}
	}
	return nil
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
