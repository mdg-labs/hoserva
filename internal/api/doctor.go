package api

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/pool"
)

const (
	mergerfsMinVersion = "2.40.2"
	snapraidMinVersion = "12.4"
	doctorProbeTimeout = 8 * time.Second
)

// DiskLister enumerates block devices for doctor and disk list handlers.
type DiskLister interface {
	List(ctx context.Context) ([]disk.Disk, error)
}

type mountProbe func(path string) (bool, error)

func runDoctorChecks(ctx context.Context, disks disk.Provider, parityEng parity.Engine, probe mountProbe, host *config.HostInventory) *apiv1.DoctorReport {
	if probe == nil {
		probe = pathIsMountpoint
	}
	checks := []apiv1.DoctorCheck{
		packageVersionCheck(ctx, "mergerfs", mergerfsMinVersion),
		packageVersionCheck(ctx, "snapraid", snapraidMinVersion),
		dockerCheck(ctx),
		bootSpaceCheck(),
		mountStateCheck(probe, pool.CatchAllPath),
		parityFreshnessCheck(ctx, parityEng),
	}
	if disks != nil {
		checks = append(checks, diskInventoryCheck(ctx, disks))
		checks = append(checks, smartCheck(ctx, disks))
	}
	checks = append(checks, hostConfigChecks(host)...)
	overall := apiv1.DoctorCheckStatusPass
	for _, c := range checks {
		if c.Status == apiv1.DoctorCheckStatusFail {
			overall = apiv1.DoctorCheckStatusFail
			break
		}
		if c.Status == apiv1.DoctorCheckStatusWarn && overall == apiv1.DoctorCheckStatusPass {
			overall = apiv1.DoctorCheckStatusWarn
		}
	}
	return &apiv1.DoctorReport{Overall: overall, Checks: checks}
}

func packageVersionCheck(ctx context.Context, pkg, minVersion string) apiv1.DoctorCheck {
	id := "pkg_" + pkg
	out, err := doctorCommandOutput(ctx, "dpkg-query", "-W", "-f=${Version}", pkg)
	if err != nil {
		return apiv1.DoctorCheck{
			ID:          id,
			Name:        pkg + " package",
			Status:      apiv1.DoctorCheckStatusFail,
			Message:     fmt.Sprintf("%s is not installed", pkg),
			Remediation: apiv1.NewOptNilString(fmt.Sprintf("Install the Debian %s package", pkg)),
		}
	}
	ver := strings.TrimSpace(string(out))
	status := apiv1.DoctorCheckStatusPass
	msg := fmt.Sprintf("%s %s is installed", pkg, ver)
	var remediation apiv1.OptNilString
	if versionBelow(ver, minVersion) {
		status = apiv1.DoctorCheckStatusWarn
		msg = fmt.Sprintf("%s %s is below the tested floor %s", pkg, ver, minVersion)
		remediation = apiv1.NewOptNilString(fmt.Sprintf("Upgrade %s to at least %s", pkg, minVersion))
	}
	return apiv1.DoctorCheck{
		ID: id, Name: pkg + " version", Status: status, Message: msg, Remediation: remediation,
	}
}

func dockerCheck(ctx context.Context) apiv1.DoctorCheck {
	if _, err := exec.LookPath("docker"); err != nil {
		return apiv1.DoctorCheck{
			ID:          "docker",
			Name:        "Docker Engine",
			Status:      apiv1.DoctorCheckStatusWarn,
			Message:     "Docker is not installed — Apps will not be available",
			Remediation: apiv1.NewOptNilString("Install Docker Engine and the Compose v2 plugin"),
		}
	}
	out, err := doctorCommandOutput(ctx, "docker", "compose", "version", "--short")
	if err != nil {
		return apiv1.DoctorCheck{
			ID:          "docker_compose",
			Name:        "Docker Compose plugin",
			Status:      apiv1.DoctorCheckStatusWarn,
			Message:     "The Compose v2 plugin is missing",
			Remediation: apiv1.NewOptNilString("Install docker-compose-plugin"),
		}
	}
	return apiv1.DoctorCheck{
		ID:      "docker",
		Name:    "Docker",
		Status:  apiv1.DoctorCheckStatusPass,
		Message: fmt.Sprintf("Docker is present (compose %s)", strings.TrimSpace(string(out))),
	}
}

func bootSpaceCheck() apiv1.DoctorCheck {
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err != nil {
		return apiv1.DoctorCheck{
			ID:      "boot_space",
			Name:    "Boot device free space",
			Status:  apiv1.DoctorCheckStatusWarn,
			Message: fmt.Sprintf("Could not read free space on /: %v", err),
		}
	}
	free := st.Bavail * uint64(st.Bsize)
	gb := free / (1024 * 1024 * 1024)
	status := apiv1.DoctorCheckStatusPass
	msg := fmt.Sprintf("%d GiB free on the boot filesystem", gb)
	var remediation apiv1.OptNilString
	if gb < 5 {
		status = apiv1.DoctorCheckStatusWarn
		msg = fmt.Sprintf("Only %d GiB free on the boot filesystem", gb)
		remediation = apiv1.NewOptNilString("Free space on the boot device before installing apps or taking backups")
	}
	return apiv1.DoctorCheck{
		ID: idBootSpace, Name: "Boot device free space", Status: status, Message: msg, Remediation: remediation,
	}
}

const idBootSpace = "boot_space"

func diskInventoryCheck(ctx context.Context, lister DiskLister) apiv1.DoctorCheck {
	disks, err := lister.List(ctx)
	if err != nil {
		return apiv1.DoctorCheck{
			ID:      "disks",
			Name:    "Disk inventory",
			Status:  apiv1.DoctorCheckStatusWarn,
			Message: fmt.Sprintf("Could not enumerate disks: %v", err),
		}
	}
	data := 0
	for _, d := range disks {
		if !d.Boot {
			data++
		}
	}
	return apiv1.DoctorCheck{
		ID:      "disks",
		Name:    "Disk inventory",
		Status:  apiv1.DoctorCheckStatusPass,
		Message: fmt.Sprintf("%d data disk(s) visible (%d total block devices)", data, len(disks)),
	}
}

func versionBelow(installed, minimum string) bool {
	return cmpDebianVersion(installed, minimum) < 0
}

func cmpDebianVersion(a, b string) int {
	ap, bp := debianVersionParts(a), debianVersionParts(b)
	n := len(ap)
	if len(bp) > n {
		n = len(bp)
	}
	for i := 0; i < n; i++ {
		var av, bv int
		if i < len(ap) {
			av = ap[i]
		}
		if i < len(bp) {
			bv = bp[i]
		}
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
	}
	return 0
}

func debianVersionParts(v string) []int {
	v = strings.TrimSpace(v)
	if i := strings.IndexByte(v, ':'); i >= 0 {
		v = v[i+1:]
	}
	v = strings.SplitN(v, "-", 2)[0]
	v = strings.SplitN(v, "+", 2)[0]
	var parts []int
	for _, p := range strings.Split(v, ".") {
		n := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				break
			}
			n = n*10 + int(c-'0')
		}
		parts = append(parts, n)
	}
	return parts
}

func doctorCommandOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, doctorProbeTimeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}

func mountStateCheck(probe mountProbe, path string) apiv1.DoctorCheck {
	mounted, err := probe(path)
	if err != nil {
		return apiv1.DoctorCheck{
			ID:      "mount_state",
			Name:    "Pool mount",
			Status:  apiv1.DoctorCheckStatusWarn,
			Message: fmt.Sprintf("Could not check whether %s is mounted: %v", path, err),
		}
	}
	if mounted {
		return apiv1.DoctorCheck{
			ID:      "mount_state",
			Name:    "Pool mount",
			Status:  apiv1.DoctorCheckStatusPass,
			Message: fmt.Sprintf("The storage pool is mounted at %s", path),
		}
	}
	return apiv1.DoctorCheck{
		ID:          "mount_state",
		Name:        "Pool mount",
		Status:      apiv1.DoctorCheckStatusWarn,
		Message:     fmt.Sprintf("The storage pool is not mounted at %s", path),
		Remediation: apiv1.NewOptNilString("Start the array from the Web UI or wait for the pool service to come up"),
	}
}

func parityFreshnessCheck(ctx context.Context, eng parity.Engine) apiv1.DoctorCheck {
	if engineUnavailable(eng) {
		return apiv1.DoctorCheck{
			ID:      "parity_freshness",
			Name:    "Parity freshness",
			Status:  apiv1.DoctorCheckStatusWarn,
			Message: "Parity status is not available on this daemon",
		}
	}
	status, err := eng.Status(ctx)
	if err != nil {
		return apiv1.DoctorCheck{
			ID:          "parity_freshness",
			Name:        "Parity freshness",
			Status:      apiv1.DoctorCheckStatusWarn,
			Message:     fmt.Sprintf("Could not read parity status: %v", err),
			Remediation: apiv1.NewOptNilString("Check that SnapRAID is configured and its content file is readable"),
		}
	}
	switch status.Freshness {
	case parity.FreshnessRed:
		return apiv1.DoctorCheck{
			ID:          "parity_freshness",
			Name:        "Parity freshness",
			Status:      apiv1.DoctorCheckStatusFail,
			Message:     "Parity has errors that need attention before the next sync",
			Remediation: apiv1.NewOptNilString("Run hoserva fix after reviewing the parity dashboard"),
		}
	case parity.FreshnessAmber:
		msg := fmt.Sprintf("%d file(s) changed since the last sync", status.ChangedSinceSync)
		if !status.LastSyncAt.IsZero() {
			msg = fmt.Sprintf("%s (last sync %s)", msg, status.LastSyncAt.UTC().Format(time.RFC3339))
		}
		return apiv1.DoctorCheck{
			ID:          "parity_freshness",
			Name:        "Parity freshness",
			Status:      apiv1.DoctorCheckStatusWarn,
			Message:     msg,
			Remediation: apiv1.NewOptNilString("Run hoserva sync when you are ready to update parity"),
		}
	default:
		msg := "Parity is up to date"
		if !status.LastSyncAt.IsZero() {
			msg = fmt.Sprintf("Parity is up to date (last sync %s)", status.LastSyncAt.UTC().Format(time.RFC3339))
		}
		return apiv1.DoctorCheck{
			ID:      "parity_freshness",
			Name:    "Parity freshness",
			Status:  apiv1.DoctorCheckStatusPass,
			Message: msg,
		}
	}
}

// engineUnavailable reports whether eng is unusable: either a plain nil
// interface, or a nil concrete pointer wrapped in a non-nil parity.Engine
// (cmd/hoservad wires Handler.Parity from a *SnapraidEngine that is nil
// when no snapraid.conf exists yet — a fresh install before the array is
// configured, exactly onboarding's own situation). A typed nil does not
// compare equal to the bare nil above, so calling a method on it reaches
// SnapraidEngine's nil receiver and panics; this check catches it before
// eng.Status is ever called (#222).
func engineUnavailable(eng parity.Engine) bool {
	if eng == nil {
		return true
	}
	v := reflect.ValueOf(eng)
	return v.Kind() == reflect.Pointer && v.IsNil()
}

func smartCheck(ctx context.Context, provider disk.Provider) apiv1.DoctorCheck {
	disks, err := provider.List(ctx)
	if err != nil {
		return apiv1.DoctorCheck{
			ID:      "smart",
			Name:    "Disk health (SMART)",
			Status:  apiv1.DoctorCheckStatusWarn,
			Message: fmt.Sprintf("Could not enumerate disks for SMART: %v", err),
		}
	}
	var polled, skipped, failed int
	var warnings []string
	for _, d := range disks {
		if d.Boot {
			continue
		}
		report, err := provider.SMART(ctx, d.Device, disk.SMARTPollRespectStandby)
		if err != nil {
			failed++
			warnings = append(warnings, fmt.Sprintf("%s: %v", d.Device, err))
			continue
		}
		if report.Skipped {
			skipped++
			continue
		}
		polled++
		switch smartSeverity(report) {
		case apiv1.DoctorCheckStatusFail:
			failed++
			warnings = append(warnings, smartIssueMessage(d.Device, report))
		case apiv1.DoctorCheckStatusWarn:
			warnings = append(warnings, smartIssueMessage(d.Device, report))
		}
	}
	if failed > 0 {
		return apiv1.DoctorCheck{
			ID:          "smart",
			Name:        "Disk health (SMART)",
			Status:      apiv1.DoctorCheckStatusFail,
			Message:     fmt.Sprintf("%d disk(s) report SMART failures (%s)", failed, strings.Join(warnings, "; ")),
			Remediation: apiv1.NewOptNilString("Review disk health in the Web UI and plan replacement for any failing drive"),
		}
	}
	if len(warnings) > 0 {
		return apiv1.DoctorCheck{
			ID:          "smart",
			Name:        "Disk health (SMART)",
			Status:      apiv1.DoctorCheckStatusWarn,
			Message:     strings.Join(warnings, "; "),
			Remediation: apiv1.NewOptNilString("Keep an eye on disks with rising error counts"),
		}
	}
	msg := fmt.Sprintf("SMART looks healthy on %d disk(s)", polled)
	if skipped > 0 {
		msg = fmt.Sprintf("%s (%d left in standby and not queried)", msg, skipped)
	}
	if polled == 0 && skipped == 0 {
		msg = "No data disks are available to query"
	}
	return apiv1.DoctorCheck{
		ID:      "smart",
		Name:    "Disk health (SMART)",
		Status:  apiv1.DoctorCheckStatusPass,
		Message: msg,
	}
}

func smartSeverity(report disk.SMARTReport) apiv1.DoctorCheckStatus {
	if report.SelfTestFailed || report.OfflineUncorrectable > 0 || report.PendingSectors > 0 {
		return apiv1.DoctorCheckStatusFail
	}
	if report.ReallocatedSectors > 0 || report.CRCErrors > 0 || report.Trend == disk.Rising {
		return apiv1.DoctorCheckStatusWarn
	}
	return apiv1.DoctorCheckStatusPass
}

func smartIssueMessage(dev string, report disk.SMARTReport) string {
	switch {
	case report.SelfTestFailed:
		return fmt.Sprintf("%s: SMART self-test failed", dev)
	case report.OfflineUncorrectable > 0:
		return fmt.Sprintf("%s: %d offline uncorrectable sector(s)", dev, report.OfflineUncorrectable)
	case report.PendingSectors > 0:
		return fmt.Sprintf("%s: %d pending sector(s)", dev, report.PendingSectors)
	case report.ReallocatedSectors > 0:
		return fmt.Sprintf("%s: %d reallocated sector(s)", dev, report.ReallocatedSectors)
	case report.CRCErrors > 0:
		return fmt.Sprintf("%s: %d CRC error(s)", dev, report.CRCErrors)
	case report.Trend == disk.Rising:
		return fmt.Sprintf("%s: SMART error counts are rising", dev)
	default:
		return fmt.Sprintf("%s: SMART warning", dev)
	}
}

func pathIsMountpoint(path string) (bool, error) {
	dev, err := pathDeviceID(path)
	if err != nil {
		return false, err
	}
	parentDev, err := pathDeviceID(filepath.Dir(path))
	if err != nil {
		return false, err
	}
	return dev != parentDev, nil
}

func pathDeviceID(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("cannot determine device id for %s", path)
	}
	return uint64(st.Dev), nil
}

func (h *Handler) detectHost(ctx context.Context) *config.HostInventory {
	if h.Generator == nil {
		return nil
	}
	inv, err := config.Detect(ctx, h.Generator.Root, h.Docker)
	if err != nil {
		return nil
	}
	return &inv
}

func (h *Handler) RunDoctor(ctx context.Context) (*apiv1.DoctorReport, error) {
	engine, _, _, _ := h.CurrentParity()
	return runDoctorChecks(ctx, h.Disks, engine, nil, h.detectHost(ctx)), nil
}

func (h *Handler) GetStatus(ctx context.Context) (*apiv1.SystemStatus, error) {
	engine, _, _, _ := h.CurrentParity()
	report := runDoctorChecks(ctx, h.Disks, engine, nil, h.detectHost(ctx))
	healthy := report.Overall != apiv1.DoctorCheckStatusFail
	summary := "All checks passed"
	if !healthy {
		summary = "One or more checks failed — run hoserva doctor for details"
	} else if report.Overall == apiv1.DoctorCheckStatusWarn {
		summary = "System is running with warnings — run hoserva doctor for details"
	}
	active := int32(0)
	if h.Store != nil {
		running := job.StatusRunning
		jobs, err := h.Store.List(ctx, job.ListFilter{Status: &running})
		if err == nil {
			active = int32(len(jobs))
		}
	}
	maintenance := false
	if h.Scheduler != nil {
		maintenance = h.Scheduler.InMaintenance()
	}
	return &apiv1.SystemStatus{
		Healthy:         healthy,
		Summary:         summary,
		ActiveJobs:      apiv1.NewOptInt32(active),
		MaintenanceMode: apiv1.NewOptBool(maintenance),
	}, nil
}
