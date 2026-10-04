package migrate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/disk"
)

// importDisks is a machine for the import: each disk has a by-id link for its
// first partition and a UUID of its own.
func importDisks() []disk.Disk {
	d := func(dev, serial string, size int64, fs, uuid string) disk.Disk {
		return disk.Disk{Device: dev, Serial: serial, Size: size, Filesystem: fs, FSDevice: dev + "1", FSUUID: uuid, ByIDName: "ata-EX_" + serial, FSByIDName: "ata-EX_" + serial + "-part1"}
	}
	return []disk.Disk{
		d("/dev/sdb", "PAR1", 8*disk.TB, "xfs", "30000000-0000-4000-8000-000000000001"),
		d("/dev/sdc", "DAT1", 4*disk.TB, "xfs", "30000000-0000-4000-8000-000000000002"),
		d("/dev/sdd", "DAT2", 2*disk.TB, "xfs", "30000000-0000-4000-8000-000000000003"),
		d("/dev/nvme0n1", "CAC1", 500*disk.GB, "btrfs", "30000000-0000-4000-8000-000000000004"),
	}
}

func importReview() *Review {
	strong, notBoot := false, false
	row := func(slot string, n int, role UnraidRole, proposed ProposedRole, dev, serial string) ReviewDisk {
		return ReviewDisk{Slot: slot, DiskNumber: n, UnraidRole: role, ProposedRole: proposed, Device: dev, Serial: serial, WeakIdentity: &strong, HostBoot: &notBoot}
	}
	return &Review{Disks: []ReviewDisk{
		row("parity", 0, UnraidParity, ProposeParity, "/dev/sdb", "PAR1"),
		row("disk1", 1, UnraidData, ProposeData, "/dev/sdc", "DAT1"),
		row("disk2", 2, UnraidData, ProposeData, "/dev/sdd", "DAT2"),
		row("pool cache", 0, UnraidCache, ProposeCache, "/dev/nvme0n1", "CAC1"),
	}}
}

func roles(pairs ...string) []disk.AdoptionAssignment {
	var out []disk.AdoptionAssignment
	for _, p := range pairs {
		serial, role, _ := strings.Cut(p, "=")
		out = append(out, disk.AdoptionAssignment{Role: disk.AdoptionRole(role), Serial: serial})
	}
	return out
}

func TestPlanFromReview_OrdersDataDisksByUnraidDiskNumber(t *testing.T) {
	// Given out of order, as a user's --role flags may be.
	p, err := PlanFromReview(importReview(), importDisks(), roles("CAC1=cache", "DAT2=data", "PAR1=parity", "DAT1=data"))
	if err != nil {
		t.Fatalf("PlanFromReview: %v", err)
	}
	if len(p.Plan.Data) != 2 || p.Plan.Data[0].Serial != "DAT1" || p.Plan.Data[1].Serial != "DAT2" {
		t.Errorf("data = %+v, want disk1 then disk2, so /mnt/disk1 is Unraid's disk1", p.Plan.Data)
	}
	if len(p.Plan.Parity) != 1 || p.Plan.Cache == nil {
		t.Errorf("plan = %+v", p.Plan)
	}
	if got := p.Assignments; len(got) != 4 {
		t.Errorf("assignments = %+v", got)
	}
}

func TestPlanFromReview_AppliesTheScansAndTheCapturesRules(t *testing.T) {
	notBoot, hostBoot := false, true
	refused := func(code RefusalCode) *Review {
		rv := importReview()
		rv.Disks[2].Refused, rv.Disks[2].RefusalCode, rv.Disks[2].Refusal = true, code, "disk2 is not adopted: because"
		rv.Disks[2].ProposedRole = ""
		return rv
	}
	bootRow := func() *Review {
		rv := importReview()
		rv.Disks = append(rv.Disks, ReviewDisk{Slot: "boot", UnraidRole: UnraidBoot, ProposedRole: ProposeIgnore, Device: "/dev/sde", Serial: "BOOT1", HostBoot: &notBoot})
		return rv
	}
	hostBootCache := func() *Review {
		rv := importReview()
		rv.Disks[3].HostBoot = &hostBoot
		return rv
	}
	withBoot := func(disks []disk.Disk) []disk.Disk {
		return append(disks, disk.Disk{Device: "/dev/sde", Serial: "BOOT1", Size: 16 * disk.GB, UnraidBoot: true, Filesystem: "zfs_member", FSDevice: "/dev/sde3"})
	}
	hostBootDisks := func() []disk.Disk {
		d := importDisks()
		d[3].Boot = true
		return d
	}
	for _, tc := range []struct {
		name    string
		review  *Review
		disks   []disk.Disk
		roles   []disk.AdoptionAssignment
		wantErr error
	}{
		{"a disk the scan refused as data", refused(RefuseIntegrity), importDisks(), roles("PAR1=parity", "DAT1=data", "DAT2=data"), ErrImportDiskRefused},
		{"a disk the scan refused as parity", refused(RefuseIntegrity), importDisks(), roles("DAT1=data", "DAT2=parity"), ErrImportDiskRefused},
		{"a disk the scan refused as cache", refused(RefuseHostBoot), importDisks(), roles("PAR1=parity", "DAT1=data", "DAT2=cache"), ErrImportDiskRefused},
		{"a weak-identity parity disk the scan refused", refused(RefuseWeakIdentityParity), importDisks(), roles("DAT2=parity", "DAT1=data"), ErrImportDiskRefused},
		{"Unraid's parity disk as data, whatever it reports", importReview(), importDisks(), roles("PAR1=data", "DAT1=data"), ErrImportRole},
		{"one of Unraid's data disks as parity", importReview(), importDisks(), roles("DAT2=parity", "DAT1=data"), ErrImportRole},
		{"one of Unraid's data disks as cache", importReview(), importDisks(), roles("PAR1=parity", "DAT1=data", "DAT2=cache"), ErrImportRole},
		{"an Unraid boot device as data", bootRow(), withBoot(importDisks()), roles("PAR1=parity", "DAT1=data", "BOOT1=data"), ErrImportRole},
		{"an Unraid boot device as cache", bootRow(), withBoot(importDisks()), roles("PAR1=parity", "DAT1=data", "BOOT1=cache"), ErrImportRole},
		{"a disk the scan did not list", importReview(), importDisks(), roles("PAR1=parity", "DAT1=data", "NOPE=data"), ErrImportUnknownDisk},
		{"the shared NVMe, whole, as parity", hostBootCache(), hostBootDisks(), roles("CAC1=parity", "DAT1=data"), disk.ErrAdoptBootDisk},
		{"the shared NVMe, whole, as data", hostBootCache(), hostBootDisks(), roles("PAR1=parity", "CAC1=data"), disk.ErrAdoptBootDisk},
		{"the shared NVMe, whole, as cache", hostBootCache(), hostBootDisks(), roles("PAR1=parity", "DAT1=data", "CAC1=cache"), disk.ErrAdoptBootDisk},
		// A report that predates the field, or does not say, cannot let it through:
		// the live inventory names the boot disk.
		{"the boot disk as parity when the report cannot say it is one", importReview(), hostBootDisks(), roles("CAC1=parity", "DAT1=data"), disk.ErrAdoptBootDisk},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PlanFromReview(tc.review, tc.disks, tc.roles)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("PlanFromReview = %v, want %v", err, tc.wantErr)
			}
			if !IsImportRoleError(err) {
				t.Errorf("%v is not a refusal of the mapping", err)
			}
		})
	}
}

// The cache pool's row of an internal boot that shares its disk with the cache
// is proposed cache: it records that device's data partition, and a dedicated
// boot device, which has no cache, can only be ignored.
func TestPlanFromReview_TheCacheOfAnUnraidBootAndDataDevice(t *testing.T) {
	notBoot := false
	machine := func() []disk.Disk {
		d := importDisks()[:3]
		return append(d, disk.Disk{
			Device: "/dev/nvme0n1", Serial: "CAC1", Size: 1 * disk.TB, ByIDName: "nvme-EX_CAC1", UnraidBoot: true, Filesystem: "zfs_member", FSDevice: "/dev/nvme0n1p3",
			UnraidDataPartition: &disk.BootPartition{Device: "/dev/nvme0n1p4", Size: 400 * disk.GB, ByIDName: "nvme-EX_CAC1-part4", PartUUID: "cccc-dddd"},
		})
	}
	review := func(shared *bool) *Review {
		rv := importReview()
		rv.Disks[3].UnraidBoot = true
		rv.Disks[3].HostBoot = &notBoot
		rv.Boot.SharedWithCache = shared
		return rv
	}
	yes, no := true, false

	p, err := PlanFromReview(review(&yes), machine(), roles("PAR1=parity", "DAT1=data", "CAC1=cache"))
	if err != nil {
		t.Fatalf("PlanFromReview: %v", err)
	}
	if c := p.Plan.Cache; c == nil || c.Device != "/dev/nvme0n1p4" || c.PartUUID != "cccc-dddd" || c.ByIDName != "nvme-EX_CAC1-part4" {
		t.Errorf("cache = %+v, want partition 4 only", c)
	}
	if _, err := PlanFromReview(review(nil), machine(), roles("PAR1=parity", "DAT1=data", "CAC1=cache")); err != nil {
		t.Errorf("a capture that does not say whether the boot is shared: %v", err)
	}
	for _, tc := range []struct {
		name   string
		review *Review
		roles  []disk.AdoptionAssignment
	}{
		{"a boot device the capture says holds no cache", review(&no), roles("PAR1=parity", "DAT1=data", "CAC1=cache")},
		{"the cache pool's boot disk as data", review(&yes), roles("PAR1=parity", "CAC1=data")},
		{"the cache pool's boot disk as parity", review(&yes), roles("CAC1=parity", "DAT1=data")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PlanFromReview(tc.review, machine(), tc.roles)
			if err == nil || !IsImportRoleError(err) {
				t.Fatalf("PlanFromReview = %v, want a refusal of the mapping", err)
			}
		})
	}
}

func TestPlanFromReview_AnIgnoredDiskNeedsNoRoleRule(t *testing.T) {
	rv := importReview()
	rv.Disks = append(rv.Disks, ReviewDisk{Slot: "boot", UnraidRole: UnraidBoot, ProposedRole: ProposeIgnore, Device: "/dev/sde", Serial: "BOOT1"})
	disks := append(importDisks(), disk.Disk{Device: "/dev/sde", Serial: "BOOT1", Size: 16 * disk.GB, UnraidBoot: true, FSDevice: "/dev/sde3"})
	if _, err := PlanFromReview(rv, disks, roles("PAR1=parity", "DAT1=data", "DAT2=data", "BOOT1=ignore")); err != nil {
		t.Errorf("an Unraid boot device ignored: %v", err)
	}
	if _, err := PlanFromReview(rv, disks, roles("PAR1=parity", "DAT1=data", "GONE=ignore")); !errors.Is(err, disk.ErrAdoptDiskMissing) {
		t.Errorf("an ignored disk that is gone = %v, want it refused: every role must still match a disk", err)
	}
}

// The Unraid USB stick is refused in each role, with and without the scan having
// listed it.
func TestPlanFromReview_RefusesTheUnraidStickInEveryRole(t *testing.T) {
	stick := disk.Disk{Device: "/dev/sde", Serial: "STICK1", Size: 16 * disk.GB, Filesystem: "vfat", Label: "UNRAID", FSUUID: "ABCD-1234", FSDevice: "/dev/sde1"}
	listed := []ReviewDisk{{Slot: "boot", UnraidRole: UnraidBoot, ProposedRole: ProposeIgnore, Device: "/dev/sde", Serial: "STICK1"}, {UnraidRole: UnraidUnassigned, Device: "/dev/sde", Serial: "STICK1"}}
	for _, row := range listed {
		for _, role := range []string{"data", "parity", "cache"} {
			rv := importReview()
			rv.Disks = append(rv.Disks, row)
			base := []string{"PAR1=parity", "DAT1=data", "DAT2=data"}
			_, err := PlanFromReview(rv, append(importDisks(), stick), roles(append(base, "STICK1="+role)...))
			if err == nil {
				t.Errorf("the stick was given the %s role (row %q)", role, row.Slot)
			}
		}
	}
	// Listed by the scan as an unassigned disk, nothing marks it a boot device:
	// the live inventory still knows it for what it is.
	for _, role := range []string{"data", "parity", "cache"} {
		rv := importReview()
		rv.Disks = append(rv.Disks, ReviewDisk{UnraidRole: UnraidUnassigned, Device: "/dev/sde", Serial: "STICK1"})
		_, err := PlanFromReview(rv, append(importDisks(), stick), roles("PAR1=parity", "DAT1=data", "STICK1="+role))
		if !errors.Is(err, disk.ErrUnraidStick) {
			t.Errorf("a stick the inventory lists as %s = %v, want ErrUnraidStick", role, err)
		}
	}
}

func TestCheckImportable(t *testing.T) {
	good := &Report{Verdict: VerdictGoWithWarnings, Review: &Review{}}
	for name, tc := range map[string]struct {
		report     *Report
		unfinished bool
		want       error
	}{
		"no report":         {nil, false, ErrImportNoReport},
		"a scan unfinished": {good, true, ErrImportScanUnfinished},
		"a no-go verdict":   {&Report{Verdict: VerdictNoGo, Review: &Review{}}, false, ErrImportNoGo},
		"no disk table":     {&Report{Verdict: VerdictGo}, false, ErrImportNoReview},
		"a finished scan":   {good, false, nil},
		"a go verdict":      {&Report{Verdict: VerdictGo, Review: &Review{}}, false, nil},
	} {
		if err := CheckImportable(tc.report, tc.unfinished); !errors.Is(err, tc.want) {
			t.Errorf("%s: CheckImportable = %v, want %v", name, err, tc.want)
		}
	}
}

// serviceWithScan scans the primary fixture on the unit-test machine and returns
// the service that holds the report.
func serviceWithScan(t *testing.T, e *dataEnv) *Service {
	t.Helper()
	svc := &Service{Dir: t.TempDir() + "/migrate", Scanner: e.scanner, Sessions: newSessions(t)}
	if err := scanNow(svc, zipOf(t, e.files, false), ScanOptions{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return svc
}

func TestPlanImport_FromTheSessionsReport(t *testing.T) {
	ctx := context.Background()
	e := newDataEnv(t, primary)
	svc := serviceWithScan(t, e)

	st, err := svc.State(ctx)
	if err != nil || st.Report == nil || st.Report.Review == nil {
		t.Fatalf("State = %+v, %v", st, err)
	}
	var proposed []disk.AdoptionAssignment
	for _, d := range st.Report.Review.Disks {
		switch d.ProposedRole {
		case ProposeParity, ProposeData, ProposeCache:
			proposed = append(proposed, disk.AdoptionAssignment{Role: disk.AdoptionRole(d.ProposedRole), Serial: d.Serial})
		}
	}
	p, err := svc.PlanImport(ctx, proposed)
	if err != nil {
		t.Fatalf("PlanImport of the scan's own proposal: %v", err)
	}
	if len(p.Plan.Data) != 3 || len(p.Plan.Parity) != 1 || p.Plan.Cache == nil {
		t.Fatalf("plan = %+v", p.Plan)
	}
	for i, d := range p.Plan.Data {
		if want := fmt.Sprintf("disk%d-hoserva-test", i+1); d.Serial != want {
			t.Errorf("data disk %d is %s, want %s", i+1, d.Serial, want)
		}
	}

	if _, err := svc.PlanImport(ctx, roles("parity-hoserva-test=data", "disk1-hoserva-test=data")); !errors.Is(err, ErrImportRole) {
		t.Errorf("the scan's parity disk as data = %v, want ErrImportRole", err)
	}
}

func TestPlanImport_RefusesWithoutAFinishedGoodScan(t *testing.T) {
	ctx := context.Background()
	mapping := roles("parity-hoserva-test=parity", "disk1-hoserva-test=data")

	t.Run("no scan", func(t *testing.T) {
		e := newDataEnv(t, primary)
		svc := &Service{Dir: t.TempDir() + "/migrate", Scanner: e.scanner, Sessions: newSessions(t)}
		if _, err := svc.PlanImport(ctx, mapping); !errors.Is(err, ErrImportNoReport) {
			t.Errorf("PlanImport = %v, want ErrImportNoReport", err)
		}
	})
	t.Run("a no-go scan", func(t *testing.T) {
		e := newDataEnv(t, primary)
		e.runner.Script("xfs_repair", []string{"-n", e.dev["disk2"] + "1"}, nil, errors.New("exit status 1"))
		svc := serviceWithScan(t, e)
		if _, err := svc.PlanImport(ctx, mapping); !errors.Is(err, ErrImportNoGo) {
			t.Errorf("PlanImport = %v, want ErrImportNoGo", err)
		}
	})
	t.Run("a scan that has not finished", func(t *testing.T) {
		e := newDataEnv(t, primary)
		svc := serviceWithScan(t, e)
		if err := svc.StartScan(ctx, strings.NewReader(string(zipOf(t, e.files, false))), ScanOptions{}, func(context.Context, string) (string, error) { return "job-2", nil }); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.PlanImport(ctx, mapping); !errors.Is(err, ErrImportScanUnfinished) {
			t.Errorf("PlanImport = %v, want ErrImportScanUnfinished", err)
		}
	})
	t.Run("a report without the disk table", func(t *testing.T) {
		e := newDataEnv(t, primary)
		svc := serviceWithScan(t, e)
		sess, err := svc.load(ctx)
		if err != nil {
			t.Fatal(err)
		}
		sess.Report.Review = nil
		if err := svc.save(ctx, sess); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.PlanImport(ctx, mapping); !errors.Is(err, ErrImportNoReview) {
			t.Errorf("PlanImport = %v, want ErrImportNoReview", err)
		}
	})
	t.Run("a daemon that cannot list disks", func(t *testing.T) {
		svc := &Service{Dir: t.TempDir() + "/migrate", Sessions: newSessions(t)}
		if _, err := svc.PlanImport(ctx, mapping); !errors.Is(err, ErrImportNotConfigured) {
			t.Errorf("PlanImport = %v, want ErrImportNotConfigured", err)
		}
	})
}

// A parity slot that this machine's boot disk fills makes the scan no-go, and a
// mapping that sends parity there is refused even though a report could not stop it.
func TestPlanImport_TheBootDiskIsNeverParity(t *testing.T) {
	ctx := context.Background()
	e := newDataEnv(t, primary)
	e.setDisk("parity", func(d *disk.Disk) { d.Boot = true })
	svc := serviceWithScan(t, e)
	st, _ := svc.State(ctx)
	host := false
	for _, d := range st.Report.Review.Disks {
		if d.Slot == "parity" && d.RefusalCode == RefuseHostBoot && d.Refused {
			host = true
		}
	}
	if !host || st.Report.Verdict != VerdictNoGo {
		t.Fatalf("the scan did not refuse the boot disk in the parity slot: %+v", st.Report.Review.Disks)
	}
	_, err := svc.PlanImport(ctx, roles("parity-hoserva-test=parity", "disk1-hoserva-test=data"))
	if err == nil {
		t.Fatal("a mapping that sends parity to the boot disk was accepted")
	}
	// With a report that does not carry the refusal the live inventory still does.
	rv := importReview()
	disks := importDisks()
	disks[0].Boot = true
	if _, err := PlanFromReview(rv, disks, roles("PAR1=parity", "DAT1=data")); !errors.Is(err, disk.ErrAdoptBootDisk) {
		t.Errorf("PlanFromReview with the boot disk as parity = %v, want ErrAdoptBootDisk", err)
	}
}

func TestService_ForgetAndStateWhileAnImportIsPending(t *testing.T) {
	ctx := context.Background()
	e := newDataEnv(t, primary)
	svc := serviceWithScan(t, e)
	pending := false
	svc.Pending = func(context.Context) (bool, error) { return pending, nil }

	if st, err := svc.State(ctx); err != nil || st.Phase != PhaseScanned {
		t.Fatalf("State = %+v, %v", st, err)
	}
	pending = true
	if st, err := svc.State(ctx); err != nil || st.Phase != PhaseImported || st.Report == nil {
		t.Errorf("State while pending = %+v, %v, want the imported phase with the report kept", st, err)
	}
	if err := svc.Forget(ctx); !errors.Is(err, ErrImportPending) {
		t.Errorf("Forget while pending = %v, want ErrImportPending", err)
	}
	if st, _ := svc.State(ctx); st.Report == nil {
		t.Error("a refused Forget deleted the report")
	}
	svc.Pending = func(context.Context) (bool, error) { return false, errors.New("injected: the database is gone") }
	if err := svc.Forget(ctx); err == nil || errors.Is(err, ErrImportPending) {
		t.Errorf("Forget with an unknown pending state = %v, want the failure, never a deletion", err)
	}
	if _, err := svc.State(ctx); err == nil {
		t.Error("State answered although whether an import is pending could not be read")
	}
	svc.Pending = func(context.Context) (bool, error) { return false, nil }
	if err := svc.Forget(ctx); err != nil {
		t.Errorf("Forget with nothing pending = %v", err)
	}
}
