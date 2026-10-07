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

// latest returns the channel's head release when it is a well-formed
// entry strictly newer than current. It returns nil when there is
// nothing newer, or when current is not a version that can be compared:
// an unreadable running version never offers an install.
func (idx *Index) latest(channel Channel, current string) (*Release, error) {
	releases := idx.Channels[channel]
	if len(releases) == 0 {
		return nil, nil
	}
	head := releases[0]
	if err := validateEntry(head); err != nil {
		return nil, err
	}
	if channel == ChannelStable && head.Channel != ChannelStable {
		return nil, fmt.Errorf("%w: %s is not a stable release", ErrReleaseMismatch, head.Version)
	}
	cmp, err := compareDebianVersions(head.Version, current)
	if err != nil || cmp <= 0 {
		return nil, nil
	}
	return &head, nil
}

// findVersion returns the entry for exactly the given version.
func (idx *Index) findVersion(version string) (*Release, error) {
	for _, list := range idx.Channels {
		for _, r := range list {
			if r.Version == version {
				if err := validateEntry(r); err != nil {
					return nil, err
				}
				cp := r
				return &cp, nil
			}
		}
	}
	return nil, nil
}

// validateEntry requires an index entry's tag and channel to be the ones
// its version implies. The entry is only a pointer: the version that is
// installed is the one the signed SHA256SUMS names (downloadAndVerify).
func validateEntry(r Release) error {
	if !isReleaseVersion(r.Version) {
		return fmt.Errorf("%w: %q is not a release version", ErrReleaseMismatch, r.Version)
	}
	if r.Tag != tagForVersion(r.Version) {
		return fmt.Errorf("%w: tag %q does not belong to version %s", ErrReleaseMismatch, r.Tag, r.Version)
	}
	if r.Channel != channelForVersion(r.Version) {
		return fmt.Errorf("%w: version %s is not on channel %q", ErrReleaseMismatch, r.Version, r.Channel)
	}
	return nil
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
