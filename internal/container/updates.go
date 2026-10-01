package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/distribution/reference"

	"github.com/mdg-labs/hoserva/internal/store"
)

// UpdateNotChecked is the status of a container whose image no check has
// looked at yet, or that a check could not look at (a registry that wants a
// login, a digest-pinned image). It never reads as up to date.
const UpdateNotChecked = store.UpdateNotChecked

// UpdateResults is the image_update_checks table (store.UpdateStore).
type UpdateResults interface {
	PutImageUpdateCheck(ctx context.Context, c store.ImageUpdateCheck) error
	ListImageUpdateChecks(ctx context.Context) ([]store.ImageUpdateCheck, error)
	DeleteImageUpdateCheck(ctx context.Context, image string) error
}

var _ UpdateResults = (*store.UpdateStore)(nil)

// UpdateChecker is the daily container update check (doc 04 §6, Q81): for
// each image a container runs it compares the digest the registry serves
// for the tag with the digest the local image was pulled as, and looks for a
// newer version tag. It asks the registry for manifests and tag names only,
// never a layer.
type UpdateChecker struct {
	Provider Provider
	Registry RegistryClient
	Results  UpdateResults
	// Now reports the time stored with a result; nil means time.Now.
	Now func() time.Time
}

func (c *UpdateChecker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// checkTarget is one image and the containers that run it.
type checkTarget struct {
	ref        ImageRef
	parseErr   error
	image      string
	containers []Container
}

// Run checks every image a container runs and stores one result per image,
// deleting the results of images no container runs any more. A registry
// that answers with a rate limit is skipped for the rest of the run, and
// each of its images is stored as skipped, never as up to date. Nothing is
// stored when the Engine cannot be listed.
func (c *UpdateChecker) Run(ctx context.Context, out io.Writer) error {
	containers, err := c.Provider.List(ctx)
	if err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}
	images, err := c.Provider.Images(ctx)
	if err != nil {
		return fmt.Errorf("listing images: %w", err)
	}
	targets, err := checkTargets(ctx, c.Provider, containers)
	if err != nil {
		return err
	}
	stored, err := c.Results.ListImageUpdateChecks(ctx)
	if err != nil {
		return err
	}
	current := map[string]bool{}
	for _, t := range targets {
		current[t.key()] = true
	}
	for _, s := range stored {
		if !current[s.Image] {
			if err := c.Results.DeleteImageUpdateCheck(ctx, s.Image); err != nil {
				return err
			}
		}
	}

	limited := map[string]bool{}
	for _, t := range targets {
		if err := ctx.Err(); err != nil {
			return err
		}
		res := c.checkTarget(ctx, t, images, limited)
		if err := ctx.Err(); err != nil {
			return err
		}
		res.Image = t.key()
		res.CheckedAt = c.now()
		if err := c.Results.PutImageUpdateCheck(ctx, res); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "%s: %s%s\n", res.Image, res.Status, resultDetail(res))
	}
	_, _ = fmt.Fprintf(out, "checked %d image(s)\n", len(targets))
	return nil
}

func resultDetail(r store.ImageUpdateCheck) string {
	var parts []string
	if r.Kind != "" {
		parts = append(parts, r.Kind)
	}
	if r.AvailableTag != "" {
		parts = append(parts, r.AvailableTag)
	}
	if r.Message != "" {
		parts = append(parts, r.Message)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

func (t checkTarget) key() string {
	if t.parseErr != nil {
		return t.image
	}
	return t.ref.String()
}

// configuredRef is the reference a container was created with: the listing
// reports an image ID instead once the tag points at a different image, which
// would send the check to a registry repository named "sha256". It reports
// false for a container that is pinned to a digest, or that the Engine no
// longer has.
func configuredRef(ctx context.Context, p Provider, ct Container) (repo, tag, raw string, ok bool, err error) {
	if ct.Pinned {
		return "", "", "", false, nil
	}
	img, err := p.ConfiguredImage(ctx, ct.ID)
	if errors.Is(err, ErrNotFound) {
		return "", "", "", false, nil
	}
	if err != nil {
		return "", "", "", false, fmt.Errorf("reading the image reference of container %q: %w", ct.Name, err)
	}
	if img.Pinned {
		return "", "", "", false, nil
	}
	repo, tag = splitImageRef(img.Ref)
	return repo, tag, img.Ref, true, nil
}

func checkTargets(ctx context.Context, p Provider, containers []Container) ([]checkTarget, error) {
	byKey := map[string]*checkTarget{}
	var order []string
	for _, ct := range containers {
		if isRecreateTemp(ct.Name) {
			continue
		}
		repo, tag, raw, ok, err := configuredRef(ctx, p, ct)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		t := checkTarget{image: raw}
		t.ref, t.parseErr = ParseImageRef(repo, tag)
		k := t.key()
		if byKey[k] == nil {
			byKey[k] = &t
			order = append(order, k)
		}
		byKey[k].containers = append(byKey[k].containers, ct)
	}
	sort.Strings(order)
	out := make([]checkTarget, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	return out, nil
}

func isRecreateTemp(name string) bool {
	return strings.HasSuffix(name, newNameSuffix) || strings.HasSuffix(name, oldNameSuffix)
}

const pinnedMessage = "the container is pinned to an image digest, so there is no tag to look for an update of"

const deniedMessage = "the registry wants a login, and Hoserva checks registries anonymously only"

const noLocalDigestMessage = "the image %s has no registry digest recorded locally (built locally?), so it cannot be compared"

const skippedMessage = "the registry is rate limiting requests; skipped until the next daily check"

func (c *UpdateChecker) checkTarget(ctx context.Context, t checkTarget, images []Image, limited map[string]bool) store.ImageUpdateCheck {
	failed := func(format string, args ...any) store.ImageUpdateCheck {
		return store.ImageUpdateCheck{Status: store.UpdateFailed, Message: fmt.Sprintf(format, args...)}
	}
	if t.parseErr != nil {
		return failed("%v", t.parseErr)
	}
	if limited[t.ref.Registry] {
		return store.ImageUpdateCheck{Status: store.UpdateSkipped, Message: skippedMessage}
	}
	remote, err := c.Registry.ManifestDigest(ctx, t.ref)
	if errors.Is(err, ErrRateLimited) {
		limited[t.ref.Registry] = true
		return store.ImageUpdateCheck{Status: store.UpdateSkipped, Message: skippedMessage}
	}
	if errors.Is(err, ErrRegistryDenied) {
		return store.ImageUpdateCheck{Status: store.UpdateNotChecked, Message: deniedMessage}
	}
	if err != nil {
		return failed("reading the manifest: %v", err)
	}

	// A container whose image has no registry digest is left out here, so it
	// cannot mask its siblings' result; Statuses reports it as failed.
	res := store.ImageUpdateCheck{Status: store.UpdateUpToDate}
	compared := false
	for _, ct := range t.containers {
		local := localDigests(ct, t.ref, images)
		if len(local) == 0 {
			continue
		}
		compared = true
		if !local[remote] {
			res = store.ImageUpdateCheck{Status: store.UpdateAvailable, Kind: store.UpdateKindNewBuild}
		}
	}
	if !compared {
		return failed(noLocalDigestMessage, t.ref)
	}

	parsed, versioned := parseVersionTag(t.ref.Tag)
	if !versioned {
		return res
	}
	tags, err := c.Registry.Tags(ctx, t.ref)
	if errors.Is(err, ErrRateLimited) {
		limited[t.ref.Registry] = true
		if res.Status == store.UpdateAvailable {
			res.Message = "the registry is rate limiting requests, so newer version tags were not looked for"
			return res
		}
		return store.ImageUpdateCheck{Status: store.UpdateSkipped, Message: skippedMessage}
	}
	if errors.Is(err, ErrRegistryDenied) {
		if res.Status == store.UpdateAvailable {
			res.Message = "the registry wants a login, so newer version tags were not looked for"
			return res
		}
		return store.ImageUpdateCheck{Status: store.UpdateNotChecked, Message: deniedMessage}
	}
	if err != nil {
		if res.Status == store.UpdateAvailable {
			res.Message = fmt.Sprintf("newer version tags could not be listed: %v", err)
			return res
		}
		return failed("listing tags: %v", err)
	}
	if newer, ok := newestVersionTag(parsed, tags); ok {
		return store.ImageUpdateCheck{Status: store.UpdateAvailable, Kind: store.UpdateKindNewVersion, AvailableTag: newer}
	}
	return res
}

// localDigests is the set of registry manifest digests the container's
// image was pulled as from ref's repository.
func localDigests(ct Container, ref ImageRef, images []Image) map[string]bool {
	out := map[string]bool{}
	for _, img := range images {
		if img.ID != ct.ImageID {
			continue
		}
		for _, rd := range img.RepoDigests {
			name, digest, ok := strings.Cut(rd, "@")
			if !ok {
				continue
			}
			named, err := reference.ParseNormalizedNamed(name)
			if err != nil || reference.Domain(named) != ref.Registry || reference.Path(named) != ref.Repository {
				continue
			}
			out[digest] = true
		}
	}
	return out
}

// ContainerUpdate is one container's update status as the API reports it.
type ContainerUpdate struct {
	Container    string
	Image        string
	Tag          string
	Status       string
	Kind         string
	AvailableTag string
	Message      string
	CheckedAt    time.Time // zero when no check has reached the image
}

// Statuses reports every container's stored update result. A container no
// check has reached, or whose image cannot be parsed, is UpdateNotChecked
// or failed, never up to date.
func (c *UpdateChecker) Statuses(ctx context.Context) ([]ContainerUpdate, error) {
	containers, err := c.Provider.List(ctx)
	if err != nil {
		return nil, err
	}
	images, err := c.Provider.Images(ctx)
	if err != nil {
		return nil, err
	}
	stored, err := c.Results.ListImageUpdateChecks(ctx)
	if err != nil {
		return nil, err
	}
	byImage := make(map[string]store.ImageUpdateCheck, len(stored))
	for _, s := range stored {
		byImage[s.Image] = s
	}
	out := make([]ContainerUpdate, 0, len(containers))
	for _, ct := range containers {
		if isRecreateTemp(ct.Name) {
			continue
		}
		u := ContainerUpdate{Container: ct.Name, Image: ct.Image, Tag: ct.Tag, Status: UpdateNotChecked}
		if ct.Pinned {
			u.Message = pinnedMessage
			out = append(out, u)
			continue
		}
		img, err := c.Provider.ConfiguredImage(ctx, ct.ID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading the image reference of container %q: %w", ct.Name, err)
		}
		repo, tag := splitImageRef(img.Ref)
		u.Image, u.Tag = repo, tag
		ref, err := ParseImageRef(repo, tag)
		if img.Pinned {
			u.Image, u.Tag = ct.Image, ct.Tag
			u.Message = pinnedMessage
		} else if err != nil {
			u.Status, u.Message = store.UpdateFailed, err.Error()
		} else if s, ok := byImage[ref.String()]; ok {
			u.Status, u.Kind, u.AvailableTag, u.Message, u.CheckedAt = s.Status, s.Kind, s.AvailableTag, s.Message, s.CheckedAt
			digestResult := s.Status == store.UpdateUpToDate || s.Kind == store.UpdateKindNewBuild
			if digestResult && len(localDigests(ct, ref, images)) == 0 {
				u.Status, u.Kind, u.Message = store.UpdateFailed, "", fmt.Sprintf(noLocalDigestMessage, ref)
			}
		}
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Container < out[j].Container })
	return out, nil
}

// versionTag is a tag like "v2.1.3" or "10.9-alpine": an optional "v", a
// dotted run of numbers, and a suffix.
type versionTag struct {
	prefix, suffix string
	parts          []uint64
}

var versionTagPattern = regexp.MustCompile(`^(v?)(\d+(?:\.\d+)*)(.*)$`)

func parseVersionTag(tag string) (versionTag, bool) {
	m := versionTagPattern.FindStringSubmatch(tag)
	if m == nil {
		return versionTag{}, false
	}
	var parts []uint64
	for _, p := range strings.Split(m[2], ".") {
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return versionTag{}, false
		}
		parts = append(parts, n)
	}
	return versionTag{prefix: m[1], suffix: m[3], parts: parts}, true
}

// newestVersionTag is the highest tag with the same prefix, suffix and
// number of version parts as current that is newer than it: "10.9.7" has
// "10.10.0" but not "10.9" or "11.0.0-rc1".
func newestVersionTag(current versionTag, tags []string) (string, bool) {
	best, bestTag := current, ""
	for _, tag := range tags {
		v, ok := parseVersionTag(tag)
		if !ok || v.prefix != current.prefix || v.suffix != current.suffix || len(v.parts) != len(current.parts) {
			continue
		}
		if compareParts(v.parts, best.parts) > 0 {
			best, bestTag = v, tag
		}
	}
	return bestTag, bestTag != ""
}

func compareParts(a, b []uint64) int {
	for i := range a {
		switch {
		case a[i] < b[i]:
			return -1
		case a[i] > b[i]:
			return 1
		}
	}
	return 0
}
