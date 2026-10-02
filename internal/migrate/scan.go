package migrate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/mdg-labs/hoserva/internal/disk"
)

// ErrUnsupportedLayout is returned for a version or flash layout outside the
// allowlist when the override was not given (Q24).
var ErrUnsupportedLayout = errors.New("unsupported Unraid version or flash layout")

// appsUID is the UID Hoserva's apps user is pinned to when it is free, so
// converted Unraid templates keep their PUID=99 (Q26).
const appsUID = 99

// syncBytesPerSecond is the planning rate behind the initial sync estimate: the
// data disks are read in parallel, so this is an aggregate figure, not what one
// disk delivers. It is an assumption that sets expectations, not a measurement.
const syncBytesPerSecond = 400 * 1000 * 1000

// ScanOptions are what the user chose when starting a scan.
type ScanOptions struct {
	// UnverifiedLayout lets a version or layout outside the allowlist through,
	// recorded in the report.
	UnverifiedLayout bool
}

// Scanner runs the pre-flight checks of doc 05 §3 that this part of the scan
// owns. It reads the source, asks the disk provider for the inventory it
// already has, and writes nothing.
type Scanner struct {
	Disks disk.Provider
	// UIDOwner returns the account holding uid on this host, or "" when it is
	// free. Nil asks the system's user database.
	UIDOwner func(uid int) (string, error)
	// Now stamps the report. Nil uses the wall clock.
	Now func() time.Time
}

// LayoutProblem says why f is outside the allowlist, or "" when it is on it.
func (f *Flash) LayoutProblem() string {
	switch {
	case f.Version == "":
		return "changes.txt does not state an Unraid version"
	case !supportedVersion(f.Version):
		return fmt.Sprintf("Unraid %s is neither 6.12.x nor 7.x", f.Version)
	case !f.HasKernel:
		return "the zip has no bzimage at its root, so it is not a flash layout Hoserva knows"
	}
	return ""
}

// Inspect reads src and applies the refusals that need no disk: a source with
// no usable disk.cfg (ErrNoDiskCfg), and a version or layout outside the
// allowlist without the override (ErrUnsupportedLayout). It is what the API
// runs before queuing a scan, so a refusal never costs a job.
func Inspect(src FlashSource, opts ScanOptions) (*Flash, error) {
	f, err := ReadFlash(src)
	if err != nil {
		return nil, err
	}
	if p := f.LayoutProblem(); p != "" && !opts.UnverifiedLayout {
		return nil, fmt.Errorf("%w: %s; start the scan with the unverified-layout override to go on anyway", ErrUnsupportedLayout, p)
	}
	return f, nil
}

// InspectUpload reads an upload into memory, bounded by the size limit, and
// applies Inspect to it, for a caller that keeps nothing (the mock API).
// Service.StartScan stages the upload on disk instead.
func InspectUpload(upload io.Reader, opts ScanOptions) (*Flash, error) {
	data, err := io.ReadAll(io.LimitReader(upload, maxZipBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading the upload: %w", err)
	}
	if int64(len(data)) > maxZipBytes {
		return nil, ErrZipTooLarge
	}
	src, err := OpenZip(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	return Inspect(src, opts)
}

// member is one disk the Unraid configuration names: an array slot or a pool
// device.
type member struct {
	subject string
	role    Role
	id      string
	index   int
	unraid  int64 // size Unraid recorded, in bytes; 0 when unknown
	disk    *disk.Disk
	problem string // why disk is nil
}

// Scan runs every check against the source and the machine's disks, and returns
// the report. It returns an error only when the scan could not run: a refused
// source (Inspect) or a failure to list the disks.
func (s *Scanner) Scan(ctx context.Context, src FlashSource, opts ScanOptions) (*Report, error) {
	f, err := Inspect(src, opts)
	if err != nil {
		return nil, err
	}
	disks, err := s.Disks.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing this machine's disks: %w", err)
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	r := &Report{GeneratedAt: now().UTC(), UnraidVersion: f.Version}

	s.checkVersion(r, f, opts)
	checkCapture(r, f)
	members := buildMembers(f, disks)
	checkBoot(r, f, disks)
	checkMapping(r, f, members)
	checkIdentity(r, members)
	checkParity(r, f)
	checkParitySize(r, f, members)
	if err := s.checkSMART(ctx, r, members); err != nil {
		return nil, err
	}
	s.checkUID(r)
	checkSyncEstimate(r, f, members)
	r.conclude()
	return r, nil
}

func (s *Scanner) checkVersion(r *Report, f *Flash, opts ScanOptions) {
	if p := f.LayoutProblem(); p != "" {
		r.UnverifiedLayout = true
		r.add(CheckVersion, StatusWarn, "", "%s. The scan went ahead only because --unverified-layout was given; this override is recorded in the report (Q24).", p)
	} else {
		r.add(CheckVersion, StatusPass, "", "Unraid %s, flash layout recognised.", f.Version)
	}
	if c := f.Capture; c != nil && c.UnraidVersion != "" && f.Version != "" && c.UnraidVersion != f.Version {
		r.add(CheckVersion, StatusFlag, "", "changes.txt says Unraid %s and the capture says %s: the capture may be from another server or from before an upgrade.", f.Version, c.UnraidVersion)
	}
}

func checkCapture(r *Report, f *Flash) {
	switch {
	case f.CaptureProblem != "":
		r.add(CheckCapture, StatusWarn, "", "%s, so the boot mode is unknown.", f.CaptureProblem)
	case f.Capture == nil:
		r.add(CheckCapture, StatusWarn, "", "The zip has no capture (config/hoserva/capture.json). Run the prepare script on the Unraid server and take the Flash Backup again.")
	}
}

// buildMembers lists every disk the configuration names and matches each
// against the inventory by serial or WWN. A role is the slot's, from
// disks.ini, whatever filesystem the matched device reports.
func buildMembers(f *Flash, disks []disk.Disk) []*member {
	var out []*member
	for _, sl := range f.Slots {
		role := sl.Role()
		if (role != RoleParity && role != RoleData) || !sl.Assigned() {
			continue
		}
		out = append(out, &member{subject: sl.Name, role: role, id: sl.ID, index: sl.Index, unraid: sl.SizeKiB * 1024})
	}
	for _, p := range f.Pools {
		out = append(out, &member{subject: "pool " + p.Pool, role: RoleCache, id: p.ID})
	}

	claims := map[string][]*member{}
	for _, m := range out {
		var found []*disk.Disk
		for i := range disks {
			if idMatches(m.id, disks[i]) {
				found = append(found, &disks[i])
			}
		}
		switch len(found) {
		case 0:
			m.problem = "no disk on this machine has this serial or WWN"
		case 1:
			m.disk = found[0]
			claims[found[0].Device] = append(claims[found[0].Device], m)
		default:
			m.problem = fmt.Sprintf("%d disks on this machine match this identity, so none is taken", len(found))
		}
	}
	for _, ms := range claims {
		if len(ms) > 1 {
			for _, m := range ms {
				m.disk = nil
				m.problem = "another slot names the same disk, so the identity is ambiguous"
			}
		}
	}
	return out
}

func idMatches(id string, d disk.Disk) bool {
	id = strings.ToLower(id)
	if d.Serial != "" {
		serial := strings.ToLower(d.Serial)
		if id == serial || strings.HasSuffix(id, "_"+serial) {
			return true
		}
	}
	if d.WWN != "" {
		wwn := strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(d.WWN), "wwn-"), "0x")
		if len(wwn) >= 8 && strings.Contains(id, wwn) {
			return true
		}
	}
	return false
}

func checkBoot(r *Report, f *Flash, disks []disk.Disk) {
	if f.Capture == nil || f.Capture.Boot.Mode == "" {
		r.add(CheckBootDevice, StatusWarn, "", "The boot mode is unknown: the capture does not say whether Unraid boots from a USB stick or an internal device.")
		return
	}
	b := f.Capture.Boot
	switch b.Mode {
	case "usb":
		r.add(CheckBootDevice, StatusInfo, "", "Unraid boots from a USB stick. Keep the stick: it is the rollback.")
		return
	case "internal":
	default:
		r.add(CheckBootDevice, StatusWarn, "", "The boot mode is unknown: the capture reports %q, which is neither usb nor internal.", b.Mode)
		return
	}
	extra := ""
	if b.Mirrored {
		extra += ", mirrored"
	}
	r.add(CheckBootDevice, StatusInfo, "", "Unraid boots from an internal boot pool (%s) on %d device(s)%s. The Flash Backup zip is the only configuration source: the boot pool is ZFS, which Hoserva does not read, and a stick still attached is not read either.", b.Filesystem, len(b.Devices), extra)
	for _, bd := range b.Devices {
		here := ""
		for _, d := range disks {
			if bd.Serial != "" && strings.EqualFold(d.Serial, bd.Serial) {
				here = ", seen on this machine as " + d.Device
			}
		}
		r.add(CheckBootDevice, StatusInfo, bd.Name, "Unraid boot device (serial %s, %s, %s)%s. It is never offered a data or parity role.", bd.Serial, bd.Model, bd.Size, here)
	}
	if b.SharedWithDataPool {
		r.add(CheckBootDevice, StatusInfo, "", "The boot device also holds a data area: it is Unraid's cache, which is re-created, not adopted.")
	}
}

func bootSerials(f *Flash) map[string]bool {
	out := map[string]bool{}
	if f.Capture == nil {
		return out
	}
	for _, d := range f.Capture.Boot.Devices {
		if d.Serial != "" {
			out[strings.ToLower(d.Serial)] = true
		}
	}
	return out
}

func checkMapping(r *Report, f *Flash, members []*member) {
	switch {
	case f.DisksINIProblem != "":
		r.add(CheckMapping, StatusWarn, "", "The capture's disks.ini could not be read (%s). The serial table will come from your own step 7 table at import.", f.DisksINIProblem)
	case !f.HaveDisksINI:
		r.add(CheckMapping, StatusWarn, "", "The capture has no disks.ini, so the slot of each serial is not known. The serial table will come from your own step 7 table at import.")
	}
	for _, p := range f.PoolProblems {
		r.add(CheckMapping, StatusWarn, "", "A pool config could not be read: %s.", p)
	}
	boot := bootSerials(f)
	for _, m := range members {
		if m.disk == nil {
			r.add(CheckMapping, StatusFlag, m.subject, "Unraid had a disk with serial %s here, and %s. Attach it before the import.", m.id, m.problem)
			continue
		}
		if boot[strings.ToLower(m.disk.Serial)] && m.role != RoleCache {
			r.add(CheckMapping, StatusRefuse, m.subject, "serial %s is Unraid's boot device (%s), which is never given a data or parity role.", m.id, m.disk.Device)
			continue
		}
		r.add(CheckMapping, StatusPass, m.subject, "%s", mappingDetail(f, m))
	}
}

func mappingDetail(f *Flash, m *member) string {
	var b strings.Builder
	fmt.Fprintf(&b, "serial %s", m.id)
	switch m.role {
	case RoleParity:
		fmt.Fprintf(&b, " is Unraid parity slot %d", m.index)
	case RoleData:
		fmt.Fprintf(&b, " is Unraid disk %d", m.index)
		if fs := f.DiskCfg.FsTypes[m.index]; fs != "" {
			fmt.Fprintf(&b, " (%s)", fs)
		}
	default:
		b.WriteString(" is a pool device")
	}
	fmt.Fprintf(&b, "; this machine has it as %s, %s.", m.disk.Device, formatBytes(m.disk.Size))
	return b.String()
}

func checkIdentity(r *Report, members []*member) {
	for _, m := range members {
		if m.disk == nil {
			continue
		}
		switch {
		case m.disk.WeakIdentity && m.role == RoleParity:
			r.add(CheckIdentity, StatusRefuse, m.subject, "%s has only a weak identity (a USB enclosure hides its serial), and a parity disk must not (Q21).", m.disk.Device)
		case m.disk.WeakIdentity:
			r.add(CheckIdentity, StatusFlag, m.subject, "%s has only a weak identity (a USB enclosure hides its serial): it can be a data disk, matched by filesystem UUID and size (Q21).", m.disk.Device)
		default:
			r.add(CheckIdentity, StatusPass, m.subject, "%s has a WWN or serial.", m.disk.Device)
		}
	}
}

func checkParity(r *Report, f *Flash) {
	n, from := 0, ""
	if f.HaveDisksINI {
		for _, sl := range f.Slots {
			if sl.Role() == RoleParity && sl.Assigned() {
				n++
			}
		}
	} else {
		n, from = len(f.DiskCfg.ParitySlots), " (counted in disk.cfg, as the capture has no disks.ini)"
	}
	switch {
	case n == 0:
		r.add(CheckParity, StatusWarn, "", "No parity disk is assigned%s. The data disks can be adopted, and parity is created from nothing.", from)
	case n > 2:
		r.add(CheckParity, StatusRefuse, "", "%d parity disks%s: Hoserva supports one or two (Q19).", n, from)
	default:
		r.add(CheckParity, StatusInfo, "", "%d parity disk(s)%s. Parity is rewritten from scratch either way.", n, from)
	}
}

func (m *member) size() int64 {
	if m.disk != nil {
		return m.disk.Size
	}
	return m.unraid
}

func checkParitySize(r *Report, f *Flash, members []*member) {
	var parity, data []*member
	for _, m := range members {
		switch m.role {
		case RoleParity:
			parity = append(parity, m)
		case RoleData:
			data = append(data, m)
		}
	}
	if len(parity) == 0 || len(data) == 0 {
		return
	}
	var smallest, largest int64
	for i, m := range parity {
		if m.size() == 0 {
			r.add(CheckParitySize, StatusInfo, m.subject, "The size is not known, so the parity size was not checked.")
			return
		}
		if i == 0 || m.size() < smallest {
			smallest = m.size()
		}
	}
	for _, m := range data {
		if m.size() == 0 {
			r.add(CheckParitySize, StatusInfo, m.subject, "The size is not known, so the parity size was not checked.")
			return
		}
		largest = max(largest, m.size())
	}
	if smallest < largest {
		r.add(CheckParitySize, StatusRefuse, "", "The smallest parity disk is %s and the largest data disk is %s: SnapRAID needs parity at least as large as the largest data disk (Q20).", formatBytes(smallest), formatBytes(largest))
		return
	}
	r.add(CheckParitySize, StatusPass, "", "The smallest parity disk (%s) is at least as large as the largest data disk (%s).", formatBytes(smallest), formatBytes(largest))
}

func (s *Scanner) checkSMART(ctx context.Context, r *Report, members []*member) error {
	for _, m := range members {
		if m.disk == nil {
			continue
		}
		rep, err := s.Disks.SMART(ctx, m.disk.Device, disk.SMARTPollRespectStandby)
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		switch {
		case err != nil:
			r.add(CheckSMART, StatusWarn, m.subject, "SMART could not be read for %s: %v. It was not checked.", m.disk.Device, err)
		case rep.Skipped:
			r.add(CheckSMART, StatusWarn, m.subject, "%s is in standby, so SMART was not checked: the scan never wakes a disk.", m.disk.Device)
		case rep.ReallocatedSectors > 0 || rep.PendingSectors > 0:
			r.add(CheckSMART, StatusFlag, m.subject, "Recommend aborting: %s has %d reallocated and %d pending sectors, and the unprotected window of the migration is when a marginal disk fails.", m.disk.Device, rep.ReallocatedSectors, rep.PendingSectors)
		default:
			r.add(CheckSMART, StatusPass, m.subject, "%s has no reallocated or pending sectors.", m.disk.Device)
		}
	}
	return nil
}

func (s *Scanner) checkUID(r *Report) {
	lookup := s.UIDOwner
	if lookup == nil {
		lookup = systemUIDOwner
	}
	owner, err := lookup(appsUID)
	switch {
	case err != nil:
		r.add(CheckUID99, StatusWarn, "", "UID %d could not be checked on this host: %v.", appsUID, err)
	case owner != "":
		r.add(CheckUID99, StatusFlag, "", "UID %d is taken on this host by the account %s, so the hoserva-apps user cannot be pinned to it and Unraid templates' PUID=99 will not match it (Q26).", appsUID, owner)
	default:
		r.add(CheckUID99, StatusPass, "", "UID %d is free for the hoserva-apps user.", appsUID)
	}
}

func systemUIDOwner(uid int) (string, error) {
	u, err := user.LookupId(strconv.Itoa(uid))
	var unknown user.UnknownUserIdError
	if errors.As(err, &unknown) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return u.Username, nil
}

func checkSyncEstimate(r *Report, f *Flash, members []*member) {
	var total int64
	for _, m := range members {
		if m.role == RoleData {
			total += m.size()
		}
	}
	if total == 0 {
		r.add(CheckSyncEstimate, StatusInfo, "", "Not estimated: the size of the data disks is not known.")
		return
	}
	d := time.Duration(float64(total) / syncBytesPerSecond * float64(time.Second))
	r.add(CheckSyncEstimate, StatusInfo, "", "About %.1f hours for %s of data disks, if they are full, at an assumed %d MB/s. It is a planning figure, not a measurement: the first sync is a long job, and it runs only when you start it.", d.Hours(), formatBytes(total), syncBytesPerSecond/1000/1000)
}

func formatBytes(n int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	v, i := float64(n), 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}
