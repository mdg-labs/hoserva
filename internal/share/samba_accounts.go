package share

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// SambaAccounts sets and removes a user's Samba passdb entry (#49, Q27,
// doc 03 §7) — the system-touching half of "setting a password writes the
// UI hash and the Samba passdb entry together". It sits behind this
// interface, with a scriptable fake below, so nothing exercising the
// password-set flow ever execs smbpasswd (CLAUDE.md).
type SambaAccounts interface {
	// SetPassword creates username's Samba account if it doesn't already
	// exist, or updates its password if it does.
	SetPassword(ctx context.Context, username, password string) error
	// Delete removes username's Samba account. Deleting an account that
	// was never provisioned is not an error.
	Delete(ctx context.Context, username string) error
}

// SmbpasswdAccounts is the real SambaAccounts, exec'ing smbpasswd with an
// argv Go builds directly — never a shell (CLAUDE.md: "never interpolate
// user or template input into a shell command").
type SmbpasswdAccounts struct{}

// NewSmbpasswdAccounts returns the real SambaAccounts.
func NewSmbpasswdAccounts() SmbpasswdAccounts { return SmbpasswdAccounts{} }

var _ SambaAccounts = SmbpasswdAccounts{}

// SetPassword runs `smbpasswd -a -s <username>`, which both creates a new
// Samba account and resets an existing one's password (`-a` is a no-op on
// an already-provisioned account beyond the reset). `-s` puts smbpasswd in
// scripted mode, reading the new password twice from stdin — never from
// argv, where it would leak into the process list every other local
// account can read.
func (SmbpasswdAccounts) SetPassword(ctx context.Context, username, password string) error {
	cmd := exec.CommandContext(ctx, "smbpasswd", "-a", "-s", username)
	cmd.Stdin = strings.NewReader(password + "\n" + password + "\n")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("smbpasswd -a %s: %w: %s", username, err, bytes.TrimSpace(stderr.Bytes()))
	}
	return nil
}

// Delete runs `smbpasswd -x <username>`. Deleting an account that was
// never provisioned is not an error (the SambaAccounts contract, e.g. a
// user deleted before any password was ever set for them): smbpasswd
// reports that case on stderr ("Failed to find entry for user …") rather
// than a dedicated exit status, so that message is what tells a
// genuinely absent account apart from a real failure.
func (SmbpasswdAccounts) Delete(ctx context.Context, username string) error {
	cmd := exec.CommandContext(ctx, "smbpasswd", "-x", username)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if strings.Contains(strings.ToLower(stderr.String()), "failed to find entry") {
			return nil
		}
		return fmt.Errorf("smbpasswd -x %s: %w: %s", username, err, bytes.TrimSpace(stderr.Bytes()))
	}
	return nil
}

// FakeSambaAccounts is a scriptable simulator of SambaAccounts (doc 06
// §2): a test tells it the next SetPassword or Delete call should fail,
// and reads back which passwords it actually recorded, instead of running
// smbpasswd against a host that has no Samba installed.
type FakeSambaAccounts struct {
	mu        sync.Mutex
	passwords map[string]string
	setErr    error
	deleteErr error
}

// NewFakeSambaAccounts returns a FakeSambaAccounts with no accounts and no
// scripted failures.
func NewFakeSambaAccounts() *FakeSambaAccounts {
	return &FakeSambaAccounts{passwords: make(map[string]string)}
}

var _ SambaAccounts = (*FakeSambaAccounts)(nil)

// SetPassword records password for username, unless FailSetPassword has
// scripted a failure.
func (f *FakeSambaAccounts) SetPassword(ctx context.Context, username, password string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return f.setErr
	}
	f.passwords[username] = password
	return nil
}

// Delete removes username's recorded password, unless FailDelete has
// scripted a failure.
func (f *FakeSambaAccounts) Delete(ctx context.Context, username string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.passwords, username)
	return nil
}

// FailSetPassword scripts every future SetPassword call to fail with err.
// Pass nil to clear.
func (f *FakeSambaAccounts) FailSetPassword(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setErr = err
}

// FailDelete scripts every future Delete call to fail with err. Pass nil
// to clear.
func (f *FakeSambaAccounts) FailDelete(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteErr = err
}

// Password returns the password last recorded for username, and whether
// one exists at all.
func (f *FakeSambaAccounts) Password(username string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.passwords[username]
	return p, ok
}
