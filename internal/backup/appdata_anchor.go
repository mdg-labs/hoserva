package backup

import (
	"errors"
	"fmt"
	"io"

	"golang.org/x/sys/unix"

	"github.com/mdg-labs/hoserva/internal/beneath"
)

// appdataAnchorAttr is the extended attribute that records, on an appdata
// directory, the path it belongs at (doc 10 §2). The trusted namespace is
// readable and writable only with CAP_SYS_ADMIN, and a rename keeps it with
// the directory.
const appdataAnchorAttr = "trusted.hoserva.appdata"

// DirAttrs reads and creates extended attributes of a directory through an
// open descriptor. Get returns unix.ENODATA for an attribute that is not set;
// Create returns unix.EEXIST for one that is, and never replaces it. A test
// supplies a fake, since the trusted namespace needs privilege.
type DirAttrs interface {
	Get(fd int, name string) ([]byte, error)
	Create(fd int, name string, value []byte) error
}

type systemDirAttrs struct{}

func (systemDirAttrs) Get(fd int, name string) ([]byte, error) {
	buf := make([]byte, unix.PathMax+1)
	n, err := unix.Fgetxattr(fd, name, buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func (systemDirAttrs) Create(fd int, name string, value []byte) error {
	return unix.Fsetxattr(fd, name, value, unix.XATTR_CREATE)
}

func (a *AppdataService) dirAttrs() DirAttrs {
	if a.Attrs != nil {
		return a.Attrs
	}
	return systemDirAttrs{}
}

// readAnchor reads the anchor of the directory fd. A directory without one, or
// on a filesystem that cannot hold one, has none; any other failure is an
// error, never read as "none".
func readAnchor(attrs DirAttrs, fd int) (value string, present bool, err error) {
	raw, err := attrs.Get(fd, appdataAnchorAttr)
	switch {
	case err == nil:
		return string(raw), true, nil
	case errors.Is(err, unix.ENODATA), errors.Is(err, unix.ENOTSUP):
		return "", false, nil
	default:
		return "", false, err
	}
}

// setAnchor anchors the directory fd to value unless it has an anchor, which
// it never changes. It returns nil when the directory is anchored to value
// afterwards.
func setAnchor(attrs DirAttrs, fd int, value string) error {
	err := attrs.Create(fd, appdataAnchorAttr, []byte(value))
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EEXIST):
		got, present, rerr := readAnchor(attrs, fd)
		switch {
		case rerr != nil:
			return fmt.Errorf("reading %s: %w", appdataAnchorAttr, rerr)
		case !present:
			return fmt.Errorf("%s was removed while it was being read", appdataAnchorAttr)
		case got != value:
			return fmt.Errorf("already anchored to %q, left unchanged", got)
		}
		return nil
	default:
		return fmt.Errorf("setting %s: %w", appdataAnchorAttr, err)
	}
}

// anchorReport writes a line for each directory that could not be anchored. A
// filesystem or process that cannot hold the attribute at all is reported once,
// not once per directory.
type anchorReport struct {
	out        io.Writer
	cannotHold bool
}

func (r *anchorReport) note(dir string, err error) {
	switch {
	case err == nil:
	case errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EPERM):
		if !r.cannotHold {
			r.cannotHold = true
			_, _ = fmt.Fprintf(r.out, "warning: appdata directories cannot be anchored here (%s: %v), so a restore cannot check which directory it replaces\n", dir, err)
		}
	default:
		_, _ = fmt.Fprintf(r.out, "warning: %s is not anchored: %v\n", dir, err)
	}
}

// anchorDirs anchors each of dirs to its own path, reaching it through its
// parent's descriptor with no link followed. A failure is reported and the
// backup goes on: the anchor guards a later restore and the backup is the data.
func (a *AppdataService) anchorDirs(rep *anchorReport, dirs []string) {
	attrs := a.dirAttrs()
	held := &heldDirs{}
	defer held.close()
	for _, d := range dirs {
		rep.note(d, anchorDir(attrs, held, d))
	}
}

func anchorDir(attrs DirAttrs, held *heldDirs, dir string) error {
	parent, name, err := held.parent(dir)
	if err != nil {
		return err
	}
	fd, err := beneath.Open(parent, name, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return fmt.Errorf("opening %s: %w", dir, err)
	}
	defer func() { _ = unix.Close(fd) }()
	return setAnchor(attrs, fd, dir)
}

// requireAnchored refuses unless each recorded directory of dirs is not
// anchored to another path. The anchor is read through a descriptor opened
// relative to the directory's held parent and checked against the recorded
// device and inode, so it is the recorded directory's. A directory without an
// anchor is accepted.
func (p *heldDirs) requireAnchored(attrs DirAttrs, dirs []string, recorded []liveIdentity) error {
	for i, d := range dirs {
		if !recorded[i].present {
			continue
		}
		if err := p.requireAnchoredDir(attrs, d, recorded[i]); err != nil {
			return err
		}
	}
	return nil
}

func (p *heldDirs) requireAnchoredDir(attrs DirAttrs, dir string, want liveIdentity) error {
	parent, name, err := p.parent(dir)
	if err != nil {
		return err
	}
	fd, err := beneath.Open(parent, name, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return fmt.Errorf("opening %s to read its anchor: %w", dir, err)
	}
	defer func() { _ = unix.Close(fd) }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("reading %s: %w", dir, err)
	}
	if identityOf(&st) != want {
		return fmt.Errorf("%s is not the directory the restore recorded", dir)
	}
	got, present, err := readAnchor(attrs, fd)
	if err != nil {
		return fmt.Errorf("reading the anchor of %s: %w", dir, err)
	}
	if present && got != dir {
		return fmt.Errorf("%q is anchored to %q, so it is not the directory this restore replaces", dir, got)
	}
	return nil
}

// anchorTree anchors the tree the restore unpacked for s to the path it will
// be swapped in at. The tree is opened relative to its work directory and must
// be the one extract recorded; failing that is an error. Failing to set the
// anchor is returned in note, as the backup reports it, and the restore goes on.
func (p *heldDirs) anchorTree(attrs DirAttrs, s *appdataSwap) (note, err error) {
	parent, name, err := p.parent(s.fresh)
	if err != nil {
		return nil, err
	}
	fd, err := beneath.Open(parent, name, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", s.fresh, err)
	}
	defer func() { _ = unix.Close(fd) }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, fmt.Errorf("reading %s: %w", s.fresh, err)
	}
	if !s.tree.present || identityOf(&st) != s.tree {
		return nil, fmt.Errorf("%s is not the tree the restore unpacked", s.fresh)
	}
	return setAnchor(attrs, fd, s.live), nil
}
