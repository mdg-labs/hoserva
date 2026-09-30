package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Paths names every on-disk source the archive builder reads. Callers in
// tests and dev runs point these at temp directories — never /etc/hoserva
// or /var/lib/hoserva directly (CLAUDE.md).
type Paths struct {
	StateDir            string
	ConfigRoot          string
	DBPath              string
	StacksDir           string
	TemplatesDir        string
	SnapraidContentPath string
	// MachineKeyPath is the machine key file hoservad loaded (Q28). Whatever
	// the archive would otherwise copy, this file never enters it.
	MachineKeyPath string
}

// ArchiveOption configures an optional part of BuildArchive's output.
// A variadic trailing parameter, rather than growing BuildArchive's fixed
// argument list, so every existing caller keeps compiling unchanged. Both
// production callers — Service.Run (the nightly config-backup chain) and
// internal/api/pool_handler.go's on-demand ExportConfig — pass
// WithRecipient, so every archive carries identity.age (criterion 3)
// whether it is written to a destination or downloaded directly.
type ArchiveOption func(*archiveOptions)

type archiveOptions struct {
	recipient   *Recipient
	sealSecrets string
	hostRoot    string
	hostFiles   []string
}

// hostFilesDir is the archive directory holding the host files a restore is
// about to replace, each at its path relative to the host's config root. It
// is written only into the pre-import archive of a bare-metal restore; no
// restore reads it back (the files it holds are the new host's, not the
// configuration being restored), so it is there for a user to recover from.
const hostFilesDir = "host"

// WithHostFiles copies each of rels, relative to root, into the archive's
// host/ directory. A file that cannot be read fails the build: the caller is
// about to replace it, so a copy that is not there must stop it.
func WithHostFiles(root string, rels []string) ArchiveOption {
	return func(o *archiveOptions) {
		o.hostRoot = root
		o.hostFiles = rels
	}
}

// WithSecretsPassphrase seals secrets.age under passphrase instead of the
// configured backup passphrase (Q80), so an archive is built with secrets
// when none is configured. Empty, it changes nothing.
func WithSecretsPassphrase(passphrase string) ArchiveOption {
	return func(o *archiveOptions) { o.sealSecrets = passphrase }
}

// WithRecipient attaches the onboarding recipient (Q80) so BuildArchive
// wraps its private identity into identity.age. Omitted (or recipient
// nil), an archive simply carries no identity.age — the same
// "restores everything except this" fallback secrets.age already uses
// when no passphrase is configured.
func WithRecipient(recipient *Recipient) ArchiveOption {
	return func(o *archiveOptions) { o.recipient = recipient }
}

// BuildArchive assembles doc 10 §1's layout in stagingDir and returns the
// manifest checksum map. stagingDir must already exist.
func BuildArchive(ctx context.Context, db *sql.DB, paths Paths, src SecretSource, cipher SecretCipher, host, version string, now time.Time, stagingDir string, opts ...ArchiveOption) (Manifest, error) {
	var cfg archiveOptions
	for _, opt := range opts {
		opt(&cfg)
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}

	stateDB := filepath.Join(stagingDir, "state.db")
	if err := vacuumInto(ctx, db, stateDB); err != nil {
		return Manifest{}, fmt.Errorf("snapshotting database: %w", err)
	}

	stackEnvs, err := collectStackEnvs(paths.StacksDir)
	if err != nil {
		return Manifest{}, err
	}
	if err := copyStacksComposeOnly(paths.StacksDir, filepath.Join(stagingDir, "stacks")); err != nil {
		return Manifest{}, err
	}

	secrets, err := buildSecretsAge(ctx, src, cipher, stackEnvs, cfg.sealSecrets)
	if err != nil {
		return Manifest{}, err
	}
	if len(secrets) > 0 {
		if err := os.WriteFile(filepath.Join(stagingDir, "secrets.age"), secrets, 0o600); err != nil {
			return Manifest{}, fmt.Errorf("writing secrets.age: %w", err)
		}
	}

	identity, err := buildIdentityAge(ctx, src, cfg.recipient)
	if err != nil {
		return Manifest{}, err
	}
	if len(identity) > 0 {
		if err := os.WriteFile(filepath.Join(stagingDir, "identity.age"), identity, 0o600); err != nil {
			return Manifest{}, fmt.Errorf("writing identity.age: %w", err)
		}
	}

	keys, err := newMachineKeyFilter(paths.MachineKeyPath)
	if err != nil {
		return Manifest{}, err
	}

	if err := copyTreeIfExists(paths.ConfigRoot, filepath.Join(stagingDir, "generated"), isGeneratedFile, keys); err != nil {
		return Manifest{}, err
	}
	if err := copyTreeIfExists(paths.ConfigRoot, filepath.Join(stagingDir, "custom"), isCustomFile, keys); err != nil {
		return Manifest{}, err
	}
	if err := copyTreeIfExists(paths.TemplatesDir, filepath.Join(stagingDir, "templates"), nil, keys); err != nil {
		return Manifest{}, err
	}
	for _, rel := range cfg.hostFiles {
		if !filepath.IsLocal(rel) {
			return Manifest{}, fmt.Errorf("host file %q is not inside the host's config root", rel)
		}
		hostFile := filepath.Join(cfg.hostRoot, rel)
		if isKey, err := keys.isMachineKey(hostFile); err != nil {
			return Manifest{}, fmt.Errorf("saving host file %s: %w", rel, err)
		} else if isKey {
			return Manifest{}, fmt.Errorf("host file %q is the machine key, which an archive never holds", rel)
		}
		if err := copyPath(filepath.Join(stagingDir, hostFilesDir, rel), hostFile); err != nil {
			return Manifest{}, fmt.Errorf("saving host file %s: %w", rel, err)
		}
	}
	if err := copySnapraidContent(paths.SnapraidContentPath, filepath.Join(stagingDir, "snapraid-content"), keys); err != nil {
		return Manifest{}, err
	}

	checksums := map[string]string{}
	files, err := listArchiveFiles(stagingDir)
	if err != nil {
		return Manifest{}, err
	}
	for _, rel := range files {
		sum, err := hashFile(filepath.Join(stagingDir, rel))
		if err != nil {
			return Manifest{}, fmt.Errorf("hashing %s: %w", rel, err)
		}
		checksums[rel] = sum
	}

	manifest := buildManifest(host, version, now, checksums)
	if err := writeManifest(filepath.Join(stagingDir, "manifest.json"), manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// machineKeyFileName is the machine key's file name in the packaged layout.
// A file of that name is left out wherever it is found, so a caller that
// leaves Paths.MachineKeyPath empty still cannot put the key in an archive.
const machineKeyFileName = "secret.key"

// machineKeyFilter decides, for each file the builder is about to copy,
// whether it is the machine key. The configured key is matched by identity
// (os.SameFile on the followed file), not by spelling, so a symlink or hard
// link to it, or another path to the same file, is left out too.
type machineKeyFilter struct {
	key os.FileInfo
}

func newMachineKeyFilter(path string) (*machineKeyFilter, error) {
	if path == "" {
		return &machineKeyFilter{}, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &machineKeyFilter{}, nil
		}
		return nil, fmt.Errorf("checking machine key %s: %w", path, err)
	}
	return &machineKeyFilter{key: info}, nil
}

// isMachineKey errors when path cannot be examined: a file that might be the
// key is never copied on the strength of a failed comparison.
func (f *machineKeyFilter) isMachineKey(path string) (bool, error) {
	if filepath.Base(path) == machineKeyFileName {
		return true, nil
	}
	if f.key == nil {
		return false, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return false, fmt.Errorf("checking %s against the machine key: %w", path, err)
	}
	return os.SameFile(f.key, info), nil
}

func isGeneratedFile(rel string) bool {
	base := filepath.Base(rel)
	if strings.HasPrefix(base, ".") {
		return false
	}
	if base == "smb.custom.conf" || strings.HasSuffix(base, ".custom.conf") {
		return false
	}
	return true
}

func isCustomFile(rel string) bool {
	base := filepath.Base(rel)
	return strings.HasSuffix(base, ".custom.conf")
}

func listArchiveFiles(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "manifest.json" {
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	return out, err
}

func copyTreeIfExists(src, dst string, include func(rel string) bool, keys *machineKeyFilter) error {
	if src == "" {
		return nil
	}
	absSrc, err := filepath.Abs(src)
	if err != nil {
		return fmt.Errorf("resolving source path %q: %w", src, err)
	}
	absDst, err := filepath.Abs(dst)
	if err != nil {
		return fmt.Errorf("resolving destination path %q: %w", dst, err)
	}
	if absSrc == absDst || strings.HasPrefix(absDst, absSrc+string(os.PathSeparator)) {
		return fmt.Errorf("copy destination %q is inside source %q", dst, src)
	}
	info, err := os.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("checking %s: %w", src, err)
	}
	if !info.IsDir() {
		if include != nil && !include(filepath.Base(src)) {
			return nil
		}
		if isKey, err := keys.isMachineKey(src); err != nil || isKey {
			return err
		}
		return copyPath(filepath.Join(dst, filepath.Base(src)), src)
	}

	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if include != nil && !include(rel) {
			return nil
		}
		if isKey, err := keys.isMachineKey(path); err != nil || isKey {
			return err
		}
		return copyPath(filepath.Join(dst, rel), path)
	})
}

func copySnapraidContent(src, dstDir string, keys *machineKeyFilter) error {
	if src == "" {
		return nil
	}
	info, err := os.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("checking snapraid content %s: %w", src, err)
	}
	if info.IsDir() {
		return copyTreeIfExists(src, dstDir, nil, keys)
	}
	if isKey, err := keys.isMachineKey(src); err != nil || isKey {
		return err
	}
	if err := os.MkdirAll(dstDir, 0o700); err != nil {
		return err
	}
	return copyPath(filepath.Join(dstDir, filepath.Base(src)), src)
}

func collectStackEnvs(stacksDir string) ([]StackEnv, error) {
	var out []StackEnv
	info, err := os.Stat(stacksDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("checking stacks directory: %w", err)
	}
	if !info.IsDir() {
		return nil, nil
	}
	entries, err := os.ReadDir(stacksDir)
	if err != nil {
		return nil, fmt.Errorf("listing stacks: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		envPath := filepath.Join(stacksDir, e.Name(), ".env")
		body, err := os.ReadFile(envPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("reading %s: %w", envPath, err)
		}
		out = append(out, StackEnv{Stack: e.Name(), Body: body})
	}
	return out, nil
}

func copyStacksComposeOnly(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("checking stacks directory: %w", err)
	}
	if !info.IsDir() {
		return nil
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("listing stacks: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		stackSrc := filepath.Join(src, e.Name())
		stackDst := filepath.Join(dst, e.Name())
		if err := os.MkdirAll(stackDst, 0o700); err != nil {
			return err
		}
		for _, name := range []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml", "meta.json"} {
			srcFile := filepath.Join(stackSrc, name)
			if _, err := os.Stat(srcFile); err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return err
			}
			if err := copyPath(filepath.Join(stackDst, name), srcFile); err != nil {
				return fmt.Errorf("copying %s: %w", name, err)
			}
		}
	}
	return nil
}
