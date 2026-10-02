package migrate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/disk"
)

const (
	stickDevice = "/dev/sdz"
	stickUUID   = "ABCD-1234"
	primary     = "unraid-6.12-xfs-single-parity"
)

var fixedNow = func() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) }

// writeTree lays files out under where, as a mounted flash holds them.
func writeTree(where string, files map[string][]byte) error {
	for name, data := range files {
		p := filepath.Join(where, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

type stickRig struct {
	svc     *Service
	disks   *disk.FakeProvider
	mounter *disk.FakeReadOnlyMounter
	files   map[string][]byte
	spec    fixtureSpec
}

// newStickRig is a Service over a variant's disks and an UNRAID-labelled FAT
// stick at stickDevice, whose "mount" puts the variant's flash tree in the
// mountpoint.
func newStickRig(t *testing.T, variant string) *stickRig {
	t.Helper()
	svc, spec, files := newService(t, variant)
	disks := fixtureDisks(spec)
	disks.AddDisk(stickDevice, disk.Disk{Size: 16 << 30, Model: "Flash Drive", Filesystem: "vfat", Label: "UNRAID", FSUUID: stickUUID})
	svc.Scanner = &Scanner{Disks: disks, UIDOwner: noUID, Now: fixedNow}
	r := &stickRig{svc: svc, disks: disks, mounter: disk.NewFakeReadOnlyMounter(), files: files, spec: spec}
	r.mounter.OnMount = func(where string) error { return writeTree(where, r.files) }
	svc.Mounter = r.mounter
	return r
}

func (r *stickRig) mountpoint() string { return filepath.Join(r.svc.Dir, "stick") }

func (r *stickRig) assertReleased(t *testing.T) {
	t.Helper()
	if got := r.mounter.MountedPaths(); len(got) != 0 {
		t.Fatalf("the stick was left mounted at %v", got)
	}
}

func (r *stickRig) startDeviceScan(device string, opts ScanOptions) (string, error) {
	var scan string
	err := r.svc.StartDeviceScan(ctx0, device, opts, func(_ context.Context, got string) (string, error) {
		scan = got
		return "job-1", nil
	})
	return scan, err
}

func (r *stickRig) scanDeviceNow(t *testing.T, opts ScanOptions) error {
	t.Helper()
	scan, err := r.startDeviceScan(stickDevice, opts)
	if err != nil {
		return err
	}
	return r.svc.RunScan(ctx0, io.Discard, scan)
}

func TestFlashOffer_OnlyAFATDiskLabelledUNRAIDOutsideTheArray(t *testing.T) {
	r := newStickRig(t, primary)
	r.disks.AddDisk("/dev/sdy", disk.Disk{Filesystem: "vfat", Label: "unraid", FSUUID: "1111-2222"})
	r.disks.AddDisk("/dev/sdx", disk.Disk{Filesystem: "vfat", Label: "MEDIA", FSUUID: "3333-4444"})
	r.disks.AddDisk("/dev/sdw", disk.Disk{Filesystem: "xfs", Label: "UNRAID", FSUUID: "5555"})
	r.disks.AddDisk("/dev/sdv", disk.Disk{Filesystem: "vfat", Label: "UNRAID"})
	r.disks.AddDisk("/dev/sdu", disk.Disk{Filesystem: "vfat", Label: "UNRAID", FSUUID: "6666-7777", Boot: true})
	r.disks.AddDisk("/dev/sdt", disk.Disk{Filesystem: "vfat", Label: "UNRAID", FSUUID: "8888-9999"})
	r.disks.AddDisk("/dev/sds", disk.Disk{Filesystem: "vfat", Label: "UNRAID", FSUUID: "AAAA-0001"})
	r.disks.AddDisk("/dev/sdr", disk.Disk{Filesystem: "vfat", Label: "UNRAID", FSUUID: "aaaa-0001"})
	r.svc.ArrayDevices = func(context.Context) (map[string]struct{}, error) {
		return map[string]struct{}{"/dev/sdt": {}}, nil
	}

	offer, err := r.svc.FlashOffer(ctx0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range offer.Devices {
		got = append(got, d.Device)
	}
	if want := []string{"/dev/sdy", stickDevice}; !reflect.DeepEqual(got, want) {
		t.Fatalf("offered %v, want %v: not the wrong filesystem or label, no UUID, the boot disk, an array disk, or two disks sharing a UUID", got, want)
	}
	if offer.ZipOnly {
		t.Fatal("ZipOnly with no report")
	}
	if len(r.mounter.Mounts) != 0 {
		t.Fatalf("listing the offer mounted %v", r.mounter.Mounts)
	}
}

func TestFlashOffer_NothingWithoutAMounterOrWhenTheArrayLookupFails(t *testing.T) {
	r := newStickRig(t, primary)
	r.svc.Mounter = nil
	if offer, err := r.svc.FlashOffer(ctx0); err != nil || len(offer.Devices) != 0 || offer.ZipOnly {
		t.Fatalf("FlashOffer without a mounter = %+v, %v", offer, err)
	}
	if _, err := r.startDeviceScan(stickDevice, ScanOptions{}); !errors.Is(err, ErrNoDeviceSource) {
		t.Fatalf("StartDeviceScan without a mounter = %v, want %v", err, ErrNoDeviceSource)
	}

	r.svc.Mounter = r.mounter
	boom := errors.New("array store down")
	r.svc.ArrayDevices = func(context.Context) (map[string]struct{}, error) { return nil, boom }
	if _, err := r.svc.FlashOffer(ctx0); !errors.Is(err, boom) {
		t.Fatalf("FlashOffer = %v, want the array lookup's error: not knowing the array's disks means no device is offered", err)
	}
	if _, err := r.startDeviceScan(stickDevice, ScanOptions{}); !errors.Is(err, boom) {
		t.Fatalf("StartDeviceScan = %v, want the array lookup's error", err)
	}
	if len(r.mounter.Mounts) != 0 {
		t.Fatalf("mounted %v while the array's disks were unknown", r.mounter.Mounts)
	}
}

// The scan result from the stick is the result from the same variant's zip.
func TestStartDeviceScan_ReportEqualsTheZipsReport(t *testing.T) {
	r := newStickRig(t, primary)
	if err := r.scanDeviceNow(t, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	fromStick, err := r.svc.State(ctx0)
	if err != nil || fromStick.Phase != PhaseScanned || fromStick.Report == nil {
		t.Fatalf("State = %+v, %v", fromStick, err)
	}

	z := newStickRig(t, primary)
	if err := scanNow(z.svc, zipOf(t, z.files, false), ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	fromZip, err := z.svc.State(ctx0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromStick.Report, fromZip.Report) {
		t.Fatalf("the stick's report differs from the zip's:\nstick %+v\nzip   %+v", fromStick.Report, fromZip.Report)
	}
	r.assertReleased(t)
}

func TestStartDeviceScan_MountsReadOnlyByUUIDAtThePrivateMountpointAndReleasesIt(t *testing.T) {
	r := newStickRig(t, primary)
	if err := r.scanDeviceNow(t, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(r.mounter.Mounts) != 2 {
		t.Fatalf("mounted %d times, want once to inspect when queuing and once for the job: %v", len(r.mounter.Mounts), r.mounter.Mounts)
	}
	for _, m := range r.mounter.Mounts {
		if m.FSType != "vfat" || m.UUID != stickUUID || m.Where != r.mountpoint() {
			t.Errorf("mount = %+v, want vfat %s at %s", m, stickUUID, r.mountpoint())
		}
	}
	if len(r.mounter.Unmounts) != 2 {
		t.Errorf("unmounted %d times, want 2", len(r.mounter.Unmounts))
	}
	r.assertReleased(t)
	if info, err := os.Stat(r.svc.Dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("the mountpoint's parent = %v, %v; want 0700", info, err)
	}
}

func TestStartDeviceScan_RecordsTheDeviceAsTheSourceAndForgetKeepsNothing(t *testing.T) {
	r := newStickRig(t, primary)
	if err := r.scanDeviceNow(t, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	st, err := r.svc.State(ctx0)
	if err != nil || st.Source == nil {
		t.Fatalf("State = %+v, %v", st, err)
	}
	if dev, ok := st.Source.IsDevice(); !ok || dev != stickDevice || st.Source.Size != 0 {
		t.Fatalf("Source = %+v, want the device %s with no zip size", st.Source, stickDevice)
	}
	for _, n := range dirEntries(t, r.svc.Dir) {
		if strings.HasPrefix(n, "upload-") {
			t.Errorf("a stick scan staged %s", n)
		}
	}

	// A restart keeps a device source: it never named a file.
	if err := r.svc.Recover(ctx0); err != nil {
		t.Fatal(err)
	}
	if st, _ := r.svc.State(ctx0); st.Source == nil || st.Phase != PhaseScanned {
		t.Fatalf("after Recover State = %+v, want the device source kept", st)
	}

	if err := r.svc.Forget(ctx0); err != nil {
		t.Fatal(err)
	}
	if st, _ := r.svc.State(ctx0); st.Phase != PhaseNone || st.Source != nil || st.Report != nil {
		t.Fatalf("after Forget State = %+v", st)
	}
	r.assertReleased(t)
}

// Nothing is mounted for a device that is not on offer, and a device name is
// never a path a client chose to mount.
func TestStartDeviceScan_RefusesADeviceThatIsNotOnOfferWithoutMounting(t *testing.T) {
	r := newStickRig(t, primary)
	for _, dev := range []string{"/dev/sda", "/dev/sdb", "", "/dev/sdz; touch /tmp/x", "/dev/../dev/sdz", "sdz", "device:/dev/sdz"} {
		_, err := r.startDeviceScan(dev, ScanOptions{})
		if !errors.Is(err, ErrNotFlashDevice) {
			t.Errorf("StartDeviceScan(%q) = %v, want %v", dev, err, ErrNotFlashDevice)
		}
	}
	if len(r.mounter.Mounts) != 0 {
		t.Fatalf("mounted %v for a device that is not on offer", r.mounter.Mounts)
	}
	if st, _ := r.svc.State(ctx0); st.Phase != PhaseNone {
		t.Fatalf("a refused scan left phase %s", st.Phase)
	}
}

func TestStartDeviceScan_RefusesWhatInspectRefusesAndReleasesTheStick(t *testing.T) {
	t.Run("no disk.cfg", func(t *testing.T) {
		r := newStickRig(t, primary)
		delete(r.files, "config/disk.cfg")
		if _, err := r.startDeviceScan(stickDevice, ScanOptions{}); !errors.Is(err, ErrNoDiskCfg) {
			t.Fatalf("StartDeviceScan = %v, want %v", err, ErrNoDiskCfg)
		}
		r.assertReleased(t)
		if st, _ := r.svc.State(ctx0); st.Phase != PhaseNone {
			t.Fatalf("a refused scan left phase %s", st.Phase)
		}
	})
	t.Run("an unsupported layout without the override", func(t *testing.T) {
		r := newStickRig(t, primary)
		r.files["changes.txt"] = []byte("# Version 5.0.0 2020-01-01\n")
		if _, err := r.startDeviceScan(stickDevice, ScanOptions{}); !errors.Is(err, ErrUnsupportedLayout) {
			t.Fatalf("StartDeviceScan = %v, want %v", err, ErrUnsupportedLayout)
		}
		r.assertReleased(t)
		if _, err := r.startDeviceScan(stickDevice, ScanOptions{UnverifiedLayout: true}); err != nil {
			t.Fatalf("with the override: %v", err)
		}
	})
}

func TestStartDeviceScan_AMountThatFailsIsReportedAndLeavesNothing(t *testing.T) {
	r := newStickRig(t, primary)
	r.mounter.MountErr = errors.New("wrong fs type")
	if _, err := r.startDeviceScan(stickDevice, ScanOptions{}); !errors.Is(err, ErrFlashDevice) {
		t.Fatalf("StartDeviceScan = %v, want %v", err, ErrFlashDevice)
	}
	r.assertReleased(t)
	if st, _ := r.svc.State(ctx0); st.Phase != PhaseNone {
		t.Fatalf("a refused scan left phase %s", st.Phase)
	}
}

func TestStartDeviceScan_SubmitFailureRestoresTheSession(t *testing.T) {
	r := newStickRig(t, primary)
	if err := scanNow(r.svc, zipOf(t, r.files, false), ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	before, _ := r.svc.State(ctx0)
	boom := errors.New("scheduler down")
	err := r.svc.StartDeviceScan(ctx0, stickDevice, ScanOptions{}, func(context.Context, string) (string, error) { return "", boom })
	if !errors.Is(err, boom) {
		t.Fatalf("StartDeviceScan = %v, want the submit error", err)
	}
	if after, _ := r.svc.State(ctx0); !reflect.DeepEqual(before, after) {
		t.Fatalf("the session changed: before %+v, after %+v", before, after)
	}
	r.assertReleased(t)
}

func TestStartDeviceScan_RefusedWhileAScanRuns(t *testing.T) {
	r := newStickRig(t, primary)
	if _, err := r.startDeviceScan(stickDevice, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	r.svc.JobEnded = fakeJobs{}.ended
	if _, err := r.startDeviceScan(stickDevice, ScanOptions{}); !errors.Is(err, ErrScanInProgress) {
		t.Fatalf("a second scan = %v, want %v", err, ErrScanInProgress)
	}
}

// A failure inside the job leaves the session failed and the stick unmounted.
func TestRunScan_AFailedStickScanIsRecordedAndReleased(t *testing.T) {
	r := newStickRig(t, primary)
	scan, err := r.startDeviceScan(stickDevice, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The stick that was fine when the scan was queued is empty when the job
	// mounts it.
	r.mounter.OnMount = func(string) error { return nil }
	if err := r.svc.RunScan(ctx0, io.Discard, scan); !errors.Is(err, ErrNoDiskCfg) {
		t.Fatalf("RunScan = %v, want %v", err, ErrNoDiskCfg)
	}
	r.assertReleased(t)
	st, _ := r.svc.State(ctx0)
	if st.Phase != PhaseScanFailed || st.Report != nil || !strings.Contains(st.ScanError, "disk.cfg") {
		t.Fatalf("State = %+v, want scan_failed with the reason", st)
	}
}

func TestRunScan_AStickThatCannotBeUnmountedFailsTheScanAndIsRetriedBeforeTheNextMount(t *testing.T) {
	r := newStickRig(t, primary)
	scan, err := r.startDeviceScan(stickDevice, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r.mounter.UnmountErr = errors.New("target is busy")
	if err := r.svc.RunScan(ctx0, io.Discard, scan); !errors.Is(err, ErrFlashDevice) || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("RunScan = %v, want %v naming the busy mount", err, ErrFlashDevice)
	}
	st, _ := r.svc.State(ctx0)
	if st.Phase != PhaseScanFailed || st.Report != nil {
		t.Fatalf("State = %+v: a scan of a source that could not be released has no report", st)
	}
	if got := r.mounter.MountedPaths(); len(got) != 1 || got[0] != r.mountpoint() {
		t.Fatalf("mounted = %v, want the mount that could not be released", got)
	}

	// The next scan will not mount over it while it cannot be released.
	mounts := len(r.mounter.Mounts)
	if _, err := r.startDeviceScan(stickDevice, ScanOptions{}); !errors.Is(err, ErrFlashDevice) {
		t.Fatalf("StartDeviceScan over a stuck mount = %v, want %v", err, ErrFlashDevice)
	}
	if len(r.mounter.Mounts) != mounts {
		t.Fatal("mounted over a mount that could not be released")
	}
	// Once it can be released, the next scan releases it first.
	r.mounter.UnmountErr = nil
	if err := r.scanDeviceNow(t, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	r.assertReleased(t)
}

func TestRunScan_ADeviceThatIsNoLongerTheStickIsNotMounted(t *testing.T) {
	r := newStickRig(t, primary)
	scan, err := r.startDeviceScan(stickDevice, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mounts := len(r.mounter.Mounts)
	// The stick is pulled and the name goes to a data disk.
	r.disks.Reassign(stickDevice, "/dev/sdq")
	r.disks.AddDisk(stickDevice, disk.Disk{Filesystem: "xfs", Label: "disk9", FSUUID: "cafe"})
	r.disks.Reassign("/dev/sdq", "/dev/sdq2")
	if err := r.svc.RunScan(ctx0, io.Discard, scan); !errors.Is(err, ErrNotFlashDevice) {
		t.Fatalf("RunScan = %v, want %v", err, ErrNotFlashDevice)
	}
	if len(r.mounter.Mounts) != mounts {
		t.Fatal("the job mounted a device that is not the stick")
	}
}

func TestRecover_ReleasesAMountAPreviousProcessLeft(t *testing.T) {
	r := newStickRig(t, primary)
	if err := os.MkdirAll(r.svc.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	r.mounter.SetMounted(r.mountpoint())
	if err := r.svc.Recover(ctx0); err != nil {
		t.Fatal(err)
	}
	r.assertReleased(t)
	if len(r.mounter.Unmounts) != 1 || r.mounter.Unmounts[0] != r.mountpoint() {
		t.Fatalf("unmounts = %v, want %s", r.mounter.Unmounts, r.mountpoint())
	}
}

// A mount that cannot be released does not take the zip source down with it.
func TestRecover_AStuckMountOnlyBlocksTheNextStickScan(t *testing.T) {
	r := newStickRig(t, primary)
	if err := os.MkdirAll(r.svc.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	r.mounter.SetMounted(r.mountpoint())
	r.mounter.UnmountErr = errors.New("target is busy")
	if err := r.svc.Recover(ctx0); err != nil {
		t.Fatalf("Recover = %v, want the zip source to stay usable", err)
	}
	if err := scanNow(r.svc, zipOf(t, r.files, false), ScanOptions{}); err != nil {
		t.Fatalf("a zip scan beside a stuck mount: %v", err)
	}
	if _, err := r.startDeviceScan(stickDevice, ScanOptions{}); !errors.Is(err, ErrFlashDevice) {
		t.Fatalf("StartDeviceScan = %v, want %v", err, ErrFlashDevice)
	}
	if len(r.mounter.Mounts) != 0 {
		t.Fatalf("mounted %v over a stale mount", r.mounter.Mounts)
	}
}

func internalBoot(files map[string][]byte) {
	capture := files["config/hoserva/capture.json"]
	files["config/hoserva/capture.json"] = bytes.Replace(capture, []byte(`"mode": "usb"`), []byte(`"mode": "internal"`), 1)
}

// An internal-boot server's zip is the only source (Q25): no stick is offered
// once a session's capture says so, and one is not read either.
func TestStickIsNotOfferedOrReadWhenTheSessionsCaptureSaysInternalBoot(t *testing.T) {
	r := newStickRig(t, primary)
	internalBoot(r.files)
	if err := scanNow(r.svc, zipOf(t, r.files, false), ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	offer, err := r.svc.FlashOffer(ctx0)
	if err != nil || !offer.ZipOnly || len(offer.Devices) != 0 {
		t.Fatalf("FlashOffer = %+v, %v; want zip only with no device", offer, err)
	}
	if _, err := r.startDeviceScan(stickDevice, ScanOptions{}); !errors.Is(err, ErrZipOnly) {
		t.Fatalf("StartDeviceScan = %v, want %v", err, ErrZipOnly)
	}
	if len(r.mounter.Mounts) != 0 {
		t.Fatalf("mounted %v although the zip is the only source", r.mounter.Mounts)
	}

	// Forgetting the session ends the rule with the capture that set it.
	if err := r.svc.Forget(ctx0); err != nil {
		t.Fatal(err)
	}
	if offer, _ := r.svc.FlashOffer(ctx0); offer.ZipOnly || len(offer.Devices) != 1 {
		t.Fatalf("after Forget FlashOffer = %+v", offer)
	}
}

func TestStartDeviceScan_AStickWhoseOwnCaptureSaysInternalBootIsRefused(t *testing.T) {
	r := newStickRig(t, primary)
	internalBoot(r.files)
	if _, err := r.startDeviceScan(stickDevice, ScanOptions{}); !errors.Is(err, ErrZipOnly) {
		t.Fatalf("StartDeviceScan = %v, want %v", err, ErrZipOnly)
	}
	r.assertReleased(t)
	if st, _ := r.svc.State(ctx0); st.Phase != PhaseNone {
		t.Fatalf("a refused scan left phase %s", st.Phase)
	}
}

func TestDirSource_ReadsFilesAndSkipsTheJunkTheZipSkips(t *testing.T) {
	_, _, files := newService(t, primary)
	dir := t.TempDir()
	if err := writeTree(dir, files); err != nil {
		t.Fatal(err)
	}
	src, err := OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	zsrc := openZipBytes(t, zipOf(t, files, true))

	for _, d := range []string{"", "config", "config/hoserva", "config/plugins/dockerMan"} {
		if got, want := src.List(d), zsrc.List(d); !reflect.DeepEqual(got, want) {
			t.Errorf("List(%q):\ndir %v\nzip %v", d, got, want)
		}
	}
	for _, n := range src.List("") {
		if ignoredPath(n) {
			t.Errorf("List returned the ignored %q", n)
		}
	}
	want, _ := zsrc.Read("config/disk.cfg")
	got, err := src.Read("config/disk.cfg")
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("Read = %q, %v; want the zip's content", got, err)
	}
	if _, err := src.Read("config/nope.cfg"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Read of a missing file = %v, want fs.ErrNotExist", err)
	}
	if _, err := src.Read("config/disk.cfg/inside"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Read below a file = %v, want fs.ErrNotExist", err)
	}
	if err := src.Err(); err != nil {
		t.Fatalf("Err = %v after reads that only found nothing", err)
	}
}

// What a flash holds is data: a link in it that leads out of the mount is not
// followed.
func TestDirSource_DoesNotFollowALinkOutOfTheMount(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("not the flash"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "config", "disk.cfg")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	src, err := OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	if data, err := src.Read("config/disk.cfg"); err == nil {
		t.Fatalf("read %q through a link out of the mount", data)
	}
	if got := src.List("config"); len(got) != 0 {
		t.Fatalf("List = %v, a link is not a regular file", got)
	}
}

func TestDirSource_BoundsAFileAndKeepsTheFirstListingError(t *testing.T) {
	dir := t.TempDir()
	big := make([]byte, maxEntryBytes+1)
	if err := os.WriteFile(filepath.Join(dir, "big"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	src, err := OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	if _, err := src.Read("big"); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("Read of an oversized file = %v", err)
	}

	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })
	if f, err := os.Open(sub); err == nil {
		_ = f.Close()
		t.Skip("running as a user that reads an unreadable directory")
	}
	src.List("")
	if src.Err() == nil {
		t.Fatal("an unreadable directory was listed without an error: a scan could report on half a flash")
	}
}
