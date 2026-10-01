package container

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/store"
)

type memUpdates struct {
	checks map[string]store.ImageUpdateCheck
	puts   int
}

func newMemUpdates() *memUpdates {
	return &memUpdates{checks: map[string]store.ImageUpdateCheck{}}
}

func (m *memUpdates) PutImageUpdateCheck(_ context.Context, c store.ImageUpdateCheck) error {
	m.puts++
	m.checks[c.Image] = c
	return nil
}

func (m *memUpdates) ListImageUpdateChecks(context.Context) ([]store.ImageUpdateCheck, error) {
	var out []store.ImageUpdateCheck
	for _, c := range m.checks {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Image < out[j].Image })
	return out, nil
}

func (m *memUpdates) DeleteImageUpdateCheck(_ context.Context, image string) error {
	delete(m.checks, image)
	return nil
}

const (
	digestOld = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	digestNew = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

type updateRig struct {
	checker  *UpdateChecker
	provider *FakeProvider
	registry *FakeRegistry
	results  *memUpdates
}

func newUpdateRig() *updateRig {
	r := &updateRig{provider: NewFakeProvider(), registry: NewFakeRegistry(), results: newMemUpdates()}
	r.checker = &UpdateChecker{
		Provider: r.provider,
		Registry: r.registry,
		Results:  r.results,
		Now:      func() time.Time { return time.Date(2026, 9, 30, 6, 10, 0, 0, time.UTC) },
	}
	return r
}

// run adds a container of repo:tag whose local image was pulled as localDigest
// and has the registry serve remote for it.
func (r *updateRig) run(t *testing.T, name, repo, tag, localDigest, remoteDigest string) ImageRef {
	t.Helper()
	ref, err := ParseImageRef(repo, tag)
	if err != nil {
		t.Fatal(err)
	}
	id := "sha256:img-" + name
	r.provider.AddContainer(Container{ID: "id-" + name, Name: name, Image: repo, Tag: tag, ImageID: id})
	r.provider.AddImage(Image{ID: id, RepoDigests: []string{repo + "@" + localDigest}})
	if remoteDigest != "" {
		r.registry.SetDigest(ref, remoteDigest)
	}
	return ref
}

func (r *updateRig) check(t *testing.T) string {
	t.Helper()
	var out bytes.Buffer
	if err := r.checker.Run(context.Background(), &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return out.String()
}

func (r *updateRig) result(t *testing.T, image string) store.ImageUpdateCheck {
	t.Helper()
	c, ok := r.results.checks[image]
	if !ok {
		t.Fatalf("no result stored for %s; have %v", image, r.results.checks)
	}
	return c
}

func TestUpdateChecker_NewBuildOnTheSameTagIsNotANewVersion(t *testing.T) {
	r := newUpdateRig()
	r.run(t, "web", "nginx", "latest", digestOld, digestNew)
	r.run(t, "db", "postgres", "16.4", digestOld, digestOld)
	r.registry.SetTags(ImageRef{Registry: "docker.io", Repository: "library/postgres"}, []string{"16.4"})
	r.check(t)

	web := r.result(t, "nginx:latest")
	if web.Status != store.UpdateAvailable || web.Kind != store.UpdateKindNewBuild || web.AvailableTag != "" {
		t.Errorf("nginx:latest = %+v, want update_available/new_build with no tag", web)
	}
	if db := r.result(t, "postgres:16.4"); db.Status != store.UpdateUpToDate {
		t.Errorf("postgres:16.4 = %+v, want up_to_date", db)
	}
	for _, c := range r.registry.Calls() {
		if strings.HasPrefix(c, "tags") && strings.Contains(c, "nginx") {
			t.Errorf("asked for the tags of a non-version tag: %v", r.registry.Calls())
		}
	}
}

func TestUpdateChecker_NewVersionTagIsLabelledDifferently(t *testing.T) {
	r := newUpdateRig()
	ref := r.run(t, "db", "postgres", "16.4", digestOld, digestOld)
	r.registry.SetTags(ref, []string{"16.4", "16.5", "16.10", "17.0", "16.11-alpine", "15.9", "16", "latest", "16.12-rc1"})
	r.check(t)

	got := r.result(t, "postgres:16.4")
	if got.Status != store.UpdateAvailable || got.Kind != store.UpdateKindNewVersion || got.AvailableTag != "17.0" {
		t.Fatalf("postgres:16.4 = %+v, want update_available/new_version 17.0 (numeric order, not lexical; 16.11-alpine, 16 and 16.12-rc1 are other shapes)", got)
	}
}

func TestUpdateChecker_NewVersionWithNewBuildReportsTheVersion(t *testing.T) {
	r := newUpdateRig()
	ref := r.run(t, "s", "ghcr.io/acme/app", "v1.2.3", digestOld, digestNew)
	r.registry.SetTags(ref, []string{"v1.2.3", "v1.3.0", "1.4.0"})
	r.check(t)
	got := r.result(t, "ghcr.io/acme/app:v1.2.3")
	if got.Kind != store.UpdateKindNewVersion || got.AvailableTag != "v1.3.0" {
		t.Fatalf("= %+v, want new_version v1.3.0", got)
	}
}

func TestUpdateChecker_RateLimitedRegistryIsSkippedNeverUpToDate(t *testing.T) {
	r := newUpdateRig()
	r.run(t, "a", "nginx", "latest", digestOld, "")
	r.run(t, "b", "redis", "latest", digestOld, "")
	r.run(t, "c", "ghcr.io/acme/app", "latest", digestOld, digestOld)
	// Stored from an earlier run: a stale up_to_date must not survive a
	// check that could not look.
	r.results.checks["nginx:latest"] = store.ImageUpdateCheck{Image: "nginx:latest", Status: store.UpdateUpToDate}
	r.registry.SetRegistryError("docker.io", ErrRateLimited)
	r.check(t)

	for _, image := range []string{"nginx:latest", "redis:latest"} {
		got := r.result(t, image)
		if got.Status != store.UpdateSkipped || !strings.Contains(got.Message, "rate limiting") {
			t.Errorf("%s = %+v, want skipped with the rate limit as the reason", image, got)
		}
	}
	if got := r.result(t, "ghcr.io/acme/app:latest"); got.Status != store.UpdateUpToDate {
		t.Errorf("ghcr.io image = %+v, want a rate limit on docker.io to leave other registries checked", got)
	}
	var hubCalls int
	for _, c := range r.registry.Calls() {
		if strings.HasPrefix(c, "manifest") && !strings.Contains(c, "ghcr.io") {
			hubCalls++
		}
	}
	if hubCalls != 1 {
		t.Errorf("docker.io was asked %d times (%v), want once: a rate-limited registry is left alone for the rest of the run", hubCalls, r.registry.Calls())
	}
	statuses, err := r.checker.Statuses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range statuses {
		if s.Container != "c" && s.Status != store.UpdateSkipped {
			t.Errorf("Statuses %s = %q, want skipped", s.Container, s.Status)
		}
	}
}

func TestUpdateChecker_RateLimitedTagListIsNeverUpToDate(t *testing.T) {
	r := newUpdateRig()
	r.run(t, "db", "postgres", "16.4", digestOld, digestOld)
	r.checker.Registry = tagsLimited{r.registry}
	r.check(t)
	if got := r.result(t, "postgres:16.4"); got.Status != store.UpdateSkipped {
		t.Fatalf("= %+v, want skipped: the manifest matched but newer tags were not looked for", got)
	}
}

type tagsLimited struct{ *FakeRegistry }

func (tagsLimited) Tags(context.Context, ImageRef) ([]string, error) {
	return nil, ErrRateLimited
}

func TestUpdateChecker_FailuresAreNeverUpToDate(t *testing.T) {
	r := newUpdateRig()
	r.run(t, "gone", "acme/gone", "latest", digestOld, "")
	r.provider.AddContainer(Container{ID: "id-local", Name: "local", Image: "acme/local", Tag: "dev", ImageID: "sha256:local"})
	r.provider.AddImage(Image{ID: "sha256:local"})
	r.registry.SetDigest(ImageRef{Registry: "docker.io", Repository: "acme/local", Tag: "dev"}, digestNew)
	r.check(t)

	for _, image := range []string{"acme/gone:latest", "acme/local:dev"} {
		if got := r.result(t, image); got.Status != store.UpdateFailed || got.Message == "" {
			t.Errorf("%s = %+v, want failed with a reason", image, got)
		}
	}
}

func TestUpdateChecker_ARegistryThatWantsALoginIsNotCheckedNeverUpToDate(t *testing.T) {
	r := newUpdateRig()
	r.run(t, "denied", "acme/private", "latest", digestOld, "")
	r.registry.SetImageError(ImageRef{Registry: "docker.io", Repository: "acme/private", Tag: "latest"}, ErrRegistryDenied)
	tagsDenied := r.run(t, "tagsdenied", "acme/other", "1.0", digestOld, digestOld)
	r.registry.SetTagsError(tagsDenied, ErrRegistryDenied)
	// A stale up_to_date from before the registry wanted a login.
	r.results.checks["acme/private:latest"] = store.ImageUpdateCheck{Image: "acme/private:latest", Status: store.UpdateUpToDate}
	r.check(t)

	for _, image := range []string{"acme/private:latest", "acme/other:1.0"} {
		got := r.result(t, image)
		if got.Status != store.UpdateNotChecked || !strings.Contains(got.Message, "wants a login") || got.CheckedAt.IsZero() {
			t.Errorf("%s = %+v, want not_checked with the login as the reason and the time it was tried", image, got)
		}
	}
	statuses, err := r.checker.Statuses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range statuses {
		if s.Status != UpdateNotChecked || s.Message == "" || s.CheckedAt.IsZero() {
			t.Errorf("Statuses %s = %+v, want not_checked with a reason and the time it was tried", s.Container, s)
		}
	}
}

func TestUpdateChecker_ACutOffTagListIsNeverUpToDate(t *testing.T) {
	r := newUpdateRig()
	ref := r.run(t, "db", "postgres", "16.4", digestOld, digestOld)
	r.registry.SetTagsError(ref, errors.New("the tag list of library/postgres has more than 100 pages, so newer tags may be missing"))
	r.run(t, "db2", "ghcr.io/acme/app", "1.0", digestOld, digestNew)
	r.registry.SetTagsError(ImageRef{Registry: "ghcr.io", Repository: "acme/app"}, errors.New("cut off"))
	r.check(t)

	got := r.result(t, "postgres:16.4")
	if got.Status != store.UpdateFailed || !strings.Contains(got.Message, "more than 100 pages") {
		t.Errorf("postgres:16.4 = %+v, want failed naming the cut-off listing", got)
	}
	got = r.result(t, "ghcr.io/acme/app:1.0")
	if got.Status != store.UpdateAvailable || got.Kind != store.UpdateKindNewBuild || !strings.Contains(got.Message, "could not be listed") {
		t.Errorf("ghcr.io/acme/app:1.0 = %+v, want the new build kept, saying newer tags were not listed", got)
	}
}

func TestUpdateChecker_ADigestPinnedContainerHasNoTagToCheck(t *testing.T) {
	r := newUpdateRig()
	r.provider.AddContainer(Container{ID: "id-pin", Name: "pin", Image: "nginx", Tag: "", Pinned: true, ImageID: "sha256:img-pin"})
	r.provider.AddImage(Image{ID: "sha256:img-pin", RepoDigests: []string{"nginx@" + digestOld}})
	r.run(t, "web", "nginx", "latest", digestOld, digestNew)
	// A stale result for the key a pinned container used to be checked under.
	r.results.checks["nginx:latest"] = store.ImageUpdateCheck{Image: "nginx:latest", Status: store.UpdateUpToDate}
	r.check(t)

	if got := r.result(t, "nginx:latest"); got.Status != store.UpdateAvailable || got.Kind != store.UpdateKindNewBuild {
		t.Errorf("nginx:latest = %+v, want only the container that runs the latest tag judged by it", got)
	}
	for _, c := range r.registry.Calls() {
		if strings.Count(c, "nginx") > 0 && c != "manifest nginx:latest" {
			t.Errorf("registry call %q: the pinned container was looked up", c)
		}
	}
	statuses, err := r.checker.Statuses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range statuses {
		if s.Container == "pin" && (s.Status != UpdateNotChecked || !strings.Contains(s.Message, "pinned to an image digest") || !s.CheckedAt.IsZero()) {
			t.Errorf("pin = %+v, want not_checked saying it is pinned to a digest", s)
		}
	}
}

func TestUpdateChecker_PrunesImagesNoContainerRunsAndSkipsRecreateTemps(t *testing.T) {
	r := newUpdateRig()
	r.run(t, "web", "nginx", "latest", digestOld, digestOld)
	r.run(t, "web_hoserva-old", "nginx", "1.0", digestOld, digestOld)
	r.results.checks["redis:latest"] = store.ImageUpdateCheck{Image: "redis:latest", Status: store.UpdateAvailable}
	r.check(t)
	if _, ok := r.results.checks["redis:latest"]; ok {
		t.Error("the result of an image no container runs was kept")
	}
	if _, ok := r.results.checks["nginx:1.0"]; ok {
		t.Error("a _hoserva-old container, which a recreate is about to remove, was checked")
	}
}

func TestUpdateChecker_EngineUnavailableStoresNothing(t *testing.T) {
	r := newUpdateRig()
	r.run(t, "web", "nginx", "latest", digestOld, digestOld)
	r.results.checks["nginx:latest"] = store.ImageUpdateCheck{Image: "nginx:latest", Status: store.UpdateAvailable}
	r.provider.SetUnavailable(nil)
	err := r.checker.Run(context.Background(), &bytes.Buffer{})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Run = %v, want ErrUnavailable", err)
	}
	if r.results.puts != 0 || r.results.checks["nginx:latest"].Status != store.UpdateAvailable {
		t.Errorf("a check that could not list containers changed stored results: %+v", r.results.checks)
	}
}

func TestUpdateChecker_CancelledRunStoresNoResult(t *testing.T) {
	r := newUpdateRig()
	r.run(t, "web", "nginx", "latest", digestOld, digestOld)
	ctx, cancel := context.WithCancel(context.Background())
	r.checker.Registry = cancelling{cancel}
	if err := r.checker.Run(ctx, &bytes.Buffer{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	if r.results.puts != 0 {
		t.Errorf("a cancelled check stored %d results, want none: a cancel is not a failed registry", r.results.puts)
	}
}

// cancelling is a registry whose request is cut short by the caller's
// cancel, which surfaces as the context's error.
type cancelling struct{ cancel context.CancelFunc }

func (c cancelling) ManifestDigest(ctx context.Context, _ ImageRef) (string, error) {
	c.cancel()
	return "", ctx.Err()
}

func (cancelling) Tags(context.Context, ImageRef) ([]string, error) {
	return nil, nil
}

// A locally built container on the same image:tag neither hides its pulled
// sibling's update nor is reported as up to date itself.
func TestUpdateChecker_ALocallyBuiltSiblingFailsAloneNeverUpToDate(t *testing.T) {
	r := newUpdateRig()
	r.run(t, "pulled", "nginx", "latest", digestOld, digestNew)
	r.provider.AddContainer(Container{ID: "id-local", Name: "local", Image: "nginx", Tag: "latest", ImageID: "sha256:local"})
	r.provider.AddImage(Image{ID: "sha256:local"})
	r.check(t)

	if got := r.result(t, "nginx:latest"); got.Status != store.UpdateAvailable || got.Kind != store.UpdateKindNewBuild {
		t.Fatalf("nginx:latest = %+v, want update_available/new_build", got)
	}
	got, err := r.checker.Statuses(context.Background())
	if err != nil || len(got) != 2 {
		t.Fatalf("Statuses = %v, %v", got, err)
	}
	if local := got[0]; local.Container != "local" || local.Status != store.UpdateFailed || local.Kind != "" || local.Message == "" {
		t.Errorf("local = %+v, want failed with a reason", local)
	}
	if pulled := got[1]; pulled.Container != "pulled" || pulled.Status != store.UpdateAvailable || pulled.Kind != store.UpdateKindNewBuild {
		t.Errorf("pulled = %+v, want update_available/new_build", pulled)
	}
}

func TestUpdateChecker_StatusesNeverReadUncheckedAsUpToDate(t *testing.T) {
	r := newUpdateRig()
	r.run(t, "new", "nginx", "latest", digestOld, digestOld)
	got, err := r.checker.Statuses(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("Statuses = %v, %v", got, err)
	}
	if got[0].Status != UpdateNotChecked || !got[0].CheckedAt.IsZero() {
		t.Fatalf("= %+v, want not_checked with no time", got[0])
	}
	r.check(t)
	got, _ = r.checker.Statuses(context.Background())
	if got[0].Status != store.UpdateUpToDate || got[0].CheckedAt.IsZero() {
		t.Fatalf("after a check = %+v, want up_to_date with its time", got[0])
	}
}

func TestVersionTags(t *testing.T) {
	cur, ok := parseVersionTag("10.9.7")
	if !ok {
		t.Fatal("10.9.7 is a version tag")
	}
	for _, tag := range []string{"latest", "stable", "lts", "alpine"} {
		if _, ok := parseVersionTag(tag); ok {
			t.Errorf("%q parsed as a version tag", tag)
		}
	}
	if _, ok := parseVersionTag("99999999999999999999.1"); ok {
		t.Error("an overflowing part parsed as a version tag")
	}
	tags := []string{"10.9.7", "10.9.8", "10.10.0", "10.9", "11.0.0-rc1", "9.99.99", "v10.11.0", "10.9.7-alpine"}
	if got, ok := newestVersionTag(cur, tags); !ok || got != "10.10.0" {
		t.Errorf("newestVersionTag = %q, %v, want 10.10.0 (numeric, not lexical, and only the same shape)", got, ok)
	}
	if got, ok := newestVersionTag(cur, []string{"10.9.7", "9.0.0"}); ok {
		t.Errorf("newestVersionTag = %q, want none when nothing is newer", got)
	}
}

func TestImageRef(t *testing.T) {
	for _, c := range []struct{ image, tag, want, registry, repo string }{
		{"nginx", "", "nginx:latest", "docker.io", "library/nginx"},
		{"portainer/portainer-ce", "2.21.4", "portainer/portainer-ce:2.21.4", "docker.io", "portainer/portainer-ce"},
		{"lscr.io/linuxserver/jellyfin", "10.9.7", "lscr.io/linuxserver/jellyfin:10.9.7", "lscr.io", "linuxserver/jellyfin"},
		{"registry.example.com:5000/app", "1", "registry.example.com:5000/app:1", "registry.example.com:5000", "app"},
	} {
		ref, err := ParseImageRef(c.image, c.tag)
		if err != nil || ref.String() != c.want || ref.Registry != c.registry || ref.Repository != c.repo {
			t.Errorf("ParseImageRef(%q, %q) = %+v (%v), want %s on %s as %s", c.image, c.tag, ref, err, c.want, c.registry, c.repo)
		}
	}
}

// Two containers run jellyfin:10.9.7; one is updated, which moves the tag to
// the new image, so the Engine lists the other under its image ID.
func movedTagRig(t *testing.T) *updateRig {
	t.Helper()
	r := newUpdateRig()
	const repo, tag = "lscr.io/linuxserver/jellyfin", "10.9.7"
	ref, err := ParseImageRef(repo, tag)
	if err != nil {
		t.Fatal(err)
	}
	r.provider.AddContainer(Container{ID: "id-a", Name: "a", Image: repo, Tag: tag, ImageID: "sha256:img-new"})
	r.provider.AddContainer(Container{ID: "id-b", Name: "b", Image: repo, Tag: tag, ImageID: "sha256:img-old"})
	r.provider.AddImage(Image{ID: "sha256:img-new", RepoTags: []string{ref.String()}, RepoDigests: []string{repo + "@" + digestNew}})
	r.provider.AddImage(Image{ID: "sha256:img-old", RepoDigests: []string{repo + "@" + digestOld}})
	r.registry.SetDigest(ref, digestNew)
	r.registry.SetTags(ref, []string{tag})
	listed, err := r.provider.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if listed[1].Image != "sha256" {
		t.Fatalf("the fake did not rewrite the listing of the container left on the old image: %+v", listed[1])
	}
	return r
}

func TestUpdateChecker_AContainerWhoseTagMovedIsCheckedByItsCreationReference(t *testing.T) {
	r := movedTagRig(t)
	r.check(t)

	if got := r.result(t, "lscr.io/linuxserver/jellyfin:10.9.7"); got.Status != store.UpdateAvailable || got.Kind != store.UpdateKindNewBuild {
		t.Fatalf("jellyfin:10.9.7 = %+v, want update_available/new_build for the container left on the old image", got)
	}
	for image := range r.results.checks {
		if strings.Contains(image, "sha256") {
			t.Errorf("a result is stored for %q, the image ID the listing reports", image)
		}
	}
	for _, c := range r.registry.Calls() {
		if strings.Contains(c, "sha256") {
			t.Errorf("registry call %q: the listing's image ID was taken for a repository", c)
		}
	}
	statuses, err := r.checker.Statuses(context.Background())
	if err != nil || len(statuses) != 2 {
		t.Fatalf("Statuses = %v, %v", statuses, err)
	}
	for _, s := range statuses {
		if s.Container == "b" && (s.Status != store.UpdateAvailable || s.Image != "lscr.io/linuxserver/jellyfin" || s.Tag != "10.9.7") {
			t.Errorf("b = %+v, want update_available on lscr.io/linuxserver/jellyfin:10.9.7, never not_checked with a login message", s)
		}
	}
}

func TestUpdateChecker_BulkUpdateIncludesAContainerWhoseTagMoved(t *testing.T) {
	r := movedTagRig(t)
	r.check(t)
	u := &Updater{
		Lifecycle: &Lifecycle{Provider: r.provider},
		History:   newMemHistory(),
		Snapshots: &fakeSnapshots{provider: r.provider},
		Statuses:  r.checker,
	}
	sel, err := u.BulkTargets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(sel.Containers, "b") {
		t.Fatalf("bulk targets = %v, want b included", sel.Containers)
	}
}
