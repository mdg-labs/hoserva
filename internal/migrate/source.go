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
// zip today, a flash device mounted read-only later. Its root is /boot, so every
// config file is named under "config/". Names are slash-separated and never
// carry the junk a flash accumulates (ignoredPath).
type FlashSource interface {
	// List returns every regular file under dir ("" for the root), sorted.
	List(dir string) []string
	// Read returns one file's content, or an error wrapping fs.ErrNotExist.
	Read(name string) ([]byte, error)
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
