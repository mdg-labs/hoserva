package main

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

// contractCases is #272's request table: every entry names the operation
// it exercises (contractCase.op) and, where the case's evidence traces to
// a specific historical drift, the PR that found it. Devices, mountpoints
// and channel/share/user names below match mockArrayDisks/
// mockDiskInventory("healthy") (contract_rig_test.go) — the same fixture
// data both handlers in a case are seeded from. No scenario here seeds a
// cache-role disk (shares.go's own mockCacheModeNeedsCacheDisk), so a
// case that creates a share and does not care about cache placement
// always passes CacheMode: array-only explicitly — cache-then-move (the
// unset default) is its own case below.
var contractCases = []contractCase{
	// --- Array/disk topology (PR182: mock topology plan ignored
	// filesystem and allowed a second cache disk) ---
	{
		op:       "CreateArray",
		name:     "valid_topology_queues_a_job",
		scenario: "fresh-install",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.CreateArray(ctx, &apiv1.CreateArrayRequest{
				Confirmation: "ERASE /dev/sdb, /dev/sdc",
				Disks: []apiv1.ArrayDiskAssignment{
					{Device: "/dev/sdb", Role: apiv1.ArrayDiskRoleParity, Filesystem: apiv1.NewOptArrayDiskFilesystem(apiv1.ArrayDiskFilesystemXfs)},
					{Device: "/dev/sdc", Role: apiv1.ArrayDiskRoleData, Filesystem: apiv1.NewOptArrayDiskFilesystem(apiv1.ArrayDiskFilesystemXfs)},
				},
			})
			return err
		},
	},
	{
		op:       "CreateArray",
		name:     "missing_confirmation",
		scenario: "fresh-install",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.CreateArray(ctx, &apiv1.CreateArrayRequest{
				Disks: []apiv1.ArrayDiskAssignment{
					{Device: "/dev/sdb", Role: apiv1.ArrayDiskRoleParity, Filesystem: apiv1.NewOptArrayDiskFilesystem(apiv1.ArrayDiskFilesystemXfs)},
					{Device: "/dev/sdc", Role: apiv1.ArrayDiskRoleData, Filesystem: apiv1.NewOptArrayDiskFilesystem(apiv1.ArrayDiskFilesystemXfs)},
				},
			})
			return err
		},
	},
	{
		op:   "CreateArray",
		name: "second_cache_disk_is_refused",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.CreateArray(ctx, &apiv1.CreateArrayRequest{
				Confirmation: "ERASE /dev/sdb, /dev/sdc, /dev/sdd, /dev/sde",
				Disks: []apiv1.ArrayDiskAssignment{
					{Device: "/dev/sdb", Role: apiv1.ArrayDiskRoleParity, Filesystem: apiv1.NewOptArrayDiskFilesystem(apiv1.ArrayDiskFilesystemXfs)},
					{Device: "/dev/sdc", Role: apiv1.ArrayDiskRoleData, Filesystem: apiv1.NewOptArrayDiskFilesystem(apiv1.ArrayDiskFilesystemXfs)},
					{Device: "/dev/sdd", Role: apiv1.ArrayDiskRoleCache, Filesystem: apiv1.NewOptArrayDiskFilesystem(apiv1.ArrayDiskFilesystemXfs)},
					{Device: "/dev/sde", Role: apiv1.ArrayDiskRoleCache, Filesystem: apiv1.NewOptArrayDiskFilesystem(apiv1.ArrayDiskFilesystemXfs)},
				},
			})
			return err
		},
	},
	{
		op:       "CreateArray",
		name:     "non_xfs_parity_is_refused",
		scenario: "fresh-install",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.CreateArray(ctx, &apiv1.CreateArrayRequest{
				Confirmation: "ERASE /dev/sdb, /dev/sdc",
				Disks: []apiv1.ArrayDiskAssignment{
					{Device: "/dev/sdb", Role: apiv1.ArrayDiskRoleParity, Filesystem: apiv1.NewOptArrayDiskFilesystem(apiv1.ArrayDiskFilesystemExt4)},
					{Device: "/dev/sdc", Role: apiv1.ArrayDiskRoleData, Filesystem: apiv1.NewOptArrayDiskFilesystem(apiv1.ArrayDiskFilesystemXfs)},
				},
			})
			return err
		},
	},
	{
		op:   "PlanDiskAdd",
		name: "valid_spare_device",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.PlanDiskAdd(ctx, &apiv1.AddDiskPlanRequest{Device: "/dev/sdf"})
			return err
		},
	},
	{
		// PlanDiskAdd on /dev/sdf, then AddDisk with the plan's own
		// confirmation, succeeds on both sides.
		op:   "AddDisk",
		name: "valid_spare_device",
		run: func(ctx context.Context, h apiv1.Handler) error {
			plan, err := h.PlanDiskAdd(ctx, &apiv1.AddDiskPlanRequest{Device: "/dev/sdf"})
			if err != nil {
				return err
			}
			_, err = h.AddDisk(ctx, &apiv1.AddDiskRequest{Device: "/dev/sdf", Confirmation: plan.Confirmation})
			return err
		},
	},
	{
		op:   "AddDisk",
		name: "missing_confirmation",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.AddDisk(ctx, &apiv1.AddDiskRequest{Device: "/dev/sdf"})
			return err
		},
	},
	{
		op:   "AddDisk",
		name: "unmanaged_device",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.AddDisk(ctx, &apiv1.AddDiskRequest{Device: "/dev/zzz", Confirmation: "ERASE /dev/zzz"})
			return err
		},
	},
	{
		// mockArrayDisks/mockDiskInventory's own "degraded" scenario
		// carries disk4 with no matching inventory entry at all —
		// genuinely absent, the one slot job.ConfirmReplacementTargetAbsent
		// lets a plan reach (disk_lifecycle.go's own comment on
		// mockArrayDisks).
		op:       "PlanDiskReplace",
		name:     "valid_absent_disk",
		scenario: "degraded",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.PlanDiskReplace(ctx, &apiv1.ReplaceDiskPlanRequest{Mountpoint: "/mnt/disk4", Device: "/dev/sdf"})
			return err
		},
	},
	{
		op:   "PlanDiskReplace",
		name: "slot_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.PlanDiskReplace(ctx, &apiv1.ReplaceDiskPlanRequest{Mountpoint: "/mnt/disk9", Device: "/dev/sdf"})
			return err
		},
	},
	{
		// PlanDiskReplace on the same absent disk4, then ReplaceDisk
		// with the plan's own confirmation, succeeds on both sides.
		op:       "ReplaceDisk",
		name:     "valid_absent_disk",
		scenario: "degraded",
		run: func(ctx context.Context, h apiv1.Handler) error {
			plan, err := h.PlanDiskReplace(ctx, &apiv1.ReplaceDiskPlanRequest{Mountpoint: "/mnt/disk4", Device: "/dev/sdf"})
			if err != nil {
				return err
			}
			_, err = h.ReplaceDisk(ctx, &apiv1.ReplaceDiskRequest{Mountpoint: "/mnt/disk4", Device: "/dev/sdf", Confirmation: plan.Confirmation})
			return err
		},
	},
	{
		op:   "ReplaceDisk",
		name: "slot_disk_still_present",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ReplaceDisk(ctx, &apiv1.ReplaceDiskRequest{Mountpoint: "/mnt/disk1", Device: "/dev/sdf", Confirmation: "ERASE /dev/sdf"})
			return err
		},
	},
	{
		op:   "PlanDiskUpgrade",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.PlanDiskUpgrade(ctx, &apiv1.DiskUpgradePlanRequest{Mountpoint: "/mnt/disk1", Device: "/dev/sdf"})
			return err
		},
	},
	{
		op:   "PlanDiskUpgrade",
		name: "slot_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.PlanDiskUpgrade(ctx, &apiv1.DiskUpgradePlanRequest{Mountpoint: "/mnt/disk9", Device: "/dev/sdf"})
			return err
		},
	},
	{
		// A data-disk upgrade is admitted only once the array's stop
		// sequence has completed (job.Scheduler.admitDiskUpgradeDataLocked,
		// doc 02 §4 E8) — StopArray first, then PlanDiskUpgrade for the
		// plan's own confirmation, then UpgradeDisk, succeeds on both
		// sides.
		op:   "UpgradeDisk",
		name: "valid_data_disk",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
				return err
			}
			plan, err := h.PlanDiskUpgrade(ctx, &apiv1.DiskUpgradePlanRequest{Mountpoint: "/mnt/disk1", Device: "/dev/sdf"})
			if err != nil {
				return err
			}
			_, err = h.UpgradeDisk(ctx, &apiv1.UpgradeDiskRequest{Mountpoint: "/mnt/disk1", Device: "/dev/sdf", Confirmation: plan.Confirmation})
			return err
		},
	},
	{
		op:   "UpgradeDisk",
		name: "missing_confirmation",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpgradeDisk(ctx, &apiv1.UpgradeDiskRequest{Mountpoint: "/mnt/disk1", Device: "/dev/sdf"})
			return err
		},
	},
	{
		op:   "FinishDiskRemoval",
		name: "disk_not_evacuated",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.FinishDiskRemoval(ctx, &apiv1.FinishDiskRemovalRequest{Mountpoint: "/mnt/disk1", Confirmation: "REMOVE /mnt/disk1"})
			return err
		},
	},
	{
		op:   "PlanDiskEvacuation",
		name: "valid_disk",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.PlanDiskEvacuation(ctx, &apiv1.EvacuateDiskPlanRequest{Mountpoint: "/mnt/disk1"})
			return err
		},
	},
	{
		// PlanDiskEvacuation on /mnt/disk1, then EvacuateDisk with the
		// plan's own confirmation, succeeds on both sides.
		op:   "EvacuateDisk",
		name: "valid_disk",
		run: func(ctx context.Context, h apiv1.Handler) error {
			res, err := h.PlanDiskEvacuation(ctx, &apiv1.EvacuateDiskPlanRequest{Mountpoint: "/mnt/disk1"})
			if err != nil {
				return err
			}
			plan, ok := res.(*apiv1.EvacuationPlan)
			if !ok {
				return fmt.Errorf("PlanDiskEvacuation = %T, want *apiv1.EvacuationPlan", res)
			}
			_, err = h.EvacuateDisk(ctx, &apiv1.EvacuateDiskRequest{Mountpoint: "/mnt/disk1", Confirmation: plan.Confirmation})
			return err
		},
	},
	{
		op:   "EvacuateDisk",
		name: "missing_confirmation",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.EvacuateDisk(ctx, &apiv1.EvacuateDiskRequest{Mountpoint: "/mnt/disk1"})
			return err
		},
	},
	{
		op:   "CancelDiskRemoval",
		name: "disk_slot_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			return h.CancelDiskRemoval(ctx, &apiv1.CancelDiskRemovalRequest{Mountpoint: "/mnt/disk9"})
		},
	},
	{
		op:   "CancelDiskRemoval",
		name: "not_in_removal",
		run: func(ctx context.Context, h apiv1.Handler) error {
			return h.CancelDiskRemoval(ctx, &apiv1.CancelDiskRemovalRequest{Mountpoint: "/mnt/disk1"})
		},
	},
	{
		// mockArrayDisks/mockPoolStatus's own "sync-blocked" scenario
		// (#361) carries disk5 already evacuated — the one removal state
		// no scenario had a disk in before this issue.
		op:       "CancelDiskRemoval",
		name:     "valid_evacuated_disk",
		scenario: "sync-blocked",
		run: func(ctx context.Context, h apiv1.Handler) error {
			return h.CancelDiskRemoval(ctx, &apiv1.CancelDiskRemovalRequest{Mountpoint: "/mnt/disk5"})
		},
	},
	{
		op:   "PlanRebalance",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.PlanRebalance(ctx)
			return err
		},
	},
	{
		// PlanRebalance, then StartRebalance with the plan's own
		// confirmation, succeeds on both sides.
		op:   "StartRebalance",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			plan, err := h.PlanRebalance(ctx)
			if err != nil {
				return err
			}
			_, err = h.StartRebalance(ctx, &apiv1.StartRebalanceRequest{Confirmation: plan.Confirmation})
			return err
		},
	},
	{
		op:   "StartRebalance",
		name: "missing_confirmation",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.StartRebalance(ctx, &apiv1.StartRebalanceRequest{})
			return err
		},
	},
	{
		op:   "ListDisks",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ListDisks(ctx)
			return err
		},
	},
	{
		op:   "GetPool",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetPool(ctx)
			return err
		},
	},

	// --- Jobs / parity operations, including #330's maintenance-mode
	// refusal (Q70) ---
	{
		op:   "GetStatus",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetStatus(ctx)
			return err
		},
	},
	{
		op:   "StopArray",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true})
			return err
		},
	},
	{
		op:   "StopArray",
		name: "missing_confirm",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.StopArray(ctx, &apiv1.StopArrayRequest{})
			return err
		},
	},
	{
		op:   "StartArray",
		name: "valid_noop_while_running",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.StartArray(ctx)
			return err
		},
	},
	{
		op:   "StartSync",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.StartSync(ctx, &apiv1.StartSyncRequest{})
			return err
		},
	},
	{
		op:   "StartSync",
		name: "refused_in_maintenance_mode",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
				return err
			}
			_, err := h.StartSync(ctx, &apiv1.StartSyncRequest{})
			return err
		},
	},
	{
		op:   "StartScrub",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.StartScrub(ctx, &apiv1.StartScrubRequest{})
			return err
		},
	},
	{
		op:   "StartScrub",
		name: "refused_in_maintenance_mode",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
				return err
			}
			_, err := h.StartScrub(ctx, &apiv1.StartScrubRequest{})
			return err
		},
	},
	{
		op:   "StartFix",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.StartFix(ctx, &apiv1.StartFixRequest{Confirm: true})
			return err
		},
	},
	{
		op:   "StartFix",
		name: "missing_confirm",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.StartFix(ctx, &apiv1.StartFixRequest{})
			return err
		},
	},
	{
		op:   "StartMover",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.StartMover(ctx)
			return err
		},
	},
	{
		op:   "StartMover",
		name: "refused_in_maintenance_mode",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
				return err
			}
			_, err := h.StartMover(ctx)
			return err
		},
	},
	{
		op:   "GetLastMoverRun",
		name: "valid_before_first_run",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetLastMoverRun(ctx)
			return err
		},
	},
	{
		op:   "GetCacheUsage",
		name: "valid_before_first_run",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetCacheUsage(ctx)
			return err
		},
	},
	{
		op:   "ListJobs",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ListJobs(ctx, apiv1.ListJobsParams{})
			return err
		},
	},
	{
		// StartSync, then GetJob on the returned job's own id, succeeds
		// on both sides.
		op:   "GetJob",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			j, err := h.StartSync(ctx, &apiv1.StartSyncRequest{})
			if err != nil {
				return err
			}
			_, err = h.GetJob(ctx, apiv1.GetJobParams{JobId: j.ID})
			return err
		},
	},
	{
		op:   "GetJob",
		name: "unknown_id",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetJob(ctx, apiv1.GetJobParams{JobId: uuid.New()})
			return err
		},
	},
	{
		op:   "GetJobLog",
		name: "unknown_id",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetJobLog(ctx, apiv1.GetJobLogParams{JobId: uuid.New()})
			return err
		},
	},
	{
		op:   "CancelJob",
		name: "unknown_id",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.CancelJob(ctx, apiv1.CancelJobParams{JobId: uuid.New()})
			return err
		},
	},
	{
		op:   "ResumeJob",
		name: "unknown_id",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ResumeJob(ctx, apiv1.ResumeJobParams{JobId: uuid.New()})
			return err
		},
	},
	{
		op:   "GetParity",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetParity(ctx)
			return err
		},
	},
	{
		op:   "RunParityDiff",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.RunParityDiff(ctx)
			return err
		},
	},

	// --- Shares (Q14/#46) ---
	{
		op:   "ListShares",
		name: "valid_empty",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ListShares(ctx)
			return err
		},
	},
	{
		op:   "GetShare",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CreateShare(ctx, &apiv1.CreateShareRequest{Name: "media", CacheMode: apiv1.NewOptShareCacheMode(apiv1.ShareCacheModeArrayOnly)}); err != nil {
				return err
			}
			_, err := h.GetShare(ctx, apiv1.GetShareParams{Name: "media"})
			return err
		},
	},
	{
		op:   "GetShare",
		name: "unknown_name",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetShare(ctx, apiv1.GetShareParams{Name: "nope"})
			return err
		},
	},
	{
		op:   "CreateShare",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.CreateShare(ctx, &apiv1.CreateShareRequest{Name: "media", CacheMode: apiv1.NewOptShareCacheMode(apiv1.ShareCacheModeArrayOnly)})
			return err
		},
	},
	{
		// Production's CreateShare defaults to cache-then-move
		// (internal/share/service.go) and refuses it without a cache
		// disk in the array; mockCacheModeNeedsCacheDisk (shares.go)
		// mirrors that check, and mockArrayDisks("healthy") has no
		// cache disk, so the default is refused with
		// share_invalid_input on both sides.
		op:   "CreateShare",
		name: "cache_then_move_default_needs_a_cache_disk",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.CreateShare(ctx, &apiv1.CreateShareRequest{Name: "media"})
			return err
		},
	},
	{
		op:   "CreateShare",
		name: "duplicate_name_is_refused",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CreateShare(ctx, &apiv1.CreateShareRequest{Name: "media", CacheMode: apiv1.NewOptShareCacheMode(apiv1.ShareCacheModeArrayOnly)}); err != nil {
				return err
			}
			_, err := h.CreateShare(ctx, &apiv1.CreateShareRequest{Name: "media", CacheMode: apiv1.NewOptShareCacheMode(apiv1.ShareCacheModeArrayOnly)})
			return err
		},
	},
	{
		// Create an array-only share, then change its create policy,
		// succeeds on both sides.
		op:   "UpdateShare",
		name: "valid_create_policy_change",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CreateShare(ctx, &apiv1.CreateShareRequest{Name: "media", CacheMode: apiv1.NewOptShareCacheMode(apiv1.ShareCacheModeArrayOnly)}); err != nil {
				return err
			}
			_, err := h.UpdateShare(ctx, &apiv1.UpdateShareRequest{
				CreatePolicy: apiv1.NewOptArrayCreatePolicy(apiv1.ArrayCreatePolicyMfs),
			}, apiv1.UpdateShareParams{Name: "media"})
			return err
		},
	},
	{
		op:   "UpdateShare",
		name: "unknown_name",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateShare(ctx, &apiv1.UpdateShareRequest{}, apiv1.UpdateShareParams{Name: "nope"})
			return err
		},
	},
	{
		// The same cache-disk requirement CreateShare's default hits,
		// exercised on an explicit UpdateShare request instead of the
		// unset default — share.Service.Update carries an identical
		// check (service.go's own second "cache mode %q needs a cache
		// disk" site).
		op:   "UpdateShare",
		name: "cache_then_move_needs_a_cache_disk",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CreateShare(ctx, &apiv1.CreateShareRequest{Name: "media", CacheMode: apiv1.NewOptShareCacheMode(apiv1.ShareCacheModeArrayOnly)}); err != nil {
				return err
			}
			_, err := h.UpdateShare(ctx, &apiv1.UpdateShareRequest{
				CacheMode: apiv1.NewOptShareCacheMode(apiv1.ShareCacheModeCacheThenMove),
			}, apiv1.UpdateShareParams{Name: "media"})
			return err
		},
	},
	{
		// Create a share, then delete it with Confirm: true, succeeds
		// on both sides.
		op:   "DeleteShare",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CreateShare(ctx, &apiv1.CreateShareRequest{Name: "media", CacheMode: apiv1.NewOptShareCacheMode(apiv1.ShareCacheModeArrayOnly)}); err != nil {
				return err
			}
			return h.DeleteShare(ctx, &apiv1.ConfirmShareRequest{Confirm: true}, apiv1.DeleteShareParams{Name: "media"})
		},
	},
	{
		op:   "DeleteShare",
		name: "missing_confirm",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CreateShare(ctx, &apiv1.CreateShareRequest{Name: "media", CacheMode: apiv1.NewOptShareCacheMode(apiv1.ShareCacheModeArrayOnly)}); err != nil {
				return err
			}
			return h.DeleteShare(ctx, &apiv1.ConfirmShareRequest{}, apiv1.DeleteShareParams{Name: "media"})
		},
	},
	{
		op:   "DeleteShareData",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CreateShare(ctx, &apiv1.CreateShareRequest{Name: "media", CacheMode: apiv1.NewOptShareCacheMode(apiv1.ShareCacheModeArrayOnly)}); err != nil {
				return err
			}
			return h.DeleteShareData(ctx, &apiv1.DeleteShareDataRequest{Confirmation: "media"}, apiv1.DeleteShareDataParams{Name: "media"})
		},
	},
	{
		op:   "DeleteShareData",
		name: "wrong_confirmation_phrase",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CreateShare(ctx, &apiv1.CreateShareRequest{Name: "media", CacheMode: apiv1.NewOptShareCacheMode(apiv1.ShareCacheModeArrayOnly)}); err != nil {
				return err
			}
			return h.DeleteShareData(ctx, &apiv1.DeleteShareDataRequest{Confirmation: "wrong"}, apiv1.DeleteShareDataParams{Name: "media"})
		},
	},
	{
		op:   "DeleteShareFile",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CreateShare(ctx, &apiv1.CreateShareRequest{Name: "media", CacheMode: apiv1.NewOptShareCacheMode(apiv1.ShareCacheModeArrayOnly)}); err != nil {
				return err
			}
			return h.DeleteShareFile(ctx, &apiv1.ConfirmShareRequest{Confirm: true}, apiv1.DeleteShareFileParams{Name: "media", Path: "movie.mkv"})
		},
	},
	{
		op:   "DeleteShareFile",
		name: "missing_confirm",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CreateShare(ctx, &apiv1.CreateShareRequest{Name: "media", CacheMode: apiv1.NewOptShareCacheMode(apiv1.ShareCacheModeArrayOnly)}); err != nil {
				return err
			}
			return h.DeleteShareFile(ctx, &apiv1.ConfirmShareRequest{}, apiv1.DeleteShareFileParams{Name: "media", Path: "movie.mkv"})
		},
	},
	{
		op:   "StartShareRelocation",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CreateShare(ctx, &apiv1.CreateShareRequest{Name: "media", CacheMode: apiv1.NewOptShareCacheMode(apiv1.ShareCacheModeArrayOnly)}); err != nil {
				return err
			}
			_, err := h.StartShareRelocation(ctx, &apiv1.StartShareRelocationRequest{To: apiv1.StartShareRelocationRequestToArray}, apiv1.StartShareRelocationParams{Name: "media"})
			return err
		},
	},
	{
		op:   "StartShareRelocation",
		name: "unknown_share",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.StartShareRelocation(ctx, &apiv1.StartShareRelocationRequest{To: apiv1.StartShareRelocationRequestToArray}, apiv1.StartShareRelocationParams{Name: "nope"})
			return err
		},
	},
	{
		op:   "StartShareRelocation",
		name: "refused_in_maintenance_mode",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CreateShare(ctx, &apiv1.CreateShareRequest{Name: "media", CacheMode: apiv1.NewOptShareCacheMode(apiv1.ShareCacheModeArrayOnly)}); err != nil {
				return err
			}
			if _, err := h.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
				return err
			}
			_, err := h.StartShareRelocation(ctx, &apiv1.StartShareRelocationRequest{To: apiv1.StartShareRelocationRequestToArray}, apiv1.StartShareRelocationParams{Name: "media"})
			return err
		},
	},
	{
		op:   "BrowseShare",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CreateShare(ctx, &apiv1.CreateShareRequest{Name: "media", CacheMode: apiv1.NewOptShareCacheMode(apiv1.ShareCacheModeArrayOnly)}); err != nil {
				return err
			}
			_, err := h.BrowseShare(ctx, apiv1.BrowseShareParams{Name: "media"})
			return err
		},
	},
	{
		op:   "BrowseShare",
		name: "unknown_name",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.BrowseShare(ctx, apiv1.BrowseShareParams{Name: "nope"})
			return err
		},
	},
	{
		op:   "GetSharePermissions",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CreateShare(ctx, &apiv1.CreateShareRequest{Name: "media", CacheMode: apiv1.NewOptShareCacheMode(apiv1.ShareCacheModeArrayOnly)}); err != nil {
				return err
			}
			_, err := h.GetSharePermissions(ctx, apiv1.GetSharePermissionsParams{Name: "media"})
			return err
		},
	},
	{
		op:   "GetSharePermissions",
		name: "unknown_share",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetSharePermissions(ctx, apiv1.GetSharePermissionsParams{Name: "nope"})
			return err
		},
	},
	{
		// Create a share and a user, then grant that user access,
		// succeeds on both sides.
		op:   "UpdateSharePermissions",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CreateShare(ctx, &apiv1.CreateShareRequest{Name: "media", CacheMode: apiv1.NewOptShareCacheMode(apiv1.ShareCacheModeArrayOnly)}); err != nil {
				return err
			}
			u, err := h.CreateUser(ctx, &apiv1.CreateUserRequest{Username: "permviewer"})
			if err != nil {
				return err
			}
			_, err = h.UpdateSharePermissions(ctx, &apiv1.UpdateSharePermissionsRequest{
				Users: []apiv1.UpdateSharePermissionsRequestUsersItem{{UserId: u.ID, Access: apiv1.ShareAccessLevelReadOnly}},
			}, apiv1.UpdateSharePermissionsParams{Name: "media"})
			return err
		},
	},
	{
		// Production's UpdateSharePermissions validates every user id
		// in a full-replace write (existsInTx, share_permissions.go)
		// and refuses an unknown one with user_not_found/404; the mock
		// mirrors that check (users.go) instead of writing an empty-
		// username entry for it.
		op:   "UpdateSharePermissions",
		name: "unknown_user_is_refused",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CreateShare(ctx, &apiv1.CreateShareRequest{Name: "media", CacheMode: apiv1.NewOptShareCacheMode(apiv1.ShareCacheModeArrayOnly)}); err != nil {
				return err
			}
			_, err := h.UpdateSharePermissions(ctx, &apiv1.UpdateSharePermissionsRequest{
				Users: []apiv1.UpdateSharePermissionsRequestUsersItem{{UserId: uuid.New(), Access: apiv1.ShareAccessLevelReadOnly}},
			}, apiv1.UpdateSharePermissionsParams{Name: "media"})
			return err
		},
	},
	{
		// Production's SetSharePermissions refuses a repeated user or
		// group id with duplicate_grant/400 before any existence check
		// (rejectDuplicateGrants, share_permissions.go); the mock
		// mirrors that order.
		op:   "UpdateSharePermissions",
		name: "duplicate_user_is_refused",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CreateShare(ctx, &apiv1.CreateShareRequest{Name: "media", CacheMode: apiv1.NewOptShareCacheMode(apiv1.ShareCacheModeArrayOnly)}); err != nil {
				return err
			}
			u, err := h.CreateUser(ctx, &apiv1.CreateUserRequest{Username: "permviewer"})
			if err != nil {
				return err
			}
			_, err = h.UpdateSharePermissions(ctx, &apiv1.UpdateSharePermissionsRequest{
				Users: []apiv1.UpdateSharePermissionsRequestUsersItem{
					{UserId: u.ID, Access: apiv1.ShareAccessLevelReadOnly},
					{UserId: u.ID, Access: apiv1.ShareAccessLevelReadWrite},
				},
			}, apiv1.UpdateSharePermissionsParams{Name: "media"})
			return err
		},
	},
	{
		// GetUserSharePermissions on a real user's own id succeeds on
		// both sides.
		op:   "GetUserSharePermissions",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			u, err := h.CreateUser(ctx, &apiv1.CreateUserRequest{Username: "permviewer"})
			if err != nil {
				return err
			}
			_, err = h.GetUserSharePermissions(ctx, apiv1.GetUserSharePermissionsParams{UserId: u.ID})
			return err
		},
	},
	{
		op:   "GetUserSharePermissions",
		name: "unknown_user",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetUserSharePermissions(ctx, apiv1.GetUserSharePermissionsParams{UserId: uuid.New()})
			return err
		},
	},
	{
		// UpdateUserSharePermissions on a real user's own id, with an
		// empty replace list, succeeds on both sides.
		op:   "UpdateUserSharePermissions",
		name: "valid_empty",
		run: func(ctx context.Context, h apiv1.Handler) error {
			u, err := h.CreateUser(ctx, &apiv1.CreateUserRequest{Username: "permviewer"})
			if err != nil {
				return err
			}
			_, err = h.UpdateUserSharePermissions(ctx, &apiv1.UpdateUserSharePermissionsRequest{}, apiv1.UpdateUserSharePermissionsParams{UserId: u.ID})
			return err
		},
	},
	{
		op:   "UpdateUserSharePermissions",
		name: "unknown_user",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateUserSharePermissions(ctx, &apiv1.UpdateUserSharePermissionsRequest{}, apiv1.UpdateUserSharePermissionsParams{UserId: uuid.New()})
			return err
		},
	},

	// --- Notifications (PR166: unknown channel id accepted by a
	// routing update) ---
	{
		op:   "ListNotificationChannels",
		name: "valid_empty",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ListNotificationChannels(ctx)
			return err
		},
	},
	{
		op:   "CreateNotificationChannel",
		name: "valid_webhook",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.CreateNotificationChannel(ctx, &apiv1.CreateNotificationChannelRequest{
				Name: "ops", Type: apiv1.NotificationChannelTypeWebhook, Enabled: true,
				WebhookUrl: apiv1.NewOptString("https://example.com/hook"),
			})
			return err
		},
	},
	{
		op:   "GetNotificationChannel",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			ch, err := h.CreateNotificationChannel(ctx, &apiv1.CreateNotificationChannelRequest{
				Name: "ops", Type: apiv1.NotificationChannelTypeWebhook, Enabled: true,
				WebhookUrl: apiv1.NewOptString("https://example.com/hook"),
			})
			if err != nil {
				return err
			}
			_, err = h.GetNotificationChannel(ctx, apiv1.GetNotificationChannelParams{ChannelId: ch.ID})
			return err
		},
	},
	{
		op:   "GetNotificationChannel",
		name: "unknown_id",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetNotificationChannel(ctx, apiv1.GetNotificationChannelParams{ChannelId: uuid.New()})
			return err
		},
	},
	{
		op:   "UpdateNotificationChannel",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			ch, err := h.CreateNotificationChannel(ctx, &apiv1.CreateNotificationChannelRequest{
				Name: "ops", Type: apiv1.NotificationChannelTypeWebhook, Enabled: true,
				WebhookUrl: apiv1.NewOptString("https://example.com/hook"),
			})
			if err != nil {
				return err
			}
			_, err = h.UpdateNotificationChannel(ctx, &apiv1.UpdateNotificationChannelRequest{
				Name: "ops-renamed", Type: apiv1.NotificationChannelTypeWebhook, Enabled: true,
				WebhookUrl: apiv1.NewOptString("https://example.com/hook"),
			}, apiv1.UpdateNotificationChannelParams{ChannelId: ch.ID})
			return err
		},
	},
	{
		op:   "UpdateNotificationChannel",
		name: "unknown_id",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateNotificationChannel(ctx, &apiv1.UpdateNotificationChannelRequest{
				Name: "ops", Type: apiv1.NotificationChannelTypeWebhook, Enabled: true,
				WebhookUrl: apiv1.NewOptString("https://example.com/hook"),
			}, apiv1.UpdateNotificationChannelParams{ChannelId: uuid.New()})
			return err
		},
	},
	{
		op:   "DeleteNotificationChannel",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			ch, err := h.CreateNotificationChannel(ctx, &apiv1.CreateNotificationChannelRequest{
				Name: "ops", Type: apiv1.NotificationChannelTypeWebhook, Enabled: true,
				WebhookUrl: apiv1.NewOptString("https://example.com/hook"),
			})
			if err != nil {
				return err
			}
			return h.DeleteNotificationChannel(ctx, apiv1.DeleteNotificationChannelParams{ChannelId: ch.ID})
		},
	},
	{
		op:   "DeleteNotificationChannel",
		name: "unknown_id",
		run: func(ctx context.Context, h apiv1.Handler) error {
			return h.DeleteNotificationChannel(ctx, apiv1.DeleteNotificationChannelParams{ChannelId: uuid.New()})
		},
	},
	{
		op:   "SendTestNotification",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			ch, err := h.CreateNotificationChannel(ctx, &apiv1.CreateNotificationChannelRequest{
				Name: "ops", Type: apiv1.NotificationChannelTypeWebhook, Enabled: true,
				WebhookUrl: apiv1.NewOptString("https://example.com/hook"),
			})
			if err != nil {
				return err
			}
			_, err = h.SendTestNotification(ctx, apiv1.SendTestNotificationParams{ChannelId: ch.ID})
			return err
		},
	},
	{
		op:   "SendTestNotification",
		name: "unknown_id",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.SendTestNotification(ctx, apiv1.SendTestNotificationParams{ChannelId: uuid.New()})
			return err
		},
	},
	{
		op:   "GetNotificationRouting",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetNotificationRouting(ctx)
			return err
		},
	},
	{
		op:   "UpdateNotificationRoute",
		name: "valid_existing_channel",
		run: func(ctx context.Context, h apiv1.Handler) error {
			ch, err := h.CreateNotificationChannel(ctx, &apiv1.CreateNotificationChannelRequest{
				Name: "ops", Type: apiv1.NotificationChannelTypeWebhook, Enabled: true,
				WebhookUrl: apiv1.NewOptString("https://example.com/hook"),
			})
			if err != nil {
				return err
			}
			_, err = h.UpdateNotificationRoute(ctx, &apiv1.UpdateNotificationRouteRequest{
				Severity: apiv1.NotificationLevelWarning, ChannelIds: []uuid.UUID{ch.ID},
			}, apiv1.UpdateNotificationRouteParams{EventType: apiv1.NotificationEventTypeDiskOffline})
			return err
		},
	},
	{
		op:   "UpdateNotificationRoute",
		name: "unknown_channel_id_is_refused",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateNotificationRoute(ctx, &apiv1.UpdateNotificationRouteRequest{
				Severity: apiv1.NotificationLevelWarning, ChannelIds: []uuid.UUID{uuid.New()},
			}, apiv1.UpdateNotificationRouteParams{EventType: apiv1.NotificationEventTypeDiskOffline})
			return err
		},
	},
	{
		op:   "GetQuietHours",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetQuietHours(ctx)
			return err
		},
	},
	{
		op:   "UpdateQuietHours",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateQuietHours(ctx, &apiv1.UpdateQuietHoursRequest{Enabled: true, Start: "22:00", End: "07:00"})
			return err
		},
	},
	{
		op:   "ListNotifications",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ListNotifications(ctx)
			return err
		},
	},
	{
		op:   "MarkNotificationsRead",
		name: "valid_empty",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.MarkNotificationsRead(ctx, &apiv1.MarkNotificationsReadRequest{})
			return err
		},
	},

	// --- Users, groups, sessions and API tokens (PR228: token-role
	// rules) ---
	{
		op:   "ListUsers",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ListUsers(ctx)
			return err
		},
	},
	{
		op:   "CreateUser",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.CreateUser(ctx, &apiv1.CreateUserRequest{Username: "viewer1"})
			return err
		},
	},
	{
		op:   "CreateUser",
		name: "duplicate_username_is_refused",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CreateUser(ctx, &apiv1.CreateUserRequest{Username: "viewer1"}); err != nil {
				return err
			}
			_, err := h.CreateUser(ctx, &apiv1.CreateUserRequest{Username: "viewer1"})
			return err
		},
	},
	{
		op:   "UpdateUser",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			u, err := h.CreateUser(ctx, &apiv1.CreateUserRequest{Username: "viewer1"})
			if err != nil {
				return err
			}
			_, err = h.UpdateUser(ctx, &apiv1.UpdateUserRequest{Role: apiv1.UpdateUserRequestRoleViewer}, apiv1.UpdateUserParams{UserId: u.ID})
			return err
		},
	},
	{
		op:   "UpdateUser",
		name: "unknown_id",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateUser(ctx, &apiv1.UpdateUserRequest{Role: apiv1.UpdateUserRequestRoleViewer}, apiv1.UpdateUserParams{UserId: uuid.New()})
			return err
		},
	},
	{
		op:   "DeleteUser",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			u, err := h.CreateUser(ctx, &apiv1.CreateUserRequest{Username: "viewer1"})
			if err != nil {
				return err
			}
			return h.DeleteUser(ctx, apiv1.DeleteUserParams{UserId: u.ID})
		},
	},
	{
		op:   "DeleteUser",
		name: "unknown_id",
		run: func(ctx context.Context, h apiv1.Handler) error {
			return h.DeleteUser(ctx, apiv1.DeleteUserParams{UserId: uuid.New()})
		},
	},
	{
		op:   "CreateApiToken",
		name: "unknown_username",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.CreateApiToken(ctx, &apiv1.CreateApiTokenRequest{Name: "t1", Role: apiv1.ApiTokenRoleViewer}, apiv1.CreateApiTokenParams{Username: "nope"})
			return err
		},
	},
	{
		op:   "CreateApiToken",
		name: "share_only_account_is_refused",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CreateUser(ctx, &apiv1.CreateUserRequest{Username: "shareonly1", Role: apiv1.NewOptCreateUserRequestRole(apiv1.CreateUserRequestRoleShareOnly)}); err != nil {
				return err
			}
			_, err := h.CreateApiToken(ctx, &apiv1.CreateApiTokenRequest{Name: "t1", Role: apiv1.ApiTokenRoleViewer}, apiv1.CreateApiTokenParams{Username: "shareonly1"})
			return err
		},
	},
	{
		op:   "CreateApiToken",
		name: "viewer_account_cannot_get_an_admin_token",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CreateUser(ctx, &apiv1.CreateUserRequest{Username: "viewer2", Role: apiv1.NewOptCreateUserRequestRole(apiv1.CreateUserRequestRoleViewer)}); err != nil {
				return err
			}
			_, err := h.CreateApiToken(ctx, &apiv1.CreateApiTokenRequest{Name: "t1", Role: apiv1.ApiTokenRoleAdmin}, apiv1.CreateApiTokenParams{Username: "viewer2"})
			return err
		},
	},
	{
		op:   "CreateApiToken",
		name: "valid_admin_account_viewer_token",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.CreateApiToken(ctx, &apiv1.CreateApiTokenRequest{Name: "t1", Role: apiv1.ApiTokenRoleViewer}, apiv1.CreateApiTokenParams{Username: "admin"})
			return err
		},
	},
	{
		op:   "ListApiTokens",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ListApiTokens(ctx)
			return err
		},
	},
	{
		op:   "RevokeApiToken",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			tok, err := h.CreateApiToken(ctx, &apiv1.CreateApiTokenRequest{Name: "t1", Role: apiv1.ApiTokenRoleViewer}, apiv1.CreateApiTokenParams{Username: "admin"})
			if err != nil {
				return err
			}
			return h.RevokeApiToken(ctx, apiv1.RevokeApiTokenParams{TokenId: tok.ID})
		},
	},
	{
		op:   "RevokeApiToken",
		name: "unknown_id",
		run: func(ctx context.Context, h apiv1.Handler) error {
			return h.RevokeApiToken(ctx, apiv1.RevokeApiTokenParams{TokenId: "nope"})
		},
	},
	{
		op:   "ListUserGroups",
		name: "valid_empty",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ListUserGroups(ctx)
			return err
		},
	},
	{
		op:   "CreateUserGroup",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.CreateUserGroup(ctx, &apiv1.CreateUserGroupRequest{Name: "media-writers"})
			return err
		},
	},
	{
		op:   "DeleteUserGroup",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			g, err := h.CreateUserGroup(ctx, &apiv1.CreateUserGroupRequest{Name: "media-writers"})
			if err != nil {
				return err
			}
			return h.DeleteUserGroup(ctx, apiv1.DeleteUserGroupParams{GroupId: g.ID})
		},
	},
	{
		op:   "DeleteUserGroup",
		name: "unknown_id",
		run: func(ctx context.Context, h apiv1.Handler) error {
			return h.DeleteUserGroup(ctx, apiv1.DeleteUserGroupParams{GroupId: uuid.New()})
		},
	},
	{
		op:   "SetUserGroupMembers",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			g, err := h.CreateUserGroup(ctx, &apiv1.CreateUserGroupRequest{Name: "media-writers"})
			if err != nil {
				return err
			}
			u, err := h.CreateUser(ctx, &apiv1.CreateUserRequest{Username: "viewer1"})
			if err != nil {
				return err
			}
			_, err = h.SetUserGroupMembers(ctx, &apiv1.SetUserGroupMembersRequest{UserIds: []uuid.UUID{u.ID}}, apiv1.SetUserGroupMembersParams{GroupId: g.ID})
			return err
		},
	},
	{
		op:   "SetUserGroupMembers",
		name: "unknown_group",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.SetUserGroupMembers(ctx, &apiv1.SetUserGroupMembersRequest{}, apiv1.SetUserGroupMembersParams{GroupId: uuid.New()})
			return err
		},
	},
	{
		// Production's own AuthStore.SetGroupMembers (groups_store.go)
		// refuses an unknown member id with user_not_found/404 in a
		// full-replace write; the mock mirrors that check (users.go).
		op:   "SetUserGroupMembers",
		name: "unknown_member_is_refused",
		run: func(ctx context.Context, h apiv1.Handler) error {
			g, err := h.CreateUserGroup(ctx, &apiv1.CreateUserGroupRequest{Name: "media-writers"})
			if err != nil {
				return err
			}
			_, err = h.SetUserGroupMembers(ctx, &apiv1.SetUserGroupMembersRequest{UserIds: []uuid.UUID{uuid.New()}}, apiv1.SetUserGroupMembersParams{GroupId: g.ID})
			return err
		},
	},
	{
		// Production's own AuthStore.SetGroupMembers (groups_store.go)
		// refuses a repeated member id with duplicate_grant/400,
		// checked per element before the unknown-member check; the
		// mock mirrors that same per-element, duplicate-before-unknown
		// order (users.go).
		op:   "SetUserGroupMembers",
		name: "duplicate_member_is_refused",
		run: func(ctx context.Context, h apiv1.Handler) error {
			g, err := h.CreateUserGroup(ctx, &apiv1.CreateUserGroupRequest{Name: "media-writers"})
			if err != nil {
				return err
			}
			u, err := h.CreateUser(ctx, &apiv1.CreateUserRequest{Username: "viewer1"})
			if err != nil {
				return err
			}
			_, err = h.SetUserGroupMembers(ctx, &apiv1.SetUserGroupMembersRequest{UserIds: []uuid.UUID{u.ID, u.ID}}, apiv1.SetUserGroupMembersParams{GroupId: g.ID})
			return err
		},
	},
	{
		op:   "SetUserPassword",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			u, err := h.CreateUser(ctx, &apiv1.CreateUserRequest{Username: "viewer1"})
			if err != nil {
				return err
			}
			return h.SetUserPassword(ctx, &apiv1.SetUserPasswordRequest{Password: "correct horse battery staple"}, apiv1.SetUserPasswordParams{UserId: u.ID})
		},
	},
	{
		op:   "SetUserPassword",
		name: "unknown_id",
		run: func(ctx context.Context, h apiv1.Handler) error {
			return h.SetUserPassword(ctx, &apiv1.SetUserPasswordRequest{Password: "correct horse battery staple"}, apiv1.SetUserPasswordParams{UserId: uuid.New()})
		},
	},
	{
		op:   "GetSetupStatus",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetSetupStatus(ctx)
			return err
		},
	},
	{
		// On the one scenario neither side seeds an admin for
		// (newContractProductionHandler, mockAdminID's own comment in
		// auth.go), CreateFirstAdmin succeeds on both sides.
		op:       "CreateFirstAdmin",
		name:     "valid",
		scenario: "fresh-install",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.CreateFirstAdmin(ctx, &apiv1.CreateFirstAdminRequest{Username: "admin", Password: "correct horse battery staple"})
			return err
		},
	},
	{
		// mockAdminID's canned admin (auth.go) exists in every scenario
		// but fresh-install, exactly as newContractProductionHandler
		// seeds one (contract_rig_test.go) — CreateFirstAdmin on the
		// default "healthy" scenario must refuse the same way production
		// refuses a second first-admin.
		op:   "CreateFirstAdmin",
		name: "already_configured_is_refused",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.CreateFirstAdmin(ctx, &apiv1.CreateFirstAdminRequest{Username: "someone-else", Password: "another password entirely"})
			return err
		},
	},
	{
		op:   "ListSessions",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ListSessions(ctx)
			return err
		},
	},
	{
		op:   "RevokeSession",
		name: "unknown_id",
		run: func(ctx context.Context, h apiv1.Handler) error {
			return h.RevokeSession(ctx, apiv1.RevokeSessionParams{SessionId: "nope"})
		},
	},

	// --- Root-only recovery operations (doc 01 §7): this rig calls
	// handler methods directly, not over HTTP, and production's own
	// requireRootPeer is reachable in tests with a root peer credential
	// (auth.WithPeerCredential, internal/api/recovery_handler_test.go).
	// The mock refuses these operations unconditionally by design
	// (recovery.go carries no peer-credential layer at all), which is
	// what these cases exercise.
	{
		op:   "ResetUserPassword",
		name: "refused_without_a_root_peer",
		run: func(ctx context.Context, h apiv1.Handler) error {
			return h.ResetUserPassword(ctx, &apiv1.ResetUserPasswordRequest{Password: "correct horse battery staple"}, apiv1.ResetUserPasswordParams{Username: "admin"})
		},
	},
	{
		op:   "DisableUserTotp",
		name: "refused_without_a_root_peer",
		run: func(ctx context.Context, h apiv1.Handler) error {
			return h.DisableUserTotp(ctx, apiv1.DisableUserTotpParams{Username: "admin"})
		},
	},
	{
		op:   "UnlockUser",
		name: "refused_without_a_root_peer",
		run: func(ctx context.Context, h apiv1.Handler) error {
			return h.UnlockUser(ctx, apiv1.UnlockUserParams{Username: "admin"})
		},
	},

	// --- Schedules (#197, doc 03 §8.4) ---
	{
		op:   "GetSchedules",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetSchedules(ctx)
			return err
		},
	},
	{
		op:   "UpdateMaintenanceChainSchedule",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateMaintenanceChainSchedule(ctx, &apiv1.UpdateMaintenanceChainScheduleRequest{
				StartTime: apiv1.NewOptString("01:30"),
			})
			return err
		},
	},
	{
		op:   "UpdateMaintenanceChainSchedule",
		name: "out_of_range_weekly_scrub_day_is_refused",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateMaintenanceChainSchedule(ctx, &apiv1.UpdateMaintenanceChainScheduleRequest{
				WeeklyScrubDay: apiv1.NewOptWeekday(9),
			})
			return err
		},
	},
	{
		op:   "UpdateMaintenanceChainSchedule",
		name: "malformed_start_time_is_refused",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateMaintenanceChainSchedule(ctx, &apiv1.UpdateMaintenanceChainScheduleRequest{
				StartTime: apiv1.NewOptString("not-a-clock"),
			})
			return err
		},
	},
	{
		op:   "UpdateScheduledJob",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateScheduledJob(ctx, &apiv1.UpdateScheduledJobRequest{
				Enabled: apiv1.NewOptBool(false),
			}, apiv1.UpdateScheduledJobParams{JobId: apiv1.OtherScheduleJobIdSmartSelfTest})
			return err
		},
	},
	{
		op:   "UpdateScheduledJob",
		name: "unknown_job_id",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateScheduledJob(ctx, &apiv1.UpdateScheduledJobRequest{
				Enabled: apiv1.NewOptBool(false),
			}, apiv1.UpdateScheduledJobParams{JobId: "not_a_real_job"})
			return err
		},
	},
	{
		op:   "UpdateScheduledJob",
		name: "unknown_frequency_is_refused",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateScheduledJob(ctx, &apiv1.UpdateScheduledJobRequest{
				Frequency: apiv1.NewOptScheduleFrequency("hourly"),
			}, apiv1.UpdateScheduledJobParams{JobId: apiv1.OtherScheduleJobIdSmartSelfTest})
			return err
		},
	},

	// --- General settings (#178) ---
	{
		op:   "GetGeneralSettings",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetGeneralSettings(ctx)
			return err
		},
	},
	{
		op:   "UpdateGeneralSettings",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateGeneralSettings(ctx, &apiv1.UpdateGeneralSettingsRequest{
				Hostname: apiv1.NewOptString("nas"),
			})
			return err
		},
	},
	{
		op:   "UpdateGeneralSettings",
		name: "unknown_timezone_is_refused",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateGeneralSettings(ctx, &apiv1.UpdateGeneralSettingsRequest{
				Timezone: apiv1.NewOptString("Not/AZone"),
			})
			return err
		},
	},

	// --- UPS / NUT settings (#249, doc 03 §8.1) ---
	{
		op:   "GetUPSSettings",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetUPSSettings(ctx)
			return err
		},
	},
	{
		op:   "UpdateUPSSettings",
		name: "valid_usb",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateUPSSettings(ctx, &apiv1.UpdateUPSSettingsRequest{
				Connection:      apiv1.UPSConnectionUsb,
				Driver:          apiv1.NewOptString("usbhid-ups"),
				Port:            apiv1.NewOptString("auto"),
				MonitorPassword: apiv1.NewOptString("s3cr3t-monitor-pass"),
			})
			return err
		},
	},
	{
		op:   "UpdateUPSSettings",
		name: "usb_missing_driver_is_refused",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateUPSSettings(ctx, &apiv1.UpdateUPSSettingsRequest{
				Connection:      apiv1.UPSConnectionUsb,
				Port:            apiv1.NewOptString("auto"),
				MonitorPassword: apiv1.NewOptString("pass"),
			})
			return err
		},
	},

	// --- Network / TLS / Let's Encrypt (Q75, #114, #211) ---
	{
		op:   "GetNetworkSettings",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetNetworkSettings(ctx)
			return err
		},
	},
	{
		op:   "ApplyNetworkSettings",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			req := &apiv1.ApplyNetworkSettingsRequest{}
			req.SetAllowAllSources(apiv1.NewOptBool(true))
			_, err := h.ApplyNetworkSettings(ctx, req)
			return err
		},
	},
	{
		op:   "ApplyNetworkSettings",
		name: "addressing_without_method_is_refused",
		run: func(ctx context.Context, h apiv1.Handler) error {
			req := &apiv1.ApplyNetworkSettingsRequest{}
			req.SetInterface(apiv1.NewOptString("enp1s0"))
			_, err := h.ApplyNetworkSettings(ctx, req)
			return err
		},
	},
	{
		// ApplyNetworkSettings with an addressing change (dhcp needs no
		// address/prefix), then ConfirmNetworkSettings on the pending
		// change it creates, succeeds on both sides.
		op:   "ConfirmNetworkSettings",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			req := &apiv1.ApplyNetworkSettingsRequest{}
			req.SetInterface(apiv1.NewOptString("enp1s0"))
			req.SetMethod(apiv1.NewOptNetworkAddressMethod(apiv1.NetworkAddressMethodDhcp))
			if _, err := h.ApplyNetworkSettings(ctx, req); err != nil {
				return err
			}
			_, err := h.ConfirmNetworkSettings(ctx)
			return err
		},
	},
	{
		op:   "ConfirmNetworkSettings",
		name: "no_pending_change_is_refused",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ConfirmNetworkSettings(ctx)
			return err
		},
	},
	{
		op:   "RegenerateTLSCertificate",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.RegenerateTLSCertificate(ctx)
			return err
		},
	},
	{
		// acme.Service.Configure only validates and persists the DNS-01
		// setup — issuing the certificate itself is job.TypeACMEIssue,
		// registered with a no-op RunFunc in this rig
		// (contractProductionRunFuncs) — so a well-formed Cloudflare
		// setup succeeds on both sides without any real ACME/DNS call.
		op:   "ConfigureLetsEncrypt",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			req := &apiv1.ConfigureLetsEncryptRequest{
				Domain:   "nas.example.com",
				Provider: apiv1.DNS01ProviderCloudflare,
			}
			req.SetCloudflareAPIToken(apiv1.NewOptString("token"))
			_, err := h.ConfigureLetsEncrypt(ctx, req)
			return err
		},
	},
	{
		op:   "ConfigureLetsEncrypt",
		name: "invalid_domain_is_refused",
		run: func(ctx context.Context, h apiv1.Handler) error {
			req := &apiv1.ConfigureLetsEncryptRequest{
				Domain:   "not a host",
				Provider: apiv1.DNS01ProviderCloudflare,
			}
			req.SetCloudflareAPIToken(apiv1.NewOptString("token"))
			_, err := h.ConfigureLetsEncrypt(ctx, req)
			return err
		},
	},
	{
		op:   "ConfigureLetsEncrypt",
		name: "missing_credential_is_refused",
		run: func(ctx context.Context, h apiv1.Handler) error {
			req := &apiv1.ConfigureLetsEncryptRequest{
				Domain:   "nas.example.com",
				Provider: apiv1.DNS01ProviderCloudflare,
			}
			_, err := h.ConfigureLetsEncrypt(ctx, req)
			return err
		},
	},
	{
		op:   "DisableLetsEncrypt",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.DisableLetsEncrypt(ctx)
			return err
		},
	},

	// --- External disks (Q72, #288) ---
	{
		op:   "ListExternalDisks",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ListExternalDisks(ctx)
			return err
		},
	},
	{
		op:   "RegisterExternalDisk",
		name: "valid_new_device",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.RegisterExternalDisk(ctx, &apiv1.RegisterExternalDiskRequest{Device: "/dev/sdf", Label: "backup2"})
			return err
		},
	},
	{
		// Production's RegisterExternalDisk refuses a device its own
		// inventory (disk.Provider) has never heard of with
		// unmanaged_device/400 (external_handler.go); the mock mirrors
		// that check (external.go) instead of registering any device
		// string unconditionally.
		op:   "RegisterExternalDisk",
		name: "unmanaged_device_is_refused",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.RegisterExternalDisk(ctx, &apiv1.RegisterExternalDiskRequest{Device: "/dev/zzz", Label: "backup3"})
			return err
		},
	},
	{
		// Production's RegisterExternalDisk refuses a device already
		// holding an array role with invalid_plan/400
		// (disk.ErrExternalInArray, external_handler.go); the mock
		// mirrors that check (external.go). /dev/sdb is mockArrayDisks'
		// disk1 in every scenario but fresh-install.
		op:   "RegisterExternalDisk",
		name: "array_member_is_refused",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.RegisterExternalDisk(ctx, &apiv1.RegisterExternalDiskRequest{Device: "/dev/sdb", Label: "backup4"})
			return err
		},
	},
	{
		// Production's own disk.ValidateExternalLabel
		// (external_handler.go) refuses a label that is not a single
		// path segment under ExternalMountRoot with invalid_plan/400;
		// the mock runs this same check (external.go).
		op:   "RegisterExternalDisk",
		name: "invalid_label_is_refused",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.RegisterExternalDisk(ctx, &apiv1.RegisterExternalDiskRequest{Device: "/dev/sdf", Label: "../etc"})
			return err
		},
	},
	{
		// Register a new external disk, then update it, succeeds on
		// both sides.
		op:   "UpdateExternalDisk",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.RegisterExternalDisk(ctx, &apiv1.RegisterExternalDiskRequest{Device: "/dev/sdf", Label: "backup2"}); err != nil {
				return err
			}
			_, err := h.UpdateExternalDisk(ctx, &apiv1.UpdateExternalDiskRequest{BackupDestination: apiv1.NewOptBool(true)}, apiv1.UpdateExternalDiskParams{Label: "backup2"})
			return err
		},
	},
	{
		op:   "UpdateExternalDisk",
		name: "unknown_label",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateExternalDisk(ctx, &apiv1.UpdateExternalDiskRequest{BackupDestination: apiv1.NewOptBool(true)}, apiv1.UpdateExternalDiskParams{Label: "nope"})
			return err
		},
	},
	{
		// MountExternalDisk on the "backup" udev label
		// (mockDiskInventory's own mockUSBDisk, already carrying a
		// filesystem) succeeds on both sides.
		op:   "MountExternalDisk",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.MountExternalDisk(ctx, apiv1.MountExternalDiskParams{Label: "backup"})
			return err
		},
	},
	{
		op:   "MountExternalDisk",
		name: "unknown_label",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.MountExternalDisk(ctx, apiv1.MountExternalDiskParams{Label: "nope"})
			return err
		},
	},
	{
		op:   "EjectExternalDisk",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.MountExternalDisk(ctx, apiv1.MountExternalDiskParams{Label: "backup"}); err != nil {
				return err
			}
			_, err := h.EjectExternalDisk(ctx, apiv1.EjectExternalDiskParams{Label: "backup"})
			return err
		},
	},
	{
		op:   "EjectExternalDisk",
		name: "unknown_label",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.EjectExternalDisk(ctx, apiv1.EjectExternalDiskParams{Label: "nope"})
			return err
		},
	},
	{
		op:   "FormatExternalDisk",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.FormatExternalDisk(ctx, &apiv1.FormatExternalDiskRequest{Confirmation: "ERASE /dev/sdf"}, apiv1.FormatExternalDiskParams{Label: "backup"})
			return err
		},
	},
	{
		op:   "FormatExternalDisk",
		name: "missing_confirmation",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.FormatExternalDisk(ctx, &apiv1.FormatExternalDiskRequest{}, apiv1.FormatExternalDiskParams{Label: "backup"})
			return err
		},
	},

	// --- Host-config import and doctor (Q76) ---
	{
		op:   "RunDoctor",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.RunDoctor(ctx)
			return err
		},
	},
	{
		// This rig's Docker fake (config.MemoryDocker{},
		// contract_rig_test.go) answers List() with no error, so
		// production's own config.Detect/HostInventory.Found (host.go)
		// treats host_docker_containers as detected even though this
		// rig's host root is an otherwise-empty t.TempDir() with no
		// other host-config file planted — leave is a no-op decision
		// either way, so this is a real success on both sides without
		// any filesystem fixture.
		op:   "ApplyHostConfig",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ApplyHostConfig(ctx, &apiv1.ApplyHostConfigRequest{
				Files: []apiv1.HostConfigChoice{{ID: "host_docker_containers", Decision: apiv1.HostConfigDecisionLeave}},
			})
			return err
		},
	},
	{
		// Production's ApplyHostConfig refuses a host-config id it
		// doesn't recognise with invalid_host_config/400
		// (config.KindFromCheckID, hostconfig.go); the mock mirrors
		// that check against mockHostConfigKinds (phase1.go) instead
		// of echoing any id back as if it had been applied.
		op:   "ApplyHostConfig",
		name: "unknown_host_config_id_is_refused",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ApplyHostConfig(ctx, &apiv1.ApplyHostConfigRequest{
				Files: []apiv1.HostConfigChoice{{ID: "not_a_real_check", Decision: apiv1.HostConfigDecisionLeave}},
			})
			return err
		},
	},

	// --- Metrics and wake events (Q74, Q32) ---
	{
		op:   "GetMetrics",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			from := contractNow.Add(-time.Hour)
			_, err := h.GetMetrics(ctx, apiv1.GetMetricsParams{Metric: "disk_throughput_bytes_per_sec", From: from, To: contractNow})
			return err
		},
	},
	{
		op:   "GetMetrics",
		name: "from_not_before_to_is_refused",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetMetrics(ctx, apiv1.GetMetricsParams{Metric: "disk_throughput_bytes_per_sec", From: contractNow, To: contractNow})
			return err
		},
	},
	{
		op:   "ListWakeEvents",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ListWakeEvents(ctx)
			return err
		},
	},

	// --- Self-update (Q67, Q68) ---
	{
		op:   "CheckForUpdate",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.CheckForUpdate(ctx)
			return err
		},
	},
	{
		op:   "GetUpdateStatus",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetUpdateStatus(ctx)
			return err
		},
	},
	{
		op:   "UpdateUpdateSettings",
		name: "valid_channel_change",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateUpdateSettings(ctx, &apiv1.UpdateUpdateSettingsRequest{Channel: apiv1.NewOptUpdateChannel(apiv1.UpdateChannelBeta)})
			return err
		},
	},
	{
		// #373: an unrecognized channel is bad client input (400), not an
		// opaque 500 — the generated decoder accepts any string here
		// (UpdateChannel.Decode has no default-case error), so both
		// handlers must reject it themselves.
		op:   "UpdateUpdateSettings",
		name: "unknown_channel",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateUpdateSettings(ctx, &apiv1.UpdateUpdateSettingsRequest{Channel: apiv1.NewOptUpdateChannel(apiv1.UpdateChannel("nightly"))})
			return err
		},
	},
	{
		// CheckForUpdate (both sides' own fixture reports v0.2.0
		// available over the current 0.1.0 — update_handler_test.go's
		// own newUpdateHandler shape), then ApplyUpdate with
		// Confirm: true, succeeds on both sides.
		op:   "ApplyUpdate",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CheckForUpdate(ctx); err != nil {
				return err
			}
			_, err := h.ApplyUpdate(ctx, &apiv1.ConfirmUpdateRequest{Confirm: true})
			return err
		},
	},
	{
		op:   "ApplyUpdate",
		name: "missing_confirm",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ApplyUpdate(ctx, &apiv1.ConfirmUpdateRequest{Confirm: false})
			return err
		},
	},
	{
		// CheckForUpdate, ApplyUpdate (so a previous version exists to
		// roll back to), then RollbackUpdate with Confirm: true,
		// succeeds on both sides.
		op:   "RollbackUpdate",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CheckForUpdate(ctx); err != nil {
				return err
			}
			if _, err := h.ApplyUpdate(ctx, &apiv1.ConfirmUpdateRequest{Confirm: true}); err != nil {
				return err
			}
			_, err := h.RollbackUpdate(ctx, &apiv1.ConfirmUpdateRequest{Confirm: true})
			return err
		},
	},
	{
		op:   "RollbackUpdate",
		name: "missing_confirm",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.RollbackUpdate(ctx, &apiv1.ConfirmUpdateRequest{Confirm: false})
			return err
		},
	},
	{
		op:   "RebootHost",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.RebootHost(ctx, &apiv1.ConfirmUpdateRequest{Confirm: true})
			return err
		},
	},
	{
		op:   "RebootHost",
		name: "missing_confirm",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.RebootHost(ctx, &apiv1.ConfirmUpdateRequest{Confirm: false})
			return err
		},
	},

	// --- Config export/import (#272: ImportConfig's own confirm check
	// runs before touching h.Backup, so it needs no backup.Service
	// wiring at all — see contract_test.go's contractSkip for why
	// ExportConfig itself is not here) ---
	{
		op:   "ImportConfig",
		name: "missing_confirm",
		run: func(ctx context.Context, h apiv1.Handler) error {
			return h.ImportConfig(ctx, &apiv1.ImportConfigReq{Confirm: false})
		},
	},
}
