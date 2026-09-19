package job

import (
	"context"
	"fmt"

	"github.com/mdg-labs/hoserva/internal/parity"
)

// EngineDiffGuard is the production DiffGuard: it runs Engine.Diff and
// evaluates Guard against that report. SnapraidEngine.Sync still evaluates
// the same guard internally before writing parity; this adapter exists so
// MaintenanceChain can stop before Submit(TypeSync) when the night's
// diff is already blocked.
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
	return g.Guard.Evaluate(diff, nil, nil).Blocked, nil
}
