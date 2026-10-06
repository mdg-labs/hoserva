// Package beneath resolves paths below a trusted directory without ever
// following a symbolic link, for code that runs as root over a tree whose
// users can rearrange it while it works (#626). Every component is opened
// with openat(2) and O_NOFOLLOW from the directory above it, and callers
// change owner and mode through the descriptor they got, so a component
// swapped for a symlink — at any depth, at any moment — is an error, never
// a redirect to a file outside the tree.
package beneath

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

// ErrSymlink is returned for a path component that is a symbolic link where
// this package does not follow one.
var ErrSymlink = errors.New("is a symbolic link")

// OpenRoot opens dir, the trusted directory everything else is resolved
// beneath, and returns its descriptor. dir itself is resolved the ordinary
// way: it is configuration (a data disk's mount point), not user content.
func OpenRoot(dir string) (int, error) {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open %s: %w", dir, err)
	}
	return fd, nil
}

// Components splits a slash-separated path relative to a root into its
// components, refusing an empty path, an absolute one, an empty component
// and "." or "..", none of which a path beneath a root has.
func Components(rel string) ([]string, error) {
	comps := strings.Split(rel, "/")
	for _, c := range comps {
		if c == "" || c == "." || c == ".." {
			return nil, fmt.Errorf("%q is not a path beneath a directory", rel)
		}
	}
	return comps, nil
}

// Open opens the single path component name in the directory dirfd, with
// O_NOFOLLOW, and returns ErrSymlink when name is a symbolic link. flags
// are the caller's (O_RDONLY, O_DIRECTORY, O_NONBLOCK …); O_NOFOLLOW and
// O_CLOEXEC are added. The caller closes the descriptor.
func Open(dirfd int, name string, flags int) (int, error) {
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		return -1, fmt.Errorf("%q is not a single path component", name)
	}
	fd, err := unix.Openat(dirfd, name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err == nil {
		return fd, nil
	}
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
		var st unix.Stat_t
		if unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK {
			return -1, fmt.Errorf("%s: %w", name, ErrSymlink)
		}
	}
	return -1, fmt.Errorf("open %s: %w", name, err)
}

// Walker opens directories beneath one root and keeps the chain of
// descriptors it last opened, so consecutive paths in the same directory
// cost no further opens. The kept descriptors name the directories that
// were opened, so a path swapped afterwards cannot redirect later work in
// them. A Walker is for one goroutine.
type Walker struct {
	root  int
	names []string
	fds   []int
}

// NewWalker opens dir as a Walker's root.
func NewWalker(dir string) (*Walker, error) {
	fd, err := OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Walker{root: fd}, nil
}

// Close releases every descriptor the Walker holds.
func (w *Walker) Close() {
	w.truncate(0)
	_ = unix.Close(w.root)
}

// Root is the descriptor of the Walker's root directory.
func (w *Walker) Root() int { return w.root }

func (w *Walker) truncate(n int) {
	for i := len(w.fds) - 1; i >= n; i-- {
		_ = unix.Close(w.fds[i])
	}
	w.fds = w.fds[:n]
	w.names = w.names[:n]
}

func (w *Walker) top() int {
	if len(w.fds) == 0 {
		return w.root
	}
	return w.fds[len(w.fds)-1]
}

// Dir returns the descriptor of the directory rel beneath the root ("" is
// the root itself), valid until the next call or Close. Each directory
// that had to be opened on the way, from the first one not already held,
// is passed to opened with its parent's descriptor, its own and its path
// beneath the root; an error from opened stops the walk and drops that
// directory, so the next walk to it opens and reports it again. A missing
// component is an error wrapping unix.ENOENT and a symbolic link one
// wrapping ErrSymlink.
func (w *Walker) Dir(rel string, opened func(parent, fd int, rel string) error) (int, error) {
	var comps []string
	if rel != "" {
		var err error
		if comps, err = Components(rel); err != nil {
			return -1, err
		}
	}
	keep := 0
	for keep < len(comps) && keep < len(w.names) && comps[keep] == w.names[keep] {
		keep++
	}
	w.truncate(keep)
	for i := keep; i < len(comps); i++ {
		parent := w.top()
		fd, err := Open(parent, comps[i], unix.O_RDONLY|unix.O_DIRECTORY)
		if err != nil {
			return -1, err
		}
		w.names = append(w.names, comps[i])
		w.fds = append(w.fds, fd)
		if opened != nil {
			if err := opened(parent, fd, strings.Join(comps[:i+1], "/")); err != nil {
				w.truncate(len(w.fds) - 1)
				return -1, err
			}
		}
	}
	return w.top(), nil
}
