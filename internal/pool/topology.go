package pool

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// CatchAllPath is where the catch-all pool mounts (doc 02 §1, Q12).
const CatchAllPath = "/mnt/user"

// ArrayRootPath is where the mover's own array-only write targets mount
// (doc 02 §1, Q12; doc 09 §2) — a separate hierarchy from CatchAllPath,
// never nested under it, so it needs no RequiresMountsFor= dependency
// on the catch-all. Exported so a caller building unit names from paths
// (config.WritePoolMounts's own reconciliation) can recognize a mover
// target unit without duplicating this path as a literal.
const ArrayRootPath = "/run/hoserva/array"

// SharePath returns share's own mount point under the catch-all.
func SharePath(share string) string {
	return CatchAllPath + "/" + share
}

// MoverTargetPath returns share's own mover write-target mount point.
func MoverTargetPath(share string) string {
	return ArrayRootPath + "/" + share
}

// ErrNoDataDisks is returned by any constructor below asked to build
// branches with nothing to build them from.
var ErrNoDataDisks = errors.New("pool: at least one data disk is required")

// ErrDataDiskAlreadyPresent is AppendDataDisk's refusal: mount is already
// one of dataDisks.
var ErrDataDiskAlreadyPresent = errors.New("pool: data disk is already in the pool")

// ErrDataDiskNotPresent is RemoveDataDisk's refusal: mount is not one of
// dataDisks.
var ErrDataDiskNotPresent = errors.New("pool: data disk is not in the pool")

// ErrCacheOnlyNotMoved is MoverTargetMount's refusal for a CacheOnly
// share: that data lives on cache permanently and is never moved
// (share.go), so no mover write-target mount may exist for it.
var ErrCacheOnlyNotMoved = errors.New("pool: cache-only share has no mover write target")

// CatchAllMount builds /mnt/user (doc 02 §1, Q12): a mergerfs pool over
// every data disk, RW, the default create policy — ordered after each
// disk's own block-device mount, so `ls /mnt/user` and a stray
// top-level write land on the array, never the boot device.
func CatchAllMount(dataDisks []string, opts Options) (Mount, error) {
	return catchAllMount(dataDisks, "", opts)
}

// CatchAllMountRemoving builds /mnt/user exactly as CatchAllMount does,
// except removingDisk's own branch reads NC instead of RW (doc 09 §4 step
// 2: "set its branches to no-create in every mount, so nothing new lands
// on it, ... for that disk only"). removingDisk stays in dataDisks and
// stays mounted — it still holds files the evacuation plan
// (internal/cache's PlanEvacuation) is still reading off it while its
// removing state is active; this only stops mergerfs from placing
// anything new there. A caller moves to plain CatchAllMount, with
// removingDisk excluded from dataDisks via RemoveDataDisk, only once
// evacuation has finished (doc 09 §4 step 7).
func CatchAllMountRemoving(dataDisks []string, removingDisk string, opts Options) (Mount, error) {
	return catchAllMount(dataDisks, removingDisk, opts)
}

func catchAllMount(dataDisks []string, removingDisk string, opts Options) (Mount, error) {
	if len(dataDisks) == 0 {
		return Mount{}, ErrNoDataDisks
	}
	if err := checkRemovingDisk(dataDisks, removingDisk); err != nil {
		return Mount{}, err
	}
	return Mount{
		Where:             CatchAllPath,
		What:              rwBranchesRemoving(dataDisks, removingDisk),
		FSName:            "hoserva-pool",
		CreatePolicy:      DefaultCreatePolicy,
		Options:           opts,
		Description:       "Hoserva catch-all pool",
		RequiresMountsFor: append([]string(nil), dataDisks...),
	}, nil
}

// ShareMount builds share's own /mnt/user/<share> mount (doc 02 §1,
// Q12): its branches follow its cache mode, ordered after the catch-all
// — mounting a share before /mnt/user is up makes it silently
// unreachable rather than exposed early, once the catch-all does mount
// over it (S6, doc 08 §6) — and after whichever block-device mounts its
// own branches sit on. cachePath is the cache disk's own mountpoint
// (e.g. "/mnt/cache"); it is unused, and may be empty, for an
// array-only share.
func ShareMount(share Share, dataDisks []string, cachePath string, opts Options) (Mount, error) {
	return shareMount(share, dataDisks, cachePath, "", opts)
}

// ShareMountRemoving builds share's own /mnt/user/<share> mount exactly
// as ShareMount does, except removingDisk's own branch reads NC instead
// of whatever create mode it would otherwise have (doc 09 §4 step 2). A
// CacheThenMove share's data-disk branches are already NC regardless —
// writes always land on cache first, never directly on a data disk — so
// removingDisk changes nothing there; an ArrayOnly share's branches are
// RW by default, and that is exactly what step 2 needs turned off for
// removingDisk.
func ShareMountRemoving(share Share, dataDisks []string, cachePath, removingDisk string, opts Options) (Mount, error) {
	return shareMount(share, dataDisks, cachePath, removingDisk, opts)
}

func shareMount(share Share, dataDisks []string, cachePath, removingDisk string, opts Options) (Mount, error) {
	if err := ValidateShareName(share.Name); err != nil {
		return Mount{}, err
	}

	var what string
	requires := []string{CatchAllPath}

	switch share.CacheMode {
	case CacheThenMove:
		if cachePath == "" {
			return Mount{}, fmt.Errorf("pool: share %q is cache-then-move but has no cache path", share.Name)
		}
		if len(dataDisks) == 0 {
			return Mount{}, ErrNoDataDisks
		}
		if err := checkRemovingDisk(dataDisks, removingDisk); err != nil {
			return Mount{}, err
		}
		branches := append([]string{cachePath + "/" + share.Name + "=RW"}, shareBranchesRemoving(dataDisks, share.Name, "NC", removingDisk)...)
		what = strings.Join(branches, ":")
		requires = append(requires, cachePath)
		requires = append(requires, dataDisks...)
	case CacheOnly:
		if cachePath == "" {
			return Mount{}, fmt.Errorf("pool: share %q is cache-only but has no cache path", share.Name)
		}
		what = cachePath + "/" + share.Name + "=RW"
		requires = append(requires, cachePath)
	case ArrayOnly:
		if len(dataDisks) == 0 {
			return Mount{}, ErrNoDataDisks
		}
		if err := checkRemovingDisk(dataDisks, removingDisk); err != nil {
			return Mount{}, err
		}
		what = strings.Join(shareBranchesRemoving(dataDisks, share.Name, "RW", removingDisk), ":")
		requires = append(requires, dataDisks...)
	default:
		return Mount{}, fmt.Errorf("pool: share %q has unknown cache mode %q", share.Name, share.CacheMode)
	}

	return Mount{
		Where:             SharePath(share.Name),
		What:              what,
		FSName:            "hoserva-" + share.Name,
		CreatePolicy:      share.CreatePolicy,
		Options:           opts,
		Description:       fmt.Sprintf("Hoserva share %s", share.Name),
		RequiresMountsFor: requires,
	}, nil
}

// MoverTargetMount builds share's own /run/hoserva/array/<share> mount
// (doc 02 §1, Q12; doc 09 §2): array-only branches with the share's own
// create policy, so mergerfs — not the mover — places every file the
// mover relocates (doc 09 §2, "one placement algorithm").
func MoverTargetMount(share Share, dataDisks []string, opts Options) (Mount, error) {
	return moverTargetMount(share, dataDisks, "", opts)
}

// MoverTargetMountRemoving builds share's own mover write-target mount
// exactly as MoverTargetMount does, except removingDisk's own branch
// reads NC instead of RW (doc 09 §4 step 2): the mover must stop placing
// newly relocated files on a disk that is being evacuated, exactly like
// every other mount step 2 covers.
func MoverTargetMountRemoving(share Share, dataDisks []string, removingDisk string, opts Options) (Mount, error) {
	return moverTargetMount(share, dataDisks, removingDisk, opts)
}

func moverTargetMount(share Share, dataDisks []string, removingDisk string, opts Options) (Mount, error) {
	if err := ValidateShareName(share.Name); err != nil {
		return Mount{}, err
	}
	if share.CacheMode == CacheOnly {
		return Mount{}, fmt.Errorf("%w: %q", ErrCacheOnlyNotMoved, share.Name)
	}
	if len(dataDisks) == 0 {
		return Mount{}, ErrNoDataDisks
	}
	if err := checkRemovingDisk(dataDisks, removingDisk); err != nil {
		return Mount{}, err
	}
	return Mount{
		Where:             MoverTargetPath(share.Name),
		What:              strings.Join(shareBranchesRemoving(dataDisks, share.Name, "RW", removingDisk), ":"),
		FSName:            "hoserva-" + share.Name,
		CreatePolicy:      share.CreatePolicy,
		Options:           opts,
		Description:       fmt.Sprintf("Hoserva share %s — mover write target", share.Name),
		RequiresMountsFor: append([]string(nil), dataDisks...),
	}, nil
}

// AppendDataDisk returns dataDisks with mount appended (doc 02 §4 "Adding
// a disk" step 5: "add to the branch lists of the catch-all and every
// share mount"), refusing (ErrDataDiskAlreadyPresent) a mount already in
// the list — every branch list this package builds is a plain ordered
// slice with no de-duplication of its own, so a caller appending the same
// disk twice would otherwise mount it as two branches of the same pool.
func AppendDataDisk(dataDisks []string, mount string) ([]string, error) {
	for _, d := range dataDisks {
		if d == mount {
			return nil, fmt.Errorf("%w: %s", ErrDataDiskAlreadyPresent, mount)
		}
	}
	return append(append([]string(nil), dataDisks...), mount), nil
}

// RemoveDataDisk returns dataDisks with mount removed (doc 09 §4 step 7:
// "remove from every mergerfs branch list, remount"), refusing
// (ErrDataDiskNotPresent) a mount that isn't one of dataDisks — the
// mirror of AppendDataDisk's own refusal to add one twice. The caller is
// expected to have already run internal/cache's PlanEvacuation/
// RunRebalance and its own post-check to completion (steps 1-6) before
// this ever runs: this function has no way to know whether mount still
// holds files, and does not try to.
func RemoveDataDisk(dataDisks []string, mount string) ([]string, error) {
	for i, d := range dataDisks {
		if d == mount {
			out := make([]string, 0, len(dataDisks)-1)
			out = append(out, dataDisks[:i]...)
			out = append(out, dataDisks[i+1:]...)
			return out, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrDataDiskNotPresent, mount)
}

func rwBranches(disks []string) string {
	return rwBranchesRemoving(disks, "")
}

// rwBranchesRemoving is rwBranches with removingDisk's own branch (when
// non-empty) marked NC instead of RW (doc 09 §4 step 2) — CatchAllMount's
// and CatchAllMountRemoving's shared implementation.
func rwBranchesRemoving(disks []string, removingDisk string) string {
	branches := make([]string, len(disks))
	for i, d := range disks {
		mode := "RW"
		if d == removingDisk {
			mode = "NC"
		}
		branches[i] = d + "=" + mode
	}
	return strings.Join(branches, ":")
}

// shareBranchesRemoving builds one "<disk>/<share>=<mode>" branch per
// disk, with removingDisk's own branch (when non-empty) marked NC
// regardless of mode (doc 09 §4 step 2) — ShareMount/ShareMountRemoving's
// and MoverTargetMount/MoverTargetMountRemoving's shared implementation.
func shareBranchesRemoving(disks []string, share, mode, removingDisk string) []string {
	branches := make([]string, len(disks))
	for i, d := range disks {
		m := mode
		if d == removingDisk {
			m = "NC"
		}
		branches[i] = d + "/" + share + "=" + m
	}
	return branches
}

// checkRemovingDisk refuses a removingDisk that is not exactly one of
// dataDisks (a trailing slash, an uncleaned path, a stale list): no
// branch would be marked NC, so mergerfs would keep placing new files on
// the disk being evacuated and the post-check would fail only after the
// whole copy and sync cycle.
func checkRemovingDisk(dataDisks []string, removingDisk string) error {
	if removingDisk == "" || slices.Contains(dataDisks, removingDisk) {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrDataDiskNotPresent, removingDisk)
}
