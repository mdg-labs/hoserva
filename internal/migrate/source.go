package migrate

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// MaxZipBytes bounds the Flash Backup zip a session accepts. A real flash is a
// few hundred MiB: the kernel images dominate, and the scan reads none of them.
const MaxZipBytes int64 = 2 << 30

// maxZipBytes is MaxZipBytes, a variable only so a test can lower it.
var maxZipBytes = MaxZipBytes

// maxEntryBytes bounds one config file read from the zip.
const maxEntryBytes int64 = 16 << 20

// ErrInvalidZip is wrapped by every refusal of a zip's own structure: not a zip
// at all, an entry path that escapes the root, a duplicate entry, or a size
// beyond maxZipBytes.
var ErrInvalidZip = errors.New("not a usable Flash Backup zip")

// FlashSource is where Unraid's configuration is read from: the Flash Backup
// zip, or the flash device mounted read-only (DirSource). Its root is /boot, so
// every config file is named under "config/". Names are slash-separated and never
// carry the junk a flash accumulates (ignoredPath).
type FlashSource interface {
	// List returns every regular file under dir ("" for the root), sorted.
	List(dir string) []string
	// Read returns one file's content, or an error wrapping fs.ErrNotExist.
	Read(name string) ([]byte, error)
	// ModTime returns when the file was last modified, and false when the
	// source does not know or the file is not there. A zip stores the time of
	// the flash's file; a FAT stick and a zip carry no time zone, so a time is
	// good to a day at best.
	ModTime(name string) (time.Time, bool)
}

// ZipSource is a FlashSource over a zip, read in memory entry by entry.
type ZipSource struct {
	files map[string]*zip.File
}

// OpenZip indexes the zip in r. size is its length in bytes. Every entry's
// path is checked, whether or not the scan will read it.
func OpenZip(r io.ReaderAt, size int64) (*ZipSource, error) {
	if size > maxZipBytes {
		return nil, fmt.Errorf("%w: it is %d bytes and the limit is %d", ErrInvalidZip, size, maxZipBytes)
	}
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidZip, err)
	}
	src := &ZipSource{files: make(map[string]*zip.File, len(zr.File))}
	seen := make(map[string]bool, len(zr.File))
	for _, f := range zr.File {
		name, err := cleanEntryName(f.Name)
		if err != nil {
			return nil, err
		}
		if seen[name] {
			return nil, fmt.Errorf("%w: %q appears twice", ErrInvalidZip, name)
		}
		seen[name] = true
		if f.FileInfo().IsDir() || !f.Mode().IsRegular() || name == "" || ignoredPath(name) {
			continue
		}
		src.files[name] = f
	}
	return src, nil
}

// cleanEntryName refuses an entry that could land outside the root if the zip
// were ever extracted: an absolute path, a ".." element, a backslash form of
// either, or a NUL. Nothing here extracts, but a source that carries one is not
// a Flash Backup.
func cleanEntryName(raw string) (string, error) {
	if strings.ContainsRune(raw, 0) {
		return "", fmt.Errorf("%w: an entry name contains a NUL byte", ErrInvalidZip)
	}
	name := strings.ReplaceAll(raw, "\\", "/")
	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("%w: entry %q is an absolute path", ErrInvalidZip, raw)
	}
	for _, elem := range strings.Split(name, "/") {
		if elem == ".." {
			return "", fmt.Errorf("%w: entry %q leaves the zip's root", ErrInvalidZip, raw)
		}
	}
	return strings.TrimSuffix(path.Clean(name), "/"), nil
}

// ignoredPath reports whether name is flash junk the scan never reads as
// configuration: the previous release Unraid keeps, desktop litter and AppleDouble
// twins (anywhere), and a git directory (anywhere).
func ignoredPath(name string) bool {
	parts := strings.Split(name, "/")
	if parts[0] == "previous" || parts[0] == "prev" {
		return true
	}
	if parts[0] == ".Spotlight-V100" || parts[0] == ".fseventsd" || parts[0] == "System Volume Information" {
		return true
	}
	if strings.HasPrefix(parts[0], ".Trash") {
		return true
	}
	for _, p := range parts {
		if p == ".git" || strings.HasPrefix(p, "._") {
			return true
		}
	}
	return false
}

// List implements FlashSource.
func (z *ZipSource) List(dir string) []string {
	prefix := ""
	if dir != "" {
		prefix = strings.TrimSuffix(dir, "/") + "/"
	}
	var out []string
	for name := range z.files {
		if strings.HasPrefix(name, prefix) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Read implements FlashSource.
func (z *ZipSource) Read(name string) ([]byte, error) {
	f, ok := z.files[name]
	if !ok {
		return nil, fmt.Errorf("%s: %w", name, fs.ErrNotExist)
	}
	if f.UncompressedSize64 > uint64(maxEntryBytes) {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrInvalidZip, name, maxEntryBytes)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("%w: opening %s: %v", ErrInvalidZip, name, err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, maxEntryBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: reading %s: %v", ErrInvalidZip, name, err)
	}
	if int64(len(data)) > maxEntryBytes {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrInvalidZip, name, maxEntryBytes)
	}
	return data, nil
}

// ModTime implements FlashSource.
func (z *ZipSource) ModTime(name string) (time.Time, bool) {
	f, ok := z.files[name]
	if !ok || f.Modified.IsZero() {
		return time.Time{}, false
	}
	return f.Modified, true
}

// OpenZipFile opens the zip at path, for a session's stored source.
func OpenZipFile(p string) (*ZipSource, *os.File, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	src, err := OpenZip(f, st.Size())
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return src, f, nil
}

// DirSource is a FlashSource over a directory that is a flash device mounted
// read-only. It reads through an os.Root, so no name leads out of the directory,
// and it only ever opens a file for reading. List and Read cannot return the
// error a failing device gives, so the first one is kept for Err: a scan that
// listed half a flash because the stick was pulled must not report on it.
type DirSource struct {
	root *os.Root

	mu  sync.Mutex
	err error
}

// OpenDir opens dir, the mountpoint of a flash device, as a source.
func OpenDir(dir string) (*DirSource, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &DirSource{root: root}, nil
}

// Close releases the directory.
func (d *DirSource) Close() error { return d.root.Close() }

// Err returns the first read error List or Read met, or nil.
func (d *DirSource) Err() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err
}

func (d *DirSource) fail(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err == nil {
		d.err = err
	}
}

// List implements FlashSource. It reads directory entries only, never a file,
// and does not enter a directory the scan ignores.
func (d *DirSource) List(dir string) []string {
	start := "."
	if dir != "" {
		start = path.Clean(dir)
	}
	var out []string
	err := fs.WalkDir(d.root.FS(), start, func(name string, e fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && name == start {
				return fs.SkipDir
			}
			return err
		}
		if name != "." && ignoredPath(name) {
			if e.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if e.Type().IsRegular() {
			out = append(out, name)
		}
		return nil
	})
	if err != nil {
		d.fail(fmt.Errorf("listing %q: %w", dir, err))
	}
	sort.Strings(out)
	return out
}

// Read implements FlashSource.
func (d *DirSource) Read(name string) ([]byte, error) {
	f, err := d.root.Open(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return nil, fmt.Errorf("%s: %w", name, fs.ErrNotExist)
		}
		d.fail(err)
		return nil, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		d.fail(err)
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: %w", name, fs.ErrNotExist)
	}
	if st.Size() > maxEntryBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, maxEntryBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxEntryBytes+1))
	if err != nil {
		d.fail(err)
		return nil, err
	}
	if int64(len(data)) > maxEntryBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, maxEntryBytes)
	}
	return data, nil
}

// ModTime implements FlashSource.
func (d *DirSource) ModTime(name string) (time.Time, bool) {
	st, err := d.root.Stat(name)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			d.fail(err)
		}
		return time.Time{}, false
	}
	return st.ModTime(), true
}
