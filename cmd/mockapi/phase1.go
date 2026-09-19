package main

import (
	"bytes"
	"context"
	"time"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/disk"
)

const mockDiskSize = 4_000_000_000_000

func errConfirmRequired() error {
	return &mockError{code: "confirmation_required", statusCode: 409, message: "this operation requires an explicit confirmation"}
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
	}
	if scenario == "degraded" {
		disks[2].Failed = apiv1.NewOptBool(true)
	}
	return disks
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

func mockSystemStatus(scenario string, activeJobs int32) *apiv1.SystemStatus {
	report := mockDoctorReport(scenario)
	healthy := report.Overall != apiv1.DoctorCheckStatusFail
	summary := "All checks passed"
	if !healthy {
		summary = "One or more checks failed — run hoserva doctor for details"
	} else if report.Overall == apiv1.DoctorCheckStatusWarn {
		summary = "System is running with warnings — run hoserva doctor for details"
	}

	status := &apiv1.SystemStatus{
		Healthy:    healthy,
		Summary:    summary,
		ActiveJobs: apiv1.NewOptInt32(activeJobs),
	}
	if scenario == "degraded" {
		status.ArrayDegraded = apiv1.NewOptBool(true)
	}
	if scenario == "sync-blocked" {
		status.ParityBlocked = apiv1.NewOptBool(true)
	}
	return status
}

func (h *handler) submitParityJob(jobType apiv1.JobType, cancellable bool) (*apiv1.Job, error) {
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
	return mockSystemStatus(h.scenario, h.countActiveJobs()), nil
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

func (h *handler) CreateArray(ctx context.Context, req *apiv1.CreateArrayRequest) (*apiv1.Job, error) {
	plan := mockTopologyPlan(req)
	if req.Confirmation == "" || plan.CheckConfirmation(req.Confirmation) != nil {
		return nil, errConfirmRequired()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
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

func mockTopologyPlan(req *apiv1.CreateArrayRequest) disk.TopologyPlan {
	var plan disk.TopologyPlan
	for _, a := range req.Disks {
		assigned := disk.AssignedDisk{
			Device:     a.Device,
			Filesystem: disk.XFS,
			Adopt:      a.Adopt.Or(false),
		}
		switch a.Role {
		case apiv1.ArrayDiskRoleParity:
			plan.Parity = append(plan.Parity, assigned)
		case apiv1.ArrayDiskRoleData:
			plan.Data = append(plan.Data, assigned)
		case apiv1.ArrayDiskRoleCache:
			c := assigned
			plan.Cache = &c
		}
	}
	return plan
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
