package pool

import "context"

// MountController adapts a Mount plus the Mounter that brings it up or
// down to the doc 02 §4 stop/start sequence's generic Where/Mount/Unmount
// shape (internal/job.ArrayMount) — this package imports nothing from
// internal/job (structural interface satisfaction needs no import), so
// job.ArraySequence can be built from a slice of these directly.
type MountController struct {
	Mnt     Mount
	Mounter Mounter
}

// Where is m's own mount point, for logging and error messages.
func (c MountController) Where() string { return c.Mnt.Where }

// Mount brings c's own Mount up through its Mounter.
func (c MountController) Mount(ctx context.Context) error {
	return c.Mounter.Mount(ctx, c.Mnt)
}

// Unmount tears c's own Mount down through its Mounter.
func (c MountController) Unmount(ctx context.Context) error {
	return c.Mounter.Unmount(ctx, c.Mnt.Where)
}
