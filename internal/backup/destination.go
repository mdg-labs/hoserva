package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mdg-labs/hoserva/internal/beneath"
)

// archiveNamePattern matches doc 10 §1's archive filenames. The first group
// is the id of the installation that wrote the archive, absent on an
// archive written before archives carried one. The timestamp group accepts
// both the original minute resolution
// (hoserva-config-2026-09-14T03-00.tar.zst, still on disk from before
// #401) and the current second resolution
// (hoserva-config-<id>-2026-09-14T03-00-05.tar.zst) that fix added, an
// optional "-<n>" collision counter (#401), an optional ".<reason>"
// pre-change marker (#401), and the same name with a trailing ".age" when
// the archive written to a destination was age-encrypted (Q80).
var archiveNamePattern = regexp.MustCompile(`^hoserva-config-(?:([0-9a-f]{12})-)?(\d{4}-\d{2}-\d{2}T\d{2}-\d{2}(?:-\d{2})?)(?:-\d+)?(?:\.(pre-import|pre-update|pre-topology))?\.tar\.zst(\.age)?$`)

// DestinationType is how a destination is reached (doc 10 §1). A local
// path is written directly; every other type goes through rclone.
type DestinationType string

const (
	TypeLocal  DestinationType = "local"
	TypeSMB    DestinationType = "smb"
	TypeS3     DestinationType = "s3"
	TypeSFTP   DestinationType = "sftp"
	TypeWebDAV DestinationType = "webdav"
	TypeRclone DestinationType = "rclone"
)

// Destination is one backup target (doc 10 §1, Q40). The zero Type is a
// local path. Encrypt requests age encryption of the archive written here
// (Q80): local destinations may opt in, and every remote destination has
// it set, after ValidateRemoteDestination has already refused one with no
// backup passphrase configured.
type Destination struct {
	ID   string
	Name string
	Type DestinationType
	// Path is the directory for a local destination; for a remote one it
	// is the path within the remote (bucket and prefix, share and folder).
	Path      string
	Enabled   bool
	Encrypt   bool
	Retention Retention

	// Options are the non-secret rclone backend settings of a remote
	// destination, keyed by rclone's own option names.
	Options map[string]string
	// SealedSecrets is the machine-key ciphertext of the remote
	// destination's credentials, as stored. Secrets holds them in plain
	// text, and is only ever populated for the duration of one operation.
	SealedSecrets []byte
	Secrets       map[string]string

	LastSuccessfulBackupAt *time.Time
	StaleAlertedAt         *time.Time
	// EnabledAt is when the destination was last switched from disabled
	// to enabled; nil until it first was.
	EnabledAt *time.Time
	CreatedAt time.Time
}

func (d Destination) isRemote() bool {
	return d.Type != "" && d.Type != TypeLocal
}

// Retention is the grandfather-father-son policy doc 10 §1 describes.
type Retention struct {
	Daily   int
	Weekly  int
	Monthly int
}

type archiveEntry struct {
	name    string
	path    string
	modTime time.Time
	reason  Reason
	// installation is the id of the installation that wrote the archive;
	// empty for a legacy archive whose name carries none.
	installation string
}

// archiveOwner says which archives on a destination one installation may
// prune. A destination can be shared with other installations (one storage
// box, one bucket prefix, one NFS mount), so retention only ever removes
// archives that carry this installation's id. A legacy archive, written
// before names carried an id, is only claimed on a destination this
// installation alone has written to: the default boot and pool
// destinations.
type archiveOwner struct {
	installation string
	legacy       bool
}

func (o archiveOwner) owns(e archiveEntry) bool {
	if e.installation == "" {
		return o.legacy
	}
	return e.installation == o.installation
}

// openDestinationDir opens the local destination directory path. A path at or
// under poolRoot is reached by opening poolRoot the ordinary way and walking
// every component below it with O_NOFOLLOW, so a symbolic link in any of them,
// the directory itself included, is an error wrapping beneath.ErrSymlink and
// nothing is resolved by name a second time. The pooled view is where a
// container with the pool mapped can replace the default destination with a
// link, and root must not then write or prune wherever that link points (doc
// 15 T2). A poolRoot that does not exist yet is created along with the destination. Any other path, including an empty poolRoot, is opened the ordinary
// way, so a link on it is followed. With create, missing directories are made
// private (0700); one that already exists keeps its mode and owner, since it
// may be a share other users rely on. The caller closes the descriptor.
func openDestinationDir(poolRoot, path string, create bool) (int, error) {
	if !filepath.IsAbs(path) {
		return -1, fmt.Errorf("destination %q is not an absolute path", path)
	}
	path = filepath.Clean(path)
	if poolRoot == "" || !underPoolRoot(path, poolRoot) {
		if create {
			if err := os.MkdirAll(path, 0o700); err != nil {
				return -1, err
			}
		}
		fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			return -1, fmt.Errorf("opening destination %q: %w", path, err)
		}
		return fd, nil
	}
	root := filepath.Clean(poolRoot)
	if create {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return -1, err
		}
	}
	fd, err := beneath.OpenRoot(root)
	if err != nil {
		return -1, fmt.Errorf("opening destination %q: %w", path, err)
	}
	for _, c := range strings.Split(strings.Trim(strings.TrimPrefix(path, root), "/"), "/") {
		if c == "" {
			continue
		}
		next, err := beneath.Open(fd, c, unix.O_RDONLY|unix.O_DIRECTORY)
		if create && errors.Is(err, unix.ENOENT) {
			if mkErr := unix.Mkdirat(fd, c, 0o700); mkErr != nil && !errors.Is(mkErr, unix.EEXIST) {
				_ = unix.Close(fd)
				return -1, fmt.Errorf("creating %q in %q: %w", c, path, mkErr)
			}
			next, err = beneath.Open(fd, c, unix.O_RDONLY|unix.O_DIRECTORY)
		}
		_ = unix.Close(fd)
		if err != nil {
			return -1, fmt.Errorf("opening destination %q: %w", path, err)
		}
		fd = next
	}
	return fd, nil
}

// singleName refuses a file name that is not one path component.
func singleName(name string) error {
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		return fmt.Errorf("%q is not a file name", name)
	}
	return nil
}

func tempSuffix() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// writeArchive copies archivePath into dest.Path under its basename. A
// destination directory it has to create is private (0700); one that
// already exists keeps its mode and owner, since it may be a share other
// users rely on. The archive itself is always written 0600. The directory is
// opened by openDestinationDir and the temporary file, rename and sync all
// go through its descriptor.
func writeArchive(dest Destination, archivePath string) error {
	return writeArchiveUnder("", dest, archivePath)
}

// writeArchiveUnder is writeArchive for a destination that may lie under
// poolRoot (see openDestinationDir).
func writeArchiveUnder(poolRoot string, dest Destination, archivePath string) error {
	dirfd, err := openDestinationDir(poolRoot, dest.Path, true)
	if err != nil {
		return fmt.Errorf("creating destination %q: %w", dest.Path, err)
	}
	defer func() { _ = unix.Close(dirfd) }()

	name := filepath.Base(archivePath)
	if err := singleName(name); err != nil {
		return fmt.Errorf("writing archive: %w", err)
	}
	final := filepath.Join(dest.Path, name)

	in, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("opening archive for copy: %w", err)
	}
	defer func() { _ = in.Close() }()

	var out *os.File
	var tmpName string
	for {
		suffix, err := tempSuffix()
		if err != nil {
			return fmt.Errorf("naming temp archive in %q: %w", dest.Path, err)
		}
		tmpName = name + "." + suffix + ".tmp"
		fd, err := unix.Openat(dirfd, tmpName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return fmt.Errorf("creating temp archive in %q: %w", dest.Path, err)
		}
		out = os.NewFile(uintptr(fd), tmpName)
		break
	}
	tmp := filepath.Join(dest.Path, tmpName)
	removeTmp := func() { _ = unix.Unlinkat(dirfd, tmpName, 0) }
	if err := out.Chmod(0o600); err != nil {
		_ = out.Close()
		removeTmp()
		return fmt.Errorf("restricting temp archive %q: %w", tmp, err)
	}
	if _, err := copyFile(out, in); err != nil {
		_ = out.Close()
		removeTmp()
		return fmt.Errorf("copying archive to %q: %w", tmp, err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		removeTmp()
		return fmt.Errorf("syncing temp archive %q: %w", tmp, err)
	}
	if err := out.Close(); err != nil {
		removeTmp()
		return fmt.Errorf("closing temp archive %q: %w", tmp, err)
	}
	if err := unix.Renameat(dirfd, tmpName, dirfd, name); err != nil {
		removeTmp()
		return fmt.Errorf("finalizing archive at %q: %w", final, err)
	}
	if err := unix.Fsync(dirfd); err != nil {
		return fmt.Errorf("syncing directory %s: %w", dest.Path, err)
	}
	return nil
}

// archiveTarget is where a destination's archives live: a directory, or
// an rclone remote.
type archiveTarget interface {
	write(ctx context.Context, srcPath string) error
	list(ctx context.Context) ([]archiveEntry, error)
	// files lists every regular file in the destination's directory,
	// whatever its name.
	files(ctx context.Context) ([]targetFile, error)
	// fetch copies the named file to dstPath, which must not exist.
	fetch(ctx context.Context, name, dstPath string) error
	remove(ctx context.Context, name string) error
	readBack(ctx context.Context, name string) ([]byte, error)
}

// targetFile is one file in a destination's directory.
type targetFile struct {
	name    string
	size    int64
	modTime time.Time
}

type localTarget struct {
	dest     Destination
	poolRoot string
}

func (t localTarget) files(_ context.Context) ([]targetFile, error) {
	dirfd, err := openDestinationDir(t.poolRoot, t.dest.Path, false)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing destination %q: %w", t.dest.Path, err)
	}
	defer func() { _ = unix.Close(dirfd) }()
	names, err := beneath.ReadNames(dirfd)
	if err != nil {
		return nil, fmt.Errorf("listing destination %q: %w", t.dest.Path, err)
	}
	var out []targetFile
	for _, n := range names {
		var st unix.Stat_t
		if unix.Fstatat(dirfd, n, &st, unix.AT_SYMLINK_NOFOLLOW) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
			continue
		}
		out = append(out, targetFile{name: n, size: st.Size, modTime: time.Unix(st.Mtim.Unix())})
	}
	return out, nil
}

// openFile opens the regular file name in dirfd for reading without
// following a link.
func openFile(dirfd int, dir, name string) (*os.File, error) {
	if err := singleName(name); err != nil {
		return nil, err
	}
	fd, err := beneath.Open(dirfd, name, unix.O_RDONLY)
	if err != nil {
		return nil, fmt.Errorf("opening %q: %w", filepath.Join(dir, name), err)
	}
	return os.NewFile(uintptr(fd), name), nil
}

func (t localTarget) fetch(ctx context.Context, name, dstPath string) error {
	dirfd, err := openDestinationDir(t.poolRoot, t.dest.Path, false)
	if err != nil {
		return fmt.Errorf("opening destination %q: %w", t.dest.Path, err)
	}
	defer func() { _ = unix.Close(dirfd) }()
	in, err := openFile(dirfd, t.dest.Path, name)
	if err != nil {
		return fmt.Errorf("fetching %q: %w", name, err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dstPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("creating %q: %w", dstPath, err)
	}
	if _, err := io.Copy(out, ctxReader{ctx: ctx, r: in}); err != nil {
		_ = out.Close()
		_ = os.Remove(dstPath)
		return fmt.Errorf("copying %q: %w", name, err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dstPath)
		return fmt.Errorf("copying %q: %w", name, err)
	}
	return nil
}

// ctxReader stops a copy of a large file when its context ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

func (t localTarget) write(_ context.Context, srcPath string) error {
	return writeArchiveUnder(t.poolRoot, t.dest, srcPath)
}

func (t localTarget) list(_ context.Context) ([]archiveEntry, error) {
	return listArchivesUnder(t.poolRoot, t.dest.Path)
}

func (t localTarget) readBack(_ context.Context, name string) ([]byte, error) {
	dirfd, err := openDestinationDir(t.poolRoot, t.dest.Path, false)
	if err != nil {
		return nil, fmt.Errorf("reading %q back: %w", filepath.Join(t.dest.Path, name), err)
	}
	defer func() { _ = unix.Close(dirfd) }()
	f, err := openFile(dirfd, t.dest.Path, name)
	if err != nil {
		return nil, fmt.Errorf("reading %q back: %w", filepath.Join(t.dest.Path, name), err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("reading %q back: %w", filepath.Join(t.dest.Path, name), err)
	}
	return b, nil
}

func (t localTarget) remove(_ context.Context, name string) error {
	if err := singleName(name); err != nil {
		return fmt.Errorf("removing %q: %w", name, err)
	}
	dirfd, err := openDestinationDir(t.poolRoot, t.dest.Path, false)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("removing %q: %w", filepath.Join(t.dest.Path, name), err)
	}
	defer func() { _ = unix.Close(dirfd) }()
	if err := unix.Unlinkat(dirfd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("removing %q: %w", filepath.Join(t.dest.Path, name), err)
	}
	return nil
}

// pruneDestination enforces dest's retention on a local destination.
func pruneDestination(dest Destination, owner archiveOwner, now time.Time, justWritten string) error {
	return pruneTarget(context.Background(), localTarget{dest: dest}, owner, dest.Retention, now, justWritten)
}

// pruneTarget enforces retention on the archives owner owns — only
// filenames matching archiveNamePattern that carry owner's installation id
// (or, for a legacy owner, no id) are candidates, and an archive another
// installation wrote to the same directory is neither counted towards nor
// removed by this retention. An encrypted archive's
// identity sidecar (identitySidecarSuffix) is never itself a candidate —
// it never matches archiveNamePattern — but is removed alongside its own
// archive so pruning an old encrypted archive never leaves its sidecar
// behind as an orphan.
func pruneTarget(ctx context.Context, t archiveTarget, owner archiveOwner, ret Retention, now time.Time, justWritten string) error {
	listed, err := t.list(ctx)
	if err != nil {
		return err
	}
	var entries []archiveEntry
	for _, e := range listed {
		if owner.owns(e) {
			entries = append(entries, e)
		}
	}
	keep := retentionKeepers(entries, ret, now, justWritten)
	for _, e := range entries {
		if keep[e.name] {
			continue
		}
		if err := t.remove(ctx, e.name+identitySidecarSuffix); err != nil {
			return fmt.Errorf("pruning identity sidecar for %q: %w", e.name, err)
		}
		if err := t.remove(ctx, e.name); err != nil {
			return fmt.Errorf("pruning archive %q: %w", e.name, err)
		}
	}
	return nil
}

func listArchives(dir string) ([]archiveEntry, error) {
	return listArchivesUnder("", dir)
}

func listArchivesUnder(poolRoot, dir string) ([]archiveEntry, error) {
	dirfd, err := openDestinationDir(poolRoot, dir, false)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing destination %q: %w", dir, err)
	}
	defer func() { _ = unix.Close(dirfd) }()
	names, err := beneath.ReadNames(dirfd)
	if err != nil {
		return nil, fmt.Errorf("listing destination %q: %w", dir, err)
	}
	var out []archiveEntry
	for _, n := range names {
		m := archiveNamePattern.FindStringSubmatch(n)
		if m == nil {
			continue
		}
		var st unix.Stat_t
		if unix.Fstatat(dirfd, n, &st, unix.AT_SYMLINK_NOFOLLOW) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
			continue
		}
		out = append(out, archiveEntry{
			name:         n,
			path:         filepath.Join(dir, n),
			modTime:      time.Unix(st.Mtim.Unix()),
			reason:       Reason(m[3]),
			installation: m[1],
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].modTime.After(out[j].modTime)
	})
	return out, nil
}

// preChangeKeepCount bounds how many pre-change archives (#401) survive
// per destination on top of the daily/weekly/monthly tiers below — doc 10
// §1's recommended default, covering a burst of imports or updates on one
// day without growing retention unbounded.
const preChangeKeepCount = 5

// retentionKeepers decides which of entries survive. Ordinary archives fill
// the daily/weekly/monthly tiers against ordinary archives only, so a
// pre-change archive (#401) can neither occupy nor evict an ordinary slot
// (#478). The newest preChangeKeepCount pre-change archives are kept on top.
// A pre-change archive beyond that bound is kept only as the newest of a day,
// week or month that a tier would keep and in which no ordinary archive
// exists, so it fills a hole and never displaces an ordinary archive. The
// tier windows are the most recent periods holding any archive, ordinary or
// pre-change. entries is sorted newest-first (listArchives).
func retentionKeepers(entries []archiveEntry, ret Retention, now time.Time, justWritten string) map[string]bool {
	keep := map[string]bool{justWritten: true}
	if len(entries) == 0 {
		return keep
	}

	preChangeKept := 0
	ordinary := make([]archiveEntry, 0, len(entries))
	overflow := make([]archiveEntry, 0, len(entries))
	for _, e := range entries {
		switch {
		case e.reason == ReasonNone:
			ordinary = append(ordinary, e)
		case preChangeKept < preChangeKeepCount:
			keep[e.name] = true
			preChangeKept++
		default:
			overflow = append(overflow, e)
		}
	}

	tiers := []struct {
		period func(time.Time) string
		count  int
	}{
		{func(t time.Time) string { return t.Format("2006-01-02") }, ret.Daily},
		{func(t time.Time) string {
			year, week := t.ISOWeek()
			return fmt.Sprintf("%04d-W%02d", year, week)
		}, ret.Weekly},
		{func(t time.Time) string { return t.Format("2006-01") }, ret.Monthly},
	}
	for _, tier := range tiers {
		newestOrdinary := newestPerPeriod(ordinary, tier.period)
		for i, period := range sortedKeys(newestOrdinary) {
			if i >= tier.count {
				break
			}
			keep[newestOrdinary[period].name] = true
		}

		newestOverflow := newestPerPeriod(overflow, tier.period)
		windowPeriods := map[string]bool{}
		for _, e := range entries {
			windowPeriods[tier.period(e.modTime)] = true
		}
		for i, period := range sortedKeys(windowPeriods) {
			if i >= tier.count {
				break
			}
			if _, hasOrdinary := newestOrdinary[period]; hasOrdinary {
				continue
			}
			if e, ok := newestOverflow[period]; ok {
				keep[e.name] = true
			}
		}
	}

	return keep
}

func newestPerPeriod(entries []archiveEntry, period func(time.Time) string) map[string]archiveEntry {
	newest := map[string]archiveEntry{}
	for _, e := range entries {
		key := period(e.modTime)
		if cur, ok := newest[key]; !ok || e.modTime.After(cur.modTime) {
			newest[key] = e
		}
	}
	return newest
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	return keys
}
