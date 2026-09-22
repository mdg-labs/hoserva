package api

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/parity"
)

type parityCache struct {
	diff  parity.DiffReport
	guard parity.GuardResult
	set   bool
}

type paritySnapshotStore struct {
	mu    sync.RWMutex
	cache parityCache
}

func (h *Handler) parityStore() *paritySnapshotStore {
	h.parityOnce.Do(func() {
		if h.paritySnap == nil {
			h.paritySnap = &paritySnapshotStore{}
		}
	})
	return h.paritySnap
}

func (h *Handler) GetParity(ctx context.Context) (*apiv1.ParitySnapshot, error) {
	if engineUnavailable(h.Parity) {
		return nil, &apiError{code: "not_configured", statusCode: 501, message: "parity status is not available on this daemon"}
	}
	status, err := h.Parity.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("parity status: %w", err)
	}
	snap := parityStatusToAPI(status)
	store := h.parityStore()
	store.mu.RLock()
	if store.cache.set {
		attachDiffToSnapshot(snap, store.cache.diff, store.cache.guard)
	}
	store.mu.RUnlock()
	return snap, nil
}

func (h *Handler) RunParityDiff(ctx context.Context) (*apiv1.ParityDiffResult, error) {
	if engineUnavailable(h.Parity) {
		return nil, &apiError{code: "not_configured", statusCode: 501, message: "parity diff is not available on this daemon"}
	}
	diff, err := h.Parity.Diff(ctx)
	if err != nil {
		return nil, fmt.Errorf("parity diff: %w", err)
	}
	var manifest []parity.ManifestEntry
	var removingDisks map[string]bool
	if h.RelocationManifest != nil {
		manifest, removingDisks, err = h.RelocationManifest.Current(ctx)
		if err != nil {
			return nil, fmt.Errorf("parity diff: loading relocation manifest: %w", err)
		}
		manifest, err = parity.ConfirmManifestTargets(ctx, h.Parity, diff, manifest)
		if err != nil {
			return nil, fmt.Errorf("parity diff: confirming relocation manifest targets: %w", err)
		}
	}
	guard := h.ParityGuard.Evaluate(diff, manifest, removingDisks)
	annotated := guard.Diff
	store := h.parityStore()
	store.mu.Lock()
	store.cache = parityCache{diff: annotated, guard: guard, set: true}
	store.mu.Unlock()
	return &apiv1.ParityDiffResult{
		Groups: diffToAPIGroups(annotated, guard),
		Guard:  guardToAPI(guard),
	}, nil
}

func parityStatusToAPI(status parity.ParityStatus) *apiv1.ParitySnapshot {
	snap := &apiv1.ParitySnapshot{
		Freshness:        freshnessToAPI(status.Freshness),
		ChangedSinceSync: apiv1.NewOptInt32(int32(status.ChangedSinceSync)),
		DataDisks:        apiv1.NewOptInt32(int32(status.DataDisks)),
		ParityDisks:      apiv1.NewOptInt32(int32(status.ParityDisks)),
	}
	if !status.LastSyncAt.IsZero() {
		snap.LastSyncAt = apiv1.NewOptDateTime(status.LastSyncAt.UTC())
	}
	return snap
}

func attachDiffToSnapshot(snap *apiv1.ParitySnapshot, diff parity.DiffReport, guard parity.GuardResult) {
	snap.Guard = apiv1.NewOptParityGuardState(guardToAPI(guard))
	snap.Groups = diffToAPIGroups(diff, guard)
}

func freshnessToAPI(level parity.FreshnessLevel) apiv1.ParityFreshness {
	switch level {
	case parity.FreshnessAmber:
		return apiv1.ParityFreshnessAmber
	case parity.FreshnessRed:
		return apiv1.ParityFreshnessRed
	default:
		return apiv1.ParityFreshnessGreen
	}
}

func guardToAPI(result parity.GuardResult) apiv1.ParityGuardState {
	out := apiv1.ParityGuardState{WouldBlock: result.Blocked}
	if !result.Blocked {
		return out
	}
	out.Triggers = make([]apiv1.ParityGuardTrigger, 0, len(result.Triggers))
	for _, trigger := range result.Triggers {
		out.Triggers = append(out.Triggers, guardTriggerToAPI(trigger))
	}
	out.RemovedCount = apiv1.NewOptInt32(int32(result.RemovedCount))
	out.RemovedUpdatedPercent = apiv1.NewOptFloat32(float32(result.RemovedUpdatedPercent))
	if len(result.ZeroFilesDisks) > 0 {
		disks := make([]apiv1.ParityZeroFilesDisk, 0, len(result.ZeroFilesDisks))
		for _, disk := range result.ZeroFilesDisks {
			disks = append(disks, apiv1.ParityZeroFilesDisk{
				Disk:        disk.Disk,
				FilesBefore: int32(disk.FilesBefore),
			})
		}
		out.ZeroFilesDisks = disks
	}
	if summary := guardSummary(result); summary != "" {
		out.Summary = apiv1.NewOptString(summary)
	}
	return out
}

func guardTriggerToAPI(trigger parity.GuardTrigger) apiv1.ParityGuardTrigger {
	switch trigger {
	case parity.TriggerRemovedUpdatedPercent:
		return apiv1.ParityGuardTriggerRemovedUpdatedPercent
	case parity.TriggerZeroFiles:
		return apiv1.ParityGuardTriggerZeroFiles
	default:
		return apiv1.ParityGuardTriggerRemovedCount
	}
}

func guardSummary(result parity.GuardResult) string {
	if !result.Blocked {
		return ""
	}
	var parts []string
	if guardHasTrigger(result, parity.TriggerRemovedCount) {
		parts = append(parts, fmt.Sprintf("%d removed files exceed the threshold", result.RemovedCount))
	}
	if guardHasTrigger(result, parity.TriggerRemovedUpdatedPercent) {
		parts = append(parts, fmt.Sprintf("%.1f%% removed+updated exceeds the threshold", result.RemovedUpdatedPercent))
	}
	if guardHasTrigger(result, parity.TriggerZeroFiles) {
		for _, disk := range result.ZeroFilesDisks {
			parts = append(parts, fmt.Sprintf("%s dropped to zero files (had %d)", disk.Disk, disk.FilesBefore))
		}
	}
	return strings.Join(parts, "; ")
}

func diffToAPIGroups(diff parity.DiffReport, guard parity.GuardResult) []apiv1.ParityDiffGroup {
	accounted := accountedRemovalKeys(guard.AccountedRemovals)
	groups := []apiv1.ParityDiffGroup{
		{
			Category: apiv1.ParityDiffCategoryRemoved,
			Count:    int32(diff.Removed - diff.MovedByHoserva),
			Paths:    diffPaths(diff.RemovedFiles, accounted),
		},
		{
			Category: apiv1.ParityDiffCategoryUpdated,
			Count:    int32(diff.Updated),
		},
		{
			Category: apiv1.ParityDiffCategoryAdded,
			Count:    int32(diff.Added),
			Paths:    diffFilePaths(diff.AddedFiles),
		},
		{
			Category: apiv1.ParityDiffCategoryMoved,
			Count:    int32(diff.Moved),
		},
		{
			Category: apiv1.ParityDiffCategoryCopied,
			Count:    int32(diff.Copied),
		},
		{
			Category: apiv1.ParityDiffCategoryMovedByHoserva,
			Count:    int32(diff.MovedByHoserva),
			Paths:    manifestPaths(guard.AccountedRemovals),
		},
	}
	return groups
}

type diffPathKey struct {
	disk string
	path string
}

func accountedRemovalKeys(entries []parity.ManifestEntry) map[diffPathKey]struct{} {
	out := make(map[diffPathKey]struct{}, len(entries))
	for _, entry := range entries {
		out[diffPathKey{
			disk: filepath.Clean(entry.SourceDisk),
			path: entry.RelPath,
		}] = struct{}{}
	}
	return out
}

func diffPaths(files []parity.DiffFile, skip map[diffPathKey]struct{}) []string {
	if len(files) == 0 {
		return nil
	}
	out := make([]string, 0, len(files))
	for _, file := range files {
		key := diffPathKey{disk: filepath.Clean(file.Disk), path: file.RelPath}
		if skip != nil {
			if _, ok := skip[key]; ok {
				continue
			}
		}
		out = append(out, formatDiffPath(file.Disk, file.RelPath))
	}
	return out
}

func diffFilePaths(files []parity.DiffFile) []string {
	if len(files) == 0 {
		return nil
	}
	out := make([]string, 0, len(files))
	for _, file := range files {
		out = append(out, formatDiffPath(file.Disk, file.RelPath))
	}
	return out
}

func manifestPaths(entries []parity.ManifestEntry) []string {
	if len(entries) == 0 {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, formatDiffPath(entry.SourceDisk, entry.RelPath))
	}
	return out
}

func formatDiffPath(mount, relPath string) string {
	return filepath.Join(filepath.Clean(mount), relPath)
}

func guardHasTrigger(result parity.GuardResult, trigger parity.GuardTrigger) bool {
	for _, got := range result.Triggers {
		if got == trigger {
			return true
		}
	}
	return false
}
