//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45),
// never on the host: it is built with `go test -tags lab -c` from the
// host (compiling touches no device) and the resulting binary is run
// with `docker compose exec -T lab <binary>` inside the lab container.
// It proves createArray's RunFunc formats only assigned loop devices,
// persists topology in SQLite, mounts those devices by filesystem UUID
// at the documented paths, that a failed FormatPlan writes no topology
// and no mount units, and that a matching retry after persist-then-apply
// failure re-applies from SQLite without a second format.

package job

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"
)

func labDir(t *testing.T) string {
	t.Helper()
	id := os.Getenv("HOSERVA_LAB_ID")
	if id == "" {
		t.Skip("HOSERVA_LAB_ID not set — this test only runs inside its own lab container")
	}
	return filepath.Join("/lab", id)
}

func createLoopImage(ctx context.Context, t *testing.T, r disk.Runner, lab, name, size string) string {
	t.Helper()

	imgDir := filepath.Join(lab, "img")
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", imgDir, err)
	}
	img := filepath.Join(imgDir, name+".img")
	if err := os.Remove(img); err != nil && !os.IsNotExist(err) {
		t.Fatalf("removing stale %s: %v", img, err)
	}

	if _, err := r.Run(ctx, "truncate", "-s", size, img); err != nil {
		t.Fatalf("truncate %s: %v", img, err)
	}

	out, err := r.Run(ctx, "losetup", "--find", "--show", img)
	if err != nil {
		t.Fatalf("losetup --find --show %s: %v", img, err)
	}
	dev := strings.TrimSpace(string(out))
	if !strings.HasPrefix(dev, "/dev/loop") {
		t.Fatalf("losetup %s: got %q, want a /dev/loopN device", img, dev)
	}

	backing, err := r.Run(ctx, "losetup", "-j", img, "--output", "NAME", "--noheadings")
	if err != nil || strings.TrimSpace(string(backing)) != dev {
		t.Fatalf("losetup -j %s: got %q (err %v), want %q — refusing to trust a device this call did not just attach", img, backing, err, dev)
	}

	t.Cleanup(func() {
		if !loopStillBacksImage(t, dev, img) {
			t.Logf("skipping detach of %s: no longer backed by %s (likely reused by another lab)", dev, img)
			return
		}
		_, _ = r.Run(context.Background(), "losetup", "-d", dev)
	})
	return dev
}

// loopOwnerSysfsRoot stands in for /sys/block; overridden by
// TestLabLoopStillBacksImage_RefusesReusedDevice to inject a temp
// directory so the ownership check below can be exercised without a
// real loop device.
var loopOwnerSysfsRoot = "/sys/block"

// loopStillBacksImage reports whether dev is still the loop device
// backing img, read straight from
// /sys/block/<loopN>/loop/backing_file rather than trusted from when
// this test first attached it. Loop device numbers are host-global
// (doc 06 §3, Q45) and freed the moment another process detaches them,
// so by the time a t.Cleanup runs, dev may already belong to a
// concurrently running lab's own image — CLAUDE.md's "detach only the
// ones backed by your own lab's image files".
func loopStillBacksImage(t *testing.T, dev, img string) bool {
	t.Helper()
	loopName := filepath.Base(dev)
	if !strings.HasPrefix(loopName, "loop") {
		return false
	}
	raw, err := os.ReadFile(filepath.Join(loopOwnerSysfsRoot, loopName, "loop", "backing_file"))
	if err != nil {
		// Not attached any more — nothing left for dev to own.
		return false
	}
	backing := strings.TrimSuffix(strings.TrimSpace(string(raw)), " (deleted)")

	wantImg, err := filepath.EvalSymlinks(img)
	if err != nil {
		wantImg = filepath.Clean(img)
	}
	gotImg, err := filepath.EvalSymlinks(backing)
	if err != nil {
		gotImg = filepath.Clean(backing)
	}
	return gotImg == wantImg
}

func blkidType(ctx context.Context, r disk.Runner, dev string) string {
	out, _ := r.Run(ctx, "blkid", "-s", "TYPE", "-o", "value", dev)
	return strings.TrimSpace(string(out))
}

func blkidUUID(ctx context.Context, r disk.Runner, dev string) string {
	out, _ := r.Run(ctx, "blkid", "-s", "UUID", "-o", "value", dev)
	return strings.TrimSpace(string(out))
}

func findmntUUID(ctx context.Context, r disk.Runner, where string) string {
	out, _ := r.Run(ctx, "findmnt", "-n", "-o", "UUID", where)
	return strings.TrimSpace(string(out))
}

func labRegisterDiskFormat(t *testing.T, s *Scheduler, p disk.Provider, r disk.Runner, mounter disk.UnitMounter) (*store.ArrayStore, string) {
	t.Helper()
	st := store.NewArrayStore(newTestDB(t))
	genRoot := t.TempDir()
	labRegisterDiskFormatDeps(t, s, p, r, st, genRoot, mounter)
	return st, genRoot
}

func labRegisterDiskFormatDeps(t *testing.T, s *Scheduler, p disk.Provider, r disk.Runner, st *store.ArrayStore, genRoot string, mounter disk.UnitMounter) {
	t.Helper()
	s.registry.Register(TypeDiskFormat, false, RunDiskFormat(DiskFormatDeps{
		Provider:  p,
		Runner:    r,
		Store:     st,
		Generator: config.NewGenerator(genRoot),
		Mounter:   mounter,
	}))
}

// countingRunner counts mkfs.* invocations so a matching retry after
// persist-then-apply failure can prove FormatPlan did not run again.
type countingRunner struct {
	inner disk.Runner
	mkfs  int
}

func (r *countingRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if strings.HasPrefix(name, "mkfs.") {
		r.mkfs++
	}
	return r.inner.Run(ctx, name, args...)
}

// failOnceMounter fails the first Mount, then delegates. Used to inject
// applyArrayFromStore failure after PutArray has already persisted.
type failOnceMounter struct {
	inner   disk.UnitMounter
	failed  bool
	failErr error
}

func (m *failOnceMounter) Mount(ctx context.Context, unit disk.MountUnit) error {
	if !m.failed {
		m.failed = true
		return m.failErr
	}
	return m.inner.Mount(ctx, unit)
}

func (m *failOnceMounter) Unmount(ctx context.Context, unit disk.MountUnit) error {
	return m.inner.Unmount(ctx, unit)
}

func unmountIfMounted(r disk.Runner, where string) {
	_, _ = r.Run(context.Background(), "umount", where)
}

// TestLabCreateArray_RunFuncFormatsOnlyAssignedLoops is the create-array
// job's central safety-critical property, exercised against real mkfs
// and real loop devices: an attached but unassigned loop is not
// formatted, a non-loop path standing in for a real disk is refused
// before mkfs, and only the plan's own assigned loops are formatted.
func TestLabCreateArray_RunFuncFormatsOnlyAssignedLoops(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	exec := disk.CommandRunner{}
	provider := &disk.LinuxProvider{Lister: disk.NewLister(), Exec: exec}

	parity := createLoopImage(ctx, t, exec, lab, "create-array-parity", "320M")
	data := createLoopImage(ctx, t, exec, lab, "create-array-data", "320M")
	cache := createLoopImage(ctx, t, exec, lab, "create-array-cache", "320M")
	spare := createLoopImage(ctx, t, exec, lab, "create-array-spare", "320M")
	loopSize := int64(320 << 20)

	s := newTestScheduler(t)
	c := disk.AssignedDisk{Device: cache, Filesystem: disk.XFS}
	labRegisterDiskFormat(t, s, provider, exec, disk.NewFakeMounter())

	good := DiskFormatParams{
		Parity: []disk.AssignedDisk{{Device: parity, Filesystem: disk.XFS}},
		Data:   []disk.AssignedDisk{{Device: data, Filesystem: disk.XFS}},
		Cache:  &c,
		Sizes:  map[string]int64{parity: loopSize, data: loopSize, cache: loopSize},
	}
	good.Confirmation = good.Plan().Confirmation()

	j, err := s.Submit(ctx, TypeDiskFormat, nil, mustJSON(t, good))
	if err != nil {
		t.Fatalf("Submit(assigned loops): %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("assigned loops: status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	if got := blkidType(ctx, exec, parity); got != "xfs" {
		t.Fatalf("parity %s: blkid TYPE = %q, want xfs", parity, got)
	}
	if got := blkidType(ctx, exec, data); got != "xfs" {
		t.Fatalf("data %s: blkid TYPE = %q, want xfs", data, got)
	}
	if got := blkidType(ctx, exec, cache); got != "xfs" {
		t.Fatalf("cache %s: blkid TYPE = %q, want xfs", cache, got)
	}
	if got := blkidType(ctx, exec, spare); got != "" {
		t.Fatalf("spare %s gained a filesystem (%q)", spare, got)
	}

	s2 := newTestScheduler(t)
	st2, genRoot2 := labRegisterDiskFormat(t, s2, provider, exec, disk.NewFakeMounter())
	bad := DiskFormatParams{
		Parity: []disk.AssignedDisk{{Device: spare, Filesystem: disk.XFS}},
		Data:   []disk.AssignedDisk{{Device: "/dev/sda1", Filesystem: disk.XFS}},
		Sizes:  map[string]int64{spare: loopSize, "/dev/sda1": loopSize},
	}
	bad.Confirmation = bad.Plan().Confirmation()

	j, err = s2.Submit(ctx, TypeDiskFormat, nil, mustJSON(t, bad))
	if err != nil {
		t.Fatalf("Submit(non-loop path): %v", err)
	}
	finished = await(t, s2, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("non-loop path: status = %s (%s), want failed", finished.Status, finished.ErrorMessage)
	}
	if !strings.Contains(finished.ErrorMessage, "not a loop device") && !strings.Contains(finished.ErrorMessage, "unmanaged") {
		t.Fatalf("non-loop path ErrorMessage = %q, want unmanaged-device refusal", finished.ErrorMessage)
	}
	if got := blkidType(ctx, exec, spare); got != "" {
		t.Fatalf("spare %s was formatted (%q) when the plan named /dev/sda1", spare, got)
	}
	exists, err := st2.Exists(ctx)
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if exists {
		t.Fatal("unmanaged-path job wrote array topology")
	}
	if _, err := os.Stat(filepath.Join(genRoot2, "snapraid.conf")); !os.IsNotExist(err) {
		t.Fatal("unmanaged-path job wrote snapraid.conf")
	}
}

func TestLabCreateArray_FailedFormatWritesNoTopologyOrMounts(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	exec := disk.CommandRunner{}
	provider := &disk.LinuxProvider{Lister: disk.NewLister(), Exec: exec}

	// Data is too small for mkfs.xfs so FormatPlan fails after parity
	// may already have been wiped — the data-loss shape this test
	// exists to refuse: no topology, no units, no mounts.
	parity := createLoopImage(ctx, t, exec, lab, "fail-topology-parity", "320M")
	data := createLoopImage(ctx, t, exec, lab, "fail-topology-data", "16M")
	cache := createLoopImage(ctx, t, exec, lab, "fail-topology-cache", "320M")
	loopSize := int64(320 << 20)

	t.Cleanup(func() {
		unmountIfMounted(exec, "/mnt/disk1")
		unmountIfMounted(exec, "/mnt/parity1")
		unmountIfMounted(exec, "/mnt/cache")
	})

	s := newTestScheduler(t)
	c := disk.AssignedDisk{Device: cache, Filesystem: disk.XFS}
	st, genRoot := labRegisterDiskFormat(t, s, provider, exec, disk.DirectMounter{Runner: exec})

	params := DiskFormatParams{
		Parity: []disk.AssignedDisk{{Device: parity, Filesystem: disk.XFS}},
		Data:   []disk.AssignedDisk{{Device: data, Filesystem: disk.XFS}},
		Cache:  &c,
		Sizes:  map[string]int64{parity: loopSize, data: loopSize, cache: loopSize},
	}
	params.Confirmation = params.Plan().Confirmation()

	j, err := s.Submit(ctx, TypeDiskFormat, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s (%s), want failed", finished.Status, finished.ErrorMessage)
	}
	exists, err := st.Exists(ctx)
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if exists {
		t.Fatal("failed FormatPlan wrote array topology")
	}
	if _, err := os.Stat(filepath.Join(genRoot, "snapraid.conf")); !os.IsNotExist(err) {
		t.Fatal("failed FormatPlan wrote snapraid.conf")
	}
	if entries, err := os.ReadDir(filepath.Join(genRoot, "systemd", "system")); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".mount") {
				t.Fatalf("failed FormatPlan wrote mount unit %s", e.Name())
			}
		}
	}
	if findmntUUID(ctx, exec, "/mnt/disk1") != "" {
		t.Fatal("failed FormatPlan mounted /mnt/disk1")
	}
	if findmntUUID(ctx, exec, "/mnt/parity1") != "" {
		t.Fatal("failed FormatPlan mounted /mnt/parity1")
	}
	if findmntUUID(ctx, exec, "/mnt/cache") != "" {
		t.Fatal("failed FormatPlan mounted /mnt/cache")
	}
}

func TestLabCreateArray_PersistsTopologyAndMountsByUUID(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	exec := disk.CommandRunner{}
	provider := &disk.LinuxProvider{Lister: disk.NewLister(), Exec: exec}

	parity := createLoopImage(ctx, t, exec, lab, "persist-parity", "320M")
	data := createLoopImage(ctx, t, exec, lab, "persist-data", "320M")
	cache := createLoopImage(ctx, t, exec, lab, "persist-cache", "320M")
	spare := createLoopImage(ctx, t, exec, lab, "persist-spare", "320M")
	loopSize := int64(320 << 20)

	t.Cleanup(func() {
		unmountIfMounted(exec, "/mnt/disk1")
		unmountIfMounted(exec, "/mnt/parity1")
		unmountIfMounted(exec, "/mnt/cache")
	})

	s := newTestScheduler(t)
	c := disk.AssignedDisk{Device: cache, Filesystem: disk.XFS}
	st, genRoot := labRegisterDiskFormat(t, s, provider, exec, disk.DirectMounter{Runner: exec})

	params := DiskFormatParams{
		Parity:       []disk.AssignedDisk{{Device: parity, Filesystem: disk.XFS}},
		Data:         []disk.AssignedDisk{{Device: data, Filesystem: disk.XFS}},
		Cache:        &c,
		Sizes:        map[string]int64{parity: loopSize, data: loopSize, cache: loopSize},
		CreatePolicy: "mfs",
		MinFreeSpace: "1G",
	}
	params.Confirmation = params.Plan().Confirmation()

	j, err := s.Submit(ctx, TypeDiskFormat, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}

	settings, disks, err := st.GetArray(ctx)
	if err != nil {
		t.Fatalf("GetArray: %v", err)
	}
	if settings.CreatePolicy != "mfs" || settings.MinFreeSpace != "1G" {
		t.Fatalf("settings = %+v, want mfs/1G", settings)
	}
	if len(disks) != 3 {
		t.Fatalf("len(disks) = %d, want 3: %+v", len(disks), disks)
	}

	parityUUID := blkidUUID(ctx, exec, parity)
	dataUUID := blkidUUID(ctx, exec, data)
	cacheUUID := blkidUUID(ctx, exec, cache)
	if parityUUID == "" || dataUUID == "" || cacheUUID == "" {
		t.Fatalf("missing filesystem UUID: parity=%q data=%q cache=%q", parityUUID, dataUUID, cacheUUID)
	}
	if got := findmntUUID(ctx, exec, "/mnt/parity1"); got != parityUUID {
		t.Fatalf("/mnt/parity1 UUID = %q, want %q", got, parityUUID)
	}
	if got := findmntUUID(ctx, exec, "/mnt/disk1"); got != dataUUID {
		t.Fatalf("/mnt/disk1 UUID = %q, want %q", got, dataUUID)
	}
	if got := findmntUUID(ctx, exec, "/mnt/cache"); got != cacheUUID {
		t.Fatalf("/mnt/cache UUID = %q, want %q", got, cacheUUID)
	}
	if got := findmntUUID(ctx, exec, spare); got != "" {
		t.Fatalf("unassigned %s is mounted", spare)
	}

	conf, err := os.ReadFile(filepath.Join(genRoot, "snapraid.conf"))
	if err != nil {
		t.Fatalf("reading snapraid.conf: %v", err)
	}
	got := string(conf)
	for _, want := range []string{"/mnt/parity1", "/mnt/disk1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("snapraid.conf missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, spare) {
		t.Fatalf("snapraid.conf names unassigned loop %s:\n%s", spare, got)
	}
	if strings.Contains(got, "/mnt/disk2") {
		t.Fatalf("snapraid.conf names a disk the plan did not assign:\n%s", got)
	}

	unit, err := os.ReadFile(filepath.Join(genRoot, "systemd", "system", "mnt-disk1.mount"))
	if err != nil {
		t.Fatalf("reading data disk unit: %v", err)
	}
	if !strings.Contains(string(unit), "What=/dev/disk/by-uuid/"+dataUUID) {
		t.Fatalf("data disk unit not bound to filesystem UUID %s:\n%s", dataUUID, unit)
	}
	if strings.Contains(string(unit), data) {
		t.Fatalf("data disk unit names the loop device path:\n%s", unit)
	}
}

// TestLabCreateArray_RetryAfterApplyFailureDoesNotReformat injects an
// apply failure after PutArray, then retries the same create-array plan
// against real loop devices: FormatPlan / mkfs must not run again, and
// the assigned loops must come up by the stored filesystem UUID with
// generated units and snapraid.conf matching the stored plan.
func TestLabCreateArray_RetryAfterApplyFailureDoesNotReformat(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	exec := &countingRunner{inner: disk.CommandRunner{}}
	provider := &disk.LinuxProvider{Lister: disk.NewLister(), Exec: exec}

	parity := createLoopImage(ctx, t, exec, lab, "retry-apply-parity", "320M")
	data := createLoopImage(ctx, t, exec, lab, "retry-apply-data", "320M")
	cache := createLoopImage(ctx, t, exec, lab, "retry-apply-cache", "320M")
	spare := createLoopImage(ctx, t, exec, lab, "retry-apply-spare", "320M")
	loopSize := int64(320 << 20)

	t.Cleanup(func() {
		unmountIfMounted(exec, "/mnt/disk1")
		unmountIfMounted(exec, "/mnt/parity1")
		unmountIfMounted(exec, "/mnt/cache")
	})

	mounter := &failOnceMounter{
		inner:   disk.DirectMounter{Runner: exec},
		failErr: errors.New("injected apply failure after persist"),
	}
	s := newTestScheduler(t)
	c := disk.AssignedDisk{Device: cache, Filesystem: disk.XFS}
	st, genRoot := labRegisterDiskFormat(t, s, provider, exec, mounter)

	params := DiskFormatParams{
		Parity: []disk.AssignedDisk{{Device: parity, Filesystem: disk.XFS}},
		Data:   []disk.AssignedDisk{{Device: data, Filesystem: disk.XFS}},
		Cache:  &c,
		Sizes:  map[string]int64{parity: loopSize, data: loopSize, cache: loopSize},
	}
	params.Confirmation = params.Plan().Confirmation()

	j, err := s.Submit(ctx, TypeDiskFormat, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("first job status = %s (%s), want failed", finished.Status, finished.ErrorMessage)
	}
	if !strings.Contains(finished.ErrorMessage, "injected apply failure after persist") {
		t.Fatalf("first job ErrorMessage = %q, want injected apply failure", finished.ErrorMessage)
	}
	exists, err := st.Exists(ctx)
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !exists {
		t.Fatal("first job did not persist topology before apply failed")
	}
	firstMkfs := exec.mkfs
	if firstMkfs != 3 {
		t.Fatalf("first job mkfs count = %d, want 3 assigned loops", firstMkfs)
	}
	parityUUID := blkidUUID(ctx, exec, parity)
	dataUUID := blkidUUID(ctx, exec, data)
	cacheUUID := blkidUUID(ctx, exec, cache)
	if parityUUID == "" || dataUUID == "" || cacheUUID == "" {
		t.Fatalf("missing filesystem UUID after first format: parity=%q data=%q cache=%q", parityUUID, dataUUID, cacheUUID)
	}

	j2, err := s.Submit(ctx, TypeDiskFormat, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit retry: %v", err)
	}
	retry := await(t, s, j2.ID)
	if retry.Status != StatusSucceeded {
		t.Fatalf("retry status = %s (%s), want succeeded", retry.Status, retry.ErrorMessage)
	}
	if exec.mkfs != firstMkfs {
		t.Fatalf("retry formatted assigned loops again: mkfs count = %d, want %d", exec.mkfs, firstMkfs)
	}
	if got := blkidUUID(ctx, exec, parity); got != parityUUID {
		t.Fatalf("parity UUID changed on retry: %q -> %q (second format)", parityUUID, got)
	}
	if got := blkidUUID(ctx, exec, data); got != dataUUID {
		t.Fatalf("data UUID changed on retry: %q -> %q (second format)", dataUUID, got)
	}
	if got := blkidUUID(ctx, exec, cache); got != cacheUUID {
		t.Fatalf("cache UUID changed on retry: %q -> %q (second format)", cacheUUID, got)
	}
	if got := blkidType(ctx, exec, spare); got != "" {
		t.Fatalf("unassigned %s gained a filesystem (%q)", spare, got)
	}

	_, disks, err := st.GetArray(ctx)
	if err != nil {
		t.Fatalf("GetArray: %v", err)
	}
	byRole := map[string]store.ArrayDisk{}
	for _, d := range disks {
		byRole[d.Role] = d
	}
	if byRole[store.ArrayRoleParity].FSUUID != parityUUID || byRole[store.ArrayRoleData].FSUUID != dataUUID || byRole[store.ArrayRoleCache].FSUUID != cacheUUID {
		t.Fatalf("stored UUIDs = %+v, want parity=%s data=%s cache=%s", byRole, parityUUID, dataUUID, cacheUUID)
	}
	if got := findmntUUID(ctx, exec, "/mnt/parity1"); got != parityUUID {
		t.Fatalf("/mnt/parity1 UUID = %q, want stored %q", got, parityUUID)
	}
	if got := findmntUUID(ctx, exec, "/mnt/disk1"); got != dataUUID {
		t.Fatalf("/mnt/disk1 UUID = %q, want stored %q", got, dataUUID)
	}
	if got := findmntUUID(ctx, exec, "/mnt/cache"); got != cacheUUID {
		t.Fatalf("/mnt/cache UUID = %q, want stored %q", got, cacheUUID)
	}

	conf, err := os.ReadFile(filepath.Join(genRoot, "snapraid.conf"))
	if err != nil {
		t.Fatalf("reading snapraid.conf: %v", err)
	}
	got := string(conf)
	for _, want := range []string{"/mnt/parity1", "/mnt/disk1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("snapraid.conf missing stored mount %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, spare) || strings.Contains(got, "/mnt/disk2") {
		t.Fatalf("snapraid.conf names a disk the stored plan did not assign:\n%s", got)
	}
	unit, err := os.ReadFile(filepath.Join(genRoot, "systemd", "system", "mnt-disk1.mount"))
	if err != nil {
		t.Fatalf("reading data disk unit: %v", err)
	}
	if !strings.Contains(string(unit), "What=/dev/disk/by-uuid/"+dataUUID) {
		t.Fatalf("data disk unit not bound to stored filesystem UUID %s:\n%s", dataUUID, unit)
	}
}

// TestLabLoopStillBacksImage_RefusesReusedDevice is this issue's own
// acceptance test (#383): a temp directory stands in for /sys/block, so
// this runs on any machine, not only inside the lab. A device number
// whose backing file no longer names this helper's own image — the
// shape left once a concurrently running lab reuses a freed loop
// number — is refused, never trusted from when it was first attached.
func TestLabLoopStillBacksImage_RefusesReusedDevice(t *testing.T) {
	root := t.TempDir()
	orig := loopOwnerSysfsRoot
	loopOwnerSysfsRoot = root
	t.Cleanup(func() { loopOwnerSysfsRoot = orig })

	ownImg := filepath.Join(t.TempDir(), "own.img")
	if err := os.WriteFile(ownImg, nil, 0o644); err != nil {
		t.Fatalf("writing %s: %v", ownImg, err)
	}
	otherImg := filepath.Join(t.TempDir(), "other-lab.img")
	if err := os.WriteFile(otherImg, nil, 0o644); err != nil {
		t.Fatalf("writing %s: %v", otherImg, err)
	}

	loopDir := filepath.Join(root, "loop7", "loop")
	if err := os.MkdirAll(loopDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", loopDir, err)
	}
	backingFile := filepath.Join(loopDir, "backing_file")

	if err := os.WriteFile(backingFile, []byte(ownImg+"\n"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", backingFile, err)
	}
	if !loopStillBacksImage(t, "/dev/loop7", ownImg) {
		t.Fatalf("loopStillBacksImage(own image) = false, want true")
	}

	if err := os.WriteFile(backingFile, []byte(otherImg+"\n"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", backingFile, err)
	}
	if loopStillBacksImage(t, "/dev/loop7", ownImg) {
		t.Fatalf("loopStillBacksImage(reused device) = true, want false — /dev/loop7 no longer backs this lab's image")
	}

	if err := os.RemoveAll(filepath.Join(root, "loop7", "loop")); err != nil {
		t.Fatalf("removing %s: %v", filepath.Join(root, "loop7", "loop"), err)
	}
	if loopStillBacksImage(t, "/dev/loop7", ownImg) {
		t.Fatalf("loopStillBacksImage(detached device) = true, want false — no loop/ subdirectory means not attached")
	}
}

// TestLabCreateLoopImage_CleanupSkipsDetachOfReusedDevice proves the
// t.Cleanup createLoopImage registers actually consults
// loopStillBacksImage before calling losetup -d, against a FakeRunner
// (CLAUDE.md's scriptable fake, internal/disk) rather than a real
// device: after the injected sysfs backing file is rewritten to name a
// different lab's image, the cleanup never issues losetup -d.
func TestLabCreateLoopImage_CleanupSkipsDetachOfReusedDevice(t *testing.T) {
	lab := t.TempDir()
	root := t.TempDir()
	orig := loopOwnerSysfsRoot
	loopOwnerSysfsRoot = root
	t.Cleanup(func() { loopOwnerSysfsRoot = orig })

	r := disk.NewFakeRunner()
	img := filepath.Join(lab, "img", "p383.img")
	dev := "/dev/loop9"
	r.Script("losetup", []string{"--find", "--show", img}, []byte(dev+"\n"), nil)
	r.Script("losetup", []string{"-j", img, "--output", "NAME", "--noheadings"}, []byte(dev+"\n"), nil)

	t.Run("attach", func(t *testing.T) {
		got := createLoopImage(context.Background(), t, r, lab, "p383", "320M")
		if got != dev {
			t.Fatalf("createLoopImage = %q, want %q", got, dev)
		}

		loopDir := filepath.Join(root, "loop9", "loop")
		if err := os.MkdirAll(loopDir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", loopDir, err)
		}
		// A concurrently running lab has since claimed this freed loop
		// number for its own image — the scenario #383 fixes.
		otherImg := filepath.Join(t.TempDir(), "other-lab.img")
		if err := os.WriteFile(filepath.Join(loopDir, "backing_file"), []byte(otherImg+"\n"), 0o644); err != nil {
			t.Fatalf("writing backing_file: %v", err)
		}
	})

	for _, c := range r.Calls() {
		if c.Name == "losetup" && len(c.Args) > 0 && c.Args[0] == "-d" {
			t.Fatalf("cleanup ran losetup -d %v against a device now backed by another lab's image", c.Args)
		}
	}
}
