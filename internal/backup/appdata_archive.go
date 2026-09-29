package backup

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// An appdata archive is a tar.zst holding, in order: the header entry, one
// "data/<i>/…" tree per directory of the container's appdata, and the
// trailer entry. The trailer carries what the tree held, so verifying the
// archive is one sequential read of it and needs no second copy of the
// data: a truncated or altered archive has no trailer, or one that does
// not match what was read.
//
// The trailer's SHA256 covers every byte of the tar stream before the
// trailer entry: each entry's header (name, type, mode, owner, times, link
// target, size), its content and its padding, so it covers everything
// extraction reads from an entry. The zstd frame's own checksum covers the
// whole decompressed stream including the trailer, and verifyAppdata reads
// to the end of the frame to check it. Both are unkeyed checksums stored in
// the archive itself: they detect accidental corruption and truncation, and
// say nothing about an archive someone rewrote and re-checksummed on purpose.
const (
	appdataFormatVersion = 1
	appdataHeaderName    = "hoserva-appdata.json"
	appdataTrailerName   = "hoserva-appdata-trailer.json"
	appdataMetaLimit     = 4 << 20
)

// appdataHeader says what an archive is and where its trees go back to.
// Dirs[i] is the absolute host path archived under "data/<i>/".
type appdataHeader struct {
	Version       int       `json:"version"`
	Container     string    `json:"container"`
	Image         string    `json:"image"`
	CreatedAt     time.Time `json:"createdAt"`
	Hostname      string    `json:"hostname"`
	Stopped       bool      `json:"stopped"`
	DatabaseImage bool      `json:"databaseImage"`
	Reason        string    `json:"reason,omitempty"`
	Dirs          []string  `json:"dirs"`
}

// appdataTrailer is what the packer wrote. Changed counts files that were
// not the size they had when their header was written, because they shrank
// or grew, or vanished, while they were copied; a stopped container has
// none. A file rewritten in place at the same size is not detected. SHA256
// is over the tar stream up to the trailer entry.
type appdataTrailer struct {
	Files   int64  `json:"files"`
	Bytes   int64  `json:"bytes"`
	Skipped int64  `json:"skipped"`
	Changed int64  `json:"changed"`
	SHA256  string `json:"sha256"`
}

func appdataPrefix(i int) string {
	return "data/" + strconv.Itoa(i) + "/"
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// packAppdata writes hdr and every tree hdr.Dirs names to dest, through a
// temporary file that is renamed into place only once complete. A socket,
// device or named pipe has no content to keep and is skipped and counted.
func packAppdata(ctx context.Context, dest string, hdr appdataHeader) (appdataTrailer, error) {
	var trailer appdataTrailer
	out, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+".*.tmp")
	if err != nil {
		return trailer, fmt.Errorf("creating archive temp file: %w", err)
	}
	tmp := out.Name()
	fail := func(err error) (appdataTrailer, error) {
		_ = out.Close()
		_ = os.Remove(tmp)
		return trailer, err
	}
	if err := out.Chmod(0o600); err != nil {
		return fail(fmt.Errorf("restricting archive temp file: %w", err))
	}
	zw, err := zstd.NewWriter(out, zstd.WithEncoderCRC(true))
	if err != nil {
		return fail(fmt.Errorf("creating zstd writer: %w", err))
	}
	sum := sha256.New()
	tw := tar.NewWriter(io.MultiWriter(zw, sum))

	hdr.Version = appdataFormatVersion
	if err := writeAppdataMeta(tw, appdataHeaderName, hdr, hdr.CreatedAt); err != nil {
		_ = zw.Close()
		return fail(err)
	}
	for i, dir := range hdr.Dirs {
		if err := packAppdataTree(ctx, tw, &trailer, dir, appdataPrefix(i)); err != nil {
			_ = zw.Close()
			return fail(fmt.Errorf("archiving %s: %w", dir, err))
		}
	}
	if err := tw.Flush(); err != nil {
		_ = zw.Close()
		return fail(fmt.Errorf("padding the last archived file: %w", err))
	}
	trailer.SHA256 = hex.EncodeToString(sum.Sum(nil))
	if err := writeAppdataMeta(tw, appdataTrailerName, trailer, hdr.CreatedAt); err != nil {
		_ = zw.Close()
		return fail(err)
	}
	if err := tw.Close(); err != nil {
		_ = zw.Close()
		return fail(fmt.Errorf("closing tar writer: %w", err))
	}
	if err := zw.Close(); err != nil {
		return fail(fmt.Errorf("closing zstd writer: %w", err))
	}
	if err := out.Sync(); err != nil {
		return fail(fmt.Errorf("syncing archive temp file: %w", err))
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return trailer, fmt.Errorf("closing archive temp file: %w", err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return trailer, fmt.Errorf("finalizing archive: %w", err)
	}
	return trailer, fsyncDir(filepath.Dir(dest))
}

// writeAppdataMeta writes v as a regular entry whose header is one USTAR
// block, which is what lets verifyAppdata find where the tar stream the
// trailer's checksum covers ends.
func writeAppdataMeta(tw *tar.Writer, name string, v any, at time.Time) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encoding %s: %w", name, err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(raw)), ModTime: at.Round(time.Second), Format: tar.FormatUSTAR}); err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}
	if _, err := tw.Write(raw); err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}
	return nil
}

func packAppdataTree(ctx context.Context, tw *tar.Writer, trailer *appdataTrailer, root, prefix string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		name := prefix
		if rel != "." {
			name += filepath.ToSlash(rel)
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				trailer.Changed++
				return nil
			}
			return err
		}
		switch {
		case info.IsDir():
			h, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			h.Name = strings.TrimSuffix(name, "/") + "/"
			return tw.WriteHeader(h)
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					trailer.Changed++
					return nil
				}
				return err
			}
			h, err := tar.FileInfoHeader(info, link)
			if err != nil {
				return err
			}
			h.Name = name
			return tw.WriteHeader(h)
		case info.Mode().IsRegular():
			return packAppdataFile(tw, trailer, p, name)
		default:
			trailer.Skipped++
			return nil
		}
	})
}

func packAppdataFile(tw *tar.Writer, trailer *appdataTrailer, p, name string) error {
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			trailer.Changed++
			return nil
		}
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		trailer.Skipped++
		return nil
	}
	h, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	h.Name = name
	if err := tw.WriteHeader(h); err != nil {
		return err
	}
	changed, err := copyExactly(tw, f, h.Size)
	if err != nil {
		return err
	}
	if changed {
		trailer.Changed++
	}
	trailer.Files++
	trailer.Bytes += h.Size
	return nil
}

// copyExactly writes size bytes from r to w, which a tar entry requires
// even when the file has shrunk since its header was written: the rest is
// zeros. A file that has grown is cut at size. Either way changed reports
// it, a grown file by r still holding a byte once size have been copied.
func copyExactly(w io.Writer, r io.Reader, size int64) (changed bool, err error) {
	n, err := io.CopyN(w, r, size)
	if errors.Is(err, io.EOF) {
		_, err = io.CopyN(w, zeroReader{}, size-n)
		return true, err
	}
	if err != nil {
		return false, err
	}
	var extra [1]byte
	m, err := r.Read(extra[:])
	if m > 0 {
		return true, nil
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return false, nil
}

// laggingHash hashes what is written to it except the last lag bytes,
// which it holds back. The tar stream is read through it, so once the
// header block of the trailer entry has been read it has hashed exactly
// what precedes that block, whichever way the reader chunks its reads.
type laggingHash struct {
	h       hash.Hash
	pending []byte
	lag     int
}

func (l *laggingHash) Write(p []byte) (int, error) {
	l.pending = append(l.pending, p...)
	if extra := len(l.pending) - l.lag; extra > 0 {
		_, _ = l.h.Write(l.pending[:extra])
		l.pending = append(l.pending[:0], l.pending[extra:]...)
	}
	return len(p), nil
}

func (l *laggingHash) sum() string {
	return hex.EncodeToString(l.h.Sum(nil))
}

// tarBlock is the size of a tar header block, and of the trailer entry's
// header, which writeAppdataMeta keeps to one.
const tarBlock = 512

// verifyAppdata reads the whole archive, to the end of its zstd frame so
// the frame's checksum is checked too, and checks it against its own
// trailer, returning the header.
func verifyAppdata(archivePath string) (appdataHeader, appdataTrailer, error) {
	var hdr appdataHeader
	var trailer appdataTrailer
	in, err := os.Open(archivePath)
	if err != nil {
		return hdr, trailer, fmt.Errorf("opening archive: %w", err)
	}
	defer func() { _ = in.Close() }()
	zr, err := zstd.NewReader(in)
	if err != nil {
		return hdr, trailer, fmt.Errorf("creating zstd reader: %w", err)
	}
	defer zr.Close()
	stream := &laggingHash{h: sha256.New(), lag: tarBlock}
	tr := tar.NewReader(io.TeeReader(zr, stream))

	first, err := tr.Next()
	if err != nil {
		return hdr, trailer, fmt.Errorf("reading archive header: %w", err)
	}
	if first.Name != appdataHeaderName {
		return hdr, trailer, fmt.Errorf("archive starts with %q, not its header", first.Name)
	}
	if err := readAppdataMeta(tr, first, &hdr); err != nil {
		return hdr, trailer, err
	}
	if hdr.Version != appdataFormatVersion {
		return hdr, trailer, fmt.Errorf("archive format version %d is not supported", hdr.Version)
	}

	var got appdataTrailer
	var written *appdataTrailer
	for {
		e, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return hdr, trailer, fmt.Errorf("reading archive: %w", err)
		}
		if written != nil {
			return hdr, trailer, errors.New("archive has entries after its trailer")
		}
		if e.Name == appdataTrailerName {
			got.SHA256 = stream.sum()
			var t appdataTrailer
			if err := readAppdataMeta(tr, e, &t); err != nil {
				return hdr, trailer, err
			}
			written = &t
			continue
		}
		if _, _, ok := appdataEntryDir(e.Name, len(hdr.Dirs)); !ok {
			return hdr, trailer, fmt.Errorf("archive entry %q is outside its directories", e.Name)
		}
		if e.Typeflag != tar.TypeReg {
			continue
		}
		n, err := io.Copy(io.Discard, tr)
		if err != nil {
			return hdr, trailer, fmt.Errorf("reading %q: %w", e.Name, err)
		}
		got.Files++
		got.Bytes += n
	}
	if written == nil {
		return hdr, trailer, errors.New("archive has no trailer: it is incomplete")
	}
	rest, err := io.Copy(io.Discard, zr)
	if err != nil {
		return hdr, trailer, fmt.Errorf("reading the end of the archive: %w", err)
	}
	if rest > 0 {
		return hdr, trailer, errors.New("archive has data after its end")
	}
	if got.Files != written.Files || got.Bytes != written.Bytes || got.SHA256 != written.SHA256 {
		return hdr, trailer, fmt.Errorf("archive content does not match its trailer (read %d files, %d bytes; trailer says %d files, %d bytes, or its checksum differs)", got.Files, got.Bytes, written.Files, written.Bytes)
	}
	return hdr, *written, nil
}

func readAppdataMeta(r io.Reader, e *tar.Header, into any) error {
	if e.Size > appdataMetaLimit {
		return fmt.Errorf("archive entry %q is too large", e.Name)
	}
	raw, err := io.ReadAll(io.LimitReader(r, appdataMetaLimit))
	if err != nil {
		return fmt.Errorf("reading %q: %w", e.Name, err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("decoding %q: %w", e.Name, err)
	}
	return nil
}

// appdataEntryDir splits "data/<i>/<rest>" into i and rest, and reports
// false for a name that is not under one of n trees.
func appdataEntryDir(name string, n int) (int, string, bool) {
	rest, ok := strings.CutPrefix(name, "data/")
	if !ok {
		return 0, "", false
	}
	idx, rel, _ := strings.Cut(rest, "/")
	i, err := strconv.Atoi(idx)
	if err != nil || i < 0 || i >= n || strconv.Itoa(i) != idx {
		return 0, "", false
	}
	return i, rel, true
}

type appdataDirMeta struct {
	path string
	hdr  *tar.Header
}

// extractAppdata unpacks the archive's trees into targets, one directory
// per tree, each of which must not exist yet. It creates only directories,
// regular files and symbolic links, and refuses an entry that would be
// written through a symbolic link the archive itself created, so nothing
// lands outside its target. Ownership is restored when running as root.
func extractAppdata(ctx context.Context, archivePath string, hdr appdataHeader, targets []string) error {
	if len(targets) != len(hdr.Dirs) {
		return errors.New("extracting appdata: one target per archived directory is required")
	}
	roots := make([]string, len(targets))
	for i, t := range targets {
		if err := os.Mkdir(t, 0o700); err != nil {
			return fmt.Errorf("creating %s: %w", t, err)
		}
		real, err := filepath.EvalSymlinks(t)
		if err != nil {
			return fmt.Errorf("resolving %s: %w", t, err)
		}
		roots[i] = real
	}

	in, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("opening archive: %w", err)
	}
	defer func() { _ = in.Close() }()
	zr, err := zstd.NewReader(in)
	if err != nil {
		return fmt.Errorf("creating zstd reader: %w", err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)

	asRoot := os.Geteuid() == 0
	checked := map[string]bool{}
	var dirs []appdataDirMeta
	for {
		e, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("reading archive: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.Name == appdataHeaderName || e.Name == appdataTrailerName {
			continue
		}
		i, rel, ok := appdataEntryDir(e.Name, len(targets))
		if !ok {
			return fmt.Errorf("archive entry %q is outside its directories", e.Name)
		}
		rel = path.Clean(strings.TrimSuffix(rel, "/"))
		if rel == "." || rel == "" {
			dirs = append(dirs, appdataDirMeta{path: targets[i], hdr: e})
			continue
		}
		if !filepath.IsLocal(filepath.FromSlash(rel)) {
			return fmt.Errorf("archive entry %q escapes its directory", e.Name)
		}
		dst := filepath.Join(targets[i], filepath.FromSlash(rel))
		if err := requireInside(roots[i], filepath.Dir(dst), checked); err != nil {
			return fmt.Errorf("archive entry %q: %w", e.Name, err)
		}
		switch e.Typeflag {
		case tar.TypeDir:
			if err := os.Mkdir(dst, 0o700); err != nil {
				return fmt.Errorf("creating %s: %w", dst, err)
			}
			dirs = append(dirs, appdataDirMeta{path: dst, hdr: e})
		case tar.TypeReg:
			if err := extractAppdataFile(tr, e, dst, asRoot); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.Symlink(e.Linkname, dst); err != nil {
				return fmt.Errorf("creating symlink %s: %w", dst, err)
			}
			if asRoot {
				if err := os.Lchown(dst, e.Uid, e.Gid); err != nil {
					return fmt.Errorf("restoring owner of %s: %w", dst, err)
				}
			}
		default:
			return fmt.Errorf("archive entry %q has an unsupported type", e.Name)
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		m := dirs[i]
		if err := applyAppdataMeta(m.path, m.hdr, asRoot); err != nil {
			return err
		}
	}
	return nil
}

func requireInside(root, dir string, checked map[string]bool) error {
	if checked[dir] {
		return nil
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", dir, err)
	}
	if real != root && !strings.HasPrefix(real, root+string(filepath.Separator)) {
		return fmt.Errorf("%s resolves outside %s", dir, root)
	}
	checked[dir] = true
	return nil
}

func extractAppdataFile(r io.Reader, e *tar.Header, dst string, asRoot bool) error {
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("creating %s: %w", dst, err)
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing %s: %w", dst, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", dst, err)
	}
	return applyAppdataMeta(dst, e, asRoot)
}

func applyAppdataMeta(p string, e *tar.Header, asRoot bool) error {
	if asRoot {
		if err := os.Lchown(p, e.Uid, e.Gid); err != nil {
			return fmt.Errorf("restoring owner of %s: %w", p, err)
		}
	}
	mode := e.FileInfo().Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
	if err := os.Chmod(p, mode); err != nil {
		return fmt.Errorf("restoring mode of %s: %w", p, err)
	}
	if err := os.Chtimes(p, e.ModTime, e.ModTime); err != nil {
		return fmt.Errorf("restoring times of %s: %w", p, err)
	}
	return nil
}

// syncAppdataTree flushes every regular file and directory under root to
// disk, so a tree that is about to be renamed into place is not left
// partly empty by a power loss right after the rename.
func syncAppdataTree(root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && !d.Type().IsRegular() {
			return nil
		}
		if err := fsyncPath(p); err != nil {
			return fmt.Errorf("syncing %s: %w", p, err)
		}
		return nil
	})
}
