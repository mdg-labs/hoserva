package job

import (
	"context"
	"fmt"

	"github.com/mdg-labs/hoserva/internal/parity"
)

// EngineDiffGuard is the production DiffGuard: it runs Engine.Diff, applies
// the same manifest-aware, TargetConfirmed-gated accounting
// SnapraidEngine.Sync and the diff-preview endpoint (RunParityDiff,
// internal/api/parity_handler.go, #252) use — parity.ConfirmManifestTargets
// against Engine's currently persisted relocation manifest — and then
// evaluates Guard against the confirmed manifest. Without that accounting,
// this pre-check could decide "blocked" on a diff Sync itself would go on
// to run cleanly, exactly a Q14 two-phase relocation's trailing sync
// (#253): MaintenanceChain would then skip Submit(TypeSync) for a sync
// that was never actually unsafe. SnapraidEngine.Sync still evaluates the
// same guard internally before writing parity; this adapter exists so
// MaintenanceChain can stop before Submit(TypeSync) when the night's diff
// is genuinely still blocked, and the two can never disagree on the same
// manifest/diff state.
type EngineDiffGuard struct {
	Engine parity.Engine
	Guard  parity.Guard
}

func (g EngineDiffGuard) Evaluate(ctx context.Context) (bool, error) {
	if g.Engine == nil {
		return false, fmt.Errorf("job: maintenance chain: no parity engine configured")
	}
	diff, err := g.Engine.Diff(ctx)
	if err != nil {
		return false, err
	}

	var manifest []parity.ManifestEntry
	var removingDisks map[string]bool
	if ms, ok := g.Engine.(relocationManifestSource); ok {
		manifest, removingDisks, err = ms.CurrentRelocationManifest(ctx)
		if err != nil {
			return false, fmt.Errorf("job: maintenance chain: loading relocation manifest: %w", err)
		}
	}

	confirmed, err := parity.ConfirmManifestTargets(ctx, g.Engine, diff, manifest)
	if err != nil {
		return false, err
	}
	return g.Guard.Evaluate(diff, confirmed, removingDisks).Blocked, nil
}
