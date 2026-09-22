package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/mdg-labs/hoserva/internal/auth"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/notify"
)

// upsControlSocketName is the ups control socket's filename, always a
// sibling of the main API socket (cfg.socketPath) — sharing its directory
// means it inherits the same production (/run/hoserva) or dev
// (workspace-local) placement without a flag of its own.
const upsControlSocketName = "ups-control.sock"

// upsControlSocketPath returns the sibling path upsControlSocketName
// resolves to next to apiSocketPath.
func upsControlSocketPath(apiSocketPath string) string {
	return filepath.Join(filepath.Dir(apiSocketPath), upsControlSocketName)
}

// systemctlPowerOff is job.PowerOff's real implementation (doc 02 §6,
// Q70): the last step of the low-battery shutdown sequence, run only
// once job.ArraySequence.Stop has already checkpointed every job and torn
// storage down.
type systemctlPowerOff struct {
	Runner disk.Runner
}

func (p systemctlPowerOff) PowerOff(ctx context.Context) error {
	_, err := p.Runner.Run(ctx, "systemctl", "poweroff")
	if err != nil {
		return fmt.Errorf("systemctl poweroff: %w", err)
	}
	return nil
}

// upsNotifier adapts notify.Service to job.UPSNotifier, the same
// log-and-continue pattern scheduleNotifier (schedule.go) already uses
// for the guard-blocked notification: a failed delivery must never turn
// into a failed shutdown or a lost on-battery reaction.
type upsNotifier struct {
	svc *notify.Service
}

func (n *upsNotifier) NotifyUPSOnBattery(ctx context.Context) {
	if n == nil || n.svc == nil {
		return
	}
	if err := n.svc.PublishUPSOnBattery(ctx); err != nil {
		log.Printf("hoservad: notifying ups on-battery: %v", err)
	}
}

func (n *upsNotifier) NotifyUPSBatteryLow(ctx context.Context) {
	if n == nil || n.svc == nil {
		return
	}
	if err := n.svc.PublishUPSBatteryLow(ctx); err != nil {
		log.Printf("hoservad: notifying ups battery low: %v", err)
	}
}

// upsShutdownLookup composes job.UPSShutdown (Q77, doc 02 §6) against
// whichever job.ArraySequence currentArray reports at the moment a
// low-battery shutdown actually runs, not whichever one existed when
// newUPSController built this — a live array creation after daemon
// startup (#262) must be visible to a shutdown that happens afterward but
// before any restart, not leave this stuck on the nil/empty sequence
// startup saw (#263). currentArray is handler.CurrentArray in production;
// a nil currentArray (no array ever configured) falls back to Scheduler
// alone, same as newUPSController's own pre-#263 fallback, so a
// low-battery event still checkpoints any running job.
type upsShutdownLookup struct {
	scheduler    *job.Scheduler
	currentArray func() *job.ArraySequence
	power        job.PowerOff
}

func (u upsShutdownLookup) Shutdown(ctx context.Context) error {
	seq := job.ArraySequence{Scheduler: u.scheduler}
	if u.currentArray != nil {
		if current := u.currentArray(); current != nil {
			seq = *current
		}
	}
	return job.UPSShutdown{Array: seq, Power: u.power}.Shutdown(ctx)
}

// newUPSController builds the daemon's single job.UPSController (Q77,
// doc 02 §6): its Shutdown resolves currentArray at the moment a
// low-battery event actually triggers a shutdown (upsShutdownLookup
// above), never once at construction — #262 lets an array be created live
// on an already-running daemon, and this must react to that array, not a
// stale snapshot from before it existed.
func newUPSController(scheduler *job.Scheduler, currentArray func() *job.ArraySequence, notifyService *notify.Service, runner disk.Runner) *job.UPSController {
	return &job.UPSController{
		Scheduler: scheduler,
		Notifier:  &upsNotifier{svc: notifyService},
		Shutdown: upsShutdownLookup{
			scheduler:    scheduler,
			currentArray: currentArray,
			power:        systemctlPowerOff{Runner: runner},
		},
	}
}

// serveUPSControl accepts connections on ln until ctx is cancelled,
// handling each with handleUPSControlConn. This is the wiring doc 02 §6
// and Q77 describe: NUT's own upsmon reaches the running daemon's
// *job.UPSController — the one instance holding the live Scheduler state
// Array.Stop needs to checkpoint a running job — only through this
// socket. A separate `hoservad -ups-notify`/`-ups-shutdown` process
// building its own fresh job.ArraySequence could never see those
// in-memory jobs at all (job.Scheduler.Drain waits on channels only the
// daemon's own goroutines hold), so both hooks must reach this same
// process, never a standalone one.
func serveUPSControl(ctx context.Context, ln net.Listener, controller *job.UPSController, lookup auth.GroupLookup, daemonUID uint32) {
	var warnMissingGroupOnce sync.Once
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("hoservad: ups control socket accept: %v", err)
			continue
		}
		go handleUPSControlConn(ctx, conn, controller, lookup, daemonUID, &warnMissingGroupOnce)
	}
}

// handleUPSControlConn reads one line — a job.UPSNotifyType value — and
// replies "OK\n" or "ERROR: <message>\n". `hoservad -ups-notify=TYPE`
// sends TYPE verbatim (NUT's own NOTIFYTYPE, ONBATT/ONLINE/LOWBATT);
// `hoservad -ups-shutdown` (SHUTDOWNCMD's own entry point) always sends
// LOWBATT — the same job.UPSController.HandleNotify call NOTIFYCMD's own
// LOWBATT notification makes, so both of upsmon's independent shutdown
// triggers run the identical, single-in-flight-run sequence (job/ups.go's
// own shutdownRun) rather than racing.
func handleUPSControlConn(ctx context.Context, conn net.Conn, controller *job.UPSController, lookup auth.GroupLookup, daemonUID uint32, warnMissingGroupOnce *sync.Once) {
	defer func() { _ = conn.Close() }()

	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return
	}
	cred, err := auth.PeerCredentialOf(uc)
	if err != nil {
		replyLine(conn, "ERROR: identifying the caller: %v", err)
		return
	}

	authorized, err := authorizeUnixPeer(cred, lookup, daemonUID)
	switch {
	case err == nil:
	case errors.Is(err, auth.ErrGroupNotFound):
		warnMissingGroupOnce.Do(func() {
			log.Printf("hoservad: group %q does not exist — the ups control socket accepts only root and this daemon's own user until it is created (Q44)", hoservaGroup)
		})
	default:
		replyLine(conn, "ERROR: checking authorization: %v", err)
		return
	}
	if !authorized {
		replyLine(conn, "ERROR: forbidden: connect as root, this daemon's own user, or a member of the hoserva group")
		return
	}

	line, readErr := bufio.NewReader(conn).ReadString('\n')
	notifyType := strings.TrimSpace(line)
	if notifyType == "" {
		replyLine(conn, "ERROR: empty request")
		return
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		replyLine(conn, "ERROR: reading request: %v", readErr)
		return
	}

	if err := controller.HandleNotify(ctx, job.UPSNotifyType(notifyType)); err != nil {
		replyLine(conn, "ERROR: %v", err)
		return
	}
	replyLine(conn, "OK")
}

// replyLine writes one line to conn. The daemon-side error a caller
// cares about is always controller.HandleNotify's own return value,
// already reported over this same connection by the time replyLine is
// called for it — a failed write here only means the peer (dialUPSControl,
// always run synchronously by a helper NUT just forked) already hung up
// or died, nothing further to do about it.
func replyLine(conn net.Conn, format string, args ...any) {
	_, _ = fmt.Fprintf(conn, format+"\n", args...)
}

// runUPSControlClient is `hoservad -ups-notify=TYPE`/`-ups-shutdown`'s own
// entry point (main.go): both are transient, one-shot invocations — NUT's
// own NOTIFYCMD and SHUTDOWNCMD each fork a fresh process — never the
// long-running daemon itself, so they get their own signal-bounded
// context rather than run's.
func runUPSControlClient(cfg config, notifyType string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return dialUPSControl(ctx, upsControlSocketPath(cfg.socketPath), notifyType)
}

// dialUPSControl is the client side both `hoservad -ups-notify` and
// `-ups-shutdown` use: connect to the running daemon's control socket,
// send notifyType, and translate its reply into a Go error — non-nil
// exactly when upsmon should see a non-zero exit (main.go), since a
// SHUTDOWNCMD or NOTIFYCMD helper that reports success without the
// sequence actually completing would defeat the whole point of wiring it
// through job.UPSController in the first place.
func dialUPSControl(ctx context.Context, socketPath, notifyType string) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return fmt.Errorf("connecting to the daemon's ups control socket at %s: %w", socketPath, err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := fmt.Fprintf(conn, "%s\n", notifyType); err != nil {
		return fmt.Errorf("sending %s to the ups control socket: %w", notifyType, err)
	}
	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return fmt.Errorf("reading the ups control socket's reply: %w", err)
	}
	reply = strings.TrimSpace(reply)
	if reply != "OK" {
		return fmt.Errorf("ups control socket refused %s: %s", notifyType, reply)
	}
	return nil
}
