package template

import (
	"archive/tar"
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

var (
	// ErrBadSignature is returned when a catalog archive's detached
	// signature does not verify against the catalog key.
	ErrBadSignature = errors.New("template: the catalog archive's signature does not verify")
	// ErrNotNewer is returned when a signed archive's serial is not higher
	// than the installed catalog's, so an older signed catalog cannot be
	// replayed (doc 04 §7).
	ErrNotNewer = errors.New("template: the catalog archive is not newer than the installed catalog")
	// ErrBadArchive is returned when a signed archive is not a catalog:
	// not zstd or tar, an entry outside index.json and <id>/, an entry type
	// other than a plain file or directory, or a missing or unreadable
	// index.json.
	ErrBadArchive = errors.New("template: the catalog archive is malformed")
)

// maxCatalogBytes caps the uncompressed size of a catalog archive's files; the
// real archive is tens of kilobytes. A variable so a test can lower it.
var maxCatalogBytes = 256 << 20

const (
	maxIndexBytes = 1 << 20

	indexFile     = "index.json"
	stagingSuffix = ".new"
	backupSuffix  = ".old"
)

// CatalogStore is the on-disk copy of the curated catalog, a directory of
// <id>/ per template plus index.json (the archive's own layout, read by
// DirCatalog). An archive replaces it only after its signature verifies and
// its serial is higher than the installed one.
type CatalogStore struct {
	Dir string
	// Key verifies the archive signature; nil means CatalogPublicKey.
	Key ed25519.PublicKey

	rename func(oldpath, newpath string) error
}

func (s CatalogStore) key() ed25519.PublicKey {
	if s.Key != nil {
		return s.Key
	}
	return CatalogPublicKey
}

func (s CatalogStore) mv(oldpath, newpath string) error {
	if s.rename != nil {
		return s.rename(oldpath, newpath)
	}
	return os.Rename(oldpath, newpath)
}

// VerifyArchive checks the detached signature over archive against key, then
// that the archive is a well-formed catalog, and returns its serial. It
// touches no file.
func VerifyArchive(key ed25519.PublicKey, archive, sig []byte) (int64, error) {
	if len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, archive, sig) {
		return 0, ErrBadSignature
	}
	return readArchive(archive, nil)
}

// Serial is the installed catalog's serial, and false when no catalog with a
// readable index.json is installed.
func (s CatalogStore) Serial() (int64, bool, error) {
	data, err := os.ReadFile(filepath.Join(s.Dir, indexFile))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("reading the installed catalog's index: %w", err)
	}
	serial, err := parseSerial(data)
	if err != nil {
		return 0, false, nil
	}
	return serial, true, nil
}

// Install replaces the installed catalog with archive. It refuses an
// archive whose signature does not verify, that is malformed, or whose serial
// is not higher than the installed one (ErrNotNewer), and every refusal
// leaves the installed catalog untouched.
func (s CatalogStore) Install(archive, sig []byte) error {
	_, err := s.install(archive, sig, true)
	return err
}

// Seed installs archive when no catalog is installed or the installed one is
// older, and leaves an equal or newer one alone. The signature is verified
// either way.
func (s CatalogStore) Seed(archive, sig []byte) (bool, error) {
	return s.install(archive, sig, false)
}

func (s CatalogStore) install(archive, sig []byte, strict bool) (installed bool, err error) {
	serial, err := VerifyArchive(s.key(), archive, sig)
	if err != nil {
		return false, err
	}
	if err := s.Recover(); err != nil {
		return false, err
	}
	current, have, err := s.Serial()
	if err != nil {
		return false, err
	}
	if have && serial <= current {
		if strict {
			return false, fmt.Errorf("%w: serial %d, installed %d", ErrNotNewer, serial, current)
		}
		return false, nil
	}

	staging := s.Dir + stagingSuffix
	if err := os.MkdirAll(filepath.Dir(s.Dir), 0o755); err != nil {
		return false, fmt.Errorf("creating the catalog's parent directory: %w", err)
	}
	if err := os.Mkdir(staging, 0o755); err != nil {
		return false, fmt.Errorf("creating the catalog staging directory: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := extractArchive(archive, staging); err != nil {
		return false, err
	}
	if err := s.swap(staging); err != nil {
		return false, err
	}
	return true, nil
}

// swap moves the previous copy aside, moves staging in and removes the
// previous copy. A process that stops between the two renames leaves no
// Dir, which Recover repairs on the next start.
func (s CatalogStore) swap(staging string) error {
	backup := s.Dir + backupSuffix
	_, err := os.Lstat(s.Dir)
	have := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("checking the installed catalog: %w", err)
	}
	if have {
		if err := s.mv(s.Dir, backup); err != nil {
			return fmt.Errorf("moving the installed catalog aside: %w", err)
		}
	}
	if err := s.mv(staging, s.Dir); err != nil {
		if have {
			if rerr := s.mv(backup, s.Dir); rerr != nil {
				return fmt.Errorf("moving the new catalog into place: %w (the previous catalog stays at %s and is restored on the next start: %v)", err, backup, rerr)
			}
		}
		return fmt.Errorf("moving the new catalog into place: %w", err)
	}
	if err := syncDir(filepath.Dir(s.Dir)); err != nil {
		return fmt.Errorf("syncing the catalog's parent directory: %w", err)
	}
	if have {
		// The new catalog is live; a backup that cannot be removed now is
		// removed by Recover on the next start, and must not make this
		// replacement report failure.
		_ = os.RemoveAll(backup)
	}
	return nil
}

// Recover repairs what an interrupted swap left: it restores the previous
// copy when the catalog directory is missing, drops a backup the live
// directory has outlived, and drops a half-written staging directory. It
// never touches a live catalog.
func (s CatalogStore) Recover() error {
	backup := s.Dir + backupSuffix
	_, dirErr := os.Lstat(s.Dir)
	_, backupErr := os.Lstat(backup)
	switch {
	case dirErr != nil && !errors.Is(dirErr, fs.ErrNotExist):
		return fmt.Errorf("checking the installed catalog: %w", dirErr)
	case backupErr != nil && !errors.Is(backupErr, fs.ErrNotExist):
		return fmt.Errorf("checking the catalog backup: %w", backupErr)
	case dirErr != nil && backupErr == nil:
		if err := s.mv(backup, s.Dir); err != nil {
			return fmt.Errorf("restoring the previous catalog: %w", err)
		}
		if err := syncDir(filepath.Dir(s.Dir)); err != nil {
			return fmt.Errorf("syncing the catalog's parent directory: %w", err)
		}
	case dirErr == nil && backupErr == nil:
		if err := os.RemoveAll(backup); err != nil {
			return fmt.Errorf("removing the replaced catalog: %w", err)
		}
	}
	if err := os.RemoveAll(s.Dir + stagingSuffix); err != nil {
		return fmt.Errorf("removing a half-written catalog: %w", err)
	}
	return nil
}

// readArchive walks the archive, refusing anything that is not a plain file
// or directory directly under index.json or an <id>/ directory, calls visit
// with every entry, and returns the serial index.json carries.
func readArchive(archive []byte, visit func(name string, dir bool, size int64, r io.Reader) error) (int64, error) {
	zr, err := zstd.NewReader(bytes.NewReader(archive), zstd.WithDecoderConcurrency(1))
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrBadArchive, err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)

	remaining := int64(maxCatalogBytes)
	seen := map[string]bool{}
	var index []byte
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("%w: %v", ErrBadArchive, err)
		}
		name, dir, err := entryName(hdr, seen)
		if err != nil {
			return 0, err
		}
		if !dir {
			if hdr.Size > remaining {
				return 0, fmt.Errorf("%w: more than %d bytes of files", ErrBadArchive, maxCatalogBytes)
			}
			remaining -= hdr.Size
		}
		var body io.Reader = tr
		if name == indexFile {
			if hdr.Size > maxIndexBytes {
				return 0, fmt.Errorf("%w: %s is larger than %d bytes", ErrBadArchive, indexFile, maxIndexBytes)
			}
			if index, err = io.ReadAll(tr); err != nil {
				return 0, fmt.Errorf("%w: reading %s: %v", ErrBadArchive, indexFile, err)
			}
			body = bytes.NewReader(index)
		}
		if visit != nil {
			if err := visit(name, dir, hdr.Size, body); err != nil {
				return 0, err
			}
		}
	}
	if index == nil {
		return 0, fmt.Errorf("%w: no %s", ErrBadArchive, indexFile)
	}
	serial, err := parseSerial(index)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrBadArchive, err)
	}
	return serial, nil
}

// entryName validates one tar entry and records it in seen (name to
// is-directory), so a duplicate, or a file used as a directory, is refused
// here rather than failing half way through writing.
func entryName(hdr *tar.Header, seen map[string]bool) (string, bool, error) {
	var dir bool
	switch hdr.Typeflag {
	case tar.TypeReg:
	case tar.TypeDir:
		dir = true
	default:
		return "", false, fmt.Errorf("%w: %q is neither a file nor a directory", ErrBadArchive, hdr.Name)
	}
	name := hdr.Name
	if dir {
		name = strings.TrimSuffix(name, "/")
	}
	if name == "" || strings.Contains(name, `\`) {
		return "", false, fmt.Errorf("%w: entry name %q", ErrBadArchive, hdr.Name)
	}
	for _, c := range []byte(name) {
		if c < 0x20 || c == 0x7f {
			return "", false, fmt.Errorf("%w: entry name %q", ErrBadArchive, hdr.Name)
		}
	}
	parts := strings.Split(name, "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return "", false, fmt.Errorf("%w: entry name %q", ErrBadArchive, hdr.Name)
		}
	}
	if parts[0] == indexFile {
		if len(parts) != 1 || dir {
			return "", false, fmt.Errorf("%w: %q is not a plain %s", ErrBadArchive, hdr.Name, indexFile)
		}
	} else if !idPattern.MatchString(parts[0]) || (len(parts) == 1 && !dir) {
		return "", false, fmt.Errorf("%w: %q is outside %s and a template directory", ErrBadArchive, hdr.Name, indexFile)
	}

	if isDir, dup := seen[name]; dup && (!dir || !isDir) {
		return "", false, fmt.Errorf("%w: %q appears twice", ErrBadArchive, hdr.Name)
	}
	for i := 1; i < len(parts); i++ {
		if isDir, ok := seen[strings.Join(parts[:i], "/")]; ok && !isDir {
			return "", false, fmt.Errorf("%w: %q is inside a file", ErrBadArchive, hdr.Name)
		}
	}
	for i := 1; i < len(parts); i++ {
		seen[strings.Join(parts[:i], "/")] = true
	}
	seen[name] = dir
	return name, dir, nil
}

func parseSerial(index []byte) (int64, error) {
	var doc struct {
		Serial int64 `json:"serial"`
	}
	if err := json.Unmarshal(index, &doc); err != nil {
		return 0, fmt.Errorf("%s is not valid JSON: %w", indexFile, err)
	}
	if doc.Serial <= 0 {
		return 0, fmt.Errorf("%s carries no serial", indexFile)
	}
	return doc.Serial, nil
}

// extractArchive writes the archive's files under root, which must be an
// empty directory. Names were validated by readArchive, and nothing but
// plain files and directories is ever created, so no entry can resolve
// outside root.
func extractArchive(archive []byte, root string) error {
	_, err := readArchive(archive, func(name string, dir bool, size int64, r io.Reader) error {
		target := filepath.Join(root, filepath.FromSlash(name))
		if dir {
			return os.MkdirAll(target, 0o755)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("creating the directory for %s: %w", name, err)
		}
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return fmt.Errorf("creating %s: %w", name, err)
		}
		if _, err := io.Copy(f, io.LimitReader(r, size)); err != nil {
			_ = f.Close()
			return fmt.Errorf("writing %s: %w", name, err)
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return fmt.Errorf("syncing %s: %w", name, err)
		}
		return f.Close()
	})
	if err != nil {
		return err
	}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		return syncDir(p)
	})
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}
