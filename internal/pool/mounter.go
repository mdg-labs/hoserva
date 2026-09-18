package pool

import (
	"context"
	"fmt"

	"github.com/mdg-labs/hoserva/internal/disk"
)

// Mounter brings up and tears down a Mount's real mergerfs process
// through disk.Runner — the same argv-only, no-shell execution disk's
// own Provider uses (CLAUDE.md), reused here rather than duplicated.
type Mounter struct {
	Runner disk.Runner
}

// Mount execs mnt's own argv and returns once the mount is live.
// mergerfs daemonizes on its own (Argv passes no -f), matching
// scripts/devenv/create-array.sh's own invocation, so Runner.Run's
// ordinary wait-for-exit is enough — no foreground process or PID
// tracking is needed here.
func (m Mounter) Mount(ctx context.Context, mnt Mount) error {
	argv := mnt.Argv()
	if _, err := m.Runner.Run(ctx, argv[0], argv[1:]...); err != nil {
		return fmt.Errorf("pool: mounting %s: %w", mnt.Where, err)
	}
	return nil
}

// Unmount tears down the mergerfs mount at where with fusermount -u —
// the plain, non-lazy form S6 found sufficient even against a mount
// whose own process had already died (doc 08 §6).
func (m Mounter) Unmount(ctx context.Context, where string) error {
	if _, err := m.Runner.Run(ctx, "fusermount", "-u", where); err != nil {
		return fmt.Errorf("pool: unmounting %s: %w", where, err)
	}
	return nil
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
