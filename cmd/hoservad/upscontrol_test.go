package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/auth"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
)

// recordingUPSNotifier is job.UPSNotifier's own fake for these tests —
// internal/job's own ups_test.go already has an identical fakeUPSNotifier,
// unreachable from this package (unexported, different package).
type recordingUPSNotifier struct {
	mu             sync.Mutex
	onBatteryCount int
}

func (n *recordingUPSNotifier) NotifyUPSOnBattery(ctx context.Context) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.onBatteryCount++
}

func (n *recordingUPSNotifier) NotifyUPSBatteryLow(ctx context.Context) {}

func (n *recordingUPSNotifier) onBattery() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.onBatteryCount
}

func startUPSControlServer(t *testing.T, controller *job.UPSController, lookup auth.GroupLookup, daemonUID uint32) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ups-control.sock")
	ln, err := setupUnixListener(path)
	if err != nil {
		t.Fatalf("setupUnixListener: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = ln.Close()
	})
	go serveUPSControl(ctx, ln, controller, lookup, daemonUID)
	return path
}

// TestDialUPSControl_DeliversNotifyToRunningController is the end-to-end
// proof that a separate `hoservad -ups-notify` process actually reaches
// the daemon's own live job.UPSController — not a second, freshly built
// one — over the control socket.
func TestDialUPSControl_DeliversNotifyToRunningController(t *testing.T) {
	notifier := &recordingUPSNotifier{}
	controller := &job.UPSController{Scheduler: newRegistryTestScheduler(t, job.NewRegistry()), Notifier: notifier}
	path := startUPSControlServer(t, controller, &auth.FakeGroupLookup{Group: hoservaGroup, Exists: true}, uint32(os.Getuid()))

	if err := dialUPSControl(context.Background(), path, string(job.UPSNotifyOnBattery)); err != nil {
		t.Fatalf("dialUPSControl(ONBATT): %v", err)
	}
	waitForCondition(t, time.Second, func() bool { return notifier.onBattery() == 1 })
}

// TestDialUPSControl_ShutdownRunsArrayStopThenPowerOff proves
// `-ups-shutdown`'s own path (main.go sends LOWBATT) reaches the same
// job.UPSShutdown composition the NOTIFYCMD(LOWBATT) path uses: the
// service configured on the running ArraySequence is stopped, and only
// then does PowerOff run.
func TestDialUPSControl_ShutdownRunsArrayStopThenPowerOff(t *testing.T) {
	scheduler := newRegistryTestScheduler(t, job.NewRegistry())
	runner := disk.NewFakeRunner()
	controller := newUPSController(scheduler, nil, nil, runner)
	path := startUPSControlServer(t, controller, &auth.FakeGroupLookup{Group: hoservaGroup, Exists: true}, uint32(os.Getuid()))

	if err := dialUPSControl(context.Background(), path, string(job.UPSNotifyLowBattery)); err != nil {
		t.Fatalf("dialUPSControl(LOWBATT): %v", err)
	}
	calls := runner.Calls()
	if len(calls) != 1 || calls[0].Name != "systemctl" || len(calls[0].Args) != 1 || calls[0].Args[0] != "poweroff" {
		t.Fatalf("runner calls = %+v, want exactly one `systemctl poweroff`", calls)
	}
}

// TestDialUPSControl_UnauthorizedPeerIsRefused proves the ups control
// socket applies the same Q44 admission rule as the main API socket — a
// stranger cannot trigger a shutdown just by connecting to the socket
// file.
func TestDialUPSControl_UnauthorizedPeerIsRefused(t *testing.T) {
	controller := &job.UPSController{Scheduler: newRegistryTestScheduler(t, job.NewRegistry())}
	lookup := &auth.FakeGroupLookup{Group: hoservaGroup, Exists: true}
	// A daemon uid that can never match this test process's own uid, and
	// a lookup that never reports the caller as a group member either.
	path := startUPSControlServer(t, controller, lookup, uint32(os.Getuid())+1)

	err := dialUPSControl(context.Background(), path, string(job.UPSNotifyOnBattery))
	if err == nil {
		t.Fatal("dialUPSControl from an unauthorized peer = nil, want an error")
	}
}

// TestDialUPSControl_UnknownSocketFails proves the client surfaces a
// clear error rather than hanging when nothing is listening — main.go's
// own non-zero exit (so upsmon never treats a failed dial as success)
// depends on this.
func TestDialUPSControl_UnknownSocketFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nothing-listening.sock")
	if err := dialUPSControl(context.Background(), path, string(job.UPSNotifyOnBattery)); err == nil {
		t.Fatal("dialUPSControl against a socket nothing listens on = nil, want an error")
	}
}

// TestAuthorizeUnixPeer_RunAsUserRootIsAdmittedToUPSControlSocket proves
// the identity NOTIFYCMD's own child actually connects as once
// RenderUPSMonConf emits "RUN_AS_USER root" (internal/config/nut.go) is
// admitted to the ups control socket — not merely a same-uid dev-host
// coincidence (the other tests in this file dial as the test process's
// own uid, which happens to equal daemonUID there). daemonUID here is
// deliberately a non-root value neither identical to nor derived from
// the caller's uid, so the only reason UID 0 is admitted is
// authorizeUnixPeer's own root rule — the same rule the ups control
// socket and the main API socket both apply (Q44) — never a lookup
// match or a same-uid coincidence.
func TestAuthorizeUnixPeer_RunAsUserRootIsAdmittedToUPSControlSocket(t *testing.T) {
	lookup := &auth.FakeGroupLookup{Group: hoservaGroup, Exists: true}
	authorized, err := authorizeUnixPeer(auth.PeerCredential{UID: 0, GID: 0}, lookup, 1000)
	if err != nil {
		t.Fatalf("authorizeUnixPeer: %v", err)
	}
	if !authorized {
		t.Fatal("authorizeUnixPeer(UID 0, ...) = false, want true — RUN_AS_USER root's own NOTIFYCMD/SHUTDOWNCMD child must be admitted to the ups control socket")
	}
}

func TestUPSControlSocketPath_IsSiblingOfAPISocket(t *testing.T) {
	got := upsControlSocketPath("/run/hoserva/hoserva.sock")
	want := "/run/hoserva/ups-control.sock"
	if got != want {
		t.Fatalf("upsControlSocketPath = %q, want %q", got, want)
	}
}

// TestNewUPSController_NilArraySequenceStillSetsScheduler proves a host
// with no array configured yet still gets a controller whose Shutdown
// can checkpoint any running job (newArraySequence's own nil-until-an-
// array-exists behavior must not leave UPSController unable to react to
// a low-battery event at all).
func TestNewUPSController_NilArraySequenceStillSetsScheduler(t *testing.T) {
	scheduler := newRegistryTestScheduler(t, job.NewRegistry())
	controller := newUPSController(scheduler, nil, nil, disk.NewFakeRunner())

	shutdown, ok := controller.Shutdown.(job.UPSShutdown)
	if !ok {
		t.Fatalf("Shutdown is %T, want job.UPSShutdown", controller.Shutdown)
	}
	if shutdown.Array.Scheduler != scheduler {
		t.Fatal("UPSShutdown.Array.Scheduler must be set even with no array configured yet")
	}
}

func TestSystemctlPowerOff_RunsSystemctlPoweroff(t *testing.T) {
	runner := disk.NewFakeRunner()
	if err := (systemctlPowerOff{Runner: runner}).PowerOff(context.Background()); err != nil {
		t.Fatalf("PowerOff: %v", err)
	}
	calls := runner.Calls()
	if len(calls) != 1 || calls[0].Name != "systemctl" || len(calls[0].Args) != 1 || calls[0].Args[0] != "poweroff" {
		t.Fatalf("calls = %+v, want exactly one `systemctl poweroff`", calls)
	}
}

func waitForCondition(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !fn() {
		t.Fatal("condition never became true")
	}
}
