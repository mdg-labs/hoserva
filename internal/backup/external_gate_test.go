package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// gateRig is an external disk "usb" whose mount is a directory the test
// owns: unmounting it moves the directory away, the way an unmount hides
// what the disk holds, so anything a backup writes afterwards lands in a
// mount point that no longer exists — the boot device.
type gateRig struct {
	*externalRig
	mountDir string
	image    string

	mu      sync.Mutex
	mounted bool
	// checkHook, when set, runs inside the next mount check, after it has read
	// the mount state — the point between "confirmed mounted" and the write.
	checkHook func(mounted bool)
}

func newGateRig(t *testing.T, gates *ExternalWriteGates) *gateRig {
	t.Helper()
	base := newExternalRig(t)
	dest := base.registerFlagged(t, "usb")
	base.svc.ExternalGates = gates
	r := &gateRig{externalRig: base, mountDir: dest.Path, image: filepath.Join(t.TempDir(), "disk-image"), mounted: true}
	if err := os.MkdirAll(r.mountDir, 0o755); err != nil {
		t.Fatal(err)
	}
	base.svc.ExternalMounted = func(_ context.Context, path string) (bool, error) {
		r.mu.Lock()
		mounted, hook := r.mounted && path == r.mountDir, r.checkHook
		r.checkHook = nil
		r.mu.Unlock()
		if hook != nil {
			hook(mounted)
		}
		return mounted, nil
	}
	return r
}

func (r *gateRig) unmount(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := os.Rename(r.mountDir, r.image); err != nil {
		return err
	}
	r.mounted = false
	return nil
}

func (r *gateRig) mount(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := os.Rename(r.image, r.mountDir); err != nil {
		return err
	}
	r.mounted = true
	return nil
}

func (r *gateRig) archives(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	return len(entries)
}

// blockNextMountCheck makes the next mount check announce itself and wait:
// the backup has read "mounted" and not yet written.
func (r *gateRig) blockNextMountCheck() (checking <-chan struct{}, proceed func()) {
	c, p := make(chan struct{}), make(chan struct{})
	r.mu.Lock()
	r.checkHook = func(bool) {
		close(c)
		<-p
	}
	r.mu.Unlock()
	return c, func() { close(p) }
}

// waitClosed returns once an Eject of label has closed its gate: it holds the
// disk's lifecycle and is waiting for the writes admitted before it.
func waitClosed(t *testing.T, gates *ExternalWriteGates, label string) {
	t.Helper()
	d := gates.disk(label)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		d.writes.mu.Lock()
		closed := d.writes.closed
		d.writes.mu.Unlock()
		if closed {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the eject never closed the disk's gate")
}

func (r *gateRig) runInBackground() <-chan error {
	done := make(chan error, 1)
	go func() { done <- r.svc.Run(context.Background()) }()
	return done
}

func TestExternalWriteGates_EjectBetweenTheMountCheckAndTheWriteNeverWritesToTheBootDevice(t *testing.T) {
	r := newGateRig(t, &ExternalWriteGates{})
	checking, proceed := r.blockNextMountCheck()
	run := r.runInBackground()
	<-checking

	ejected := make(chan error, 1)
	go func() { ejected <- r.svc.ExternalGates.Eject(context.Background(), "usb", time.Minute, r.unmount) }()
	waitClosed(t, r.svc.ExternalGates, "usb")
	proceed()

	if err := <-run; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := <-ejected; err != nil {
		t.Fatalf("Eject: %v", err)
	}
	if _, err := os.Stat(r.mountDir); !os.IsNotExist(err) {
		t.Fatalf("something was created at %q after the unmount: %v", r.mountDir, err)
	}
	if n := r.archives(t, r.image); n != 1 {
		t.Fatalf("the disk holds %d archives, want the one written before the unmount", n)
	}
}

// Without the gate the same interleaving lands the archive on the boot
// device: the test above is capable of failing.
func TestExternalWriteGates_UngatedEjectLosesTheRace(t *testing.T) {
	r := newGateRig(t, nil)
	checking, proceed := r.blockNextMountCheck()
	run := r.runInBackground()
	<-checking

	if err := r.svc.ExternalGates.Eject(context.Background(), "usb", time.Minute, r.unmount); err != nil {
		t.Fatalf("Eject: %v", err)
	}
	proceed()
	if err := <-run; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(r.mountDir); err != nil {
		t.Fatalf("the interleaving no longer reproduces the write to the boot device: %v", err)
	}
}

func TestExternalWriteGates_EjectWaitsForTheWriteAndUnmountsAfterIt(t *testing.T) {
	r := newGateRig(t, &ExternalWriteGates{})
	checking, proceed := r.blockNextMountCheck()
	run := r.runInBackground()
	<-checking

	unmounted := make(chan struct{})
	ejected := make(chan error, 1)
	go func() {
		ejected <- r.svc.ExternalGates.Eject(context.Background(), "usb", time.Minute, func(ctx context.Context) error {
			close(unmounted)
			return r.unmount(ctx)
		})
	}()
	waitClosed(t, r.svc.ExternalGates, "usb")
	select {
	case <-unmounted:
		t.Fatal("the disk was unmounted while a backup write was admitted")
	case <-time.After(100 * time.Millisecond):
	}
	proceed()
	<-run
	if err := <-ejected; err != nil {
		t.Fatalf("Eject: %v", err)
	}
}

func TestExternalWriteGates_EjectSkipsTheWriteThatFollowsIt(t *testing.T) {
	r := newGateRig(t, &ExternalWriteGates{})
	if err := r.svc.ExternalGates.Eject(context.Background(), "usb", time.Minute, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("Eject: %v", err)
	}

	// The mount table still says mounted: only the gate can refuse this.
	err := r.svc.Run(context.Background())
	if err == nil {
		t.Fatal("Run reported success with its only destination ejected")
	}
	if n := r.archives(t, r.mountDir); n != 0 {
		t.Fatalf("a write reached the ejected disk: %d entries", n)
	}
	if len(r.logs) != 1 || !strings.Contains(r.logs[0], "external:usb") || !strings.Contains(r.logs[0], "ejected") {
		t.Fatalf("logs = %q, want one line naming the skipped destination and the eject", r.logs)
	}
}

func TestExternalWriteGates_MountReadmitsWrites(t *testing.T) {
	r := newGateRig(t, &ExternalWriteGates{})
	if err := r.svc.ExternalGates.Eject(context.Background(), "usb", time.Minute, r.unmount); err != nil {
		t.Fatalf("Eject: %v", err)
	}
	if err := r.svc.ExternalGates.Mount(context.Background(), "usb", r.mount); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	if err := r.svc.Run(context.Background()); err != nil {
		t.Fatalf("Run after the disk was mounted again: %v", err)
	}
	if n := r.archives(t, r.mountDir); n != 1 {
		t.Fatalf("the remounted disk holds %d archives, want 1", n)
	}
}

func TestExternalWriteGates_FailedMountLeavesTheGateClosed(t *testing.T) {
	r := newGateRig(t, &ExternalWriteGates{})
	if err := r.svc.ExternalGates.Eject(context.Background(), "usb", time.Minute, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("Eject: %v", err)
	}
	mountErr := errors.New("mount failed")
	if err := r.svc.ExternalGates.Mount(context.Background(), "usb", func(context.Context) error { return mountErr }); !errors.Is(err, mountErr) {
		t.Fatalf("Mount = %v, want the mount's own error", err)
	}
	if err := r.svc.Run(context.Background()); err == nil {
		t.Fatal("a backup was admitted although the disk failed to mount")
	}
}

func TestExternalWriteGates_EjectThatOutwaitsItsDeadlineLeavesTheDiskMountedAndOpen(t *testing.T) {
	r := newGateRig(t, &ExternalWriteGates{})
	checking, proceed := r.blockNextMountCheck()
	run := r.runInBackground()
	<-checking

	unmounted := false
	err := r.svc.ExternalGates.Eject(context.Background(), "usb", 50*time.Millisecond, func(context.Context) error {
		unmounted = true
		return nil
	})
	if !errors.Is(err, ErrExternalWriteInFlight) {
		t.Fatalf("Eject = %v, want ErrExternalWriteInFlight", err)
	}
	if unmounted {
		t.Fatal("the disk was unmounted although the wait expired")
	}
	proceed()
	if err := <-run; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := r.archives(t, r.mountDir); n != 1 {
		t.Fatalf("the disk holds %d archives, want the in-flight write to complete", n)
	}
	if err := r.svc.Run(context.Background()); err != nil {
		t.Fatalf("a later backup was refused after the eject gave up: %v", err)
	}
}

func TestExternalWriteGates_FailedUnmountReopensTheGate(t *testing.T) {
	r := newGateRig(t, &ExternalWriteGates{})
	busy := errors.New("target is busy")
	if err := r.svc.ExternalGates.Eject(context.Background(), "usb", time.Minute, func(context.Context) error { return busy }); !errors.Is(err, busy) {
		t.Fatalf("Eject = %v, want the unmount's own error", err)
	}
	if err := r.svc.Run(context.Background()); err != nil {
		t.Fatalf("a backup was refused after the eject failed and left the disk mounted: %v", err)
	}
	if n := r.archives(t, r.mountDir); n != 1 {
		t.Fatalf("the mounted disk holds %d archives, want 1", n)
	}
}

func TestExternalWriteGates_RefusedAdmissionReleasesItsSlot(t *testing.T) {
	r := newGateRig(t, &ExternalWriteGates{})
	r.mu.Lock()
	r.mounted = false
	r.mu.Unlock()
	if err := r.svc.Run(context.Background()); err == nil {
		t.Fatal("Run reported success with the disk unmounted")
	}
	err := r.svc.ExternalGates.Eject(context.Background(), "usb", 50*time.Millisecond, func(context.Context) error { return nil })
	if err != nil {
		t.Fatalf("Eject after a refused write: %v", err)
	}
}

func TestExternalWriteGates_MountWaitsForAnEjectInProgress(t *testing.T) {
	r := newGateRig(t, &ExternalWriteGates{})
	checking, proceed := r.blockNextMountCheck()
	run := r.runInBackground()
	<-checking
	ejected := make(chan error, 1)
	go func() { ejected <- r.svc.ExternalGates.Eject(context.Background(), "usb", time.Minute, r.unmount) }()
	waitClosed(t, r.svc.ExternalGates, "usb")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	called := false
	if err := r.svc.ExternalGates.Mount(ctx, "usb", func(context.Context) error { called = true; return nil }); err == nil {
		t.Fatal("Mount ran while an eject of the same disk was in progress")
	}
	if called {
		t.Fatal("the mount ran although it could not take the disk's lifecycle")
	}
	proceed()
	<-run
	if err := <-ejected; err != nil {
		t.Fatalf("Eject: %v", err)
	}
}

func TestExternalWriteGates_GateIsPerDisk(t *testing.T) {
	gates := &ExternalWriteGates{}
	release, ok := gates.begin("usb")
	if !ok {
		t.Fatal("begin refused on an open gate")
	}
	defer release()
	if err := gates.Eject(context.Background(), "other", 50*time.Millisecond, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("an eject of another disk waited for usb's write: %v", err)
	}
	if _, ok := gates.begin("usb"); !ok {
		t.Fatal("usb refused writes although only another disk was ejected")
	}
}
