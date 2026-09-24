package config

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// Q77 UPS config paths, relative to Generator.Root ("/etc" in production,
// so these land at /etc/nut/*.conf — the paths Debian's nut package
// reads). PathUPSMonConf and PathUPSDUsers carry the MONITOR/upsd.users
// password (WriteUPS below writes both root:nut 0640, Mode:
// upsmonConfFileMode/upsdUsersFileMode with Group: NUTGroup, so the
// service that reads each one after dropping to its own account — upsmon
// as nut, upsd as nut — can still open it); PathNUTConf and PathUPSConf
// embed nothing secret and write at defaultFileMode.
const (
	PathNUTConf    = "nut/nut.conf"
	PathUPSConf    = "nut/ups.conf"
	PathUPSDUsers  = "nut/upsd.users"
	PathUPSMonConf = "nut/upsmon.conf"
)

// UPSConnection is doc 03 §8.1's UPS connection choice-cards.
type UPSConnection string

const (
	UPSConnectionUSB     UPSConnection = "usb"
	UPSConnectionNetwork UPSConnection = "network"
)

// UPSLocalName is the fixed name Hoserva's own local ups.conf driver
// stanza and upsmon.conf MONITOR line use for a USB-attached UPS (Q77
// supports exactly one UPS) — never user-editable, since nothing else on
// this host ever needs to name it differently.
const UPSLocalName = "hoserva-ups"

// UPSMonitorUser is the fixed local account name upsd and upsmon use to
// talk to each other over a USB-attached UPS. A network NUT server's own
// monitoring account is that server's own configuration, supplied by the
// user as UPSState.NetworkUsername/NetworkPassword instead.
const UPSMonitorUser = "hoserva-monitor"

// UPSNotifyCmd is the fixed path upsmon.conf's own NOTIFYCMD names. It is
// a literal constant, never built from user or template input (CLAUDE.md:
// "never interpolate user or template input into a shell command") — NUT
// itself invokes it with the notification type in $NOTIFYTYPE
// (upsmon.conf(5)). The script at this path (packaging/) is a thin
// wrapper around `hoservad -ups-notify`, which relays the notification to
// the running daemon's job.UPSController.HandleNotify over its own
// control socket (cmd/hoservad/upscontrol.go) — the live Scheduler state
// Array.Stop needs to checkpoint a running job only exists in that one
// process.
const UPSNotifyCmd = "/usr/lib/hoserva/nut-notify"

// UPSShutdownCmd is the fixed path upsmon.conf's own SHUTDOWNCMD names —
// a literal constant for the same reason UPSNotifyCmd is. Unlike NUT's
// own default shutdown command, this is a Hoserva-owned synchronous
// helper (packaging/'s thin wrapper around `hoservad -ups-shutdown`) that
// runs the exact same checkpoint-then-stop-then-power-off sequence
// UPSNotifyCmd's own LOWBATT notification already triggers, so upsmon's
// FSD path — which calls SHUTDOWNCMD independently of, and shortly after,
// its own NOTIFYCMD(LOWBATT) — can never race a raw `shutdown -h +0`
// against Hoserva's own clean teardown (doc 02 §6, Q70, Q77).
const UPSShutdownCmd = "/usr/lib/hoserva/nut-shutdown"

// UPSState is doc 03 §8.1's UPS settings card, generated into NUT's own
// config from the database (Q77) — the shape testdata/ups/*/state.json
// carries for golden tests.
type UPSState struct {
	Connection UPSConnection `json:"connection"`

	// USB fields (Connection == UPSConnectionUSB): the local driver NUT's
	// own upsdrvctl runs against the attached device. Driver names one
	// of NUT's own supported drivers (e.g. "usbhid-ups", "nutdrv_qx");
	// Hoserva never inspects or validates hardware compatibility itself
	// (D1: orchestrate NUT, never reimplement it).
	Driver          string `json:"driver,omitempty"`
	Port            string `json:"port,omitempty"`
	MonitorPassword string `json:"monitor_password,omitempty"`

	// Network fields (Connection == UPSConnectionNetwork): a remote NUT
	// server this host monitors as a client. Its own ups.conf and
	// upsd.users are that server's configuration, not Hoserva's.
	NetworkHost     string `json:"network_host,omitempty"`
	NetworkPort     int    `json:"network_port,omitempty"`
	NetworkUPSName  string `json:"network_ups_name,omitempty"`
	NetworkUsername string `json:"network_username,omitempty"`
	NetworkPassword string `json:"network_password,omitempty"`

	// LowBatteryPercent and RuntimeSeconds are Q77's clean-shutdown
	// trigger: "at low battery, or after a configurable runtime on
	// battery". They render into the local driver stanza's own
	// override.battery.* variables (USB mode only) — the trigger a NUT
	// driver that supports runtime estimation reports back as its own LB
	// status. A network NUT server's LOWBATT threshold is that server's
	// own configuration; Hoserva has no file of its own to push it into,
	// so these are silently ignored outside USB mode (D1: NUT decides
	// when a UPS is "low", Hoserva only reacts).
	LowBatteryPercent int `json:"low_battery_percent,omitempty"`
	RuntimeSeconds    int `json:"runtime_seconds,omitempty"`
}

// ErrInvalidUPSField reports a UPSState field the render functions below
// cannot safely place in a generated NUT config file: every render
// function inserts these fields directly into config lines, and NUT's
// own file formats have no escaping for them, so a CR or LF could add an
// attacker- or fat-fingered-controlled directive line, and whitespace in
// a MONITOR-line field (upsmon.conf(5)) would misalign its positional
// tokens. validateUPSState is the one place this is checked, called from
// both CanWriteUPS (preflight) and WriteUPS (the actual sink), since
// WriteUPS has no other caller yet to guarantee CanWriteUPS ran first.
var ErrInvalidUPSField = fmt.Errorf("config: ups field contains whitespace or a control character")

// validateUPSState rejects a whitespace or control character in any
// field the render functions below place in a NUT config file.
func validateUPSState(state UPSState) error {
	fields := []struct {
		name  string
		value string
	}{
		{"driver", state.Driver},
		{"port", state.Port},
		{"monitor_password", state.MonitorPassword},
		{"network_host", state.NetworkHost},
		{"network_ups_name", state.NetworkUPSName},
		{"network_username", state.NetworkUsername},
		{"network_password", state.NetworkPassword},
	}
	for _, f := range fields {
		if strings.IndexFunc(f.value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
			return fmt.Errorf("%w: %s", ErrInvalidUPSField, f.name)
		}
	}
	return nil
}

// RenderNUTConf renders /etc/nut/nut.conf's MODE line: standalone for a
// USB-attached UPS Hoserva runs its own upsd against, netclient for a
// remote NUT server Hoserva only monitors.
func RenderNUTConf(state UPSState) string {
	mode := "netclient"
	if state.Connection == UPSConnectionUSB {
		mode = "standalone"
	}
	return fmt.Sprintf("MODE=%s\n", mode)
}

// RenderUPSConf renders the local driver stanza ups.conf needs for a
// USB-attached UPS (Q77). It has no meaning in network mode — the
// remote server's own ups.conf already names its own driver — so callers
// only render this for UPSConnectionUSB.
func RenderUPSConf(state UPSState) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s]\n", UPSLocalName)
	fmt.Fprintf(&b, "\tdriver = %s\n", state.Driver)
	fmt.Fprintf(&b, "\tport = %s\n", state.Port)
	if state.LowBatteryPercent > 0 {
		fmt.Fprintf(&b, "\toverride.battery.charge.low = %d\n", state.LowBatteryPercent)
	}
	if state.RuntimeSeconds > 0 {
		fmt.Fprintf(&b, "\toverride.battery.runtime.low = %d\n", state.RuntimeSeconds)
	}
	b.WriteString("\tdesc = \"Hoserva-managed UPS\"\n")
	return b.String()
}

// RenderUPSDUsers renders the local monitoring account upsd.users needs
// for a USB-attached UPS (Q77) — meaningless in network mode, for the
// same reason as RenderUPSConf.
func RenderUPSDUsers(state UPSState) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s]\n", UPSMonitorUser)
	fmt.Fprintf(&b, "\tpassword = %s\n", state.MonitorPassword)
	b.WriteString("\tupsmon master\n")
	return b.String()
}

// RenderUPSMonConf renders upsmon.conf: the MONITOR line for whichever
// connection is configured, and the NOTIFYCMD/NOTIFYFLAG wiring Q77's
// on-battery/power-restored/low-battery reactions (doc 02 §6) run
// through — upsmon itself decides when the UPS is on battery or low
// (D1), Hoserva only names the command it calls back into.
//
// upsmon splits into a privileged parent, which alone ever runs
// SHUTDOWNCMD, and an unprivileged child dropped to RUN_AS_USER, which
// runs everything else: NOTIFYCMD, and — for a network UPS — the upsd
// protocol client that parses whatever the remote server sends back for
// "MONITOR ...@<host> ... slave". This omits RUN_AS_USER entirely, so
// upsmon keeps Debian's nut package default (the nut user) rather than
// putting that unprivileged majority back under root: the ups control
// socket NOTIFYCMD/SHUTDOWNCMD's helpers reach
// (cmd/hoservad/upscontrol.go) is instead root:nut 0660 in its own
// right (applySocketGroupPermissions, main.go), admitting any nut-group
// peer on that socket alone — nut never joins the hoserva group, which
// stays reserved for root-equivalent admin access (Q44, doc 01 §7): a
// parsing bug in this unprivileged child's own network upsd client,
// exploited by a malicious remote NUT server, must never reach
// hoserva.sock. SHUTDOWNCMD still runs in upsmon's own root parent
// process regardless of RUN_AS_USER (upsmon.conf(5)), so nut-shutdown is
// unaffected by this.
func RenderUPSMonConf(state UPSState) string {
	var b strings.Builder
	switch state.Connection {
	case UPSConnectionUSB:
		fmt.Fprintf(&b, "MONITOR %s@localhost 1 %s %s master\n", UPSLocalName, UPSMonitorUser, state.MonitorPassword)
	case UPSConnectionNetwork:
		fmt.Fprintf(&b, "MONITOR %s@%s 1 %s %s slave\n", state.NetworkUPSName, networkHostPort(state), state.NetworkUsername, state.NetworkPassword)
	}
	b.WriteString("MINSUPPLIES 1\n")
	fmt.Fprintf(&b, "SHUTDOWNCMD \"%s\"\n", UPSShutdownCmd)
	fmt.Fprintf(&b, "NOTIFYCMD %s\n", UPSNotifyCmd)
	b.WriteString("NOTIFYFLAG ONBATT SYSLOG+EXEC\n")
	b.WriteString("NOTIFYFLAG ONLINE SYSLOG+EXEC\n")
	b.WriteString("NOTIFYFLAG LOWBATT SYSLOG+EXEC\n")
	return b.String()
}

func networkHostPort(state UPSState) string {
	if state.NetworkPort != 0 {
		return fmt.Sprintf("%s:%d", state.NetworkHost, state.NetworkPort)
	}
	return state.NetworkHost
}

// CanWriteUPS preflights every file WriteUPS would write for state, so a
// caller applying a UPS configuration change can refuse before writing
// any of them (Q76's own CanWrite, extended to a multi-file apply the
// way CanWriteShareFiles already does for shares).
func (g *Generator) CanWriteUPS(ctx context.Context, state UPSState) error {
	if err := validateUPSState(state); err != nil {
		return err
	}
	if _, err := g.groupID(NUTGroup); err != nil {
		return err
	}
	if err := g.CanWrite(ctx, PathNUTConf); err != nil {
		return err
	}
	if err := g.CanWrite(ctx, PathUPSMonConf); err != nil {
		return err
	}
	if state.Connection != UPSConnectionUSB {
		return nil
	}
	if err := g.CanWrite(ctx, PathUPSConf); err != nil {
		return err
	}
	return g.CanWrite(ctx, PathUPSDUsers)
}

// NUTGroup is the group Debian's nut packages run upsd and upsmon's own
// unprivileged child under. Both upsd.users and upsmon.conf are read
// after dropping to it, so each is written root:nut 0640 — NUT's own
// recommended upsd.users ownership (upsd.users(5)), applied to
// upsmon.conf for the same reason — never root-only.
const NUTGroup = "nut"

const upsdUsersFileMode = 0o640
const upsmonConfFileMode = 0o640

// WriteUPS renders and writes every NUT config file state's connection
// calls for: nut.conf and upsmon.conf always, plus ups.conf and
// upsd.users for a USB-attached UPS. Switching away from USB removes the
// now-stale local driver files (reconcileUSBFiles) — the same
// removed-when-no-longer-applicable pattern WritePoolMounts already uses
// for a share that stops needing its own mover-target unit.
func (g *Generator) WriteUPS(ctx context.Context, state UPSState, command string, revision int, now time.Time) error {
	if err := validateUPSState(state); err != nil {
		return err
	}
	if _, err := g.groupID(NUTGroup); err != nil {
		return err
	}
	if err := g.Write(ctx, File{Path: PathNUTConf, Command: command, Body: []byte(RenderNUTConf(state))}, revision, now); err != nil {
		return err
	}
	if err := g.Write(ctx, File{Path: PathUPSMonConf, Command: command, Body: []byte(RenderUPSMonConf(state)), Mode: upsmonConfFileMode, Group: NUTGroup}, revision, now); err != nil {
		return err
	}
	if state.Connection != UPSConnectionUSB {
		return g.reconcileUSBFiles(ctx)
	}
	if err := g.Write(ctx, File{Path: PathUPSConf, Command: command, Body: []byte(RenderUPSConf(state))}, revision, now); err != nil {
		return err
	}
	return g.Write(ctx, File{Path: PathUPSDUsers, Command: command, Body: []byte(RenderUPSDUsers(state)), Mode: upsdUsersFileMode, Group: NUTGroup}, revision, now)
}

// reconcileUSBFiles removes ups.conf and upsd.users once the connection
// switches away from USB: a stale local driver stanza left behind after
// switching to a network NUT server would otherwise keep naming a UPS
// Hoserva no longer runs a driver for locally. RemoveManaged only ever
// deletes a file still StatusManaged, so a hand-edited one is left for a
// human to resolve (pool.go's reconcilePoolMounts precedent).
func (g *Generator) reconcileUSBFiles(ctx context.Context) error {
	if _, err := g.RemoveManaged(ctx, PathUPSConf); err != nil {
		return err
	}
	_, err := g.RemoveManaged(ctx, PathUPSDUsers)
	return err
}
