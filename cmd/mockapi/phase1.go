package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
)

const mockDiskSize = 4_000_000_000_000

func errConfirmRequired() error {
	return &mockError{code: "confirmation_required", statusCode: 409, message: "this operation requires an explicit confirmation"}
}

func errInvalidPlan(err error) error {
	return &mockError{code: "invalid_plan", statusCode: 400, message: err.Error()}
}

func errRootOnlyRecovery() error {
	return &mockError{code: "forbidden", statusCode: 403, message: "this operation requires root over the Unix socket"}
}

func mockDiskInventory(scenario string) []apiv1.DiskInventoryEntry {
	if scenario == "fresh-install" {
		return []apiv1.DiskInventoryEntry{
			{
				Device:          "/dev/sdb",
				SizeBytes:       mockDiskSize,
				Model:           apiv1.NewOptString("WDC WD40EFRX"),
				Serial:          apiv1.NewOptString("WD-WCC4E1234567"),
				Filesystem:      apiv1.NewOptString("xfs"),
				Label:           apiv1.NewOptString("disk1"),
				SmartStatus:     apiv1.NewOptString("ok"),
				ContainsData:    apiv1.NewOptBool(true),
				LooksLikeUnraid: apiv1.NewOptBool(true),
			},
			{
				Device:          "/dev/sdc",
				SizeBytes:       mockDiskSize,
				Model:           apiv1.NewOptString("WDC WD40EFRX"),
				Serial:          apiv1.NewOptString("WD-WCC4E7654321"),
				SmartStatus:     apiv1.NewOptString("ok"),
				ContainsData:    apiv1.NewOptBool(false),
				LooksLikeUnraid: apiv1.NewOptBool(false),
			},
			mockUSBDisk(),
		}
	}

	disks := []apiv1.DiskInventoryEntry{
		{
			Device:    "/dev/sdb",
			SizeBytes: mockDiskSize,
			Model:     apiv1.NewOptString("WDC WD40EFRX"),
			Serial:    apiv1.NewOptString("WD-WCC4E1234567"),
		},
		{
			Device:    "/dev/sdc",
			SizeBytes: mockDiskSize,
			Model:     apiv1.NewOptString("WDC WD40EFRX"),
			Serial:    apiv1.NewOptString("WD-WCC4E7654321"),
		},
		{
			Device:    "/dev/sdd",
			SizeBytes: mockDiskSize,
			Model:     apiv1.NewOptString("WDC WD40EFRX"),
			Serial:    apiv1.NewOptString("WD-WCC4E9999999"),
		},
		{
			Device:    "/dev/sde",
			SizeBytes: mockDiskSize,
			Model:     apiv1.NewOptString("WDC WD140EFGX"),
			Serial:    apiv1.NewOptString("WD-WCC7E0000001"),
		},
		// Not in mockArrayDisks — the spare candidate `disk add`/`disk
		// replace` demos exercise against (#288).
		{
			Device:    "/dev/sdf",
			SizeBytes: mockDiskSize,
			Model:     apiv1.NewOptString("WDC WD40EFRX"),
			Serial:    apiv1.NewOptString("WD-WCC4E1111111"),
		},
	}
	if scenario == "degraded" {
		disks[2].Failed = apiv1.NewOptBool(true)
	}
	if scenario == "sync-blocked" {
		// disk5 mirrors mockArrayDisks's own "sync-blocked" scenario
		// (#361): already evacuated, so `disk remove finish`/the pool
		// page's Finish removal have a disk they can actually run to
		// success against the mock — the one gap #358's executor found
		// (no scenario had an evacuated disk).
		disks = append(disks, apiv1.DiskInventoryEntry{
			Device: "/dev/sdg", SizeBytes: mockDiskSize,
			Model: apiv1.NewOptString("WDC WD40EFRX"), Serial: apiv1.NewOptString("WD-WCC4E2222222"),
		})
	}
	return append(disks, mockUSBDisk())
}

func mockPoolStatus(scenario string) *apiv1.PoolStatus {
	if scenario == "fresh-install" {
		return &apiv1.PoolStatus{Mounted: false, Disks: nil}
	}

	size := apiv1.NewOptNilInt64(mockDiskSize)
	used := apiv1.NewOptNilInt64(mockDiskSize / 2)
	disks := []apiv1.PoolDiskEntry{
		{
			Device:     "/dev/sdb",
			MountPoint: "/mnt/disk1",
			Role:       apiv1.PoolDiskEntryRoleData,
			State:      apiv1.DiskStateActive,
			SizeBytes:  size,
			UsedBytes:  used,
		},
		{
			Device:     "/dev/sdc",
			MountPoint: "/mnt/disk2",
			Role:       apiv1.PoolDiskEntryRoleData,
			State:      apiv1.DiskStateActive,
			SizeBytes:  size,
			UsedBytes:  used,
		},
		{
			Device:     "/dev/sdd",
			MountPoint: "/mnt/disk3",
			Role:       apiv1.PoolDiskEntryRoleData,
			State:      apiv1.DiskStateActive,
			SizeBytes:  size,
			UsedBytes:  used,
		},
		{
			Device:     "/dev/sde",
			MountPoint: "/mnt/parity",
			Role:       apiv1.PoolDiskEntryRoleParity,
			State:      apiv1.DiskStateActive,
			SizeBytes:  size,
			UsedBytes:  apiv1.NewOptNilInt64(mockDiskSize / 10),
		},
	}
	if scenario == "degraded" {
		disks[1].State = apiv1.DiskStateFailed
		// disk3 mirrors mockArrayDisks's own "degraded" scenario (#359,
		// doc 09 §4 step 2): mid-evacuation, no-create.
		disks[2].RemovalState = apiv1.NewOptNilDiskRemovalState(apiv1.DiskRemovalStateEvacuating)
		disks[2].FinishConfirmation = apiv1.NewOptString(job.EvacuationConfirmation(disks[2].MountPoint))
		// A stored array member with no identity match in inventory at
		// all (#326) — the literal "failed disk" scenario doc 02 §4
		// describes, mirroring production GetPool's shape: stored
		// device/role/mountpoint, no size/used/free.
		disks = append(disks, apiv1.PoolDiskEntry{
			Device:     "/dev/sdx",
			MountPoint: "/mnt/disk4",
			Role:       apiv1.PoolDiskEntryRoleData,
			State:      apiv1.DiskStateMissing,
		})
	}
	if scenario == "sync-blocked" {
		// Mirrors mockArrayDisks's own "sync-blocked" scenario (#361):
		// disk5 already evacuated and ready for `finishDiskRemoval` —
		// the one removal state no scenario had a disk in before this,
		// so the pool page's Finish removal action has something to
		// demo a real success against.
		disks = append(disks, apiv1.PoolDiskEntry{
			Device: "/dev/sdg", MountPoint: "/mnt/disk5", Role: apiv1.PoolDiskEntryRoleData,
			State: apiv1.DiskStateActive, SizeBytes: size, UsedBytes: used,
			RemovalState:       apiv1.NewOptNilDiskRemovalState(apiv1.DiskRemovalStateEvacuated),
			FinishConfirmation: apiv1.NewOptString(job.EvacuationConfirmation("/mnt/disk5")),
		})
	}
	return &apiv1.PoolStatus{Mounted: true, Disks: disks}
}

func (h *handler) countActiveJobs() int32 {
	var active int32
	for _, job := range h.jobs {
		switch job.Status {
		case apiv1.JobStatusQueued, apiv1.JobStatusRunning:
			active++
		}
	}
	return active
}

func mockDoctorReport(scenario string) *apiv1.DoctorReport {
	pass := func(id, name, msg string) apiv1.DoctorCheck {
		return apiv1.DoctorCheck{ID: id, Name: name, Status: apiv1.DoctorCheckStatusPass, Message: msg}
	}
	warn := func(id, name, msg string) apiv1.DoctorCheck {
		return apiv1.DoctorCheck{ID: id, Name: name, Status: apiv1.DoctorCheckStatusWarn, Message: msg}
	}
	fail := func(id, name, msg string) apiv1.DoctorCheck {
		return apiv1.DoctorCheck{ID: id, Name: name, Status: apiv1.DoctorCheckStatusFail, Message: msg}
	}

	checks := []apiv1.DoctorCheck{
		pass("pkg_mergerfs", "mergerfs version", "mergerfs 2.40.2 is installed"),
		pass("pkg_snapraid", "snapraid version", "snapraid 12.4 is installed"),
		pass("docker", "Docker", "Docker is present (compose 2.24.0)"),
		pass("boot_space", "Boot device free space", "42 GiB free on the boot filesystem"),
	}

	switch scenario {
	case "fresh-install":
		checks = append(checks,
			warn("mounts", "Pool mount", "The data pool is not mounted yet"),
			warn("parity_freshness", "Parity freshness", "No parity sync has run yet"),
			pass("host_samba", "Samba shares", "1 share: media"),
			pass("host_nfs", "NFS exports", "1 export: /export/media"),
			pass("host_fstab", "fstab mounts", "1 mount: /mnt/media"),
			pass("host_docker_containers", "Docker containers", "1 container: jellyfin"),
			pass("host_docker_images", "Docker images", "1 image: linuxserver/jellyfin:latest"),
		)
	case "degraded":
		checks = append(checks,
			pass("mounts", "Pool mount", "The data pool is mounted at /mnt/user"),
			fail("parity_integrity", "Parity integrity", "Parity check found 3 mismatched blocks on disk2 — the array is degraded until a sync clears them."),
		)
	case "sync-blocked":
		checks = append(checks,
			pass("mounts", "Pool mount", "The data pool is mounted at /mnt/user"),
			warn("threshold_guard", "Threshold guard", "The free-space threshold guard is tripped on disk3 (doc 02 §2) — clear space or lower the threshold, then sync again."),
		)
	case "rebuilding":
		checks = append(checks,
			pass("mounts", "Pool mount", "The data pool is mounted at /mnt/user"),
			warn("parity_rebuild", "Parity rebuild", "A fix job is rewriting data from parity"),
		)
	case "migration-pending":
		checks = append(checks,
			pass("mounts", "Pool mount", "The data pool is mounted at /mnt/user"),
			pass("parity_freshness", "Parity freshness", "Parity was synced within the last 24 hours"),
			warn("migration", "VM migration", "A VM migration import job is queued"),
		)
	default:
		checks = append(checks,
			pass("mounts", "Pool mount", "The data pool is mounted at /mnt/user"),
			pass("parity_freshness", "Parity freshness", "Parity was synced within the last 24 hours"),
		)
	}

	overall := apiv1.DoctorCheckStatusPass
	for _, check := range checks {
		if check.Status == apiv1.DoctorCheckStatusFail {
			overall = apiv1.DoctorCheckStatusFail
			break
		}
		if check.Status == apiv1.DoctorCheckStatusWarn && overall == apiv1.DoctorCheckStatusPass {
			overall = apiv1.DoctorCheckStatusWarn
		}
	}
	return &apiv1.DoctorReport{Overall: overall, Checks: checks}
}

// mockSystemStatus mirrors production GetStatus's own arrayDegraded/
// arrayDegradedAcknowledged derivation (internal/api's degradedGate +
// disk.StorageGate.Missing()/Ready()): arrayDegraded is true for the
// "degraded" scenario's missing disk4 (mockPoolStatus) regardless of
// acknowledgement — acknowledging must never report a still-degraded
// array as healthy (#385 finding 2) — and arrayDegradedAcknowledged is
// what flips once acknowledgeDegraded (this mock's own stand-in for
// Acknowledge) sets degradedAcknowledged.
func mockSystemStatus(scenario string, activeJobs int32, maintenance, degradedAcknowledged bool) *apiv1.SystemStatus {
	report := mockDoctorReport(scenario)
	healthy := report.Overall != apiv1.DoctorCheckStatusFail
	summary := "All checks passed"
	if !healthy {
		summary = "One or more checks failed — run hoserva doctor for details"
	} else if report.Overall == apiv1.DoctorCheckStatusWarn {
		summary = "System is running with warnings — run hoserva doctor for details"
	}

	status := &apiv1.SystemStatus{
		Healthy:         healthy,
		Summary:         summary,
		ActiveJobs:      apiv1.NewOptInt32(activeJobs),
		MaintenanceMode: apiv1.NewOptBool(maintenance),
		// storageServicesReleased (#385 finding 2) mirrors
		// storageTargetSync.Ready() without a real gate behind this
		// mock: outside the "degraded" scenario nothing ever gated the
		// dependent services, so they are released whenever the array
		// itself is not in maintenance; in the "degraded" scenario they
		// are only released once AcknowledgeDegradedArray has actually
		// succeeded (degradedAcknowledged) and the array is not
		// currently in maintenance — an acknowledge attempted during
		// maintenance sets degradedAcknowledged but is refused
		// (array_services_not_started) before this ever reports true.
		StorageServicesReleased: apiv1.NewOptBool(!maintenance && (scenario != "degraded" || degradedAcknowledged)),
	}
	if scenario == "degraded" {
		status.ArrayDegraded = apiv1.NewOptBool(true)
		status.ArrayDegradedAcknowledged = apiv1.NewOptBool(degradedAcknowledged)
	}
	if scenario == "sync-blocked" {
		status.ParityBlocked = apiv1.NewOptBool(true)
	}
	return status
}

// submitParityJob is called with h.mu already held. Production's
// Scheduler.Submit refuses every job type but TypeDiskUpgradeData while
// maintenance mode is active (Q70).
func (h *handler) submitParityJob(jobType apiv1.JobType, cancellable bool) (*apiv1.Job, error) {
	if h.maintenance {
		return nil, errMaintenanceMode()
	}
	now := time.Now().UTC()
	job := apiv1.Job{
		ID:          uuid.New(),
		Type:        jobType,
		Class:       apiv1.JobClassParity,
		Status:      apiv1.JobStatusQueued,
		Resumable:   false,
		Cancellable: cancellable,
		CreatedAt:   now,
	}
	h.jobs[job.ID] = job
	return &job, nil
}

func (h *handler) GetStatus(ctx context.Context) (*apiv1.SystemStatus, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return mockSystemStatus(h.scenario, h.countActiveJobs(), h.maintenance, h.degradedAcknowledged), nil
}

// errArrayNotDegraded mirrors production's own refusal (internal/api's
// errArrayNotDegraded, disk.ErrNothingToAcknowledge): acknowledging a
// degraded state that does not exist would let a stale acknowledgement
// outlive the situation it was about.
func errArrayNotDegraded() error {
	return &mockError{code: "array_not_degraded", statusCode: 409, message: "the array is not degraded — nothing to acknowledge"}
}

// errArrayServicesNotStarted mirrors production's own refusal
// (internal/api's errArrayServicesNotStarted, api.ErrDegradedServicesNotStarted)
// for the same case cmd/hoservad's storageTargetSync.UpdateOrError
// reports: the acknowledgement itself succeeded, but the not-ready→ready
// transition it triggers did not actually start anything because the
// array is in maintenance mode (#385 finding 1).
func errArrayServicesNotStarted() error {
	return &mockError{
		code:       "array_services_not_started",
		statusCode: 409,
		message:    "the array was acknowledged, but the gated services could not be started: storage gate is ready but the array is in maintenance mode — leaving Samba/NFS/Docker/libvirt gated closed until array start",
	}
}

// AcknowledgeDegradedArray mirrors production's own operation (#385): only
// the "degraded" scenario's missing disk4 (mockPoolStatus) has anything to
// acknowledge. A second call while the disk is still (mock-)missing
// succeeds again, exactly like a real disk.StorageGate.Acknowledge, which
// only refuses once the gate itself is genuinely ready — this mock never
// re-evaluates disk4 as present, so it is never refused on a repeat call.
// While the array is in maintenance mode (an explicit `array stop`) the
// acknowledgement itself is still recorded — h.degradedAcknowledged is
// set exactly as it is outside maintenance mode, mirroring
// disk.StorageGate.Acknowledge's own contract — but the call is refused
// with array_services_not_started (#385 finding 1), the same as
// production's storageTargetSync.UpdateOrError refusing the not-ready→
// ready transition it would otherwise run.
func (h *handler) AcknowledgeDegradedArray(ctx context.Context) (*apiv1.SystemStatus, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.scenario != "degraded" {
		return nil, errArrayNotDegraded()
	}
	h.degradedAcknowledged = true
	if h.maintenance {
		return nil, errArrayServicesNotStarted()
	}
	return mockSystemStatus(h.scenario, h.countActiveJobs(), h.maintenance, h.degradedAcknowledged), nil
}

func (h *handler) GetPool(ctx context.Context) (*apiv1.PoolStatus, error) {
	return mockPoolStatus(h.scenario), nil
}

func (h *handler) ListDisks(ctx context.Context) (*apiv1.ListDisksOK, error) {
	return &apiv1.ListDisksOK{Disks: mockDiskInventory(h.scenario)}, nil
}

func (h *handler) RunDoctor(ctx context.Context) (*apiv1.DoctorReport, error) {
	return mockDoctorReport(h.scenario), nil
}

// errInvalidHostConfig mirrors internal/api's own errInvalidHostConfig
// (hostconfig.go): an unknown host-config id, a duplicate choice for the
// same one, or a decision that isn't import/leave is refused the same
// way production refuses it, not silently accepted.
func errInvalidHostConfig(msg string) error {
	return &mockError{code: "invalid_host_config", statusCode: 400, message: msg}
}

// mockHostConfigKinds mirrors internal/config's own KindFromCheckID
// whitelist (host.go) closely enough for this mock's own fixed check ids
// (phase1.go's mockDoctorReport): every id RunDoctor ever reports here.
var mockHostConfigKinds = map[string]bool{
	"host_samba":             true,
	"host_nfs":               true,
	"host_fstab":             true,
	"host_docker_containers": true,
	"host_docker_images":     true,
}

func (h *handler) ApplyHostConfig(ctx context.Context, req *apiv1.ApplyHostConfigRequest) (*apiv1.ApplyHostConfigResult, error) {
	seen := map[apiv1.HostConfigID]struct{}{}
	for _, choice := range req.Files {
		if !mockHostConfigKinds[string(choice.ID)] {
			return nil, errInvalidHostConfig(fmt.Sprintf("unknown host-config id %q", choice.ID))
		}
		if _, dup := seen[choice.ID]; dup {
			return nil, errInvalidHostConfig(fmt.Sprintf("duplicate choice for %s", choice.ID))
		}
		seen[choice.ID] = struct{}{}
		if choice.Decision != apiv1.HostConfigDecisionImport && choice.Decision != apiv1.HostConfigDecisionLeave {
			return nil, errInvalidHostConfig(fmt.Sprintf("decision for %s must be import or leave", choice.ID))
		}
	}
	return &apiv1.ApplyHostConfigResult{
		Files:          req.Files,
		DockerDataRoot: "/var/lib/docker",
	}, nil
}

func (h *handler) StartSync(ctx context.Context, req *apiv1.StartSyncRequest) (*apiv1.Job, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.submitParityJob(apiv1.JobTypeSync, false)
}

func (h *handler) StartScrub(ctx context.Context, req *apiv1.StartScrubRequest) (*apiv1.Job, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.submitParityJob(apiv1.JobTypeScrub, false)
}

func (h *handler) StartFix(ctx context.Context, req *apiv1.StartFixRequest) (*apiv1.Job, error) {
	if !req.Confirm {
		return nil, errConfirmRequired()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.submitParityJob(apiv1.JobTypeFix, true)
}

// StartMover is `hoserva mover run`'s manual trigger (doc 09 §2) — the
// same TypeMover job the threshold poll and the nightly chain submit in
// the real daemon; this mock has no scheduler of its own, so it just
// records the queued job like submitParityJob does for sync/scrub/fix.
func (h *handler) StartMover(ctx context.Context) (*apiv1.Job, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Production's Scheduler.Submit refuses every job type but
	// TypeDiskUpgradeData while maintenance mode is active (Q70).
	if h.maintenance {
		return nil, errMaintenanceMode()
	}
	now := time.Now().UTC()
	job := apiv1.Job{
		ID:          uuid.New(),
		Type:        apiv1.JobTypeMover,
		Class:       apiv1.JobClassArrayWrite,
		Status:      apiv1.JobStatusQueued,
		Resumable:   true,
		Cancellable: true,
		CreatedAt:   now,
	}
	h.jobs[job.ID] = job
	return &job, nil
}

// GetLastMoverRun mirrors production's null-before-first-run shape (#273).
func (h *handler) GetLastMoverRun(ctx context.Context) (apiv1.NilMoverRunResult, error) {
	var null apiv1.NilMoverRunResult
	null.SetToNull()
	return null, nil
}

// GetCacheUsage mirrors production's null-before-first-run shape (#273).
func (h *handler) GetCacheUsage(ctx context.Context) (apiv1.NilCacheUsageBreakdown, error) {
	var null apiv1.NilCacheUsageBreakdown
	null.SetToNull()
	return null, nil
}

func (h *handler) CreateArray(ctx context.Context, req *apiv1.CreateArrayRequest) (*apiv1.Job, error) {
	plan, err := mockTopologyPlan(req)
	if err != nil {
		return nil, err
	}
	if req.Confirmation == "" || plan.CheckConfirmation(req.Confirmation) != nil {
		return nil, errConfirmRequired()
	}
	if err := plan.Validate(mockDiskSizes(h.scenario)); err != nil {
		return nil, errInvalidPlan(err)
	}
	if err := disk.CheckFormatTargets(plan); err != nil {
		return nil, &mockError{code: "unmanaged_device", statusCode: 400, message: err.Error()}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// Production's Scheduler.Submit refuses every job type but
	// TypeDiskUpgradeData while maintenance mode is active (Q70).
	if h.maintenance {
		return nil, errMaintenanceMode()
	}
	now := time.Now().UTC()
	job := apiv1.Job{
		ID:          uuid.New(),
		Type:        apiv1.JobTypeDiskFormat,
		Class:       apiv1.JobClassTopology,
		Status:      apiv1.JobStatusQueued,
		Resumable:   false,
		Cancellable: false,
		CreatedAt:   now,
	}
	h.jobs[job.ID] = job
	return &job, nil
}

func mockDiskSizes(scenario string) map[string]int64 {
	sizes := make(map[string]int64)
	for _, d := range mockDiskInventory(scenario) {
		sizes[d.Device] = d.SizeBytes
	}
	return sizes
}

func mockTopologyPlan(req *apiv1.CreateArrayRequest) (disk.TopologyPlan, error) {
	var plan disk.TopologyPlan
	for _, a := range req.Disks {
		assigned, err := mockAssignedDisk(a)
		if err != nil {
			return disk.TopologyPlan{}, err
		}
		switch a.Role {
		case apiv1.ArrayDiskRoleParity:
			plan.Parity = append(plan.Parity, assigned)
		case apiv1.ArrayDiskRoleData:
			plan.Data = append(plan.Data, assigned)
		case apiv1.ArrayDiskRoleCache:
			if plan.Cache != nil {
				return disk.TopologyPlan{}, errInvalidPlan(errors.New("disk: at most one cache disk can be assigned"))
			}
			c := assigned
			plan.Cache = &c
		default:
			return disk.TopologyPlan{}, errInvalidPlan(fmt.Errorf("disk: unknown role %q", a.Role))
		}
	}
	return plan, nil
}

func mockAssignedDisk(a apiv1.ArrayDiskAssignment) (disk.AssignedDisk, error) {
	fs := disk.XFS
	if v, ok := a.Filesystem.Get(); ok {
		switch v {
		case apiv1.ArrayDiskFilesystemXfs:
			fs = disk.XFS
		case apiv1.ArrayDiskFilesystemExt4:
			fs = disk.EXT4
		case apiv1.ArrayDiskFilesystemBtrfs:
			fs = disk.BTRFS
		default:
			return disk.AssignedDisk{}, errInvalidPlan(fmt.Errorf("%w: %s", disk.ErrUnsupportedFilesystem, v))
		}
	}
	return disk.AssignedDisk{
		Device:     a.Device,
		Filesystem: fs,
		Adopt:      a.Adopt.Or(false),
	}, nil
}

func (h *handler) ExportConfig(ctx context.Context) (apiv1.ExportConfigOK, error) {
	// Opaque stub bytes — the mock never builds a real archive (doc 10 §1).
	return apiv1.ExportConfigOK{Data: bytes.NewReader([]byte("mock hoserva config export"))}, nil
}

func (h *handler) ImportConfig(ctx context.Context, req *apiv1.ImportConfigReq) error {
	if !req.Confirm {
		return errConfirmRequired()
	}
	return nil
}

func (h *handler) StopArray(ctx context.Context, req *apiv1.StopArrayRequest) (*apiv1.SystemStatus, error) {
	if !req.Confirm {
		return nil, errConfirmRequired()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if id, pending := h.pendingDiskUpgradeLocked(); pending {
		return nil, errDiskUpgradePending(id)
	}
	h.maintenance = true
	return mockSystemStatus(h.scenario, h.countActiveJobs(), h.maintenance, h.degradedAcknowledged), nil
}

func (h *handler) StartArray(ctx context.Context) (*apiv1.SystemStatus, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if id, pending := h.pendingDiskUpgradeLocked(); pending {
		return nil, errDiskUpgradePending(id)
	}
	h.maintenance = false
	return mockSystemStatus(h.scenario, h.countActiveJobs(), h.maintenance, h.degradedAcknowledged), nil
}
