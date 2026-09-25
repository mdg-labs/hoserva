package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mdg-labs/hoserva/internal/cache"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/store"
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
	// Store persists the disk's own removal state (doc 09 §4 step 2,
	// #359), held by this job's id: "evacuating" before this run's first
	// copy and on every resume, "evacuated" once the run finishes and its
	// post-check passes, and released only when this job is cancelled. A
	// plain failure keeps the disk "evacuating", so nothing new lands on
	// it until it is evacuated again or that run is cancelled. Required.
	Store removalStateStore
	// ArrayReady regenerates every pool mount from the store and applies
	// it to the running pool — cmd/hoservad wires the topology hook that
	// returns a failed live update rather than logging it — so a Store
	// change reaches the running mounts, not only the unit files. A
	// failure before the first copy fails the job before anything is
	// copied (doc 09 §4 step 2). Required.
	ArrayReady func(ctx context.Context) error
}

// removalStateStore is the subset of *store.ArrayStore RunEvacuation and
// EvacuationAbort use to hold and release doc 09 §4 step 2's removal
// state (#359).
type removalStateStore interface {
	SetRemovalState(ctx context.Context, mountpoint, state, jobID string) error
	ReleaseRemovalState(ctx context.Context, mountpoint, jobID string) (bool, error)
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
		if d.Store == nil {
			return fmt.Errorf("job: evacuation: Deps.Store is required")
		}
		if d.ArrayReady == nil {
			return fmt.Errorf("job: evacuation: Deps.ArrayReady is required")
		}
		if rc.JobID() == "" {
			return fmt.Errorf("job: evacuation: the run has no job id to hold the removal state")
		}
		err = d.run(ctx, rc, p)
		// Only Scheduler.Cancel of this running job cancels its ctx:
		// startJobLocked roots it at context.Background, and a
		// maintenance or battery stop of a resumable job closes
		// StopRequested instead. The user chose not to remove this disk
		// now, so whatever removal state this job holds is released and
		// the pool re-applied, wherever the cancel landed — including
		// during the pre-copy ArrayReady.
		if ctx.Err() == nil {
			return err
		}
		cleanupCtx := context.WithoutCancel(ctx)
		cleanupErr := err
		code := ""
		// d.run's own preliminary "end resumable" choice is committed
		// against a racing Cancel atomically, under the running job's own
		// lock (RunContext.KeepForResume, #378): whichever of the two
		// wins that race decides, so by the time d.run returns here with
		// ctx already cancelled, it has always already taken its own
		// non-resumable branch itself. Every non-resumable return d.run can
		// take clears the relocation manifest and removing-disks exemption
		// itself before returning — the ordinary post-copy ending inline,
		// and its two pre-copy returns (SetRemovalState, the pre-copy
		// ArrayReady) through the same earlyReturnErr/
		// clearOwnedEvacuationManifest helper a resumed run needs, since
		// either can fail after an earlier stop already committed that
		// state for Resume (#378) — so there is nothing left for this
		// outer pass to clear a second time, and reading the manifest
		// again here to check would only risk misreporting a transient
		// read error as a clear that never happened. A failure of that
		// clear comes back wrapped in a *CancelCleanupError; its code is
		// kept as this report's code rather than a further failure below
		// silently displacing it.
		var alreadyReported *CancelCleanupError
		if errors.As(err, &alreadyReported) {
			code = alreadyReported.Code
		}
		if relErr := releaseRemovalState(cleanupCtx, d.Store, d.ArrayReady, p.Mountpoint, rc.JobID()); relErr != nil {
			// The removal state itself may already be released (the
			// database now says RW) even though this specific failure is
			// in re-applying that to the live pool — runJob still records
			// the job cancelled (the user's cancel is honoured either
			// way), but must never silently drop a failure that can leave
			// the database and the live mounts disagreeing about whether
			// this disk takes writes (#364).
			cleanupErr = combineEvacuationErr(cleanupErr, "releasing the removal state after cancel", relErr)
			if code == "" {
				code = "evacuation_cancel_reapply_failed"
			}
		}
		if code != "" {
			return &CancelCleanupError{Code: code, Err: cleanupErr}
		}
		return err
	}
}

func (d EvacuationDeps) run(ctx context.Context, rc *RunContext, p EvacuationParams) error {
	// doc 09 §4 step 2: the disk stops taking new writes before this
	// run's first copy, and again on every resume (Resume re-enters this
	// same RunFunc). A failure of either step fails the job before any
	// copy runs.
	if err := d.Store.SetRemovalState(ctx, p.Mountpoint, store.RemovalStateEvacuating, rc.JobID()); err != nil {
		return d.earlyReturnErr(ctx, p.Mountpoint, fmt.Errorf("job: evacuation: marking %s removing: %w", p.Mountpoint, err))
	}
	if err := d.ArrayReady(ctx); err != nil {
		return d.earlyReturnErr(ctx, p.Mountpoint, fmt.Errorf("job: evacuation: applying no-create to the live pool: %w", err))
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
	//
	// Even once every other condition holds, rc.KeepForResume() still has
	// the last word: it and Cancel decide under the same lock, so a
	// Cancel landing anywhere between this preliminary check and the
	// caller's own outer ctx.Err() check (#378) can never leave this
	// run's manifest and exemption committed for Resume while the job
	// also ends up recorded cancelled. It is called only once this
	// preliminary check already holds — a stop that was never going to
	// end resumable anyway needs no atomic commit against Cancel, since
	// its state is cleared unconditionally just below regardless of how
	// Cancel's own race with it comes out.
	resumable := err == nil && report.Interrupted && ctx.Err() == nil
	if resumable {
		resumable = rc.KeepForResume()
	}
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
			combined := combineEvacuationErr(err, "clearing relocation manifest", clearErr)
			// ctx.Err() != nil here means this non-resumable ending is an
			// explicit Cancel (the doc comment on RunEvacuation's own
			// outer ctx.Err() check: only Scheduler.Cancel of a running
			// evacuation ever cancels this ctx). runJob's own reasonCancel
			// branch keeps only a *CancelCleanupError under a cancelled
			// outcome and drops anything else — a plain error here would
			// leave this failed clear silently unrecorded even though the
			// removing-disks exemption it tried to release may still be
			// persisted (#378).
			if ctx.Err() != nil {
				return &CancelCleanupError{Code: "evacuation_cancel_manifest_clear_failed", Err: combined}
			}
			return combined
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
	// The copy and its post-check both succeeded: the disk stays
	// no-create (steps 7-9, #358, have not run yet) but is no longer
	// mid-copy.
	if err := d.Store.SetRemovalState(ctx, p.Mountpoint, store.RemovalStateEvacuated, rc.JobID()); err != nil {
		return fmt.Errorf("job: evacuation: marking %s evacuated: %w", p.Mountpoint, err)
	}
	return nil
}

// earlyReturnErr wraps a failure from d.run's two pre-copy steps —
// SetRemovalState or the pre-copy ArrayReady — neither of which is a
// resumable ending (doc 09 §4 step 2 runs both on every resume too, before
// RunRebalance is ever re-entered, so resumable's own check at the bottom
// of d.run is never reached here). A resume can land here after an
// earlier graceful stop already committed this run's manifest and
// removing-disks exemption for Resume (RunContext.KeepForResume, #378):
// since this return does not keep the run for another resume, whatever
// that earlier stop left behind must not survive it. On a fresh
// (non-resumed) run nothing is persisted yet, so
// clearOwnedEvacuationManifest is a no-op — it only clears when the
// persisted removing-disks set still names exactly this job's own
// mountpoint. Uses a non-cancellable context, the same reasoning as
// d.run's own later clear: a Cancel landing during either failure must
// not leave the exemption stranded. A cancel whose clear itself fails is
// reported as a *CancelCleanupError (runJob's own doc comment on why: a
// plain error under a cancelled outcome is otherwise dropped); an
// ordinary failure's clear failure is folded into the same error instead,
// so the Failed job's recorded error names both.
func (d EvacuationDeps) earlyReturnErr(ctx context.Context, mountpoint string, runErr error) error {
	if d.Manifest == nil {
		return runErr
	}
	clearCtx := context.WithoutCancel(ctx)
	clearErr := clearOwnedEvacuationManifest(clearCtx, d.Manifest, mountpoint)
	if clearErr == nil {
		return runErr
	}
	combined := combineEvacuationErr(runErr, "clearing relocation manifest", clearErr)
	if ctx.Err() != nil {
		return &CancelCleanupError{Code: "evacuation_cancel_manifest_clear_failed", Err: combined}
	}
	return combined
}

// releaseRemovalState releases the removal state jobID holds on
// mountpoint and, when there was one, re-applies the pool so the disk
// takes writes again. A state some other job holds is left alone.
func releaseRemovalState(ctx context.Context, removal removalStateStore, arrayReady func(ctx context.Context) error, mountpoint, jobID string) error {
	released, err := removal.ReleaseRemovalState(ctx, mountpoint, jobID)
	if err != nil {
		return err
	}
	if !released {
		return nil
	}
	if err := arrayReady(ctx); err != nil {
		return fmt.Errorf("reapplying the live pool: %w", err)
	}
	return nil
}

// combineEvacuationErr reports the run's own failure, if any, as the
// primary error — the one that determines the job's error message and
// code — while still surfacing a subsequent cleanup failure (clearing the
// relocation manifest or the removal state) rather than silently dropping
// it. A cleanup failure never displaces the run's own error, only
// annotates it.
func combineEvacuationErr(runErr error, cleanupDescription string, cleanupErr error) error {
	if runErr == nil {
		return fmt.Errorf("job: evacuation: %s: %w", cleanupDescription, cleanupErr)
	}
	return fmt.Errorf("%w (also failed %s: %v)", runErr, cleanupDescription, cleanupErr)
}

// clearOwnedEvacuationManifest clears the persisted relocation manifest
// and removing-disks exemption only when the persisted removing-disks set
// still names exactly mountpoint and nothing else — never blind, because
// the manifest store holds one shared slot and a different array-write job
// could have claimed it for an unrelated relocation by the time this runs
// (EvacuationAbort's own doc comment). Used by EvacuationAbort, whose own
// job never ran the copy that would otherwise have cleared this state
// inline, and by d.run's own earlyReturnErr, for the two pre-copy returns
// (SetRemovalState, the pre-copy ArrayReady) a resume can hit before ever
// reaching that copy again (#378) — RunEvacuation's own d.run clears this
// state, one way or the other, on every ending it does not keep for
// Resume.
func clearOwnedEvacuationManifest(ctx context.Context, manifest relocationManifestStore, mountpoint string) error {
	_, removingDisks, err := manifest.Current(ctx)
	if err != nil {
		return fmt.Errorf("reading relocation manifest: %w", err)
	}
	if len(removingDisks) != 1 || !removingDisks[mountpoint] {
		return nil
	}
	if err := manifest.Replace(ctx, nil, nil); err != nil {
		return fmt.Errorf("clearing relocation manifest: %w", err)
	}
	return nil
}

// EvacuationAbort is the AbortFunc hoservad registers for
// job.TypeEvacuation (doc 09 §4, #274, #359): Scheduler.Cancel of a
// queued or interrupted evacuation never re-enters RunEvacuation, so this
// is where that cancel releases what the job left behind.
//
// The relocation manifest and its removing-disks exemption (#274) are
// cleared only when the persisted removing-disks set still names exactly
// this job's own mountpoint and nothing else — never blind — because the
// manifest store holds one shared slot and, by the time Cancel runs, a
// different array-write job (doc 01 §4's mutual exclusion only ever
// excludes a *running* job of the same class, never an interrupted one)
// could already have claimed it for an unrelated relocation. Without the
// clear, a stale exemption would keep exempting the disk from the
// guard's zero-files rule, and a stale manifest entry could exempt a
// later, unrelated removal of the same path (matchManifest,
// internal/parity/guard.go).
//
// The disk's removal state (#359) is released by the job that holds it,
// id, whether or not a manifest was ever persisted — an evacuation
// stopped during its first batch's copy has none — and never when some
// other job holds it.
func EvacuationAbort(manifest relocationManifestStore, removal removalStateStore, arrayReady func(ctx context.Context) error) AbortFunc {
	return func(ctx context.Context, id string, params []byte) error {
		p, err := decodeEvacuationParams(params)
		if err != nil {
			return err
		}
		if err := clearOwnedEvacuationManifest(ctx, manifest, p.Mountpoint); err != nil {
			return fmt.Errorf("job: evacuation: %w", err)
		}
		if err := releaseRemovalState(ctx, removal, arrayReady, p.Mountpoint, id); err != nil {
			return fmt.Errorf("job: evacuation: releasing removal state: %w", err)
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
