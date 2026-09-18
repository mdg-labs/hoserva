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

	// Now and Sleep default to the wall clock; tests override them to
	// script FailAfter/SlowDown without actually waiting.
	Now   Clock
	Sleep Sleeper
}

// NewFakeProvider returns a FakeProvider with no disks and the real clock.
func NewFakeProvider() *FakeProvider {
	return &FakeProvider{
		disks: make(map[string]*fakeDisk),
		Now:   time.Now,
		Sleep: time.Sleep,
	}
}

// AddDisk registers a disk, healthy and spun up, ready for a test or the
// frontend dev server to see through List.
func (f *FakeProvider) AddDisk(dev string, d Disk) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d.Device = dev
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
	fd, ok := f.disks[dev]
	if !ok || fd.formattedAs == "" {
		return "", false
	}
	return fd.formattedAs, true
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
	defer f.mu.Unlock()

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
	return out, nil
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

var _ Runner = (*FakeRunner)(nil)
