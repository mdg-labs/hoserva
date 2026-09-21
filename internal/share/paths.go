package share

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mdg-labs/hoserva/internal/pool"
)

const maxShareNameLen = 64

var (
	// ErrInvalidName is a share name pool.ValidateShareName refuses, or
	// one longer than the API's 64-character cap.
	ErrInvalidName = errors.New("share: invalid name")
	// ErrPathEscapes is a browse or delete path that leaves the share.
	ErrPathEscapes = errors.New("share: path escapes the share")
	// ErrWrongSharePath is delete-data asked to remove a path that is
	// not one of this share's own branch directories.
	ErrWrongSharePath = errors.New("share: path is not this share's data")
	// ErrFileNotFound is a browse-delete path that does not exist.
	ErrFileNotFound = errors.New("share: no such file or directory")
)

// shareDataRoots are the directories delete-data may remove and create
// must mkdir: this share's directory on each branch its cache mode
// uses, and nothing else — not other shares, not parity, not disks that
// do not hold this share.
func shareDataRoots(name string, mode pool.CacheMode, dataDisks []string, cachePath string) ([]string, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	var roots []string
	switch mode {
	case pool.CacheOnly:
		if cachePath == "" {
			return nil, fmt.Errorf("%w: cache-only share %q has no cache path", ErrInvalidInput, name)
		}
		roots = append(roots, filepath.Join(cachePath, name))
	case pool.CacheThenMove:
		if cachePath == "" {
			return nil, fmt.Errorf("%w: cache-then-move share %q has no cache path", ErrInvalidInput, name)
		}
		roots = append(roots, filepath.Join(cachePath, name))
		for _, d := range dataDisks {
			roots = append(roots, filepath.Join(d, name))
		}
	case pool.ArrayOnly:
		if len(dataDisks) == 0 {
			return nil, fmt.Errorf("%w: array-only share %q has no data disks", ErrInvalidInput, name)
		}
		for _, d := range dataDisks {
			roots = append(roots, filepath.Join(d, name))
		}
	default:
		return nil, fmt.Errorf("%w: unknown cache mode %q", ErrInvalidInput, mode)
	}
	return roots, nil
}

// confineSharePath joins rel onto root and refuses anything that is not
// root itself or a child of it — including after Clean of `..`.
func confineSharePath(root, rel string) (string, error) {
	root = filepath.Clean(root)
	if rel == "" || rel == "." {
		return root, nil
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: %q", ErrPathEscapes, rel)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q", ErrPathEscapes, rel)
	}
	joined := filepath.Join(root, clean)
	relToRoot, err := filepath.Rel(root, joined)
	if err != nil || !pathInside(relToRoot) {
		return "", fmt.Errorf("%w: %q", ErrPathEscapes, rel)
	}
	return joined, nil
}

// confineSharePathOnFS is confineSharePath plus a symlink-aware check:
// the resolved target must stay under the resolved share root. A
// dangling symlink is refused the same way as an escape — ReadDir
// would otherwise follow it.
func confineSharePathOnFS(fs FS, root, rel string) (string, error) {
	joined, err := confineSharePath(root, rel)
	if err != nil {
		return "", err
	}
	resolvedRoot, err := fs.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("share: resolving share root %s: %w", root, err)
	}
	resolved, err := fs.EvalSymlinks(joined)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("share: resolving %s: %w", joined, err)
		}
		info, lerr := fs.Lstat(joined)
		if lerr != nil {
			if os.IsNotExist(lerr) {
				return joined, nil
			}
			return "", lerr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%w: %q", ErrPathEscapes, rel)
		}
		return joined, nil
	}
	relToRoot, err := filepath.Rel(resolvedRoot, resolved)
	if err != nil || !pathInside(relToRoot) {
		return "", fmt.Errorf("%w: %q", ErrPathEscapes, rel)
	}
	return resolved, nil
}

func pathInside(relToRoot string) bool {
	return relToRoot != ".." && !strings.HasPrefix(relToRoot, ".."+string(filepath.Separator))
}

// allowedSharePath reports whether path is exactly one of the allowed
// share branch directories (after Clean). A `..` that would land on
// another share or a parity file is not allowed.
func allowedSharePath(path string, allowed []string) bool {
	clean := filepath.Clean(path)
	for _, a := range allowed {
		if clean == filepath.Clean(a) {
			return true
		}
	}
	return false
}

func validateName(name string) error {
	if err := pool.ValidateShareName(name); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidName, err)
	}
	if len(name) > maxShareNameLen {
		return fmt.Errorf("%w: %q is longer than %d characters", ErrInvalidName, name, maxShareNameLen)
	}
	return nil
}
