package main

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/auth"
	cfggen "github.com/mdg-labs/hoserva/internal/config"
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
	go serveUPSControl(ctx, ln, controller, lookup, daemonUID, cfggen.NUTGroup)
	return path
}

// TestDialUPSControl_DeliversNotifyToRunningController is the end-to-end
// proof that a separate `hoservad -ups-notify` process actually reaches
// the daemon's own live job.UPSController — not a second, freshly built
// one — over the control socket.
func TestDialUPSControl_DeliversNotifyToRunningController(t *testing.T) {
	notifier := &recordingUPSNotifier{}
	controller := &job.UPSController{Scheduler: newRegistryTestScheduler(t, job.NewRegistry()), Notifier: notifier}
	path := startUPSControlServer(t, controller, &auth.FakeGroupLookup{Group: cfggen.NUTGroup, Exists: true}, uint32(os.Getuid()))

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
	path := startUPSControlServer(t, controller, &auth.FakeGroupLookup{Group: cfggen.NUTGroup, Exists: true}, uint32(os.Getuid()))

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
	lookup := &auth.FakeGroupLookup{Group: cfggen.NUTGroup, Exists: true}
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

// TestDialUPSControl_EmptyNotifyTypeIsRefusedBeforeDialing proves an
// empty NOTIFYTYPE fails the client instead of reaching the daemon.
// main.go selects client mode from the flag's presence rather than its
// value, so an empty value exits non-zero here and never falls through
// to run() against the live daemon's database.
func TestDialUPSControl_EmptyNotifyTypeIsRefusedBeforeDialing(t *testing.T) {
	notifier := &recordingUPSNotifier{}
	controller := &job.UPSController{Scheduler: newRegistryTestScheduler(t, job.NewRegistry()), Notifier: notifier}
	path := startUPSControlServer(t, controller, &auth.FakeGroupLookup{Group: cfggen.NUTGroup, Exists: true}, uint32(os.Getuid()))

	if err := dialUPSControl(context.Background(), path, ""); err == nil {
		t.Fatal(`dialUPSControl(""): nil error, want a refusal`)
	}
	if got := notifier.onBattery(); got != 0 {
		t.Fatalf("notifier saw %d on-battery notifications, want 0", got)
	}
}

// TestAuthorizeUnixPeer_NUTGroupPeerIsAdmittedToUPSControlSocket proves
// the identity NOTIFYCMD's own child actually connects as, once
// applySocketGroupPermissions widens the ups control socket to the nut
// group (cmd/hoservad/main.go) — nut is always in its own nut group, so
// this needs no packaging or admin step at all — is admitted, not a
// root or same-uid coincidence. daemonUID here is deliberately a
// non-root value neither identical to nor derived from the peer's own
// uid, so the only reason this peer is admitted is authorizeUnixPeer's
// own group-membership rule, checked against the nut group the ups
// control socket names.
func TestAuthorizeUnixPeer_NUTGroupPeerIsAdmittedToUPSControlSocket(t *testing.T) {
	lookup := &auth.FakeGroupLookup{Group: cfggen.NUTGroup, Exists: true, Members: map[uint32]bool{2000: true}}
	authorized, err := authorizeUnixPeer(auth.PeerCredential{UID: 3000, GID: 2000}, lookup, 1000, cfggen.NUTGroup)
	if err != nil {
		t.Fatalf("authorizeUnixPeer: %v", err)
	}
	if !authorized {
		t.Fatal("authorizeUnixPeer(nut-group peer, nut group) = false, want true — upsmon's unprivileged nut child must be admitted to the ups control socket via the nut group")
	}
}

// TestAuthorizeUnixPeer_NUTGroupPeerIsRefusedOnHoservaSocket is the
// maintainer decision (#340) this issue's second attempt implements: nut
// never joins the hoserva group, so the same peer admitted to the ups
// control socket above must be refused on hoserva.sock — membership in
// hoserva is root-equivalent (Q44, doc 01 §7: it also admits a peer to
// the admin API, which can format disks), and a parsing bug in upsmon's
// own unprivileged nut child (this issue's whole reason for existing)
// must never reach that far.
func TestAuthorizeUnixPeer_NUTGroupPeerIsRefusedOnHoservaSocket(t *testing.T) {
	lookup := &auth.FakeGroupLookup{Group: cfggen.NUTGroup, Exists: true, Members: map[uint32]bool{2000: true}}
	authorized, err := authorizeUnixPeer(auth.PeerCredential{UID: 3000, GID: 2000}, lookup, 1000, hoservaGroup)
	if err != nil && err != auth.ErrGroupNotFound {
		t.Fatalf("authorizeUnixPeer: %v", err)
	}
	if authorized {
		t.Fatal("authorizeUnixPeer(nut-group peer, hoserva group) = true, want false — nut must never be admitted to hoserva.sock")
	}
}

// TestAuthorizeUnixPeer_UnrelatedNonRootPeerIsRefusedOnUPSControlSocket
// proves a peer that is neither root, this daemon's own uid, nor a
// member of the nut group is still refused on the ups control socket —
// widening that socket to the nut group above must not have widened
// admission beyond Q44's own rule.
func TestAuthorizeUnixPeer_UnrelatedNonRootPeerIsRefusedOnUPSControlSocket(t *testing.T) {
	lookup := &auth.FakeGroupLookup{Group: cfggen.NUTGroup, Exists: true, Members: map[uint32]bool{2000: true}}
	authorized, err := authorizeUnixPeer(auth.PeerCredential{UID: 3000, GID: 9999}, lookup, 1000, cfggen.NUTGroup)
	if err != nil {
		t.Fatalf("authorizeUnixPeer: %v", err)
	}
	if authorized {
		t.Fatal("authorizeUnixPeer(unrelated non-root peer, nut group) = true, want false")
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

	shutdown, ok := controller.Shutdown.(upsShutdownLookup)
	if !ok {
		t.Fatalf("Shutdown is %T, want upsShutdownLookup", controller.Shutdown)
	}
	if shutdown.scheduler != scheduler {
		t.Fatal("upsShutdownLookup.scheduler must be set even with no array configured yet")
	}
}

// TestUPSShutdownLookup_ResolvesCurrentArrayAtShutdownTime is #263's own
// regression: a UPSController built (as newUPSController always is) with
// no array yet configured, whose currentArray func later starts reporting
// a real, live-created job.ArraySequence, must run that real sequence's
// Stop — not the nil/empty one newUPSController saw at construction —
// once a low-battery event actually triggers a shutdown.
func TestUPSShutdownLookup_ResolvesCurrentArrayAtShutdownTime(t *testing.T) {
	scheduler := newRegistryTestScheduler(t, job.NewRegistry())
	runner := disk.NewFakeRunner()

	mount := arrayTestCatchAll{where: "/mnt/disk1", runner: runner}
	var current *job.ArraySequence // nil until "live array creation" below

	controller := newUPSController(scheduler, func() *job.ArraySequence { return current }, nil, runner)

	// Simulate a live array creation completing after the controller was
	// already built, exactly #262's ArrayReady hook does on a running
	// daemon.
	current = &job.ArraySequence{Scheduler: scheduler, Disks: []job.ArrayMount{mount}}

	if err := controller.HandleNotify(context.Background(), job.UPSNotifyLowBattery); err != nil {
		t.Fatalf("HandleNotify(LOWBATT): %v", err)
	}
	calls := runner.Calls()
	if len(calls) != 2 {
		t.Fatalf("runner calls = %+v, want the live array's own unmount then poweroff — a stale/empty sequence would skip straight to poweroff", calls)
	}
	requireArgv(t, calls[0], "fusermount", "-u", "/mnt/disk1")
	requireArgv(t, calls[1], "systemctl", "poweroff")
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

// upsControlConnPair returns the two ends of a connected unix socket: the
// peer a test drives, and the server-side *net.UnixConn
// handleUPSControlConn requires.
func upsControlConnPair(t *testing.T) (peer, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("unix", filepath.Join(t.TempDir(), "pair.sock"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	peer, err = net.Dial("unix", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	if err := peer.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set peer read deadline: %v", err)
	}
	server, err = ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	return peer, server
}

func runUPSControlHandler(ctx context.Context, server net.Conn, controller *job.UPSController) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		var once sync.Once
		handleUPSControlConn(ctx, server, controller, &auth.FakeGroupLookup{Group: cfggen.NUTGroup, Exists: true}, uint32(os.Getuid()), cfggen.NUTGroup, &once)
	}()
	return done
}

// TestHandleUPSControlConn_OversizedRequestIsRefusedWithoutBuffering proves
// a peer that streams bytes with no newline is cut off after a bounded
// read: the handler replies with an error and closes the connection while
// the peer still has most of the stream unsent.
func TestHandleUPSControlConn_OversizedRequestIsRefusedWithoutBuffering(t *testing.T) {
	controller := &job.UPSController{Scheduler: newRegistryTestScheduler(t, job.NewRegistry())}
	peer, server := upsControlConnPair(t)
	done := runUPSControlHandler(context.Background(), server, controller)

	const total = 8 << 20
	written := make(chan int, 1)
	go func() {
		chunk := []byte(strings.Repeat("A", 4096))
		n := 0
		for n < total {
			w, err := peer.Write(chunk)
			n += w
			if err != nil {
				break
			}
		}
		written <- n
	}()

	reply, err := bufio.NewReader(peer).ReadString('\n')
	if err != nil {
		t.Fatalf("reading the reply: %v", err)
	}
	if !strings.HasPrefix(reply, "ERROR:") {
		t.Fatalf("reply = %q, want an ERROR line", reply)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after an oversized request")
	}
	select {
	case n := <-written:
		if n >= total {
			t.Fatalf("peer wrote all %d bytes; the handler drained the stream instead of refusing it", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("peer's writer never stopped after the handler closed the connection")
	}
}

// TestHandleUPSControlConn_SilentPeerTimesOut proves a peer that connects
// and sends nothing does not hold the handler open past the read deadline.
func TestHandleUPSControlConn_SilentPeerTimesOut(t *testing.T) {
	old := upsControlReadTimeout
	upsControlReadTimeout = 100 * time.Millisecond
	t.Cleanup(func() { upsControlReadTimeout = old })

	controller := &job.UPSController{Scheduler: newRegistryTestScheduler(t, job.NewRegistry())}
	peer, server := upsControlConnPair(t)
	done := runUPSControlHandler(context.Background(), server, controller)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler still open long after the read deadline for a peer that sent nothing")
	}
	reply, err := bufio.NewReader(peer).ReadString('\n')
	if err != nil {
		t.Fatalf("reading the reply: %v", err)
	}
	if !strings.HasPrefix(reply, "ERROR:") {
		t.Fatalf("reply = %q, want an ERROR line", reply)
	}
}

// TestServeUPSControl_ConnectionsBeyondTheCapAreClosedUnread proves the
// accept loop runs at most upsControlMaxConns handlers at once: with that many
// silent peers holding every slot, a further connection is closed with no
// reply long before the read deadline would end a handler, and once the
// silent peers time out a real notification is handled again.
func TestServeUPSControl_ConnectionsBeyondTheCapAreClosedUnread(t *testing.T) {
	old := upsControlReadTimeout
	upsControlReadTimeout = 2 * time.Second
	t.Cleanup(func() { upsControlReadTimeout = old })

	notifier := &recordingUPSNotifier{}
	controller := &job.UPSController{Scheduler: newRegistryTestScheduler(t, job.NewRegistry()), Notifier: notifier}
	path := startUPSControlServer(t, controller, &auth.FakeGroupLookup{Group: cfggen.NUTGroup, Exists: true}, uint32(os.Getuid()))

	dial := func() net.Conn {
		t.Helper()
		c, err := net.Dial("unix", path)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}

	idle := make([]net.Conn, upsControlMaxConns)
	for i := range idle {
		idle[i] = dial()
	}
	// The accept loop takes connections in the order they were dialed and
	// claims a slot before it accepts the next, so the extras below meet a
	// full set of slots.

	for i := 0; i < 4; i++ {
		extra := dial()
		if err := extra.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatalf("set read deadline: %v", err)
		}
		n, err := extra.Read(make([]byte, 64))
		if n != 0 || err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("extra connection %d: Read = (%d, %v), want it closed with no reply before the handlers' read timeout", i, n, err)
		}
	}

	for i, c := range idle {
		if err := c.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatalf("set read deadline: %v", err)
		}
		reply, err := bufio.NewReader(c).ReadString('\n')
		if err != nil || !strings.HasPrefix(reply, "ERROR:") {
			t.Fatalf("idle connection %d: reply = %q, err = %v, want the read-timeout ERROR line", i, reply, err)
		}
	}

	// A handler frees its slot only after it returns, which can trail its reply,
	// so a notification sent too early is closed unread; retry until one is taken.
	waitForCondition(t, 5*time.Second, func() bool {
		return dialUPSControl(context.Background(), path, string(job.UPSNotifyOnBattery)) == nil
	})
	waitForCondition(t, time.Second, func() bool { return notifier.onBattery() == 1 })
}
