package migrate

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// The Unraid USB stick is a FAT filesystem labelled UNRAID (doc 05 §3). It is
// the user's rollback (doc 05 §4 step 11, §5), so it is only ever mounted
// read-only, at a private mountpoint, for as long as one scan reads it.
const (
	stickLabel      = "UNRAID"
	stickFilesystem = "vfat"
	// stickDir is the stick's mountpoint under Service.Dir (0700), so no other
	// user of the machine can walk the configuration it holds.
	stickDir = "stick"
	// devicePrefix marks a scan or a source that is a flash device, not an
	// uploaded zip: the scan record's File is devicePrefix + the device path.
	devicePrefix = "device:"

	stickUnmountTimeout = 30 * time.Second
	bootModeInternal    = "internal"
)

var (
	// ErrNotFlashDevice is returned for a device that is not an Unraid flash
	// device on offer: not a FAT filesystem labelled UNRAID, or the boot disk, or
	// an array disk, or one whose filesystem UUID is not unique on this machine.
	ErrNotFlashDevice = errors.New("not an Unraid flash device this machine offers")
	// ErrZipOnly is returned for a stick scan when Unraid booted from an
	// internal device: its boot pool is ZFS, which Hoserva does not read, and a
	// stick still attached is a copy Unraid no longer writes (Q25).
	ErrZipOnly = errors.New("the Unraid server booted from an internal device, so the Flash Backup zip is the only source")
	// ErrFlashDevice is wrapped by a failure to read the flash device: it could
	// not be mounted read-only, was pulled while it was read, or could not be
	// unmounted.
	ErrFlashDevice = errors.New("the flash device could not be read")
	// ErrNoDeviceSource is returned when this daemon has no way to mount a flash
	// device.
	ErrNoDeviceSource = errors.New("this daemon cannot read a flash device")
)

// FlashDevice is a disk offered as the Unraid flash.
type FlashDevice struct {
	Device string
	Size   int64
	Model  string
	Serial string
	FSUUID string
}

// FlashOffer is what the session offers as a configuration source besides the
// zip: the devices a scan can read, or ZipOnly when none may be.
type FlashOffer struct {
	Devices []FlashDevice
	ZipOnly bool
}

// IsDevice reports whether the source is a flash device, and which.
func (s SourceInfo) IsDevice() (device string, ok bool) {
	return strings.CutPrefix(s.File, devicePrefix)
}

func isDeviceScan(file string) bool { return strings.HasPrefix(file, devicePrefix) }

// stickCandidates is every disk that may be read as the Unraid flash: a FAT
// filesystem labelled UNRAID that udev reports, on a disk that is neither the
// boot disk nor in the array, whose UUID no other disk shares. A UUID shared
// with another disk makes `mount -U` ambiguous, and so is refused.
func (s *Service) stickCandidates(ctx context.Context) ([]FlashDevice, error) {
	disks, err := s.Scanner.Disks.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing this machine's disks: %w", err)
	}
	inArray := map[string]struct{}{}
	if s.ArrayDevices != nil {
		if inArray, err = s.ArrayDevices(ctx); err != nil {
			return nil, fmt.Errorf("reading the array's disks: %w", err)
		}
	}
	uuids := map[string]int{}
	for _, d := range disks {
		if d.FSUUID != "" {
			uuids[strings.ToUpper(d.FSUUID)]++
		}
	}
	var out []FlashDevice
	for _, d := range disks {
		if _, member := inArray[d.Device]; member || d.Boot || d.Failed {
			continue
		}
		if d.Filesystem != stickFilesystem || !strings.EqualFold(d.Label, stickLabel) {
			continue
		}
		if d.FSUUID == "" || uuids[strings.ToUpper(d.FSUUID)] != 1 {
			continue
		}
		out = append(out, FlashDevice{Device: d.Device, Size: d.Size, Model: d.Model, Serial: d.Serial, FSUUID: d.FSUUID})
	}
	return out, nil
}

// zipOnly reports whether the session's report was made from a capture that says
// Unraid booted from an internal device.
func (s *Service) zipOnly(sess *session) bool {
	return sess.Report != nil && sess.Report.BootMode == bootModeInternal
}

// FlashOffer says which flash devices a scan may read now. It lists the
// devices udev already knows; it opens none and wakes none.
func (s *Service) FlashOffer(ctx context.Context) (*FlashOffer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Mounter == nil {
		return &FlashOffer{}, nil
	}
	sess, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	if s.zipOnly(sess) {
		return &FlashOffer{ZipOnly: true}, nil
	}
	devices, err := s.stickCandidates(ctx)
	if err != nil {
		return nil, err
	}
	return &FlashOffer{Devices: devices}, nil
}

// flashDevice returns the offered device named by device, or ErrNotFlashDevice.
func (s *Service) flashDevice(ctx context.Context, device string) (FlashDevice, error) {
	candidates, err := s.stickCandidates(ctx)
	if err != nil {
		return FlashDevice{}, err
	}
	for _, c := range candidates {
		if c.Device == device {
			return c, nil
		}
	}
	return FlashDevice{}, fmt.Errorf("%w: %s is not a FAT filesystem labelled %s on a disk outside the array", ErrNotFlashDevice, device, stickLabel)
}

// withStick mounts dev read-only at its private mountpoint, runs fn on the
// source it holds and unmounts again, whatever fn returned. An error is
// ErrFlashDevice when the device could not be read: it did not mount read-only,
// a read failed (the stick pulled), or it could not be unmounted. The scan's
// result is not used when the stick could not be released.
func (s *Service) withStick(ctx context.Context, dev FlashDevice, fn func(FlashSource) error) (err error) {
	if s.Mounter == nil {
		return ErrNoDeviceSource
	}
	s.stickMu.Lock()
	defer s.stickMu.Unlock()
	where := s.path(stickDir)
	if err := s.releaseStick(ctx); err != nil {
		return err
	}
	if err := s.Mounter.MountReadOnly(ctx, stickFilesystem, dev.FSUUID, where); err != nil {
		return fmt.Errorf("%w: %w", ErrFlashDevice, err)
	}
	defer func() {
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stickUnmountTimeout)
		defer cancel()
		if uerr := s.Mounter.Unmount(uctx, where); uerr != nil {
			err = errors.Join(err, fmt.Errorf("%w: unmounting %s: %w", ErrFlashDevice, where, uerr))
		}
	}()
	src, err := OpenDir(where)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrFlashDevice, err)
	}
	defer func() { _ = src.Close() }()
	err = fn(src)
	if rerr := src.Err(); rerr != nil {
		err = errors.Join(err, fmt.Errorf("%w: %w", ErrFlashDevice, rerr))
	}
	return err
}

// releaseStick unmounts the private mountpoint when it is mounted: the mount a
// killed process left. It fails when the mount cannot be released, so nothing
// is mounted over or beside a mount that is still there.
func (s *Service) releaseStick(ctx context.Context) error {
	where := s.path(stickDir)
	mounted, err := s.Mounter.IsMounted(ctx, where)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrFlashDevice, err)
	}
	if !mounted {
		return nil
	}
	if err := s.Mounter.Unmount(ctx, where); err != nil {
		return fmt.Errorf("%w: a previous mount of the flash device is still at %s: %w", ErrFlashDevice, where, err)
	}
	return nil
}

// recoverStick runs at start: a flash device mounted at the private mountpoint
// is a mount of an earlier process, and is released. A mount that stays does not
// stop the zip, only the next stick scan, which refuses until it is gone.
func (s *Service) recoverStick(ctx context.Context) {
	if s.Mounter == nil {
		return
	}
	s.stickMu.Lock()
	defer s.stickMu.Unlock()
	if err := s.releaseStick(ctx); err != nil {
		log.Printf("migrate: %v", err)
	}
}

// StartDeviceScan is StartScan for the Unraid flash device: it refuses a device
// that is not on offer, and a source that says Unraid booted internally, reads
// the device once to refuse what Inspect refuses, records the scan in the
// session and calls submit to queue the job. The device is mounted read-only
// for that read and for the job's, and is unmounted after each. A refusal, or a
// submit that fails, leaves the session as it was.
func (s *Service) StartDeviceScan(ctx context.Context, device string, opts ScanOptions, submit func(ctx context.Context, scan string) (jobID string, err error)) error {
	if s.Mounter == nil {
		return ErrNoDeviceSource
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureDir(); err != nil {
		return err
	}
	sess, err := s.load(ctx)
	if err != nil {
		return err
	}
	if running, _, err := s.scanOutcome(ctx, sess.Scan); err != nil {
		return err
	} else if running {
		return ErrScanInProgress
	}
	if s.zipOnly(sess) {
		return ErrZipOnly
	}
	dev, err := s.flashDevice(ctx, device)
	if err != nil {
		return err
	}
	if err := s.withStick(ctx, dev, func(src FlashSource) error {
		f, err := Inspect(src, opts)
		if err != nil {
			return err
		}
		if f.Capture != nil && f.Capture.Boot.Mode == bootModeInternal {
			return ErrZipOnly
		}
		return nil
	}); err != nil {
		return err
	}
	rec := &scanRecord{File: devicePrefix + dev.Device, ReceivedAt: time.Now().UTC(), UnverifiedLayout: opts.UnverifiedLayout}
	return s.queue(ctx, sess, rec, submit)
}

// scanDevice is the migration_scan job for a scan record naming a flash device.
// The device is looked up again: a name that was a stick when the scan was
// queued may be another disk now.
func (s *Service) scanDevice(ctx context.Context, rec scanRecord) (*Report, error) {
	device, _ := strings.CutPrefix(rec.File, devicePrefix)
	dev, err := s.flashDevice(ctx, device)
	if err != nil {
		return nil, err
	}
	var report *Report
	err = s.withStick(ctx, dev, func(src FlashSource) error {
		var err error
		report, err = s.Scanner.Scan(ctx, src, ScanOptions{UnverifiedLayout: rec.UnverifiedLayout})
		return err
	})
	if err != nil {
		return nil, err
	}
	if report.BootMode == bootModeInternal {
		return nil, ErrZipOnly
	}
	return report, nil
}
