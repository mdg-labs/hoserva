package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

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
	// guard to need accounting for. Narrowed to relocationManifestStore
	// (Replace and Current, the two methods this file calls) rather than the
	// concrete *parity.RelocationManifestStore so a test can wrap the real
	// store with a call-counting fake to pin persistRelocationManifestOnCheckpoint's
	// write-amplification bound without touching internal/parity's own
	// algorithms; *parity.RelocationManifestStore satisfies it unchanged.
	// Current is needed for clearOwnedRelocationManifest's ownership check.
	Manifest relocationManifestStore
	// Open is the open-file checker both directions consult before and
	// after each copy. Production leaves it nil, which cache.Deps resolves
	// to the real /proc-scanning ProcOpenChecker; a unit test injects a
	// fake so it never walks the host's /proc.
	Open cache.OpenChecker
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
		toCache := p.To == shareRelocationToCache
		share, err := d.Share(ctx, p.Share)
		if err != nil {
			lookupErr := fmt.Errorf("job: share relocation: resolving share %q: %w", p.Share, err)
			// A resumed to-cache run kept its manifest for Resume, and this
			// failure ends the job failed, which can be neither resumed nor
			// cancelled, so ShareRelocationAbort never runs: clear it here.
			// Without a checkpoint nothing of this job was ever persisted,
			// so whatever the slot holds is another job's. Under a stop
			// requested by maintenance mode runJob records any error as
			// interrupted, so the manifest stays for Resume. The cache mount
			// is unknown, so ownership is judged by shape alone, as in the
			// abort.
			if d.Manifest == nil || !toCache || rc.InitialCheckpoint() == nil {
				return lookupErr
			}
			select {
			case <-rc.StopRequested():
				if ctx.Err() == nil && rc.KeepForResume() {
					return lookupErr
				}
			default:
			}
			if clearErr := clearOwnedRelocationManifest(context.WithoutCancel(ctx), d.Manifest, p.Share, ""); clearErr != nil {
				return &CancelCleanupError{
					Code: "share_relocation_manifest_clear_failed",
					Err:  fmt.Errorf("%w (also failed: %v)", lookupErr, clearErr),
				}
			}
			return lookupErr
		}
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
			report, err = cache.RelocateToArray(ctx, share, d.Config, cache.Deps{Open: d.Open}, hooks, rc.InitialCheckpoint())
		} else {
			report, err = cache.RelocateToCache(ctx, share, d.Config, cache.Deps{Sync: d.Sync, Open: d.Open}, hooks, rc.InitialCheckpoint())
		}
		if !report.StartedAt.IsZero() {
			_, _ = fmt.Fprintln(rc.Output(), report.Summary())
		}
		if d.Manifest != nil && toCache {
			// Every ending this run cannot resume from clears the manifest,
			// as evacuation does: a finished run (its trailing sync
			// succeeded, so parity already accounts for every delete), a
			// failed one (a failed job is never resumed), and a cancelled
			// one. Only a graceful stop (maintenance mode, an on-battery
			// pause) leaves a checkpoint Resume continues from, and only
			// rc.KeepForResume(), which decides under the same lock as
			// Scheduler.Cancel, commits to that. A manifest that outlived
			// its job would exempt later removals at the same disk+path
			// from every sync's guard (matchManifest, internal/parity/guard.go).
			resumable := err == nil && report.Interrupted && ctx.Err() == nil
			if resumable {
				resumable = rc.KeepForResume()
			}
			if !resumable {
				// A non-cancellable context: a Cancel landing between the
				// run's last action and this clear must not make the clear
				// itself fail with context.Canceled, which runJob could not
				// tell apart from the RunFunc's own reaction to the cancel
				// (isCancellationDerived, scheduler.go) and would drop,
				// stranding the manifest (#381, the same fix #378 made for
				// evacuation).
				clearCtx := context.WithoutCancel(ctx)
				if clearErr := clearOwnedRelocationManifest(clearCtx, d.Manifest, share.Name, filepath.Dir(share.CachePath)); clearErr != nil {
					runErr := err
					if runErr == nil && !report.Interrupted && report.Incomplete() {
						runErr = incompleteRelocationError(report)
					}
					if runErr == nil {
						return fmt.Errorf("job: share relocation: %w", clearErr)
					}
					// Always a *CancelCleanupError, never conditioned on
					// ctx.Err() (#379): a clear that fails while racing a
					// Cancel is then recorded rather than dropped, and
					// without a Cancel runJob keeps the combined message
					// under the plain failure.
					return &CancelCleanupError{
						Code: "share_relocation_manifest_clear_failed",
						Err:  fmt.Errorf("%w (also failed: %v)", runErr, clearErr),
					}
				}
			}
		}
		if err == nil && !report.Interrupted && report.Incomplete() {
			return &OutcomeError{Status: StatusFailed, Code: relocationIncompleteCode, Err: incompleteRelocationError(report)}
		}
		return err
	}
}

// relocationIncompleteCode is the error code of a share relocation that
// ran to its end but left entries behind: the job fails rather than
// succeeds, because the share's files are not all on the new side.
const relocationIncompleteCode = "relocation_incomplete"

const incompleteListLimit = 10

// incompleteRelocationError names what an incomplete relocation left
// behind and why. Runtime-only sockets are not part of it; the job log
// already carries every entry, this is the part a job's error text keeps.
func incompleteRelocationError(report cache.Report) error {
	var parts []string
	for _, e := range report.LeftBehind() {
		if e.Result == cache.ResultSkippedSocket {
			continue
		}
		why := string(e.Result)
		switch {
		case e.Err != "":
			why += ": " + e.Err
		case e.Reason != "":
			why += ": " + e.Reason
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", e.Path, why))
	}
	shown := parts
	if len(shown) > incompleteListLimit {
		shown = shown[:incompleteListLimit]
	}
	msg := fmt.Sprintf("share relocation incomplete: %d entries left behind: %s", len(parts), strings.Join(shown, "; "))
	if len(parts) > len(shown) {
		msg += fmt.Sprintf("; and %d more (see the job log)", len(parts)-len(shown))
	}
	return errors.New(msg)
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

// clearOwnedRelocationManifest clears the persisted relocation manifest only
// when it is still the array→cache one of the share called name, never blind:
// the store holds one shared slot, and a different job (a rebalance, an
// evacuation, another share's relocation) may have written it since. A
// manifest that cannot be read is an error rather than a clear or a silent
// keep: clearing could wipe another job's, keeping could strand this one,
// and a returned error is reported on the job (and leaves a cancelled one
// retryable) instead.
func clearOwnedRelocationManifest(ctx context.Context, store relocationManifestStore, name, cacheMount string) error {
	manifest, removingDisks, err := store.Current(ctx)
	if err != nil {
		return fmt.Errorf("reading relocation manifest: %w", err)
	}
	if !isRelocationManifestOf(name, cacheMount, manifest, removingDisks) {
		return nil
	}
	if err := store.Replace(ctx, nil, nil); err != nil {
		return fmt.Errorf("clearing relocation manifest: %w", err)
	}
	return nil
}

// isRelocationManifestOf reports whether the stored manifest is what
// RelocateToCache writes for the share called name: every entry lies under
// the share's name and targets one and the same disk (the cache mount the
// share's cache path sits on, when cacheMount is known), and no
// removing-disks set is stored (only evacuation writes one).
func isRelocationManifestOf(name, cacheMount string, manifest []parity.ManifestEntry, removingDisks map[string]bool) bool {
	if len(manifest) == 0 || len(removingDisks) > 0 {
		return false
	}
	if cacheMount == "" {
		cacheMount = manifest[0].TargetDisk
	}
	prefix := name + string(filepath.Separator)
	for _, e := range manifest {
		if e.TargetDisk != cacheMount || !strings.HasPrefix(e.RelPath, prefix) {
			return false
		}
	}
	return true
}

// ShareRelocationAbort is the AbortFunc hoservad registers for
// job.TypeShareRelocation (doc 09 §2, #732): Scheduler.Cancel of a queued or
// interrupted relocation never re-enters RunShareRelocation, so this is
// where that cancel releases the manifest an interrupted array→cache run
// kept for Resume. The clear is ownership-checked, so a manifest some other
// job has since written is left alone. A to-array relocation never writes
// one, and a deployment with no manifest store has nothing to clear.
//
// A share that can no longer be resolved (deleted, or its cache disk gone)
// must not make the job impossible to cancel, since Resume would fail on the
// same lookup. Then the cache mount is unknown and ownership falls back to
// the manifest's own shape: under the share's name, one target, no removing
// disks.
func ShareRelocationAbort(d ShareRelocationDeps) AbortFunc {
	return func(ctx context.Context, id string, params []byte) error {
		if d.Manifest == nil {
			return nil
		}
		p, err := decodeShareRelocationParams(params)
		if err != nil {
			return err
		}
		if p.To != shareRelocationToCache {
			return nil
		}
		var cacheMount string
		if share, err := d.Share(ctx, p.Share); err == nil {
			cacheMount = filepath.Dir(share.CachePath)
		}
		if err := clearOwnedRelocationManifest(ctx, d.Manifest, p.Share, cacheMount); err != nil {
			return fmt.Errorf("job: share relocation: %w", err)
		}
		return nil
	}
}
