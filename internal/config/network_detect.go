package config

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	readOnlyNetworkManager  = "This host uses NetworkManager. Hoserva can change networking only when the host uses ifupdown."
	readOnlySystemdNetworkd = "This host uses systemd-networkd. Hoserva can change networking only when the host uses ifupdown."
	readOnlyUnknown         = "No supported network backend was detected. Hoserva can change networking only when the host uses ifupdown."
)

// BackendDetector reports which network manager the host is running.
type BackendDetector interface {
	Detect(ctx context.Context) (backend Backend, reason string, err error)
}

// ExecDetector looks at systemd unit state and ifupdown files under Root.
// Unit tests inject a fake instead of this type.
type ExecDetector struct {
	Root     string
	LookPath func(string) (string, error)
	Run      func(ctx context.Context, name string, args ...string) error
}

func (d ExecDetector) lookPath(file string) (string, error) {
	if d.LookPath != nil {
		return d.LookPath(file)
	}
	return exec.LookPath(file)
}

func (d ExecDetector) run(ctx context.Context, name string, args ...string) error {
	if d.Run != nil {
		return d.Run(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...).Run()
}

func (d ExecDetector) unitActive(ctx context.Context, unit string) bool {
	if _, err := d.lookPath("systemctl"); err != nil {
		return false
	}
	return d.run(ctx, "systemctl", "is-active", "--quiet", unit) == nil
}

// Detect reports NetworkManager, systemd-networkd, ifupdown or unknown.
// NetworkManager and systemd-networkd win over ifupdown when their units
// are active, so a desktop install with both packages is read-only.
func (d ExecDetector) Detect(ctx context.Context) (Backend, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	if d.unitActive(ctx, "NetworkManager") {
		return BackendNetworkManager, readOnlyNetworkManager, nil
	}
	if d.unitActive(ctx, "systemd-networkd") {
		return BackendSystemdNetworkd, readOnlySystemdNetworkd, nil
	}
	if _, err := d.lookPath("ifup"); err == nil {
		return BackendIfupdown, "", nil
	}
	root := d.Root
	if root == "" {
		root = "/etc"
	}
	if _, err := os.Stat(filepath.Join(root, "network", "interfaces")); err == nil {
		return BackendIfupdown, "", nil
	}
	return BackendUnknown, readOnlyUnknown, nil
}

// IfupdownRunner applies an interfaces.d change to a live interface.
type IfupdownRunner interface {
	Apply(ctx context.Context, iface string) error
}

// ExecIfupdown runs ifdown/ifup with a structured argv (never a shell).
type ExecIfupdown struct {
	Run func(ctx context.Context, name string, args ...string) error
}

func (e ExecIfupdown) run(ctx context.Context, name string, args ...string) error {
	if e.Run != nil {
		return e.Run(ctx, name, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return fmt.Errorf("config: %s %s: %w", name, strings.Join(args, " "), err)
		}
		return fmt.Errorf("config: %s %s: %w (%s)", name, strings.Join(args, " "), err, msg)
	}
	return nil
}

// Apply brings iface down then up so the managed interfaces.d file is
// the live configuration. ifdown is allowed to fail (the iface may not
// have been up); ifup's error is returned.
func (e ExecIfupdown) Apply(ctx context.Context, iface string) error {
	if !validIfaceName(iface) {
		return fmt.Errorf("%w: invalid interface name %q", ErrNetworkInvalid, iface)
	}
	_ = e.run(ctx, "ifdown", "--force", iface)
	return e.run(ctx, "ifup", iface)
}

// LinkSource lists host NICs. Tests inject a fake; production uses LinuxLinks.
type LinkSource interface {
	List(ctx context.Context) ([]Iface, error)
}

// MemoryDetector is a scriptable fake for tests.
type MemoryDetector struct {
	Backend Backend
	Reason  string
	Err     error
}

func (m MemoryDetector) Detect(context.Context) (Backend, string, error) {
	return m.Backend, m.Reason, m.Err
}

// MemoryRunner records ifupdown Apply calls.
type MemoryRunner struct {
	Calls []string
	Err   error
	Hook  func(iface string) error
}

func (m *MemoryRunner) Apply(_ context.Context, iface string) error {
	m.Calls = append(m.Calls, iface)
	if m.Hook != nil {
		if err := m.Hook(iface); err != nil {
			return err
		}
	}
	return m.Err
}

// MemoryLinks is a scriptable fake NIC list.
type MemoryLinks struct {
	Ifaces []Iface
	Err    error
}

func (m MemoryLinks) List(context.Context) ([]Iface, error) {
	if m.Err != nil {
		return nil, m.Err
	}
	out := make([]Iface, len(m.Ifaces))
	copy(out, m.Ifaces)
	return out, nil
}

func defaultDNS(root string) []string {
	raw, err := os.ReadFile(filepath.Join(root, "resolv.conf"))
	if err != nil {
		return nil
	}
	var dns []string
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "nameserver" {
			dns = append(dns, fields[1])
		}
	}
	return dns
}

func defaultGateway(procNetRoute string) (iface, gw string) {
	raw, err := os.ReadFile(procNetRoute)
	if err != nil {
		return "", ""
	}
	for i, line := range strings.Split(string(raw), "\n") {
		if i == 0 {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		if fields[1] != "00000000" {
			continue
		}
		ip, err := parseHexIPv4(fields[2])
		if err != nil {
			continue
		}
		return fields[0], ip
	}
	return "", ""
}

func parseHexIPv4(s string) (string, error) {
	if len(s) != 8 {
		return "", errors.New("not an IPv4 hex gateway")
	}
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 4 {
		return "", errors.New("not an IPv4 hex gateway")
	}
	return fmt.Sprintf("%d.%d.%d.%d", raw[3], raw[2], raw[1], raw[0]), nil
}

func mergeLive(listed []Iface, managed map[string]Iface, dns []string, gwIface, gw string) []Iface {
	out := make([]Iface, 0, len(listed))
	for _, live := range listed {
		if cfg, ok := managed[live.Name]; ok {
			live.Method = cfg.Method
			if cfg.Address != "" {
				live.Address = cfg.Address
				live.Prefix = cfg.Prefix
			}
			if cfg.Gateway != "" {
				live.Gateway = cfg.Gateway
			}
			if len(cfg.DNS) > 0 {
				live.DNS = cfg.DNS
			}
		}
		if live.Gateway == "" && (gwIface == "" || gwIface == live.Name) {
			live.Gateway = gw
		}
		if len(live.DNS) == 0 {
			live.DNS = dns
		}
		out = append(out, live)
	}
	return out
}

func detectTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 5*time.Second)
}
