package share

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// SambaAccounts sets and removes a user's Samba passdb entry and the system
// account behind it (#49, #596, Q26, Q27, doc 03 §7) — the system-touching
// half of "setting a password writes the UI hash and the Samba passdb entry
// together". It sits behind this interface, with a scriptable fake below, so nothing exercising the
// password-set flow ever execs smbpasswd (CLAUDE.md).
type SambaAccounts interface {
	// SetPassword creates username's system and Samba accounts if they don't
	// already exist, or updates the password if they do.
	SetPassword(ctx context.Context, username, password string) error
	// Delete removes username's Samba account and then its system account.
	// Deleting an account that was never provisioned is not an error.
	Delete(ctx context.Context, username string) error
}

// SmbpasswdAccounts is the real SambaAccounts. `smbpasswd -a` needs a system
// account of the same name, so it provisions that account (Unix) around every
// smbpasswd call. Commands run
// through Cmd with an argv Go builds directly — never a shell (CLAUDE.md:
// "never interpolate user or template input into a shell command").
type SmbpasswdAccounts struct {
	Unix UnixAccounts
	Cmd  Commander
}

// NewSmbpasswdAccounts returns the real SambaAccounts.
func NewSmbpasswdAccounts() SmbpasswdAccounts {
	cmd := ExecCommander{}
	return SmbpasswdAccounts{Unix: CommandUnixAccounts{Cmd: cmd}, Cmd: cmd}
}

var _ SambaAccounts = SmbpasswdAccounts{}

// SetPassword makes sure username has its system account, then runs `smbpasswd
// -a -s <username>`, which both creates a new Samba account and resets an
// existing one's password (`-a` is a no-op on an already-provisioned account
// beyond the reset). `-s` puts smbpasswd in scripted mode, reading the new
// password twice from stdin — never from argv, where it would leak into the
// process list every other local account can read.
//
// When smbpasswd fails after this call created the system account, the
// account is removed again: it has no Samba entry, so it could not log in,
// but nothing refers to it. A name that is not a valid username is refused
// before any command runs.
func (a SmbpasswdAccounts) SetPassword(ctx context.Context, username, password string) error {
	if err := ValidateAccountName(username); err != nil {
		return err
	}
	created, err := a.Unix.Ensure(ctx, username)
	if err != nil {
		return err
	}
	res, err := a.Cmd.Run(ctx, password+"\n"+password+"\n", "smbpasswd", "-a", "-s", username)
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("smbpasswd -a %s exited %d: %s", username, res.ExitCode, res.Stderr)
	}
	if err == nil {
		return nil
	}
	if !created {
		return err
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), compensationTimeout)
	defer cancel()
	if rmErr := a.Unix.Remove(rctx, username); rmErr != nil {
		return errors.Join(err, fmt.Errorf("removing the system account just created for %s: %w", username, rmErr))
	}
	return err
}

// compensationTimeout bounds the removal of a system account created by a
// SetPassword whose smbpasswd then failed, which must run even when the
// request that failed was cancelled.
const compensationTimeout = 10 * time.Second

// Delete runs `smbpasswd -x <username>`, then removes the system account. The
// order is what makes a partial failure safe: once the Samba entry is gone no
// SMB login works, and the system account that may remain has no password and
// no login shell, so nothing can log in either way; a retry finishes the
// job. Deleting an account that was never provisioned is not an error (the
// SambaAccounts contract, e.g. a user deleted before any password was ever
// set for them): smbpasswd reports that case on stderr ("Failed to find entry
// for user …") rather than a dedicated exit status, so that message is what
// tells a genuinely absent account apart from a real failure.
//
// A name that is not a valid username can have no account of Hoserva's and
// is skipped without running anything.
func (a SmbpasswdAccounts) Delete(ctx context.Context, username string) error {
	if ValidateAccountName(username) != nil {
		return nil
	}
	res, err := a.Cmd.Run(ctx, "", "smbpasswd", "-x", username)
	if err != nil {
		return fmt.Errorf("smbpasswd -x %s: %w", username, err)
	}
	if res.ExitCode != 0 && !strings.Contains(strings.ToLower(res.Stderr+res.Stdout), "failed to find entry") {
		return fmt.Errorf("smbpasswd -x %s exited %d: %s", username, res.ExitCode, res.Stderr)
	}
	return a.Unix.Remove(ctx, username)
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
