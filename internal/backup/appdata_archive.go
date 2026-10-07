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
	"golang.org/x/sys/unix"

	"github.com/mdg-labs/hoserva/internal/beneath"
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
	tree int
	rel  string
	hdr  *tar.Header
}

// beforeAppdataMeta, when set, runs after the archive's entries are written
// and before ownership, modes and times are applied to its directories, so
// a test can rearrange the tree at that moment.
var beforeAppdataMeta func()

// extract unpacks the archive's trees into targets, one directory per tree,
// each of which must not exist yet. Everything is done relative to
// descriptors: a target is created in its held parent and every entry is
// created, and every directory's ownership, mode and times are applied,
// through the descriptor of the directory it lies in, which is opened from
// the one above with O_NOFOLLOW. It creates only directories, regular files
// and symbolic links, and refuses an entry that would be written through a
// symbolic link, so nothing lands outside its target. Each target's
// descriptor must pass checkCreatedDir, and its identity is recorded in
// trees; a target that fails the check keeps what it holds and is not
// recorded. Ownership is restored when running as root.
func (p *heldDirs) extract(ctx context.Context, archivePath string, hdr appdataHeader, targets []string, trees []liveIdentity) error {
	if len(targets) != len(hdr.Dirs) || len(trees) != len(targets) {
		return errors.New("extracting appdata: one target per archived directory is required")
	}
	walkers := make([]*beneath.Walker, len(targets))
	defer func() {
		for _, w := range walkers {
			if w != nil {
				w.Close()
			}
		}
	}()
	for i, t := range targets {
		parent, name, err := p.parent(t)
		if err != nil {
			return err
		}
		if err := unix.Mkdirat(parent, name, 0o700); err != nil {
			return fmt.Errorf("creating %s: %w", t, err)
		}
		if beforeAppdataTreeOpen != nil {
			beforeAppdataTreeOpen(t)
		}
		fd, err := beneath.Open(parent, name, unix.O_RDONLY|unix.O_DIRECTORY)
		if err != nil {
			return fmt.Errorf("opening %s: %w", t, err)
		}
		id, err := checkCreatedDir(fd, t)
		if err != nil {
			_ = unix.Close(fd)
			return fmt.Errorf("refusing the tree: %w", err)
		}
		trees[i] = id
		walkers[i] = beneath.NewWalkerAt(fd)
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
			dirs = append(dirs, appdataDirMeta{tree: i, hdr: e})
			continue
		}
		if !filepath.IsLocal(filepath.FromSlash(rel)) {
			return fmt.Errorf("archive entry %q escapes its directory", e.Name)
		}
		dir, base := path.Split(rel)
		dfd, resolved, release, err := openTreeDir(walkers[i], strings.TrimSuffix(dir, "/"))
		if err != nil {
			return fmt.Errorf("archive entry %q: %w", e.Name, err)
		}
		err = extractAppdataEntry(tr, e, dfd, base, filepath.Join(targets[i], filepath.FromSlash(rel)), asRoot)
		release()
		if err != nil {
			return err
		}
		if e.Typeflag == tar.TypeDir {
			dirs = append(dirs, appdataDirMeta{tree: i, rel: path.Join(resolved, base), hdr: e})
		}
	}
	if beforeAppdataMeta != nil {
		beforeAppdataMeta()
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		m := dirs[i]
		if err := p.applyDirMeta(walkers[m.tree], targets[m.tree], m, asRoot); err != nil {
			return err
		}
	}
	return nil
}

func (p *heldDirs) applyDirMeta(w *beneath.Walker, target string, m appdataDirMeta, asRoot bool) error {
	if m.rel == "" {
		parent, name, err := p.parent(target)
		if err != nil {
			return err
		}
		return applyAppdataMeta(w.Root(), parent, name, target, m.hdr, asRoot)
	}
	dir, base := path.Split(m.rel)
	pfd, err := w.Dir(strings.TrimSuffix(dir, "/"), nil)
	if err != nil {
		return fmt.Errorf("restoring %s: %w", filepath.Join(target, filepath.FromSlash(m.rel)), err)
	}
	fd, err := beneath.Open(pfd, base, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return fmt.Errorf("restoring %s: %w", filepath.Join(target, filepath.FromSlash(m.rel)), err)
	}
	defer func() { _ = unix.Close(fd) }()
	return applyAppdataMeta(fd, pfd, base, filepath.Join(target, filepath.FromSlash(m.rel)), m.hdr, asRoot)
}

func extractAppdataEntry(r io.Reader, e *tar.Header, dfd int, base, dst string, asRoot bool) error {
	switch e.Typeflag {
	case tar.TypeDir:
		if err := unix.Mkdirat(dfd, base, 0o700); err != nil {
			return fmt.Errorf("creating %s: %w", dst, err)
		}
	case tar.TypeReg:
		return extractAppdataFile(r, e, dfd, base, dst, asRoot)
	case tar.TypeSymlink:
		if err := unix.Symlinkat(e.Linkname, dfd, base); err != nil {
			return fmt.Errorf("creating symlink %s: %w", dst, err)
		}
		if asRoot {
			if err := unix.Fchownat(dfd, base, e.Uid, e.Gid, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				return fmt.Errorf("restoring owner of %s: %w", dst, err)
			}
		}
	default:
		return fmt.Errorf("archive entry %q has an unsupported type", e.Name)
	}
	return nil
}

// maxTreeLinks is how many symbolic links resolving one directory may
// follow, as many as the operating system follows.
const maxTreeLinks = 255

// openTreeDir opens the directory rel below the walker's root, following
// the symbolic links the archive itself created as long as they stay inside
// the tree: it returns the directory's descriptor, its path below the root
// with every link resolved, and a function that releases the descriptor.
// Links are resolved here, one component at a time, through descriptors: an
// absolute target, one climbing above the root, a missing component and
// anything that is not a directory are refused.
func openTreeDir(w *beneath.Walker, rel string) (int, string, func(), error) {
	if fd, err := w.Dir(rel, nil); err == nil {
		return fd, rel, func() {}, nil
	} else if !errors.Is(err, beneath.ErrSymlink) {
		return -1, "", nil, err
	}
	var held []int
	var names []string
	release := func() {
		for _, fd := range held {
			_ = unix.Close(fd)
		}
	}
	cur := w.Root()
	todo := strings.Split(rel, "/")
	links := 0
	for len(todo) > 0 {
		c := todo[0]
		todo = todo[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			if len(held) == 0 {
				release()
				return -1, "", nil, errors.New("resolves outside its directory")
			}
			_ = unix.Close(held[len(held)-1])
			held, names = held[:len(held)-1], names[:len(names)-1]
			if cur = w.Root(); len(held) > 0 {
				cur = held[len(held)-1]
			}
			continue
		}
		var st unix.Stat_t
		if err := unix.Fstatat(cur, c, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			release()
			return -1, "", nil, fmt.Errorf("resolving %s: %w", c, err)
		}
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFLNK:
			if links++; links > maxTreeLinks {
				release()
				return -1, "", nil, fmt.Errorf("%s is part of a loop of links", c)
			}
			buf := make([]byte, unix.PathMax)
			n, err := unix.Readlinkat(cur, c, buf)
			if err != nil {
				release()
				return -1, "", nil, fmt.Errorf("reading link %s: %w", c, err)
			}
			target := string(buf[:n])
			if strings.HasPrefix(target, "/") {
				release()
				return -1, "", nil, fmt.Errorf("%s resolves outside its directory", c)
			}
			todo = append(strings.Split(target, "/"), todo...)
		case unix.S_IFDIR:
			fd, err := beneath.Open(cur, c, unix.O_RDONLY|unix.O_DIRECTORY)
			if err != nil {
				release()
				return -1, "", nil, err
			}
			held, names = append(held, fd), append(names, c)
			cur = fd
		default:
			release()
			return -1, "", nil, fmt.Errorf("%s is not a directory", c)
		}
	}
	if len(held) == 0 {
		fd, err := unix.FcntlInt(uintptr(w.Root()), unix.F_DUPFD_CLOEXEC, 0)
		if err != nil {
			return -1, "", nil, err
		}
		return fd, "", func() { _ = unix.Close(fd) }, nil
	}
	return cur, strings.Join(names, "/"), release, nil
}

func extractAppdataFile(r io.Reader, e *tar.Header, dirfd int, name, display string, asRoot bool) error {
	fd, err := unix.Openat(dirfd, name, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("creating %s: %w", display, err)
	}
	f := os.NewFile(uintptr(fd), name)
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing %s: %w", display, err)
	}
	if err := applyAppdataMeta(fd, dirfd, name, display, e, asRoot); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", display, err)
	}
	return nil
}

// applyAppdataMeta sets ownership and mode through fd, and times through
// the entry's name in the directory dirfd without following a link there.
func applyAppdataMeta(fd, dirfd int, name, display string, e *tar.Header, asRoot bool) error {
	if asRoot {
		if err := unix.Fchown(fd, e.Uid, e.Gid); err != nil {
			return fmt.Errorf("restoring owner of %s: %w", display, err)
		}
	}
	if err := unix.Fchmod(fd, unixMode(e.FileInfo().Mode())); err != nil {
		return fmt.Errorf("restoring mode of %s: %w", display, err)
	}
	ts := unix.NsecToTimespec(e.ModTime.UnixNano())
	if err := unix.UtimesNanoAt(dirfd, name, []unix.Timespec{ts, ts}, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("restoring times of %s: %w", display, err)
	}
	return nil
}

func unixMode(m fs.FileMode) uint32 {
	v := uint32(m.Perm())
	if m&fs.ModeSetuid != 0 {
		v |= unix.S_ISUID
	}
	if m&fs.ModeSetgid != 0 {
		v |= unix.S_ISGID
	}
	if m&fs.ModeSticky != 0 {
		v |= unix.S_ISVTX
	}
	return v
}

// syncAppdataTree flushes every regular file and directory under the
// directory dirfd to disk, so a tree that is about to be renamed into place
// is not left partly empty by a power loss right after the rename. It
// descends through descriptors and follows no link.
func syncAppdataTree(dirfd int, display string) error {
	if err := unix.Fsync(dirfd); err != nil {
		return fmt.Errorf("syncing %s: %w", display, err)
	}
	names, err := beneath.ReadNames(dirfd)
	if err != nil {
		return fmt.Errorf("reading %s: %w", display, err)
	}
	for _, n := range names {
		var st unix.Stat_t
		if err := unix.Fstatat(dirfd, n, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("syncing %s: %w", filepath.Join(display, n), err)
		}
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			fd, err := beneath.Open(dirfd, n, unix.O_RDONLY|unix.O_DIRECTORY)
			if err != nil {
				return fmt.Errorf("syncing %s: %w", filepath.Join(display, n), err)
			}
			err = syncAppdataTree(fd, filepath.Join(display, n))
			_ = unix.Close(fd)
			if err != nil {
				return err
			}
		case unix.S_IFREG:
			fd, err := beneath.Open(dirfd, n, unix.O_RDONLY|unix.O_NONBLOCK)
			if err != nil {
				return fmt.Errorf("syncing %s: %w", filepath.Join(display, n), err)
			}
			err = unix.Fsync(fd)
			_ = unix.Close(fd)
			if err != nil {
				return fmt.Errorf("syncing %s: %w", filepath.Join(display, n), err)
			}
		}
	}
	return nil
}
