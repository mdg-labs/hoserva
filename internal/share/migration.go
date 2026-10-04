package share

import (
	"context"
	"fmt"

	"github.com/mdg-labs/hoserva/internal/pool"
)

// CompleteMigrationResult says what CompleteMigration did to each share.
type CompleteMigrationResult struct {
	// Applied are the shares whose Unraid cache mode was applied, as
	// "name: array-only -> cache-then-move".
	Applied []string
	// Kept are the shares whose Unraid cache mode needs a cache disk this array
	// does not have: they stay array-only with the mode still recorded.
	Kept []string
	// Prepared is how many share branch directories were brought to the shared
	// group and setgid mode.
	Prepared int
}

// CompleteMigration is the shares' part of the point of no return (doc 05 §4
// step 17), run once the data disks are mounted read-write and the cache is
// mounted. For each share it applies the cache mode the import deferred
// (shares.target_cache_mode, which no cache could take before the cache
// existed), and brings the share's top-level branch directory on every branch
// its cache mode uses to the setgid mode and shared group of Q26 (the
// import wrote nothing to an adopted disk). It touches each top-level
// directory only, never what is under it, so no file's ownership is rewritten.
//
// It is refused while the migration is pending (ErrMigrationPending): the data
// disks are read-only until the point of no return. It is safe to run again: a
// directory already in the right mode and group is set again, and a share
// whose mode was applied has no target left. The pool's mounts, smb.conf and
// exports are the caller's to regenerate afterwards (ApplyTopology).
func (s *Service) CompleteMigration(ctx context.Context) (CompleteMigrationResult, error) {
	defer s.postCommit(ctx)
	var res CompleteMigrationResult
	if s.Shares == nil {
		return res, fmt.Errorf("share: CompleteMigration requires a ShareStore")
	}
	settings, disks, err := s.array(ctx)
	if err != nil {
		return res, err
	}
	if settings.MigrationPending {
		return res, fmt.Errorf("%w: the shares' directories and cache modes are applied at the point of no return", ErrMigrationPending)
	}
	data, cache, _ := splitDisks(disks)
	rows, err := s.Shares.List(ctx)
	if err != nil {
		return res, fmt.Errorf("share: listing shares: %w", err)
	}
	for _, row := range rows {
		sh := shareFromStore(row)
		mode := sh.CacheMode
		applied := false
		if sh.TargetCacheMode != "" {
			if err := validateCacheMode(sh.TargetCacheMode); err != nil {
				return res, err
			}
			if needsCache(sh.TargetCacheMode) && cache == "" {
				res.Kept = append(res.Kept, fmt.Sprintf("%s: %s needs a cache disk and this array has none", sh.Name, sh.TargetCacheMode))
			} else {
				mode, applied = sh.TargetCacheMode, true
			}
		}
		roots, err := shareDataRoots(sh.Name, mode, data, cache)
		if err != nil {
			return res, err
		}
		if err := s.ensureBranchDirs(roots); err != nil {
			return res, err
		}
		res.Prepared += len(roots)
		if !applied {
			continue
		}
		prev := sh.CacheMode
		sh.CacheMode, sh.TargetCacheMode = mode, ""
		sh.UpdatedAt = s.now().UTC()
		if err := s.Shares.Update(ctx, toStore(sh)); err != nil {
			return res, fmt.Errorf("share: applying the cache mode of %s: %w", sh.Name, err)
		}
		res.Applied = append(res.Applied, fmt.Sprintf("%s: %s -> %s", sh.Name, prev, mode))
	}
	return res, nil
}

func needsCache(mode pool.CacheMode) bool {
	return mode == pool.CacheThenMove || mode == pool.CacheOnly
}
