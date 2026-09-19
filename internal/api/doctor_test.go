package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/parity"
)

func findDoctorCheck(report *apiv1.DoctorReport, id string) apiv1.DoctorCheck {
	for _, c := range report.Checks {
		if c.ID == id {
			return c
		}
	}
	return apiv1.DoctorCheck{}
}

func TestMountStateCheck_Mounted(t *testing.T) {
	report := runDoctorChecks(context.Background(), nil, nil, func(string) (bool, error) {
		return true, nil
	})
	check := findDoctorCheck(report, "mount_state")
	if check.Status != apiv1.DoctorCheckStatusPass {
		t.Fatalf("mount_state status = %q, want pass", check.Status)
	}
	if check.Message == "" {
		t.Fatal("mount_state message is empty")
	}
}

func TestMountStateCheck_NotMounted(t *testing.T) {
	report := runDoctorChecks(context.Background(), nil, nil, func(string) (bool, error) {
		return false, nil
	})
	check := findDoctorCheck(report, "mount_state")
	if check.Status != apiv1.DoctorCheckStatusWarn {
		t.Fatalf("mount_state status = %q, want warn", check.Status)
	}
}

func TestParityFreshnessCheck_Green(t *testing.T) {
	eng := parity.NewFakeEngine()
	lastSync := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	eng.SetStatus(parity.ParityStatus{
		Freshness:  parity.FreshnessGreen,
		LastSyncAt: lastSync,
	})
	report := runDoctorChecks(context.Background(), nil, eng, func(string) (bool, error) { return false, nil })
	check := findDoctorCheck(report, "parity_freshness")
	if check.Status != apiv1.DoctorCheckStatusPass {
		t.Fatalf("parity_freshness status = %q, want pass", check.Status)
	}
}

func TestParityFreshnessCheck_Amber(t *testing.T) {
	eng := parity.NewFakeEngine()
	eng.SetStatus(parity.ParityStatus{
		Freshness:        parity.FreshnessAmber,
		ChangedSinceSync: 12,
	})
	report := runDoctorChecks(context.Background(), nil, eng, func(string) (bool, error) { return false, nil })
	check := findDoctorCheck(report, "parity_freshness")
	if check.Status != apiv1.DoctorCheckStatusWarn {
		t.Fatalf("parity_freshness status = %q, want warn", check.Status)
	}
}

func TestParityFreshnessCheck_Red(t *testing.T) {
	eng := parity.NewFakeEngine()
	eng.SetStatus(parity.ParityStatus{Freshness: parity.FreshnessRed})
	report := runDoctorChecks(context.Background(), nil, eng, func(string) (bool, error) { return false, nil })
	check := findDoctorCheck(report, "parity_freshness")
	if check.Status != apiv1.DoctorCheckStatusFail {
		t.Fatalf("parity_freshness status = %q, want fail", check.Status)
	}
}

func TestParityFreshnessCheck_Error(t *testing.T) {
	eng := parity.NewFakeEngine()
	eng.FailStatus(errors.New("snapraid unavailable"))
	report := runDoctorChecks(context.Background(), nil, eng, func(string) (bool, error) { return false, nil })
	check := findDoctorCheck(report, "parity_freshness")
	if check.Status != apiv1.DoctorCheckStatusWarn {
		t.Fatalf("parity_freshness status = %q, want warn on error", check.Status)
	}
}

func TestSmartCheck_Healthy(t *testing.T) {
	f := disk.NewFakeProvider()
	f.AddDisk("/dev/sdb", disk.Disk{Device: "/dev/sdb", Size: disk.TB})
	f.SetSMART("/dev/sdb", disk.SMARTReport{SpinState: disk.Active})

	report := runDoctorChecks(context.Background(), f, nil, func(string) (bool, error) { return false, nil })
	check := findDoctorCheck(report, "smart")
	if check.Status != apiv1.DoctorCheckStatusPass {
		t.Fatalf("smart status = %q, want pass; message=%q", check.Status, check.Message)
	}
	calls := f.SMARTCalls()
	if len(calls) != 1 || calls[0].Mode != disk.SMARTPollRespectStandby {
		t.Fatalf("SMARTCalls = %+v, want one RespectStandby poll", calls)
	}
}

func TestSmartCheck_ReallocatedWarns(t *testing.T) {
	f := disk.NewFakeProvider()
	f.AddDisk("/dev/sdb", disk.Disk{Device: "/dev/sdb", Size: disk.TB})
	f.SetSMART("/dev/sdb", disk.SMARTReport{ReallocatedSectors: 4, SpinState: disk.Active})

	report := runDoctorChecks(context.Background(), f, nil, func(string) (bool, error) { return false, nil })
	check := findDoctorCheck(report, "smart")
	if check.Status != apiv1.DoctorCheckStatusWarn {
		t.Fatalf("smart status = %q, want warn; message=%q", check.Status, check.Message)
	}
}

func TestSmartCheck_SelfTestFailedFails(t *testing.T) {
	f := disk.NewFakeProvider()
	f.AddDisk("/dev/sdb", disk.Disk{Device: "/dev/sdb", Size: disk.TB})
	f.SetSMART("/dev/sdb", disk.SMARTReport{SelfTestFailed: true, SpinState: disk.Active})

	report := runDoctorChecks(context.Background(), f, nil, func(string) (bool, error) { return false, nil })
	check := findDoctorCheck(report, "smart")
	if check.Status != apiv1.DoctorCheckStatusFail {
		t.Fatalf("smart status = %q, want fail; message=%q", check.Status, check.Message)
	}
}

func TestSmartCheck_StandbySkipped(t *testing.T) {
	f := disk.NewFakeProvider()
	f.AddDisk("/dev/sdb", disk.Disk{Device: "/dev/sdb", Size: disk.TB})
	f.SetSpinState("/dev/sdb", disk.Standby)

	report := runDoctorChecks(context.Background(), f, nil, func(string) (bool, error) { return false, nil })
	check := findDoctorCheck(report, "smart")
	if check.Status != apiv1.DoctorCheckStatusPass {
		t.Fatalf("smart status = %q, want pass for standby skip; message=%q", check.Status, check.Message)
	}
	if check.Message == "" || !strings.Contains(check.Message, "standby") {
		t.Fatalf("smart message = %q, want standby mentioned", check.Message)
	}
}

func TestVersionBelow_NumericAndEpoch(t *testing.T) {
	if versionBelow("12.10", "12.4") {
		t.Fatal("12.10 should not be below 12.4")
	}
	if !versionBelow("2.9", "2.40.2") {
		t.Fatal("2.9 should be below 2.40.2")
	}
	if versionBelow("1:2.40.2", "2.40.2") {
		t.Fatal("epoch-prefixed 2.40.2 should meet the floor")
	}
	if !versionBelow("12.4", "12.4.1") {
		t.Fatal("12.4 should be below 12.4.1")
	}
}
