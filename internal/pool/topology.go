package pool

import (
	"errors"
	"fmt"
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

// ErrCacheOnlyNotMoved is MoverTargetMount's refusal for a CacheOnly
// share: that data lives on cache permanently and is never moved
// (share.go), so no mover write-target mount may exist for it.
var ErrCacheOnlyNotMoved = errors.New("pool: cache-only share has no mover write target")

// CatchAllMount builds /mnt/user (doc 02 §1, Q12): a mergerfs pool over
// every data disk, RW, the default create policy — ordered after each
// disk's own block-device mount, so `ls /mnt/user` and a stray
// top-level write land on the array, never the boot device.
func CatchAllMount(dataDisks []string, opts Options) (Mount, error) {
	if len(dataDisks) == 0 {
		return Mount{}, ErrNoDataDisks
	}
	return Mount{
		Where:             CatchAllPath,
		What:              rwBranches(dataDisks),
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
		branches := append([]string{cachePath + "/" + share.Name + "=RW"}, shareBranches(dataDisks, share.Name, "NC")...)
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
		what = strings.Join(shareBranches(dataDisks, share.Name, "RW"), ":")
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
	if err := ValidateShareName(share.Name); err != nil {
		return Mount{}, err
	}
	if share.CacheMode == CacheOnly {
		return Mount{}, fmt.Errorf("%w: %q", ErrCacheOnlyNotMoved, share.Name)
	}
	if len(dataDisks) == 0 {
		return Mount{}, ErrNoDataDisks
	}
	return Mount{
		Where:             MoverTargetPath(share.Name),
		What:              strings.Join(shareBranches(dataDisks, share.Name, "RW"), ":"),
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

func rwBranches(disks []string) string {
	branches := make([]string, len(disks))
	for i, d := range disks {
		branches[i] = d + "=RW"
	}
	return strings.Join(branches, ":")
}

func shareBranches(disks []string, share, mode string) []string {
	branches := make([]string, len(disks))
	for i, d := range disks {
		branches[i] = d + "/" + share + "=" + mode
	}
	return branches
}
