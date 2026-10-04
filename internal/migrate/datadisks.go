package migrate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/mdg-labs/hoserva/internal/disk"
)

// ErrDiskRelease is returned when a source disk's read-only mount could not be
// released. The scan fails: nothing may stay mounted when it ends.
var ErrDiskRelease = errors.New("a source disk could not be unmounted")

// fsVerdict is what the flash and the device together say a data disk is.
type fsVerdict struct {
	fs disk.FilesystemType
	// refusal is why the disk is not adopted, empty when it is.
	refusal string
	code    RefusalCode
}

// classifyFilesystem decides whether a data disk's filesystem is one Hoserva
// adopts. flash is diskFsType.N from config/disk.cfg, falling back to the
// disks.ini fsType (slot); udev is the device's own ID_FS_TYPE. The flash gives
// the name Unraid knows the disk by, including the luks: prefix (Q22); the
// device gives what is on it. A disagreement is a refusal, never a guess.
func classifyFilesystem(flash, slot, udev string) fsVerdict {
	named := strings.ToLower(strings.TrimSpace(flash))
	if named == "" || named == "auto" {
		named = strings.ToLower(strings.TrimSpace(slot))
	}
	if named == "auto" {
		named = ""
	}
	encrypted := strings.HasPrefix(named, "luks:") || strings.HasPrefix(named, "luks-")
	inner := strings.TrimPrefix(strings.TrimPrefix(named, "luks:"), "luks-")
	switch {
	case encrypted || udev == "crypto_LUKS":
		return fsVerdict{code: RefuseEncrypted, refusal: fmt.Sprintf("it is encrypted (LUKS; the flash says %s, the device says %s). Hoserva does not adopt encrypted disks: the recovery risk is too high (Q22). Decrypt it in Unraid first, or leave it out of the migration", orNone(named), orNone(udev))}
	case named == "zfs" || strings.HasPrefix(named, "zfs") || udev == "zfs_member":
		return fsVerdict{code: RefuseZFS, refusal: fmt.Sprintf("it is a ZFS disk (the flash says %s, the device says %s). Hoserva cannot read ZFS without OpenZFS and does not adopt it (Q23)", orNone(named), orNone(udev))}
	}
	want, known := readableFS(inner)
	if named != "" && !known {
		return fsVerdict{code: RefuseUnsupportedFS, refusal: fmt.Sprintf("the flash names the filesystem %q, which Hoserva does not adopt (xfs, ext4 or single-device btrfs, Q23)", named)}
	}
	have, onDevice := readableFS(udev)
	switch {
	case udev == "":
		return fsVerdict{code: RefuseNoFilesystem, refusal: "the device reports no filesystem, so there is nothing to adopt"}
	case !onDevice:
		return fsVerdict{code: RefuseUnsupportedFS, refusal: fmt.Sprintf("the device holds %q, which Hoserva does not adopt (xfs, ext4 or single-device btrfs, Q23)", udev)}
	case named != "" && want != have:
		return fsVerdict{code: RefuseFilesystemClash, refusal: fmt.Sprintf("the flash says the disk is %s and the device says it is %s; Hoserva does not guess which is right", want, have)}
	}
	return fsVerdict{fs: have}
}

func orNone(s string) string {
	if s == "" {
		return "nothing"
	}
	return s
}

var (
	btrfsNumDevices = regexp.MustCompile(`(?m)^num_devices\s+(\d+)\b`)
	btrfsLogRoot    = regexp.MustCompile(`(?m)^log_root\s+(\d+)\b`)
)

// cleanStopAdvice follows a refusal for a log that was not closed cleanly.
const cleanStopAdvice = " The log was not unmounted cleanly: start Unraid, stop the array cleanly, and scan again. Hoserva never replays a log on a disk it does not own yet."

type btrfsSuperblock struct {
	devices int
	logRoot uint64
}

// btrfsSuper reads dev's own superblock through `btrfs inspect-internal
// dump-super`, which writes nothing: how many devices the filesystem spans, and
// whether a tree log is pending. It reads only dev: `btrfs filesystem show`
// scans every block device on the machine for the others, and would read disks
// this check has no business with.
func btrfsSuper(ctx context.Context, r disk.Runner, dev string) (btrfsSuperblock, error) {
	out, err := r.Run(ctx, "btrfs", "inspect-internal", "dump-super", dev)
	if err != nil {
		return btrfsSuperblock{}, err
	}
	var sb btrfsSuperblock
	m := btrfsNumDevices.FindSubmatch(out)
	if m == nil {
		return sb, errors.New("btrfs inspect-internal dump-super did not say how many devices it has")
	}
	if sb.devices, err = strconv.Atoi(string(m[1])); err != nil {
		return sb, err
	}
	m = btrfsLogRoot.FindSubmatch(out)
	if m == nil {
		return sb, errors.New("btrfs inspect-internal dump-super did not say whether it has a tree log")
	}
	if sb.logRoot, err = strconv.ParseUint(string(m[1]), 10, 64); err != nil {
		return sb, err
	}
	return sb, nil
}

// logReason is why a read-only mount that does not replay the log would show
// less than the disk holds, empty when no tree log is pending.
func (sb btrfsSuperblock) logReason() string {
	if sb.logRoot == 0 {
		return ""
	}
	return fmt.Sprintf("its btrfs superblock records a tree log that was never replayed (log_root %d), so a mount that does not replay it would miss files the disk holds", sb.logRoot)
}

// ext4Recovery reports whether dev's ext4 journal holds changes not yet applied
// (needs_recovery), from the superblock through `dumpe2fs -h`, which writes
// nothing. e2fsck -n exits 0 on such a filesystem.
func ext4Recovery(ctx context.Context, r disk.Runner, dev string) (bool, error) {
	out, err := r.Run(ctx, "dumpe2fs", "-h", dev)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if feats, ok := strings.CutPrefix(line, "Filesystem features:"); ok {
			for _, f := range strings.Fields(feats) {
				if f == "needs_recovery" {
					return true, nil
				}
			}
			return false, nil
		}
	}
	return false, errors.New("dumpe2fs -h did not list the filesystem features")
}

// pendingLog says why dev, an ext4 or btrfs filesystem, cannot be read by a
// mount that does not replay its log, empty when it can. An error means the
// log could not be inspected, which is never read as clean.
func pendingLog(ctx context.Context, r disk.Runner, dev string, fs disk.FilesystemType) (string, error) {
	switch fs {
	case disk.EXT4:
		pending, err := ext4Recovery(ctx, r, dev)
		if err != nil {
			return "", err
		}
		if pending {
			return "its ext4 journal needs recovery (needs_recovery), so a mount that does not replay it would miss files the disk holds", nil
		}
		return "", nil
	case disk.BTRFS:
		sb, err := btrfsSuper(ctx, r, dev)
		if err != nil {
			return "", err
		}
		return sb.logReason(), nil
	}
	return "", nil
}

// maxCheckDetail bounds how much of a check tool's own message the report
// quotes.
const maxCheckDetail = 400

func briefly(err error) string {
	s := strings.Join(strings.Fields(err.Error()), " ")
	if utf8.RuneCountInString(s) > maxCheckDetail {
		s = string([]rune(s)[:maxCheckDetail]) + "..."
	}
	return s
}

// slotName names a slot in the baseline and in scratch file names, built from its
// number so no name in the source reaches a path.
func slotName(m *member) string { return "disk" + strconv.Itoa(m.index) }

// refuseDisk marks m as not adopted and says so in the report.
func (r *Report) refuseDisk(m *member, check string, code RefusalCode, format string, args ...any) {
	m.refusal = fmt.Sprintf(format, args...)
	r.refuse(m, check, code, "%s is not adopted: %s", m.subject, m.refusal)
}

// refuse adds a refuse row for m's slot and keeps the first refusal of m for
// the review's table, with the row's own text.
func (r *Report) refuse(m *member, check string, code RefusalCode, format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	r.add(check, StatusRefuse, m.subject, "%s", text)
	if m.refusalCode == "" {
		m.refusalCode, m.refusalText = code, text
	}
}

// checkDataDisks runs the checks of doc 05 §3 that read the data disks: the
// filesystem of each, its read-only integrity check, and the baseline verify
// compares against. Only slots disks.ini records as data are looked at: a
// parity slot is never mounted or checked, whatever its device reports. A disk
// that fails a check is refused by name and the scan goes on with the rest.
// Nothing is written to any disk. It returns an error only when the scan
// cannot go on: it was cancelled, or a mount could not be released.
func (s *Scanner) checkDataDisks(ctx context.Context, r *Report, f *Flash, members []*member, all []disk.Disk, opts ScanOptions) error {
	if !f.HaveDisksINI {
		r.add(CheckDataDisks, StatusInfo, "", "The capture has no usable disks.ini, so no disk is known to be a data disk: no filesystem is checked and no baseline is recorded until you assign the roles at import.")
		return nil
	}
	for _, m := range members {
		if m.role == RoleParity && m.disk != nil {
			note := ""
			if m.disk.Filesystem != "" {
				note = fmt.Sprintf(" Its device reports %s: a parity disk's bytes are an XOR of the data disks', which can leave a valid-looking superblock.", m.disk.Filesystem)
			}
			r.add(CheckDataDisks, StatusInfo, m.subject, "A parity slot: %s is never mounted or checked, and its role comes from its slot, never from its filesystem.%s", m.disk.Device, note)
		}
	}
	var data []*member
	for _, m := range members {
		if m.role == RoleData && m.disk != nil {
			data = append(data, m)
		}
	}
	if len(data) == 0 {
		return nil
	}
	if s.Runner == nil || s.Mounter == nil || s.Dir == "" {
		r.add(CheckDataDisks, StatusWarn, "", "This daemon is not set up to read the data disks, so their filesystems were not checked and no baseline was recorded. A scan from a running hoservad does both.")
		return nil
	}
	sort.SliceStable(data, func(i, j int) bool { return data[i].index < data[j].index })

	uuids := map[string][]string{}
	for _, d := range all {
		if d.FSUUID != "" {
			k := strings.ToLower(d.FSUUID)
			uuids[k] = append(uuids[k], d.Device)
		}
	}

	boot := bootSerials(f)
	var candidates []*member
	for _, m := range data {
		d := m.disk
		switch {
		case boot[strings.ToLower(d.Serial)]:
			r.refuseDisk(m, CheckBootDevice, RefuseBootDevice, "serial %s is the boot device the capture names, which is never given a data role", d.Serial)
			continue
		case d.Boot:
			r.refuseDisk(m, CheckDataDisks, RefuseHostBoot, "%s is the disk this machine boots from; it can never be an array disk", d.Device)
			continue
		case d.UnraidBoot || disk.IsUnraidStick(*d):
			r.refuseDisk(m, CheckBootDevice, RefuseBootDevice, "%s is an Unraid boot device, which is never given a data role", d.Device)
			continue
		case d.Failed:
			r.refuseDisk(m, CheckDataDisks, RefuseFailed, "%s is reported failed", d.Device)
			continue
		}
		v := classifyFilesystem(s.flashFs(f, m), m.slotFs, d.Filesystem)
		if v.refusal != "" {
			r.refuseDisk(m, CheckDataDisks, v.code, "%s", v.refusal)
			continue
		}
		if !strings.HasPrefix(d.FSDevice, "/dev/") || d.FSUUID == "" {
			r.refuseDisk(m, CheckDataDisks, RefuseNoFilesystemNode, "udev reports no filesystem node or UUID for %s, and Hoserva mounts by filesystem UUID (Q21)", d.Device)
			continue
		}
		m.fs = v.fs
		candidates = append(candidates, m)
	}

	// Two data disks with one filesystem UUID cannot both be adopted: an XFS
	// filesystem is not mounted twice under one UUID, and the array's records
	// are keyed by it. A data disk whose UUID another disk that is not adopted
	// also carries (a former parity disk holds a byte copy of a one-data-disk
	// array's filesystem) is adopted through its own device, a by-id link, and
	// never found by its UUID, so the copy cannot be mounted in its place; with
	// no such link nothing but the UUID could tell the two apart, and it is
	// refused.
	dataDevices := map[string]bool{}
	for _, m := range candidates {
		dataDevices[m.disk.Device] = true
	}
	var unique []*member
	for _, m := range candidates {
		var sharedData, sharedOther []string
		for _, dev := range uuids[strings.ToLower(m.disk.FSUUID)] {
			switch {
			case dev == m.disk.Device:
			case dataDevices[dev]:
				sharedData = append(sharedData, dev)
			default:
				sharedOther = append(sharedOther, dev)
			}
		}
		switch {
		case len(sharedData) > 0:
			sort.Strings(sharedData)
			r.refuseDisk(m, CheckDataDisks, RefuseDuplicateUUID, "its filesystem UUID is also on %s, another data disk, and two data disks with one UUID cannot both be adopted (Unraid mounts XFS with nouuid, so this can happen there)", joinNames(sharedData))
			continue
		case len(sharedOther) > 0 && m.disk.FSByIDName == "":
			sort.Strings(sharedOther)
			r.refuseDisk(m, CheckDataDisks, RefuseDuplicateUUID, "its filesystem UUID is also on %s, and it has no /dev/disk/by-id link to be mounted through instead of by UUID", joinNames(sharedOther))
			continue
		case len(sharedOther) > 0:
			sort.Strings(sharedOther)
			r.add(CheckDataDisks, StatusInfo, m.subject, "Its filesystem UUID is also on %s, which is not adopted (the parity disk of a one-data-disk array holds a copy of it): %s is mounted through its own /dev/disk/by-id link, never by UUID.", joinNames(sharedOther), m.disk.Device)
		}
		unique = append(unique, m)
	}

	var clean []*member
	for _, m := range unique {
		if m.fs == disk.BTRFS {
			sb, err := btrfsSuper(ctx, s.Runner, m.disk.FSDevice)
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			switch {
			case err != nil:
				r.refuseDisk(m, CheckDataDisks, RefuseUnverifiedFS, "it could not be shown to be a single-device btrfs filesystem with a clean log (%s)", briefly(err))
				continue
			case sb.devices != 1:
				r.refuseDisk(m, CheckDataDisks, RefuseMultiDeviceBtrfs, "it is one of %d devices of a btrfs filesystem, which is not a self-contained filesystem per disk (Q23)", sb.devices)
				continue
			}
			if reason := sb.logReason(); reason != "" {
				r.refuseDisk(m, CheckIntegrity, RefusePendingLog, "%s.%s", reason, cleanStopAdvice)
				continue
			}
		}
		if m.fs == disk.EXT4 {
			reason, err := pendingLog(ctx, s.Runner, m.disk.FSDevice, m.fs)
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			switch {
			case err != nil:
				r.refuseDisk(m, CheckIntegrity, RefuseUnverifiedFS, "its ext4 journal could not be shown to be clean (%s)", briefly(err))
				continue
			case reason != "":
				r.refuseDisk(m, CheckIntegrity, RefusePendingLog, "%s.%s", reason, cleanStopAdvice)
				continue
			}
		}
		err := disk.AdoptCheck(ctx, s.Runner, m.disk.FSDevice, m.fs)
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err != nil {
			cause := errors.Unwrap(err)
			if cause == nil {
				cause = err
			}
			hint := ""
			if m.fs == disk.XFS && strings.Contains(cause.Error(), "replay") {
				hint = cleanStopAdvice
			}
			r.refuseDisk(m, CheckIntegrity, RefuseIntegrity, "its read-only %s check failed (%s). Computing parity over a damaged filesystem would keep the damage.%s", m.fs, briefly(cause), hint)
			continue
		}
		agree := "the flash and the device agree on it"
		if strings.TrimSpace(s.flashFs(f, m)) == "" || strings.EqualFold(strings.TrimSpace(s.flashFs(f, m)), "auto") {
			agree = "the flash does not name it, so the device's own signature is what is used"
		}
		r.add(CheckDataDisks, StatusPass, m.subject, "%s on %s, a single filesystem; %s.", m.fs, m.disk.Device, agree)
		r.add(CheckIntegrity, StatusPass, m.subject, "The read-only %s check of %s is clean.", m.fs, m.disk.Device)
		clean = append(clean, m)
	}
	return s.recordBaseline(ctx, r, clean, opts)
}

// flashFs is the filesystem type the flash gives m's slot.
func (s *Scanner) flashFs(f *Flash, m *member) string {
	return f.DiskCfg.FsTypes[m.index]
}

// newToken returns a random name part.
func newToken() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// freeBytes is what is free to an unprivileged writer on the filesystem at path.
func freeBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

func (s *Scanner) free(path string) (int64, error) {
	if s.FreeSpace != nil {
		return s.FreeSpace(path)
	}
	return freeBytes(path)
}
