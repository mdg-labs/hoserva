package disk

import (
	"context"
	"fmt"
	"strings"
)

// StorageStoppedFlagPath is the durable flag a persisted, user-requested
// `array stop` creates and `array start` removes (#387, raised by L3
// nightly runs 36258823325 and 36264953516): every Hoserva-generated
// mount unit — every physical disk/parity/cache mount here, and the
// catch-all and every per-share mount (pool.Mount.Render) — carries
// ConditionPathExists=!<this path>, so a start any of them ever receives
// while it exists is skipped as a no-op, never merely refused. A
// transient stop (StopForShutdown, a reboot or a UPS low-battery
// shutdown) never creates this flag and never clears one already in
// force (#387): neither is a user asking the array to stay
// stopped once the box comes back, and a flag left behind here would
// fail every mount unit's own condition in the boot-time systemd
// transaction that runs before hoservad is even exec'd.
//
// This exists because a hard dependency on the storage-readiness gate
// (hoserva-storage-ready.service) cannot live on these units themselves:
// nfs-utils' own systemd integration derives a RequiresMountsFor=
// directly on nfs-server.service for every path listed in /etc/exports —
// confirmed empirically (`systemctl show mnt-user.mount -p RequiredBy`
// named nfs-server.service the moment a share's own NFS export was
// enabled) — an edge entirely outside any unit Hoserva itself writes, and
// reachable the instant the guest boots: smbd.service and
// nfs-kernel-server.service are both systemd-enabled (WantedBy=
// multi-user.target), so systemd starts pulling this same chain — the
// exported share's own mount, the catch-all, the physical data disks
// under it — before hoservad's own process has even been exec'd, let
// alone reached Startup. A `/run`-backed flag (this constant's own first
// shape) cannot gate that: `/run` is a fresh tmpfs every boot, so
// immediately after a crash or a `virsh destroy` the flag is absent no
// matter what was persisted, and confirmed directly against a real
// guest reboot: the boot-time cascade above mounted a data disk and the
// catch-all a full 9 seconds before hoservad's own "Starting
// hoserva.service" line — long before Startup could ever have written a
// tmpfs flag from the persisted array_maintenance row. This path is
// therefore under Hoserva's own durable state directory instead: `array
// stop` writes it as part of closing the storage-target gate (Close,
// cmd/hoservad/storagetarget.go) and it survives a crash or `virsh
// destroy` exactly like the array_maintenance row it mirrors, so it is
// already present — before hoservad, systemd, or anything else has done
// anything this boot — for every mount unit's own condition check to see,
// with no dependency on hoservad's own startup timing at all.
//
// A ConditionPathExists= failure makes systemd skip the mount unit's
// start silently rather than fail it, so nfs-server.service's own
// unrelated dependency job for it still succeeds trivially — exactly as
// if there had been nothing left to mount — instead of remounting a disk
// the user may be mid-swap on.
//
// hoservad's own explicit mounts (ArraySequence.Start's Disks/CatchAll/
// ShareMounts loops) are never affected: StorageTarget.Open removes this
// flag before Start ever calls Mount on anything (job/array.go), and
// Startup (cmd/hoservad/storagetarget.go) reconciles it against the
// persisted array_maintenance row before evaluating readiness at all —
// the row is authoritative (D4); this file is a generated artifact of it,
// exactly like the systemd units themselves. Startup also regenerates
// every managed disk and pool mount unit from SQLite and explicitly
// (re)mounts every physical disk itself before reporting the gate ready,
// so a boot where this flag was stale when the boot-time systemd
// transaction ran — silently skipping a disk's own automatic nofail
// activation — can never leave a disk or the pool unmounted just because
// nothing else was going to retry it.
const StorageStoppedFlagPath = "/var/lib/hoserva/array-stopped"

// MountUnit is one systemd .mount unit for a physical disk (doc 01 §6,
// doc 02 §1): mounted by filesystem UUID (Q21) rather than /dev/sdX,
// which can renumber on reboot; nofail so a missing disk never hangs
// boot. nofail alone is what does this — it drops the mount from
// local-fs.target's required ordering, so boot proceeds without ever
// waiting on the device — not any bounded wait: systemd's
// x-systemd.device-timeout= option is documented to apply only to an
// /etc/fstab entry and is silently ignored in a native unit's own
// Options=, so this renderer never emits it (previously rendered but
// inert, doc 02 §1).
type MountUnit struct {
	Where       string
	UUID        string
	Filesystem  FilesystemType
	Description string
}

// UnitFileName returns the systemd unit filename systemd-escape would
// produce for a mount at where: drop the leading slash, turn every
// remaining slash into '-'. Hoserva's own mount paths (/mnt/disk1,
// /mnt/parity1, /mnt/cache) never contain a character systemd-escape
// would need to hex-escape, so this narrower rule is all it needs.
func UnitFileName(where string) string {
	return strings.ReplaceAll(strings.Trim(where, "/"), "/", "-") + ".mount"
}

// Render returns u's systemd unit file content — the body a caller
// writes through config.Generator under its own doc 01 §2 header (D4:
// config files are generated, never hand-edited).
func (u MountUnit) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[Unit]\nDescription=%s\n", u.Description)
	fmt.Fprintf(&b, "ConditionPathExists=!%s\n\n", StorageStoppedFlagPath)
	fmt.Fprintf(&b, "[Mount]\nWhat=/dev/disk/by-uuid/%s\nWhere=%s\nType=%s\n", u.UUID, u.Where, u.Filesystem)
	fmt.Fprintf(&b, "Options=defaults,nofail\n")
	return b.String()
}

// MountPlan assigns every disk in plan its standard mountpoint (doc 01
// §6: /mnt/diskN, /mnt/parityN, /mnt/cache) and builds its MountUnit.
// uuids supplies each device's already-resolved filesystem UUID
// (FilesystemUUID, read once after formatting or adopting) — MountPlan
// itself does no IO.
func MountPlan(plan TopologyPlan, uuids map[string]string) ([]MountUnit, error) {
	var units []MountUnit

	for i, d := range plan.Data {
		u, err := mountUnitFor(fmt.Sprintf("/mnt/disk%d", i+1), fmt.Sprintf("Hoserva data disk %d", i+1), d, uuids)
		if err != nil {
			return nil, err
		}
		units = append(units, u)
	}
	for i, d := range plan.Parity {
		u, err := mountUnitFor(fmt.Sprintf("/mnt/parity%d", i+1), fmt.Sprintf("Hoserva parity disk %d", i+1), d, uuids)
		if err != nil {
			return nil, err
		}
		units = append(units, u)
	}
	if plan.Cache != nil {
		u, err := mountUnitFor("/mnt/cache", "Hoserva cache disk", *plan.Cache, uuids)
		if err != nil {
			return nil, err
		}
		units = append(units, u)
	}

	return units, nil
}

func mountUnitFor(where, description string, d AssignedDisk, uuids map[string]string) (MountUnit, error) {
	uuid, ok := uuids[d.Device]
	if !ok || uuid == "" {
		return MountUnit{}, fmt.Errorf("disk: no filesystem UUID recorded for %s", d.Device)
	}
	return MountUnit{
		Where:       where,
		UUID:        uuid,
		Filesystem:  d.Filesystem,
		Description: description,
	}, nil
}

// FilesystemUUID reads dev's filesystem UUID via blkid — the identity a
// mount unit resolves through (Q21), never the transient /dev/sdX name
// a reordered controller or a swapped cable can renumber.
func FilesystemUUID(ctx context.Context, r Runner, dev string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	out, err := r.Run(ctx, "blkid", "-s", "UUID", "-o", "value", dev)
	if err != nil {
		return "", fmt.Errorf("disk: reading filesystem UUID for %s: %w", dev, err)
	}
	uuid := strings.TrimSpace(string(out))
	if uuid == "" {
		return "", fmt.Errorf("disk: %s reported no filesystem UUID", dev)
	}
	return uuid, nil
}

// MountedUUID reads the filesystem UUID currently mounted at where, via
// findmnt (the same tool DirectMounter.Mount already falls back to, to
// recognise a mount a retried apply already satisfied). A replace's own
// fix step calls this immediately before running SnapRAID, to confirm the
// slot is actually backed by the replacement it just formatted rather than
// whatever was mounted there before (doc 02 §4 "Replacing a failed disk").
func MountedUUID(ctx context.Context, r Runner, where string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	out, err := r.Run(ctx, "findmnt", "-n", "-o", "UUID", where)
	if err != nil {
		return "", fmt.Errorf("disk: reading the filesystem UUID mounted at %s: %w", where, err)
	}
	uuid := strings.TrimSpace(string(out))
	if uuid == "" {
		return "", fmt.Errorf("disk: %s reported no mounted filesystem UUID", where)
	}
	return uuid, nil
}
