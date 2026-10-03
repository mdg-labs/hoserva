package disk

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// ReadOnlyMounter mounts a filesystem so that nothing Hoserva does can write to
// it, for a device that is the user's rollback or backup and not Hoserva's to
// change: the Unraid USB stick (doc 05 §3, Q25). It is not the external-disk
// path (external.go), which mounts read-write for containers.
//
// A mount is judged from the kernel's mount table, never from the exit status
// of the command that made it: MountReadOnly returns nil only once the table
// shows the filesystem at where, read-only both on the mount and on the
// superblock, and a probe of the device itself reports the filesystem UUID asked
// for.
type ReadOnlyMounter interface {
	// MountReadOnly mounts the device node device, whose filesystem of type
	// fsType has UUID uuid, at where. The device node is what is mounted, so
	// nothing is looked up by UUID (that lookup needs udev's links); uuid is
	// what the caller validated and is confirmed against the device after the
	// mount. It refuses a where that is already a mountpoint, and an fsType
	// whose no-write options are not known. When it cannot show the mount is
	// read-only and of that UUID it unmounts again and returns an error.
	MountReadOnly(ctx context.Context, fsType, device, uuid, where string) error
	// Unmount unmounts where, which is success when it is not a mountpoint. It
	// returns nil only once the mount table no longer lists where.
	Unmount(ctx context.Context, where string) error
	// IsMounted reports whether where is a mountpoint in the kernel's table.
	IsMounted(ctx context.Context, where string) (bool, error)
}

// ErrReadOnlyMount is wrapped by every refusal or failure of a read-only mount.
var ErrReadOnlyMount = errors.New("disk: read-only mount")

// readOnlyOptions are the mount options per filesystem type: ro first, then
// the options that stop a read from becoming a write (no access-time updates)
// or a file on the device from acting (no setuid, device nodes or execution).
// FAT's long names are read as UTF-8 whatever the kernel's default character
// set is, so a name with an accent is the one the zip of the same flash holds.
// A type that is not here is refused rather than mounted with a guess.
var readOnlyOptions = map[string]string{
	"vfat": "ro,noatime,nodiratime,nosuid,nodev,noexec,utf8=1",
}

// KernelReadOnlyMounter is the real ReadOnlyMounter: mount(8) and umount(8) as
// argvs through Runner, then the mount table to check what they did.
type KernelReadOnlyMounter struct {
	Runner Runner
	// MountInfo is the mountinfo file to read; empty uses DefaultMountInfo.
	MountInfo string
}

func (k KernelReadOnlyMounter) kernel() KernelMounts {
	return KernelMounts(k)
}

func (k KernelReadOnlyMounter) mountInfo() string {
	if k.MountInfo == "" {
		return DefaultMountInfo
	}
	return k.MountInfo
}

// IsMounted implements ReadOnlyMounter.
func (k KernelReadOnlyMounter) IsMounted(ctx context.Context, where string) (bool, error) {
	return k.kernel().IsMounted(ctx, where)
}

// MountReadOnly implements ReadOnlyMounter.
func (k KernelReadOnlyMounter) MountReadOnly(ctx context.Context, fsType, device, uuid, where string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	opts, ok := readOnlyOptions[fsType]
	if !ok {
		return fmt.Errorf("%w: no read-only options are known for filesystem type %q", ErrReadOnlyMount, fsType)
	}
	if !strings.HasPrefix(device, "/dev/") || strings.ContainsAny(device, "\x00\n") {
		return fmt.Errorf("%w: %q is not a device node", ErrReadOnlyMount, device)
	}
	if uuid == "" || strings.HasPrefix(uuid, "-") {
		return fmt.Errorf("%w: %q is not a filesystem UUID", ErrReadOnlyMount, uuid)
	}
	if err := os.MkdirAll(where, 0o700); err != nil {
		return fmt.Errorf("%w: creating %s: %v", ErrReadOnlyMount, where, err)
	}
	// The mount table lists the resolved path, so the path checked is that one.
	where, err := filepath.EvalSymlinks(where)
	if err != nil {
		return fmt.Errorf("%w: resolving the mountpoint: %v", ErrReadOnlyMount, err)
	}
	if mounted, err := k.IsMounted(ctx, where); err != nil {
		return fmt.Errorf("%w: %v", ErrReadOnlyMount, err)
	} else if mounted {
		return fmt.Errorf("%w: %s is already a mountpoint", ErrReadOnlyMount, where)
	}

	_, runErr := k.Runner.Run(ctx, "mount", "-t", fsType, "-o", opts, device, where)
	if err := k.confirmReadOnly(ctx, fsType, device, uuid, where); err != nil {
		cause := err
		if runErr != nil {
			cause = fmt.Errorf("mount: %w; %v", runErr, err)
		}
		// Whatever is at where is released: it is not shown to be a read-only
		// mount of the filesystem asked for.
		if uerr := k.Unmount(context.WithoutCancel(ctx), where); uerr != nil {
			return errors.Join(fmt.Errorf("%w: %v", ErrReadOnlyMount, cause), uerr)
		}
		return fmt.Errorf("%w: %v", ErrReadOnlyMount, cause)
	}
	return nil
}

// confirmReadOnly reads the mount table: where must be a mountpoint of fsType,
// read-only on the mount and on the superblock, and device, which is what was
// mounted, must hold a filesystem whose UUID is uuid.
func (k KernelReadOnlyMounter) confirmReadOnly(ctx context.Context, fsType, device, uuid, where string) error {
	f, err := os.Open(k.mountInfo())
	if err != nil {
		return fmt.Errorf("reading the mount table: %w", err)
	}
	defer func() { _ = f.Close() }()
	m, found, err := mountEntry(f, where)
	if err != nil {
		return err
	}
	switch {
	case !found:
		return fmt.Errorf("%s is not in the mount table after mounting", where)
	case m.fsType != fsType:
		return fmt.Errorf("%s is mounted as %q, not %q", where, m.fsType, fsType)
	case !hasOption(m.mountOptions, "ro"):
		return fmt.Errorf("%s is mounted without ro (%s)", where, m.mountOptions)
	case !hasOption(m.superOptions, "ro"):
		return fmt.Errorf("%s's superblock is not read-only (%s)", where, m.superOptions)
	}
	got, err := ProbedUUID(ctx, k.Runner, device)
	if err != nil {
		return err
	}
	if !strings.EqualFold(got, uuid) {
		return fmt.Errorf("%s holds filesystem UUID %s, want %s", device, got, uuid)
	}
	return nil
}

// ProbedUUID reads the filesystem UUID on device by a direct probe of the
// device (blkid -p), which needs neither udev's /dev/disk links nor blkid's
// cache. It is read-only, and meant for a device that is already mounted: the
// device is not woken for it.
func ProbedUUID(ctx context.Context, r Runner, device string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	out, err := r.Run(ctx, "blkid", "-p", "-s", "UUID", "-o", "value", device)
	if err != nil {
		return "", fmt.Errorf("disk: probing the filesystem UUID of %s: %w", device, err)
	}
	uuid := strings.TrimSpace(string(out))
	if uuid == "" {
		return "", fmt.Errorf("disk: %s reported no filesystem UUID", device)
	}
	return uuid, nil
}

// Unmount implements ReadOnlyMounter.
func (k KernelReadOnlyMounter) Unmount(ctx context.Context, where string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(where); err == nil {
		where = resolved
	}
	mounted, err := k.IsMounted(ctx, where)
	if err != nil {
		return err
	}
	if !mounted {
		return nil
	}
	umountErr := k.kernel().UnmountOnce(ctx, where)
	mounted, err = k.IsMounted(ctx, where)
	if err != nil {
		return errors.Join(umountErr, err)
	}
	if mounted {
		if umountErr == nil {
			umountErr = errors.New("umount exited 0")
		}
		return fmt.Errorf("disk: %s is still mounted: %w", where, umountErr)
	}
	return nil
}

type mountInfoEntry struct {
	mountOptions string
	fsType       string
	superOptions string
}

// mountEntry returns the mountinfo(5) entry for the mountpoint where; of
// several stacked on it, the last, which is the one a path resolves to.
func mountEntry(r io.Reader, where string) (mountInfoEntry, bool, error) {
	var out mountInfoEntry
	found := false
	want := filepath.Clean(where)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 {
			continue
		}
		sep := -1
		for i := 6; i < len(fields); i++ {
			if fields[i] == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || len(fields) < sep+4 {
			continue
		}
		if filepath.Clean(unescapeMountInfo(fields[4])) != want {
			continue
		}
		out = mountInfoEntry{mountOptions: fields[5], fsType: fields[sep+1], superOptions: fields[sep+3]}
		found = true
	}
	if err := sc.Err(); err != nil {
		return mountInfoEntry{}, false, fmt.Errorf("parsing the mount table: %w", err)
	}
	return out, found, nil
}

func hasOption(options, want string) bool {
	for _, o := range strings.Split(options, ",") {
		if o == want {
			return true
		}
	}
	return false
}

// ReadOnlyMountCall is one MountReadOnly call a FakeReadOnlyMounter saw.
type ReadOnlyMountCall struct {
	FSType, Device, UUID, Where string
}

// FakeReadOnlyMounter is a scriptable ReadOnlyMounter. It mounts nothing: it
// keeps the set of paths it was asked to mount and runs OnMount, which a test
// uses to put the "device's" files in where; Unmount empties where again.
type FakeReadOnlyMounter struct {
	mu       sync.Mutex
	Mounts   []ReadOnlyMountCall
	Unmounts []string
	mounted  map[string]bool
	// MountErr and UnmountErr, when set, are what the call returns. A failed
	// unmount leaves the path mounted.
	MountErr   error
	UnmountErr error
	// OnMount runs after a mount succeeds, with its mountpoint.
	OnMount func(where string) error
}

// NewFakeReadOnlyMounter returns a FakeReadOnlyMounter with nothing mounted.
func NewFakeReadOnlyMounter() *FakeReadOnlyMounter {
	return &FakeReadOnlyMounter{mounted: map[string]bool{}}
}

// SetMounted marks where mounted, as a mount a previous process left behind.
func (f *FakeReadOnlyMounter) SetMounted(where string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mounted[filepath.Clean(where)] = true
}

// MountedPaths returns the paths currently mounted, sorted.
func (f *FakeReadOnlyMounter) MountedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for p, m := range f.mounted {
		if m {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// MountReadOnly implements ReadOnlyMounter.
func (f *FakeReadOnlyMounter) MountReadOnly(ctx context.Context, fsType, device, uuid, where string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	f.Mounts = append(f.Mounts, ReadOnlyMountCall{FSType: fsType, Device: device, UUID: uuid, Where: where})
	if f.MountErr != nil {
		f.mu.Unlock()
		return f.MountErr
	}
	if f.mounted[filepath.Clean(where)] {
		f.mu.Unlock()
		return fmt.Errorf("%w: %s is already a mountpoint", ErrReadOnlyMount, where)
	}
	if err := os.MkdirAll(where, 0o700); err != nil {
		f.mu.Unlock()
		return err
	}
	f.mounted[filepath.Clean(where)] = true
	onMount := f.OnMount
	f.mu.Unlock()
	if onMount != nil {
		return onMount(where)
	}
	return nil
}

// Unmount implements ReadOnlyMounter.
func (f *FakeReadOnlyMounter) Unmount(ctx context.Context, where string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Unmounts = append(f.Unmounts, where)
	if f.UnmountErr != nil {
		return f.UnmountErr
	}
	delete(f.mounted, filepath.Clean(where))
	// What a mount showed is gone with it.
	entries, _ := os.ReadDir(where)
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(where, e.Name()))
	}
	return nil
}

// IsMounted implements ReadOnlyMounter.
func (f *FakeReadOnlyMounter) IsMounted(ctx context.Context, where string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mounted[filepath.Clean(where)], nil
}

var (
	_ ReadOnlyMounter = KernelReadOnlyMounter{}
	_ ReadOnlyMounter = (*FakeReadOnlyMounter)(nil)
)
