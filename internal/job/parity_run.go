package job

import (
	"context"

	"github.com/mdg-labs/hoserva/internal/parity"
)

// RunSync is the RunFunc a later wiring issue registers for TypeSync: it
// reads persisted params from RunContext and calls eng.Sync. Tests
// register it against FakeEngine; production Registry.Register is out of
// this issue's scope.
func RunSync(eng parity.Engine) RunFunc {
	return func(ctx context.Context, rc *RunContext) error {
		opts, err := SyncOptsFromParams(rc.Params())
		if err != nil {
			return err
		}
		ch, err := eng.Sync(ctx, opts)
		return drainProgress(ch, err)
	}
}

// RunScrub is RunSync for TypeScrub.
func RunScrub(eng parity.Engine) RunFunc {
	return func(ctx context.Context, rc *RunContext) error {
		pct, err := ScrubPercentFromParams(rc.Params())
		if err != nil {
			return err
		}
		ch, err := eng.Scrub(ctx, pct, parity.DefaultScrubOlderThanDays)
		return drainProgress(ch, err)
	}
}

// RunFix is RunSync for TypeFix.
func RunFix(eng parity.Engine) RunFunc {
	return func(ctx context.Context, rc *RunContext) error {
		opts, err := FixOptsFromParams(rc.Params())
		if err != nil {
			return err
		}
		ch, err := eng.Fix(ctx, opts)
		return drainProgress(ch, err)
	}
}

func drainProgress(ch <-chan parity.Progress, err error) error {
	if err != nil {
		return err
	}
	for p := range ch {
		if p.Err != nil {
			return p.Err
		}
	}
	return nil
}
