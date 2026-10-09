package template

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

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
	// other than a plain file or directory, more entries or bytes than the
	// caps allow, a name that is too long or too deep, or a missing or
	// unreadable index.json.
	ErrBadArchive = errors.New("template: the catalog archive is malformed")
)

// maxCatalogBytes caps the uncompressed size of a catalog archive's files; the
// real archive is tens of kilobytes. A variable so a test can lower it.
var maxCatalogBytes = 256 << 20

// maxCatalogEntries caps the number of entries, directories included, in a
// catalog archive. A variable so a test can lower it.
var maxCatalogEntries = 20000

const (
	// entryCostBytes is charged against the byte budget for every entry, so
	// a flood of empty files and directories spends the budget too.
	entryCostBytes = 4096
	// maxEntryNameBytes and maxEntryDepth bound one entry's name; the
	// duplicate check records a prefix of it for every path component.
	maxEntryNameBytes = 256
	maxEntryDepth     = 8
	// tarTrailerSlack is added to the byte budget to bound the decoded
	// stream: an entry's tar framing (a 512-byte header, up to 511 bytes of
	// padding and, for a name of at most maxEntryNameBytes, one PAX header
	// and record block) stays under entryCostBytes, which the budget already
	// charges, so only the end-of-archive blocks and a writer's record
	// padding are left over.
	tarTrailerSlack = 16 << 10
)

// decodedLimit fails once more than left bytes have been read through it, so
// metadata tar.Reader consumes internally (PAX and GNU long-name headers)
// spends the same bounded budget as file data.
type decodedLimit struct {
	r    io.Reader
	left int64
}

func (d *decodedLimit) Read(p []byte) (int, error) {
	if d.left < 0 {
		return 0, fmt.Errorf("%w: the decoded archive is larger than %d bytes", ErrBadArchive, maxCatalogBytes)
	}
	if int64(len(p)) > d.left+1 {
		p = p[:d.left+1]
	}
	n, err := d.r.Read(p)
	d.left -= int64(n)
	if d.left < 0 {
		return 0, fmt.Errorf("%w: the decoded archive is larger than %d bytes", ErrBadArchive, maxCatalogBytes)
	}
	return n, err
}

const (
	maxIndexBytes = 1 << 20

	indexFile     = "index.json"
	validatorFile = "fetch.json"
	stagingSuffix = ".new"
	backupSuffix  = ".old"
)

// storeLocks holds one mutex per catalog directory, so every CatalogStore
// over the same directory, however many copies of the value exist, runs one
// write at a time: the startup seed, a manual check and a background check
// never overlap (doc 04 §7).
var storeLocks sync.Map

func (s CatalogStore) lock() func() {
	m, _ := storeLocks.LoadOrStore(filepath.Clean(s.Dir), &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// CatalogStore is the on-disk copy of the curated catalog, a directory of
// <id>/ per template plus index.json (the archive's own layout, read by
// DirCatalog). An archive replaces it only after its signature verifies and
// its serial is higher than the installed one.
type CatalogStore struct {
	Dir string
	// Key verifies the archive signature; nil means CatalogPublicKey.
	Key ed25519.PublicKey
	// Unsigned installs an archive with no signature check at all. Only a
	// user-added source with no key of its own sets it (doc 04 §7); the zero
	// value, which the curated catalog uses, verifies every archive.
	Unsigned bool

	rename func(oldpath, newpath string) error
}

func (s CatalogStore) key() ed25519.PublicKey {
	if s.Key != nil {
		return s.Key
	}
	return CatalogPublicKey
}

// verify checks archive and its signature the way this store trusts them:
// unsigned stores check only that the archive is a well-formed catalog.
func (s CatalogStore) verify(ctx context.Context, archive, sig []byte) (int64, []byte, error) {
	if s.Unsigned {
		return checkArchive(ctx, archive)
	}
	return verifyArchive(ctx, s.key(), archive, sig)
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
	serial, _, err := verifyArchive(context.Background(), key, archive, sig)
	return serial, err
}

// verifyArchive is VerifyArchive that also returns the archive's index.json,
// which must parse as a catalog index so a signed archive DirCatalog could
// not list is never installed.
func verifyArchive(ctx context.Context, key ed25519.PublicKey, archive, sig []byte) (int64, []byte, error) {
	if len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, archive, sig) {
		return 0, nil, ErrBadSignature
	}
	return checkArchive(ctx, archive)
}

// checkArchive is the well-formedness half of verifyArchive: it reads the
// archive and returns its serial and index.json.
func checkArchive(ctx context.Context, archive []byte) (int64, []byte, error) {
	serial, index, err := readArchive(ctx, archive, nil)
	if err != nil {
		return 0, nil, err
	}
	if _, err := parseIndex(index); err != nil {
		return 0, nil, fmt.Errorf("%w: %s %v", ErrBadArchive, indexFile, err)
	}
	return serial, index, nil
}

// Serial is the installed catalog's serial, and false when no catalog with a
// readable index.json is installed.
func (s CatalogStore) Serial() (int64, bool, error) {
	data, err := s.readIndex()
	if err != nil || data == nil {
		return 0, false, err
	}
	serial, err := parseSerial(data)
	if err != nil {
		return 0, false, nil
	}
	return serial, true, nil
}

// readIndex is the installed catalog's index.json, or nil when none is
// installed.
func (s CatalogStore) readIndex() ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(s.Dir, indexFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the installed catalog's index: %w", err)
	}
	return data, nil
}

// Install replaces the installed catalog with archive. It refuses an
// archive whose signature does not verify, that is malformed, or whose serial
// is not higher than the installed one (ErrNotNewer), and every refusal
// leaves the installed catalog untouched.
func (s CatalogStore) Install(archive, sig []byte) error {
	defer s.lock()()
	_, err := s.install(context.Background(), archive, sig, installOptions{strict: true})
	return err
}

// Seed installs archive when no catalog is installed or the installed one is
// older, and leaves an equal or newer one alone. The signature is verified
// either way.
func (s CatalogStore) Seed(archive, sig []byte) (bool, error) {
	defer s.lock()()
	r, err := s.install(context.Background(), archive, sig, installOptions{})
	return r.installed, err
}

// Validators are the HTTP validators of the archive a fetched catalog came
// from. They are stored inside the catalog directory, so they are replaced by
// the same rename as the catalog they describe and never outlive it.
type Validators struct {
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"lastModified,omitempty"`
}

func (v Validators) empty() bool { return v.ETag == "" && v.LastModified == "" }

// Validators returns the validators stored with the installed catalog. None
// are stored for a catalog that was not fetched, and an unreadable file counts
// as none, which only makes the next request unconditional.
func (s CatalogStore) Validators() Validators {
	data, err := os.ReadFile(filepath.Join(s.Dir, validatorFile))
	if err != nil {
		return Validators{}
	}
	var v Validators
	if json.Unmarshal(data, &v) != nil {
		return Validators{}
	}
	return v
}

// Fetched is what InstallFetched did: whether it replaced the installed
// catalog, and the index.json of the catalog before and after (nil when there
// was none).
type Fetched struct {
	Installed bool
	Previous  []byte
	Current   []byte
}

// InstallFetched installs an archive fetched from the network, recording v
// with it. Like Install it refuses an archive that does not verify or is
// older than the installed catalog. An archive with the installed serial and
// the installed index.json is the same catalog again: it replaces nothing,
// refreshes the stored validators, and reports Installed false. An archive
// with the installed serial and a different index.json is refused
// (ErrNotNewer).
func (s CatalogStore) InstallFetched(ctx context.Context, archive, sig []byte, v Validators) (Fetched, error) {
	defer s.lock()()
	r, err := s.install(ctx, archive, sig, installOptions{strict: true, fetched: &v})
	return Fetched{Installed: r.installed, Previous: r.previous, Current: r.current}, err
}

type installOptions struct {
	strict  bool
	fetched *Validators
}

type installResult struct {
	installed bool
	previous  []byte
	current   []byte
}

func (s CatalogStore) install(ctx context.Context, archive, sig []byte, opts installOptions) (res installResult, err error) {
	serial, index, err := s.verify(ctx, archive, sig)
	if err != nil {
		return res, err
	}
	if err := s.recoverLocked(); err != nil {
		return res, err
	}
	previous, err := s.readIndex()
	if err != nil {
		return res, err
	}
	res.previous, res.current = previous, index
	current, have, err := s.Serial()
	if err != nil {
		return res, err
	}
	if have && serial <= current {
		if opts.fetched != nil && serial == current && bytes.Equal(index, previous) {
			res.current = previous
			if opts.fetched.empty() {
				return res, nil
			}
			return res, writeValidators(s.Dir, *opts.fetched)
		}
		if opts.strict {
			return res, fmt.Errorf("%w: serial %d, installed %d", ErrNotNewer, serial, current)
		}
		return res, nil
	}

	staging := s.Dir + stagingSuffix
	if err := os.MkdirAll(filepath.Dir(s.Dir), 0o755); err != nil {
		return res, fmt.Errorf("creating the catalog's parent directory: %w", err)
	}
	if err := os.Mkdir(staging, 0o755); err != nil {
		return res, fmt.Errorf("creating the catalog staging directory: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := extractArchive(ctx, archive, staging); err != nil {
		return res, err
	}
	if opts.fetched != nil && !opts.fetched.empty() {
		if err := writeValidators(staging, *opts.fetched); err != nil {
			return res, err
		}
	}
	if err := s.swap(staging); err != nil {
		return res, err
	}
	res.installed = true
	return res, nil
}

// writeValidators stores v in dir through a temporary file and a rename, so
// a reader and an interrupted write never see a half-written file.
func writeValidators(dir string, v Validators) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encoding the catalog validators: %w", err)
	}
	tmp := filepath.Join(dir, validatorFile+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("writing the catalog validators: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("writing the catalog validators: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("syncing the catalog validators: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("writing the catalog validators: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, validatorFile)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("storing the catalog validators: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("syncing the catalog validators: %w", err)
	}
	return nil
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
	defer s.lock()()
	return s.recoverLocked()
}

func (s CatalogStore) recoverLocked() error {
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
// with every entry, and returns the serial index.json carries and its bytes.
func readArchive(ctx context.Context, archive []byte, visit func(name string, dir bool, size int64, r io.Reader) error) (int64, []byte, error) {
	zr, err := zstd.NewReader(bytes.NewReader(archive), zstd.WithDecoderConcurrency(1))
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %v", ErrBadArchive, err)
	}
	defer zr.Close()
	tr := tar.NewReader(&decodedLimit{r: zr, left: int64(maxCatalogBytes) + tarTrailerSlack})

	remaining := int64(maxCatalogBytes)
	seen := map[string]bool{}
	var index []byte
	entries := 0
	for {
		if err := ctx.Err(); err != nil {
			return 0, nil, err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, nil, fmt.Errorf("%w: %v", ErrBadArchive, err)
		}
		if entries++; entries > maxCatalogEntries {
			return 0, nil, fmt.Errorf("%w: more than %d entries", ErrBadArchive, maxCatalogEntries)
		}
		name, dir, err := entryName(hdr, seen)
		if err != nil {
			return 0, nil, err
		}
		if remaining -= entryCostBytes; remaining < 0 {
			return 0, nil, fmt.Errorf("%w: more than %d bytes of files and entries", ErrBadArchive, maxCatalogBytes)
		}
		if !dir {
			if hdr.Size > remaining {
				return 0, nil, fmt.Errorf("%w: more than %d bytes of files and entries", ErrBadArchive, maxCatalogBytes)
			}
			remaining -= hdr.Size
		}
		var body io.Reader = tr
		if name == indexFile {
			if hdr.Size > maxIndexBytes {
				return 0, nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrBadArchive, indexFile, maxIndexBytes)
			}
			if index, err = io.ReadAll(tr); err != nil {
				return 0, nil, fmt.Errorf("%w: reading %s: %v", ErrBadArchive, indexFile, err)
			}
			body = bytes.NewReader(index)
		}
		if visit != nil {
			if err := visit(name, dir, hdr.Size, body); err != nil {
				return 0, nil, err
			}
		}
	}
	if index == nil {
		return 0, nil, fmt.Errorf("%w: no %s", ErrBadArchive, indexFile)
	}
	serial, err := parseSerial(index)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %v", ErrBadArchive, err)
	}
	return serial, index, nil
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
	if len(hdr.Name) > maxEntryNameBytes {
		return "", false, fmt.Errorf("%w: an entry name is longer than %d bytes", ErrBadArchive, maxEntryNameBytes)
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
	if len(parts) > maxEntryDepth {
		return "", false, fmt.Errorf("%w: %q is nested deeper than %d levels", ErrBadArchive, hdr.Name, maxEntryDepth)
	}
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
func extractArchive(ctx context.Context, archive []byte, root string) error {
	_, _, err := readArchive(ctx, archive, func(name string, dir bool, size int64, r io.Reader) error {
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
