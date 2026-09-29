package backup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// FakeSecretSource is a scriptable SecretSource for tests (CLAUDE.md).
type FakeSecretSource struct {
	Passphrase string
	HasPass    bool
	Secrets    []DatabaseSecret
}

func (f *FakeSecretSource) BackupPassphrase(ctx context.Context) (string, bool, error) {
	return f.Passphrase, f.HasPass, nil
}

func (f *FakeSecretSource) DatabaseSecrets(ctx context.Context) ([]DatabaseSecret, error) {
	return f.Secrets, nil
}

// FakeSecretCipher is a reversible XOR cipher for tests — not real crypto.
type FakeSecretCipher struct{}

func (FakeSecretCipher) Decrypt(ciphertext []byte) ([]byte, error) {
	out := make([]byte, len(ciphertext))
	for i, b := range ciphertext {
		out[i] = b ^ 0x5a
	}
	return out, nil
}

// Encrypt reverses Decrypt's XOR (it is its own inverse), so
// FakeSecretCipher also satisfies RecipientCipher for tests that need
// both directions.
func (FakeSecretCipher) Encrypt(plaintext []byte) ([]byte, error) {
	return FakeSecretCipher{}.Decrypt(plaintext)
}

// FakeRecipientStore is a scriptable RecipientStore for tests (CLAUDE.md)
// — no database involved. A caller shares one instance across two
// LoadOrGenerateRecipient calls to simulate the row persisting across a
// daemon restart, the same way a real database would.
type FakeRecipientStore struct {
	Public          string
	WrappedIdentity []byte
	CheckValue      []byte
	Found           bool
}

var _ RecipientStore = (*FakeRecipientStore)(nil)

func (f *FakeRecipientStore) GetRecipient(ctx context.Context) (string, []byte, []byte, bool, error) {
	return f.Public, f.WrappedIdentity, f.CheckValue, f.Found, nil
}

func (f *FakeRecipientStore) SetRecipient(ctx context.Context, publicRecipient string, wrappedIdentity, checkValue []byte, createdAt time.Time) error {
	f.Public = publicRecipient
	f.WrappedIdentity = wrappedIdentity
	f.CheckValue = checkValue
	f.Found = true
	return nil
}

// FakeDestinationStore is an in-memory DestinationStore for tests
// (CLAUDE.md).
type FakeDestinationStore struct {
	mu    sync.Mutex
	dests []Destination
}

var _ DestinationStore = (*FakeDestinationStore)(nil)

func (f *FakeDestinationStore) ListDestinations(ctx context.Context) ([]Destination, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Destination(nil), f.dests...), nil
}

func (f *FakeDestinationStore) GetDestination(ctx context.Context, id string) (Destination, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.dests {
		if d.ID == id {
			return d, nil
		}
	}
	return Destination{}, ErrDestinationNotFound
}

func (f *FakeDestinationStore) CreateDestination(ctx context.Context, d Destination) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dests = append(f.dests, d)
	return nil
}

func (f *FakeDestinationStore) SeedDestinations(ctx context.Context, ds []Destination) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.dests) > 0 {
		return nil
	}
	f.dests = append(f.dests, ds...)
	return nil
}

func (f *FakeDestinationStore) DeleteDestination(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, d := range f.dests {
		if d.ID == id {
			f.dests = append(f.dests[:i], f.dests[i+1:]...)
			return nil
		}
	}
	return ErrDestinationNotFound
}

func (f *FakeDestinationStore) update(id string, fn func(*Destination)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.dests {
		if f.dests[i].ID == id {
			fn(&f.dests[i])
			return nil
		}
	}
	return ErrDestinationNotFound
}

func (f *FakeDestinationStore) RecordBackupSuccess(ctx context.Context, id string, at time.Time) error {
	return f.update(id, func(d *Destination) {
		d.LastSuccessfulBackupAt = &at
		d.StaleAlertedAt = nil
	})
}

func (f *FakeDestinationStore) MarkStaleAlerted(ctx context.Context, id string, observed *time.Time, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.dests {
		d := &f.dests[i]
		if d.ID != id {
			continue
		}
		if (d.LastSuccessfulBackupAt == nil) != (observed == nil) ||
			(observed != nil && !d.LastSuccessfulBackupAt.Equal(*observed)) {
			return ErrDestinationNotFound
		}
		d.StaleAlertedAt = &at
		return nil
	}
	return ErrDestinationNotFound
}

// FakeRclone is a scriptable RcloneRunner backed by an in-memory remote
// filesystem, for tests (CLAUDE.md). It understands exactly the rclone
// subcommands the package runs — version, obscure, copy, copyto, lsjson, cat,
// deletefile — and records every call.
type FakeRclone struct {
	mu sync.Mutex
	// Missing makes every call return ErrRcloneMissing.
	Missing bool
	// Fail maps a subcommand name to the error its calls return.
	Fail map[string]error
	// Now stamps uploaded files; nil uses time.Now.
	Now func() time.Time

	files map[string][]byte
	mtime map[string]time.Time
	dirs  map[string]bool
	calls []RcloneCommand
}

var _ RcloneRunner = (*FakeRclone)(nil)

// Calls returns every command run so far.
func (f *FakeRclone) Calls() []RcloneCommand {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]RcloneCommand(nil), f.calls...)
}

// Files returns the remote file paths ("<remote>:<dir>/<name>") present.
func (f *FakeRclone) Files() map[string][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string][]byte, len(f.files))
	for k, v := range f.files {
		out[k] = append([]byte(nil), v...)
	}
	return out
}

// Put places a file on the fake remote with the given modification time.
func (f *FakeRclone) Put(remoteDir, name string, data []byte, mod time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	key := fakeJoin(remoteDir, name)
	f.files[key] = data
	f.mtime[key] = mod
	f.dirs[remoteDir] = true
}

func (f *FakeRclone) init() {
	if f.files == nil {
		f.files, f.mtime, f.dirs = map[string][]byte{}, map[string]time.Time{}, map[string]bool{}
	}
}

func fakeJoin(dir, name string) string {
	if strings.HasSuffix(dir, ":") {
		return dir + name
	}
	return dir + "/" + name
}

func (f *FakeRclone) Run(ctx context.Context, c RcloneCommand) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	f.calls = append(f.calls, RcloneCommand{
		Args: append([]string(nil), c.Args...), Env: append([]string(nil), c.Env...), Stdin: append([]byte(nil), c.Stdin...),
	})
	if f.Missing {
		return nil, ErrRcloneMissing
	}
	if len(c.Args) == 0 {
		return nil, &RcloneExitError{Code: 1, Output: "no subcommand"}
	}
	if err := f.Fail[c.Args[0]]; err != nil {
		return nil, err
	}
	args := c.Args
	switch args[0] {
	case "version":
		return []byte("rclone v1.0-fake\n"), nil
	case "obscure":
		return []byte("obscured-" + string(c.Stdin) + "\n"), nil
	case "copy":
		src, dir := args[len(args)-2], args[len(args)-1]
		data, err := os.ReadFile(src)
		if err != nil {
			return nil, &RcloneExitError{Code: 1, Output: err.Error()}
		}
		key := fakeJoin(dir, filepath.Base(src))
		if _, exists := f.files[key]; exists && contains(args, "--immutable") {
			return nil, &RcloneExitError{Code: 1, Output: "immutable file modified"}
		}
		now := time.Now()
		if f.Now != nil {
			now = f.Now()
		}
		f.files[key], f.mtime[key], f.dirs[dir] = data, now, true
		return nil, nil
	case "copyto":
		src, dst := args[len(args)-2], args[len(args)-1]
		data, ok := f.files[src]
		if !ok {
			return nil, &RcloneExitError{Code: 4, Output: "object not found"}
		}
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return nil, &RcloneExitError{Code: 1, Output: err.Error()}
		}
		return nil, nil
	case "lsjson":
		dir := args[len(args)-1]
		if !f.dirs[dir] {
			return nil, &RcloneExitError{Code: 3, Output: "directory not found"}
		}
		prefix := fakeJoin(dir, "")
		var out []rcloneFile
		for key, data := range f.files {
			if name, ok := strings.CutPrefix(key, prefix); ok && !strings.Contains(name, "/") {
				out = append(out, rcloneFile{Name: name, Size: int64(len(data)), ModTime: f.mtime[key]})
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		raw, err := json.Marshal(out)
		if err != nil {
			return nil, err
		}
		return raw, nil
	case "cat":
		data, ok := f.files[args[len(args)-1]]
		if !ok {
			return nil, &RcloneExitError{Code: 3, Output: "not found"}
		}
		return data, nil
	case "deletefile":
		key := args[len(args)-1]
		if _, ok := f.files[key]; !ok {
			return nil, &RcloneExitError{Code: 4, Output: "object not found"}
		}
		delete(f.files, key)
		delete(f.mtime, key)
		return nil, nil
	}
	return nil, &RcloneExitError{Code: 1, Output: "unsupported subcommand " + args[0]}
}

// FakeAppdataPolicyStore is an in-memory AppdataPolicyStore for tests
// (CLAUDE.md).
type FakeAppdataPolicyStore struct {
	mu       sync.Mutex
	policies map[string]AppdataPolicy
}

var _ AppdataPolicyStore = (*FakeAppdataPolicyStore)(nil)

func (f *FakeAppdataPolicyStore) ListAppdataPolicies(ctx context.Context) ([]AppdataPolicy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]AppdataPolicy, 0, len(f.policies))
	for _, p := range f.policies {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Container < out[j].Container })
	return out, nil
}

func (f *FakeAppdataPolicyStore) SetAppdataPolicy(ctx context.Context, p AppdataPolicy, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.policies == nil {
		f.policies = map[string]AppdataPolicy{}
	}
	f.policies[p.Container] = p
	return nil
}

// FakeDrillStore is an in-memory DrillStore for tests (CLAUDE.md).
// RecordErr, when set, makes RecordDrill fail.
type FakeDrillStore struct {
	mu        sync.Mutex
	last      *DrillResult
	Recorded  int
	RecordErr error
}

var _ DrillStore = (*FakeDrillStore)(nil)

func (f *FakeDrillStore) RecordDrill(ctx context.Context, r DrillResult) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.RecordErr != nil {
		return f.RecordErr
	}
	f.Recorded++
	f.last = &r
	return nil
}

func (f *FakeDrillStore) LastDrill(ctx context.Context) (*DrillResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.last == nil {
		return nil, nil
	}
	r := *f.last
	return &r, nil
}
