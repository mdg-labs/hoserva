package backup

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrExternalWriteInFlight is Eject's refusal when a backup write to the disk
// did not finish within the wait: the disk is left mounted and open to
// backups.
var ErrExternalWriteInFlight = errors.New("backup: a backup write to the disk is still running")

// ExternalWriteGates gives each external disk its own PoolWriteGate, keyed by
// label, so an eject of /mnt/disks/<label> and a backup write to it exclude
// each other (#454). A write takes a slot for the disk before it confirms the
// disk is mounted and holds it until the write and prune are done; Eject
// closes the disk's gate, which refuses new writes and waits for those in
// flight, and only then unmounts. The mount check alone cannot close that
// race: mount confirmed, disk ejected, then the write creating its directory
// on the boot device, hidden once the disk mounts again.
//
// A gate closed by a completed Eject stays closed until Mount succeeds for
// the label, so a write attempted in between is refused by the gate itself
// and not only by the mount table. The zero value is ready to use, and a nil
// *ExternalWriteGates never refuses or waits — a Service or Handler built
// without one behaves as it did before this gate, guarded by the mount check.
//
// Lock order: a disk's lifecycle, then its gate; a write holds a gate slot
// and never takes a lifecycle, so the two cannot wait on each other.
type ExternalWriteGates struct {
	mu    sync.Mutex
	disks map[string]*externalDiskGate
}

type externalDiskGate struct {
	writes PoolWriteGate
	// lifecycle serializes Eject and Mount of one disk, so a Mount cannot
	// reopen the gate under an Eject still waiting on it. A channel rather
	// than a mutex so a waiter honors its context.
	lifecycle chan struct{}
}

func (g *ExternalWriteGates) disk(label string) *externalDiskGate {
	g.mu.Lock()
	defer g.mu.Unlock()
	d, ok := g.disks[label]
	if !ok {
		if g.disks == nil {
			g.disks = map[string]*externalDiskGate{}
		}
		d = &externalDiskGate{lifecycle: make(chan struct{}, 1)}
		g.disks[label] = d
	}
	return d
}

func (d *externalDiskGate) acquire(ctx context.Context) error {
	select {
	case d.lifecycle <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *externalDiskGate) unlock() { <-d.lifecycle }

// begin admits one write to the disk labelled label, refusing while its gate
// is closed. A true result must be matched by exactly one call to release.
func (g *ExternalWriteGates) begin(label string) (release func(), ok bool) {
	if g == nil {
		return func() {}, true
	}
	d := g.disk(label)
	if !d.writes.begin() {
		return nil, false
	}
	return d.writes.end, true
}

// Eject runs unmount with the disk's gate closed: writes admitted before it
// finish first, and none is admitted after. The wait for them, and for a
// Mount or another Eject of the same disk, is bounded by wait and ctx;
// unmount itself gets ctx alone. When the wait expires, or unmount fails, the
// gate reopens and the disk stays as it was — an unmount that failed only
// after unmounting (a spindown error) is caught by the mount check, which
// reads the mount table. After a successful unmount the gate stays closed
// until Mount.
func (g *ExternalWriteGates) Eject(ctx context.Context, label string, wait time.Duration, unmount func(context.Context) error) error {
	if g == nil {
		return unmount(ctx)
	}
	d := g.disk(label)
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	if err := d.acquire(waitCtx); err != nil {
		return fmt.Errorf("%w: waiting for another eject or mount of %q: %v", ErrExternalWriteInFlight, label, err)
	}
	defer d.unlock()

	if err := d.writes.Close(waitCtx); err != nil {
		d.writes.Open()
		return fmt.Errorf("%w: %v", ErrExternalWriteInFlight, err)
	}
	if err := unmount(ctx); err != nil {
		d.writes.Open()
		return err
	}
	return nil
}

// Mount runs mount and, once it succeeds, admits writes to the disk again.
func (g *ExternalWriteGates) Mount(ctx context.Context, label string, mount func(context.Context) error) error {
	if g == nil {
		return mount(ctx)
	}
	d := g.disk(label)
	if err := d.acquire(ctx); err != nil {
		return fmt.Errorf("waiting for an eject of %q to finish: %w", label, err)
	}
	defer d.unlock()

	if err := mount(ctx); err != nil {
		return err
	}
	d.writes.Open()
	return nil
}
