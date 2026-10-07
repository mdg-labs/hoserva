package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/mdg-labs/hoserva/internal/beneath"
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

// beforeAppdataRootOpen, when set, runs after an appdata location is
// resolved and before its descriptor is opened, so a test can rearrange the
// tree at that moment.
var beforeAppdataRootOpen func(root string)

// beforeAppdataRemove, when set, runs just before each planned directory is
// removed, so a test can rearrange the tree at that moment.
var beforeAppdataRemove func(path string)

// removeAppdataDirs removes each planned directory tree and returns the
// ones it removed. Each root is resolved once and then opened once by
// walking every component of the resolved path from "/" without following a
// symbolic link, and every plan path is removed relative to the root that
// holds it the same way, so a component swapped for a link after planning,
// the last one of the root included, is refused rather than followed. A plan
// path that no longer exists is skipped.
func removeAppdataDirs(ctx context.Context, roots, plan []string) ([]string, error) {
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
	rootFDs := map[string]int{}
	defer func() {
		for _, fd := range rootFDs {
			_ = unix.Close(fd)
		}
	}()

	var deleted []string
	for _, p := range plan {
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
		root := ""
		for _, r := range resolvedRoots {
			if within(r, p) {
				root = r
				break
			}
		}
		if root == "" {
			return deleted, fmt.Errorf("%s is not inside an appdata location; not deleting it", p)
		}
		fd, ok := rootFDs[root]
		if !ok {
			if beforeAppdataRootOpen != nil {
				beforeAppdataRootOpen(root)
			}
			var err error
			if fd, err = beneath.OpenResolvedDir(root); err != nil {
				return deleted, fmt.Errorf("opening appdata location %s: %w", root, err)
			}
			rootFDs[root] = fd
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return deleted, fmt.Errorf("deleting %s: %w", p, err)
		}
		if beforeAppdataRemove != nil {
			beforeAppdataRemove(p)
		}
		if err := beneath.RemoveAll(fd, filepath.ToSlash(rel)); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return deleted, fmt.Errorf("deleting %s: %w", p, err)
		}
		deleted = append(deleted, p)
	}
	return deleted, nil
}
