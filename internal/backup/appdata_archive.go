package backup

import (
	"archive/tar"
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
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
// What does is the trailer's MAC, an HMAC-SHA256 under a key only this
// installation holds (deriveAppdataKey), over the stream's SHA-256, and so
// over the header and every entry, and over the trailer's counts.
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
// is over the tar stream up to the trailer entry. MAC is the hex HMAC that
// authenticates the other fields; an archive written before it existed has
// none.
type appdataTrailer struct {
	Files   int64  `json:"files"`
	Bytes   int64  `json:"bytes"`
	Skipped int64  `json:"skipped"`
	Changed int64  `json:"changed"`
	SHA256  string `json:"sha256"`
	MAC     string `json:"mac,omitempty"`
}

// appdataKeyLabel separates the key appdata archives are authenticated with
// from every other use of the onboarding identity.
const appdataKeyLabel = "hoserva appdata archive authentication v1"

// deriveAppdataKey is the HKDF-SHA256 key, under appdataKeyLabel, of the
// onboarding identity. The identity is stored wrapped under the machine key,
// travels wrapped under the backup passphrase in a config archive's
// identity.age when a backup passphrase is set, and is in the clear only in
// memory. It survives a bare-metal restore (Q80), which is why it keys the
// tag and the machine key, which does not travel in a config backup, does not.
func deriveAppdataKey(identity string) ([]byte, error) {
	if identity == "" {
		return nil, errors.New("no onboarding identity is available to authenticate appdata archives")
	}
	key, err := hkdf.Key(sha256.New, []byte(identity), nil, appdataKeyLabel, sha256.Size)
	if err != nil {
		return nil, fmt.Errorf("deriving the appdata archive key: %w", err)
	}
	return key, nil
}

// mac is the tag of t under key: its stream checksum and its counts, which
// the stream checksum does not cover because the trailer follows the stream.
func (t appdataTrailer) mac(key []byte) string {
	m := hmac.New(sha256.New, key)
	_, _ = fmt.Fprintf(m, "hoserva-appdata-mac-v1\n%s\n%d\n%d\n%d\n%d\n", t.SHA256, t.Files, t.Bytes, t.Skipped, t.Changed)
	return hex.EncodeToString(m.Sum(nil))
}

// authentic reports whether t carries the tag key gives it.
func (t appdataTrailer) authentic(key []byte) bool {
	want, err := hex.DecodeString(t.MAC)
	if err != nil || len(want) == 0 || len(key) == 0 {
		return false
	}
	got, err := hex.DecodeString(t.mac(key))
	return err == nil && hmac.Equal(got, want)
}

func appdataPrefix(i int) string {
	return "data/" + strconv.Itoa(i) + "/"
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// archivedIDs holds, for each directory a packer archived, the device and
// inode of every entry it wrote or skipped under it, as the packer read them.
type archivedIDs map[string]map[devIno]struct{}

type devIno struct{ dev, ino uint64 }

func (set archivedIDs) record(dir string, info fs.FileInfo) {
	if set == nil {
		return
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		set[dir][devIno{uint64(st.Dev), uint64(st.Ino)}] = struct{}{}
	}
}

// packAppdata writes hdr and every tree hdr.Dirs names to dest, through a
// temporary file that is renamed into place only once complete, with the
// trailer authenticated under key. A socket, device or named pipe has no
// content to keep and is skipped and counted.
func packAppdata(ctx context.Context, dest string, hdr appdataHeader, key []byte) (appdataTrailer, error) {
	return packAppdataRecording(ctx, dest, hdr, key, nil)
}

// packAppdataRecording is packAppdata that also records into archived, under
// each directory of hdr.Dirs, the device and inode of every directory, file,
// link and skipped special file it read there, taken from the same lstat or
// open descriptor the entry was archived from. A name that vanished before it
// could be read is not recorded. A nil archived records nothing.
func packAppdataRecording(ctx context.Context, dest string, hdr appdataHeader, key []byte, archived archivedIDs) (appdataTrailer, error) {
	var trailer appdataTrailer
	if len(key) == 0 {
		return trailer, errors.New("an appdata archive is not written without a key to authenticate it")
	}
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
		if archived != nil {
			archived[dir] = map[devIno]struct{}{}
		}
		if err := packAppdataTree(ctx, tw, &trailer, dir, appdataPrefix(i), archived); err != nil {
			_ = zw.Close()
			return fail(fmt.Errorf("archiving %s: %w", dir, err))
		}
	}
	if err := tw.Flush(); err != nil {
		_ = zw.Close()
		return fail(fmt.Errorf("padding the last archived file: %w", err))
	}
	trailer.SHA256 = hex.EncodeToString(sum.Sum(nil))
	trailer.MAC = trailer.mac(key)
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

// packAppdataTree writes the tree below root under prefix. root is opened
// with every component refused if it is a link, and every entry below it is
// opened from its parent's descriptor without following a link, so a
// directory or file a container swaps for a link while the tree is read is
// not followed: it is counted as changed and what it pointed at is never
// read.
func packAppdataTree(ctx context.Context, tw *tar.Writer, trailer *appdataTrailer, root, prefix string, archived archivedIDs) error {
	tarName := func(rel string) string {
		if rel == "" {
			return prefix
		}
		return prefix + rel
	}
	w := appdataWalk{
		ctx: ctx,
		dir: func(rel string, fd int) error {
			info, err := fdInfo(fd)
			if err != nil {
				return err
			}
			h, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			h.Name = strings.TrimSuffix(tarName(rel), "/") + "/"
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			archived.record(root, info)
			return nil
		},
		entry: func(parent int, name, rel string, st *unix.Stat_t) error {
			if st.Mode&unix.S_IFMT == unix.S_IFREG {
				return packAppdataFile(tw, trailer, parent, name, tarName(rel), func(info fs.FileInfo) { archived.record(root, info) })
			}
			return packAppdataLink(tw, trailer, parent, name, tarName(rel), func(info fs.FileInfo) { archived.record(root, info) })
		},
		gone: func() { trailer.Changed++ },
	}
	return w.walk(root)
}

// packAppdataLink archives a symbolic link, or counts a special file as
// skipped. It holds the entry itself, never what a link points at, so the
// header and the target it records are of one object.
func packAppdataLink(tw *tar.Writer, trailer *appdataTrailer, parent int, name, tarName string, archived func(fs.FileInfo)) error {
	fd, err := beneath.Open(parent, name, unix.O_PATH)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			trailer.Changed++
			return nil
		}
		return err
	}
	f := os.NewFile(uintptr(fd), name)
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		link, err := readlinkFd(fd)
		if err != nil {
			return err
		}
		h, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		h.Name = tarName
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		archived(info)
	case info.Mode().IsRegular() || info.IsDir():
		trailer.Changed++
	default:
		trailer.Skipped++
		archived(info)
	}
	return nil
}

func readlinkFd(fd int) (string, error) {
	for size := 256; ; size *= 2 {
		buf := make([]byte, size)
		n, err := unix.Readlinkat(fd, "", buf)
		if err != nil {
			return "", fmt.Errorf("reading a link: %w", err)
		}
		if n < size {
			return string(buf[:n]), nil
		}
	}
}

// fdInfo is the file information of the open descriptor fd, taken from the
// descriptor itself.
func fdInfo(fd int) (fs.FileInfo, error) {
	dup, err := unix.Dup(fd)
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(dup)
	f := os.NewFile(uintptr(dup), "")
	defer func() { _ = f.Close() }()
	return f.Stat()
}

func packAppdataFile(tw *tar.Writer, trailer *appdataTrailer, parent int, name, tarName string, archived func(fs.FileInfo)) error {
	fd, err := beneath.Open(parent, name, unix.O_RDONLY|unix.O_NONBLOCK)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, beneath.ErrSymlink) {
			trailer.Changed++
			return nil
		}
		return err
	}
	f := os.NewFile(uintptr(fd), name)
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.IsDir() {
		trailer.Changed++
		return nil
	}
	if !info.Mode().IsRegular() {
		trailer.Skipped++
		archived(info)
		return nil
	}
	h, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	h.Name = tarName
	if err := tw.WriteHeader(h); err != nil {
		return err
	}
	archived(info)
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

// beforeOpen, when set, runs after the walk has read an entry's type and
// before it opens the entry, so a test can rearrange the tree at that moment.
var beforeOpen func(rel string)

// beforeList, when set, runs after the walk has opened a directory and before
// it lists it, so a test can remove the directory at that moment.
var beforeList func(rel string)

// errWalkRootMissing marks the error of a walk whose root is not there, as
// against one that stops partway.
var errWalkRootMissing = errors.New("the directory to walk is not there")

// appdataWalk reads a tree by descriptors: each directory is opened from its
// parent's descriptor without following a link, listed, and its entries
// visited in name order, a directory before what it holds. An entry that is
// gone, or is a link where a directory was listed, when it is opened is
// reported to gone and not entered; nothing is ever opened through a link.
type appdataWalk struct {
	ctx context.Context
	// dir is called with each directory as it is entered, the root as "".
	dir func(rel string, fd int) error
	// entry is called with each entry that is not a directory, with the
	// descriptor of its parent and what the listing found it to be.
	entry func(parent int, name, rel string, st *unix.Stat_t) error
	gone  func()
}

func (w *appdataWalk) walk(root string) error {
	fd, err := beneath.OpenResolvedDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %w", errWalkRootMissing, err)
		}
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	if err := w.dir("", fd); err != nil {
		return err
	}
	return w.children(fd, "")
}

func (w *appdataWalk) children(dirfd int, rel string) error {
	if beforeList != nil {
		beforeList(rel)
	}
	names, err := beneath.ReadNames(dirfd)
	if err != nil {
		// The kernel refuses to list a directory removed after it was
		// opened; its entries are gone, as an entry removed before its
		// open is.
		if errors.Is(err, unix.ENOENT) {
			w.gone()
			return nil
		}
		return fmt.Errorf("listing %q: %w", rel, err)
	}
	slices.Sort(names)
	for _, name := range names {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		childRel := path.Join(rel, name)
		var st unix.Stat_t
		if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				w.gone()
				continue
			}
			return fmt.Errorf("reading %q: %w", childRel, err)
		}
		if beforeOpen != nil {
			beforeOpen(childRel)
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			if err := w.entry(dirfd, name, childRel, &st); err != nil {
				return err
			}
			continue
		}
		fd, err := beneath.Open(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY)
		if err != nil {
			if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, beneath.ErrSymlink) {
				w.gone()
				continue
			}
			return err
		}
		err = w.dir(childRel, fd)
		if err == nil {
			err = w.children(fd, childRel)
		}
		_ = unix.Close(fd)
		if err != nil {
			return err
		}
	}
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
// recorded. Ownership is restored when running as root. A setuid bit on an
// entry the archive gives to uid 0, and a setgid bit on one it gives to
// gid 0 or to a group in privileged, is not restored; stripped, if set, is
// called for each such entry with its place in the archived directory and
// the bits left off.
func (p *heldDirs) extract(ctx context.Context, archivePath string, hdr appdataHeader, targets []string, trees []liveIdentity, privileged privilegedGIDs, stripped func(path, bits string)) error {
	if len(targets) != len(hdr.Dirs) || len(trees) != len(targets) {
		return errors.New("extracting appdata: one target per archived directory is required")
	}
	note := func(tree int, rel, what string) {
		if what != "" && stripped != nil {
			stripped(filepath.Join(hdr.Dirs[tree], filepath.FromSlash(rel)), what)
		}
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
		what, err := extractAppdataEntry(tr, e, dfd, base, filepath.Join(targets[i], filepath.FromSlash(rel)), asRoot, privileged)
		release()
		if err != nil {
			return err
		}
		note(i, rel, what)
		if e.Typeflag == tar.TypeDir {
			dirs = append(dirs, appdataDirMeta{tree: i, rel: path.Join(resolved, base), hdr: e})
		}
	}
	if beforeAppdataMeta != nil {
		beforeAppdataMeta()
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		m := dirs[i]
		what, err := p.applyDirMeta(walkers[m.tree], targets[m.tree], m, asRoot, privileged)
		if err != nil {
			return err
		}
		note(m.tree, m.rel, what)
	}
	return nil
}

func (p *heldDirs) applyDirMeta(w *beneath.Walker, target string, m appdataDirMeta, asRoot bool, privileged privilegedGIDs) (string, error) {
	if m.rel == "" {
		parent, name, err := p.parent(target)
		if err != nil {
			return "", err
		}
		return applyAppdataMeta(w.Root(), parent, name, target, m.hdr, asRoot, privileged)
	}
	dir, base := path.Split(m.rel)
	pfd, err := w.Dir(strings.TrimSuffix(dir, "/"), nil)
	if err != nil {
		return "", fmt.Errorf("restoring %s: %w", filepath.Join(target, filepath.FromSlash(m.rel)), err)
	}
	fd, err := beneath.Open(pfd, base, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return "", fmt.Errorf("restoring %s: %w", filepath.Join(target, filepath.FromSlash(m.rel)), err)
	}
	defer func() { _ = unix.Close(fd) }()
	return applyAppdataMeta(fd, pfd, base, filepath.Join(target, filepath.FromSlash(m.rel)), m.hdr, asRoot, privileged)
}

// extractAppdataEntry creates one entry. A non-empty first result names the
// mode bits that were not restored (see applyAppdataMeta).
func extractAppdataEntry(r io.Reader, e *tar.Header, dfd int, base, dst string, asRoot bool, privileged privilegedGIDs) (string, error) {
	switch e.Typeflag {
	case tar.TypeDir:
		if err := unix.Mkdirat(dfd, base, 0o700); err != nil {
			return "", fmt.Errorf("creating %s: %w", dst, err)
		}
	case tar.TypeReg:
		return extractAppdataFile(r, e, dfd, base, dst, asRoot, privileged)
	case tar.TypeSymlink:
		if err := unix.Symlinkat(e.Linkname, dfd, base); err != nil {
			return "", fmt.Errorf("creating symlink %s: %w", dst, err)
		}
		if asRoot {
			if err := unix.Fchownat(dfd, base, e.Uid, e.Gid, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				return "", fmt.Errorf("restoring owner of %s: %w", dst, err)
			}
		}
	default:
		return "", fmt.Errorf("archive entry %q has an unsupported type", e.Name)
	}
	return "", nil
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

func extractAppdataFile(r io.Reader, e *tar.Header, dirfd int, name, display string, asRoot bool, privileged privilegedGIDs) (string, error) {
	fd, err := unix.Openat(dirfd, name, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return "", fmt.Errorf("creating %s: %w", display, err)
	}
	f := os.NewFile(uintptr(fd), name)
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("writing %s: %w", display, err)
	}
	stripped, err := applyAppdataMeta(fd, dirfd, name, display, e, asRoot, privileged)
	if err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("writing %s: %w", display, err)
	}
	return stripped, nil
}

// applyAppdataMeta sets ownership and mode through fd, and times through
// the entry's name in the directory dirfd without following a link there.
// The setuid bit of an entry archived as owned by uid 0 and the setgid bit of
// one archived with gid 0 or a gid in privileged are left off, and the first
// result names which ("setuid", "setgid" or both).
func applyAppdataMeta(fd, dirfd int, name, display string, e *tar.Header, asRoot bool, privileged privilegedGIDs) (string, error) {
	if asRoot {
		if err := unix.Fchown(fd, e.Uid, e.Gid); err != nil {
			return "", fmt.Errorf("restoring owner of %s: %w", display, err)
		}
	}
	mode, stripped := restoredMode(e, privileged)
	if err := unix.Fchmod(fd, mode); err != nil {
		return "", fmt.Errorf("restoring mode of %s: %w", display, err)
	}
	ts := unix.NsecToTimespec(e.ModTime.UnixNano())
	if err := unix.UtimesNanoAt(dirfd, name, []unix.Timespec{ts, ts}, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return "", fmt.Errorf("restoring times of %s: %w", display, err)
	}
	return stripped, nil
}

// privilegedGroupNames are the groups on a host whose members, or whose
// setgid programs, reach root: hoserva (the API socket, Q44), sudo, disk and
// kmem (raw devices and memory), docker and libvirt (the engines), shadow
// (root's secrets).
var privilegedGroupNames = []string{"hoserva", "sudo", "disk", "docker", "kmem", "shadow", "libvirt"}

// privilegedGIDs is the set of group ids a restore never gives a setgid bit
// to, besides gid 0.
type privilegedGIDs map[int]bool

func (g privilegedGIDs) has(gid int) bool {
	return gid == 0 || g[gid]
}

// resolvePrivilegedGroups reads the host's gid for each privilegedGroupNames
// entry through lookup. A group the host does not have is left out; any
// other failure is returned, so a restore does not go on with a set that may
// be missing a group.
func resolvePrivilegedGroups(lookup func(name string) (*user.Group, error)) (privilegedGIDs, error) {
	out := privilegedGIDs{}
	for _, name := range privilegedGroupNames {
		g, err := lookup(name)
		if err != nil {
			var unknown user.UnknownGroupError
			if errors.As(err, &unknown) {
				continue
			}
			return nil, fmt.Errorf("reading the privileged group %s: %w", name, err)
		}
		gid, err := strconv.Atoi(g.Gid)
		if err != nil {
			return nil, fmt.Errorf("reading the privileged group %s: gid %q: %w", name, g.Gid, err)
		}
		out[gid] = true
	}
	return out, nil
}

// restoredMode is the mode an archived entry is restored with: what the
// archive holds, less a setuid bit on an entry given to uid 0 and a setgid
// bit on one given to gid 0 or to a group in privileged, which would hand a
// root-owned program or a root-equivalent group to whatever the archive
// says. The second result names the bits left off.
func restoredMode(e *tar.Header, privileged privilegedGIDs) (uint32, string) {
	mode := unixMode(e.FileInfo().Mode())
	var left []string
	if mode&unix.S_ISUID != 0 && e.Uid == 0 {
		mode &^= unix.S_ISUID
		left = append(left, "setuid")
	}
	if mode&unix.S_ISGID != 0 && privileged.has(e.Gid) {
		mode &^= unix.S_ISGID
		left = append(left, "setgid")
	}
	return mode, strings.Join(left, " and ")
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
