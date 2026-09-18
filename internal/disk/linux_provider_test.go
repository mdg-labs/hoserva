package disk

import (
	"context"
	"errors"
	"testing"
	"time"
)

func newTestProvider(t *testing.T) (*LinuxProvider, *FakeRunner) {
	t.Helper()
	runner := NewFakeRunner()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &LinuxProvider{
		Lister: newTestLister(t),
		Exec:   runner,
		Events: NewSpinEventLog(),
		Trends: NewTrendTracker(),
		Now:    func() time.Time { return now },
	}, runner
}

func TestLinuxProvider_SMART_RespectsStandbyByDefault(t *testing.T) {
	p, runner := newTestProvider(t)
	fixture := readFixture(t, "smartctl_ata_healthy.json")
	runner.Script("smartctl", []string{"-j", "-n", "standby", "-a", "/dev/sda"}, fixture, nil)

	report, err := p.SMART(context.Background(), "/dev/sda", SMARTPollRespectStandby)
	if err != nil {
		t.Fatalf("SMART: %v", err)
	}
	if report.ReallocatedSectors != 0 {
		t.Fatalf("SMART: got %+v", report)
	}

	calls := runner.Calls()
	if len(calls) != 1 {
		t.Fatalf("Calls: got %d, want 1", len(calls))
	}
	if calls[0].Name != "smartctl" {
		t.Fatalf("Calls[0].Name = %q, want smartctl", calls[0].Name)
	}
	wantArgs := []string{"-j", "-n", "standby", "-a", "/dev/sda"}
	if !equalArgs(calls[0].Args, wantArgs) {
		t.Fatalf("Calls[0].Args = %v, want %v (RespectStandby must pass -n standby)", calls[0].Args, wantArgs)
	}
}

func TestLinuxProvider_SMART_ForceOmitsStandbyFlag(t *testing.T) {
	p, runner := newTestProvider(t)
	fixture := readFixture(t, "smartctl_ata_healthy.json")
	runner.Script("smartctl", []string{"-j", "-a", "/dev/sda"}, fixture, nil)

	if _, err := p.SMART(context.Background(), "/dev/sda", SMARTPollForce); err != nil {
		t.Fatalf("SMART: %v", err)
	}

	calls := runner.Calls()
	if len(calls) != 1 {
		t.Fatalf("Calls: got %d, want 1", len(calls))
	}
	for _, a := range calls[0].Args {
		if a == "standby" {
			t.Fatalf("Calls[0].Args = %v, Force must never pass -n standby", calls[0].Args)
		}
	}
}

func TestLinuxProvider_SMART_RecordsSpinTransitionWithoutAWaitingQuery(t *testing.T) {
	p, runner := newTestProvider(t)
	standby := readFixture(t, "smartctl_ata_standby.json")
	healthy := readFixture(t, "smartctl_ata_healthy.json")
	runner.Script("smartctl", []string{"-j", "-n", "standby", "-a", "/dev/sda"}, standby, nil)

	if _, err := p.SMART(context.Background(), "/dev/sda", SMARTPollRespectStandby); err != nil {
		t.Fatalf("SMART (standby): %v", err)
	}
	if got := p.Events.Events("/dev/sda"); len(got) != 0 {
		t.Fatalf("Events after the first (standby) poll: got %+v, want none (no prior state to transition from)", got)
	}

	runner.Script("smartctl", []string{"-j", "-n", "standby", "-a", "/dev/sda"}, healthy, nil)
	if _, err := p.SMART(context.Background(), "/dev/sda", SMARTPollRespectStandby); err != nil {
		t.Fatalf("SMART (healthy): %v", err)
	}

	events := p.Events.Events("/dev/sda")
	if len(events) != 1 || events[0].From != Standby || events[0].To != Active {
		t.Fatalf("Events after the second poll: got %+v, want one Standby -> Active transition", events)
	}

	// Every call in this test passed -n standby: the transition was
	// observed from data the poll already had, never from a wake.
	for _, c := range runner.Calls() {
		if !containsArg(c.Args, "standby") {
			t.Fatalf("call %+v did not pass -n standby", c)
		}
	}
}

func TestLinuxProvider_SMART_TrendAcrossPolls(t *testing.T) {
	p, runner := newTestProvider(t)
	healthy := readFixture(t, "smartctl_ata_healthy.json")
	warning := readFixture(t, "smartctl_ata_warning_reallocated.json")

	runner.Script("smartctl", []string{"-j", "-n", "standby", "-a", "/dev/sda"}, healthy, nil)
	first, err := p.SMART(context.Background(), "/dev/sda", SMARTPollRespectStandby)
	if err != nil {
		t.Fatalf("SMART (first): %v", err)
	}
	if first.Trend != Stable {
		t.Fatalf("first poll Trend = %v, want Stable (nothing to compare against)", first.Trend)
	}

	runner.Script("smartctl", []string{"-j", "-n", "standby", "-a", "/dev/sda"}, warning, nil)
	second, err := p.SMART(context.Background(), "/dev/sda", SMARTPollRespectStandby)
	if err != nil {
		t.Fatalf("SMART (second): %v", err)
	}
	if second.Trend != Rising {
		t.Fatalf("second poll Trend = %v, want Rising (reallocated sectors 0 -> 8)", second.Trend)
	}
}

func TestLinuxProvider_SMART_ParseErrorSurfaced(t *testing.T) {
	p, runner := newTestProvider(t)
	runner.Script("smartctl", []string{"-j", "-n", "standby", "-a", "/dev/sda"}, []byte("not json"), nil)

	if _, err := p.SMART(context.Background(), "/dev/sda", SMARTPollRespectStandby); !errors.Is(err, ErrSMARTParse) {
		t.Fatalf("SMART: got %v, want ErrSMARTParse", err)
	}
}

func TestLinuxProvider_Spindown_RefusesBootDevice(t *testing.T) {
	p, _ := newTestProvider(t)
	if err := p.Spindown(context.Background(), "/dev/sda"); !errors.Is(err, ErrBootDevice) {
		t.Fatalf("Spindown(boot device): got %v, want ErrBootDevice", err)
	}
}

func TestLinuxProvider_Spindown_RecordsTransition(t *testing.T) {
	p, runner := newTestProvider(t)
	runner.Script("hdparm", []string{"-y", "/dev/sdb"}, nil, nil)
	p.Events.Observe("/dev/sdb", Active, time.Now())

	if err := p.Spindown(context.Background(), "/dev/sdb"); err != nil {
		t.Fatalf("Spindown: %v", err)
	}

	events := p.Events.Events("/dev/sdb")
	if len(events) != 1 || events[0].To != Standby {
		t.Fatalf("Events: got %+v, want one transition to Standby", events)
	}
}

func TestLinuxProvider_Format_RefusesBootDevice(t *testing.T) {
	p, _ := newTestProvider(t)
	if err := p.Format(context.Background(), "/dev/sda", XFS); !errors.Is(err, ErrBootDevice) {
		t.Fatalf("Format(boot device): got %v, want ErrBootDevice", err)
	}
}

func TestLinuxProvider_Format_ExecsMkfsXFS(t *testing.T) {
	p, runner := newTestProvider(t)
	runner.Script("mkfs.xfs", []string{"-f", "/dev/sdb"}, nil, nil)

	if err := p.Format(context.Background(), "/dev/sdb", XFS); err != nil {
		t.Fatalf("Format: %v", err)
	}

	calls := runner.Calls()
	if len(calls) != 1 || calls[0].Name != "mkfs.xfs" {
		t.Fatalf("Calls: got %+v, want one mkfs.xfs call", calls)
	}
	if !equalArgs(calls[0].Args, []string{"-f", "/dev/sdb"}) {
		t.Fatalf("Calls[0].Args = %v, want [-f /dev/sdb]", calls[0].Args)
	}
}

func TestLinuxProvider_Format_ExecsMkfsExt4(t *testing.T) {
	p, runner := newTestProvider(t)
	runner.Script("mkfs.ext4", []string{"-F", "/dev/sdb"}, nil, nil)

	if err := p.Format(context.Background(), "/dev/sdb", EXT4); err != nil {
		t.Fatalf("Format: %v", err)
	}
	if calls := runner.Calls(); len(calls) != 1 || calls[0].Name != "mkfs.ext4" {
		t.Fatalf("Calls: got %+v, want one mkfs.ext4 call", calls)
	}
}

func TestLinuxProvider_Format_ExecsMkfsBtrfs(t *testing.T) {
	p, runner := newTestProvider(t)
	runner.Script("mkfs.btrfs", []string{"-f", "/dev/sdb"}, nil, nil)

	if err := p.Format(context.Background(), "/dev/sdb", BTRFS); err != nil {
		t.Fatalf("Format: %v", err)
	}
	if calls := runner.Calls(); len(calls) != 1 || calls[0].Name != "mkfs.btrfs" {
		t.Fatalf("Calls: got %+v, want one mkfs.btrfs call", calls)
	}
}

func TestLinuxProvider_Format_RefusesUnsupportedFilesystem(t *testing.T) {
	p, runner := newTestProvider(t)
	if err := p.Format(context.Background(), "/dev/sdb", FilesystemType("zfs")); !errors.Is(err, ErrUnsupportedFilesystem) {
		t.Fatalf("Format(zfs): got %v, want ErrUnsupportedFilesystem", err)
	}
	if calls := runner.Calls(); len(calls) != 0 {
		t.Fatalf("Format(zfs) ran a command: %+v, want none", calls)
	}
}

func TestLinuxProvider_Format_SurfacesMkfsFailure(t *testing.T) {
	p, runner := newTestProvider(t)
	mkfsErr := errors.New("device or resource busy")
	runner.Script("mkfs.xfs", []string{"-f", "/dev/sdb"}, nil, mkfsErr)

	if err := p.Format(context.Background(), "/dev/sdb", XFS); !errors.Is(err, mkfsErr) {
		t.Fatalf("Format: got %v, want it to wrap %v", err, mkfsErr)
	}
}

func TestLinuxProvider_ImplementsProvider(t *testing.T) {
	var _ Provider = NewLinuxProvider()
}

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
