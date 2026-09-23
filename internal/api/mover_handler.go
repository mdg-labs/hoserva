package api

import (
	"context"
	"errors"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/cache"
)

// GetLastMoverRun returns the structured result of the most recent
// finished mover run (#273, doc 09 §2, doc 03 §3.6). Null when none has
// finished yet — never a zero placeholder.
func (h *Handler) GetLastMoverRun(ctx context.Context) (apiv1.NilMoverRunResult, error) {
	if h.MoverResults == nil {
		var null apiv1.NilMoverRunResult
		null.SetToNull()
		return null, nil
	}
	run, err := h.MoverResults.LastRun(ctx)
	if err != nil {
		if errors.Is(err, cache.ErrNoMoverRun) {
			var null apiv1.NilMoverRunResult
			null.SetToNull()
			return null, nil
		}
		return apiv1.NilMoverRunResult{}, err
	}
	return apiv1.NewNilMoverRunResult(moverRunResultToAPI(run)), nil
}

// GetCacheUsage returns the persisted cache usage breakdown (#273, Q87).
// Null when no mover run has computed it yet.
func (h *Handler) GetCacheUsage(ctx context.Context) (apiv1.NilCacheUsageBreakdown, error) {
	if h.MoverResults == nil {
		var null apiv1.NilCacheUsageBreakdown
		null.SetToNull()
		return null, nil
	}
	usage, err := h.MoverResults.CacheUsage(ctx)
	if err != nil {
		if errors.Is(err, cache.ErrNoMoverRun) {
			var null apiv1.NilCacheUsageBreakdown
			null.SetToNull()
			return null, nil
		}
		return apiv1.NilCacheUsageBreakdown{}, err
	}
	return apiv1.NewNilCacheUsageBreakdown(apiv1.CacheUsageBreakdown{
		AppdataBytes:      usage.AppdataBytes,
		PendingMovesBytes: usage.PendingMovesBytes,
		OtherBytes:        usage.OtherBytes,
		ComputedAt:        usage.ComputedAt,
	}), nil
}

func moverRunResultToAPI(run cache.PersistedRun) apiv1.MoverRunResult {
	skipped := make([]apiv1.MoverSkippedEntry, 0, len(run.Skipped))
	for _, e := range run.Skipped {
		entry := apiv1.MoverSkippedEntry{
			Share:  e.Share,
			Path:   e.Path,
			Result: string(e.Result),
		}
		if e.Bytes > 0 {
			entry.Bytes = apiv1.NewOptInt64(e.Bytes)
		}
		if e.Reason != "" {
			entry.Reason = apiv1.NewOptString(e.Reason)
		}
		skipped = append(skipped, entry)
	}
	return apiv1.MoverRunResult{
		StartedAt:   run.StartedAt,
		FinishedAt:  run.FinishedAt,
		DurationMs:  run.DurationMs,
		FilesMoved:  int32(run.FilesMoved),
		BytesMoved:  run.BytesMoved,
		Interrupted: run.Interrupted,
		Skipped:     skipped,
	}
}
