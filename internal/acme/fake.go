package acme

import (
	"context"
	"fmt"
	"sync"
)

// FakeClient is a scriptable Client (CLAUDE.md). It never contacts a
// network; tests set IssueFn. A nil IssueFn fails rather than succeeding
// by default — a silent always-succeed stub is not this package's
// production client.
type FakeClient struct {
	mu      sync.Mutex
	IssueFn func(ctx context.Context, req IssueRequest) (*Certificate, error)
	Calls   []IssueRequest
}

func (f *FakeClient) Issue(ctx context.Context, req IssueRequest) (*Certificate, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, req)
	fn := f.IssueFn
	f.mu.Unlock()
	if fn == nil {
		return nil, fmt.Errorf("acme: fake client has no IssueFn")
	}
	return fn(ctx, req)
}

// FakeSolver records Present/CleanUp for DNS-01 tests.
type FakeSolver struct {
	mu        sync.Mutex
	PresentFn func(ctx context.Context, fqdn, value string) error
	CleanUpFn func(ctx context.Context, fqdn, value string) error
	Presented [][2]string
	Cleaned   [][2]string
}

func (f *FakeSolver) Present(ctx context.Context, fqdn, value string) error {
	f.mu.Lock()
	f.Presented = append(f.Presented, [2]string{fqdn, value})
	fn := f.PresentFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, fqdn, value)
	}
	return nil
}

func (f *FakeSolver) CleanUp(ctx context.Context, fqdn, value string) error {
	f.mu.Lock()
	f.Cleaned = append(f.Cleaned, [2]string{fqdn, value})
	fn := f.CleanUpFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, fqdn, value)
	}
	return nil
}

// RecordingInstaller records Install calls and holds the current view.
// InstallFn, when set, is the swap — a test that wants a failed swap
// returns an error from it and Current stays unchanged.
type RecordingInstaller struct {
	mu        sync.Mutex
	View      CertView
	CertPEM   []byte
	KeyPEM    []byte
	Installs  int
	InstallFn func(certPEM, keyPEM []byte) error
}

func (r *RecordingInstaller) Current() (CertView, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.View, nil
}

func (r *RecordingInstaller) Install(certPEM, keyPEM []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Installs++
	if r.InstallFn != nil {
		return r.InstallFn(certPEM, keyPEM)
	}
	r.CertPEM = append([]byte(nil), certPEM...)
	r.KeyPEM = append([]byte(nil), keyPEM...)
	r.View.Kind = KindLetsEncrypt
	return nil
}

// FakePublisher records Publish calls.
type FakePublisher struct {
	mu     sync.Mutex
	Events [][3]string
	Err    error
}

func (f *FakePublisher) Publish(_ context.Context, event, title, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Events = append(f.Events, [3]string{event, title, message})
	return f.Err
}

// FakeCipher is a reversible XOR, not real cryptography — tests that
// need Q28 unreadability use *auth.MachineKey instead.
type FakeCipher struct{}

func (FakeCipher) Encrypt(plaintext []byte) ([]byte, error) {
	return xorACME(plaintext), nil
}

func (FakeCipher) Decrypt(ciphertext []byte) ([]byte, error) {
	return xorACME(ciphertext), nil
}

func xorACME(in []byte) []byte {
	out := make([]byte, len(in))
	for i, b := range in {
		out[i] = b ^ 0x5a
	}
	return out
}
