package pool

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/mdg-labs/hoserva/internal/disk"
)

// unmountRetryWindow and unmountRetryDelay bound Unmount's busy-retry
// loop: after Samba/NFS stop, their just-released file handles can leave
// fusermount briefly EBUSY (issue #332). The window is long enough for
// that to settle, short enough that a genuinely stuck pool still fails
// `array stop` within a few seconds rather than hanging it.
const (
	unmountRetryWindow = 5 * time.Second
	unmountRetryDelay  = 250 * time.Millisecond
)

// Mounter brings up and tears down a Mount by execing mergerfs directly
// through disk.Runner — the same argv-only, no-shell execution disk's
// own Provider uses (CLAUDE.md). The loop-device lab has no init system
// (doc 06 §3), so every lab test uses this type. Production hoservad
// uses SystemdMounter instead (#335): mergerfs must not live in
// hoserva.service's cgroup.
type Mounter struct {
	Runner disk.Runner

	// Now and Sleep back Unmount's busy-retry loop; both default to the
	// real wall clock (nil means "use time.Now / time.Sleep"). A test
	// overrides them to make the retry window's bound observable
	// without waiting in real time — the same pattern disk.FakeProvider
	// uses for its own scripted delays.
	Now   disk.Clock
	Sleep disk.Sleeper

	// IsMountpoint reports whether a path is currently a mount point.
	// Nil means "use the real stat-based check" (isMountpoint, below) —
	// a test injects a fake to observe Mount's already-mounted and
	// Unmount's already-gone paths without a real mount boundary on
	// disk (internal/disk's own equivalent check is unexported, so this
	// package keeps its own copy rather than reaching into it).
	IsMountpoint func(where string) (bool, error)

	// SetXattr writes one key of a live mount's runtime control file.
	// Nil means syscall.Setxattr; a test injects a recorder.
	SetXattr func(path, attr string, value []byte) error

	// GetXattr reads one key of a live mount's runtime control file. Nil
	// means syscall.Getxattr; a test injects a scripted value.
	GetXattr func(path, attr string) ([]byte, error)

	// MountTarget reports whether a path is a mount point and that mount's
	// options. Nil means disk.ReadMountTarget; a test injects a fake.
	MountTarget func(path string) (opts []string, mounted bool, err error)

	// SameFile reports whether two paths are the same directory (device and
	// inode). Nil means os.Stat on both and os.SameFile; a test injects a
	// fake.
	SameFile func(a, b string) (bool, error)
}

func (m Mounter) mountTarget() func(string) ([]string, bool, error) {
	if m.MountTarget != nil {
		return m.MountTarget
	}
	return disk.ReadMountTarget
}

func (m Mounter) sameFile() func(string, string) (bool, error) {
	if m.SameFile != nil {
		return m.SameFile
	}
	return func(a, b string) (bool, error) {
		ai, err := os.Stat(a)
		if err != nil {
			return false, err
		}
		bi, err := os.Stat(b)
		if err != nil {
			return false, err
		}
		return os.SameFile(ai, bi), nil
	}
}

func (m Mounter) getXattr() func(string, string) ([]byte, error) {
	if m.GetXattr != nil {
		return m.GetXattr
	}
	return func(path, attr string) ([]byte, error) {
		buf := make([]byte, 64<<10)
		n, err := syscall.Getxattr(path, attr, buf)
		if err != nil {
			return nil, err
		}
		return buf[:n], nil
	}
}

func (m Mounter) setXattr() func(string, string, []byte) error {
	if m.SetXattr != nil {
		return m.SetXattr
	}
	return func(path, attr string, value []byte) error {
		return syscall.Setxattr(path, attr, value, 0)
	}
}

func (m Mounter) isMountpoint() func(string) (bool, error) {
	if m.IsMountpoint != nil {
		return m.IsMountpoint
	}
	return isMountpoint
}

// isBusyUnmountError reports whether err is fusermount's EBUSY —
// "Device or resource busy" while a just-stopped service's file handles
// are still being released — and not some other failure (a bad mount
// point, permissions) that retrying cannot fix. It checks only the
// trimmed end of err's message against that exact, C-locale strerror
// text (fusermount3 never calls setlocale, and disk.CommandRunner
// appends the command's trimmed stderr last), never whether "busy"
// appears anywhere in it: err's message also carries the mount path,
// and a share name may contain "busy" (internal/pool/share.go's
// shareNamePattern allows it), so a share like "busybox" must not make
// an unrelated error retry.
func isBusyUnmountError(err error) bool {
	return err != nil && strings.HasSuffix(strings.TrimSpace(err.Error()), "Device or resource busy")
}

// systemdStopShouldRetry reports whether a systemctl stop failure is
// worth retrying. fusermount's own EBUSY text is one case. A busy .mount
// unit is the other: systemctl reports "Job for <unit>.mount failed"
// and does not append umount's strerror, so matching only
// "Device or resource busy" would give up on the first attempt.
// "not loaded", "not found", a masked unit, and an authentication
// failure are permanent and are not retried.
func systemdStopShouldRetry(err error) bool {
	if isBusyUnmountError(err) {
		return true
	}
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "not loaded") || strings.Contains(msg, "not found") ||
		strings.Contains(msg, "masked") || strings.Contains(msg, "Access denied") ||
		strings.Contains(msg, "Interactive authentication") {
		return false
	}
	return strings.Contains(msg, "Job failed") || strings.Contains(msg, ".mount failed")
}

// Mount execs mnt's own argv and returns once the mount is live.
// mergerfs daemonizes on its own (Argv passes no -f), matching
// scripts/devenv/create-array.sh's own invocation, so Runner.Run's
// ordinary wait-for-exit is enough — no foreground process or PID
// tracking is needed here.
//
// mnt.Where already being a mount point never stacks a second mergerfs on
// top of the first: a Start called against an already-mounted catch-all
// or share path (a retried apply, or a Start run without an intervening
// Stop) must not hide the live mount beneath a new one (#268). Instead
// mnt's branches, create policy and minfreespace are applied to the live
// mount through mergerfs's runtime control file (mergerfs(1) "RUNTIME
// CONFIG", 2.40.2) — a share update or an added disk takes effect at
// once, without unmounting a path Samba or NFS may be serving. A value
// already live is not written again, because a read-only mount refuses
// even a no-op setxattr (see applyRuntime, #621). The existing mount is
// only trusted as mnt's own when
// findmnt reports its SOURCE as mnt.FSName — the same fsname doc 02 §1's
// table says is "recognisable in df and mount listings" precisely so it
// can be told apart this way — because a wrong mount masquerading as
// "already there" would be worse than a start that refuses loudly.
//
// mnt's branch binds (Mount.Binds) are brought up first, on both paths, so
// a branch never joins the mount before its bind is there (bindBranch).
func (m Mounter) Mount(ctx context.Context, mnt Mount) error {
	if err := os.MkdirAll(mnt.Where, 0o755); err != nil {
		return fmt.Errorf("pool: creating mount point %s: %w", mnt.Where, err)
	}
	for _, b := range mnt.Binds {
		if err := m.bindBranch(ctx, b); err != nil {
			return err
		}
	}

	mounted, err := m.isMountpoint()(mnt.Where)
	if err != nil {
		return fmt.Errorf("pool: checking whether %s is already mounted: %w", mnt.Where, err)
	}
	if mounted {
		out, err := m.Runner.Run(ctx, "findmnt", "-n", "-o", "SOURCE", mnt.Where)
		if err != nil {
			return fmt.Errorf("pool: %s is already mounted, and its filesystem could not be identified: %w", mnt.Where, err)
		}
		if strings.TrimSpace(string(out)) == mnt.FSName {
			return m.applyRuntime(mnt)
		}
		return fmt.Errorf("pool: %s is already mounted by something other than %s", mnt.Where, mnt.FSName)
	}

	argv := mnt.Argv()
	if _, err := m.Runner.Run(ctx, argv[0], argv[1:]...); err != nil {
		return fmt.Errorf("pool: mounting %s: %w", mnt.Where, err)
	}
	return nil
}

// onSource reports whether the bind at b.Where is b.Source's own directory
// (device and inode): the disk's current filesystem, not one that has since
// left b.Source. Any error reading either reads as not.
func (m Mounter) onSource(b disk.BranchBind) bool {
	same, err := m.sameFile()(b.Source, b.Where)
	return err == nil && same
}

// bindBranch brings b up the way its systemd unit does (disk.BranchBind.
// Render), for the direct mounts the loop-device lab makes: b.Source bound
// at b.Where with nosymfollow. A source that does not exist, or that is not
// a mount point (a data disk that is not mounted, whose bare mountpoint
// directory is on the root filesystem), leaves the bind absent, and with it
// that branch, as the unit's BindsTo= on the disk's mount does.
// A bind already at b.Where is kept only while it is still b.Source's own
// directory with nosymfollow in force; one left over from a disk that was
// since unmounted or remounted is unmounted and bound again, so it never
// keeps serving a filesystem that has left b.Source. A bind that does not
// come up with nosymfollow is unmounted and refused, never used.
func (m Mounter) bindBranch(ctx context.Context, b disk.BranchBind) error {
	target := m.mountTarget()
	opts, mounted, err := target(b.Where)
	if err != nil {
		return fmt.Errorf("pool: checking the mover branch at %s: %w", b.Where, err)
	}
	if _, err := os.Stat(b.Source); errors.Is(err, fs.ErrNotExist) {
		if mounted {
			if _, err := m.Runner.Run(ctx, "umount", b.Where); err != nil {
				return fmt.Errorf("pool: unmounting the mover branch at %s, whose source %s is gone: %w", b.Where, b.Source, err)
			}
		}
		return nil
	} else if err != nil {
		return fmt.Errorf("pool: checking %s for its mover branch: %w", b.Source, err)
	}
	if _, sourceMounted, err := target(b.Source); err != nil {
		return fmt.Errorf("pool: checking whether %s is mounted for its mover branch: %w", b.Source, err)
	} else if !sourceMounted {
		if mounted {
			if _, err := m.Runner.Run(ctx, "umount", b.Where); err != nil {
				return fmt.Errorf("pool: unmounting the mover branch at %s, whose source %s is not mounted: %w", b.Where, b.Source, err)
			}
		}
		return nil
	}
	if mounted {
		if m.onSource(b) && slices.Contains(opts, "nosymfollow") {
			return nil
		}
		if _, err := m.Runner.Run(ctx, "umount", b.Where); err != nil {
			return fmt.Errorf("pool: unmounting the stale mover branch at %s: %w", b.Where, err)
		}
	}
	if err := os.MkdirAll(b.Where, 0o755); err != nil {
		return fmt.Errorf("pool: creating mover branch mount point %s: %w", b.Where, err)
	}
	if _, err := m.Runner.Run(ctx, "mount", "--bind", "-o", "nosymfollow", b.Source, b.Where); err != nil {
		return fmt.Errorf("pool: binding %s at %s: %w", b.Source, b.Where, err)
	}
	opts, mounted, err = target(b.Where)
	if err == nil && mounted && slices.Contains(opts, "nosymfollow") {
		return nil
	}
	_, _ = m.Runner.Run(ctx, "umount", b.Where)
	if err != nil {
		return fmt.Errorf("pool: confirming the mover branch at %s: %w", b.Where, err)
	}
	return fmt.Errorf("pool: the mover branch at %s did not come up with nosymfollow (mounted %t, options %q)", b.Where, mounted, strings.Join(opts, ","))
}

// applyRuntime sets mnt's runtime-configurable options on the live mount
// at mnt.Where. Values use the same syntax as the command line.
//
// A key whose live value already is the wanted one is not written: the kernel
// refuses setxattr on a read-only mount (EROFS) whatever the value, and the
// pool of a pending Unraid migration is read-only and may already be up with
// exactly these options (#621). A value that does differ is written, so a
// read-only mount that would have to change still fails, loudly, and is left
// as it is.
func (m Mounter) applyRuntime(mnt Mount) error {
	ctl := filepath.Join(mnt.Where, ".mergerfs")
	get, set := m.getXattr(), m.setXattr()
	for _, kv := range [][2]string{
		{"user.mergerfs.branches", mnt.What},
		{"user.mergerfs.category.create", string(mnt.CreatePolicy)},
		{"user.mergerfs.minfreespace", mnt.Options.minFreeSpace()},
	} {
		if live, err := get(ctl, kv[0]); err == nil && sameRuntimeValue(kv[0], kv[1], string(live)) {
			continue
		}
		if err := set(ctl, kv[0], []byte(kv[1])); err != nil {
			return fmt.Errorf("pool: updating %s on the live mount at %s: %w", kv[0], mnt.Where, err)
		}
	}
	return nil
}

// sameRuntimeValue reports whether the live value of a runtime key is the one
// wanted. mergerfs reports minfreespace in bytes however it was given ("50M"
// reads back as 52428800), so those are compared as sizes; the rest are
// compared as written. A value that cannot be told apart is never "the same".
func sameRuntimeValue(attr, want, live string) bool {
	live = strings.TrimRight(live, "\x00\n")
	if attr == "user.mergerfs.minfreespace" {
		w, werr := ParseMinFreeSpace(want)
		l, lerr := ParseMinFreeSpace(live)
		return werr == nil && lerr == nil && w == l
	}
	return live == want
}

// Unmount tears down the mergerfs mount at where with fusermount -u —
// the plain, non-lazy form S6 found sufficient even against a mount
// whose own process had already died (doc 08 §6). Never -z (lazy): a
// lazy unmount would hide a live writer instead of failing loudly.
//
// A "device or resource busy" failure is retried at unmountRetryDelay
// intervals until unmountRetryWindow elapses, since after Samba/NFS
// stop the catch-all unmount can briefly race their own file handles
// being released (issue #332). Any other error, or ctx being done,
// returns immediately without retrying. Once retrying stops — the
// window elapsed while still busy, or a non-busy failure — a where that
// turns out to already not be a mount point, or to not exist at all, is
// success, not an error: deleteShare on a share whose per-share mount is
// already gone for any reason (a reboot that never remounted it, or the
// mover's write target directory never existing on a fresh /run, #268)
// must be a clean no-op, the same tolerance disk.DirectMounter.Unmount
// already gives a physical disk, rather than 500 on fusermount's own
// "No such file or directory" / "Invalid argument" for a mount that was
// never there. A missing path cannot itself be a live mount point, so
// isMountpoint's own os.Stat failing with fs.ErrNotExist is treated the
// same as a stat that succeeds and reports "not mounted" — but any other
// stat failure (in particular ENOTCONN, what a dead FUSE mount's own
// endpoint reports, not ENOENT) still falls through to the fusermount
// error below, since that is a live-but-broken mount, not an absent one.
func (m Mounter) Unmount(ctx context.Context, where string) error {
	now := m.Now
	if now == nil {
		now = time.Now
	}
	sleep := m.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	isMountpoint := m.isMountpoint()

	deadline := now().Add(unmountRetryWindow)
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("pool: unmounting %s: %w", where, err)
		}
		_, err := m.Runner.Run(ctx, "fusermount", "-u", where)
		if err == nil {
			return nil
		}
		if isBusyUnmountError(err) && now().Before(deadline) {
			sleep(unmountRetryDelay)
			continue
		}
		mounted, checkErr := isMountpoint(where)
		if checkErr == nil && !mounted {
			return nil
		}
		if checkErr != nil && errors.Is(checkErr, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("pool: unmounting %s: %w", where, err)
	}
}

// Remount brings mnt.Where back up with a changed branch list (doc 02 §4
// "Adding a disk" step 6, "Remount, regenerate configs"): unmount
// whatever currently serves mnt.Where, then mount mnt itself. mergerfs
// itself does no parity or placement computation on this — "no rebuild",
// the property doc 02 §4 says the UI must state explicitly — so the
// pool's added capacity is available the moment mnt.Where is back up.
//
// If mounting mnt fails, Remount tries to bring previous back up rather
// than leave mnt.Where unmounted: a malformed branch list or a transient
// mergerfs failure must not turn a capacity-expansion attempt into a
// storage outage. previous is expected to be the Mount that was actually
// serving mnt.Where before this call (typically mnt with the old, not the
// grown, branch list) — passing anything else re-mounts whatever previous
// itself describes, not what was really there. A rollback failure is
// returned alongside the original mount error, never in its place, since
// losing the original failure would hide why Remount stopped short.
func (m Mounter) Remount(ctx context.Context, previous, mnt Mount) error {
	if err := m.Unmount(ctx, mnt.Where); err != nil {
		return err
	}
	if err := m.Mount(ctx, mnt); err != nil {
		if rbErr := m.Mount(ctx, previous); rbErr != nil {
			return fmt.Errorf("%w (rollback to the previous mount also failed: %v)", err, rbErr)
		}
		return err
	}
	return nil
}
