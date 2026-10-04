package migrate

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"
)

// parityEnv is an imported array waiting for its point of no return: a session
// with the scan's report, the adoption's record (two data disks, a recorded
// parity disk and cache) and a machine that still has every disk.
type parityEnv struct {
	t         *testing.T
	s         *Service
	machine   func() []disk.Disk
	review    *Review
	mu        sync.Mutex
	pending   bool
	finishing bool
	data      []store.ArrayDisk
	recorded  []store.RecordedDisk
}

type listProvider struct {
	disk.Provider
	list func() []disk.Disk
}

func (p listProvider) List(context.Context) ([]disk.Disk, error) { return p.list(), nil }

func newParityEnv(t *testing.T) *parityEnv {
	t.Helper()
	e := &parityEnv{t: t, pending: true, review: importReview()}
	e.machine = importDisks
	e.data = []store.ArrayDisk{
		{Role: store.ArrayRoleData, RoleIndex: 1, Serial: "DAT1", Mountpoint: "/mnt/disk1"},
		{Role: store.ArrayRoleData, RoleIndex: 2, Serial: "DAT2", Mountpoint: "/mnt/disk2"},
	}
	e.recorded = []store.RecordedDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Serial: "PAR1", Device: "/dev/sdb"},
		{Role: store.ArrayRoleCache, RoleIndex: 1, Serial: "CAC1", Device: "/dev/nvme0n1"},
	}
	e.s = &Service{
		Dir:      t.TempDir(),
		Sessions: newSessions(t),
		Scanner:  &Scanner{Disks: listProvider{list: func() []disk.Disk { return e.machine() }}},
		Pending: func(context.Context) (bool, error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.pending, nil
		},
		Finishing: func(context.Context) (bool, error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.finishing, nil
		},
		Record: func(context.Context) ([]store.ArrayDisk, []store.RecordedDisk, error) {
			return e.data, e.recorded, nil
		},
	}
	e.setSession(&VerifyResult{Status: VerifyPassed})
	return e
}

func (e *parityEnv) setSession(v *VerifyResult) {
	e.t.Helper()
	if err := e.s.save(context.Background(), &session{Report: &Report{Verdict: VerdictGo, Review: e.review}, Verify: v}); err != nil {
		e.t.Fatal(err)
	}
}

func TestPlanParityInit_IsOfferedOnlyAfterAPassingVerify(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		verify *VerifyResult
		want   string
	}{
		"no verify has run":         {nil, "no verify has run"},
		"a verify is running":       {&VerifyResult{Status: VerifyRunning}, "still running"},
		"the latest verify failed":  {&VerifyResult{Status: VerifyFailed, Error: "x"}, "failed"},
		"a status nothing writes":   {&VerifyResult{Status: "unknown"}, "failed"},
		"a verify that has no pass": {&VerifyResult{}, "failed"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newParityEnv(t)
			e.setSession(tc.verify)
			_, err := e.s.PlanParityInit(ctx)
			if !errors.Is(err, ErrVerifyRequired) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("PlanParityInit = %v, want ErrVerifyRequired (%s)", err, tc.want)
			}
			if _, err := e.s.ExpectedParityConfirmation(ctx); !errors.Is(err, ErrVerifyRequired) {
				t.Errorf("ExpectedParityConfirmation = %v, want ErrVerifyRequired: no confirmation is offered without a pass", err)
			}
		})
	}

	e := newParityEnv(t)
	plan, err := e.s.PlanParityInit(ctx)
	if err != nil {
		t.Fatalf("PlanParityInit after a pass: %v", err)
	}
	if got := plan.ParityInitConfirmation(); got != "ERASE /dev/nvme0n1, /dev/sdb" {
		t.Errorf("confirmation = %q, want it to name the parity disk and the cache and no data disk", got)
	}
	if len(plan.Data) != 2 || len(plan.Parity) != 1 || plan.Cache == nil {
		t.Errorf("plan = %+v", plan)
	}

	// An import that runs again forgets the pass: what was verified is not what is
	// mounted now.
	if err := e.s.InvalidateVerify(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.PlanParityInit(ctx); !errors.Is(err, ErrVerifyRequired) {
		t.Errorf("PlanParityInit after the verify result was forgotten = %v, want ErrVerifyRequired", err)
	}
	if err := e.s.InvalidateVerify(ctx); err != nil {
		t.Errorf("forgetting a result that is not there: %v", err)
	}
	if st, _ := e.s.State(ctx); st.Report == nil {
		t.Error("forgetting the verify result forgot the report too")
	}
}

func TestPlanParityInit_NeedsAPendingImportAndEveryDiskStillThere(t *testing.T) {
	ctx := context.Background()
	t.Run("nothing pending", func(t *testing.T) {
		e := newParityEnv(t)
		e.pending = false
		if _, err := e.s.PlanParityInit(ctx); !errors.Is(err, ErrParityNotPending) {
			t.Fatalf("PlanParityInit = %v, want ErrParityNotPending", err)
		}
	})
	t.Run("a daemon that cannot read the record", func(t *testing.T) {
		e := newParityEnv(t)
		e.s.Record = nil
		if _, err := e.s.PlanParityInit(ctx); !errors.Is(err, ErrParityNotConfigured) {
			t.Fatalf("PlanParityInit = %v, want ErrParityNotConfigured", err)
		}
	})
	for name, gone := range map[string]string{"the parity disk": "PAR1", "a data disk": "DAT2", "the cache": "CAC1"} {
		t.Run(name+" is gone", func(t *testing.T) {
			e := newParityEnv(t)
			e.machine = func() []disk.Disk {
				var out []disk.Disk
				for _, d := range importDisks() {
					if d.Serial != gone {
						out = append(out, d)
					}
				}
				return out
			}
			if _, err := e.s.PlanParityInit(ctx); !errors.Is(err, disk.ErrAdoptDiskMissing) {
				t.Fatalf("PlanParityInit = %v, want ErrAdoptDiskMissing", err)
			}
		})
	}
	t.Run("a disk that is now the one this machine boots from", func(t *testing.T) {
		e := newParityEnv(t)
		e.machine = func() []disk.Disk {
			d := importDisks()
			d[3].Boot = true
			return d
		}
		if _, err := e.s.PlanParityInit(ctx); !errors.Is(err, disk.ErrAdoptBootDisk) {
			t.Fatalf("PlanParityInit = %v, want ErrAdoptBootDisk", err)
		}
	})
	t.Run("a data disk now recorded as the cache is never offered for erasing", func(t *testing.T) {
		e := newParityEnv(t)
		e.recorded[1].Serial = "DAT2"
		if _, err := e.s.PlanParityInit(ctx); err == nil {
			t.Fatal("a disk the capture says is one of Unraid's data disks was offered as the cache")
		}
	})
}

// A cache that is a partition of an Unraid boot device is formatted only when the
// capture says the boot pool is not a mirrored pair.
func TestPlanParityInit_ACacheOnAnUnraidBootDeviceNeedsAnUnmirroredBootPool(t *testing.T) {
	ctx := context.Background()
	yes, no := true, false
	build := func(mirrored *bool) *parityEnv {
		e := newParityEnv(t)
		e.review.Disks[3].UnraidBoot = true
		e.review.Boot = ReviewBoot{Mode: "internal", Mirrored: mirrored, SharedWithCache: &yes}
		e.machine = func() []disk.Disk {
			d := importDisks()[:3]
			return append(d, disk.Disk{
				Device: "/dev/nvme0n1", Serial: "CAC1", Size: 1 * disk.TB, ByIDName: "nvme-EX_CAC1", UnraidBoot: true, Filesystem: "zfs_member", FSDevice: "/dev/nvme0n1p3",
				UnraidDataPartition: &disk.BootPartition{Device: "/dev/nvme0n1p4", Size: 400 * disk.GB, ByIDName: "nvme-EX_CAC1-part4", PartUUID: "cccc-dddd"},
			})
		}
		e.recorded[1] = store.RecordedDisk{Role: store.ArrayRoleCache, RoleIndex: 1, Serial: "CAC1", Device: "/dev/nvme0n1p4", ByIDName: "nvme-EX_CAC1-part4", PartUUID: "cccc-dddd"}
		e.setSession(&VerifyResult{Status: VerifyPassed})
		return e
	}

	plan, err := build(&no).s.PlanParityInit(ctx)
	if err != nil {
		t.Fatalf("PlanParityInit of an unmirrored boot + data device: %v", err)
	}
	if got := plan.ParityInitConfirmation(); got != "ERASE /dev/nvme0n1p4, /dev/sdb" {
		t.Errorf("confirmation = %q, want partition 4 named and not the disk", got)
	}
	for name, mirrored := range map[string]*bool{"a mirrored pair": &yes, "a capture that does not say": nil} {
		if _, err := build(mirrored).s.PlanParityInit(ctx); !errors.Is(err, ErrMirroredBootPool) {
			t.Errorf("%s: PlanParityInit = %v, want ErrMirroredBootPool", name, err)
		}
	}
}

func TestParityInit_SaysWhatItDoesAndWhyItCannotBeOffered(t *testing.T) {
	ctx := context.Background()
	e := newParityEnv(t)
	if st, err := e.s.State(ctx); err != nil || st.Phase != PhaseVerified {
		t.Fatalf("phase = %v, %v, want verified", st, err)
	}
	info, err := e.s.ParityInit(ctx)
	if err != nil || info == nil {
		t.Fatalf("ParityInit = %+v, %v", info, err)
	}
	if info.Finishing || info.Problem != "" || info.Confirmation != "ERASE /dev/nvme0n1, /dev/sdb" {
		t.Errorf("info = %+v", info)
	}
	if len(info.Erases) != 2 || info.Erases[0].Role != "parity" || info.Erases[0].Device != "/dev/sdb" || info.Erases[1].Role != "cache" || info.Erases[1].Partition {
		t.Errorf("erases = %+v, want the parity disk and the whole cache disk", info.Erases)
	}
	if !strings.Contains(info.Window, "no redundancy whatsoever") || len(info.Rollback) == 0 || !strings.Contains(info.Rollback[0], "restoring from backup") {
		t.Errorf("window = %q, rollback = %q: they must say it in doc 05 §5's terms", info.Window, info.Rollback)
	}

	// A disk that went away is said, and no confirmation is offered.
	e.machine = func() []disk.Disk { return importDisks()[1:] }
	info, err = e.s.ParityInit(ctx)
	if err != nil || info == nil || info.Confirmation != "" || info.Problem == "" || len(info.Erases) != 0 || info.Window == "" {
		t.Errorf("ParityInit with the parity disk gone = %+v, %v, want a problem and no confirmation", info, err)
	}

	// Not offered before a verify passed, or after one failed.
	e = newParityEnv(t)
	e.setSession(&VerifyResult{Status: VerifyFailed})
	if info, err := e.s.ParityInit(ctx); err != nil || info != nil {
		t.Errorf("ParityInit after a failed verify = %+v, %v, want nothing offered", info, err)
	}
	e.pending = false
	e.setSession(nil)
	if info, err := e.s.ParityInit(ctx); err != nil || info != nil {
		t.Errorf("ParityInit with nothing pending = %+v, %v, want nothing offered", info, err)
	}
}

// A run that stopped after the disks were formatted and recorded is finished, not
// started over: it is its own phase, erases nothing, and holds the session.
func TestParityInit_AnUnfinishedInitialisationIsItsOwnPhaseAndErasesNothing(t *testing.T) {
	ctx := context.Background()
	e := newParityEnv(t)
	e.pending, e.finishing = false, true
	st, err := e.s.State(ctx)
	if err != nil || st.Phase != PhaseInitializing {
		t.Fatalf("phase = %v, %v, want initializing", st, err)
	}
	info, err := e.s.ParityInit(ctx)
	if err != nil || info == nil || !info.Finishing || info.Confirmation != disk.ParityInitFinishConfirmation || len(info.Erases) != 0 {
		t.Fatalf("ParityInit = %+v, %v, want the finishing confirmation and nothing erased", info, err)
	}
	if got, err := e.s.ExpectedParityConfirmation(ctx); err != nil || got != disk.ParityInitFinishConfirmation {
		t.Errorf("ExpectedParityConfirmation = %q, %v", got, err)
	}
	if err := e.s.Forget(ctx); !errors.Is(err, ErrImportPending) {
		t.Errorf("Forget while the initialisation is unfinished = %v, want ErrImportPending: the session is kept until it finishes", err)
	}
	if err := e.s.CheckVerify(ctx); !errors.Is(err, ErrVerifyNotPending) {
		t.Errorf("CheckVerify = %v, want ErrVerifyNotPending: there is nothing to verify once the disks are formatted", err)
	}
}

func TestRollbackNotes_StateTheLayoutThePlanGave(t *testing.T) {
	yes, no := true, false
	cacheRow := func(hostBoot *bool) ReviewDisk { return ReviewDisk{UnraidRole: UnraidCache, HostBoot: hostBoot} }
	contains := func(notes []string, want string) bool {
		for _, n := range notes {
			if strings.Contains(n, want) {
				return true
			}
		}
		return false
	}
	for name, tc := range map[string]struct {
		review *Review
		want   []string
		not    []string
	}{
		"no review":                         {nil, nil, []string{"USB stick", "internal"}},
		"a boot mode the capture omits":     {&Review{}, nil, []string{"USB stick", "internal"}},
		"a USB stick, a separate cache":     {&Review{Boot: ReviewBoot{Mode: "usb"}, Disks: []ReviewDisk{cacheRow(&no)}}, []string{"reinsert the stick and boot"}, []string{"re-create the Unraid cache"}},
		"a USB stick, no cache":             {&Review{Boot: ReviewBoot{Mode: "usb"}}, []string{"reinsert the stick and boot"}, []string{"re-create the Unraid cache"}},
		"a USB stick, a shared NVMe":        {&Review{Boot: ReviewBoot{Mode: "usb"}, Disks: []ReviewDisk{cacheRow(&yes)}}, []string{"re-create the Unraid cache"}, []string{"reinsert the stick and boot"}},
		"a USB stick, a cache not matched":  {&Review{Boot: ReviewBoot{Mode: "usb"}, Disks: []ReviewDisk{cacheRow(nil)}}, []string{"re-create the Unraid cache", "reinsert the stick and boot"}, nil},
		"an internal mirrored pair":         {&Review{Boot: ReviewBoot{Mode: "internal", Mirrored: &yes, SharedWithCache: &no}}, []string{"mirrored pair", "degraded boot pool"}, nil},
		"an internal boot + data device":    {&Review{Boot: ReviewBoot{Mode: "internal", Mirrored: &no, SharedWithCache: &yes}}, []string{"also holds its cache", "partition 4"}, nil},
		"an internal dedicated boot device": {&Review{Boot: ReviewBoot{Mode: "internal", Mirrored: &no, SharedWithCache: &no}}, []string{"dedicated internal device"}, []string{"partition 4"}},
	} {
		t.Run(name, func(t *testing.T) {
			notes := RollbackNotes(tc.review)
			if len(notes) == 0 || !strings.Contains(notes[0], "restoring from backup") {
				t.Fatalf("notes = %q, want the general statement first", notes)
			}
			for _, w := range tc.want {
				if !contains(notes, w) {
					t.Errorf("notes %q lack %q", notes, w)
				}
			}
			for _, w := range tc.not {
				if contains(notes, w) {
					t.Errorf("notes %q mention %q, which is not this layout", notes, w)
				}
			}
		})
	}
}

func TestCheckBootCacheNotMirrored_ACachePartitionWhoseDiskCannotBeIdentifiedIsRefused(t *testing.T) {
	no := false
	review := &Review{Boot: ReviewBoot{Mode: "internal", Mirrored: &no}}
	plan := func(serial string) disk.AdoptionPlan {
		return disk.AdoptionPlan{Cache: &disk.RecordedDisk{AssignedDisk: disk.AssignedDisk{
			Device: "/dev/nvme0n1p4", ByIDName: "nvme-EX_" + serial + "-part4", Serial: serial,
		}}}
	}
	disks := func(serials ...string) []disk.Disk {
		var out []disk.Disk
		for _, s := range serials {
			out = append(out, disk.Disk{Device: "/dev/" + s, Serial: s, UnraidBoot: s == "BOOT"})
		}
		return out
	}

	for name, listed := range map[string][]disk.Disk{
		"no listed disk has the identity":    disks("OTHER"),
		"two listed disks have the identity": disks("CAC1", "CAC1"),
	} {
		err := CheckBootCacheNotMirrored(review, listed, plan("CAC1"))
		if !errors.Is(err, ErrMirroredBootPool) {
			t.Errorf("%s: CheckBootCacheNotMirrored = %v, want ErrMirroredBootPool", name, err)
		}
	}

	if err := CheckBootCacheNotMirrored(review, disks("CAC1"), plan("CAC1")); err != nil {
		t.Errorf("a cache partition of an identified disk that is not an Unraid boot device: %v, want nil", err)
	}
}
