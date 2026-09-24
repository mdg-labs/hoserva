package job

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mdg-labs/hoserva/internal/cache"
	"github.com/mdg-labs/hoserva/internal/parity"
)

// EvacuationSyncFunc is cache.SyncFunc's shape plus the disk currently
// being evacuated (doc 09 §4 step 2's zero-files exemption, Q15):
// RunEvacuation always calls it with removingDisks set to exactly
// {mountpoint: true} for the disk EvacuationParams named, so
// parity.Guard.Evaluate's own removingDisks parameter exempts it — an
// evacuation's own final, post-delete batch sync is the one that
// actually empties the disk, and without this every such sync trips the
// guard's zero-files rule after the batch's sources are already deleted
// (Q14's window would then stay open until a human confirms an ordinary
// sync by hand). Distinct from cache.SyncFunc because that type is
// shared with share relocation and the mover, neither of which ever
// empties a disk on purpose.
type EvacuationSyncFunc func(ctx context.Context, manifest []parity.ManifestEntry, removingDisks map[string]bool) error

// EvacuationDeps is what RunEvacuation needs to run an evacuation job
// (doc 09 §4, #56, #274): RebalanceDeps' own TrackedFileCount, an
// EvacuationSyncFunc rather than a plain cache.SyncFunc so every batch
// sync can carry the disk being evacuated, plus Shares to load the
// array's current share layout for cache.EvacuationPostCheck once the
// move itself finishes without being interrupted.
type EvacuationDeps struct {
	Config           cache.Config
	Sync             EvacuationSyncFunc
	TrackedFileCount func(ctx context.Context) (int, error)
	Shares           func(ctx context.Context) ([]cache.Share, error)
	// Manifest persists this run's relocation manifest and removing-disks
	// set (Q15, doc 09 §3-4) so a concurrent job.TypeSync run can see it
	// through RunSync's own relocationManifestSource wiring
	// (parity_run.go) instead of treating the disk being evacuated as an
	// ordinary, unexplained drop to zero files. Unlike
	// ShareRelocationDeps.Manifest, which never carries a removing-disks
	// set (share relocation never empties a disk on purpose, #247), this
	// one does — RunEvacuation clears both the manifest and the exemption
	// together on every ending it cannot resume from (see the doc comment
	// below on why). Optional: nil means no store is wired, and
	// RunEvacuation simply never calls it.
	Manifest relocationManifestStore
}

// relocationManifestStore is the subset of *parity.RelocationManifestStore
// RunEvacuation and EvacuationAbort need: relocationManifestReplacer's own
// Replace, plus Current to read back whatever the last checkpoint
// persisted — needed only by EvacuationAbort, to confirm the persisted
// removing-disks set still names this job's own mountpoint before clearing
// it (its own doc comment below).
type relocationManifestStore interface {
	relocationManifestReplacer
	Current(ctx context.Context) ([]parity.ManifestEntry, map[string]bool, error)
}

// RunEvacuation is the RunFunc hoservad registers for job.TypeEvacuation:
// runs the exact plan evacuateDisk most recently computed and submitted
// with this job (EvacuationParams.Plan) through cache.RunRebalance — doc
// 09 §4 steps 3-6, the same copy/verify/guarded-sync/delete/guarded-sync
// order RunRebalance already implements (Q14) — then, once that run
// finishes without being interrupted, cache.EvacuationPostCheck confirms
// the evacuated disk's own share branches hold nothing but empty
// directories (doc 09 §4 step 6) before this job reports success. This is
// evacuation's only invocation path outside tests (D18): nothing else
// calls cache.RunRebalance or cache.EvacuationPostCheck for a disk
// removal.
//
// A caller must never treat this job's success as clearing the disk for
// physical removal on its own: doc 09 §4 steps 7-9 (mergerfs branch-list
// removal, SnapRAID removal, unmount) are a separate operation this job
// does not perform.
func RunEvacuation(d EvacuationDeps) RunFunc {
	return func(ctx context.Context, rc *RunContext) error {
		p, err := decodeEvacuationParams(rc.Params())
		if err != nil {
			return err
		}
		if d.Shares == nil {
			return fmt.Errorf("job: evacuation: Deps.Shares is required for the post-check")
		}
		if d.Sync == nil {
			return fmt.Errorf("job: evacuation: Deps.Sync is required")
		}
		// removingDisks names exactly the disk this job was submitted to
		// evacuate — never recomputed or widened — so every batch sync
		// this run makes exempts that one disk's own zero-files trip and
		// no other (doc 09 §4 step 2, Q15).
		removingDisks := map[string]bool{p.Mountpoint: true}
		sync := func(ctx context.Context, manifest []parity.ManifestEntry) error {
			return d.Sync(ctx, manifest, removingDisks)
		}
		saveCheckpoint := rc.SaveCheckpoint
		if d.Manifest != nil {
			saveCheckpoint = persistEvacuationManifestOnCheckpoint(ctx, rc.SaveCheckpoint, d.Manifest, removingDisks)
		}
		hooks := cache.RunHooks{
			StopRequested:  rc.StopRequested(),
			SaveCheckpoint: saveCheckpoint,
			SetProgress:    rc.SetProgress,
			Log: func(format string, args ...any) {
				_, _ = fmt.Fprintf(rc.Output(), format+"\n", args...)
			},
		}
		report, err := cache.RunRebalance(ctx, p.Plan, d.Config, cache.Deps{Sync: sync, TrackedFileCount: d.TrackedFileCount}, hooks, rc.InitialCheckpoint())
		if !report.StartedAt.IsZero() {
			_, _ = fmt.Fprintln(rc.Output(), report.Summary())
		}
		// resumable is Q29's own test for this run: only a graceful
		// stop — maintenance mode or an on-battery pause closing
		// rc.StopRequested(), never a hard ctx cancel — leaves a
		// checkpoint Resume can continue from (ErrJobNotInterrupted:
		// "only an interrupted job can be resumed"). Scheduler.Cancel of a
		// *running* job always calls rj.cancel() regardless of
		// resumability, so ctx.Err() != nil is exactly the signal that this
		// stop is not one of those two and the job will end failed or
		// cancelled, neither of which Resume ever revisits.
		resumable := err == nil && report.Interrupted && ctx.Err() == nil
		if d.Manifest != nil && !resumable {
			// Every non-resumable ending — a clean finish, a failure, or an
			// explicit Cancel — clears the whole persisted relocation state,
			// both the manifest and the removing-disks exemption. It is not
			// enough to clear only the exemption and leave the manifest, as
			// an earlier round of this fix did: matchManifest
			// (internal/parity/guard.go) accounts a manifest entry once its
			// SourceDisk/RelPath pair appears as *any* removal in a later
			// diff — it has no way to tell this run's own delete from an
			// unrelated later removal of the same path — so a stale entry
			// left behind by a run that never reached that delete could
			// wrongly exempt a later, unrelated deletion of that same file
			// from the guard's removed-count and percent thresholds.
			// Clearing everything only makes the guard stricter: a delete
			// that did happen but whose post-delete sync never ran is simply
			// counted as an ordinary removal at the next sync, and past a
			// threshold that sync blocks until a human confirms — the safe
			// direction (Q14). A run that fails and is later retried starts
			// a fresh evacuation from a new plan, which persists its own
			// manifest from scratch. Uses a non-cancellable context: a
			// Cancel landing between the run's last action and this clear
			// must not leave the exemption stranded — the job is not
			// interrupted at this point (a clean finish never was; a
			// failure or Cancel no longer is), so EvacuationAbort is never
			// reached to clear it later. The resumable case (a graceful
			// maintenance/battery stop) is untouched here: Resume continues
			// the same run, so its manifest and exemption must survive.
			clearCtx := context.WithoutCancel(ctx)
			if clearErr := d.Manifest.Replace(clearCtx, nil, nil); clearErr != nil {
				return combineEvacuationErr(err, fmt.Errorf("job: evacuation: clearing relocation manifest: %w", clearErr))
			}
		}
		if err != nil {
			return err
		}
		if report.Interrupted {
			return nil
		}
		shares, err := d.Shares(ctx)
		if err != nil {
			return fmt.Errorf("job: evacuation: loading shares for post-check: %w", err)
		}
		if err := cache.EvacuationPostCheck(p.Mountpoint, shares); err != nil {
			return fmt.Errorf("job: evacuation: %w", err)
		}
		return nil
	}
}

// combineEvacuationErr reports the run's own failure, if any, as the
// primary error — the one that determines the job's error message and
// code — while still surfacing a subsequent failure to clean up the
// removing-disks exemption rather than silently dropping it. A cleanup
// failure never displaces the run's own error, only annotates it.
func combineEvacuationErr(runErr, cleanupErr error) error {
	if runErr == nil {
		return cleanupErr
	}
	return fmt.Errorf("%w (also failed clearing the relocation manifest: %v)", runErr, cleanupErr)
}

// EvacuationAbort is the AbortFunc hoservad registers for
// job.TypeEvacuation (doc 09 §4, #274): RunEvacuation itself clears the
// whole persisted relocation state — manifest and removing-disks exemption
// together — on every non-resumable ending its own run can reach (its own
// doc comment above), but Scheduler.Cancel of a job that is already
// StatusInterrupted never re-enters RunEvacuation at all (abortAndCancel
// goes straight from interrupted to cancelled), so without this the state
// from that job's last checkpoint would survive indefinitely, permanently
// exempting its disk from the guard's zero-files rule and letting a stale
// manifest entry exempt a later, unrelated removal of the same path
// (matchManifest, internal/parity/guard.go). Clears only when the
// persisted removing-disks set still names exactly this job's own
// mountpoint and nothing else — never blind — because the store holds one
// shared slot and, by the time Cancel runs, a different array-write job
// (doc 01 §4's mutual exclusion only ever excludes a *running* job of the
// same class, never an interrupted one) could already have claimed it for
// an unrelated relocation.
func EvacuationAbort(store relocationManifestStore) AbortFunc {
	return func(ctx context.Context, params []byte) error {
		p, err := decodeEvacuationParams(params)
		if err != nil {
			return err
		}
		_, removingDisks, err := store.Current(ctx)
		if err != nil {
			return fmt.Errorf("job: evacuation: reading relocation manifest: %w", err)
		}
		if len(removingDisks) != 1 || !removingDisks[p.Mountpoint] {
			return nil
		}
		if err := store.Replace(ctx, nil, nil); err != nil {
			return fmt.Errorf("job: evacuation: clearing relocation manifest: %w", err)
		}
		return nil
	}
}

// EvacuationConfirmation is the exact typed confirmation
// planDiskEvacuation/evacuateDisk requires: tied to the disk's own
// mountpoint, which does not change between a preview and a confirm,
// rather than to its evacuation plan's own move list
// (RebalanceConfirmation's own doc comment explains why not).
func EvacuationConfirmation(mountpoint string) string {
	return "REMOVE " + mountpoint
}

// persistEvacuationManifestOnCheckpoint wraps save so that, in addition
// to persisting the job's own resumable checkpoint exactly as before, it
// keeps the persisted relocation manifest and removing-disks set (Q15,
// doc 09 §3-4) in step with whichever batch's manifest
// cache.RebalanceCheckpoint currently carries. Unlike a share
// relocation's own checkpoint — whose manifest is fixed for the run's
// whole life, so share_relocation_run.go's own
// persistRelocationManifestOnCheckpoint only ever persists once — an
// evacuation's checkpoint manifest changes at every batch boundary
// (cache.RunRebalance's own doc comment), so a store.Replace is needed
// once per batch, not once for the whole run. It is still only once per
// batch, not once per checkpoint: cache.RunRebalance checkpoints on every
// deleted file within a batch's own delete phase (DeletedCount), and the
// manifest those checkpoints carry is the same slice, unchanged, from the
// moment the batch's copy phase completes through its final sync — a
// batch of n deletes would otherwise cost n full store.Replace calls
// (each a delete-then-insert transaction over the whole manifest, share
// relocation's own persistRelocationManifestOnCheckpoint doc comment)
// instead of the one a concurrent job.TypeSync run actually needs to see.
// persistedBatchStart tracks which batch's manifest was last persisted so
// a checkpoint carrying the same batch's manifest again is skipped.
// removingDisks is fixed for the whole run (exactly the one disk being
// evacuated), so it is carried unchanged into every call that does
// persist.
func persistEvacuationManifestOnCheckpoint(ctx context.Context, save func(data []byte) error, store relocationManifestReplacer, removingDisks map[string]bool) func(data []byte) error {
	persistedBatchStart := -1
	return func(data []byte) error {
		if err := save(data); err != nil {
			return err
		}
		var cp cache.RebalanceCheckpoint
		if err := json.Unmarshal(data, &cp); err != nil {
			return fmt.Errorf("job: evacuation: decoding rebalance checkpoint: %w", err)
		}
		if len(cp.Manifest) == 0 || cp.BatchStart == persistedBatchStart {
			return nil
		}
		if err := store.Replace(ctx, cp.Manifest, removingDisks); err != nil {
			return fmt.Errorf("job: evacuation: persisting relocation manifest: %w", err)
		}
		persistedBatchStart = cp.BatchStart
		return nil
	}
}
