package pool

import (
	"context"
	"fmt"
	"os"
	"strings"
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

// Mounter brings up and tears down a Mount's real mergerfs process
// through disk.Runner — the same argv-only, no-shell execution disk's
// own Provider uses (CLAUDE.md), reused here rather than duplicated.
type Mounter struct {
	Runner disk.Runner

	// Now and Sleep back Unmount's busy-retry loop; both default to the
	// real wall clock (nil means "use time.Now / time.Sleep"). A test
	// overrides them to make the retry window's bound observable
	// without waiting in real time — the same pattern disk.FakeProvider
	// uses for its own scripted delays.
	Now   disk.Clock
	Sleep disk.Sleeper
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

// Mount execs mnt's own argv and returns once the mount is live.
// mergerfs daemonizes on its own (Argv passes no -f), matching
// scripts/devenv/create-array.sh's own invocation, so Runner.Run's
// ordinary wait-for-exit is enough — no foreground process or PID
// tracking is needed here.
func (m Mounter) Mount(ctx context.Context, mnt Mount) error {
	if err := os.MkdirAll(mnt.Where, 0o755); err != nil {
		return fmt.Errorf("pool: creating mount point %s: %w", mnt.Where, err)
	}
	argv := mnt.Argv()
	if _, err := m.Runner.Run(ctx, argv[0], argv[1:]...); err != nil {
		return fmt.Errorf("pool: mounting %s: %w", mnt.Where, err)
	}
	return nil
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
// returns immediately without retrying. When the window ends with the
// mount still busy, Unmount returns that last error, so a genuinely
// busy pool still fails array stop rather than being silently retried
// forever.
func (m Mounter) Unmount(ctx context.Context, where string) error {
	now := m.Now
	if now == nil {
		now = time.Now
	}
	sleep := m.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}

	deadline := now().Add(unmountRetryWindow)
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("pool: unmounting %s: %w", where, err)
		}
		_, err := m.Runner.Run(ctx, "fusermount", "-u", where)
		if err == nil {
			return nil
		}
		if !isBusyUnmountError(err) {
			return fmt.Errorf("pool: unmounting %s: %w", where, err)
		}
		if !now().Before(deadline) {
			return fmt.Errorf("pool: unmounting %s: %w", where, err)
		}
		sleep(unmountRetryDelay)
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
