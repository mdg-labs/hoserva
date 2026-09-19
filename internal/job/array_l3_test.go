//go:build l3

// This file runs only inside the L3 VM harness (doc 06 §4), never on the
// host or in the loop-device lab: it is built with `go test -tags l3 -c`
// from the host (compiling touches no device) and the resulting binary
// is copied into the guest with `scp` and run with `sudo` over `ssh` —
// the same build-elsewhere/run-inside-the-real-environment pattern
// internal/pool/mount_lab_test.go's own header established for the lab,
// driven here by scripts/vm/array-sequence-check.sh (issue #146). It
// closes exactly the two gaps internal/job/array_lab_test.go's own
// header names as blocked-until-L3: real physical disk mount/unmount
// through a real systemd .mount unit (internal/disk.MountUnitController,
// which needs a real init system the loop-device lab's own capability-
// narrowed container doesn't have), and a real OS process standing in
// for a not-yet-built internal/container service holding a file open on
// a real mergerfs pool mount.
//
// Identity resolution gap (found while building this test, reported
// rather than fixed — internal/disk is outside this issue's declared
// scope): internal/disk.ResolveIdentity does not recognize the
// "virtio-<serial>" /dev/disk/by-id link libvirt/udev create for a
// virtio-blk disk — confirmed empirically against this exact harness
// (`ls -la /dev/disk/by-id` inside a fresh L3 guest shows
// "virtio-hoserva-<lab>-disk1 -> ../../vdc" etc.); ResolveIdentity's own
// switch only recognizes wwn-/ata-/scsi-/nvme-/usb- prefixes, and
// create-vm.sh's own domain XML sets each array disk's <serial> but no
// <wwn>. So disk.LinuxProvider.List() reports every L3 array disk with
// an entirely empty Identity (WeakIdentity, no WWN, no Serial) today,
// which disk.Identity.Matches can never match against anything. The
// missing-disk scenario below works around this the same way
// scripts/vm/spindown-check.sh already works around the same harness
// limitation for its own SMART-device discovery: identity comes from
// the domain XML's own untruncated <serial> (read by
// array-sequence-check.sh via `virsh dumpxml` and passed in through
// HOSERVA_L3_EXPECTED_SERIALS and HOSERVA_L3_PCI_SERIAL_MAP below,
// the latter keyed by each disk's own PCI slot — see l3PCISerialMap and
// l3DevicePCISlot), never from the guest's own
// /sys/class/block/<dev>/serial — the guest kernel truncates that to
// VIRTIO_BLK_ID_BYTES (20 bytes) before this process ever sees it, which
// collides every array disk's serial under a long enough HOSERVA_LAB_ID
// (this project's own nightly shape). This still
// genuinely proves disk.StorageGate's own
// Evaluate/Ready/Missing/Acknowledge logic, and
// job.ArraySequence.Start's refusal, against a real, physically missing
// virtio-blk device — it does not exercise ResolveIdentity's own by-id
// resolution, which the gap above leaves untested at every tier for
// this harness (a real disk, on real hardware, is unaffected: ata-,
// scsi-, nvme- and usb- prefixes are exactly what a real SATA/USB/NVMe
// drive's own by-id links use).

package job

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/pool"
)

func requireL3(t *testing.T) {
	t.Helper()
	if os.Getenv("HOSERVA_LAB_ID") == "" {
		t.Skip("HOSERVA_LAB_ID not set — this test only runs inside its own L3 VM, driven by scripts/vm/array-sequence-check.sh")
	}
}

func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("not running as root — this test formats and mounts real disks and manages real systemd units, and must run via sudo inside the guest")
	}
}

func mustL3Env(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("%s is not set — expected scripts/vm/array-sequence-check.sh to set it before invoking this binary", name)
	}
	return v
}

// presentArrayDevices lists every currently attached array disk's kernel
// device name (e.g. "vdc"), excluding the OS disk ("vda") and any
// partition entries. It never assumes a fixed letter for a given role —
// a disk detached at boot shifts every later device's own letter, since
// the guest kernel assigns /dev/vdX by virtio-blk probe order, not by
// libvirt's target-dev label — so every caller identifies a disk by its
// own PCI slot (l3DevicePCISlot) looked up in l3PCISerialMap, never by
// its device letter or by reading the guest's own, potentially-truncated
// /sys/class/block/<dev>/serial.
func presentArrayDevices(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir("/sys/class/block")
	if err != nil {
		t.Fatalf("reading /sys/class/block: %v", err)
	}
	var devs []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "vd") || name == "vda" {
			continue
		}
		if _, err := os.Stat(filepath.Join("/sys/class/block", name, "partition")); err == nil {
			continue // a partition, not a whole disk
		}
		devs = append(devs, name)
	}
	sort.Strings(devs)
	return devs
}

// l3PCISerialMap parses HOSERVA_L3_PCI_SERIAL_MAP, a "<slot>:<serial> ..."
// list scripts/vm/array-sequence-check.sh builds from the domain XML's
// own untruncated <serial> and <address .../ slot='0x..'/> elements (via
// `virsh dumpxml`, the same device-discovery approach
// scripts/vm/spindown-check.sh already uses). This is keyed by each
// disk's own PCI slot, not by its libvirt target-dev label
// ("vdc"/"vdd"/...) or by anything read from the guest itself, for two
// independent reasons: the guest kernel's own /dev/vdX naming is
// assigned by virtio-blk probe order, not by libvirt's target-dev label,
// so a detached disk shifts every later disk's own device letter
// (confirmed empirically against this exact harness — see
// array-sequence-check.sh's own comment on PCI_SERIAL_MAP); and the
// guest's own /sys/class/block/<dev>/serial is truncated by the guest
// kernel to VIRTIO_BLK_ID_BYTES (20 bytes) before it ever gets there,
// which collides every array disk's serial under a long enough
// HOSERVA_LAB_ID (this project's own nightly shape). A disk's own PCI
// slot has neither problem: it is assigned explicitly by create-vm.sh,
// never renumbered, and resolvable from the guest's own
// /sys/class/block/<dev>/device symlink (see l3DevicePCISlot).
func l3PCISerialMap(t *testing.T) map[string]string {
	t.Helper()
	raw := mustL3Env(t, "HOSERVA_L3_PCI_SERIAL_MAP")
	m := make(map[string]string)
	for _, pair := range strings.Fields(raw) {
		slot, serial, ok := strings.Cut(pair, ":")
		if !ok || slot == "" || serial == "" {
			t.Fatalf("HOSERVA_L3_PCI_SERIAL_MAP: malformed entry %q, want <slot>:<serial>", pair)
		}
		m[slot] = serial
	}
	return m
}

// l3DevicePCISlot resolves dev's own PCI slot (e.g. "0c") by following
// /sys/class/block/<dev>/device, which resolves to a path ending in
// ".../0000:00:<slot>.<function>/virtioN" — the same slot create-vm.sh
// assigned it and array-sequence-check.sh read from the domain XML into
// HOSERVA_L3_PCI_SERIAL_MAP. Never trusts /dev/vdX naming order, which
// the guest kernel reassigns by probe order once a disk is detached.
func l3DevicePCISlot(t *testing.T, dev string) string {
	t.Helper()
	target, err := filepath.EvalSymlinks(filepath.Join("/sys/class/block", dev, "device"))
	if err != nil {
		t.Fatalf("resolving PCI device path for %s: %v", dev, err)
	}
	parts := strings.Split(target, string(filepath.Separator))
	if len(parts) < 2 {
		t.Fatalf("unexpected PCI device path for %s: %q", dev, target)
	}
	pciAddr := parts[len(parts)-2] // e.g. "0000:00:0c.0"
	fields := strings.Split(pciAddr, ":")
	if len(fields) != 3 {
		t.Fatalf("unexpected PCI address shape for %s: %q", dev, pciAddr)
	}
	slot, _, ok := strings.Cut(fields[2], ".")
	if !ok || slot == "" {
		t.Fatalf("unexpected PCI slot.function shape for %s: %q", dev, fields[2])
	}
	return slot
}

func l3MustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func l3MustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func l3MustReadFile(t *testing.T, path string) string {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(got)
}

func l3SHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s for checksum: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func l3AssertMounted(t *testing.T, ctx context.Context, r disk.Runner, where string) {
	t.Helper()
	if _, err := r.Run(ctx, "mountpoint", "-q", where); err != nil {
		t.Fatalf("mountpoint -q %s: got an error (%v), want it to report a real mount", where, err)
	}
}

func l3AssertNotMounted(t *testing.T, ctx context.Context, r disk.Runner, where string) {
	t.Helper()
	if _, err := r.Run(ctx, "mountpoint", "-q", where); err == nil {
		t.Fatalf("mountpoint -q %s: succeeded, want %s to NOT be a mount", where, where)
	}
}

func l3AssertEmptyDir(t *testing.T, where string) {
	t.Helper()
	entries, err := os.ReadDir(where)
	if err != nil {
		t.Fatalf("reading %s: %v", where, err)
	}
	if len(entries) != 0 {
		t.Fatalf("%s is not empty (%v) — want nothing ever written into an unmounted mountpoint", where, entries)
	}
}

// l3WriteMountUnit writes unit's real content to
// /etc/systemd/system/<UnitFileName> and reloads systemd — the D4
// content a future config.Generator would write, built and installed
// directly here since neither it nor cmd/hoservad exist yet (this
// issue's own "Out of scope"). Removed again in t.Cleanup so repeat runs
// of this script never accumulate stale units.
func l3WriteMountUnit(t *testing.T, ctx context.Context, r disk.Runner, unit disk.MountUnit) {
	t.Helper()
	path := filepath.Join("/etc/systemd/system", disk.UnitFileName(unit.Where))
	if err := os.WriteFile(path, []byte(unit.Render()), 0o644); err != nil {
		t.Fatalf("writing unit file %s: %v", path, err)
	}
	t.Cleanup(func() {
		_ = os.Remove(path)
		_, _ = r.Run(context.Background(), "systemctl", "daemon-reload")
	})
	if _, err := r.Run(ctx, "systemctl", "daemon-reload"); err != nil {
		t.Fatalf("systemctl daemon-reload (after writing %s): %v", path, err)
	}
}

// l3FormatAndMountUnit formats dev, writes and activates its own mount
// unit at where, and returns the real disk.MountUnitController driving
// it — the L3-only piece internal/job/array_lab_test.go's own header
// names as blocked (real physical disk mount/unmount via systemd).
func l3FormatAndMountUnit(t *testing.T, ctx context.Context, provider *disk.LinuxProvider, r disk.Runner, dev, where string) disk.MountUnitController {
	t.Helper()
	l3MustMkdirAll(t, where)
	if err := provider.Format(ctx, dev, disk.EXT4); err != nil {
		t.Fatalf("Format %s: %v", dev, err)
	}
	uuid, err := disk.FilesystemUUID(ctx, r, dev)
	if err != nil {
		t.Fatalf("FilesystemUUID %s: %v", dev, err)
	}
	unit := disk.MountUnit{Where: where, UUID: uuid, Filesystem: disk.EXT4, Description: "Hoserva L3 test disk " + where}
	l3WriteMountUnit(t, ctx, r, unit)
	return disk.MountUnitController{Unit: unit, Runner: r}
}

// l3RecordingService is the fake ArrayService the missing-disk scenario
// uses to prove nothing starts before the readiness gate is
// acknowledged — every real system-touching subsystem sits behind a
// package interface with a scriptable fake (CLAUDE.md), and
// internal/container does not exist yet (Phase 3, epic #66) to provide
// a real one.
type l3RecordingService struct {
	name    string
	started bool
	stopped bool
}

func (s *l3RecordingService) Name() string { return s.name }
func (s *l3RecordingService) Start(context.Context) error {
	s.started = true
	return nil
}
func (s *l3RecordingService) Stop(context.Context) error {
	s.stopped = true
	return nil
}

// TestL3ArraySequence_MissingDiskAtBoot proves the first half of #146:
// with a data disk genuinely detached before boot, disk.StorageGate
// reports the array not ready, job.ArraySequence.Start refuses
// (ErrStorageNotReady) without mounting anything or starting any
// service, and nothing lands in any of the still-plain, still-empty
// mountpoint directories — until an operator acknowledges the degraded
// state, at which point the array (built only from the disks genuinely
// present) mounts for real and its service starts.
//
// It does not, and cannot within this issue's own declared scope, prove
// that hoservad itself refuses to reach hoserva-storage.target at real
// boot — that needs internal/disk.StorageGate wired into cmd/hoservad's
// own boot path, which #146's own summary says is conditional
// ("once... wired") and cmd/hoservad/ is explicitly outside this issue's
// declared file scope. This test instead constructs a real StorageGate
// and a real ArraySequence directly, the same way
// internal/job/array_lab_test.go already does at L2 for the same
// documented reason (no array/disk/pool operation exists in
// api/openapi.yaml yet to configure a real array through a running
// hoservad).
func TestL3ArraySequence_MissingDiskAtBoot(t *testing.T) {
	requireL3(t)
	requireRoot(t)
	ctx := context.Background()
	r := disk.CommandRunner{}
	provider := disk.NewLinuxProvider()

	mountRoot := mustL3Env(t, "HOSERVA_L3_MOUNT_ROOT")
	expectedSerials := strings.Fields(mustL3Env(t, "HOSERVA_L3_EXPECTED_SERIALS"))
	if len(expectedSerials) == 0 {
		t.Fatal("HOSERVA_L3_EXPECTED_SERIALS is empty")
	}

	pciSerials := l3PCISerialMap(t)
	presentDevices := presentArrayDevices(t)
	if len(presentDevices) == 0 {
		t.Fatal("no array disks present — expected scripts/vm/array-sequence-check.sh to have booted with at least one still attached")
	}
	// Resolved once, by each present device's own PCI slot — never by its
	// /dev/vdX letter, which the guest kernel reassigns by probe order
	// once a disk is detached (see presentArrayDevices's own doc comment).
	devSerials := make(map[string]string, len(presentDevices))
	for _, d := range presentDevices {
		slot := l3DevicePCISlot(t, d)
		serial, ok := pciSerials[slot]
		if !ok {
			t.Fatalf("HOSERVA_L3_PCI_SERIAL_MAP has no entry for present device %s (PCI slot %s)", d, slot)
		}
		devSerials[d] = serial
	}
	presentSerials := make(map[string]bool, len(presentDevices))
	for _, d := range presentDevices {
		presentSerials[devSerials[d]] = true
	}

	// Independently compute which expected disk is genuinely absent, so
	// the assertion below on StorageGate's own Evaluate result is
	// checked against ground truth this test derived itself, not
	// against whatever Evaluate happens to return.
	var missingSerial string
	missingCount := 0
	for _, s := range expectedSerials {
		if !presentSerials[s] {
			missingSerial = s
			missingCount++
		}
	}
	if missingCount != 1 {
		t.Fatalf("found %d expected disk(s) absent from the running guest, want exactly 1 — scripts/vm/array-sequence-check.sh should have detached exactly one before this boot", missingCount)
	}

	var expected []disk.ExpectedDisk
	for i, s := range expectedSerials {
		expected = append(expected, disk.ExpectedDisk{
			Identity: disk.Identity{Serial: s},
			Role:     "data",
			MountAt:  filepath.Join(mountRoot, fmt.Sprintf("disk%d", i+1)),
		})
	}
	var present []disk.Identity
	for _, d := range presentDevices {
		present = append(present, disk.Identity{Serial: devSerials[d]})
	}

	gate := disk.NewStorageGate(expected)
	check := gate.Evaluate(present)
	if check.Ready {
		t.Fatal("StorageGate.Evaluate: Ready = true with a disk genuinely detached at boot, want false")
	}
	if len(check.Missing) != 1 || check.Missing[0].Identity.Serial != missingSerial {
		t.Fatalf("StorageGate.Evaluate: Missing = %v, want exactly the detached disk (serial %q)", check.Missing, missingSerial)
	}
	if gate.Ready() {
		t.Fatal("gate.Ready() = true before any acknowledgement, want false")
	}

	const shareName = "missingdisk"

	// Build the real, physical-disk half of the array from exactly the
	// disks actually present — a data disk that is missing is never
	// something this sequence tries to mount, matching doc 02 §1's own
	// degraded-but-explicit shape. Each disk is mounted once, briefly and
	// directly (bypassing ArraySequence), only long enough to create the
	// share's own directory on its real, formatted filesystem — mergerfs
	// unions what is already there, it does not create it (the same
	// precondition internal/pool/mount_lab_test.go's own setup follows)
	// — then unmounted again immediately, so the state ArraySequence.Start
	// sees below (and the "nothing written into an empty mountpoint"
	// assertion checks) is genuinely a plain, untouched, unmounted
	// directory, not one this setup itself just wrote into. Writing the
	// share directory straight into the not-yet-mounted /mnt path (found
	// the hard way, while first building this test) instead lands it on
	// the temporary host directory, not the disk's own filesystem, and
	// trips that very assertion once the real mount later shadows it.
	var diskCtls []ArrayMount
	var dataMounts []string
	var mountpoints []string
	for i, dev := range presentDevices {
		where := filepath.Join(mountRoot, fmt.Sprintf("disk%d", i+1))
		ctl := l3FormatAndMountUnit(t, ctx, provider, r, "/dev/"+dev, where)
		if err := ctl.Mount(ctx); err != nil {
			t.Fatalf("mounting %s to seed its share directory: %v", where, err)
		}
		l3MustMkdirAll(t, filepath.Join(where, shareName))
		if err := ctl.Unmount(ctx); err != nil {
			t.Fatalf("unmounting %s after seeding its share directory: %v", where, err)
		}
		diskCtls = append(diskCtls, ctl)
		dataMounts = append(dataMounts, where)
		mountpoints = append(mountpoints, where)
	}

	catchAllWhere := filepath.Join(mountRoot, "user")
	l3MustMkdirAll(t, catchAllWhere)
	mountpoints = append(mountpoints, catchAllWhere)
	opts := pool.Options{MinFreeSpace: "50M", Responsiveness: pool.Responsive}
	catchAllMnt, err := pool.CatchAllMount(dataMounts, opts)
	if err != nil {
		t.Fatalf("CatchAllMount: %v", err)
	}
	catchAllMnt.Where = catchAllWhere
	mounter := pool.Mounter{Runner: r}
	catchAllCtl := pool.MountController{Mnt: catchAllMnt, Mounter: mounter}

	share := pool.Share{Name: shareName, CacheMode: pool.ArrayOnly, CreatePolicy: pool.KeepFoldersTogether}
	shareWhere := filepath.Join(catchAllWhere, shareName)
	shareMnt, err := pool.ShareMount(share, dataMounts, "", opts)
	if err != nil {
		t.Fatalf("ShareMount: %v", err)
	}
	shareMnt.Where = shareWhere

	svc := &l3RecordingService{name: "l3-container-proxy"}
	seq := ArraySequence{
		Gate:        gate,
		Services:    []ArrayService{svc},
		ShareMounts: []ArrayMount{pool.MountController{Mnt: shareMnt, Mounter: mounter}},
		CatchAll:    catchAllCtl,
		Disks:       diskCtls,
	}
	// A safety net, not the primary teardown (the happy path below calls
	// seq.Stop itself): if this test fails partway through with real
	// mounts already up, t.Cleanup still tears them down, so a later
	// test run never finds a disk this one left mounted (confirmed the
	// hard way while first building this test: a failed run without
	// this left /dev/vdb still mounted for the next scenario).
	t.Cleanup(func() { _ = seq.Stop(context.Background()) })

	if err := seq.Start(ctx); !errors.Is(err, ErrStorageNotReady) {
		t.Fatalf("Start with an unready gate: got %v, want ErrStorageNotReady", err)
	}
	if svc.started {
		t.Fatal("the service's own Start was called despite the gate refusing readiness — a container must never start before acknowledgement")
	}
	for _, where := range mountpoints {
		l3AssertNotMounted(t, ctx, r, where)
		l3AssertEmptyDir(t, where)
	}

	if err := gate.Acknowledge(); err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}
	if !gate.Ready() {
		t.Fatal("gate.Ready() = false after Acknowledge, want true")
	}

	if err := seq.Start(ctx); err != nil {
		t.Fatalf("Start after acknowledgement: %v", err)
	}
	if !svc.started {
		t.Fatal("the service's own Start was never called even once the gate became ready")
	}
	for _, where := range dataMounts {
		l3AssertMounted(t, ctx, r, where)
	}
	l3AssertMounted(t, ctx, r, catchAllWhere)
	l3AssertMounted(t, ctx, r, shareWhere)

	marker := filepath.Join(shareWhere, "hello.txt")
	l3MustWriteFile(t, marker, "hello from the missing-disk L3 test")
	if got := l3MustReadFile(t, marker); got != "hello from the missing-disk L3 test" {
		t.Fatalf("readback of %s = %q, want the byte-identical original", marker, got)
	}

	if err := seq.Stop(ctx); err != nil {
		t.Fatalf("cleanup Stop: %v", err)
	}
	if !svc.stopped {
		t.Fatal("the service's own Stop was never called")
	}
}

// l3OpenFileService is the ArrayService proxy standing in for a
// not-yet-built internal/container service holding a file open on the
// pool: it execs a real `tail -f <path>` process (a real open file
// descriptor, not a simulation) and, on Stop, signals and waits for it
// to actually exit. This proves ArraySequence.Stop stops a service
// before unmounting the disk it depends on, and that the pattern
// generalizes once internal/container exists (Phase 3, epic #66) — it
// proves nothing about a real container runtime's own stop-hook
// behavior, which only internal/container itself, once built, can prove
// (doc 06 §6's own "proxy with residual risk stated" pattern).
type l3OpenFileService struct {
	name string
	path string
	cmd  *exec.Cmd
}

func (s *l3OpenFileService) Name() string { return s.name }

func (s *l3OpenFileService) Start(context.Context) error {
	cmd := exec.Command("tail", "-f", s.path)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting tail -f %s: %w", s.path, err)
	}
	s.cmd = cmd
	return nil
}

func (s *l3OpenFileService) Stop(context.Context) error {
	if s.cmd == nil || s.cmd.Process == nil {
		return nil
	}
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("signaling tail -f (pid %d): %w", s.cmd.Process.Pid, err)
	}
	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()
	select {
	case <-done:
		return nil
	case <-time.After(10 * time.Second):
		_ = s.cmd.Process.Kill()
		<-done
		return fmt.Errorf("tail -f (pid %d) did not exit within 10s of SIGTERM", s.cmd.Process.Pid)
	}
}

func (s *l3OpenFileService) alive() bool {
	if s.cmd == nil || s.cmd.Process == nil {
		return false
	}
	return s.cmd.Process.Signal(syscall.Signal(0)) == nil
}

// waitUntilFileOpen polls /proc/<pid>/fd until one of tail's own file
// descriptors resolves to s.path, bounded by timeout. Start returning
// does not itself mean tail has gotten as far as open(2)-ing the file
// yet — confirmed the hard way while first building this test: without
// this wait, the very next unmount attempt sometimes raced ahead of
// tail's own open and succeeded, silently proving nothing.
func (s *l3OpenFileService) waitUntilFileOpen(timeout time.Duration) error {
	if s.cmd == nil || s.cmd.Process == nil {
		return fmt.Errorf("l3OpenFileService: process was never started")
	}
	fdDir := fmt.Sprintf("/proc/%d/fd", s.cmd.Process.Pid)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(fdDir)
		if err == nil {
			for _, e := range entries {
				if target, err := os.Readlink(filepath.Join(fdDir, e.Name())); err == nil && target == s.path {
					return nil
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("l3OpenFileService: pid %d did not open %s within %s", s.cmd.Process.Pid, s.path, timeout)
}

// TestL3ArraySequence_ServiceStopsBeforeUnmount proves the second half
// of #146: with a proxy service holding a file open on a real mergerfs
// pool mount, unmounting the pool directly (bypassing ArraySequence)
// while that file is still open genuinely fails — confirming the hazard
// doc 02 §4 names ("a container holding a file open blocks the unmount")
// is real in this environment, not theoretical — and job.ArraySequence's
// own Stop, run as intended, avoids it: the service is stopped (its
// process actually exits) before the pool is unmounted, the unmount then
// succeeds, and the file is byte-identical after a remount.
func TestL3ArraySequence_ServiceStopsBeforeUnmount(t *testing.T) {
	requireL3(t)
	requireRoot(t)
	ctx := context.Background()
	r := disk.CommandRunner{}
	provider := disk.NewLinuxProvider()

	mountRoot := mustL3Env(t, "HOSERVA_L3_MOUNT_ROOT")
	presentDevices := presentArrayDevices(t)
	if len(presentDevices) == 0 {
		t.Fatal("no array disks present")
	}

	var diskCtls []ArrayMount
	var dataMounts []string
	for i, dev := range presentDevices {
		// No hyphen: see array-sequence-check.sh's own comment on
		// HOSERVA_L3_MOUNT_ROOT for why a physical disk's mount path
		// must not contain one.
		where := filepath.Join(mountRoot, fmt.Sprintf("svcdisk%d", i+1))
		ctl := l3FormatAndMountUnit(t, ctx, provider, r, "/dev/"+dev, where)
		if err := ctl.Mount(ctx); err != nil {
			t.Fatalf("mounting %s: %v", where, err)
		}
		t.Cleanup(func(ctl disk.MountUnitController) func() {
			return func() { _ = ctl.Unmount(context.Background()) }
		}(ctl))
		diskCtls = append(diskCtls, ctl)
		dataMounts = append(dataMounts, where)
	}

	poolWhere := filepath.Join(mountRoot, "svcpool")
	l3MustMkdirAll(t, poolWhere)
	opts := pool.Options{MinFreeSpace: "50M", Responsiveness: pool.Responsive}
	catchAllMnt, err := pool.CatchAllMount(dataMounts, opts)
	if err != nil {
		t.Fatalf("CatchAllMount: %v", err)
	}
	catchAllMnt.Where = poolWhere
	mounter := pool.Mounter{Runner: r}
	catchAllCtl := pool.MountController{Mnt: catchAllMnt, Mounter: mounter}
	if err := catchAllCtl.Mount(ctx); err != nil {
		t.Fatalf("mounting pool at %s: %v", poolWhere, err)
	}
	t.Cleanup(func() { _ = catchAllCtl.Unmount(context.Background()) })

	filePath := filepath.Join(poolWhere, "open-file.txt")
	content := "the container's own file, must survive the array stop\n"
	l3MustWriteFile(t, filePath, content)
	wantSum := l3SHA256(t, filePath)

	svc := &l3OpenFileService{name: "l3-container-proxy", path: filePath}
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("starting the proxy service: %v", err)
	}
	// A safety net: if this test fails before Stop runs the real
	// ordering below, this still kills the held-open process so it
	// never outlives the test.
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })
	if err := svc.waitUntilFileOpen(5 * time.Second); err != nil {
		t.Fatalf("waiting for the proxy service to actually open %s: %v", filePath, err)
	}

	// Confirm the hazard is real before trusting the ordering that
	// avoids it (CLAUDE.md: "anything that can lose data gets its test
	// before its implementation").
	if err := mounter.Unmount(ctx, poolWhere); err == nil {
		_ = svc.Stop(context.Background())
		t.Fatal("fusermount -u succeeded while the proxy service still held a file open on the pool — the hazard this test exists to catch did not reproduce in this environment, so this test cannot prove anything about avoiding it")
	}
	l3AssertMounted(t, ctx, r, poolWhere)

	seq := ArraySequence{
		Services: []ArrayService{svc},
		CatchAll: catchAllCtl,
		Disks:    diskCtls,
	}
	if err := seq.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v (the real hazard confirmed above should already be avoided by stopping the service before unmounting)", err)
	}
	if !svc.processExited() {
		t.Fatal("the service was never actually stopped")
	}
	if svc.alive() {
		t.Fatal("the proxy process is still running after Stop reported success")
	}
	l3AssertNotMounted(t, ctx, r, poolWhere)
	for _, where := range dataMounts {
		l3AssertNotMounted(t, ctx, r, where)
	}

	if err := seq.Start(ctx); err != nil {
		t.Fatalf("Start (remount): %v", err)
	}
	l3AssertMounted(t, ctx, r, poolWhere)
	got := l3MustReadFile(t, filePath)
	if got != content {
		t.Fatalf("after remount, file = %q, want the byte-identical original %q", got, content)
	}
	if got := l3SHA256(t, filePath); got != wantSum {
		t.Fatalf("after remount, sha256 = %s, want %s", got, wantSum)
	}

	_ = seq.Stop(ctx) // best-effort cleanup
}

// processExited reports whether Stop actually ran its process to
// completion — cmd is set only once Start succeeded, and ProcessState
// is set only once Wait has returned, so this is true only once the
// real tail -f process has genuinely exited.
func (s *l3OpenFileService) processExited() bool {
	return s.cmd != nil && s.cmd.ProcessState != nil
}
