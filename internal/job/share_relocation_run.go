package job

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mdg-labs/hoserva/internal/cache"
	"github.com/mdg-labs/hoserva/internal/parity"
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
	// Manifest persists this run's relocation manifest (Q15, doc 09 §3-4)
	// so a concurrent job.TypeSync run can see it through RunSync's own
	// relocationManifestSource wiring (parity_run.go) instead of treating
	// the relocation's own removals as ordinary unaccounted ones. Optional:
	// nil means no store is wired, and RunShareRelocation simply never
	// calls it — the same degrade-conservatively shape RunSync's own read
	// side already uses for an Engine with no Relocation store. Only the
	// array→cache direction ever writes to it: RelocateToArray never
	// deletes from a data disk, so it has nothing for a concurrent sync's
	// guard to need accounting for. Narrowed to relocationManifestReplacer
	// (Replace only, the one method this file calls) rather than the
	// concrete *parity.RelocationManifestStore so a test can wrap the real
	// store with a call-counting fake to pin persistRelocationManifestOnCheckpoint's
	// write-amplification bound without touching internal/parity's own
	// algorithms; *parity.RelocationManifestStore satisfies it unchanged.
	Manifest relocationManifestReplacer
}

// relocationManifestReplacer is the subset of *parity.RelocationManifestStore
// RunShareRelocation needs.
type relocationManifestReplacer interface {
	Replace(ctx context.Context, manifest []parity.ManifestEntry, removingDisks map[string]bool) error
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
		toCache := p.To == shareRelocationToCache
		saveCheckpoint := rc.SaveCheckpoint
		if d.Manifest != nil && toCache {
			saveCheckpoint = persistRelocationManifestOnCheckpoint(ctx, rc.SaveCheckpoint, d.Manifest)
		}
		hooks := cache.RunHooks{
			StopRequested:  rc.StopRequested(),
			SaveCheckpoint: saveCheckpoint,
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
		if err == nil && !report.Interrupted && d.Manifest != nil && toCache {
			// The relocation's own trailing sync (RelocatePhaseFinalSync)
			// just succeeded, so parity already accounts for every delete
			// this run made — a persisted manifest that outlived this
			// point would wrongly exempt a later, unrelated removal at the
			// same disk+path from a future sync's own guard evaluation.
			// Uses a non-cancellable context: cache.RelocateToCache never
			// checks ctx again once that trailing sync call returns
			// (internal/cache/relocate.go), so a Cancel landing in exactly
			// that instant must not make this clear itself fail with
			// context.Canceled — that failure would be indistinguishable
			// from the RunFunc's own reaction to the cancel
			// (isCancellationDerived, scheduler.go) and silently dropped
			// under a bare, errorless Cancelled, stranding the manifest so
			// it could wrongly exempt a later, unrelated removal from the
			// guard (#381). The same fix #378 made for evacuation's own
			// clear. A clear that still fails for another, genuine reason
			// stays a plain wrapped error, exactly as before: #379 already
			// closed the fail-open at runJob's own classification layer
			// (isCancellationDerived) rather than in this RunFunc, so a
			// plain error here — never itself context.Canceled once this
			// runs on clearCtx — is recorded under any raced Cancel just as
			// it always was.
			clearCtx := context.WithoutCancel(ctx)
			if clearErr := d.Manifest.Replace(clearCtx, nil, nil); clearErr != nil {
				return fmt.Errorf("job: share relocation: clearing relocation manifest: %w", clearErr)
			}
		}
		return err
	}
}

// persistRelocationManifestOnCheckpoint wraps save so that, in addition to
// persisting the job's own resumable checkpoint exactly as before, it
// makes a RelocateToCache checkpoint's manifest durable in store the
// moment it first appears. cache.RelocateCheckpoint carries Manifest from
// the point the copy phase finishes, through syncing, deleting and
// finalSync (RelocateCheckpoint's own doc comment) unchanged — so once
// this run has persisted it once, every later checkpoint save in the same
// run (including relocateDeletePhase's own per-file checkpoint,
// internal/cache/relocate.go) carries the identical entries and does not
// need to re-persist them: store.Replace runs one DELETE-then-N-INSERT
// transaction, so calling it once per checkpoint would cost O(n) such
// transactions across an n-file relocation instead of the one this run
// actually needs. The one call that does happen fires right before
// RelocateToCache's own first guarded sync, which is what makes a
// concurrent job.TypeSync run see it (RunSync, parity_run.go) — including
// across a daemon restart, since by then the manifest is already durable
// in the same database a resumed run reads its own checkpoint back from.
func persistRelocationManifestOnCheckpoint(ctx context.Context, save func(data []byte) error, store relocationManifestReplacer) func(data []byte) error {
	persisted := false
	return func(data []byte) error {
		if err := save(data); err != nil {
			return err
		}
		if persisted {
			return nil
		}
		var cp cache.RelocateCheckpoint
		if err := json.Unmarshal(data, &cp); err != nil {
			return fmt.Errorf("job: share relocation: decoding relocate checkpoint: %w", err)
		}
		if len(cp.Manifest) == 0 {
			return nil
		}
		if err := store.Replace(ctx, cp.Manifest, nil); err != nil {
			return fmt.Errorf("job: share relocation: persisting relocation manifest: %w", err)
		}
		persisted = true
		return nil
	}
}
