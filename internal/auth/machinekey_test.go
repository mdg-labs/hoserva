package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLoadOrGenerateMachineKeyGeneratesOnce(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "secret.key")
	store := &FakeMachineKeyStore{}

	k1, err := LoadOrGenerateMachineKey(ctx, path, store)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode = %o, want 0600", perm)
	}
	if !store.Found {
		t.Fatal("expected a key-check value to be recorded after generating the first key")
	}

	// The same store instance stands in for the same underlying database
	// across the reload, exactly as a real restart would see it.
	k2, err := LoadOrGenerateMachineKey(ctx, path, store)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}

	plaintext := []byte("a secret column value")
	ciphertext, err := k1.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("encrypt with k1: %v", err)
	}
	got, err := k2.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("decrypt with k2 (reloaded key): %v", err)
	}
	if string(got) != string(plaintext) {
		t.Errorf("round trip = %q, want %q", got, plaintext)
	}
}

func TestMachineKeyEncryptDecryptRoundTrip(t *testing.T) {
	k, err := LoadOrGenerateMachineKey(context.Background(), filepath.Join(t.TempDir(), "secret.key"), &FakeMachineKeyStore{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, plaintext := range [][]byte{[]byte(""), []byte("x"), []byte("a longer totp secret value")} {
		ciphertext, err := k.Encrypt(plaintext)
		if err != nil {
			t.Fatalf("encrypt %q: %v", plaintext, err)
		}
		got, err := k.Decrypt(ciphertext)
		if err != nil {
			t.Fatalf("decrypt %q: %v", plaintext, err)
		}
		if string(got) != string(plaintext) {
			t.Errorf("round trip %q = %q", plaintext, got)
		}
	}
}

func TestMachineKeyDecryptTamperedCiphertextFails(t *testing.T) {
	k, err := LoadOrGenerateMachineKey(context.Background(), filepath.Join(t.TempDir(), "secret.key"), &FakeMachineKeyStore{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	ciphertext, err := k.Encrypt([]byte("secret"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	ciphertext[len(ciphertext)-1] ^= 0xFF
	if _, err := k.Decrypt(ciphertext); err == nil {
		t.Error("decrypting a tampered ciphertext must fail")
	}
}

func TestLoadOrGenerateMachineKeyRefusesReadableFile(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "secret.key")
	store := &FakeMachineKeyStore{}
	if _, err := LoadOrGenerateMachineKey(ctx, path, store); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	_, err := LoadOrGenerateMachineKey(ctx, path, store)
	if err == nil {
		t.Fatal("expected an error for a group/world-readable key file")
	}
}

// TestLoadOrGenerateMachineKeyRefusesWorldWritableDirectory is the review
// finding this issue closes: only the key file's own permissions were
// ever checked, never its containing directory's — a group- or
// world-writable directory lets any other account unlink and replace the
// key file, regardless of how tight the file's own mode is.
func TestLoadOrGenerateMachineKeyRefusesWorldWritableDirectory(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.key")
	store := &FakeMachineKeyStore{}
	if _, err := LoadOrGenerateMachineKey(ctx, path, store); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}

	_, err := LoadOrGenerateMachineKey(ctx, path, store)
	if !errors.Is(err, ErrKeyDirWritable) {
		t.Fatalf("LoadOrGenerateMachineKey with a world-writable key directory = %v, want ErrKeyDirWritable", err)
	}
}

func TestLoadOrGenerateMachineKeyRejectsWrongSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.key")
	if err := os.WriteFile(path, []byte("too short"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadOrGenerateMachineKey(context.Background(), path, &FakeMachineKeyStore{}); err == nil {
		t.Fatal("expected an error for a wrong-size key file")
	}
}

// TestLoadOrGenerateMachineKeyRefusesMissingKeyWithExistingCheckValue is
// the review finding this issue closes: a lost or replaced key file must
// be a fatal startup error, never a silently regenerated replacement that
// can decrypt nothing already encrypted under the real one.
func TestLoadOrGenerateMachineKeyRefusesMissingKeyWithExistingCheckValue(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "secret.key")
	store := &FakeMachineKeyStore{}
	if _, err := LoadOrGenerateMachineKey(ctx, path, store); err != nil {
		t.Fatalf("generate: %v", err)
	}

	// Simulate the key file going missing (deleted, restored from an
	// unrelated backup, ...) while the database — and its check value —
	// survive.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}

	_, err := LoadOrGenerateMachineKey(ctx, path, store)
	if !errors.Is(err, ErrMachineKeyMismatch) {
		t.Fatalf("LoadOrGenerateMachineKey with a missing key and an existing check value = %v, want ErrMachineKeyMismatch", err)
	}

	// The daemon must not have quietly written a replacement key either.
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("a fatal mismatch must not leave a freshly generated replacement key on disk")
	}
}

// TestLoadOrGenerateMachineKeyMissingKeyMessageReadsDifferentlyFromAMismatchedOne
// is the review finding this issue closes: a missing key file's own
// error used to end with "...: machine key does not match this
// installation's stored key-check value" — ErrMachineKeyMismatch's own
// text, appended by the %w that wraps it — even though nothing was
// actually compared, because there was no file to compare. A caller
// reading only the message's own words, not its Go type, would be told a
// comparison happened when it didn't. Both are still ErrMachineKeyMismatch
// (the caller-facing classification doesn't change: either way, startup
// must refuse), but only the message for a key file that is actually
// present and wrong may say "does not match" anywhere in it.
func TestLoadOrGenerateMachineKeyMissingKeyMessageReadsDifferentlyFromAMismatchedOne(t *testing.T) {
	ctx := context.Background()

	missingPath := filepath.Join(t.TempDir(), "secret.key")
	missingStore := &FakeMachineKeyStore{Secrets: true}
	_, missingErr := LoadOrGenerateMachineKey(ctx, missingPath, missingStore)
	if !errors.Is(missingErr, ErrMachineKeyMismatch) {
		t.Fatalf("missing key file = %v, want ErrMachineKeyMismatch", missingErr)
	}
	if strings.Contains(missingErr.Error(), "does not match") {
		t.Errorf("missing-key error = %q, must not claim a comparison anywhere in its text when there was no key file to compare at all", missingErr)
	}
	if !strings.Contains(missingErr.Error(), "no machine key file exists") {
		t.Errorf("missing-key error = %q, want it to say the key file is missing", missingErr)
	}

	mismatchPath := filepath.Join(t.TempDir(), "secret.key")
	mismatchStore := &FakeMachineKeyStore{}
	if _, err := LoadOrGenerateMachineKey(ctx, mismatchPath, mismatchStore); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if err := os.WriteFile(mismatchPath, make([]byte, keySize), 0o600); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	_, mismatchErr := LoadOrGenerateMachineKey(ctx, mismatchPath, mismatchStore)
	if !errors.Is(mismatchErr, ErrMachineKeyMismatch) {
		t.Fatalf("mismatched key file = %v, want ErrMachineKeyMismatch", mismatchErr)
	}
	if !strings.Contains(mismatchErr.Error(), "does not match") {
		t.Errorf("mismatched-key error = %q, want it to still say so somewhere (a key file is actually present and wrong, so the comparison this claims did happen)", mismatchErr)
	}

	if missingErr.Error() == mismatchErr.Error() {
		t.Errorf("missing-key and mismatched-key errors must read differently; both were %q", missingErr.Error())
	}
}

func TestLoadOrGenerateMachineKeyRefusesMismatchedExistingKeyFile(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "secret.key")
	store := &FakeMachineKeyStore{}
	if _, err := LoadOrGenerateMachineKey(ctx, path, store); err != nil {
		t.Fatalf("generate: %v", err)
	}

	// Swap in a different, but still well-formed, key file — simulating a
	// wrong key restored over the real one.
	other := make([]byte, keySize)
	if err := os.WriteFile(path, other, 0o600); err != nil {
		t.Fatalf("overwrite: %v", err)
	}

	_, err := LoadOrGenerateMachineKey(ctx, path, store)
	if !errors.Is(err, ErrMachineKeyMismatch) {
		t.Fatalf("LoadOrGenerateMachineKey with a mismatched key file = %v, want ErrMachineKeyMismatch", err)
	}
}

func TestLoadOrGenerateMachineKeyRefusesGeneratingOverExistingSecrets(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "secret.key")
	store := &FakeMachineKeyStore{Secrets: true}

	_, err := LoadOrGenerateMachineKey(ctx, path, store)
	if !errors.Is(err, ErrMachineKeyMismatch) {
		t.Fatalf("LoadOrGenerateMachineKey with no key file but existing encrypted secrets = %v, want ErrMachineKeyMismatch", err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("must not generate a key when the database already holds encrypted secrets")
	}
}

// TestLoadOrGenerateMachineKeyLeavesExactlyOneKeyFileWithLinkCountOne is
// the review finding this issue closes: after os.Link succeeded,
// writeKeyFileAtomically's own deferred cleanup used to skip removing the
// temp file it linked from, leaving a second, hidden hard link
// (.secret.key.tmp-*) to the same key material behind after every fresh
// generation — confirmed live at link count 2, with the key still
// recoverable under the hidden name even after the real path was deleted.
// TestLoadOrGenerateMachineKeyNeverOverwritesAnExistingKeyFile already
// covered the *load* path's own no-stray-files property; this covers the
// *generate* path, which that test never exercises.
func TestLoadOrGenerateMachineKeyLeavesExactlyOneKeyFileWithLinkCountOne(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.key")
	store := &FakeMachineKeyStore{}

	if _, err := LoadOrGenerateMachineKey(ctx, path, store); err != nil {
		t.Fatalf("generate: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("directory has %d entries after generating a key, want exactly 1 (no leftover .secret.key.tmp-*): %v", len(entries), names)
	}
	if entries[0].Name() != "secret.key" {
		t.Errorf("the one directory entry is %q, want secret.key", entries[0].Name())
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("expected a *syscall.Stat_t from FileInfo.Sys() on this platform")
	}
	if stat.Nlink != 1 {
		t.Errorf("secret.key has link count %d, want 1 (a hidden second hard link under the temp name would leave it at 2)", stat.Nlink)
	}
}

// fakeKeyFileInfo is an os.FileInfo whose Sys() returns a *syscall.Stat_t
// with a caller-chosen owner uid, so checkKeyOwner can be tested against
// ownership values a test can't otherwise arrange without root (chown-ing
// a real file to some other account).
type fakeKeyFileInfo struct{ stat syscall.Stat_t }

func (f fakeKeyFileInfo) Name() string       { return "secret.key" }
func (f fakeKeyFileInfo) Size() int64        { return int64(keySize) }
func (f fakeKeyFileInfo) Mode() os.FileMode  { return 0o600 }
func (f fakeKeyFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeKeyFileInfo) IsDir() bool        { return false }
func (f fakeKeyFileInfo) Sys() any           { return &f.stat }

// TestCheckKeyOwnerRefusesAnUnexpectedOwner is the review finding this
// issue closes: Q28's "root" owner was never actually checked — mode
// 0600 alone doesn't prove that, since root can read a 0600 file
// regardless of who owns it.
func TestCheckKeyOwnerRefusesAnUnexpectedOwner(t *testing.T) {
	other := uint32(os.Geteuid()) + 12345 // deliberately neither 0 nor our own euid.
	if err := checkKeyOwner("secret.key", fakeKeyFileInfo{stat: syscall.Stat_t{Uid: other}}); err == nil {
		t.Error("expected an error for a key file owned by neither root nor this process")
	}
}

func TestCheckKeyOwnerAcceptsRootOwner(t *testing.T) {
	if err := checkKeyOwner("secret.key", fakeKeyFileInfo{stat: syscall.Stat_t{Uid: 0}}); err != nil {
		t.Errorf("a root-owned key file should be accepted: %v", err)
	}
}

func TestCheckKeyOwnerAcceptsThisProcesssOwnEuid(t *testing.T) {
	if err := checkKeyOwner("secret.key", fakeKeyFileInfo{stat: syscall.Stat_t{Uid: uint32(os.Geteuid())}}); err != nil {
		t.Errorf("a key file owned by this process's own euid (a dev run standing in for root) should be accepted: %v", err)
	}
}

func TestLoadOrGenerateMachineKeyNeverOverwritesAnExistingKeyFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.key")
	original := make([]byte, keySize)
	for i := range original {
		original[i] = byte(i)
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	store := &FakeMachineKeyStore{}
	if _, err := LoadOrGenerateMachineKey(ctx, path, store); err != nil {
		t.Fatalf("load: %v", err)
	}

	// No temp files were left behind after a successful load of an
	// already-existing key (the atomic-write path was never taken).
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("directory has %d entries after loading an existing key, want exactly 1 (no stray temp files)", len(entries))
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(original) {
		t.Error("loading an existing key file must never modify its contents")
	}
}
