// Package auth implements the primitives issue #22 needs — password
// hashing, TOTP, sessions, the machine key, rate limiting, the LAN source
// filter and Unix-socket peer-credential checks — kept independent of
// internal/api so each has its own focused tests. internal/api wires these
// into the generated server interfaces (D18); nothing here talks to the
// generated API types.
package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// keySize is AES-256's key size — Q28 names AES-256-GCM specifically.
const keySize = 32

// keyCheckConstant is the fixed plaintext LoadOrGenerateMachineKey's HMAC
// check value is computed over (see MachineKeyStore below) — its value
// carries no meaning of its own; only whether the stored check value
// still matches it under a candidate key does.
const keyCheckConstant = "hoserva-machine-key-check-v1"

// MachineKey encrypts secret columns at rest (Q28) with AES-256-GCM. It
// protects the database file leaking (a diagnostics bundle, a copied
// backup) — not backups themselves, which re-encrypt secrets under a
// separate user-set backup passphrase (doc 10 §1), out of this issue's
// scope.
type MachineKey struct {
	aead cipher.AEAD
	raw  []byte
}

// ErrKeyReadable is returned when the machine key file is readable by
// anyone but its owner — Q28's "root, 0600" is a hard requirement, not a
// preference, since this key is the only thing standing between a leaked
// database file and every secret column in it.
var ErrKeyReadable = fmt.Errorf("machine key file must not be group- or world-readable (mode 0600)")

// ErrKeyDirWritable is returned when the machine key's own directory is
// writable by anyone but its owner. The key file's own 0600 mode
// (ErrKeyReadable) doesn't mean much if any other account can unlink it
// and replace it with one of their own choosing by writing into the
// directory that holds it — a review finding: only the key file's own
// permissions were ever checked, never its directory's.
var ErrKeyDirWritable = fmt.Errorf("machine key directory must not be group- or world-writable")

// ErrMachineKeyMismatch is returned by LoadOrGenerateMachineKey whenever
// the key at path cannot be trusted to be the one already protecting this
// installation's encrypted secrets — a missing key file with a stored
// check value (or pre-existing encrypted secrets) already on record, or a
// present key file whose check value doesn't match. Either case must be a
// fatal startup error, never a silent regeneration: a lost or replaced
// key permanently locks every encrypted secret (TOTP included) away from
// its own account, with no silent fix — see
// docs/internal/13-open-questions.md's Q28 entry, "Recovery", before
// replacing a key file this error names.
//
// Its own text is deliberately neutral rather than "does not match": a
// missing key file is wrapped under this sentinel too (generateMachineKey,
// below), and there is nothing to compare in that case — a review finding
// closed by this comment and the wording change together. Where a
// comparison actually happened (verifyOrRecordKeyCheckValue), the
// wrapping message says so itself, in its own literal text, not by
// borrowing this one's.
var ErrMachineKeyMismatch = errors.New("machine key check failed")

// MachineKeyStore is where LoadOrGenerateMachineKey persists and reads
// the key-check value (Q28), and where it confirms no secret column
// already holds data before it is willing to write a brand new key.
// Behind an interface, not internal/store/db's generated Queries
// directly, so this package stays independent of internal/api and
// internal/store (mirroring GroupLookup and every other system-touching
// interface CLAUDE.md asks for), and so tests can exercise "no check
// value yet", "a mismatched one" and "secrets already exist" without a
// real database.
type MachineKeyStore interface {
	// KeyCheckValue returns the stored check value, and found=false if
	// none has ever been written (a fresh install, before its first
	// machine key exists).
	KeyCheckValue(ctx context.Context) (value []byte, found bool, err error)
	// SetKeyCheckValue writes value the moment a machine key is first
	// generated. LoadOrGenerateMachineKey never calls this a second time
	// for the same installation.
	SetKeyCheckValue(ctx context.Context, value []byte) error
	// HasEncryptedSecrets reports whether any secret column already
	// holds data protected by some machine key — checked in addition to
	// KeyCheckValue so an installation whose check value was somehow
	// never written still refuses to have its key silently regenerated
	// out from under real, already-encrypted data.
	HasEncryptedSecrets(ctx context.Context) (bool, error)
}

// LoadOrGenerateMachineKey reads a 32-byte key from path, generating one
// — atomically, and only once per installation — the first time path
// doesn't exist. Production points path at /etc/hoserva/secret.key (root,
// 0600 — Q28); this function itself never assumes that location, and
// dev/test callers pass a workspace or temp path instead — CLAUDE.md:
// never touch /etc/hoserva from a dev run.
//
// Every call — whether it loads an existing key or generates a fresh one
// — is checked against store's key-check value (ErrMachineKeyMismatch):
// a key file that goes missing (deleted, restored from an unrelated
// backup, a wrong path) must never be quietly replaced with a new one
// that can decrypt nothing already encrypted under the real one.
func LoadOrGenerateMachineKey(ctx context.Context, path string, store MachineKeyStore) (*MachineKey, error) {
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := checkKeyPermissions(path); err != nil {
			return nil, err
		}
		if len(raw) != keySize {
			return nil, fmt.Errorf("machine key at %s is %d bytes, want %d", path, len(raw), keySize)
		}
		key, err := newMachineKey(raw)
		if err != nil {
			return nil, err
		}
		if err := verifyOrRecordKeyCheckValue(ctx, store, key, path); err != nil {
			return nil, err
		}
		return key, nil
	case os.IsNotExist(err):
		return generateMachineKey(ctx, path, store)
	default:
		return nil, fmt.Errorf("reading machine key at %s: %w", path, err)
	}
}

// generateMachineKey is the "path doesn't exist yet" branch of
// LoadOrGenerateMachineKey: it refuses outright if store shows this
// installation already has a key-check value or an encrypted secret on
// record (a missing key file is then a lost key, not a fresh install),
// then writes a fresh key atomically — a temp file, fsynced, then linked
// into place, which fails closed with EEXIST rather than silently
// overwriting anything a concurrent caller (or a restored real key) got
// there first.
func generateMachineKey(ctx context.Context, path string, store MachineKeyStore) (*MachineKey, error) {
	hasSecrets, err := store.HasEncryptedSecrets(ctx)
	if err != nil {
		return nil, fmt.Errorf("checking for existing encrypted secrets: %w", err)
	}
	if hasSecrets {
		return nil, fmt.Errorf("%w: no machine key file exists at %s, but the database already holds encrypted secrets — generating a new key now would permanently lock every one of them away from itself; see docs/internal/13-open-questions.md's Q28 entry, \"Recovery\", for the exact steps", ErrMachineKeyMismatch, path)
	}
	if _, found, err := store.KeyCheckValue(ctx); err != nil {
		return nil, fmt.Errorf("checking for an existing machine key check value: %w", err)
	} else if found {
		return nil, fmt.Errorf("%w: no machine key file exists at %s, but this installation already has a stored key-check value — the real key is missing, not never generated; see docs/internal/13-open-questions.md's Q28 entry, \"Recovery\", before generating a replacement", ErrMachineKeyMismatch, path)
	}

	raw := make([]byte, keySize)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generating machine key: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating machine key directory: %w", err)
	}
	// MkdirAll only sets 0700 on a directory it actually creates — an
	// operator-provisioned directory that already existed with looser
	// permissions is checked, not assumed, exactly like the key file
	// itself (checkKeyPermissions's own directory check, below).
	if err := checkKeyDirPermissions(dir); err != nil {
		return nil, err
	}
	if err := writeKeyFileAtomically(path, raw); err != nil {
		return nil, err
	}

	key, err := newMachineKey(raw)
	if err != nil {
		return nil, err
	}
	if err := store.SetKeyCheckValue(ctx, computeKeyCheckValue(key)); err != nil {
		return nil, fmt.Errorf("writing machine key check value: %w", err)
	}
	return key, nil
}

// writeKeyFileAtomically writes raw to path without ever overwriting an
// existing file there: a temp file in the same directory (so the final
// link is on one filesystem), 0600 before any data reaches it, fsynced
// and closed, then linked — not renamed — into place. os.Link fails with
// EEXIST if path already exists, atomically, exactly where os.Rename
// would have silently clobbered it (two daemons racing to generate a key
// at first start, or an operator's real key restored moments after a
// stray one was created).
//
// The temp name is removed once the link succeeds, and the directory is
// fsynced afterward: os.Link leaves two hard links to the same inode, one
// at path and one still at the hidden temp name, and a defer that only
// fires on the *failure* path (as this used to) never removes it on
// success — every fresh key generation left a second, hidden copy of the
// key material behind (confirmed live: link count 2, the tmp name still
// holding the key even after path itself was deleted or rotated), doubled
// in every backup of the key's directory.
func writeKeyFileAtomically(path string, raw []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".secret.key.tmp-*")
	if err != nil {
		return fmt.Errorf("creating temporary machine key file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanedUp := false
	defer func() {
		if !cleanedUp {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("setting temporary machine key file permissions: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing temporary machine key file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("syncing temporary machine key file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temporary machine key file: %w", err)
	}
	if err := os.Link(tmpPath, path); err != nil {
		return fmt.Errorf("linking generated machine key into place at %s: %w", path, err)
	}
	if err := os.Remove(tmpPath); err != nil {
		return fmt.Errorf("removing temporary machine key file %s after linking it into place: %w", tmpPath, err)
	}
	cleanedUp = true
	if err := fsyncDir(dir); err != nil {
		return err
	}
	return nil
}

// fsyncDir fsyncs dir itself, so the link/unlink that just replaced its
// one visible entry (the temp name) with the final one (path) survives a
// crash immediately after — not only the key file's own data, already
// fsynced before the link.
func fsyncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("opening %s for fsync: %w", dir, err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("syncing directory %s: %w", dir, err)
	}
	return nil
}

// verifyOrRecordKeyCheckValue checks key against store's own record of
// what the right key looks like for this installation. A brand new
// install — no check value yet, and nothing encrypted yet either — is
// the one case that writes one rather than refusing, so a key file that
// happens to already exist (an operator pre-provisioning one before
// first start) is adopted rather than rejected.
func verifyOrRecordKeyCheckValue(ctx context.Context, store MachineKeyStore, key *MachineKey, path string) error {
	value, found, err := store.KeyCheckValue(ctx)
	if err != nil {
		return fmt.Errorf("reading machine key check value: %w", err)
	}
	if !found {
		hasSecrets, err := store.HasEncryptedSecrets(ctx)
		if err != nil {
			return fmt.Errorf("checking for existing encrypted secrets: %w", err)
		}
		if hasSecrets {
			return fmt.Errorf("%w: %s has no recorded key-check value yet, but the database already holds encrypted secrets — this looks like a key mismatch, not a fresh install; see docs/internal/13-open-questions.md's Q28 entry, \"Recovery\", for the exact steps", ErrMachineKeyMismatch, path)
		}
		return store.SetKeyCheckValue(ctx, computeKeyCheckValue(key))
	}
	if !hmac.Equal(value, computeKeyCheckValue(key)) {
		return fmt.Errorf("%w: %s does not match this installation's stored key-check value — see docs/internal/13-open-questions.md's Q28 entry, \"Recovery\", before replacing it", ErrMachineKeyMismatch, path)
	}
	return nil
}

// computeKeyCheckValue is an HMAC-SHA256 of keyCheckConstant under key's
// raw bytes — never the key itself, and not reversible back to it.
func computeKeyCheckValue(key *MachineKey) []byte {
	mac := hmac.New(sha256.New, key.raw)
	mac.Write([]byte(keyCheckConstant))
	return mac.Sum(nil)
}

// checkKeyPermissions refuses a key file that is group- or world-readable
// (ErrKeyReadable), owned by neither root nor this process, or sitting in
// a group- or world-writable directory — doc 01 §7's "root, 0600"
// enforced, not just documented.
func checkKeyPermissions(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat machine key at %s: %w", path, err)
	}
	if info.Mode().Perm()&(fs.ModePerm&0o077) != 0 {
		return fmt.Errorf("%s: %w", path, ErrKeyReadable)
	}
	if err := checkKeyOwner(path, info); err != nil {
		return err
	}
	if err := checkKeyDirPermissions(filepath.Dir(path)); err != nil {
		return err
	}
	return nil
}

// checkKeyDirPermissions refuses a machine key directory that is group-
// or world-writable (ErrKeyDirWritable) — the key file's own 0600 mode
// means little if any other account can unlink and replace it by writing
// into the directory that holds it.
func checkKeyDirPermissions(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("stat machine key directory %s: %w", dir, err)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s: %w", dir, ErrKeyDirWritable)
	}
	return nil
}

// checkKeyOwner refuses a key file owned by an account that is neither
// root nor this process's own euid (a dev run's own uid stands in for
// root there, since nothing here may create a system user or chown to
// one — CLAUDE.md). Mode 0600 alone doesn't prove "root, 0600" (Q28): a
// key file copied or restored from elsewhere could still be owned by some
// other, unprivileged account — root itself can always read it regardless
// of who owns it, so the permission check above would pass either way,
// but ownership by an unexpected account is not what "root, 0600" means
// and is refused outright rather than silently trusted. See
// docs/internal/13-open-questions.md's Q28 entry, "Recovery", for what
// this is blocking startup for.
func checkKeyOwner(path string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil // best-effort: not every platform exposes this.
	}
	euid := uint32(os.Geteuid())
	if stat.Uid != 0 && stat.Uid != euid {
		return fmt.Errorf("machine key at %s is owned by uid %d, neither root nor this process's own uid (%d) — see docs/internal/13-open-questions.md's Q28 entry, \"Recovery\", before changing its ownership", path, stat.Uid, euid)
	}
	return nil
}

func newMachineKey(raw []byte) (*MachineKey, error) {
	if len(raw) != keySize {
		return nil, fmt.Errorf("machine key must be %d bytes, got %d", keySize, len(raw))
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, fmt.Errorf("building AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("building AES-GCM: %w", err)
	}
	return &MachineKey{aead: aead, raw: append([]byte(nil), raw...)}, nil
}

// Encrypt returns nonce||ciphertext, sealed with a fresh random nonce.
func (k *MachineKey) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generating nonce: %w", err)
	}
	return k.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt reverses Encrypt.
func (k *MachineKey) Decrypt(ciphertext []byte) ([]byte, error) {
	nonceSize := k.aead.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("ciphertext shorter than nonce (%d bytes)", nonceSize)
	}
	nonce, sealed := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := k.aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypting: %w", err)
	}
	return plaintext, nil
}

// FakeMachineKeyStore is a scriptable MachineKeyStore for tests
// (CLAUDE.md) — no database involved. A caller shares one instance
// across two LoadOrGenerateMachineKey calls to simulate the check value
// persisting across a daemon restart, the same way a real database would.
type FakeMachineKeyStore struct {
	Value   []byte
	Found   bool
	Secrets bool
}

var _ MachineKeyStore = (*FakeMachineKeyStore)(nil)

func (f *FakeMachineKeyStore) KeyCheckValue(ctx context.Context) ([]byte, bool, error) {
	return f.Value, f.Found, nil
}

func (f *FakeMachineKeyStore) SetKeyCheckValue(ctx context.Context, value []byte) error {
	f.Value = value
	f.Found = true
	return nil
}

func (f *FakeMachineKeyStore) HasEncryptedSecrets(ctx context.Context) (bool, error) {
	return f.Secrets, nil
}
