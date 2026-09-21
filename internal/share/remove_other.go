//go:build !linux

package share

import (
	"fmt"
	"os"
	"path/filepath"
)

// unlinkConfined is the non-Linux fallback: hoservad's supported
// deployment is Debian (D2), so this keeps only best-effort, path-based
// removal for local development on other platforms — it does not close
// the ancestor-symlink-swap or leaf-swap races unlinkConfined's Linux
// implementation closes via RESOLVE_IN_ROOT|RESOLVE_NO_SYMLINKS plus a
// leaf identity check against expectedLeaf, which this build ignores.
func unlinkConfined(resolvedRoot, dirRel, base string, expectedLeaf os.FileInfo) error {
	path := filepath.Join(resolvedRoot, dirRel, base)
	if _, err := os.Lstat(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: %s", ErrFileNotFound, base)
		}
		return err
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("share: deleting %s: %w", path, err)
	}
	return nil
}
