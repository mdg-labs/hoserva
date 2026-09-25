package api_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/cache"
	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

func xfsAssignment(dev string, role apiv1.ArrayDiskRole) apiv1.ArrayDiskAssignment {
	a := apiv1.ArrayDiskAssignment{Device: dev, Role: role}
	a.SetFilesystem(apiv1.NewOptArrayDiskFilesystem(apiv1.ArrayDiskFilesystemXfs))
	return a
}

func createArrayReq(confirmation string, disks ...apiv1.ArrayDiskAssignment) *apiv1.CreateArrayRequest {
	return &apiv1.CreateArrayRequest{Disks: disks, Confirmation: confirmation}
}

func validErasePhrase(devs ...string) string {
	plan := disk.TopologyPlan{}
	if len(devs) > 0 {
		plan.Parity = []disk.AssignedDisk{{Device: devs[0], Filesystem: disk.XFS}}
	}
	for _, d := range devs[1:] {
		plan.Data = append(plan.Data, disk.AssignedDisk{Device: d, Filesystem: disk.XFS})
	}
	return plan.Confirmation()
}

func registerDiskFormat(t *testing.T, r *job.Registry, p disk.Provider) {
	t.Helper()
	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading migrations: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "array.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("opening array test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	fakeRun := disk.NewFakeRunner()
	for _, item := range []struct{ dev, uuid string }{
		{"/dev/sda", "uuid-sda"},
		{"/dev/sdb", "uuid-sdb"},
		{"/dev/sdc", "uuid-sdc"},
	} {
		fakeRun.Script("blkid", []string{"-s", "UUID", "-o", "value", item.dev}, []byte(item.uuid+"\n"), nil)
	}
	r.Register(job.TypeDiskFormat, false, job.RunDiskFormat(job.DiskFormatDeps{
		Provider:  p,
		Runner:    fakeRun,
		Store:     store.NewArrayStore(db),
		Generator: config.NewGenerator(t.TempDir()),
		Mounter:   disk.NewFakeMounter(),
		Now:       func() time.Time { return time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC) },
	}))
}

func newArrayHandler(t *testing.T) (*api.Handler, *job.Scheduler, *disk.FakeProvider) {
	t.Helper()
	h, s, r := newTestHandler(t)
	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	p.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB})
	p.AddDisk("/dev/sdc", disk.Disk{Size: 4 * disk.TB})
	h.Disks = p
	registerDiskFormat(t, r, p)
	return h, s, p
}

func TestHandler_CreateArray_WrongConfirmationFormatsNothing(t *testing.T) {
	ctx := context.Background()
	h, _, p := newArrayHandler(t)
	req := createArrayReq("erase /dev/sda, /dev/sdb",
		xfsAssignment("/dev/sda", apiv1.ArrayDiskRoleParity),
		xfsAssignment("/dev/sdb", apiv1.ArrayDiskRoleData),
	)
	_, err := h.CreateArray(ctx, req)
	status := apiError(t, h, err)
	if status.StatusCode != 409 || status.Response.Code != "confirmation_required" {
		t.Fatalf("CreateArray(wrong confirm) = %+v, want 409 confirmation_required", status)
	}
	if _, ok := p.FormattedAs("/dev/sda"); ok {
		t.Fatal("wrong confirmation formatted /dev/sda")
	}
	if _, ok := p.FormattedAs("/dev/sdb"); ok {
		t.Fatal("wrong confirmation formatted /dev/sdb")
	}
}

func TestHandler_CreateArray_MissingConfirmationFormatsNothing(t *testing.T) {
	ctx := context.Background()
	h, _, p := newArrayHandler(t)
	req := createArrayReq("",
		xfsAssignment("/dev/sda", apiv1.ArrayDiskRoleParity),
		xfsAssignment("/dev/sdb", apiv1.ArrayDiskRoleData),
	)
	_, err := h.CreateArray(ctx, req)
	status := apiError(t, h, err)
	if status.StatusCode != 409 || status.Response.Code != "confirmation_required" {
		t.Fatalf("CreateArray(missing confirm) = %+v, want 409 confirmation_required", status)
	}
	if _, ok := p.FormattedAs("/dev/sda"); ok {
		t.Fatal("missing confirmation formatted a disk")
	}
}

func TestHandler_CreateArray_UnmanagedPathFormatsNothing(t *testing.T) {
	ctx := context.Background()
	h, _, p := newArrayHandler(t)
	req := createArrayReq(validErasePhrase("/dev/sda", "/dev/sda1"),
		xfsAssignment("/dev/sda", apiv1.ArrayDiskRoleParity),
		xfsAssignment("/dev/sda1", apiv1.ArrayDiskRoleData),
	)
	_, err := h.CreateArray(ctx, req)
	status := apiError(t, h, err)
	if status.StatusCode != 400 || status.Response.Code != "unmanaged_device" {
		t.Fatalf("CreateArray(/dev/sda1) = %+v, want 400 unmanaged_device", status)
	}
	if _, ok := p.FormattedAs("/dev/sda"); ok {
		t.Fatal("unmanaged path formatted /dev/sda")
	}
	if _, ok := p.FormattedAs("/dev/sda1"); ok {
		t.Fatal("unmanaged path formatted /dev/sda1")
	}
}

func TestHandler_CreateArray_ValidateFailuresFormatNothing(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		req  *apiv1.CreateArrayRequest
		want string
	}{
		{
			name: "no parity",
			req: createArrayReq("ERASE /dev/sdb",
				xfsAssignment("/dev/sdb", apiv1.ArrayDiskRoleData),
			),
			want: "at least one parity",
		},
		{
			name: "parity not xfs",
			req: func() *apiv1.CreateArrayRequest {
				parity := apiv1.ArrayDiskAssignment{Device: "/dev/sda", Role: apiv1.ArrayDiskRoleParity}
				parity.SetFilesystem(apiv1.NewOptArrayDiskFilesystem(apiv1.ArrayDiskFilesystemExt4))
				return createArrayReq("ERASE /dev/sda, /dev/sdb",
					parity,
					xfsAssignment("/dev/sdb", apiv1.ArrayDiskRoleData),
				)
			}(),
			want: "always formatted XFS",
		},
		{
			name: "parity too small",
			req: createArrayReq("ERASE /dev/sda, /dev/sdb",
				xfsAssignment("/dev/sdb", apiv1.ArrayDiskRoleParity),
				xfsAssignment("/dev/sda", apiv1.ArrayDiskRoleData),
			),
			want: "at least as large",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, p := newArrayHandler(t)
			_, err := h.CreateArray(ctx, tc.req)
			status := apiError(t, h, err)
			if status.StatusCode != 400 || status.Response.Code != "invalid_plan" {
				t.Fatalf("CreateArray(%s) = %+v, want 400 invalid_plan", tc.name, status)
			}
			if !strings.Contains(status.Response.Message, tc.want) {
				t.Fatalf("message = %q, want substring %q", status.Response.Message, tc.want)
			}
			if _, ok := p.FormattedAs("/dev/sda"); ok {
				t.Fatalf("%s formatted /dev/sda", tc.name)
			}
			if _, ok := p.FormattedAs("/dev/sdb"); ok {
				t.Fatalf("%s formatted /dev/sdb", tc.name)
			}
		})
	}
}

func TestHandler_CreateArray_WeakIdentityParityFormatsNothing(t *testing.T) {
	ctx := context.Background()
	h, s, r := newTestHandler(t)
	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB, WeakIdentity: true})
	p.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB})
	h.Disks = p
	registerDiskFormat(t, r, p)

	req := createArrayReq("ERASE /dev/sda, /dev/sdb",
		xfsAssignment("/dev/sda", apiv1.ArrayDiskRoleParity),
		xfsAssignment("/dev/sdb", apiv1.ArrayDiskRoleData),
	)
	_, err := h.CreateArray(ctx, req)
	status := apiError(t, h, err)
	if status.StatusCode != 400 || status.Response.Code != "invalid_plan" {
		t.Fatalf("CreateArray(weak parity) = %+v, want 400 invalid_plan", status)
	}
	if !strings.Contains(status.Response.Message, "weak-identity") {
		t.Fatalf("message = %q, want weak-identity", status.Response.Message)
	}
	if _, ok := p.FormattedAs("/dev/sda"); ok {
		t.Fatal("weak-identity parity formatted /dev/sda")
	}
	_ = s
}

func TestHandler_CreateArray_SuccessReturnsTopologyJob(t *testing.T) {
	ctx := context.Background()
	h, s, p := newArrayHandler(t)
	req := createArrayReq(validErasePhrase("/dev/sda", "/dev/sdb", "/dev/sdc"),
		xfsAssignment("/dev/sda", apiv1.ArrayDiskRoleParity),
		xfsAssignment("/dev/sdb", apiv1.ArrayDiskRoleData),
		xfsAssignment("/dev/sdc", apiv1.ArrayDiskRoleCache),
	)
	got, err := h.CreateArray(ctx, req)
	if err != nil {
		t.Fatalf("CreateArray: %v", err)
	}
	if got.Type != apiv1.JobTypeDiskFormat || got.Class != apiv1.JobClassTopology {
		t.Fatalf("job type/class = %s/%s, want disk_format/topology", got.Type, got.Class)
	}
	finished := awaitJob(t, s, got.ID.String())
	if finished.Status != job.StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	if fs, ok := p.FormattedAs("/dev/sda"); !ok || fs != disk.XFS {
		t.Fatalf("FormattedAs(/dev/sda) = (%v, %v), want xfs", fs, ok)
	}
	if fs, ok := p.FormattedAs("/dev/sdb"); !ok || fs != disk.XFS {
		t.Fatalf("FormattedAs(/dev/sdb) = (%v, %v), want xfs", fs, ok)
	}
	if fs, ok := p.FormattedAs("/dev/sdc"); !ok || fs != disk.XFS {
		t.Fatalf("FormattedAs(/dev/sdc) = (%v, %v), want xfs", fs, ok)
	}
}

func TestHandler_ListDisks_DiscoveryFieldsAndStandbySMART(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newTestHandler(t)
	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sdb", disk.Disk{
		Size: 4 * disk.TB, Filesystem: "xfs", Label: "disk1",
		ContainsData: true, LooksLikeUnraid: true,
	})
	p.AddDisk("/dev/sdc", disk.Disk{Size: 4 * disk.TB})
	p.SetSpinState("/dev/sdb", disk.Standby)
	p.SetSMART("/dev/sdc", disk.SMARTReport{SelfTestFailed: true})
	h.Disks = p

	got, err := h.ListDisks(ctx)
	if err != nil {
		t.Fatalf("ListDisks: %v", err)
	}
	if len(got.Disks) != 2 {
		t.Fatalf("len = %d, want 2", len(got.Disks))
	}
	sdb := got.Disks[0]
	if sdb.Filesystem.Or("") != "xfs" || sdb.Label.Or("") != "disk1" {
		t.Fatalf("sdb filesystem/label = %q/%q", sdb.Filesystem.Or(""), sdb.Label.Or(""))
	}
	if !sdb.ContainsData.Or(false) || !sdb.LooksLikeUnraid.Or(false) {
		t.Fatal("sdb missing containsData/looksLikeUnraid")
	}
	if sdb.SmartStatus.Or("") != "standby" {
		t.Fatalf("sdb smartStatus = %q, want standby", sdb.SmartStatus.Or(""))
	}
	sdc := got.Disks[1]
	if sdc.SmartStatus.Or("") != "failing" {
		t.Fatalf("sdc smartStatus = %q, want failing", sdc.SmartStatus.Or(""))
	}
	for _, call := range p.SMARTCalls() {
		if call.Mode != disk.SMARTPollRespectStandby {
			t.Fatalf("SMART mode = %v, want RespectStandby", call.Mode)
		}
		if call.Woke {
			t.Fatalf("SMART(%s) woke a standby disk", call.Device)
		}
	}
}

// seqLogService / seqLogMount are ArraySequence fakes for the API-layer
// Q70 tests: they record Stop/Start/Mount/Unmount into a shared log so a
// failed service stop can be shown not to have been skipped past on the
// way to unmounting storage (doc 02 §4).
type seqLogService struct {
	name     string
	stopErr  error
	startErr error
	log      *[]string
}

func (f *seqLogService) Name() string { return f.name }

func (f *seqLogService) Stop(ctx context.Context) error {
	*f.log = append(*f.log, "stop:"+f.name)
	return f.stopErr
}

func (f *seqLogService) Start(ctx context.Context) error {
	*f.log = append(*f.log, "start:"+f.name)
	return f.startErr
}

type seqLogMount struct {
	where      string
	mountErr   error
	unmountErr error
	log        *[]string
}

func (f *seqLogMount) Where() string { return f.where }

func (f *seqLogMount) Mount(ctx context.Context) error {
	*f.log = append(*f.log, "mount:"+f.where)
	return f.mountErr
}

func (f *seqLogMount) Unmount(ctx context.Context) error {
	*f.log = append(*f.log, "unmount:"+f.where)
	return f.unmountErr
}

type seqReadinessGate struct{ ready bool }

func (g seqReadinessGate) Ready() bool { return g.ready }

func seqEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v (differs at index %d)", got, want, i)
		}
	}
}

func attachArraySequence(h *api.Handler, s *job.Scheduler, seq job.ArraySequence) {
	seq.Scheduler = s
	h.SetArray(&seq)
}

func confirmStop() *apiv1.StopArrayRequest {
	return &apiv1.StopArrayRequest{Confirm: true}
}

// TestHandler_StopArray_ServiceMustStopBeforeAnyUnmount is this issue's
// central data-loss-prevention test (doc 02 §4, Q70, safety-critical): the
// handler must call job.ArraySequence, not a second unmount order. A
// container that will not stop — still holding a file open on the pool is
// the literal doc 02 §4 example — must hold the whole stop, not be skipped
// past on the way to unmounting storage under it. Unmounting while that
// write is still in flight would lose it or land it on the now-empty
// directory that stands in for the mount.
func TestHandler_StopArray_ServiceMustStopBeforeAnyUnmount(t *testing.T) {
	ctx := context.Background()
	h, s, _ := newTestHandler(t)

	var log []string
	container := &seqLogService{name: "container", stopErr: errors.New("container still holds /mnt/user/media/movie.mkv open"), log: &log}
	share := &seqLogMount{where: "/mnt/user/media", log: &log}
	catchAll := &seqLogMount{where: "/mnt/user", log: &log}
	disk1 := &seqLogMount{where: "/mnt/disk1", log: &log}
	attachArraySequence(h, s, job.ArraySequence{
		Services:    []job.ArrayService{container},
		ShareMounts: []job.ArrayMount{share},
		CatchAll:    catchAll,
		Disks:       []job.ArrayMount{disk1},
	})

	_, err := h.StopArray(ctx, confirmStop())
	status := apiError(t, h, err)
	if status.StatusCode != 409 || status.Response.Code != "array_stop_failed" {
		t.Fatalf("StopArray = %+v, want 409 array_stop_failed", status)
	}
	for _, m := range log {
		if strings.HasPrefix(m, "unmount:") {
			t.Fatalf("StopArray unmounted %q after the container failed to stop — this is the exact data-loss scenario doc 02 §4 exists to prevent; full log: %v", m, log)
		}
	}
	seqEqual(t, log, []string{"stop:container"})
	if !s.InMaintenance() {
		t.Fatal("StopArray: scheduler must stay in maintenance mode after a failed stop")
	}
	got, err := h.GetStatus(ctx)
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if !got.MaintenanceMode.Or(false) {
		t.Fatal("GetStatus: maintenanceMode must stay true after a failed stop")
	}
}

func TestHandler_StopArray_MissingConfirmUnmountsNothing(t *testing.T) {
	ctx := context.Background()
	h, s, _ := newTestHandler(t)

	var log []string
	svc := &seqLogService{name: "container", log: &log}
	disk1 := &seqLogMount{where: "/mnt/disk1", log: &log}
	attachArraySequence(h, s, job.ArraySequence{
		Services: []job.ArrayService{svc},
		Disks:    []job.ArrayMount{disk1},
	})

	_, err := h.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: false})
	status := apiError(t, h, err)
	if status.StatusCode != 409 || status.Response.Code != "confirmation_required" {
		t.Fatalf("StopArray(confirm=false) = %+v, want 409 confirmation_required", status)
	}
	if len(log) != 0 {
		t.Fatalf("missing confirm touched the sequence: %v", log)
	}
	if s.InMaintenance() {
		t.Fatal("StopArray(confirm=false) entered maintenance mode")
	}
}

func TestHandler_StopArray_UnmountsInOrderAndEntersMaintenance(t *testing.T) {
	ctx := context.Background()
	h, s, _ := newTestHandler(t)

	var log []string
	vm := &seqLogService{name: "vm", log: &log}
	container := &seqLogService{name: "container", log: &log}
	share := &seqLogMount{where: "/mnt/user/media", log: &log}
	catchAll := &seqLogMount{where: "/mnt/user", log: &log}
	disk1 := &seqLogMount{where: "/mnt/disk1", log: &log}
	attachArraySequence(h, s, job.ArraySequence{
		Services:    []job.ArrayService{vm, container},
		ShareMounts: []job.ArrayMount{share},
		CatchAll:    catchAll,
		Disks:       []job.ArrayMount{disk1},
	})

	got, err := h.StopArray(ctx, confirmStop())
	if err != nil {
		t.Fatalf("StopArray: %v", err)
	}
	seqEqual(t, log, []string{
		"stop:vm", "stop:container",
		"unmount:/mnt/user/media",
		"unmount:/mnt/user",
		"unmount:/mnt/disk1",
	})
	if !got.MaintenanceMode.Or(false) {
		t.Fatal("StopArray: maintenanceMode must be true after a successful stop")
	}
	if !s.InMaintenance() {
		t.Fatal("StopArray: scheduler must be in maintenance mode after a successful stop")
	}
}

func TestHandler_StartArray_ReversesAndExitsMaintenance(t *testing.T) {
	ctx := context.Background()
	h, s, _ := newTestHandler(t)
	if err := s.EnterMaintenance(ctx); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}

	var log []string
	vm := &seqLogService{name: "vm", log: &log}
	container := &seqLogService{name: "container", log: &log}
	share := &seqLogMount{where: "/mnt/user/media", log: &log}
	catchAll := &seqLogMount{where: "/mnt/user", log: &log}
	disk1 := &seqLogMount{where: "/mnt/disk1", log: &log}
	attachArraySequence(h, s, job.ArraySequence{
		Services:    []job.ArrayService{vm, container},
		ShareMounts: []job.ArrayMount{share},
		CatchAll:    catchAll,
		Disks:       []job.ArrayMount{disk1},
	})

	got, err := h.StartArray(ctx)
	if err != nil {
		t.Fatalf("StartArray: %v", err)
	}
	seqEqual(t, log, []string{
		"mount:/mnt/disk1",
		"mount:/mnt/user",
		"mount:/mnt/user/media",
		"start:container", "start:vm",
	})
	if got.MaintenanceMode.Or(false) {
		t.Fatal("StartArray: maintenanceMode must be false after a successful start")
	}
	if s.InMaintenance() {
		t.Fatal("StartArray: scheduler must leave maintenance mode after a successful start")
	}
}

func TestHandler_StartArray_RefusesWhenGateIsNotReady(t *testing.T) {
	ctx := context.Background()
	h, s, _ := newTestHandler(t)

	var log []string
	disk1 := &seqLogMount{where: "/mnt/disk1", log: &log}
	attachArraySequence(h, s, job.ArraySequence{
		Gate:  seqReadinessGate{ready: false},
		Disks: []job.ArrayMount{disk1},
	})

	_, err := h.StartArray(ctx)
	status := apiError(t, h, err)
	if status.StatusCode != 409 || status.Response.Code != "storage_not_ready" {
		t.Fatalf("StartArray = %+v, want 409 storage_not_ready", status)
	}
	if !strings.Contains(status.Response.Message, job.ErrStorageNotReady.Error()) {
		t.Fatalf("StartArray message = %q, want %q", status.Response.Message, job.ErrStorageNotReady)
	}
	if len(log) != 0 {
		t.Fatalf("StartArray mounted %v while the gate reported not ready", log)
	}
}

// TestHandler_ArrayStartAndStop_RefusedWhileDiskUpgradeDataIsPending:
// doc 02 §4 E6 and E7 through the handler — while a data-disk upgrade is
// pending, StartArray and StopArray are refused with disk_upgrade_pending
// and nothing is mounted, unmounted or stopped.
func TestHandler_ArrayStartAndStop_RefusedWhileDiskUpgradeDataIsPending(t *testing.T) {
	ctx := context.Background()
	h, s, _ := newTestHandler(t)
	if err := s.EnterMaintenance(ctx); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}
	if err := h.Store.Create(ctx, &job.Job{
		ID: "33333333-3333-3333-3333-333333333333", Type: job.TypeDiskUpgradeData, Class: job.ClassTopology,
		Status: job.StatusInterrupted, Resumable: true, Cancellable: true, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	var log []string
	disk1 := &seqLogMount{where: "/mnt/disk1", log: &log}
	svc := &seqLogService{name: "samba", log: &log}
	attachArraySequence(h, s, job.ArraySequence{Services: []job.ArrayService{svc}, Disks: []job.ArrayMount{disk1}})

	_, err := h.StartArray(ctx)
	status := apiError(t, h, err)
	if status.StatusCode != 409 || status.Response.Code != "disk_upgrade_pending" || !strings.Contains(status.Response.Message, "33333333") {
		t.Fatalf("StartArray = %+v, want 409 disk_upgrade_pending naming the job", status)
	}
	_, err = h.StopArray(ctx, confirmStop())
	status = apiError(t, h, err)
	if status.StatusCode != 409 || status.Response.Code != "disk_upgrade_pending" {
		t.Fatalf("StopArray = %+v, want 409 disk_upgrade_pending", status)
	}
	if len(log) != 0 {
		t.Fatalf("array start/stop acted while a data-disk upgrade was pending: %v", log)
	}
}

func TestHandler_StartArray_DoesNotExitMaintenanceOnFailure(t *testing.T) {
	ctx := context.Background()
	h, s, _ := newTestHandler(t)
	if err := s.EnterMaintenance(ctx); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}

	var log []string
	disk1 := &seqLogMount{where: "/mnt/disk1", mountErr: errors.New("device timeout"), log: &log}
	attachArraySequence(h, s, job.ArraySequence{
		Disks: []job.ArrayMount{disk1},
	})

	_, err := h.StartArray(ctx)
	status := apiError(t, h, err)
	if status.StatusCode != 409 || status.Response.Code != "array_start_failed" {
		t.Fatalf("StartArray = %+v, want 409 array_start_failed", status)
	}
	if !s.InMaintenance() {
		t.Fatal("StartArray: maintenance mode must stay active when a mount step fails")
	}
	got, err := h.GetStatus(ctx)
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if !got.MaintenanceMode.Or(false) {
		t.Fatal("GetStatus: maintenanceMode must stay true after a failed start")
	}
}

// TestHandler_SetArray_ConcurrentWithStopStartIsRaceFree is #263's own
// concurrency regression: a live array creation (#262) calls SetArray from
// the create-array job's own goroutine while StopArray/StartArray (and
// cmd/hoservad's own UPS/update shutdown lookups) read the same value from
// concurrent HTTP request goroutines. Before this fix, Handler.Array was a
// bare, unsynchronized pointer field written and read directly by both
// sides — `go test -race` catches that unsynchronized concurrent access
// here; CurrentArray/SetArray's own arrayMu is what makes this clean.
func TestHandler_SetArray_ConcurrentWithStopStartIsRaceFree(t *testing.T) {
	ctx := context.Background()
	h, s, _ := newTestHandler(t)

	var wg sync.WaitGroup
	const iterations = 200

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			seq := job.ArraySequence{Scheduler: s}
			h.SetArray(&seq)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_, _ = h.StopArray(ctx, confirmStop())
			_, _ = h.StartArray(ctx)
			_ = h.CurrentArray()
		}
	}()

	wg.Wait()
}

// newDiskRemoveTestHandler wires a *api.Handler to a real SQLite array
// (two data disks) and job.Scheduler, with a parity engine set and
// job.TypeDiskRemove registered to a RunFunc that only records the params
// it was queued with — these tests are about finishDiskRemoval's own
// refusals and what it submits; internal/job's own tests and the lab
// cover the job. withParity false leaves the engine unset.
func newDiskRemoveTestHandler(t *testing.T, withParity bool) (h *api.Handler, disk1, disk2 string, queued chan []byte) {
	t.Helper()
	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "disk-remove-api-test.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, _, err := (&store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}).Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	disk1, disk2 = "/mnt/disk1", "/mnt/disk2"
	arrayStore := store.NewArrayStore(db)
	if err := arrayStore.PutArray(context.Background(), store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "10G", CreatedAt: time.Now().UTC()}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/loop9", Filesystem: "xfs", FSUUID: "uuid-p1", Mountpoint: "/mnt/parity1"},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/loop0", Filesystem: "xfs", FSUUID: "uuid-d1", Mountpoint: disk1},
		{Role: store.ArrayRoleData, RoleIndex: 2, Device: "/dev/loop1", Filesystem: "xfs", FSUUID: "uuid-d2", Mountpoint: disk2},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	jobStore := job.NewStore(db)
	registry := job.NewRegistry()
	queued = make(chan []byte, 4)
	registry.Register(job.TypeDiskRemove, false, func(ctx context.Context, rc *job.RunContext) error {
		queued <- rc.Params()
		return nil
	})
	h = &api.Handler{
		Scheduler:  job.NewScheduler(jobStore, job.NewLogStore(t.TempDir()), job.NewHub(), registry),
		Store:      jobStore,
		ArrayStore: arrayStore,
	}
	if withParity {
		h.SetParity(parity.NewFakeEngine(), parity.Guard{}, nil, func(context.Context) ([]cache.Share, error) { return nil, nil })
	}
	return h, disk1, disk2, queued
}

// TestHandler_FinishDiskRemoval_RefusalsInOrder is the acceptance
// criterion's status mapping: 501 without a parity engine, 404 for a slot
// with no data disk, 409 disk_not_evacuated for a disk not evacuated or
// further along, 409 confirmation_required — and no job for any of them.
func TestHandler_FinishDiskRemoval_RefusalsInOrder(t *testing.T) {
	ctx := context.Background()

	unconfigured, _, _, _ := newDiskRemoveTestHandler(t, false)
	if _, err := unconfigured.FinishDiskRemoval(ctx, &apiv1.FinishDiskRemovalRequest{Mountpoint: "/mnt/disk1", Confirmation: "REMOVE /mnt/disk1"}); err == nil {
		t.Fatal("FinishDiskRemoval with no parity engine = nil, want 501")
	} else if status := apiError(t, unconfigured, err); status.StatusCode != 501 || status.Response.Code != "not_configured" {
		t.Fatalf("FinishDiskRemoval with no parity engine = %+v, want 501 not_configured", status)
	}

	h, disk1, disk2, queued := newDiskRemoveTestHandler(t, true)
	if err := h.ArrayStore.SetRemovalState(ctx, disk2, store.RemovalStateEvacuating, "evac-1"); err != nil {
		t.Fatalf("SetRemovalState: %v", err)
	}
	for _, tc := range []struct {
		name       string
		mountpoint string
		confirm    string
		status     int
		code       string
	}{
		{name: "unknown slot", mountpoint: "/mnt/disk9", confirm: "REMOVE /mnt/disk9", status: 404, code: "disk_slot_not_found"},
		{name: "not in removal, wrong confirmation too", mountpoint: disk1, confirm: "nope", status: 409, code: "disk_not_evacuated"},
		{name: "still evacuating", mountpoint: disk2, confirm: job.EvacuationConfirmation(disk2), status: 409, code: "disk_not_evacuated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.FinishDiskRemoval(ctx, &apiv1.FinishDiskRemovalRequest{Mountpoint: tc.mountpoint, Confirmation: tc.confirm})
			if status := apiError(t, h, err); status.StatusCode != tc.status || status.Response.Code != tc.code {
				t.Fatalf("FinishDiskRemoval = %+v, want %d %s", status, tc.status, tc.code)
			}
		})
	}

	if err := h.ArrayStore.SetRemovalState(ctx, disk2, store.RemovalStateEvacuated, "evac-1"); err != nil {
		t.Fatalf("SetRemovalState(evacuated): %v", err)
	}
	for _, confirm := range []string{"", "REMOVE " + disk1, "nope"} {
		_, err := h.FinishDiskRemoval(ctx, &apiv1.FinishDiskRemovalRequest{Mountpoint: disk2, Confirmation: confirm})
		if status := apiError(t, h, err); status.StatusCode != 409 || status.Response.Code != "confirmation_required" {
			t.Fatalf("FinishDiskRemoval(confirmation %q) = %+v, want 409 confirmation_required", confirm, status)
		}
	}
	jobs, err := h.Store.List(ctx, job.ListFilter{})
	if err != nil {
		t.Fatalf("listing jobs: %v", err)
	}
	if len(jobs) != 0 || len(queued) != 0 {
		t.Fatalf("a refused finishDiskRemoval submitted %d jobs", len(jobs))
	}
}

// TestHandler_FinishDiskRemoval_QueuesTheJob proves an evacuated disk,
// and one a failed earlier run left unpooled or unlisted, each queue a
// disk_remove job carrying the slot and the confirmation.
func TestHandler_FinishDiskRemoval_QueuesTheJob(t *testing.T) {
	ctx := context.Background()
	h, _, disk2, queued := newDiskRemoveTestHandler(t, true)
	if err := h.ArrayStore.SetRemovalState(ctx, disk2, store.RemovalStateEvacuated, "evac-1"); err != nil {
		t.Fatalf("SetRemovalState: %v", err)
	}
	steps := []struct{ from, to string }{
		{"", ""},
		{store.RemovalStateEvacuated, store.RemovalStateUnpooled},
		{store.RemovalStateUnpooled, store.RemovalStateUnlisted},
	}
	for _, step := range steps {
		if step.to != "" {
			if err := h.ArrayStore.AdvanceRemovalState(ctx, disk2, step.from, step.to, "rm-1"); err != nil {
				t.Fatalf("AdvanceRemovalState(%s): %v", step.to, err)
			}
		}
		j, err := h.FinishDiskRemoval(ctx, &apiv1.FinishDiskRemovalRequest{Mountpoint: disk2, Confirmation: job.EvacuationConfirmation(disk2)})
		if err != nil {
			t.Fatalf("FinishDiskRemoval (%q): %v", step.to, err)
		}
		if j.Type != apiv1.JobTypeDiskRemove || j.Class != apiv1.JobClassTopology {
			t.Fatalf("queued job = %s/%s, want disk_remove/topology", j.Type, j.Class)
		}
		waitForStatus(t, h.Store, j.ID.String(), job.StatusSucceeded)
		params := <-queued
		if want := `{"mountpoint":"` + disk2 + `","confirmation":"` + job.EvacuationConfirmation(disk2) + `"}`; string(params) != want {
			t.Fatalf("params = %s, want %s", params, want)
		}
	}
}

// TestHandler_FinishDiskRemoval_StoreFailureIsInternal proves an I/O
// failure reading the array is an opaque 500, never a 4xx that would tell
// the caller the request was wrong.
func TestHandler_FinishDiskRemoval_StoreFailureIsInternal(t *testing.T) {
	ctx := context.Background()
	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading migrations: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "closed.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	if _, _, err := (&store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}).Apply(ctx); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	h, _, _, _ := newDiskRemoveTestHandler(t, true)
	h.ArrayStore = store.NewArrayStore(db)
	_ = db.Close()

	_, err = h.FinishDiskRemoval(ctx, &apiv1.FinishDiskRemovalRequest{Mountpoint: "/mnt/disk2", Confirmation: "REMOVE /mnt/disk2"})
	if status := apiError(t, h, err); status.StatusCode != 500 || status.Response.Code != "internal" {
		t.Fatalf("FinishDiskRemoval with a failing store = %+v, want an opaque 500", status)
	}
}

// fakeArrayReadyHook mirrors internal/job's own scriptable ArrayReady
// dependency (evacuation_run_test.go's doc comment) at the Handler level:
// CancelDiskRemoval (#361) is the one handler method that calls
// Handler.ArrayReady directly, outside any job, so a test needs the same
// call-counting, scriptable-error fake to prove the disk's branches are
// re-applied RW exactly when — and only when — a cancel actually clears
// the removal state.
type fakeArrayReadyHook struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (f *fakeArrayReadyHook) hook(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.err
}

func (f *fakeArrayReadyHook) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// TestHandler_CancelDiskRemoval_RefusalsInOrder is the acceptance
// criterion's status mapping: 501 with ArrayReady not wired, 404 for a
// slot with no data disk, 409 disk_removal_not_cancellable for a disk not
// currently in removal, one already unpooled or unlisted (#361's Open
// questions default: only finishDiskRemoval takes those further), and one
// evacuating whose own evacuation job is still queued — and ArrayReady is
// never called for any of them.
func TestHandler_CancelDiskRemoval_RefusalsInOrder(t *testing.T) {
	ctx := context.Background()

	unconfigured, _, _, _ := newDiskRemoveTestHandler(t, true)
	unconfigured.ArrayReady = nil
	if err := unconfigured.CancelDiskRemoval(ctx, &apiv1.CancelDiskRemovalRequest{Mountpoint: "/mnt/disk1"}); err == nil {
		t.Fatal("CancelDiskRemoval with no ArrayReady wired = nil, want 501")
	} else if status := apiError(t, unconfigured, err); status.StatusCode != 501 || status.Response.Code != "not_configured" {
		t.Fatalf("CancelDiskRemoval with no ArrayReady wired = %+v, want 501 not_configured", status)
	}

	h, disk1, disk2, _ := newDiskRemoveTestHandler(t, true)
	arrayReady := &fakeArrayReadyHook{}
	h.ArrayReady = arrayReady.hook

	if err := h.CancelDiskRemoval(ctx, &apiv1.CancelDiskRemovalRequest{Mountpoint: "/mnt/disk9"}); err == nil {
		t.Fatal("CancelDiskRemoval(unknown slot) = nil, want 404")
	} else if status := apiError(t, h, err); status.StatusCode != 404 || status.Response.Code != "disk_slot_not_found" {
		t.Fatalf("CancelDiskRemoval(unknown slot) = %+v, want 404 disk_slot_not_found", status)
	}

	if err := h.CancelDiskRemoval(ctx, &apiv1.CancelDiskRemovalRequest{Mountpoint: disk1}); err == nil {
		t.Fatal("CancelDiskRemoval(not in removal) = nil, want 409")
	} else if status := apiError(t, h, err); status.StatusCode != 409 || status.Response.Code != "disk_removal_not_cancellable" {
		t.Fatalf("CancelDiskRemoval(not in removal) = %+v, want 409 disk_removal_not_cancellable", status)
	}

	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, h *api.Handler)
	}{
		{
			name: "unpooled",
			setup: func(t *testing.T, h *api.Handler) {
				if err := h.ArrayStore.SetRemovalState(ctx, disk2, store.RemovalStateEvacuating, "evac-1"); err != nil {
					t.Fatalf("SetRemovalState(evacuating): %v", err)
				}
				if err := h.ArrayStore.AdvanceRemovalState(ctx, disk2, store.RemovalStateEvacuating, store.RemovalStateUnpooled, "evac-1"); err != nil {
					t.Fatalf("AdvanceRemovalState(unpooled): %v", err)
				}
			},
		},
		{
			name: "unlisted",
			setup: func(t *testing.T, h *api.Handler) {
				if err := h.ArrayStore.SetRemovalState(ctx, disk2, store.RemovalStateEvacuating, "evac-1"); err != nil {
					t.Fatalf("SetRemovalState(evacuating): %v", err)
				}
				if err := h.ArrayStore.AdvanceRemovalState(ctx, disk2, store.RemovalStateEvacuating, store.RemovalStateEvacuated, "evac-1"); err != nil {
					t.Fatalf("AdvanceRemovalState(evacuated): %v", err)
				}
				if err := h.ArrayStore.AdvanceRemovalState(ctx, disk2, store.RemovalStateEvacuated, store.RemovalStateUnlisted, "evac-1"); err != nil {
					t.Fatalf("AdvanceRemovalState(unlisted): %v", err)
				}
			},
		},
		{
			name: "evacuating with a still-queued evacuation job",
			setup: func(t *testing.T, h *api.Handler) {
				jobID := uuid.NewString()
				if err := h.Store.Create(ctx, &job.Job{ID: jobID, Type: job.TypeEvacuation, Class: job.ClassArrayWrite, Status: job.StatusQueued, CreatedAt: time.Now().UTC()}); err != nil {
					t.Fatalf("seeding a pending evacuation job: %v", err)
				}
				if err := h.ArrayStore.SetRemovalState(ctx, disk2, store.RemovalStateEvacuating, jobID); err != nil {
					t.Fatalf("SetRemovalState(evacuating): %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, disk2, _ := newDiskRemoveTestHandler(t, true)
			arrayReady := &fakeArrayReadyHook{}
			h.ArrayReady = arrayReady.hook
			tc.setup(t, h)

			err := h.CancelDiskRemoval(ctx, &apiv1.CancelDiskRemovalRequest{Mountpoint: disk2})
			if status := apiError(t, h, err); status.StatusCode != 409 || status.Response.Code != "disk_removal_not_cancellable" {
				t.Fatalf("CancelDiskRemoval(%s) = %+v, want 409 disk_removal_not_cancellable", tc.name, status)
			}
			if arrayReady.callCount() != 0 {
				t.Fatalf("CancelDiskRemoval(%s): ArrayReady ran %d time(s) on a refusal", tc.name, arrayReady.callCount())
			}
		})
	}
}

// TestHandler_CancelDiskRemoval_ClearsStateAndReappliesPool proves a
// successful cancel — for an evacuated disk, and for an evacuating one
// whose own job has already ended (succeeded/failed/cancelled) or whose
// job record the store no longer has — clears the disk's removal state
// back to "" and re-applies the pool live exactly once, so its branches
// go back to RW (#361).
func TestHandler_CancelDiskRemoval_ClearsStateAndReappliesPool(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, h *api.Handler, mountpoint string)
	}{
		{
			name: "evacuated",
			setup: func(t *testing.T, h *api.Handler, mountpoint string) {
				if err := h.ArrayStore.SetRemovalState(ctx, mountpoint, store.RemovalStateEvacuated, "evac-1"); err != nil {
					t.Fatalf("SetRemovalState(evacuated): %v", err)
				}
			},
		},
		{
			name: "evacuating, its job already succeeded",
			setup: func(t *testing.T, h *api.Handler, mountpoint string) {
				jobID := uuid.NewString()
				if err := h.Store.Create(ctx, &job.Job{ID: jobID, Type: job.TypeEvacuation, Class: job.ClassArrayWrite, Status: job.StatusSucceeded, CreatedAt: time.Now().UTC()}); err != nil {
					t.Fatalf("seeding an ended evacuation job: %v", err)
				}
				if err := h.ArrayStore.SetRemovalState(ctx, mountpoint, store.RemovalStateEvacuating, jobID); err != nil {
					t.Fatalf("SetRemovalState(evacuating): %v", err)
				}
			},
		},
		{
			name: "evacuating, no job record at all",
			setup: func(t *testing.T, h *api.Handler, mountpoint string) {
				if err := h.ArrayStore.SetRemovalState(ctx, mountpoint, store.RemovalStateEvacuating, "a-job-id-the-store-never-saw"); err != nil {
					t.Fatalf("SetRemovalState(evacuating): %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, disk2, _ := newDiskRemoveTestHandler(t, true)
			arrayReady := &fakeArrayReadyHook{}
			h.ArrayReady = arrayReady.hook
			tc.setup(t, h, disk2)

			if err := h.CancelDiskRemoval(ctx, &apiv1.CancelDiskRemovalRequest{Mountpoint: disk2}); err != nil {
				t.Fatalf("CancelDiskRemoval: %v", err)
			}
			if arrayReady.callCount() != 1 {
				t.Fatalf("ArrayReady was called %d time(s), want exactly 1 — the disk's branches must be re-applied RW", arrayReady.callCount())
			}
			got, err := h.ArrayStore.GetDataDiskByMountpoint(ctx, disk2)
			if err != nil {
				t.Fatalf("GetDataDiskByMountpoint: %v", err)
			}
			if got.RemovalState != "" || got.RemovalJobID != "" {
				t.Fatalf("disk after cancel: RemovalState=%q RemovalJobID=%q, want both cleared", got.RemovalState, got.RemovalJobID)
			}
		})
	}
}

// TestHandler_CancelDiskRemoval_LiveApplyFailureIsInternal proves a
// live-update failure while re-applying the pool is reported as an opaque
// internal error, with the removal state already cleared beforehand — the
// same clear-then-apply order the disk-topology jobs already use for
// their own ArrayReady call, never silently reporting success while the
// disk is still no-create, and never leaving it stranded no-create either
// once the state is already gone.
func TestHandler_CancelDiskRemoval_LiveApplyFailureIsInternal(t *testing.T) {
	ctx := context.Background()
	h, _, disk2, _ := newDiskRemoveTestHandler(t, true)
	failing := &fakeArrayReadyHook{err: errors.New("applying the new pool mounts to the running pool: simulated mount failure")}
	h.ArrayReady = failing.hook
	if err := h.ArrayStore.SetRemovalState(ctx, disk2, store.RemovalStateEvacuated, "evac-1"); err != nil {
		t.Fatalf("SetRemovalState(evacuated): %v", err)
	}

	err := h.CancelDiskRemoval(ctx, &apiv1.CancelDiskRemovalRequest{Mountpoint: disk2})
	if status := apiError(t, h, err); status.StatusCode != 500 || status.Response.Code != "internal" {
		t.Fatalf("CancelDiskRemoval with a failing ArrayReady = %+v, want an opaque 500", status)
	}
	got, gerr := h.ArrayStore.GetDataDiskByMountpoint(ctx, disk2)
	if gerr != nil {
		t.Fatalf("GetDataDiskByMountpoint: %v", gerr)
	}
	if got.RemovalState != "" {
		t.Fatalf("RemovalState after a failed live apply = %q, want cleared (never silently left no-create)", got.RemovalState)
	}
}

// TestHandler_ReplaceDisk_RefusesADiskInRemoval proves #366's refusal:
// store.ReplaceDataDisk keeps a slot's removal state, so planDiskReplace
// and replaceDisk answer 409 disk_leaving_array for a disk in any removal
// state, and nothing is formatted or queued.
func TestHandler_ReplaceDisk_RefusesADiskInRemoval(t *testing.T) {
	for _, state := range []string{store.RemovalStateEvacuating, store.RemovalStateEvacuated, store.RemovalStateUnpooled, store.RemovalStateUnlisted} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			h, _, p, st, _, _ := newDiskLifecycleHandler(t)
			seedHandlerArray(t, st, p)
			p.AddDisk("/dev/sdz", disk.Disk{Size: 4 * disk.TB})
			markRemoval(t, st, "/mnt/disk1", state)

			_, err := h.PlanDiskReplace(ctx, &apiv1.ReplaceDiskPlanRequest{Mountpoint: "/mnt/disk1", Device: "/dev/sdz"})
			if status := apiError(t, h, err); status.StatusCode != 409 || status.Response.Code != "disk_leaving_array" {
				t.Fatalf("PlanDiskReplace(%s disk) = %+v, want 409 disk_leaving_array", state, status)
			}
			_, err = h.ReplaceDisk(ctx, &apiv1.ReplaceDiskRequest{Mountpoint: "/mnt/disk1", Device: "/dev/sdz", Confirmation: "ERASE /dev/sdz"})
			if status := apiError(t, h, err); status.StatusCode != 409 || status.Response.Code != "disk_leaving_array" {
				t.Fatalf("ReplaceDisk(%s disk) = %+v, want 409 disk_leaving_array", state, status)
			}
			if _, ok := p.FormattedAs("/dev/sdz"); ok {
				t.Fatal("a refused replace formatted /dev/sdz")
			}
			jobs, err := h.Store.List(ctx, job.ListFilter{})
			if err != nil {
				t.Fatalf("listing jobs: %v", err)
			}
			if len(jobs) != 0 {
				t.Fatalf("a refused replace submitted %d job(s), want none", len(jobs))
			}
		})
	}
}
