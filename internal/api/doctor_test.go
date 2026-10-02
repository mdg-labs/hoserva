package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/container"
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
	}, nil, nil, nil)
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
	}, nil, nil, nil)
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
	report := runDoctorChecks(context.Background(), nil, eng, func(string) (bool, error) { return false, nil }, nil, nil, nil)
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
	report := runDoctorChecks(context.Background(), nil, eng, func(string) (bool, error) { return false, nil }, nil, nil, nil)
	check := findDoctorCheck(report, "parity_freshness")
	if check.Status != apiv1.DoctorCheckStatusWarn {
		t.Fatalf("parity_freshness status = %q, want warn", check.Status)
	}
}

func TestParityFreshnessCheck_Red(t *testing.T) {
	eng := parity.NewFakeEngine()
	eng.SetStatus(parity.ParityStatus{Freshness: parity.FreshnessRed})
	report := runDoctorChecks(context.Background(), nil, eng, func(string) (bool, error) { return false, nil }, nil, nil, nil)
	check := findDoctorCheck(report, "parity_freshness")
	if check.Status != apiv1.DoctorCheckStatusFail {
		t.Fatalf("parity_freshness status = %q, want fail", check.Status)
	}
}

func TestParityFreshnessCheck_Error(t *testing.T) {
	eng := parity.NewFakeEngine()
	eng.FailStatus(errors.New("snapraid unavailable"))
	report := runDoctorChecks(context.Background(), nil, eng, func(string) (bool, error) { return false, nil }, nil, nil, nil)
	check := findDoctorCheck(report, "parity_freshness")
	if check.Status != apiv1.DoctorCheckStatusWarn {
		t.Fatalf("parity_freshness status = %q, want warn on error", check.Status)
	}
}

// TestParityFreshnessCheck_NilConcreteEngine reproduces #222: a nil
// *parity.SnapraidEngine wrapped in the parity.Engine interface (exactly
// how cmd/hoservad wires Handler.Parity when no snapraid.conf exists yet)
// does not compare equal to a bare nil, so calling eng.Status on it used
// to panic — turning GET /api/v1/doctor into a connection-dropping EOF
// for every client, including `hoserva doctor apply-host-config`.
func TestParityFreshnessCheck_NilConcreteEngine(t *testing.T) {
	var eng parity.Engine = (*parity.FakeEngine)(nil)
	report := runDoctorChecks(context.Background(), nil, eng, func(string) (bool, error) { return false, nil }, nil, nil, nil)
	check := findDoctorCheck(report, "parity_freshness")
	if check.Status != apiv1.DoctorCheckStatusWarn {
		t.Fatalf("parity_freshness status = %q, want warn for a nil concrete engine", check.Status)
	}
}

func TestSmartCheck_Healthy(t *testing.T) {
	f := disk.NewFakeProvider()
	f.AddDisk("/dev/sdb", disk.Disk{Device: "/dev/sdb", Size: disk.TB})
	f.SetSMART("/dev/sdb", disk.SMARTReport{SpinState: disk.Active})

	report := runDoctorChecks(context.Background(), f, nil, func(string) (bool, error) { return false, nil }, nil, nil, nil)
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

	report := runDoctorChecks(context.Background(), f, nil, func(string) (bool, error) { return false, nil }, nil, nil, nil)
	check := findDoctorCheck(report, "smart")
	if check.Status != apiv1.DoctorCheckStatusWarn {
		t.Fatalf("smart status = %q, want warn; message=%q", check.Status, check.Message)
	}
}

func TestSmartCheck_SelfTestFailedFails(t *testing.T) {
	f := disk.NewFakeProvider()
	f.AddDisk("/dev/sdb", disk.Disk{Device: "/dev/sdb", Size: disk.TB})
	f.SetSMART("/dev/sdb", disk.SMARTReport{SelfTestFailed: true, SpinState: disk.Active})

	report := runDoctorChecks(context.Background(), f, nil, func(string) (bool, error) { return false, nil }, nil, nil, nil)
	check := findDoctorCheck(report, "smart")
	if check.Status != apiv1.DoctorCheckStatusFail {
		t.Fatalf("smart status = %q, want fail; message=%q", check.Status, check.Message)
	}
}

func TestSmartCheck_StandbySkipped(t *testing.T) {
	f := disk.NewFakeProvider()
	f.AddDisk("/dev/sdb", disk.Disk{Device: "/dev/sdb", Size: disk.TB})
	f.SetSpinState("/dev/sdb", disk.Standby)

	report := runDoctorChecks(context.Background(), f, nil, func(string) (bool, error) { return false, nil }, nil, nil, nil)
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

// TestDockerEngineChecks_Unreachable proves a Docker Engine the fake
// reports as unreachable is warned about, not silently skipped or read
// as "no containers" — the doc 04 §3 acceptance test's first half.
func TestDockerEngineChecks_Unreachable(t *testing.T) {
	f := container.NewFakeProvider()
	f.SetUnavailable(nil)

	report := runDoctorChecks(context.Background(), nil, nil, func(string) (bool, error) { return false, nil }, nil, f, nil)
	check := findDoctorCheck(report, "docker")
	if check.Status != apiv1.DoctorCheckStatusWarn {
		t.Fatalf("docker status = %q, want warn for an unreachable Engine", check.Status)
	}
	if !check.Remediation.Set {
		t.Fatal("docker check carries no remediation for an unreachable Engine")
	}
}

// TestDockerEngineChecks_OldVersionWarns proves version negotiation
// against an old Engine produces the doc 04 §3 warning — the acceptance
// test's second half, scripted entirely through FakeProvider, never a
// real daemon.
func TestDockerEngineChecks_OldVersionWarns(t *testing.T) {
	f := container.NewFakeProvider()
	f.SetVersion(container.EngineVersion{Version: "18.09.0", APIVersion: "1.39", MinAPIVersion: "1.12"})

	report := runDoctorChecks(context.Background(), nil, nil, func(string) (bool, error) { return false, nil }, nil, f, nil)
	check := findDoctorCheck(report, "docker")
	if check.Status != apiv1.DoctorCheckStatusWarn {
		t.Fatalf("docker status = %q, want warn for Engine 18.09.0 (below the tested floor)", check.Status)
	}
	if !strings.Contains(check.Message, "18.09.0") {
		t.Fatalf("docker message = %q, want the installed version named", check.Message)
	}
}

// TestDockerEngineChecks_RecentVersionPasses is OldVersionWarns' control:
// a recent Engine must not trip the same warning.
func TestDockerEngineChecks_RecentVersionPasses(t *testing.T) {
	f := container.NewFakeProvider()
	f.SetVersion(container.EngineVersion{Version: "29.8.1", APIVersion: "1.56", MinAPIVersion: "1.24"})

	report := runDoctorChecks(context.Background(), nil, nil, func(string) (bool, error) { return false, nil }, nil, f, nil)
	check := findDoctorCheck(report, "docker")
	if check.Status != apiv1.DoctorCheckStatusPass {
		t.Fatalf("docker status = %q, want pass for a recent Engine; message=%q", check.Status, check.Message)
	}
}

// TestDockerEngineChecks_FloorIsTheFirstReleaseWithoutTheEscapeAdvisories
// pins the floor at 29.5.1, the first Engine release that fixes the four
// container-escape advisories: 28.5.2 (the last 28.x) and 29.5.0 warn,
// 29.5.1 passes. It goes through runDoctorChecks, the function the
// GET /api/v1/doctor handler serves.
func TestDockerEngineChecks_FloorIsTheFirstReleaseWithoutTheEscapeAdvisories(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    apiv1.DoctorCheckStatus
	}{
		{"28.5.2", apiv1.DoctorCheckStatusWarn},
		{"29.5.0", apiv1.DoctorCheckStatusWarn},
		{"29.5.1", apiv1.DoctorCheckStatusPass},
		{"29.8.1", apiv1.DoctorCheckStatusPass},
	} {
		t.Run(tc.version, func(t *testing.T) {
			f := container.NewFakeProvider()
			f.SetVersion(container.EngineVersion{Version: tc.version, APIVersion: "1.54", MinAPIVersion: "1.24"})

			report := runDoctorChecks(context.Background(), nil, nil, func(string) (bool, error) { return false, nil }, nil, f, nil)
			check := findDoctorCheck(report, "docker")
			if check.Status != tc.want {
				t.Fatalf("docker status for Engine %s = %q, want %q; message=%q", tc.version, check.Status, tc.want, check.Message)
			}
			if tc.want == apiv1.DoctorCheckStatusWarn {
				if !strings.Contains(check.Message, tc.version) || !strings.Contains(check.Message, "29.5.1") {
					t.Fatalf("docker message = %q, want the installed version and the floor named", check.Message)
				}
				if !check.Remediation.Set {
					t.Fatal("docker warning carries no upgrade remediation")
				}
			}
		})
	}
}

// TestComposeCheck_MissingPluginReported proves a missing Compose v2
// plugin is reported as its own warning, never silently skipped — the
// doc 04 §3 acceptance test's compose half. The fixture is verbatim
// stderr from a current Docker CLI ("Docker version 29.8.1, build
// 4a63305d74"), captured via `DOCKER_HOST=unix:///nonexistent.sock docker
// nosuchplugin version` and substituting "compose" for the probed name
// (see internal/container/compose.go's composeMissingPluginPatterns):
// a v20.10-era CLI's "is not a docker command" would pass this same
// check, but would not have caught the doctor check failing open against
// what a current install actually prints.
func TestComposeCheck_MissingPluginReported(t *testing.T) {
	f := container.NewFakeProvider()
	runner := container.NewFakeRunner()
	runner.Script("docker", []string{"compose", "version"}, nil,
		errors.New("docker: unknown command: docker compose\n\nRun 'docker --help' for more information"))

	report := runDoctorChecks(context.Background(), nil, nil, func(string) (bool, error) { return false, nil }, nil, f, runner)
	check := findDoctorCheck(report, "docker_compose")
	if check.Status != apiv1.DoctorCheckStatusWarn {
		t.Fatalf("docker_compose status = %q, want warn for a missing plugin", check.Status)
	}
	if !check.Remediation.Set {
		t.Fatal("docker_compose check carries no remediation for a missing plugin")
	}
}

func TestComposeCheck_PresentPasses(t *testing.T) {
	f := container.NewFakeProvider()
	runner := container.NewFakeRunner()
	runner.Script("docker", []string{"compose", "version"}, []byte("Docker Compose version 2.29.7\n"), nil)

	report := runDoctorChecks(context.Background(), nil, nil, func(string) (bool, error) { return false, nil }, nil, f, runner)
	check := findDoctorCheck(report, "docker_compose")
	if check.Status != apiv1.DoctorCheckStatusPass {
		t.Fatalf("docker_compose status = %q, want pass; message=%q", check.Status, check.Message)
	}
	if !strings.Contains(check.Message, "2.29.7") {
		t.Fatalf("docker_compose message = %q, want the version named", check.Message)
	}
}

// TestDockerUnavailableCheck_NotInstalled proves a host with no docker
// binary at all is told to install it, with the exact apt commands from
// Docker's own repository (D8) — not a bare URL.
func TestDockerUnavailableCheck_NotInstalled(t *testing.T) {
	check := dockerUnavailableCheckFor(func() bool { return false })
	if check.Status != apiv1.DoctorCheckStatusWarn {
		t.Fatalf("status = %q, want warn", check.Status)
	}
	if !strings.Contains(check.Message, "not installed") {
		t.Fatalf("message = %q, want it to say Docker is not installed", check.Message)
	}
	if !check.Remediation.Set || !strings.Contains(check.Remediation.Value, "apt-get install -y docker-ce") {
		t.Fatalf("remediation = %+v, want the exact docker-ce apt-get install command", check.Remediation)
	}
}

// TestDockerUnavailableCheck_InstalledButNotReachable is the finding this
// closes: a host where Docker is present but its socket cannot be reached
// (the concrete case is docker.service still blocked on
// BindsTo=hoserva-storage.target during a degraded, unacknowledged boot,
// Q69) must never be told to install Docker — it is already there.
func TestDockerUnavailableCheck_InstalledButNotReachable(t *testing.T) {
	check := dockerUnavailableCheckFor(func() bool { return true })
	if check.Status != apiv1.DoctorCheckStatusWarn {
		t.Fatalf("status = %q, want warn", check.Status)
	}
	if !strings.Contains(check.Message, "not reachable") {
		t.Fatalf("message = %q, want it to say Docker is not reachable", check.Message)
	}
	if strings.Contains(check.Message, "not installed") {
		t.Fatalf("message = %q, must not say Docker is not installed when it is", check.Message)
	}
	if !check.Remediation.Set || strings.Contains(check.Remediation.Value, "apt-get install -y docker-ce") {
		t.Fatalf("remediation = %+v, must not tell an already-installed host to install Docker", check.Remediation)
	}
}

// TestComposeCheck_OtherFailureNotReportedAsMissing proves a timeout (or
// any failure besides the Compose plugin being absent) is reported as
// itself, never misreported as "the plugin is missing" — ComposeVersion's
// ErrComposeUnavailable distinguishes the two, so the doctor check must
// use that distinction rather than treating every failure the same way.
func TestComposeCheck_OtherFailureNotReportedAsMissing(t *testing.T) {
	f := container.NewFakeProvider()
	runner := container.NewFakeRunner()
	runner.Script("docker", []string{"compose", "version"}, nil, errors.New("context deadline exceeded"))

	report := runDoctorChecks(context.Background(), nil, nil, func(string) (bool, error) { return false, nil }, nil, f, runner)
	check := findDoctorCheck(report, "docker_compose")
	if check.Status != apiv1.DoctorCheckStatusWarn {
		t.Fatalf("docker_compose status = %q, want warn", check.Status)
	}
	if strings.Contains(check.Message, "missing") {
		t.Fatalf("docker_compose message = %q, must not report a timeout as a missing plugin", check.Message)
	}
	if check.Remediation.Set {
		t.Fatalf("docker_compose remediation = %+v, want none for a failure that isn't a missing plugin", check.Remediation)
	}
}

func TestDockerEngineChecks_NilProviderNotConfigured(t *testing.T) {
	report := runDoctorChecks(context.Background(), nil, nil, func(string) (bool, error) { return false, nil }, nil, nil, nil)
	check := findDoctorCheck(report, "docker")
	if check.Status != apiv1.DoctorCheckStatusWarn {
		t.Fatalf("docker status = %q, want warn when no Provider is wired", check.Status)
	}
}

// TestDoctor_SharedNVMeLayout is doctor's side of a cache on the boot
// disk's spare partition (doc 01 §6): the boot NVMe is one boot block
// device, not a data disk; SMART is never polled on it; and the free-space
// check reads the root filesystem only, whatever the cache partition holds.
func TestDoctor_SharedNVMeLayout(t *testing.T) {
	f := disk.NewFakeProvider()
	f.AddDisk("/dev/nvme0n1", disk.Disk{
		Size: disk.TB, Boot: true, Serial: "S4EWNX0M123456X", ByIDName: "nvme-X",
		CachePartitions: []disk.CachePartition{{Device: "/dev/nvme0n1p3", Size: 900 * disk.GB, ByIDName: "nvme-X-part3", PartUUID: "5b3d9e0a-03", Reason: disk.ReasonSpareBootPartition}},
	})
	f.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB})
	f.SetSMART("/dev/sdb", disk.SMARTReport{SpinState: disk.Active})

	report := runDoctorChecks(context.Background(), f, nil, func(string) (bool, error) { return false, nil }, nil, nil, nil)

	if got := findDoctorCheck(report, "disks"); !strings.Contains(got.Message, "1 data disk(s) visible (2 total block devices)") {
		t.Fatalf("disk inventory message = %q, want the boot NVMe counted as a block device but not a data disk", got.Message)
	}
	for _, call := range f.SMARTCalls() {
		if call.Device != "/dev/sdb" {
			t.Fatalf("SMART polled %s; only the data disk may be polled", call.Device)
		}
	}
	if len(f.SMARTCalls()) != 1 {
		t.Fatalf("SMARTCalls = %+v, want one poll", f.SMARTCalls())
	}
	if got := findDoctorCheck(report, idBootSpace); !strings.Contains(got.Message, "boot filesystem") {
		t.Fatalf("boot space message = %q, want the root filesystem's free space", got.Message)
	}
}
