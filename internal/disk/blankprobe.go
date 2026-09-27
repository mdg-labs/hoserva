package disk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// BlankProber is the one-off, bounded probe of a single device the
// replace path needs (#398, following #388): unlike Provider.List, which
// reads only the udev database and must never open a device (Q13, since
// it serves every GET /pool poll, every SIGHUP and every topology
// rebuild), ProbeBlank runs a real, low-level filesystem/partition-table
// probe of dev, and then a read-only readback of dev itself — the one
// device a user has just named as a replace target, never a device this
// build merely enumerated. It positively distinguishes "no signature and
// genuinely readable" from every other outcome — finding a filesystem,
// finding a partition table, the probe itself failing or being
// ambiguous, or a readback that cannot open or fully read the device —
// which its only caller (job.ConfirmReplacementTargetAbsent) treats
// identically: refuse.
type BlankProber interface {
	// ProbeBlank reports true only when dev positively carries no
	// filesystem or partition-table signature at all, and a read-only
	// readback of its first and last MiB succeeds without error. Any
	// other outcome — a signature found, or an error running the probe
	// or the readback — returns false; err is non-nil only when the
	// probe itself could not reach a positive answer (a tool error, an
	// ambiguous low-level result, or a readback failure), never merely
	// because a signature was found.
	ProbeBlank(ctx context.Context, dev string) (bool, error)
}

// exitCoder is the method os/exec's own *exec.ExitError provides
// (ExitCode() int), matched by interface here rather than against the
// concrete type, so a test can script the exit code ProbeBlank
// classifies without spawning a real process just to obtain one — the
// same pattern internal/parity's SnapraidEngine already uses for
// snapraid's own exit codes.
type exitCoder interface{ ExitCode() int }

func exitCode(err error) (code int, ok bool) {
	var ec exitCoder
	if errors.As(err, &ec) {
		return ec.ExitCode(), true
	}
	return 0, false
}

// BlankReadback is the read-only confirmation ProbeBlank runs against
// dev after blkid -p reports no signature (#398 finding 1): blkid(8)
// documents exit 2 as covering both "no signature found" and "impossible
// to gather any information about the device" — a device with read
// errors in its first sectors also exits 2, and a build that trusted
// that exit code alone would let a disk that still holds data through as
// "blank". Readback opens dev itself, so it sits behind its own
// interface with a scriptable fake (CLAUDE.md), the same as every other
// system-touching call in this package.
type BlankReadback interface {
	// Readback opens dev read-only and reads its first and last MiB (or
	// the whole device, if smaller). Any open failure, read error or
	// short read returns a non-nil error — the readback's own positive
	// confirmation that dev is reachable and yields real data, not
	// merely that blkid's own probe found no signature to report.
	Readback(ctx context.Context, dev string) error
}

// blankReadbackChunk is how much of the start and end of dev osBlank-
// Readback actually reads — enough to cross every filesystem
// superblock's and partition table's own on-disk location blkid itself
// already checked, so this exists only to positively confirm the device
// is readable, not to duplicate blkid's own signature search.
const blankReadbackChunk = 1 << 20 // 1 MiB

// osBlankReadback is the real BlankReadback: it opens dev itself,
// O_RDONLY, never O_RDWR or O_EXCL — this never writes to a device, and
// never takes an exclusive lock a genuine in-use disk would refuse.
type osBlankReadback struct{}

func (osBlankReadback) Readback(ctx context.Context, dev string) error {
	errCh := make(chan error, 1)
	go func() { errCh <- readbackDevice(dev) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

func readbackDevice(dev string) error {
	f, err := os.OpenFile(dev, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("opening %s read-only: %w", dev, err)
	}
	defer func() { _ = f.Close() }()

	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("seeking to the end of %s: %w", dev, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seeking to the start of %s: %w", dev, err)
	}
	firstChunk := size
	if firstChunk > blankReadbackChunk {
		firstChunk = blankReadbackChunk
	}
	if err := readExactly(f, firstChunk); err != nil {
		return fmt.Errorf("reading the first %d bytes of %s: %w", firstChunk, dev, err)
	}
	if size > blankReadbackChunk {
		if _, err := f.Seek(-blankReadbackChunk, io.SeekEnd); err != nil {
			return fmt.Errorf("seeking to the last %d bytes of %s: %w", blankReadbackChunk, dev, err)
		}
		if err := readExactly(f, blankReadbackChunk); err != nil {
			return fmt.Errorf("reading the last %d bytes of %s: %w", blankReadbackChunk, dev, err)
		}
	}
	return nil
}

// readExactly reads exactly n bytes from r, refusing a short read the
// same as any other read error — a device that stops returning data
// partway through must never be read as "confirmed readable".
func readExactly(r io.Reader, n int64) error {
	if n <= 0 {
		return nil
	}
	_, err := io.ReadFull(r, make([]byte, n))
	return err
}

// LinuxBlankProber is the real BlankProber: it execs blkid's own
// low-level probe mode against dev — an argv, never a shell (CLAUDE.md)
// — bypassing blkid's cache (-p) so a stale or never-populated cache
// entry cannot stand in for what is actually on the device right now —
// and, only once that reports no signature, runs a read-only readback of
// dev itself (Readback) before ever calling it blank.
type LinuxBlankProber struct {
	Exec Runner
	// Readback overrides the real device readback — set only by this
	// package's own tests, never in production; nil uses osBlankReadback.
	Readback BlankReadback
	// Timeout overrides blankProbeTimeout — set only by this package's
	// own tests, never in production.
	Timeout time.Duration
}

// blkidNoSignature and blkidAmbiguous are blkid(8)'s own documented exit
// statuses: 2 means "the specified token was not found, or no (specified)
// devices could be identified" — the low-level probe's own "found
// nothing at all", but also, by blkid(8)'s own wording, "impossible to
// gather any information about the device" — a read error against the
// device's own first sectors exits 2 as well, indistinguishable from a
// genuinely blank disk by the exit code alone (confirmed in the lab
// against util-linux 2.41.5: a nonexistent path, a permission-denied
// path and a directory all exit 2). That is exactly what Readback below
// exists to tell apart — and 8 means "an ambiguous low-level probing
// result was detected", which -p enables reporting distinctly from a
// plain "not found". Every other non-zero exit (1 usage error, 4 other
// error) is classified as an error the same way.
const (
	blkidNoSignature = 2
	blkidAmbiguous   = 8
)

// blankProbeTimeout bounds both the blkid exec and the read-only
// readback ProbeBlank runs (#398): a `mount_failed` disk with a
// genuinely stuck device — a controller wedge, not simply "no
// filesystem" — must refuse within a bound instead of leaving
// planDiskReplace/replaceDisk, and the request that reaches them, hung
// indefinitely. A timeout here is indistinguishable from any other probe
// failure and refuses exactly the same way (fail-open on the blank-probe
// path means "refuse").
const blankProbeTimeout = 20 * time.Second

func (p LinuxBlankProber) timeout() time.Duration {
	if p.Timeout > 0 {
		return p.Timeout
	}
	return blankProbeTimeout
}

func (p LinuxBlankProber) readback() BlankReadback {
	if p.Readback != nil {
		return p.Readback
	}
	return osBlankReadback{}
}

// ProbeBlank runs `blkid -p -o export dev`, which probes dev directly —
// never blkid's own cache — and reports both a filesystem signature and
// a partition table (PTTYPE), so a disk carrying a partition table but
// no filesystem is never read as blank (doc 02: array disks are always
// whole-disk, never partitioned, but a stray partition table left behind
// by some other use of the disk must still refuse). A found signature
// (exit 0) reports false with no error — this is a definite, not
// inconclusive, answer. Exit 2 ("no signature found" — but also,
// undistinguishably by exit code alone, "impossible to gather any
// information about the device", blkid(8)) is never trusted on its own:
// only once a read-only readback of dev itself (Readback) also succeeds
// does this report blank — a readback failure refuses with an error,
// exactly like any other probe failure, so a disk with unreadable
// sectors is never reported blank just because blkid could not read them
// either. An ambiguous result (8) or any other non-zero exit reports
// false with an error, since neither is a positive "no signature at
// all". Both the exec and the readback run within a single bounded
// deadline (blankProbeTimeout); any timeout, error, short read or
// ambiguous result anywhere on this path refuses.
func (p LinuxBlankProber) ProbeBlank(ctx context.Context, dev string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout())
	defer cancel()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	_, err := p.Exec.Run(ctx, "blkid", "-p", "-o", "export", dev)
	if err == nil {
		return false, nil
	}
	code, ok := exitCode(err)
	if !ok {
		return false, fmt.Errorf("disk: probing %s for a blank disk: %w", dev, err)
	}
	switch code {
	case blkidNoSignature:
		if err := p.readback().Readback(ctx, dev); err != nil {
			return false, fmt.Errorf("disk: confirming %s reads cleanly before reporting it blank: %w", dev, err)
		}
		return true, nil
	case blkidAmbiguous:
		return false, fmt.Errorf("disk: probing %s for a blank disk: blkid reported an ambiguous result: %w", dev, err)
	default:
		return false, fmt.Errorf("disk: probing %s for a blank disk: %w", dev, err)
	}
}

var _ BlankProber = LinuxBlankProber{}
