package container

import "path/filepath"

// PathUse is one container with the host paths of its mounts that use a set
// of roots. Active is true for a container that is running, paused or
// restarting: one that can hold files open under the roots.
type PathUse struct {
	Container Container
	Active    bool
	Sources   []string
}

// UsingPaths returns, in the order given, every container with a mount whose
// host path is one of roots, lies inside one, or holds one (a mount of the
// pool root holds every share). Only the path as written is compared; it is
// never resolved through a symlink, so the lookup reads no data disk
// (doc 02 §1, Q13). A mount with no absolute host path (a named volume) never
// matches.
func UsingPaths(containers []Container, roots []string) []PathUse {
	var cleaned []string
	for _, r := range roots {
		if r != "" {
			cleaned = append(cleaned, filepath.Clean(r))
		}
	}

	var uses []PathUse
	for _, c := range containers {
		var sources []string
		for _, m := range c.Mounts {
			if m.Source == "" || !filepath.IsAbs(m.Source) {
				continue
			}
			src := filepath.Clean(m.Source)
			for _, r := range cleaned {
				if holds(r, src) || within(src, r) {
					sources = append(sources, m.Source)
					break
				}
			}
		}
		if len(sources) == 0 {
			continue
		}
		uses = append(uses, PathUse{Container: c, Active: activeState(c.State), Sources: sources})
	}
	return uses
}

func activeState(state string) bool {
	switch state {
	case "running", "paused", "restarting":
		return true
	}
	return false
}
