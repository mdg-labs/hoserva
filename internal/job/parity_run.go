package job

import (
	"context"
	"fmt"

	"github.com/mdg-labs/hoserva/internal/parity"
)

// shareUsageComputer is implemented by *parity.SnapraidEngine, optionally
// wired with a parity.UsageStore: RunSync calls it once, right after a
// successful non-dry-run sync, to compute and persist per-share,
// per-disk bytes used from the tracked state that sync just wrote
// (doc 02 §1 line 78, #223) — never live, never on a timer of its own. An
// Engine that does not implement it (every FakeEngine, and a
// SnapraidEngine with no Usage store wired yet) simply has nothing to
// compute here.
type shareUsageComputer interface {
	ComputeShareUsage(ctx context.Context) error
}

// relocationManifestSource is implemented by *parity.SnapraidEngine when
// wired with a parity.RelocationManifestStore: RunSync calls it once,
// right after decoding the request's own params and before calling
// eng.Sync, to load the current relocation manifest and removing-disks
// set (Q15, doc 09 §3-4) into SyncOpts.Manifest/RemovingDisks at the
// production wiring boundary — a relocation job (the mover, rebalance,
// evacuation or share relocation) persists this state as it runs, and a
// production sync must account for it the same way a preview does
// (RunParityDiff, internal/api/parity_handler.go). An Engine that does
// not implement it (every FakeEngine, and a SnapraidEngine with no
// Relocation store wired yet) simply has nothing to load here, the same
// shape shareUsageComputer above already uses.
type relocationManifestSource interface {
	CurrentRelocationManifest(ctx context.Context) ([]parity.ManifestEntry, map[string]bool, error)
}

// RunSync is the RunFunc hoservad registers for TypeSync: it reads
// persisted params from RunContext and calls eng.Sync. Tests register it
// against FakeEngine.
func RunSync(eng parity.Engine) RunFunc {
	return func(ctx context.Context, rc *RunContext) error {
		opts, err := SyncOptsFromParams(rc.Params())
		if err != nil {
			return err
		}
		if ms, ok := eng.(relocationManifestSource); ok {
			manifest, removingDisks, err := ms.CurrentRelocationManifest(ctx)
			if err != nil {
				return fmt.Errorf("job: loading relocation manifest: %w", err)
			}
			opts.Manifest = manifest
			opts.RemovingDisks = removingDisks
		}
		ch, err := eng.Sync(ctx, opts)
		if err := drainProgress(ch, err); err != nil {
			return err
		}
		if opts.DryRun {
			// A dry run never writes a new content file (doc 01 §3): the
			// tracked state `snapraid list` would read is unchanged, so
			// there is nothing fresh to compute.
			return nil
		}
		if uc, ok := eng.(shareUsageComputer); ok {
			return uc.ComputeShareUsage(ctx)
		}
		return nil
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
