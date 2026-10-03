package disk

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// SMARTCall records one call to FakeProvider.SMART, so a test can assert on
// how polling actually behaved rather than only on its return value — the
// property doc 06 §2 asks for is "the fake was never queried in a waking
// mode", which is a property of the call, not of the report it returned.
type SMARTCall struct {
	Device string
	Mode   SMARTPollMode
	Woke   bool
	At     time.Time
}

type fakeDisk struct {
	disk        Disk
	smart       SMARTReport
	spinState   SpinState
	failAt      *time.Time
	slowdown    time.Duration
	formattedAs FilesystemType
}

// FakeProvider is a scriptable simulator of Provider, not a stub (doc 06
// §2): it can be told a disk's SMART trend, its current spin state, that it
// fails after a delay, or that it has gone slow, and it holds all of that
// state so a test can assert on it afterwards.
type FakeProvider struct {
	mu    sync.Mutex
	disks map[string]*fakeDisk
	calls []SMARTCall

	// formatCalls records the device of every Format call that was
	// accepted, in order; partFormatted holds the filesystem a boot-disk
	// cache partition was formatted with, keyed by its kernel device.
	formatCalls   []string
	partFormatted map[string]FilesystemType

	// Now and Sleep default to the wall clock; tests override them to
	// script FailAfter/SlowDown without actually waiting.
	Now   Clock
	Sleep Sleeper

	// AfterList, when set, runs once every List call returns, after its
	// own lock is released. It exists so a test can inject a mutation
	// timed exactly between an identity check (which calls List) and the
	// destructive call that follows it — the window this issue's own
	// race lives in — and assert that formatting still lands on the
	// right physical disk regardless of what happens in it.
	AfterList func()
}

// NewFakeProvider returns a FakeProvider with no disks and the real clock.
func NewFakeProvider() *FakeProvider {
	return &FakeProvider{
		disks:         make(map[string]*fakeDisk),
		partFormatted: make(map[string]FilesystemType),
		Now:           time.Now,
		Sleep:         time.Sleep,
	}
}

// AddDisk registers a disk, healthy and spun up, ready for a test or the
// frontend dev server to see through List. A disk with a filesystem and no
// FSDevice holds it on the whole disk.
func (f *FakeProvider) AddDisk(dev string, d Disk) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d.Device = dev
	if d.FSDevice == "" && (d.Filesystem != "" || d.Label != "" || d.FSUUID != "") {
		d.FSDevice = dev
	}
	f.disks[dev] = &fakeDisk{disk: d, spinState: Active}
}

// SetSMART scripts the report a disk's next SMART poll returns.
func (f *FakeProvider) SetSMART(dev string, r SMARTReport) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fd, ok := f.disks[dev]; ok {
		fd.smart = r
	}
}

// SetSpinState scripts a disk's current power state, as if hdparm had just
// reported it — for example, a disk already in standby before a poll runs.
func (f *FakeProvider) SetSpinState(dev string, s SpinState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fd, ok := f.disks[dev]; ok {
		fd.spinState = s
	}
}

// Reassign moves the disk currently at oldDev to newDev, as if udev had
// renumbered it — its identity and every other scripted property move
// with it. It is a no-op if oldDev names no disk this FakeProvider knows
// about. A test combines it with a further AddDisk(oldDev, ...) to put a
// different (or new) disk at the vacated path, modelling this issue's own
// race: the /dev/sdX path a caller confirmed an identity against no
// longer names the same physical disk by the time a later call runs.
func (f *FakeProvider) Reassign(oldDev, newDev string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fd, ok := f.disks[oldDev]
	if !ok {
		return
	}
	delete(f.disks, oldDev)
	fd.disk.Device = newDev
	if fd.disk.FSDevice == oldDev {
		fd.disk.FSDevice = newDev
	}
	f.disks[newDev] = fd
}

// FailAfter scripts a disk dying mid-operation: every call naming dev made
// at or after d (measured from FailAfter's own call, via f.Now) returns
// ErrDiskFailed, and the disk is reported Failed by List.
func (f *FakeProvider) FailAfter(dev string, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fd, ok := f.disks[dev]
	if !ok {
		return
	}
	failAt := f.Now().Add(d)
	fd.failAt = &failAt
}

// SlowDown scripts a dying-slow disk: every subsequent call naming dev
// sleeps for d (via f.Sleep) before doing anything else.
func (f *FakeProvider) SlowDown(dev string, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fd, ok := f.disks[dev]; ok {
		fd.slowdown = d
	}
}

// SMARTCalls returns every call made to SMART so far, in call order.
func (f *FakeProvider) SMARTCalls() []SMARTCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]SMARTCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// SpinState reports a disk's current scripted or simulated power state.
func (f *FakeProvider) SpinState(dev string) (SpinState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fd, ok := f.disks[dev]
	if !ok {
		return Active, fmt.Errorf("disk %s: %w", dev, ErrDiskNotFound)
	}
	return fd.spinState, nil
}

// FormattedAs reports the filesystem the fake last formatted a disk with.
func (f *FakeProvider) FormattedAs(dev string) (FilesystemType, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fs, ok := f.partFormatted[dev]; ok {
		return fs, true
	}
	fd, ok := f.disks[dev]
	if !ok || fd.formattedAs == "" {
		return "", false
	}
	return fd.formattedAs, true
}

// FormatCalls returns the device of every accepted Format call, in call
// order, so a test can assert that nothing outside the assigned targets
// was ever formatted.
func (f *FakeProvider) FormatCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.formatCalls...)
}

// cachePartitionLocked returns the boot-disk cache candidate dev names,
// by kernel device or by its by-id path. Callers must hold f.mu.
func (f *FakeProvider) cachePartitionLocked(dev string) (CachePartition, bool) {
	for _, fd := range f.disks {
		for _, c := range fd.disk.CachePartitions {
			if dev == c.Device || dev == (Identity{ByIDName: c.ByIDName}).IdentityPath() {
				return c, true
			}
		}
	}
	return CachePartition{}, false
}

func (f *FakeProvider) checkFailed(fd *fakeDisk, dev string) error {
	if fd.failAt != nil && !f.Now().Before(*fd.failAt) {
		fd.disk.Failed = true
		return fmt.Errorf("disk %s: %w", dev, ErrDiskFailed)
	}
	return nil
}

func (f *FakeProvider) List(ctx context.Context) ([]Disk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()

	devs := make([]string, 0, len(f.disks))
	for dev := range f.disks {
		devs = append(devs, dev)
	}
	sort.Strings(devs)

	out := make([]Disk, 0, len(devs))
	for _, dev := range devs {
		fd := f.disks[dev]
		if fd.failAt != nil && !f.Now().Before(*fd.failAt) {
			fd.disk.Failed = true
		}
		out = append(out, fd.disk)
	}
	hook := f.AfterList
	f.mu.Unlock()

	if hook != nil {
		hook()
	}
	return out, nil
}

// resolveLocked returns the fakeDisk dev currently names: directly, when
// dev is a plain device path this FakeProvider knows about, or by
// identity, when dev is an identity-bound /dev/disk/by-id path
// (Identity.IdentityPath) — the disk whose own WWN/Serial/ByIDName
// reconstructs exactly that path, wherever it currently lives. This is
// what models the kernel's own by-id symlink resolution: it always runs
// fresh, at the instant of the call, never trusting an earlier lookup's
// result. Callers must hold f.mu.
func (f *FakeProvider) resolveLocked(dev string) (string, *fakeDisk, bool) {
	if fd, ok := f.disks[dev]; ok {
		return dev, fd, true
	}
	for path, fd := range f.disks {
		id := Identity{WWN: fd.disk.WWN, Serial: fd.disk.Serial, ByIDName: fd.disk.ByIDName}
		if p := id.IdentityPath(); p != "" && p == dev {
			return path, fd, true
		}
	}
	return "", nil, false
}

func (f *FakeProvider) SMART(ctx context.Context, dev string, mode SMARTPollMode) (SMARTReport, error) {
	if err := ctx.Err(); err != nil {
		return SMARTReport{}, err
	}
	f.mu.Lock()
	fd, ok := f.disks[dev]
	if !ok {
		f.mu.Unlock()
		return SMARTReport{}, fmt.Errorf("disk %s: %w", dev, ErrDiskNotFound)
	}
	if err := f.checkFailed(fd, dev); err != nil {
		f.mu.Unlock()
		return SMARTReport{}, err
	}
	slowdown := fd.slowdown
	f.mu.Unlock()

	if slowdown > 0 {
		f.Sleep(slowdown)
	}
	if err := ctx.Err(); err != nil {
		return SMARTReport{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	woke := false
	if fd.spinState == Standby {
		if mode == SMARTPollRespectStandby {
			f.calls = append(f.calls, SMARTCall{Device: dev, Mode: mode, Woke: false, At: f.Now()})
			return SMARTReport{Skipped: true, SpinState: Standby}, nil
		}
		// SMARTPollForce: querying a standby disk wakes it.
		fd.spinState = Active
		woke = true
	}

	f.calls = append(f.calls, SMARTCall{Device: dev, Mode: mode, Woke: woke, At: f.Now()})
	report := fd.smart
	report.SpinState = fd.spinState
	return report, nil
}

func (f *FakeProvider) Spindown(ctx context.Context, dev string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	fd, ok := f.disks[dev]
	if !ok {
		f.mu.Unlock()
		return fmt.Errorf("disk %s: %w", dev, ErrDiskNotFound)
	}
	if err := f.checkFailed(fd, dev); err != nil {
		f.mu.Unlock()
		return err
	}
	slowdown := fd.slowdown
	f.mu.Unlock()

	if slowdown > 0 {
		f.Sleep(slowdown)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	fd.spinState = Standby
	return nil
}

func (f *FakeProvider) Format(ctx context.Context, dev string, fs FilesystemType) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	if part, isPart := f.cachePartitionLocked(dev); isPart {
		defer f.mu.Unlock()
		if !bootCacheTargetAllowed(ctx, dev) {
			return fmt.Errorf("%s: %w", dev, ErrBootDevice)
		}
		f.partFormatted[part.Device] = fs
		f.formatCalls = append(f.formatCalls, dev)
		return nil
	}
	actual, fd, ok := f.resolveLocked(dev)
	if !ok {
		f.mu.Unlock()
		return fmt.Errorf("disk %s: %w", dev, ErrDiskNotFound)
	}
	if fd.disk.Boot {
		f.mu.Unlock()
		return fmt.Errorf("%s: %w", dev, ErrBootDevice)
	}
	if err := f.checkFailed(fd, actual); err != nil {
		f.mu.Unlock()
		return err
	}
	slowdown := fd.slowdown
	f.mu.Unlock()

	if slowdown > 0 {
		f.Sleep(slowdown)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.formatCalls = append(f.formatCalls, dev)
	fd.formattedAs = fs
	fd.spinState = Active
	return nil
}

var _ Provider = (*FakeProvider)(nil)

// RunCall records one call made through a FakeRunner.
type RunCall struct {
	Name string
	Args []string
}

// FakeRunner is a scriptable Runner (CLAUDE.md): a test scripts exactly
// what stdout and error a given argv returns, and can assert on every
// call actually made — the property LinuxProvider's own tests need is
// "smartctl was invoked with -n standby, as an argv, never a shell".
type FakeRunner struct {
	mu      sync.Mutex
	outputs map[string][]byte
	errs    map[string]error
	calls   []RunCall
}

// NewFakeRunner returns a FakeRunner with nothing scripted.
func NewFakeRunner() *FakeRunner {
	return &FakeRunner{outputs: make(map[string][]byte), errs: make(map[string]error)}
}

func runnerKey(name string, args []string) string {
	return strings.Join(append([]string{name}, args...), " ")
}

// Script sets the output and error a future call with this exact argv
// returns.
func (f *FakeRunner) Script(name string, args []string, output []byte, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := runnerKey(name, args)
	f.outputs[key] = output
	f.errs[key] = err
}

// Run implements Runner by returning whatever was scripted for this
// exact argv, recording the call regardless.
func (f *FakeRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, RunCall{Name: name, Args: append([]string(nil), args...)})
	key := runnerKey(name, args)
	return f.outputs[key], f.errs[key]
}

// Calls returns every call made so far, in call order.
func (f *FakeRunner) Calls() []RunCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]RunCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// FakeBlankProber is a scriptable BlankProber (#398, CLAUDE.md): a test
// scripts exactly what ProbeBlank(dev) returns and can assert on every
// device it was actually called against — the property this issue's own
// tests need is "the probe was never called for a device the replace
// path had no reason to open", never only its return value.
type FakeBlankProber struct {
	mu     sync.Mutex
	blank  map[string]bool
	errs   map[string]error
	probed []string
}

// NewFakeBlankProber returns a FakeBlankProber with nothing scripted —
// ProbeBlank on an unscripted device reports not blank, no error, the
// same "found nothing to say it's blank" default a real probe's blkid -p
// gives for a device it cannot classify as either.
func NewFakeBlankProber() *FakeBlankProber {
	return &FakeBlankProber{blank: make(map[string]bool), errs: make(map[string]error)}
}

// ScriptBlank scripts dev's next ProbeBlank call to report blank (true,
// nil) — blkid -p's own exit 2, "no signature found".
func (f *FakeBlankProber) ScriptBlank(dev string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blank[dev] = true
	delete(f.errs, dev)
}

// ScriptFound scripts dev's next ProbeBlank call to report a positively
// found signature (false, nil) — blkid -p's own exit 0, a filesystem or
// a partition table.
func (f *FakeBlankProber) ScriptFound(dev string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blank[dev] = false
	delete(f.errs, dev)
}

// ScriptError scripts dev's next ProbeBlank call to return err — an
// ambiguous low-level result or any other probe failure.
func (f *FakeBlankProber) ScriptError(dev string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[dev] = err
	delete(f.blank, dev)
}

// ProbeBlank implements BlankProber against whatever was scripted for
// dev, recording every call regardless.
func (f *FakeBlankProber) ProbeBlank(ctx context.Context, dev string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probed = append(f.probed, dev)
	if err, ok := f.errs[dev]; ok {
		return false, err
	}
	return f.blank[dev], nil
}

// Probed returns every device ProbeBlank was actually called against, in
// call order.
func (f *FakeBlankProber) Probed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.probed))
	copy(out, f.probed)
	return out
}

var _ BlankProber = (*FakeBlankProber)(nil)

// FakeBlankReadback is a scriptable BlankReadback (#398, CLAUDE.md): a
// test scripts exactly what Readback(dev) returns and can assert on
// every device it was actually opened against. Unscripted defaults to an
// error — a real device this fake was never told to open must never be
// reported readable by accident, the same fail-closed default the real
// readback has for any device it genuinely cannot read.
type FakeBlankReadback struct {
	mu     sync.Mutex
	ok     map[string]bool
	errs   map[string]error
	opened []string
}

// NewFakeBlankReadback returns a FakeBlankReadback with nothing
// scripted.
func NewFakeBlankReadback() *FakeBlankReadback {
	return &FakeBlankReadback{ok: make(map[string]bool), errs: make(map[string]error)}
}

// ScriptOK scripts dev's next Readback call to succeed.
func (f *FakeBlankReadback) ScriptOK(dev string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ok[dev] = true
	delete(f.errs, dev)
}

// ScriptError scripts dev's next Readback call to return err — an open
// failure, a read error or a short read.
func (f *FakeBlankReadback) ScriptError(dev string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[dev] = err
	delete(f.ok, dev)
}

// Readback implements BlankReadback against whatever was scripted for
// dev, recording every call regardless. A dev with nothing scripted
// refuses (fail-closed), the same as a real device this fake was never
// told about.
func (f *FakeBlankReadback) Readback(ctx context.Context, dev string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, dev)
	if err, ok := f.errs[dev]; ok {
		return err
	}
	if f.ok[dev] {
		return nil
	}
	return fmt.Errorf("disk: no readback scripted for %s", dev)
}

// Opened returns every device Readback was actually called against, in
// call order.
func (f *FakeBlankReadback) Opened() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.opened))
	copy(out, f.opened)
	return out
}

var _ BlankReadback = (*FakeBlankReadback)(nil)

var _ Runner = (*FakeRunner)(nil)
