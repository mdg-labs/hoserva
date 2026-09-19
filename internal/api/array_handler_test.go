package api_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
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
