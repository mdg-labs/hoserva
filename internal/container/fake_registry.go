package container

import (
	"context"
	"sync"
)

// FakeRegistry is a scriptable RegistryClient (doc 06 §2): a test sets the
// digests and tags each image has, or an error for a registry or an image,
// without any network. Calls records what was asked, so a test can assert
// that a rate-limited registry was not asked again.
type FakeRegistry struct {
	mu       sync.Mutex
	digests  map[string]string
	tags     map[string][]string
	tagErrs  map[string]error
	errs     map[string]error
	registry map[string]error
	calls    []string
}

// NewFakeRegistry returns a FakeRegistry that knows no image.
func NewFakeRegistry() *FakeRegistry {
	return &FakeRegistry{
		digests:  map[string]string{},
		tags:     map[string][]string{},
		tagErrs:  map[string]error{},
		errs:     map[string]error{},
		registry: map[string]error{},
	}
}

// SetDigest scripts the digest ManifestDigest returns for ref.
func (f *FakeRegistry) SetDigest(ref ImageRef, digest string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.digests[ref.String()] = digest
}

// SetTags scripts the tags Tags returns for ref's repository.
func (f *FakeRegistry) SetTags(ref ImageRef, tags []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tags[ref.Registry+"/"+ref.Repository] = tags
}

// SetTagsError scripts an error Tags returns for ref's repository.
func (f *FakeRegistry) SetTagsError(ref ImageRef, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tagErrs[ref.Registry+"/"+ref.Repository] = err
}

// SetImageError scripts an error ManifestDigest returns for ref.
func (f *FakeRegistry) SetImageError(ref ImageRef, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[ref.String()] = err
}

// SetRegistryError scripts an error every call to registry returns.
func (f *FakeRegistry) SetRegistryError(registry string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registry[registry] = err
}

// Calls is every "manifest <image>" and "tags <repository>" asked so far.
func (f *FakeRegistry) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *FakeRegistry) record(call string) {
	f.calls = append(f.calls, call)
}

func (f *FakeRegistry) ManifestDigest(ctx context.Context, ref ImageRef) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("manifest " + ref.String())
	if err := f.registry[ref.Registry]; err != nil {
		return "", err
	}
	if err := f.errs[ref.String()]; err != nil {
		return "", err
	}
	d, ok := f.digests[ref.String()]
	if !ok {
		return "", ErrManifestNotFound
	}
	return d, nil
}

func (f *FakeRegistry) Tags(ctx context.Context, ref ImageRef) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("tags " + ref.Registry + "/" + ref.Repository)
	if err := f.registry[ref.Registry]; err != nil {
		return nil, err
	}
	if err := f.tagErrs[ref.Registry+"/"+ref.Repository]; err != nil {
		return nil, err
	}
	return append([]string(nil), f.tags[ref.Registry+"/"+ref.Repository]...), nil
}

var _ RegistryClient = (*FakeRegistry)(nil)
