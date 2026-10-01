package container

import (
	"path/filepath"
	"strings"

	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"
)

// Kinds of storage a mount's host path can lie on.
const (
	MountPool    = "pool"
	MountDisk    = "disk"
	MountCache   = "cache"
	MountOutside = "outside"
)

// MountLocation is where a mount's host path lies. Share is the share's name
// for a pool path below the pool's root, and Disk the data disk's number for
// a disk path.
type MountLocation struct {
	Kind  string
	Share string
	Disk  int
}

// ClassifyMount decides from the path alone, against the pool's mount point
// and the mount points of disks, which storage source lies on. It never
// resolves a symlink or touches the path, so classifying a pooled path wakes
// no data disk (doc 02 §1, Q13): a path through a symlink that leads
// elsewhere is classified by where it is written, not where it leads.
// Anything else, including the boot device and a parity disk, is
// MountOutside.
func ClassifyMount(source string, disks []store.ArrayDisk) MountLocation {
	p := filepath.Clean(source)
	if rest, ok := below(pool.CatchAllPath, p); ok {
		share, _, _ := strings.Cut(rest, "/")
		return MountLocation{Kind: MountPool, Share: share}
	}
	for _, d := range disks {
		if d.Mountpoint == "" {
			continue
		}
		if _, ok := below(filepath.Clean(d.Mountpoint), p); !ok {
			continue
		}
		switch d.Role {
		case store.ArrayRoleData:
			return MountLocation{Kind: MountDisk, Disk: d.RoleIndex}
		case store.ArrayRoleCache:
			return MountLocation{Kind: MountCache}
		}
	}
	return MountLocation{Kind: MountOutside}
}

// below reports whether path is root or lies inside it, and the part of path
// after root ("" for root itself).
func below(root, path string) (string, bool) {
	if path == root {
		return "", true
	}
	if within(root, path) {
		return strings.TrimPrefix(path, root+"/"), true
	}
	return "", false
}
