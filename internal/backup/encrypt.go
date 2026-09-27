package backup

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"filippo.io/age"
)

// identitySidecarSuffix names the small, separately age-scrypt-encrypted
// file written alongside every encrypted archive (encryptedArtifacts).
// age's own spec refuses to combine a scrypt recipient stanza with any
// other recipient in one file ("an scrypt recipient must be the only
// one"), so "a restore needs only the passphrase" (Q80) can't be one
// age.Encrypt call to both the onboarding X25519 recipient and the
// passphrase — instead the main archive is encrypted to the recipient
// alone, and this sidecar, encrypted to the passphrase alone, carries the
// matching private identity: decrypt the sidecar with the passphrase,
// parse the recovered identity, then decrypt the main archive with it.
const identitySidecarSuffix = ".identity.age"

// buildIdentityAge wraps the onboarding recipient's private identity
// under the backup passphrase, through the exact same scrypt path
// buildSecretsAge already uses for secrets.age (Q80) — so identity.age is
// decryptable by the same code that opens secrets.age. Embedded inside
// the plaintext archive (BuildArchive), it lets a restore that has
// already opened the archive some other way recover the same onboarding
// keypair for future archives, rather than starting a new one. Omitted
// when no recipient exists yet or no passphrase is configured, the same
// fallback secrets.age itself uses: a restore without the passphrase
// restores everything except secrets and the recipient identity, and
// says so (doc 10 §1).
func buildIdentityAge(ctx context.Context, src SecretSource, recipient *Recipient) ([]byte, error) {
	if src == nil || recipient == nil || recipient.Identity == "" {
		return nil, nil
	}
	passphrase, ok, err := src.BackupPassphrase(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading backup passphrase for onboarding recipient: %w", err)
	}
	if !ok || passphrase == "" {
		return nil, nil
	}
	return encryptWithPassphrase([]byte(recipient.Identity), passphrase)
}

// encryptedArtifacts holds the two files a destination requiring
// encryption receives instead of the plaintext archive: Archive is the
// tar.zst age-encrypted to the onboarding recipient's public key, and
// IdentitySidecar is the matching private identity, age-scrypt-encrypted
// under the backup passphrase alone (identitySidecarSuffix). Both are
// temp files the caller (Service.Run) removes once written to every
// destination that needs them.
type encryptedArtifacts struct {
	ArchivePath  string
	SidecarPath  string
	SidecarBytes []byte
}

// buildEncryptedArtifacts produces encryptedArtifacts for archivePath:
// the archive itself age-encrypted to publicRecipient, and a sidecar
// carrying identity (already wrapped under passphrase by
// buildIdentityAge's same scrypt path) so a human holding only the
// passphrase can still recover the archive (Q80 — "a restore needs only
// the passphrase"): decrypt the sidecar with the passphrase to recover
// the identity, then decrypt the archive with that identity.
func buildEncryptedArtifacts(archivePath, publicRecipient string, identitySidecar []byte) (*encryptedArtifacts, error) {
	archiveAgePath, err := encryptArchiveForDestination(archivePath, publicRecipient)
	if err != nil {
		return nil, err
	}
	sidecarPath, err := writeIdentitySidecar(archiveAgePath, identitySidecar)
	if err != nil {
		_ = os.Remove(archiveAgePath)
		return nil, err
	}
	return &encryptedArtifacts{ArchivePath: archiveAgePath, SidecarPath: sidecarPath, SidecarBytes: identitySidecar}, nil
}

// encryptArchiveForDestination age-encrypts archivePath's plaintext
// tar.zst contents to the onboarding recipient's public X25519 key alone,
// writing the ciphertext to a new file at archivePath+".age" and
// returning its path.
func encryptArchiveForDestination(archivePath, publicRecipient string) (string, error) {
	xRecipient, err := age.ParseX25519Recipient(publicRecipient)
	if err != nil {
		return "", fmt.Errorf("parsing onboarding recipient: %w", err)
	}

	in, err := os.Open(archivePath)
	if err != nil {
		return "", fmt.Errorf("opening archive for encryption: %w", err)
	}
	defer func() { _ = in.Close() }()

	dest := archivePath + ".age"
	out, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+".*.tmp")
	if err != nil {
		return "", fmt.Errorf("creating encrypted archive temp file: %w", err)
	}
	tmp := out.Name()
	cleanup := func() {
		_ = out.Close()
		_ = os.Remove(tmp)
	}
	if err := out.Chmod(0o600); err != nil {
		cleanup()
		return "", fmt.Errorf("restricting encrypted archive temp file: %w", err)
	}

	w, err := age.Encrypt(out, xRecipient)
	if err != nil {
		cleanup()
		return "", fmt.Errorf("starting archive encryption: %w", err)
	}
	if _, err := io.Copy(w, in); err != nil {
		cleanup()
		return "", fmt.Errorf("encrypting archive: %w", err)
	}
	if err := w.Close(); err != nil {
		cleanup()
		return "", fmt.Errorf("finalizing archive encryption: %w", err)
	}
	if err := out.Sync(); err != nil {
		cleanup()
		return "", fmt.Errorf("syncing encrypted archive temp file: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("closing encrypted archive temp file: %w", err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("finalizing encrypted archive at %q: %w", dest, err)
	}
	if err := fsyncDir(filepath.Dir(dest)); err != nil {
		return "", err
	}
	return dest, nil
}

// writeIdentitySidecar writes sidecar (already age-scrypt-encrypted)
// next to archiveAgePath, atomically, returning its path.
func writeIdentitySidecar(archiveAgePath string, sidecar []byte) (string, error) {
	dest := archiveAgePath + identitySidecarSuffix
	dir := filepath.Dir(dest)
	out, err := os.CreateTemp(dir, filepath.Base(dest)+".*.tmp")
	if err != nil {
		return "", fmt.Errorf("creating identity sidecar temp file: %w", err)
	}
	tmp := out.Name()
	if err := out.Chmod(0o600); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return "", fmt.Errorf("restricting identity sidecar temp file: %w", err)
	}
	if _, err := out.Write(sidecar); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return "", fmt.Errorf("writing identity sidecar temp file: %w", err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return "", fmt.Errorf("syncing identity sidecar temp file: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("closing identity sidecar temp file: %w", err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("finalizing identity sidecar at %q: %w", dest, err)
	}
	if err := fsyncDir(dir); err != nil {
		return "", err
	}
	return dest, nil
}

// decryptIdentitySidecar reverses buildIdentityAge/writeIdentitySidecar:
// scrypt-decrypts sidecar under passphrase and returns the recovered
// private identity string.
func decryptIdentitySidecar(sidecar []byte, passphrase string) (string, error) {
	identity, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return "", fmt.Errorf("creating age scrypt identity: %w", err)
	}
	r, err := age.Decrypt(bytes.NewReader(sidecar), identity)
	if err != nil {
		return "", fmt.Errorf("decrypting identity sidecar: %w", err)
	}
	plain, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("reading decrypted identity sidecar: %w", err)
	}
	return string(plain), nil
}

// decryptArchiveWithPassphrase reverses buildEncryptedArtifacts using
// only the backup passphrase (Q80 — "a restore needs only the
// passphrase"): it opens sidecarPath to recover the private identity,
// then uses that identity to decrypt encryptedPath. Exercised directly
// by this package's own tests to prove the archive really is decryptable
// that way, not only that encryption ran.
func decryptArchiveWithPassphrase(encryptedPath, sidecarPath, passphrase string) ([]byte, error) {
	sidecar, err := os.ReadFile(sidecarPath)
	if err != nil {
		return nil, fmt.Errorf("reading identity sidecar: %w", err)
	}
	identityStr, err := decryptIdentitySidecar(sidecar, passphrase)
	if err != nil {
		return nil, err
	}
	identity, err := age.ParseX25519Identity(identityStr)
	if err != nil {
		return nil, fmt.Errorf("parsing recovered identity: %w", err)
	}
	return decryptArchive(encryptedPath, identity)
}

// decryptArchiveWithRecipient reverses encryptArchiveForDestination using
// the onboarding recipient's own private identity — the route the box
// itself would use to read back one of its own encrypted archives
// without needing the human passphrase or the sidecar file at all.
func decryptArchiveWithRecipient(encryptedPath string, recipient *Recipient) ([]byte, error) {
	identity, err := age.ParseX25519Identity(recipient.Identity)
	if err != nil {
		return nil, fmt.Errorf("parsing onboarding recipient identity: %w", err)
	}
	return decryptArchive(encryptedPath, identity)
}

func decryptArchive(encryptedPath string, identity age.Identity) ([]byte, error) {
	in, err := os.Open(encryptedPath)
	if err != nil {
		return nil, fmt.Errorf("opening encrypted archive: %w", err)
	}
	defer func() { _ = in.Close() }()

	r, err := age.Decrypt(in, identity)
	if err != nil {
		return nil, fmt.Errorf("decrypting archive: %w", err)
	}
	return io.ReadAll(r)
}
