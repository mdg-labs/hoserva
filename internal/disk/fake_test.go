package disk

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestFakeProvider_List(t *testing.T) {
	f := NewFakeProvider()
	f.AddDisk("/dev/sdb", Disk{Size: 8 * TB, Model: "WD80EFZX", Serial: "WCC4N1234567"})
	f.AddDisk("/dev/sdc", Disk{Size: 4 * TB, Model: "ST4000DM", Serial: "Z1234567"})

	got, err := f.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List: got %d disks, want 2", len(got))
	}
	if got[0].Device != "/dev/sdb" || got[1].Device != "/dev/sdc" {
		t.Fatalf("List: not sorted by device: %+v", got)
	}
	if got[0].Size != 8*TB || got[0].Model != "WD80EFZX" {
		t.Fatalf("List: disk data not preserved: %+v", got[0])
	}
}

func TestFakeProvider_List_UnknownContextCancelled(t *testing.T) {
	f := NewFakeProvider()
	f.AddDisk("/dev/sdb", Disk{Size: TB})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := f.List(ctx); err == nil {
		t.Fatal("List with a cancelled context: got nil error")
	}
}

func TestFakeProvider_SMART_ScriptedTrend(t *testing.T) {
	f := NewFakeProvider()
	f.AddDisk("/dev/sdb", Disk{})
	f.SetSMART("/dev/sdb", SMARTReport{ReallocatedSectors: 4, Trend: Rising})

	got, err := f.SMART(context.Background(), "/dev/sdb", SMARTPollForce)
	if err != nil {
		t.Fatalf("SMART: %v", err)
	}
	if got.ReallocatedSectors != 4 || got.Trend != Rising {
		t.Fatalf("SMART: got %+v, want ReallocatedSectors=4 Trend=Rising", got)
	}
}

func TestFakeProvider_SMART_UnknownDisk(t *testing.T) {
	f := NewFakeProvider()
	if _, err := f.SMART(context.Background(), "/dev/nope", SMARTPollForce); !errors.Is(err, ErrDiskNotFound) {
		t.Fatalf("SMART on unknown disk: got %v, want ErrDiskNotFound", err)
	}
}

// TestSMARTPolling_NeverQueriesStandbyDiskInWakingMode is the test the issue
// asks for: SMART polling must never wake a standby disk (doc 02 §1, §4;
// doc 06 §2). It is written against the fake's own recorded call log and
// spin-state transitions, not just its return value, and it is paired with
// TestFakeProvider_SMART_ForceWakesStandbyDisk below so that the assertion
// is one a regression could actually fail: if RespectStandby ever started
// behaving like Force (or vice versa), one of the two tests would catch it.
func TestSMARTPolling_NeverQueriesStandbyDiskInWakingMode(t *testing.T) {
	f := NewFakeProvider()
	f.AddDisk("/dev/sdb", Disk{})
	f.AddDisk("/dev/sdc", Disk{})
	f.SetSpinState("/dev/sdb", Standby)
	f.SetSpinState("/dev/sdc", Active)

	devices := []string{"/dev/sdb", "/dev/sdc"}
	for _, dev := range devices {
		if _, err := f.SMART(context.Background(), dev, SMARTPollRespectStandby); err != nil {
			t.Fatalf("SMART(%s, RespectStandby): %v", dev, err)
		}
	}

	for _, call := range f.SMARTCalls() {
		if call.Mode == SMARTPollRespectStandby && call.Woke {
			t.Fatalf("SMART call %+v woke its disk under RespectStandby", call)
		}
	}

	state, err := f.SpinState("/dev/sdb")
	if err != nil {
		t.Fatalf("SpinState: %v", err)
	}
	if state != Standby {
		t.Fatalf("SpinState(/dev/sdb) after a respectful poll: got %v, want Standby", state)
	}
}

func TestFakeProvider_SMART_ForceWakesStandbyDisk(t *testing.T) {
	f := NewFakeProvider()
	f.AddDisk("/dev/sdb", Disk{})
	f.SetSpinState("/dev/sdb", Standby)

	report, err := f.SMART(context.Background(), "/dev/sdb", SMARTPollForce)
	if err != nil {
		t.Fatalf("SMART: %v", err)
	}
	if report.Skipped {
		t.Fatal("SMART(Force) on a standby disk: report marked Skipped, want a real query")
	}

	calls := f.SMARTCalls()
	if len(calls) != 1 || !calls[0].Woke {
		t.Fatalf("SMART(Force) call log: got %+v, want exactly one call with Woke=true", calls)
	}

	state, err := f.SpinState("/dev/sdb")
	if err != nil {
		t.Fatalf("SpinState: %v", err)
	}
	if state != Active {
		t.Fatalf("SpinState(/dev/sdb) after a forced poll: got %v, want Active", state)
	}
}

func TestFakeProvider_SMART_RespectStandbySkipsReport(t *testing.T) {
	f := NewFakeProvider()
	f.AddDisk("/dev/sdb", Disk{})
	f.SetSpinState("/dev/sdb", Standby)
	f.SetSMART("/dev/sdb", SMARTReport{ReallocatedSectors: 9})

	report, err := f.SMART(context.Background(), "/dev/sdb", SMARTPollRespectStandby)
	if err != nil {
		t.Fatalf("SMART: %v", err)
	}
	if !report.Skipped {
		t.Fatal("SMART(RespectStandby) on a standby disk: report not marked Skipped")
	}
	if report.ReallocatedSectors != 0 {
		t.Fatalf("SMART(RespectStandby) on a standby disk returned attribute data: %+v", report)
	}
}

func TestFakeProvider_FailAfter(t *testing.T) {
	f := NewFakeProvider()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f.Now = func() time.Time { return now }
	f.AddDisk("/dev/sdc", Disk{})
	f.FailAfter("/dev/sdc", 30*time.Second)

	if err := f.Spindown(context.Background(), "/dev/sdc"); err != nil {
		t.Fatalf("Spindown before the failure delay: %v", err)
	}

	now = now.Add(31 * time.Second)

	if err := f.Spindown(context.Background(), "/dev/sdc"); !errors.Is(err, ErrDiskFailed) {
		t.Fatalf("Spindown after the failure delay: got %v, want ErrDiskFailed", err)
	}
	if _, err := f.SMART(context.Background(), "/dev/sdc", SMARTPollForce); !errors.Is(err, ErrDiskFailed) {
		t.Fatalf("SMART after the failure delay: got %v, want ErrDiskFailed", err)
	}
	if err := f.Format(context.Background(), "/dev/sdc", XFS); !errors.Is(err, ErrDiskFailed) {
		t.Fatalf("Format after the failure delay: got %v, want ErrDiskFailed", err)
	}

	disks, err := f.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !disks[0].Failed {
		t.Fatalf("List after the failure delay: disk not marked Failed: %+v", disks[0])
	}
}

func TestFakeProvider_SlowDown(t *testing.T) {
	f := NewFakeProvider()
	f.AddDisk("/dev/sde", Disk{})
	f.SlowDown("/dev/sde", 10*time.Millisecond)

	var slept time.Duration
	f.Sleep = func(d time.Duration) { slept = d }

	if err := f.Spindown(context.Background(), "/dev/sde"); err != nil {
		t.Fatalf("Spindown: %v", err)
	}
	if slept != 10*time.Millisecond {
		t.Fatalf("Sleep called with %v, want 10ms", slept)
	}
}

func TestFakeProvider_Format(t *testing.T) {
	f := NewFakeProvider()
	f.AddDisk("/dev/sdd", Disk{})

	if err := f.Format(context.Background(), "/dev/sdd", XFS); err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs, ok := f.FormattedAs("/dev/sdd")
	if !ok || fs != XFS {
		t.Fatalf("FormattedAs: got (%v, %v), want (xfs, true)", fs, ok)
	}
}

func TestFakeProvider_ImplementsProvider(t *testing.T) {
	var _ Provider = NewFakeProvider()
}
