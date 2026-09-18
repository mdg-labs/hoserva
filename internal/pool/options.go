package pool

import "fmt"

// Responsiveness is doc 02 §1's single plain-language setting standing
// in for mergerfs's cache.entry/cache.attr/cache.negative_entry kernel
// dentry/attribute TTLs (doc 08 §1: "Responsiveness vs. quiet disks").
// A longer TTL means fewer passthrough lookups and fewer wakeups, at
// the cost of staleness when something changes out of band.
type Responsiveness string

const (
	// Responsive is the default: a 1-second TTL, confirmed accepted
	// and reported by mergerfs 2.40.2 (S8, doc 08 §8).
	Responsive Responsiveness = "responsive"
	// Quiet is doc 08 §1's "Quiet mode" preset: a 600-second (10
	// minute) TTL, recommended by S1 (doc 08 §1) for the culprits its
	// own run didn't test (directory browsing, an indexer, `updatedb`,
	// multiple clients) rather than confirmed necessary by that run.
	Quiet Responsiveness = "quiet"
)

// entrySeconds is the integer-seconds value mergerfs's cache.entry,
// cache.attr and cache.negative_entry options each take — confirmed by
// S8 (doc 08 §8) to round-trip through the mount's own runtime control
// file unchanged.
func (r Responsiveness) entrySeconds() int {
	if r == Quiet {
		return 600
	}
	return 1
}

// Options are the mergerfs mount options doc 02 §1's table sets that
// are shared across the whole pool rather than per share — category.create
// (CreatePolicy) and fsname/branches are per mount, computed by the
// constructors in topology.go.
type Options struct {
	// MinFreeSpace is mergerfs's own minfreespace value, in its size-suffix
	// syntax (e.g. "50G") — doc 02 §1's default, kept configurable.
	MinFreeSpace   string
	Responsiveness Responsiveness
}

// DefaultOptions is doc 02 §1's table: minfreespace defaults to 50G,
// Responsiveness defaults to Responsive.
func DefaultOptions() Options {
	return Options{MinFreeSpace: "50G", Responsiveness: Responsive}
}

// render returns o's own comma-separated mergerfs options, in doc 02
// §1's table order, minus category.create and fsname (added by the
// caller, which knows the per-mount policy and name).
func (o Options) render() string {
	return fmt.Sprintf(
		"moveonenospc=true,dropcacheonclose=true,minfreespace=%s,cache.files=partial,cache.entry=%d,cache.attr=%d,cache.negative_entry=%d,cache.statfs=0",
		o.MinFreeSpace, o.Responsiveness.entrySeconds(), o.Responsiveness.entrySeconds(), o.Responsiveness.entrySeconds(),
	)
}
