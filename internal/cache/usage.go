package cache

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ComputeUsageBreakdown walks each share's cache-side tree once — as a
// by-product of the mover job that just finished, never on a timer
// (Q87 / Q13) — and combines those sums with a single statfs(2) of the
// cache mount for the "other" remainder.
//
// Returns nil (no error) when cacheMount is empty: there is no cache
// disk, so there is nothing honest to persist for the breakdown.
func ComputeUsageBreakdown(cacheMount string, shares []UsageShare, at time.Time) (*UsageBreakdown, error) {
	if cacheMount == "" {
		return nil, nil
	}
	var appdata, pending int64
	for _, s := range shares {
		sum, err := sumTreeBytes(s.Path)
		if err != nil {
			return nil, fmt.Errorf("summing %s (%s): %w", s.Name, s.Mode, err)
		}
		switch s.Mode {
		case "cache-only":
			appdata += sum
		case "cache-then-move":
			pending += sum
		default:
			// array-only (and unknown) share dirs on cache, if any, fall
			// into "other" via the statfs remainder below — do not add
			// them to appdata or pending.
		}
	}
	usage, err := UsageBytes(cacheMount)
	if err != nil {
		return nil, fmt.Errorf("statfs %s: %w", cacheMount, err)
	}
	used := usage.TotalBytes - usage.FreeBytes
	if used < 0 {
		used = 0
	}
	other := used - appdata - pending
	if other < 0 {
		other = 0
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	return &UsageBreakdown{
		AppdataBytes:      appdata,
		PendingMovesBytes: pending,
		OtherBytes:        other,
		ComputedAt:        at.UTC(),
	}, nil
}

// sumTreeBytes totals regular-file sizes under root, skipping mover temp
// suffixes the same way enumerateFiles does. Missing roots are zero, not
// an error — a share that has never received a write has no directory yet.
func sumTreeBytes(root string) (int64, error) {
	if _, err := os.Stat(root); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && path != root {
				return nil
			}
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		if strings.Contains(d.Name(), tempSuffix) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}
