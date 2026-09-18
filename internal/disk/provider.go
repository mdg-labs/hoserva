// Package disk defines the subsystem abstraction over block devices, their
// identity, SMART health and spin state (doc 01 §4). Nothing outside this
// package talks to smartctl, hdparm or a partitioning tool directly; every
// caller — the API, jobs, the CLI's dev mode — goes through Provider, so
// storage logic is testable without a NAS (doc 06 §2).
package disk

import (
	"context"
	"errors"
	"time"
)

// Byte-size constants, for expressing disk and threshold sizes without a
// magic number at every call site.
const (
	KB int64 = 1 << (10 * (iota + 1))
	MB
	GB
	TB
)

// ErrDiskNotFound is returned when an operation names a device the provider
// has never seen.
var ErrDiskNotFound = errors.New("disk: not found")

// ErrDiskFailed is returned by any operation against a device the provider
// has scripted or detected as failed (doc 02 §6: "one data disk fails").
var ErrDiskFailed = errors.New("disk: failed")

// FilesystemType is a filesystem Hoserva can format or adopt (doc 02 §5).
type FilesystemType string

const (
	XFS   FilesystemType = "xfs"
	EXT4  FilesystemType = "ext4"
	BTRFS FilesystemType = "btrfs"
)

// SpinState is a disk's current power state, as hdparm/smartctl report it.
type SpinState int

const (
	Active SpinState = iota
	Standby
)

// String renders state as store's spin_events table expects it (#110):
// its from_state/to_state columns are CHECK-constrained to exactly these
// two spellings.
func (s SpinState) String() string {
	if s == Standby {
		return "standby"
	}
	return "active"
}

// Trend describes a SMART attribute's direction over time. A single SMART
// snapshot can't say whether a value is a long-standing baseline or a fresh
// regression; doc 02 §4 is explicit that the trend, not the absolute value,
// is the signal worth alerting on (reallocated sectors going from 0 to 4).
type Trend int

const (
	Stable Trend = iota
	Rising
	Falling
)

// SMARTPollMode selects how SMART is queried. RespectStandby mirrors
// `smartctl -n standby`: a disk already asleep is left alone and the report
// comes back marked Skipped rather than spinning the disk up to answer the
// poll (doc 02 §1, §4). Force always queries, waking the disk if needed —
// for a user-initiated health check, never for the periodic poll.
type SMARTPollMode int

const (
	SMARTPollRespectStandby SMARTPollMode = iota
	SMARTPollForce
)

// Disk is a block device Hoserva knows about, by stable identity rather than
// its transient /dev/sdX name (Q21). WWN is preferred; Serial is the
// fallback used when no wwn-* /dev/disk/by-id link exists. WeakIdentity is
// true for a USB-enclosure disk, whose bridge chipset can hide the real
// disk's WWN and serial — allowed as a data disk, refused as parity (Q21).
// Boot marks the disk currently backing the root filesystem: it is always
// identified and always excluded from anything destructive (doc 02 §4).
// ByIDName is the by-id link basename WWN/Serial were resolved from
// (Identity.ByIDName) — empty exactly when neither is, meaning this disk
// has no by-id link at all to bind a format or adopt-check call to.
type Disk struct {
	Device       string
	Size         int64
	Model        string
	Serial       string
	WWN          string
	WeakIdentity bool
	ByIDName     string
	Boot         bool
	Failed       bool
}

// SMARTReport is one SMART poll's result. Skipped is true when a standby
// disk was left asleep rather than queried; the trend and attribute fields
// are then whatever was last known, not a fresh read.
type SMARTReport struct {
	Skipped              bool
	SpinState            SpinState
	ReallocatedSectors   int
	PendingSectors       int
	OfflineUncorrectable int
	CRCErrors            int
	TemperatureCelsius   int
	PowerOnHours         int
	Trend                Trend
	SelfTestFailed       bool
}

// Provider is the interface every subsystem touching real disks sits behind
// (doc 01 §4). context.Context is first on every method because every one
// of them does IO — enumerating devices, running smartctl, spinning a disk
// down, or formatting it.
type Provider interface {
	List(ctx context.Context) ([]Disk, error)
	SMART(ctx context.Context, dev string, mode SMARTPollMode) (SMARTReport, error)
	Spindown(ctx context.Context, dev string) error
	Format(ctx context.Context, dev string, fs FilesystemType) error
}

// Clock and Sleeper let a fake provider's time-based scripting (FailAfter,
// SlowDown) run under a test's control instead of the wall clock.
type Clock func() time.Time

type Sleeper func(time.Duration)
