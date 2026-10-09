//go:build !linux

package share

import (
	"os"
	"path/filepath"
)

// listConfined is the non-Linux fallback: hoservad's supported deployment is
// Debian (D2), so this keeps only a path-based listing for local development
// on other platforms — it does not close the symlink-swap race listConfined's
// Linux implementation closes, and reports no holding disk.
func listConfined(root, rel string) ([]BrowseEntry, error) {
	entries, err := os.ReadDir(filepath.Join(root, rel))
	if err != nil {
		return nil, err
	}
	out := make([]BrowseEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		item := BrowseEntry{Name: e.Name(), Directory: e.IsDir()}
		if !item.Directory {
			item.SizeBytes = info.Size()
		}
		out = append(out, item)
	}
	return out, nil
}
