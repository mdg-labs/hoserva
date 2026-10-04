package share

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

const (
	// UnixUIDMin and UnixUIDMax bound the UIDs of the system accounts Hoserva
	// creates for share users (Q26). The range sits above the UIDs a host's
	// own login accounts get and below the ones systemd hands out, so an
	// account in it with a no-login shell is Hoserva's own.
	UnixUIDMin = 30000
	UnixUIDMax = 39999
	// unixGID is the shared data group, users (Q26): every share user's
	// primary group, so a file it writes over SMB belongs to the same group
	// as every other share file.
	unixGID = "100"
	// unixShell is the shell of a share user's system account: it cannot log
	// in to the host, only to Samba.
	unixShell = "/usr/sbin/nologin"
)

// accountNamePattern is the one form of username Hoserva provisions an account
// for: lower case, because Samba matches names without regard to case and the
// store keeps one lower-cased name per user, and within what useradd accepts.
var accountNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_.-]{0,63}$`)

var (
	// ErrInvalidAccountName is a username Hoserva cannot give a system
	// account.
	ErrInvalidAccountName = errors.New("a username must be lower-case letters, digits, '_', '.' and '-', start with a letter or '_', and be at most 64 characters")
	// ErrAccountNameTaken is a username that a system account Hoserva did not
	// create already holds: giving that name an SMB password would hand SMB
	// access to someone else's account.
	ErrAccountNameTaken = errors.New("a system account with that name already exists and is not one Hoserva manages")
)

// ValidateAccountName reports whether name is one Hoserva provisions a system
// account for. It is the check that stands between a username and any command
// that names it.
func ValidateAccountName(name string) error {
	if !accountNamePattern.MatchString(name) {
		return fmt.Errorf("%w: %q", ErrInvalidAccountName, name)
	}
	return nil
}

// UnixAccounts creates and removes the system account Samba needs for each
// share user: `smbpasswd -a` refuses a name no Unix account holds. It sits
// behind this interface, with a scriptable fake, so nothing outside the lab
// ever execs useradd (CLAUDE.md).
type UnixAccounts interface {
	// Ensure makes sure username has a Hoserva-managed system account, and
	// reports whether this call created it. A username that a system account
	// not made by Hoserva already holds is ErrAccountNameTaken.
	Ensure(ctx context.Context, username string) (created bool, err error)
	// Remove deletes username's Hoserva-managed system account. An account
	// that does not exist, or that Hoserva did not create, is left as it is
	// and is not an error.
	Remove(ctx context.Context, username string) error
}

// CommandUnixAccounts is the real UnixAccounts: useradd and userdel through a
// Commander, with an argv built here and never a shell. The account it makes
// is locked (no password), has no home directory and no login shell, and has
// users (GID 100) as its primary group and a UID in UnixUIDMin..UnixUIDMax.
type CommandUnixAccounts struct {
	Cmd Commander
}

var _ UnixAccounts = CommandUnixAccounts{}

type passwdEntry struct {
	uid   int
	shell string
}

// managed says whether the account is one this type creates: a UID in the
// reserved range and a no-login shell.
func (e passwdEntry) managed() bool {
	return e.uid >= UnixUIDMin && e.uid <= UnixUIDMax && path.Base(e.shell) == "nologin"
}

// getent exits 2 when the key is not in the database.
const getentNotFound = 2

func (c CommandUnixAccounts) lookup(ctx context.Context, username string) (passwdEntry, bool, error) {
	res, err := c.Cmd.Run(ctx, "", "getent", "passwd", "--", username)
	if err != nil {
		return passwdEntry{}, false, fmt.Errorf("looking up system account %s: %w", username, err)
	}
	switch res.ExitCode {
	case 0:
	case getentNotFound:
		return passwdEntry{}, false, nil
	default:
		return passwdEntry{}, false, fmt.Errorf("getent passwd %s exited %d: %s", username, res.ExitCode, res.Stderr)
	}
	line, _, _ := strings.Cut(res.Stdout, "\n")
	fields := strings.Split(line, ":")
	if len(fields) != 7 || fields[0] != username {
		return passwdEntry{}, false, fmt.Errorf("getent passwd %s returned %q, which is not a passwd entry for that name", username, line)
	}
	uid, err := strconv.Atoi(fields[2])
	if err != nil {
		return passwdEntry{}, false, fmt.Errorf("getent passwd %s returned the non-numeric uid %q", username, fields[2])
	}
	return passwdEntry{uid: uid, shell: fields[6]}, true, nil
}

// useradd exits 9 when the name is already in use.
const useraddNameInUse = 9

func (c CommandUnixAccounts) Ensure(ctx context.Context, username string) (bool, error) {
	if err := ValidateAccountName(username); err != nil {
		return false, err
	}
	entry, found, err := c.lookup(ctx, username)
	if err != nil {
		return false, err
	}
	if found {
		if !entry.managed() {
			return false, fmt.Errorf("%w: %s", ErrAccountNameTaken, username)
		}
		return false, nil
	}
	res, err := c.Cmd.Run(ctx, "", "useradd",
		"--no-create-home", "--no-user-group", "--gid", unixGID, "--shell", unixShell,
		"--comment", "Hoserva share user",
		"-K", "UID_MIN="+strconv.Itoa(UnixUIDMin), "-K", "UID_MAX="+strconv.Itoa(UnixUIDMax),
		"--", username)
	if err != nil {
		return false, fmt.Errorf("useradd %s: %w", username, err)
	}
	switch res.ExitCode {
	case 0:
		return true, nil
	case useraddNameInUse:
		entry, found, err := c.lookup(ctx, username)
		if err != nil {
			return false, err
		}
		if found && entry.managed() {
			return false, nil
		}
		return false, fmt.Errorf("%w: %s", ErrAccountNameTaken, username)
	default:
		return false, fmt.Errorf("useradd %s exited %d: %s", username, res.ExitCode, res.Stderr)
	}
}

// userdel exits 6 when the account does not exist.
const userdelNoSuchUser = 6

func (c CommandUnixAccounts) Remove(ctx context.Context, username string) error {
	if err := ValidateAccountName(username); err != nil {
		return nil
	}
	entry, found, err := c.lookup(ctx, username)
	if err != nil {
		return err
	}
	if !found || !entry.managed() {
		return nil
	}
	res, err := c.Cmd.Run(ctx, "", "userdel", "--", username)
	if err != nil {
		return fmt.Errorf("userdel %s: %w", username, err)
	}
	if res.ExitCode != 0 && res.ExitCode != userdelNoSuchUser {
		return fmt.Errorf("userdel %s exited %d: %s", username, res.ExitCode, res.Stderr)
	}
	return nil
}

// FakeUnixAccounts is a scriptable simulator of UnixAccounts (doc 06 §2): it
// keeps the set of accounts it was asked to make, can be told a name is held
// by an account that is not Hoserva's, and can be told the next Ensure or
// Remove should fail.
type FakeUnixAccounts struct {
	mu        sync.Mutex
	managed   map[string]bool
	foreign   map[string]bool
	events    []string
	ensureErr error
	removeErr error
}

// NewFakeUnixAccounts returns a FakeUnixAccounts with no accounts and no
// scripted failures.
func NewFakeUnixAccounts() *FakeUnixAccounts {
	return &FakeUnixAccounts{managed: map[string]bool{}, foreign: map[string]bool{}}
}

var _ UnixAccounts = (*FakeUnixAccounts)(nil)

func (f *FakeUnixAccounts) Ensure(ctx context.Context, username string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "ensure:"+username)
	if err := ValidateAccountName(username); err != nil {
		return false, err
	}
	if f.ensureErr != nil {
		return false, f.ensureErr
	}
	if f.foreign[username] {
		return false, fmt.Errorf("%w: %s", ErrAccountNameTaken, username)
	}
	if f.managed[username] {
		return false, nil
	}
	f.managed[username] = true
	return true, nil
}

func (f *FakeUnixAccounts) Remove(ctx context.Context, username string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "remove:"+username)
	if f.removeErr != nil {
		return f.removeErr
	}
	delete(f.managed, username)
	return nil
}

// HoldForeign marks username as held by a system account Hoserva did not
// create.
func (f *FakeUnixAccounts) HoldForeign(username string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.foreign[username] = true
}

// FailEnsure scripts every future Ensure call to fail with err. Pass nil to
// clear.
func (f *FakeUnixAccounts) FailEnsure(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureErr = err
}

// FailRemove scripts every future Remove call to fail with err. Pass nil to
// clear.
func (f *FakeUnixAccounts) FailRemove(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeErr = err
}

// Has reports whether a Hoserva-managed account for username exists.
func (f *FakeUnixAccounts) Has(username string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.managed[username]
}

// Events returns every Ensure and Remove call so far, as "ensure:<name>" and
// "remove:<name>", in order.
func (f *FakeUnixAccounts) Events() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}
