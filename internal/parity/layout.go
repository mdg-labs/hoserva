package parity

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// parityFileName and contentFileName are the files a parity or content
// directive points SnapRAID at, directly under a disk's mount point
// (doc 02 §2).
const (
	parityFileName  = "snapraid.parity"
	contentFileName = "snapraid.content"
)

// BootContentPath is where the boot-device copy of the content file
// lives — listed first so `snapraid status` polling always reads a disk
// that is already awake (Q13, Q18).
const BootContentPath = "/var/lib/hoserva/snapraid.content"

// DefaultExcludes are the exclude patterns doc 02 §2 always sets,
// independent of any share's own exclusion.
var DefaultExcludes = []string{
	"/lost+found/",
	"/.Trash-*/",
	"/appdata/",
	"*.unrecoverable",
	"/snapraid.content*",
	"*.hoserva-moving-*",
	".DS_Store",
}

var (
	// ErrNoParityDisks and ErrTooManyParityDisks enforce Q19: 1 or 2
	// parity disks in v1, never zero and never three or more.
	ErrNoParityDisks      = errors.New("parity: at least one parity disk is required (Q19)")
	ErrTooManyParityDisks = errors.New("parity: at most two parity disks are supported in v1 (Q19)")
	// ErrNoDataDisks is refused because a parity-only pool has nothing
	// to protect.
	ErrNoDataDisks = errors.New("parity: at least one data disk is required")
	// ErrContentPlacement is Q18's own refusal: losing every content
	// file makes parity unrecoverable, so a layout that cannot place
	// enough copies on enough distinct devices is never generated.
	ErrContentPlacement = errors.New("parity: cannot place enough content file copies on distinct physical devices (Q18)")
	// ErrEmptyMount and ErrDuplicateMount guard Q18's and Q19's distinct-
	// device guarantees at the source: an empty or repeated mount path
	// would let two roles (or two parity slots) silently alias the same
	// disk, defeating the "N distinct physical devices" safety property
	// before it's ever checked.
	ErrEmptyMount     = errors.New("parity: a mount path is empty")
	ErrDuplicateMount = errors.New("parity: the same mount path is assigned more than one role")
)

// Layout is the set of mount points array setup assigns before
// generating snapraid.conf (doc 02 §2, doc 03 §3.1). DataMounts is
// ordered by content-placement preference — most free space first
// (Q18); at setup time every disk is either empty or newly adopted, so
// the caller orders this by disk size descending, free space's own
// proxy at this point.
type Layout struct {
	ParityMounts []string `json:"parity_mounts"`
	DataMounts   []string `json:"data_mounts"`
	CacheMount   string   `json:"cache_mount,omitempty"`
	Excludes     []string `json:"excludes,omitempty"`
}

// Validate checks Q19's parity-count rule, that there is at least one data
// disk, and that every assigned mount path is non-empty and appears at
// most once across ParityMounts, DataMounts and CacheMount combined —
// paths are compared after filepath.Clean, so equivalent spellings of the
// same path still collide. It does not check disk sizes or filesystems —
// those are disk.TopologyPlan's own rules (Q20, Q23), checked against real
// disk data Layout doesn't carry.
func (l Layout) Validate() error {
	switch {
	case len(l.ParityMounts) == 0:
		return ErrNoParityDisks
	case len(l.ParityMounts) > 2:
		return ErrTooManyParityDisks
	case len(l.DataMounts) == 0:
		return ErrNoDataDisks
	}

	seen := make(map[string]string, len(l.ParityMounts)+len(l.DataMounts)+1)
	assign := func(role, path string) error {
		if path == "" {
			return fmt.Errorf("%w: %s", ErrEmptyMount, role)
		}
		clean := filepath.Clean(path)
		if other, ok := seen[clean]; ok {
			return fmt.Errorf("%w: %s and %s both target %s", ErrDuplicateMount, other, role, clean)
		}
		seen[clean] = role
		return nil
	}

	for i, m := range l.ParityMounts {
		if err := assign(fmt.Sprintf("parity mount %d", i+1), m); err != nil {
			return err
		}
	}
	for i, m := range l.DataMounts {
		if err := assign(fmt.Sprintf("data mount %d", i+1), m); err != nil {
			return err
		}
	}
	if l.CacheMount != "" {
		if err := assign("cache mount", l.CacheMount); err != nil {
			return err
		}
	}
	return nil
}

// ContentPaths computes Q18's content-file placement: the boot device
// first, cache if present, then data disks in DataMounts order, until
// the count reaches parity-disks+2. Boot, cache and every data mount are
// always distinct physical devices in Hoserva's own layout (doc 01 §6),
// so the number of paths chosen is also the number of distinct devices
// used — and because Q19 limits parity to 1 or 2 disks, that minimum is
// always 3 or 4, so Q18's "at least three distinct physical devices"
// half is already satisfied whenever the copy-count half is.
func (l Layout) ContentPaths() ([]string, error) {
	if err := l.Validate(); err != nil {
		return nil, err
	}

	min := len(l.ParityMounts) + 2
	paths := []string{BootContentPath}

	if l.CacheMount != "" {
		paths = append(paths, filepath.Join(l.CacheMount, contentFileName))
	}

	for i := 0; i < len(l.DataMounts) && len(paths) < min; i++ {
		paths = append(paths, filepath.Join(l.DataMounts[i], contentFileName))
	}

	if len(paths) < min {
		return nil, fmt.Errorf("%w: only %d of %d required copies placeable across the assigned disks", ErrContentPlacement, len(paths), min)
	}

	seen := make(map[string]bool, len(paths))
	for _, p := range paths {
		if seen[p] {
			return nil, fmt.Errorf("%w: content path %s placed more than once", ErrDuplicateMount, p)
		}
		seen[p] = true
	}
	return paths, nil
}

// Render renders l as snapraid.conf text (D1: Hoserva only generates
// SnapRAID's config, never its logic). Directive order follows
// snapraid's own convention: parity (then 2-parity for a second parity
// disk, Q19), content, data, exclude.
//
// This is a self-contained renderer rather than a caller of
// internal/config's RenderSnapraidConf (#25): that function's
// SnapraidState carries a single Parity string field, so it cannot
// express the second "2-parity" directive Q19's dual-parity support
// needs (confirmed against a real sync in spike S5,
// spikes/s5/results/snapraid.conf). Extending SnapraidState to carry
// every parity mount is a follow-up for whoever next touches
// internal/config.
func (l Layout) Render() (string, error) {
	if err := l.Validate(); err != nil {
		return "", err
	}
	contentPaths, err := l.ContentPaths()
	if err != nil {
		return "", err
	}

	var b strings.Builder
	for i, mount := range l.ParityMounts {
		fmt.Fprintf(&b, "%s %s\n", parityDirective(i), filepath.Join(mount, parityFileName))
	}
	for _, c := range contentPaths {
		fmt.Fprintf(&b, "content %s\n", c)
	}
	for i, mount := range l.DataMounts {
		fmt.Fprintf(&b, "data d%d %s\n", i+1, ensureTrailingSlash(mount))
	}

	excludes := l.Excludes
	if excludes == nil {
		excludes = DefaultExcludes
	}
	for _, e := range excludes {
		fmt.Fprintf(&b, "exclude %s\n", e)
	}
	return b.String(), nil
}

// ParityFilePath returns the parity file path Render itself computes for
// a disk mounted at mount — the same join this package's own directive
// rendering uses, exported so a caller building a ParityUpgradeSpec (#289)
// can name the old and new parity files without duplicating parityFileName.
func ParityFilePath(mount string) string {
	return filepath.Join(mount, parityFileName)
}

// parityDirective names the directive for the i'th parity disk (0-based):
// "parity" for the first, "2-parity" for the second — SnapRAID's own
// naming, confirmed against a real sync in spike S5 (doc 08 §5,
// spikes/s5/results/snapraid.conf).
func parityDirective(i int) string {
	if i == 0 {
		return "parity"
	}
	return fmt.Sprintf("%d-parity", i+1)
}

func ensureTrailingSlash(path string) string {
	if strings.HasSuffix(path, "/") {
		return path
	}
	return path + "/"
}
