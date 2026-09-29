package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mdg-labs/hoserva/internal/store"
)

// CacheAppdataRoots is where Remove may delete a container's appdata for
// an array with these disks: the mounted cache disk's appdata directory,
// where templates put it by default (doc 04's `APPDATA` input defaults to
// /mnt/cache/appdata). An array with no mounted cache disk has none.
func CacheAppdataRoots(disks []store.ArrayDisk) []string {
	for _, d := range disks {
		if d.Role == store.ArrayRoleCache && d.Mountpoint != "" {
			return []string{filepath.Join(d.Mountpoint, "appdata")}
		}
	}
	return nil
}

// ErrAppdataUnavailable is returned by Remove when the caller asked for
// the container's appdata to be deleted but no appdata location is known
// to delete under. Nothing is removed.
var ErrAppdataUnavailable = errors.New("container: no appdata location is configured, so appdata cannot be deleted")

// ErrAppdataShared is returned by Remove when a directory it would delete
// is also, contains, or lies inside a mount of another container's own
// appdata. Nothing is removed.
var ErrAppdataShared = errors.New("container: appdata is shared with another container")

// within reports whether path is strictly inside dir (never dir itself).
func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == "." {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// holds reports whether path is dir or lies inside it.
func holds(dir, path string) bool {
	return dir == path || within(dir, path)
}

// planAppdataDeletion returns the host directories Remove may delete for
// c: each bind-mount source that resolves, through symlinks, to a place
// strictly inside one of roots. A mount that is missing, outside every
// root, or equal to a root itself is never part of the plan, so a media
// or system bind mount is never touched and a container mounting a whole
// appdata root deletes nothing. It fails, planning nothing, if any
// candidate is, or contains, a mount of another container, or lies inside
// another container's own appdata directory (a mount strictly inside a
// root): that container would lose its data with it. A mount of another
// container that is an appdata root, or contains one, is no conflict.
func planAppdataDeletion(c Container, others []Container, roots []string) ([]string, error) {
	var resolvedRoots []string
	for _, r := range roots {
		real, err := filepath.EvalSymlinks(r)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("resolving appdata location %s: %w", r, err)
		}
		resolvedRoots = append(resolvedRoots, real)
	}

	var plan []string
	for _, m := range c.Mounts {
		if m.Source == "" || !filepath.IsAbs(m.Source) {
			continue
		}
		real, err := filepath.EvalSymlinks(m.Source)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("resolving mount source %s: %w", m.Source, err)
		}
		inside := false
		for _, r := range resolvedRoots {
			if within(r, real) {
				inside = true
				break
			}
		}
		if !inside {
			continue
		}
		plan = append(plan, real)
	}
	plan = dropNested(plan)

	for _, o := range others {
		if o.ID == c.ID {
			continue
		}
		for _, m := range o.Mounts {
			if m.Source == "" || !filepath.IsAbs(m.Source) {
				continue
			}
			other := m.Source
			if real, err := filepath.EvalSymlinks(m.Source); err == nil {
				other = real
			}
			otherIsAppdata := false
			for _, r := range resolvedRoots {
				if within(r, other) {
					otherIsAppdata = true
					break
				}
			}
			for _, p := range plan {
				if holds(p, other) || otherIsAppdata && within(other, p) {
					return nil, fmt.Errorf("%w: %s is used by %s", ErrAppdataShared, p, o.Name)
				}
			}
		}
	}
	return plan, nil
}

// dropNested removes duplicates and any path inside another path of the
// list, so each directory tree is deleted once.
func dropNested(paths []string) []string {
	var out []string
	for i, p := range paths {
		nested := false
		for j, q := range paths {
			if i == j {
				continue
			}
			if p == q && j < i || within(q, p) {
				nested = true
				break
			}
		}
		if !nested {
			out = append(out, p)
		}
	}
	return out
}

// removeAppdataDirs removes each planned directory tree and returns the
// ones it removed. Each path is resolved again first, so a path swapped
// for a symlink after planning is refused rather than followed.
func removeAppdataDirs(ctx context.Context, plan []string) ([]string, error) {
	var deleted []string
	for _, p := range plan {
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return deleted, fmt.Errorf("resolving %s: %w", p, err)
		}
		if real != p {
			return deleted, fmt.Errorf("%s changed after it was planned for deletion; not deleting it", p)
		}
		if err := os.RemoveAll(p); err != nil {
			return deleted, fmt.Errorf("deleting %s: %w", p, err)
		}
		deleted = append(deleted, p)
	}
	return deleted, nil
}
