package pool

import (
	"fmt"
	"strings"
)

// StorageTargetUnitName is the systemd target every storage-dependent
// service orders itself after (doc 02 §1, Q69): Samba, NFS, Docker and
// libvirt each carry a managed drop-in (ServiceDropIn below) making them
// start only once it is reached.
const StorageTargetUnitName = "hoserva-storage.target"

// DependentServiceUnits are the systemd service units doc 02 §1 names as
// starting only after hoserva-storage.target: Samba, NFS, Docker and
// libvirt, by their Debian 13 package unit names (doc 01 §1's table;
// doc 14 §2's `libvirtd`).
var DependentServiceUnits = []string{
	"smbd.service",
	"nfs-kernel-server.service",
	"docker.service",
	"libvirtd.service",
}

// StorageReadyUnitName is the systemd oneshot service hoserva-storage.target
// hard-requires (Q69): hoservad regenerates and (re-)starts it every time
// disk.StorageGate.Evaluate's inputs change (daemon start, a udev-driven
// disk-arrival event), and it succeeds — and stays "active" via
// RemainAfterExit — only once disk.StorageGate.Ready() is true: every
// expected disk present by identity (Q21), or the missing ones explicitly
// acknowledged. Because StorageTargetUnit.Render's Requires=/After= (below)
// names this unit rather than only the disk mounts, a still-degraded,
// unacknowledged array genuinely keeps hoserva-storage.target — and every
// service ordered after it via ServiceDropIn — from ever activating.
const StorageReadyUnitName = "hoserva-storage-ready.service"

// StorageReadyUnit is hoserva-storage-ready.service's own generated
// content. ExecStart is the argv hoservad execs to evaluate
// disk.StorageGate.Ready() — the daemon and CLI command that argv runs
// (internal/api, cmd/hoservad) are not yet built in this repo; this type
// only renders the systemd wiring around it, the same division target.go's
// other Render methods keep from the daemon logic they gate.
type StorageReadyUnit struct {
	ExecStart []string
}

// Render returns u's systemd unit file content, for config.Generator (D4)
// to write to /etc/systemd/system/hoserva-storage-ready.service.
func (u StorageReadyUnit) Render() string {
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=Hoserva storage readiness gate\n\n")
	b.WriteString("[Service]\nType=oneshot\nRemainAfterExit=yes\n")
	if len(u.ExecStart) > 0 {
		fmt.Fprintf(&b, "ExecStart=%s\n", strings.Join(u.ExecStart, " "))
	}
	return b.String()
}

// StorageTargetUnit is hoserva-storage.target's own generated content. The
// disk mount units get only Wants=/After= — deliberately soft, not
// Requires= — because a hard Requires= directly on them would make a
// single missing disk's failed .mount unit block the target from ever
// activating, even after the user explicitly acknowledges the degraded
// state (Q69), and overriding a systemd Requires= failure needs
// `--job-mode=ignore-dependencies` on every start, repeated forever.
// StorageReadyUnitName is the actual gate: a hard Requires=/After= on
// hoserva-storage-ready.service, the hoservad-owned oneshot that succeeds
// only once disk.StorageGate.Ready() is true. That is what makes this
// target genuinely refuse to activate while the array is degraded and
// unacknowledged — Wants=/After= on the disk mounts alone only orders
// services after whatever mounts happen to exist, without ever vetoing
// the start.
type StorageTargetUnit struct {
	DiskMountUnits []string // e.g. "mnt-disk1.mount", "mnt-parity1.mount", "mnt-cache.mount"
}

// Render returns u's systemd unit file content, for config.Generator (D4)
// to write to /etc/systemd/system/hoserva-storage.target.
func (u StorageTargetUnit) Render() string {
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=Hoserva storage ready\n")
	fmt.Fprintf(&b, "Requires=%s\n", StorageReadyUnitName)
	fmt.Fprintf(&b, "After=%s\n", StorageReadyUnitName)
	if len(u.DiskMountUnits) > 0 {
		fmt.Fprintf(&b, "Wants=%s\n", strings.Join(u.DiskMountUnits, " "))
		fmt.Fprintf(&b, "After=%s\n", strings.Join(u.DiskMountUnits, " "))
	}
	return b.String()
}

// ServiceDropIn is the managed systemd drop-in content that makes an
// existing service unit — Samba, NFS, Docker or libvirt — start only
// after hoserva-storage.target (doc 02 §1, Q69): a "10-hoserva-storage"
// override placed alongside the service's own unit file, never editing
// that unit file itself (D4: managed files are generated, and a
// vendor-shipped unit is not Hoserva's file to rewrite).
type ServiceDropIn struct {
	Unit string // e.g. "smbd.service"
}

// DropInPath is where config.Generator (D4) writes this drop-in — a path
// relative to Generator.Root in dev/test, /etc/systemd/system/<Unit>.d/
// in production.
func (d ServiceDropIn) DropInPath() string {
	return fmt.Sprintf("systemd/system/%s.d/10-hoserva-storage.conf", d.Unit)
}

// Render returns d's drop-in content: the service starts only once
// hoserva-storage.target is reached, and stops with it — mirroring the
// doc 02 §4 stop sequence's own "Samba and NFS stop" step for whatever the
// unit file mechanism itself can express (BindsTo also stops the service
// if hoserva-storage.target is ever stopped out from under it, which the
// doc 02 §4 sequence's own explicit stop step makes deliberate, not
// accidental).
func (d ServiceDropIn) Render() string {
	return fmt.Sprintf("[Unit]\nAfter=%s\nBindsTo=%s\n", StorageTargetUnitName, StorageTargetUnitName)
}
