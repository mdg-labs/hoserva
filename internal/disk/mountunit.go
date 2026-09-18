package disk

import (
	"context"
	"fmt"
	"strings"
)

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
	fmt.Fprintf(&b, "[Unit]\nDescription=%s\n\n", u.Description)
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
