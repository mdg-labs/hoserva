package update

import (
	"encoding/json"
	"fmt"
	"strings"
)

func parseIndex(body []byte) (*Index, error) {
	var idx Index
	if err := json.Unmarshal(body, &idx); err != nil {
		return nil, fmt.Errorf("update: parsing release index: %w", err)
	}
	if idx.Channels == nil {
		return nil, fmt.Errorf("update: release index has no channels")
	}
	return &idx, nil
}

func (idx *Index) latest(channel Channel, current string) *Release {
	releases := idx.Channels[channel]
	if len(releases) == 0 {
		return nil
	}
	head := releases[0]
	if current != "" && versionsEqual(head.Version, current) {
		return nil
	}
	cp := head
	return &cp
}

func (idx *Index) findVersion(version string) *Release {
	for _, list := range idx.Channels {
		for _, r := range list {
			if versionsEqual(r.Version, version) || r.Tag == version {
				cp := r
				return &cp
			}
		}
	}
	return nil
}

func versionsEqual(a, b string) bool {
	return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v")
}

func assetForArch(r *Release, arch string) (Asset, error) {
	if r == nil {
		return Asset{}, ErrNotAvailable
	}
	a, ok := r.Assets[arch]
	if !ok || a.URL == "" || a.SHA256 == "" {
		return Asset{}, fmt.Errorf("update: release %s has no %s asset", r.Version, arch)
	}
	return a, nil
}

// sumsURLs derives SHA256SUMS and SHA256SUMS.sig from a .deb asset URL
// (same GitHub Releases directory, never api.github.com).
func sumsURLs(debURL string) (sumsURL, sigURL string, err error) {
	i := strings.LastIndex(debURL, "/")
	if i < 0 {
		return "", "", fmt.Errorf("update: asset URL %q has no filename", debURL)
	}
	base := debURL[:i]
	return base + "/SHA256SUMS", base + "/SHA256SUMS.sig", nil
}
