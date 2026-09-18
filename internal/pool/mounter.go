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
