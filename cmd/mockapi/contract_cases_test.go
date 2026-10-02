package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	ht "github.com/ogen-go/ogen/http"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/template"
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
	// --- Apps (#67: Docker Engine client and Compose stacks, part A) ---
	{
		op:   "ListApps",
		name: "valid_request",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ListApps(ctx)
			return err
		},
	},
	{
		op:   "GetApp",
		name: "valid_by_name",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetApp(ctx, apiv1.GetAppParams{ID: "jellyfin"})
			return err
		},
	},
	{
		op:   "GetApp",
		name: "unknown_id_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetApp(ctx, apiv1.GetAppParams{ID: "no-such-container"})
			return err
		},
	},
	{
		op:   "ListAppImages",
		name: "valid_request",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ListAppImages(ctx)
			return err
		},
	},
	{
		op:   "ListDockerNetworks",
		name: "valid_request",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ListDockerNetworks(ctx)
			return err
		},
	},
	{
		op:   "ListAppUpdates",
		name: "valid_request",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ListAppUpdates(ctx)
			return err
		},
	},
	// --- Registry credentials (#488) ---
	{
		op:   "ListRegistryCredentials",
		name: "valid_request",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ListRegistryCredentials(ctx)
			return err
		},
	},
	{
		op:   "PutRegistryCredential",
		name: "valid_request",
		run: func(ctx context.Context, h apiv1.Handler) error {
			return h.PutRegistryCredential(ctx, &apiv1.PutRegistryCredentialRequest{Username: "me", Password: "s3cret"}, apiv1.PutRegistryCredentialParams{Registry: "ghcr.io"})
		},
	},
	{
		op:   "PutRegistryCredential",
		name: "a_name_that_is_not_a_registry_host_is_refused",
		run: func(ctx context.Context, h apiv1.Handler) error {
			return h.PutRegistryCredential(ctx, &apiv1.PutRegistryCredentialRequest{Username: "me", Password: "s3cret"}, apiv1.PutRegistryCredentialParams{Registry: "ghcr"})
		},
	},
	{
		op:   "PutRegistryCredential",
		name: "a_username_with_a_colon_is_refused",
		run: func(ctx context.Context, h apiv1.Handler) error {
			return h.PutRegistryCredential(ctx, &apiv1.PutRegistryCredentialRequest{Username: "a:b", Password: "s3cret"}, apiv1.PutRegistryCredentialParams{Registry: "ghcr.io"})
		},
	},
	{
		op:   "DeleteRegistryCredential",
		name: "valid_saved_credential",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if err := h.PutRegistryCredential(ctx, &apiv1.PutRegistryCredentialRequest{Username: "me", Password: "s3cret"}, apiv1.PutRegistryCredentialParams{Registry: "registry.example.com:5000"}); err != nil {
				return err
			}
			return h.DeleteRegistryCredential(ctx, apiv1.DeleteRegistryCredentialParams{Registry: "registry.example.com:5000"})
		},
	},
	{
		op:   "DeleteRegistryCredential",
		name: "a_registry_with_none_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			return h.DeleteRegistryCredential(ctx, apiv1.DeleteRegistryCredentialParams{Registry: "ghcr.io"})
		},
	},
	// --- Container update execution (#284) ---
	{
		op:   "UpdateApp",
		name: "valid_queues_a_job",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateApp(ctx, apiv1.UpdateAppParams{ID: "jellyfin"})
			return err
		},
	},
	{
		op:   "UpdateApp",
		name: "unknown_id_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateApp(ctx, apiv1.UpdateAppParams{ID: "no-such-container"})
			return err
		},
	},
	{
		op:   "UpdateApp",
		name: "refused_while_the_array_is_stopped",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
				return err
			}
			_, err := h.UpdateApp(ctx, apiv1.UpdateAppParams{ID: "jellyfin"})
			return err
		},
	},
	{
		op:   "RevertApp",
		name: "nothing_to_revert_for_a_container_never_updated",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.RevertApp(ctx, apiv1.RevertAppParams{ID: "jellyfin"})
			return err
		},
	},
	{
		op:   "UpdateApp",
		name: "a_container_pinned_to_a_digest_is_refused",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateApp(ctx, apiv1.UpdateAppParams{ID: "transcoder"})
			return err
		},
	},
	{
		op:   "StartAppUpdates",
		name: "a_named_container_pinned_to_a_digest_is_refused",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.StartAppUpdates(ctx, apiv1.NewOptStartAppUpdatesRequest(apiv1.StartAppUpdatesRequest{Containers: []string{"jellyfin", "transcoder"}}))
			return err
		},
	},
	{
		op:   "RevertApp",
		name: "valid_recorded_update_queues_a_job",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if err := seedJellyfinUpdate(ctx, h, 7*24*time.Hour); err != nil {
				return err
			}
			_, err := h.RevertApp(ctx, apiv1.RevertAppParams{ID: "jellyfin"})
			return err
		},
	},
	{
		op:   "RevertApp",
		name: "revert_unavailable_once_the_keep_period_is_over",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if err := seedJellyfinUpdate(ctx, h, -time.Hour); err != nil {
				return err
			}
			_, err := h.RevertApp(ctx, apiv1.RevertAppParams{ID: "jellyfin"})
			return err
		},
	},
	{
		op:   "RevertApp",
		name: "unknown_id_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.RevertApp(ctx, apiv1.RevertAppParams{ID: "no-such-container"})
			return err
		},
	},
	{
		op:   "RevertApp",
		name: "refused_while_the_array_is_stopped",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
				return err
			}
			_, err := h.RevertApp(ctx, apiv1.RevertAppParams{ID: "jellyfin"})
			return err
		},
	},
	{
		op:   "StartAppUpdates",
		name: "valid_named_container",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.StartAppUpdates(ctx, apiv1.NewOptStartAppUpdatesRequest(apiv1.StartAppUpdatesRequest{Containers: []string{"jellyfin"}}))
			return err
		},
	},
	{
		op:   "StartAppUpdates",
		name: "valid_no_names",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.StartAppUpdates(ctx, apiv1.OptStartAppUpdatesRequest{})
			return err
		},
	},
	{
		op:   "StartAppUpdates",
		name: "unknown_container_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.StartAppUpdates(ctx, apiv1.NewOptStartAppUpdatesRequest(apiv1.StartAppUpdatesRequest{Containers: []string{"jellyfin", "no-such-container"}}))
			return err
		},
	},
	{
		op:   "StartAppUpdates",
		name: "refused_while_the_array_is_stopped",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
				return err
			}
			_, err := h.StartAppUpdates(ctx, apiv1.OptStartAppUpdatesRequest{})
			return err
		},
	},
	{
		op:   "SetAppUpdatePolicy",
		name: "valid_request",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.SetAppUpdatePolicy(ctx, &apiv1.SetAppUpdatePolicyRequest{BulkExcluded: true}, apiv1.SetAppUpdatePolicyParams{ID: "jellyfin"})
			return err
		},
	},
	{
		op:   "SetAppUpdatePolicy",
		name: "unknown_id_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.SetAppUpdatePolicy(ctx, &apiv1.SetAppUpdatePolicyRequest{BulkExcluded: true}, apiv1.SetAppUpdatePolicyParams{ID: "no-such-container"})
			return err
		},
	},
	{
		op:   "ListAppUpdateHistory",
		name: "valid_request",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ListAppUpdateHistory(ctx)
			return err
		},
	},
	{
		op:   "GetAppSettings",
		name: "valid_request",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetAppSettings(ctx)
			return err
		},
	},
	{
		op:   "UpdateAppSettings",
		name: "valid_request",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateAppSettings(ctx, &apiv1.AppSettings{ImageKeepDays: 30})
			return err
		},
	},
	{
		op:   "UpdateAppSettings",
		name: "zero_days_is_invalid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateAppSettings(ctx, &apiv1.AppSettings{ImageKeepDays: 0})
			return err
		},
	},
	{
		op:   "UpdateAppSettings",
		name: "more_than_a_year_is_invalid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateAppSettings(ctx, &apiv1.AppSettings{ImageKeepDays: 366})
			return err
		},
	},
	// --- Compose stacks (#278) ---
	{
		op:   "ListStacks",
		name: "valid_request",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ListStacks(ctx)
			return err
		},
	},
	{
		op:   "CreateStack",
		name: "valid_request",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.CreateStack(ctx, &apiv1.CreateStackRequest{Name: "nginx", Compose: "services:\n  web:\n    image: nginx\n"})
			return err
		},
	},
	{
		op:   "CreateStack",
		name: "path_traversal_name_is_rejected",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.CreateStack(ctx, &apiv1.CreateStackRequest{Name: "../etc", Compose: "services: {}\n"})
			return err
		},
	},
	{
		op:   "CreateStack",
		name: "empty_compose_is_rejected",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.CreateStack(ctx, &apiv1.CreateStackRequest{Name: "nginx", Compose: " \n"})
			return err
		},
	},
	{
		op:   "CreateStack",
		name: "env_defining_a_reserved_docker_variable_is_rejected",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.CreateStack(ctx, &apiv1.CreateStackRequest{Name: "nginx", Compose: "services: {}\n", Env: apiv1.NewOptString("PATH=/stack/bin\n")})
			return err
		},
	},
	{
		op:   "CreateStack",
		name: "duplicate_name_is_a_conflict",
		run: func(ctx context.Context, h apiv1.Handler) error {
			req := &apiv1.CreateStackRequest{Name: "nginx", Compose: "services: {}\n"}
			if _, err := h.CreateStack(ctx, req); err != nil {
				return err
			}
			_, err := h.CreateStack(ctx, req)
			return err
		},
	},
	{
		op:   "GetStack",
		name: "valid_created_stack",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CreateStack(ctx, &apiv1.CreateStackRequest{Name: "nginx", Compose: "services: {}\n"}); err != nil {
				return err
			}
			_, err := h.GetStack(ctx, apiv1.GetStackParams{Name: "nginx"})
			return err
		},
	},
	{
		op:   "GetStack",
		name: "unknown_name_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetStack(ctx, apiv1.GetStackParams{Name: "nginx"})
			return err
		},
	},
	{
		op:   "RemoveStack",
		name: "valid_created_stack",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CreateStack(ctx, &apiv1.CreateStackRequest{Name: "nginx", Compose: "services: {}\n"}); err != nil {
				return err
			}
			_, err := h.RemoveStack(ctx, apiv1.RemoveStackParams{Name: "nginx"})
			return err
		},
	},
	{
		op:   "RemoveStack",
		name: "valid_remove_then_create_again",
		run: func(ctx context.Context, h apiv1.Handler) error {
			req := &apiv1.CreateStackRequest{Name: "plex", Compose: "services: {}\n"}
			if _, err := h.CreateStack(ctx, req); err != nil {
				return err
			}
			if _, err := h.RemoveStack(ctx, apiv1.RemoveStackParams{Name: "plex"}); err != nil {
				return err
			}
			_, err := h.CreateStack(ctx, req)
			return err
		},
	},
	{
		op:   "RemoveStack",
		name: "appdata_deletion_of_an_unknown_name_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.RemoveStack(ctx, apiv1.RemoveStackParams{Name: "nginx", DeleteAppdata: apiv1.NewOptBool(true)})
			return err
		},
	},
	{
		op:   "RemoveStack",
		name: "appdata_deletion_needs_an_appdata_location",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CreateStack(ctx, &apiv1.CreateStackRequest{Name: "nginx", Compose: "services: {}\n"}); err != nil {
				return err
			}
			_, err := h.RemoveStack(ctx, apiv1.RemoveStackParams{Name: "nginx", DeleteAppdata: apiv1.NewOptBool(true)})
			return err
		},
	},
	{
		op:   "RemoveStack",
		name: "unknown_name_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.RemoveStack(ctx, apiv1.RemoveStackParams{Name: "nginx"})
			return err
		},
	},
	{
		op:   "UpdateStack",
		name: "valid_edit_marks_the_stack_manually_edited",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CreateStack(ctx, &apiv1.CreateStackRequest{Name: "nginx", Compose: "services: {}\n"}); err != nil {
				return err
			}
			_, err := h.UpdateStack(ctx, &apiv1.UpdateStackRequest{Compose: "services:\n  web:\n    image: nginx:1.28\n"}, apiv1.UpdateStackParams{Name: "nginx"})
			return err
		},
	},
	{
		op:   "UpdateStack",
		name: "valid_dry_run",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CreateStack(ctx, &apiv1.CreateStackRequest{Name: "nginx", Compose: "services: {}\n"}); err != nil {
				return err
			}
			_, err := h.UpdateStack(ctx, &apiv1.UpdateStackRequest{Compose: "services: {}\n"}, apiv1.UpdateStackParams{Name: "nginx", DryRun: apiv1.NewOptBool(true)})
			return err
		},
	},
	{
		op:   "UpdateStack",
		name: "a_file_compose_rejects_is_invalid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CreateStack(ctx, &apiv1.CreateStackRequest{Name: "nginx", Compose: "services: {}\n"}); err != nil {
				return err
			}
			_, err := h.UpdateStack(ctx, &apiv1.UpdateStackRequest{Compose: "services: [\n"}, apiv1.UpdateStackParams{Name: "nginx"})
			return err
		},
	},
	{
		op:   "UpdateStack",
		name: "an_invalid_file_is_refused_by_a_dry_run_too",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CreateStack(ctx, &apiv1.CreateStackRequest{Name: "nginx", Compose: "services: {}\n"}); err != nil {
				return err
			}
			_, err := h.UpdateStack(ctx, &apiv1.UpdateStackRequest{Compose: "services: [\n"}, apiv1.UpdateStackParams{Name: "nginx", DryRun: apiv1.NewOptBool(true)})
			return err
		},
	},
	{
		op:   "UpdateStack",
		name: "empty_compose_is_rejected_before_the_stack_is_looked_up",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateStack(ctx, &apiv1.UpdateStackRequest{Compose: " \n"}, apiv1.UpdateStackParams{Name: "nginx"})
			return err
		},
	},
	{
		op:   "UpdateStack",
		name: "path_traversal_name_is_rejected",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateStack(ctx, &apiv1.UpdateStackRequest{Compose: "services: {}\n"}, apiv1.UpdateStackParams{Name: "../etc"})
			return err
		},
	},
	{
		op:   "UpdateStack",
		name: "unknown_name_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateStack(ctx, &apiv1.UpdateStackRequest{Compose: "services: {}\n"}, apiv1.UpdateStackParams{Name: "nginx"})
			return err
		},
	},
	{
		op:   "StartStack",
		name: "valid_queues_a_job",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CreateStack(ctx, &apiv1.CreateStackRequest{Name: "nginx", Compose: "services: {}\n"}); err != nil {
				return err
			}
			_, err := h.StartStack(ctx, apiv1.StartStackParams{Name: "nginx"})
			return err
		},
	},
	{
		op:   "StartStack",
		name: "unknown_name_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.StartStack(ctx, apiv1.StartStackParams{Name: "nginx"})
			return err
		},
	},
	{
		op:   "StartStack",
		name: "path_traversal_name_is_rejected",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.StartStack(ctx, apiv1.StartStackParams{Name: "../etc"})
			return err
		},
	},
	{
		op:   "StartStack",
		name: "refused_while_the_array_is_stopped",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CreateStack(ctx, &apiv1.CreateStackRequest{Name: "nginx", Compose: "services: {}\n"}); err != nil {
				return err
			}
			if _, err := h.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
				return err
			}
			_, err := h.StartStack(ctx, apiv1.StartStackParams{Name: "nginx"})
			return err
		},
	},
	// --- Installed stack's template inputs (#519) ---
	{
		op:   "GetStackConfig",
		name: "valid_installed_stack",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{}, apiv1.InstallTemplateParams{ID: "aio-notes"}); err != nil {
				return err
			}
			_, err := h.GetStackConfig(ctx, apiv1.GetStackConfigParams{Name: "aio-notes"})
			return err
		},
	},
	{
		op:   "GetStackConfig",
		name: "stack_made_without_a_template_has_none",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CreateStack(ctx, &apiv1.CreateStackRequest{Name: "plain", Compose: "services: {}\n"}); err != nil {
				return err
			}
			_, err := h.GetStackConfig(ctx, apiv1.GetStackConfigParams{Name: "plain"})
			return err
		},
	},
	{
		op:   "GetStackConfig",
		name: "unknown_name_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetStackConfig(ctx, apiv1.GetStackConfigParams{Name: "nope"})
			return err
		},
	},
	{
		op:   "GetStackConfig",
		name: "path_traversal_name_is_rejected",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetStackConfig(ctx, apiv1.GetStackConfigParams{Name: "../etc"})
			return err
		},
	},
	{
		op:   "UpdateStackConfig",
		name: "valid_change_of_a_port_and_a_generated_secret",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{}, apiv1.InstallTemplateParams{ID: "aio-notes"}); err != nil {
				return err
			}
			req := &apiv1.UpdateStackConfigRequest{
				Values:   apiv1.NewOptUpdateStackConfigRequestValues(apiv1.UpdateStackConfigRequestValues{"WEBUI_PORT": "3100", "APPDATA": "/mnt/cache/notes"}),
				Generate: []string{"DB_PASSWORD"},
			}
			_, err := h.UpdateStackConfig(ctx, req, apiv1.UpdateStackConfigParams{Name: "aio-notes"})
			return err
		},
	},
	{
		op:   "UpdateStackConfig",
		name: "port_another_container_takes",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{}, apiv1.InstallTemplateParams{ID: "aio-notes"}); err != nil {
				return err
			}
			req := &apiv1.UpdateStackConfigRequest{Values: apiv1.NewOptUpdateStackConfigRequestValues(apiv1.UpdateStackConfigRequestValues{"WEBUI_PORT": "8096"})}
			_, err := h.UpdateStackConfig(ctx, req, apiv1.UpdateStackConfigParams{Name: "aio-notes"})
			return err
		},
	},
	{
		op:   "UpdateStackConfig",
		name: "port_out_of_range",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{}, apiv1.InstallTemplateParams{ID: "aio-notes"}); err != nil {
				return err
			}
			req := &apiv1.UpdateStackConfigRequest{Values: apiv1.NewOptUpdateStackConfigRequestValues(apiv1.UpdateStackConfigRequestValues{"WEBUI_PORT": "70000"})}
			_, err := h.UpdateStackConfig(ctx, req, apiv1.UpdateStackConfigParams{Name: "aio-notes"})
			return err
		},
	},
	{
		op:   "UpdateStackConfig",
		name: "device_input_cannot_change",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{}, apiv1.InstallTemplateParams{ID: "jellyfin"}); err != nil {
				return err
			}
			req := &apiv1.UpdateStackConfigRequest{Values: apiv1.NewOptUpdateStackConfigRequestValues(apiv1.UpdateStackConfigRequestValues{"TRANSCODE_GPU": "/dev/dri/renderD128"})}
			_, err := h.UpdateStackConfig(ctx, req, apiv1.UpdateStackConfigParams{Name: "jellyfin"})
			return err
		},
	},
	{
		op:   "UpdateStackConfig",
		name: "generate_of_a_name_that_is_not_a_secret",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{}, apiv1.InstallTemplateParams{ID: "aio-notes"}); err != nil {
				return err
			}
			_, err := h.UpdateStackConfig(ctx, &apiv1.UpdateStackConfigRequest{Generate: []string{"WEBUI_PORT"}}, apiv1.UpdateStackConfigParams{Name: "aio-notes"})
			return err
		},
	},
	{
		op:   "UpdateStackConfig",
		name: "value_for_an_input_the_stack_lacks",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{}, apiv1.InstallTemplateParams{ID: "aio-notes"}); err != nil {
				return err
			}
			req := &apiv1.UpdateStackConfigRequest{Values: apiv1.NewOptUpdateStackConfigRequestValues(apiv1.UpdateStackConfigRequestValues{"NOPE": "1"})}
			_, err := h.UpdateStackConfig(ctx, req, apiv1.UpdateStackConfigParams{Name: "aio-notes"})
			return err
		},
	},
	{
		op:   "UpdateStackConfig",
		name: "stack_made_without_a_template_has_none",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CreateStack(ctx, &apiv1.CreateStackRequest{Name: "plain", Compose: "services: {}\n"}); err != nil {
				return err
			}
			_, err := h.UpdateStackConfig(ctx, &apiv1.UpdateStackConfigRequest{}, apiv1.UpdateStackConfigParams{Name: "plain"})
			return err
		},
	},
	{
		op:   "UpdateStackConfig",
		name: "unknown_name_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateStackConfig(ctx, &apiv1.UpdateStackConfigRequest{}, apiv1.UpdateStackConfigParams{Name: "nope"})
			return err
		},
	},
	{
		op:   "UpdateStackConfig",
		name: "path_traversal_name_is_rejected",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateStackConfig(ctx, &apiv1.UpdateStackConfigRequest{}, apiv1.UpdateStackConfigParams{Name: "../etc"})
			return err
		},
	},
	// --- Unraid template converter (#69) ---
	{
		op:   "ConvertUnraidTemplate",
		name: "valid_template_with_untranslated_flag",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ConvertUnraidTemplate(ctx, &apiv1.UnraidConvertRequest{XML: `<Container><Name>a</Name><Repository>x/y:1</Repository><ExtraParams>--exotic=1</ExtraParams></Container>`})
			return err
		},
	},
	{
		op:   "ConvertUnraidTemplate",
		name: "not_a_template",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ConvertUnraidTemplate(ctx, &apiv1.UnraidConvertRequest{XML: `<Compose/>`})
			return err
		},
	},
	{
		op:   "ConvertUnraidTemplate",
		name: "template_without_an_image",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ConvertUnraidTemplate(ctx, &apiv1.UnraidConvertRequest{XML: `<Container><Name>a</Name></Container>`})
			return err
		},
	},
	// --- Template install (#280) ---
	{
		op:   "PreviewTemplateInstall",
		name: "valid_privileged_template",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.PreviewTemplateInstall(ctx, &apiv1.TemplateInstallRequest{}, apiv1.PreviewTemplateInstallParams{ID: "risky-agent"})
			return err
		},
	},
	{
		op:   "PreviewTemplateInstall",
		name: "unknown_template_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.PreviewTemplateInstall(ctx, &apiv1.TemplateInstallRequest{}, apiv1.PreviewTemplateInstallParams{ID: "nope"})
			return err
		},
	},
	{
		op:   "PreviewTemplateInstall",
		name: "value_for_an_input_the_template_lacks",
		run: func(ctx context.Context, h apiv1.Handler) error {
			req := &apiv1.TemplateInstallRequest{Values: apiv1.NewOptTemplateInstallRequestValues(apiv1.TemplateInstallRequestValues{"NOPE": "1"})}
			_, err := h.PreviewTemplateInstall(ctx, req, apiv1.PreviewTemplateInstallParams{ID: "jellyfin"})
			return err
		},
	},
	{
		op:   "PreviewTemplateInstall",
		name: "invalid_stack_name",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.PreviewTemplateInstall(ctx, &apiv1.TemplateInstallRequest{Name: apiv1.NewOptString("../etc")}, apiv1.PreviewTemplateInstallParams{ID: "jellyfin"})
			return err
		},
	},
	{
		op:   "PreviewTemplateInstall",
		name: "valid_existing_network_with_limits_and_extra_parameters",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.PreviewTemplateInstall(ctx, &apiv1.TemplateInstallRequest{
				NetworkMode: apiv1.NewOptString("lan"),
				Restart:     apiv1.NewOptTemplateInstallRequestRestart(apiv1.TemplateInstallRequestRestartAlways),
				Cpus:        apiv1.NewOptFloat64(1.5),
				MemoryMiB:   apiv1.NewOptInt(512),
				ExtraParams: apiv1.NewOptString("--cap-add NET_ADMIN --no-such-flag"),
			}, apiv1.PreviewTemplateInstallParams{ID: "jellyfin"})
			return err
		},
	},
	{
		op:   "PreviewTemplateInstall",
		name: "valid_missing_network_is_a_warning",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.PreviewTemplateInstall(ctx, &apiv1.TemplateInstallRequest{NetworkMode: apiv1.NewOptString("iot")}, apiv1.PreviewTemplateInstallParams{ID: "jellyfin"})
			return err
		},
	},
	{
		op:   "PreviewTemplateInstall",
		name: "network_name_that_is_not_one",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.PreviewTemplateInstall(ctx, &apiv1.TemplateInstallRequest{NetworkMode: apiv1.NewOptString("my net")}, apiv1.PreviewTemplateInstallParams{ID: "jellyfin"})
			return err
		},
	},
	{
		op:   "PreviewTemplateInstall",
		name: "cpu_limit_out_of_range",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.PreviewTemplateInstall(ctx, &apiv1.TemplateInstallRequest{Cpus: apiv1.NewOptFloat64(0)}, apiv1.PreviewTemplateInstallParams{ID: "jellyfin"})
			return err
		},
	},
	{
		op:   "PreviewTemplateInstall",
		name: "extra_parameters_that_clash_with_a_limit",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.PreviewTemplateInstall(ctx, &apiv1.TemplateInstallRequest{MemoryMiB: apiv1.NewOptInt(512), ExtraParams: apiv1.NewOptString("--memory 1g")}, apiv1.PreviewTemplateInstallParams{ID: "jellyfin"})
			return err
		},
	},
	{
		op:   "PreviewTemplateInstall",
		name: "extra_parameter_port_a_container_holds",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.PreviewTemplateInstall(ctx, &apiv1.TemplateInstallRequest{ExtraParams: apiv1.NewOptString("-p 8096:80")}, apiv1.PreviewTemplateInstallParams{ID: "jellyfin"})
			return err
		},
	},
	{
		op:   "PreviewTemplateInstall",
		name: "network_mode_for_a_template_with_several_services",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.PreviewTemplateInstall(ctx, &apiv1.TemplateInstallRequest{NetworkMode: apiv1.NewOptString("host")}, apiv1.PreviewTemplateInstallParams{ID: "aio-notes"})
			return err
		},
	},
	{
		op:   "InstallTemplate",
		name: "network_that_does_not_exist",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{NetworkMode: apiv1.NewOptString("iot")}, apiv1.InstallTemplateParams{ID: "jellyfin"})
			return err
		},
	},
	{
		op:   "InstallTemplate",
		name: "valid_template_with_a_secret",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{}, apiv1.InstallTemplateParams{ID: "aio-notes"})
			return err
		},
	},
	{
		op:   "InstallTemplate",
		name: "valid_template_with_a_gpu",
		run: func(ctx context.Context, h apiv1.Handler) error {
			req := &apiv1.TemplateInstallRequest{Values: apiv1.NewOptTemplateInstallRequestValues(apiv1.TemplateInstallRequestValues{"TRANSCODE_GPU": "/dev/dri/renderD128"})}
			_, err := h.InstallTemplate(ctx, req, apiv1.InstallTemplateParams{ID: "jellyfin"})
			return err
		},
	},
	{
		op:   "InstallTemplate",
		name: "gpu_the_host_does_not_offer",
		run: func(ctx context.Context, h apiv1.Handler) error {
			req := &apiv1.TemplateInstallRequest{Values: apiv1.NewOptTemplateInstallRequestValues(apiv1.TemplateInstallRequestValues{"TRANSCODE_GPU": "/dev/dri/renderD129"})}
			_, err := h.InstallTemplate(ctx, req, apiv1.InstallTemplateParams{ID: "jellyfin"})
			return err
		},
	},
	{
		op:   "InstallTemplate",
		name: "port_out_of_range",
		run: func(ctx context.Context, h apiv1.Handler) error {
			req := &apiv1.TemplateInstallRequest{Values: apiv1.NewOptTemplateInstallRequestValues(apiv1.TemplateInstallRequestValues{"WEBUI_PORT": "70000"})}
			_, err := h.InstallTemplate(ctx, req, apiv1.InstallTemplateParams{ID: "jellyfin"})
			return err
		},
	},
	{
		op:   "InstallTemplate",
		name: "relative_path",
		run: func(ctx context.Context, h apiv1.Handler) error {
			req := &apiv1.TemplateInstallRequest{Values: apiv1.NewOptTemplateInstallRequestValues(apiv1.TemplateInstallRequestValues{"MEDIA": "media"})}
			_, err := h.InstallTemplate(ctx, req, apiv1.InstallTemplateParams{ID: "jellyfin"})
			return err
		},
	},
	{
		op:   "InstallTemplate",
		name: "unknown_template_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{}, apiv1.InstallTemplateParams{ID: "nope"})
			return err
		},
	},
	{
		op:   "InstallTemplate",
		name: "invalid_stack_name",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{Name: apiv1.NewOptString("Bad Name")}, apiv1.InstallTemplateParams{ID: "jellyfin"})
			return err
		},
	},
	{
		op:   "InstallTemplate",
		name: "name_already_used_by_a_stack",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CreateStack(ctx, &apiv1.CreateStackRequest{Name: "jellyfin", Compose: "services: {}\n"}); err != nil {
				return err
			}
			_, err := h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{}, apiv1.InstallTemplateParams{ID: "jellyfin"})
			return err
		},
	},
	{
		op:   "InstallTemplate",
		name: "installing_twice_under_the_same_name",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{}, apiv1.InstallTemplateParams{ID: "jellyfin"}); err != nil {
				return err
			}
			_, err := h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{}, apiv1.InstallTemplateParams{ID: "jellyfin"})
			return err
		},
	},
	// --- Catalog list, detail and icon (#513) ---
	{
		op:   "ListCatalog",
		name: "valid_list",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ListCatalog(ctx)
			return err
		},
	},
	{
		op:   "ListCatalog",
		name: "valid_list_with_an_installed_template",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{}, apiv1.InstallTemplateParams{ID: "jellyfin"}); err != nil {
				return err
			}
			_, err := h.ListCatalog(ctx)
			return err
		},
	},
	{
		op:   "RefreshCatalog",
		name: "valid_check",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.RefreshCatalog(ctx)
			return err
		},
	},
	{
		op:   "GetCatalogSettings",
		name: "valid_request",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetCatalogSettings(ctx)
			return err
		},
	},
	{
		op:   "UpdateCatalogSettings",
		name: "valid_both_settings",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateCatalogSettings(ctx, &apiv1.CatalogSettingsUpdate{
				RefreshInterval: apiv1.NewOptCatalogRefreshInterval(apiv1.CatalogRefreshInterval6h),
				CheckOnOpen:     apiv1.NewOptBool(false),
			})
			return err
		},
	},
	{
		op:   "UpdateCatalogSettings",
		name: "valid_only_the_interval_keeps_check_on_open",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.UpdateCatalogSettings(ctx, &apiv1.CatalogSettingsUpdate{CheckOnOpen: apiv1.NewOptBool(false)}); err != nil {
				return err
			}
			_, err := h.UpdateCatalogSettings(ctx, &apiv1.CatalogSettingsUpdate{RefreshInterval: apiv1.NewOptCatalogRefreshInterval(apiv1.CatalogRefreshIntervalOff)})
			return err
		},
	},
	{
		op:   "UpdateCatalogSettings",
		name: "unknown_interval_is_invalid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateCatalogSettings(ctx, &apiv1.CatalogSettingsUpdate{RefreshInterval: apiv1.NewOptCatalogRefreshInterval("2h")})
			return err
		},
	},
	{
		op:   "GetCatalogTemplate",
		name: "valid_privileged_template",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetCatalogTemplate(ctx, apiv1.GetCatalogTemplateParams{ID: "risky-agent"})
			return err
		},
	},
	{
		op:   "GetCatalogTemplate",
		name: "unknown_template_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetCatalogTemplate(ctx, apiv1.GetCatalogTemplateParams{ID: "nope"})
			return err
		},
	},
	{
		op:   "GetCatalogTemplate",
		name: "path_traversal_id_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetCatalogTemplate(ctx, apiv1.GetCatalogTemplateParams{ID: "../jellyfin"})
			return err
		},
	},
	{
		op:   "GetCatalogTemplateIcon",
		name: "valid_svg_icon",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetCatalogTemplateIcon(ctx, apiv1.GetCatalogTemplateIconParams{ID: "jellyfin"})
			return err
		},
	},
	{
		op:   "GetCatalogTemplateIcon",
		name: "unknown_template_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetCatalogTemplateIcon(ctx, apiv1.GetCatalogTemplateIconParams{ID: "nope"})
			return err
		},
	},
	{
		op:   "GetCatalogTemplateScreenshot",
		name: "valid_second_screenshot",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetCatalogTemplateScreenshot(ctx, apiv1.GetCatalogTemplateScreenshotParams{ID: "jellyfin", Index: 1})
			return err
		},
	},
	{
		op:   "GetCatalogTemplateScreenshot",
		name: "position_past_the_list_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetCatalogTemplateScreenshot(ctx, apiv1.GetCatalogTemplateScreenshotParams{ID: "jellyfin", Index: 2})
			return err
		},
	},
	{
		op:   "GetCatalogTemplateScreenshot",
		name: "template_without_screenshots_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetCatalogTemplateScreenshot(ctx, apiv1.GetCatalogTemplateScreenshotParams{ID: "aio-notes", Index: 0})
			return err
		},
	},
	{
		op:   "GetCatalogTemplateScreenshot",
		name: "unknown_template_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetCatalogTemplateScreenshot(ctx, apiv1.GetCatalogTemplateScreenshotParams{ID: "nope", Index: 0})
			return err
		},
	},
	{
		op:   "GetCatalogTemplateScreenshot",
		name: "path_traversal_id_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetCatalogTemplateScreenshot(ctx, apiv1.GetCatalogTemplateScreenshotParams{ID: "../jellyfin", Index: 0})
			return err
		},
	},
	// --- Catalog sources and the template-update check (#283) ---
	{
		op:   "ListCatalogSources",
		name: "valid_list_with_an_added_source",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.AddCatalogSource(ctx, &apiv1.AddCatalogSourceRequest{URL: "https://catalog.example.com/hoserva"}); err != nil {
				return err
			}
			_, err := h.ListCatalogSources(ctx)
			return err
		},
	},
	{
		op:   "AddCatalogSource",
		name: "valid_unsigned_source",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.AddCatalogSource(ctx, &apiv1.AddCatalogSourceRequest{URL: "https://catalog.example.com/hoserva/"})
			return err
		},
	},
	{
		op:   "AddCatalogSource",
		name: "valid_signed_source",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.AddCatalogSource(ctx, &apiv1.AddCatalogSourceRequest{URL: "https://catalog.example.com", PublicKey: apiv1.NewOptString(mockSourcePublicKey())})
			return err
		},
	},
	{
		op:   "AddCatalogSource",
		name: "http_url_is_invalid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.AddCatalogSource(ctx, &apiv1.AddCatalogSourceRequest{URL: "http://catalog.example.com"})
			return err
		},
	},
	{
		op:   "AddCatalogSource",
		name: "url_with_credentials_is_invalid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.AddCatalogSource(ctx, &apiv1.AddCatalogSourceRequest{URL: "https://user:secret@catalog.example.com"})
			return err
		},
	},
	{
		op:   "AddCatalogSource",
		name: "github_api_url_is_invalid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.AddCatalogSource(ctx, &apiv1.AddCatalogSourceRequest{URL: "https://api.github.com/repos/x/y"})
			return err
		},
	},
	{
		op:   "AddCatalogSource",
		name: "unreadable_public_key_is_invalid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.AddCatalogSource(ctx, &apiv1.AddCatalogSourceRequest{URL: "https://catalog.example.com", PublicKey: apiv1.NewOptString("not a key")})
			return err
		},
	},
	{
		op:   "AddCatalogSource",
		name: "blank_public_key_is_invalid_not_unsigned",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.AddCatalogSource(ctx, &apiv1.AddCatalogSourceRequest{URL: "https://catalog.example.com", PublicKey: apiv1.NewOptString("  ")})
			return err
		},
	},
	{
		op:   "AddCatalogSource",
		name: "unreachable_host_is_bad_gateway",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.AddCatalogSource(ctx, &apiv1.AddCatalogSourceRequest{URL: "https://" + mockDownHost})
			return err
		},
	},
	{
		op:   "AddCatalogSource",
		name: "archive_not_signed_by_the_given_key_is_rejected",
		run: func(ctx context.Context, h apiv1.Handler) error {
			other := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, ed25519.PublicKeySize))
			_, err := h.AddCatalogSource(ctx, &apiv1.AddCatalogSourceRequest{URL: "https://catalog.example.com", PublicKey: apiv1.NewOptString(other)})
			return err
		},
	},
	{
		op:   "AddCatalogSource",
		name: "second_add_of_the_same_url_conflicts",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.AddCatalogSource(ctx, &apiv1.AddCatalogSourceRequest{URL: "https://catalog.example.com/x"}); err != nil {
				return err
			}
			_, err := h.AddCatalogSource(ctx, &apiv1.AddCatalogSourceRequest{URL: "https://Catalog.Example.com/x/"})
			return err
		},
	},
	{
		op:   "AddCatalogSource",
		name: "the_curated_catalogs_own_url_conflicts",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.AddCatalogSource(ctx, &apiv1.AddCatalogSourceRequest{URL: template.DefaultCatalogURL})
			return err
		},
	},
	{
		op:   "RefreshCatalogSource",
		name: "valid_user_added_source",
		run: func(ctx context.Context, h apiv1.Handler) error {
			src, err := h.AddCatalogSource(ctx, &apiv1.AddCatalogSourceRequest{URL: "https://catalog.example.com"})
			if err != nil {
				return err
			}
			_, err = h.RefreshCatalogSource(ctx, apiv1.RefreshCatalogSourceParams{ID: src.ID})
			return err
		},
	},
	{
		op:   "RefreshCatalogSource",
		name: "valid_curated_catalog",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.RefreshCatalogSource(ctx, apiv1.RefreshCatalogSourceParams{ID: template.SourceCurated})
			return err
		},
	},
	{
		op:   "RefreshCatalogSource",
		name: "unknown_source_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.RefreshCatalogSource(ctx, apiv1.RefreshCatalogSourceParams{ID: "src-0123456789"})
			return err
		},
	},
	{
		op:   "RefreshCatalogSource",
		name: "path_traversal_id_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.RefreshCatalogSource(ctx, apiv1.RefreshCatalogSourceParams{ID: "../catalog"})
			return err
		},
	},
	{
		op:   "RemoveCatalogSource",
		name: "valid_user_added_source",
		run: func(ctx context.Context, h apiv1.Handler) error {
			src, err := h.AddCatalogSource(ctx, &apiv1.AddCatalogSourceRequest{URL: "https://catalog.example.com"})
			if err != nil {
				return err
			}
			return h.RemoveCatalogSource(ctx, apiv1.RemoveCatalogSourceParams{ID: src.ID})
		},
	},
	{
		op:   "RemoveCatalogSource",
		name: "the_curated_catalog_cannot_be_removed",
		run: func(ctx context.Context, h apiv1.Handler) error {
			return h.RemoveCatalogSource(ctx, apiv1.RemoveCatalogSourceParams{ID: template.SourceCurated})
		},
	},
	{
		op:   "RemoveCatalogSource",
		name: "unknown_source_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			return h.RemoveCatalogSource(ctx, apiv1.RemoveCatalogSourceParams{ID: "src-0123456789"})
		},
	},
	{
		op:   "RemoveCatalogSource",
		name: "second_removal_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			src, err := h.AddCatalogSource(ctx, &apiv1.AddCatalogSourceRequest{URL: "https://catalog.example.com"})
			if err != nil {
				return err
			}
			if err := h.RemoveCatalogSource(ctx, apiv1.RemoveCatalogSourceParams{ID: src.ID}); err != nil {
				return err
			}
			return h.RemoveCatalogSource(ctx, apiv1.RemoveCatalogSourceParams{ID: src.ID})
		},
	},
	{
		op:   "GetStackTemplateUpdate",
		name: "valid_stack_installed_from_a_template",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{}, apiv1.InstallTemplateParams{ID: "jellyfin"}); err != nil {
				return err
			}
			_, err := h.GetStackTemplateUpdate(ctx, apiv1.GetStackTemplateUpdateParams{Name: "jellyfin"})
			return err
		},
	},
	{
		op:   "GetStackTemplateUpdate",
		name: "valid_stack_with_no_template",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CreateStack(ctx, &apiv1.CreateStackRequest{Name: "by-hand", Compose: "services: {}\n"}); err != nil {
				return err
			}
			_, err := h.GetStackTemplateUpdate(ctx, apiv1.GetStackTemplateUpdateParams{Name: "by-hand"})
			return err
		},
	},
	{
		op:   "GetStackTemplateUpdate",
		name: "valid_stack_whose_source_is_gone",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.CreateStack(ctx, &apiv1.CreateStackRequest{
				Name:     "orphan",
				Compose:  "services: {}\n",
				Template: apiv1.NewOptStackTemplate(apiv1.StackTemplate{Source: "src-0123456789", ID: "jellyfin", Revision: "1"}),
			})
			if err != nil {
				return err
			}
			_, err = h.GetStackTemplateUpdate(ctx, apiv1.GetStackTemplateUpdateParams{Name: "orphan"})
			return err
		},
	},
	{
		op:   "GetStackTemplateUpdate",
		name: "unknown_stack_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetStackTemplateUpdate(ctx, apiv1.GetStackTemplateUpdateParams{Name: "nope"})
			return err
		},
	},
	{
		op:   "GetStackTemplateUpdate",
		name: "invalid_stack_name_is_rejected",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetStackTemplateUpdate(ctx, apiv1.GetStackTemplateUpdateParams{Name: "../etc"})
			return err
		},
	},
	{
		op:   "RemoveStack",
		name: "path_traversal_name_is_rejected",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.RemoveStack(ctx, apiv1.RemoveStackParams{Name: "../stacks", DeleteAppdata: apiv1.NewOptBool(true)})
			return err
		},
	},
	// --- Apps lifecycle (#277) ---
	{
		op:   "StartApp",
		name: "valid_by_name",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.StartApp(ctx, apiv1.StartAppParams{ID: "portainer"})
			return err
		},
	},
	{
		op:   "StartApp",
		name: "unknown_id_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.StartApp(ctx, apiv1.StartAppParams{ID: "no-such-container"})
			return err
		},
	},
	{
		op:   "StopApp",
		name: "valid_by_name",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.StopApp(ctx, apiv1.StopAppParams{ID: "jellyfin"})
			return err
		},
	},
	{
		op:   "StopApp",
		name: "unknown_id_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.StopApp(ctx, apiv1.StopAppParams{ID: "no-such-container"})
			return err
		},
	},
	{
		op:   "RestartApp",
		name: "valid_by_name",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.RestartApp(ctx, apiv1.RestartAppParams{ID: "jellyfin"})
			return err
		},
	},
	{
		op:   "RestartApp",
		name: "unknown_id_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.RestartApp(ctx, apiv1.RestartAppParams{ID: "no-such-container"})
			return err
		},
	},
	{
		op:   "RecreateApp",
		name: "valid_queues_a_job",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.RecreateApp(ctx, apiv1.RecreateAppParams{ID: "jellyfin"})
			return err
		},
	},
	{
		op:   "RecreateApp",
		name: "unknown_id_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.RecreateApp(ctx, apiv1.RecreateAppParams{ID: "no-such-container"})
			return err
		},
	},
	{
		op:   "RecreateApp",
		name: "refused_during_maintenance_mode",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
				return err
			}
			_, err := h.RecreateApp(ctx, apiv1.RecreateAppParams{ID: "jellyfin"})
			return err
		},
	},
	// #424: start, restart and recreate are refused with 409 array_stopped
	// while the array is stopped, no array is configured, or (degraded
	// scenario) its storage is not released; stop and a plain remove are not.
	{
		op:   "StartApp",
		name: "refused_while_the_array_is_stopped",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
				return err
			}
			_, err := h.StartApp(ctx, apiv1.StartAppParams{ID: "portainer"})
			return err
		},
	},
	{
		op:   "RestartApp",
		name: "refused_while_the_array_is_stopped",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
				return err
			}
			_, err := h.RestartApp(ctx, apiv1.RestartAppParams{ID: "jellyfin"})
			return err
		},
	},
	{
		op:   "StartApp",
		name: "refused_before_the_container_is_looked_up",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
				return err
			}
			_, err := h.StartApp(ctx, apiv1.StartAppParams{ID: "no-such-container"})
			return err
		},
	},
	{
		op:       "StartApp",
		name:     "refused_while_storage_is_not_ready",
		scenario: "degraded",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.StartApp(ctx, apiv1.StartAppParams{ID: "portainer"})
			return err
		},
	},
	{
		op:       "StartApp",
		name:     "refused_with_no_array_configured",
		scenario: "fresh-install",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.StartApp(ctx, apiv1.StartAppParams{ID: "portainer"})
			return err
		},
	},
	{
		op:       "StartApp",
		name:     "valid_once_the_degraded_array_is_acknowledged",
		scenario: "degraded",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.AcknowledgeDegradedArray(ctx); err != nil {
				return err
			}
			_, err := h.StartApp(ctx, apiv1.StartAppParams{ID: "portainer"})
			return err
		},
	},
	{
		op:   "StopApp",
		name: "allowed_while_the_array_is_stopped",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
				return err
			}
			_, err := h.StopApp(ctx, apiv1.StopAppParams{ID: "jellyfin"})
			return err
		},
	},
	{
		op:   "RemoveApp",
		name: "valid_stopped_container",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.RemoveApp(ctx, apiv1.RemoveAppParams{ID: "portainer"})
			return err
		},
	},
	// #428: deleting appdata is refused with 409 array_stopped while the
	// array is stopped or its storage is not ready — before the container
	// is looked up or removed — and a plain remove is still allowed.
	{
		op:   "RemoveApp",
		name: "delete_appdata_refused_while_the_array_is_stopped",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
				return err
			}
			_, err := h.RemoveApp(ctx, apiv1.RemoveAppParams{ID: "portainer", DeleteAppdata: apiv1.NewOptBool(true)})
			return err
		},
	},
	{
		op:   "RemoveApp",
		name: "delete_appdata_refused_before_the_container_is_looked_up",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
				return err
			}
			_, err := h.RemoveApp(ctx, apiv1.RemoveAppParams{ID: "no-such-container", DeleteAppdata: apiv1.NewOptBool(true)})
			return err
		},
	},
	{
		op:       "RemoveApp",
		name:     "delete_appdata_refused_while_storage_is_not_ready",
		scenario: "degraded",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.RemoveApp(ctx, apiv1.RemoveAppParams{ID: "portainer", DeleteAppdata: apiv1.NewOptBool(true)})
			return err
		},
	},
	{
		op:   "RemoveApp",
		name: "allowed_without_appdata_while_the_array_is_stopped",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
				return err
			}
			_, err := h.RemoveApp(ctx, apiv1.RemoveAppParams{ID: "portainer"})
			return err
		},
	},
	{
		// Production refuses appdata deletion with appdata_unavailable/409
		// when the array has no cache disk to hold appdata, and no mock
		// scenario has one — so the mock's appdata_shared refusal and
		// successful appdata deletion are reached only by the unit tests
		// in apps_test.go, which call removeApp directly, not by this rig.
		op:   "RemoveApp",
		name: "delete_appdata_without_a_cache_disk_is_refused",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.RemoveApp(ctx, apiv1.RemoveAppParams{ID: "portainer", DeleteAppdata: apiv1.NewOptBool(true)})
			return err
		},
	},
	{
		// Same refusal for a container whose appdata is shared: with no
		// cache disk, appdata_unavailable comes before appdata_shared.
		op:   "RemoveApp",
		name: "delete_appdata_of_a_shared_mount_without_a_cache_disk_is_refused",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.RemoveApp(ctx, apiv1.RemoveAppParams{ID: "transcoder", DeleteAppdata: apiv1.NewOptBool(true)})
			return err
		},
	},
	{
		op:       "RemoveApp",
		name:     "delete_appdata_without_an_array_is_refused",
		scenario: "fresh-install",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.RemoveApp(ctx, apiv1.RemoveAppParams{ID: "portainer", DeleteAppdata: apiv1.NewOptBool(true)})
			return err
		},
	},
	{
		op:   "RemoveApp",
		name: "running_container_is_refused",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.RemoveApp(ctx, apiv1.RemoveAppParams{ID: "jellyfin", DeleteAppdata: apiv1.NewOptBool(true)})
			return err
		},
	},
	{
		op:   "RemoveApp",
		name: "unknown_id_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.RemoveApp(ctx, apiv1.RemoveAppParams{ID: "no-such-container"})
			return err
		},
	},
	{
		op:   "GetAppLogs",
		name: "valid_request",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetAppLogs(ctx, apiv1.GetAppLogsParams{ID: "jellyfin"})
			return err
		},
	},
	{
		op:   "GetAppLogs",
		name: "unknown_id_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetAppLogs(ctx, apiv1.GetAppLogsParams{ID: "no-such-container"})
			return err
		},
	},
	{
		op:   "GetAppStats",
		name: "valid_running_container",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetAppStats(ctx, apiv1.GetAppStatsParams{ID: "jellyfin"})
			return err
		},
	},
	{
		op:   "GetAppStats",
		name: "stopped_container_is_refused",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetAppStats(ctx, apiv1.GetAppStatsParams{ID: "portainer"})
			return err
		},
	},
	{
		op:   "GetAppStats",
		name: "unknown_id_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetAppStats(ctx, apiv1.GetAppStatsParams{ID: "no-such-container"})
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
		// #385: the "degraded" scenario's own disk4 (mockArrayDisks,
		// empty identity, never in mockDiskInventory) leaves both
		// handlers' own storage gate not ready, so acknowledging succeeds
		// on both — the same not-ready state GetStatus's own "valid" case
		// above would report arrayDegraded for under this scenario.
		op:       "AcknowledgeDegradedArray",
		name:     "valid",
		scenario: "degraded",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.AcknowledgeDegradedArray(ctx)
			return err
		},
	},
	{
		// The default "healthy" scenario has every expected disk present
		// on both handlers, so there is nothing to acknowledge — refused
		// with array_not_degraded on both, never silently accepted.
		op:   "AcknowledgeDegradedArray",
		name: "refused_when_nothing_is_missing",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.AcknowledgeDegradedArray(ctx)
			return err
		},
	},
	{
		// #385 finding 5: a second acknowledge while the disk is still
		// missing must succeed on both handlers, the same as a real
		// disk.StorageGate.Acknowledge — which only refuses once the gate
		// itself is genuinely ready (last.Ready), never because it was
		// already acknowledged once. Neither this rig's own degradedGate
		// (contract_rig_test.go) nor the mock's disk4 is ever re-evaluated
		// as present here, so both stay degraded and both accept the
		// repeat call.
		op:       "AcknowledgeDegradedArray",
		name:     "valid_second_acknowledge_while_still_missing",
		scenario: "degraded",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.AcknowledgeDegradedArray(ctx); err != nil {
				return err
			}
			_, err := h.AcknowledgeDegradedArray(ctx)
			return err
		},
	},
	{
		// #385 finding 1: the mock previously accepted an acknowledge
		// during maintenance mode (200) while production refused it
		// (409 array_services_not_started, storageTargetSync.
		// UpdateOrError's own maintenance-mode check) — this rig's own
		// AcknowledgeDegraded hook (contract_rig_test.go) now carries the
		// same check, so this case fails again the moment either side
		// stops refusing it.
		op:       "AcknowledgeDegradedArray",
		name:     "refused_when_array_is_in_maintenance",
		scenario: "degraded",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
				return err
			}
			_, err := h.AcknowledgeDegradedArray(ctx)
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
		// The flag creates the disk's "external:<label>" destination
		// (doc 10 §1); another destination already named for the disk
		// refuses it with backup_destination_exists/409 on both sides.
		op:   "UpdateExternalDisk",
		name: "flag_clashing_with_a_destination_name",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.RegisterExternalDisk(ctx, &apiv1.RegisterExternalDiskRequest{Device: "/dev/sdf", Label: "backup2"}); err != nil {
				return err
			}
			if _, err := h.CreateBackupDestination(ctx, &apiv1.CreateBackupDestinationRequest{Name: "Backup2", Type: apiv1.BackupDestinationTypeLocal, Path: "/srv/elsewhere"}); err != nil {
				return err
			}
			_, err := h.UpdateExternalDisk(ctx, &apiv1.UpdateExternalDiskRequest{BackupDestination: apiv1.NewOptBool(true)}, apiv1.UpdateExternalDiskParams{Label: "backup2"})
			return err
		},
	},
	{
		op:   "RegisterExternalDisk",
		name: "flag_clashing_with_a_destination_name",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.CreateBackupDestination(ctx, &apiv1.CreateBackupDestinationRequest{Name: "Backup2", Type: apiv1.BackupDestinationTypeLocal, Path: "/srv/elsewhere"}); err != nil {
				return err
			}
			_, err := h.RegisterExternalDisk(ctx, &apiv1.RegisterExternalDiskRequest{Device: "/dev/sdf", Label: "backup2", BackupDestination: apiv1.NewOptBool(true)})
			return err
		},
	},
	{
		// Deleting a disk's destination clears its flag; the destination
		// is then gone on both sides, so a second delete is a 404.
		op:   "DeleteBackupDestination",
		name: "external_destination_twice",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.RegisterExternalDisk(ctx, &apiv1.RegisterExternalDiskRequest{Device: "/dev/sdf", Label: "backup2", BackupDestination: apiv1.NewOptBool(true)}); err != nil {
				return err
			}
			if err := h.DeleteBackupDestination(ctx, apiv1.DeleteBackupDestinationParams{DestinationId: "external:backup2"}); err != nil {
				return err
			}
			return h.DeleteBackupDestination(ctx, apiv1.DeleteBackupDestinationParams{DestinationId: "external:backup2"})
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
		// that check against mockHostConfigKinds (pool.go) instead
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

	// --- Config export/import (#272, #269: ImportConfig's own confirm
	// check runs before touching h.Backup, so missing_confirm needs no
	// backup.Service wiring at all; invalid_archive below does, and the
	// rig now carries one — see contract_test.go's contractSkip for why
	// ExportConfig itself is not here) ---
	{
		op:   "ImportConfig",
		name: "missing_confirm",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ImportConfig(ctx, &apiv1.ImportConfigReq{Confirm: false})
			return err
		},
	},
	{
		op:   "ImportConfig",
		name: "invalid_archive",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ImportConfig(ctx, &apiv1.ImportConfigReq{
				Confirm: true,
				Archive: ht.MultipartFile{File: bytes.NewReader([]byte("not a tar.zst archive"))},
			})
			return err
		},
	},
	{
		op:   "ImportConfig",
		name: "invalid_archive_with_passphrase",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ImportConfig(ctx, &apiv1.ImportConfigReq{
				Confirm:    true,
				Archive:    ht.MultipartFile{File: bytes.NewReader([]byte("not a tar.zst archive"))},
				Passphrase: apiv1.NewOptString("a passphrase"),
			})
			return err
		},
	},

	// The diskMapping field is read before the upload is, so a value that is
	// not a mapping is refused the same way on both sides whatever the
	// installation holds. The cases below it use a real archive from another
	// installation (buildMockBareMetalArchive), built inside the case.
	{
		op:   "ImportConfig",
		name: "invalid_disk_mapping",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ImportConfig(ctx, &apiv1.ImportConfigReq{
				Confirm:     true,
				Archive:     ht.MultipartFile{File: bytes.NewReader([]byte("not a tar.zst archive"))},
				DiskMapping: apiv1.NewOptString("data 1 is on /dev/sdb"),
			})
			return err
		},
	},
	{
		op:   "ImportConfig",
		name: "invalid_disk_mapping_role",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ImportConfig(ctx, &apiv1.ImportConfigReq{
				Confirm:     true,
				Archive:     ht.MultipartFile{File: bytes.NewReader([]byte("not a tar.zst archive"))},
				DiskMapping: apiv1.NewOptString(`{"disks":[{"role":"spare","roleIndex":1,"device":"/dev/sdb"}]}`),
			})
			return err
		},
	},
	{
		op:   "ImportConfig",
		name: "missing_confirm_with_a_disk_mapping",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ImportConfig(ctx, &apiv1.ImportConfigReq{Confirm: false, DiskMapping: apiv1.NewOptString(`{"disks":[]}`)})
			return err
		},
	},
	{
		op:       "ImportConfig",
		name:     "valid_bare_metal_restore",
		scenario: "fresh-install",
		run: func(ctx context.Context, h apiv1.Handler) error {
			return contractImportBareMetal(ctx, h, contractMatchedMapping)
		},
	},
	{
		op:       "ImportConfig",
		name:     "bare_metal_without_a_disk_mapping",
		scenario: "fresh-install",
		run: func(ctx context.Context, h apiv1.Handler) error {
			return contractImportBareMetal(ctx, h, "")
		},
	},
	{
		op:       "ImportConfig",
		name:     "bare_metal_with_a_stale_disk_mapping",
		scenario: "fresh-install",
		run: func(ctx context.Context, h apiv1.Handler) error {
			return contractImportBareMetal(ctx, h, `{"disks":[{"role":"data","roleIndex":1,"device":"/dev/sdc"}]}`)
		},
	},
	{
		op:       "ImportConfig",
		name:     "bare_metal_with_an_empty_disk_mapping",
		scenario: "fresh-install",
		run: func(ctx context.Context, h apiv1.Handler) error {
			return contractImportBareMetal(ctx, h, `{"disks":[]}`)
		},
	},
	{
		op:       "ImportConfig",
		name:     "bare_metal_archive_from_a_newer_schema",
		scenario: "fresh-install",
		run: func(ctx context.Context, h apiv1.Handler) error {
			return contractImportBareMetal(ctx, h, contractMatchedMapping, contractNewerSchema)
		},
	},
	{
		op:       "ImportConfig",
		name:     "bare_metal_with_the_archives_passphrase",
		scenario: "fresh-install",
		run: func(ctx context.Context, h apiv1.Handler) error {
			report, err := contractImportSealed(ctx, h, contractArchivePassphrase)
			if err != nil {
				return err
			}
			for _, n := range report.NotRestored {
				if n.Kind == apiv1.ConfigImportNotRestoredKindBackupRecipient || (n.Kind == apiv1.ConfigImportNotRestoredKindDatabaseSecret && strings.HasPrefix(n.Name, "notify_channels")) {
					return fmt.Errorf("the report lists %s %s as not restored although the passphrase opened the archive", n.Kind, n.Name)
				}
			}
			return nil
		},
	},
	{
		op:       "ImportConfig",
		name:     "bare_metal_with_a_wrong_passphrase",
		scenario: "fresh-install",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := contractImportSealed(ctx, h, "not the archive's passphrase")
			return err
		},
	},
	{
		op:       "ImportConfig",
		name:     "bare_metal_without_a_passphrase_keeps_the_recipient",
		scenario: "fresh-install",
		run: func(ctx context.Context, h apiv1.Handler) error {
			report, err := contractImportSealed(ctx, h, "")
			if err != nil {
				return err
			}
			for _, n := range report.NotRestored {
				if n.Kind == apiv1.ConfigImportNotRestoredKindBackupRecipient && n.Reason == apiv1.ConfigImportNotRestoredReasonNoPassphrase {
					return nil
				}
			}
			return fmt.Errorf("the report %+v does not say this installation kept its own backup recipient", report.NotRestored)
		},
	},
	{
		op:       "PreviewConfigImport",
		name:     "bare_metal_preview_reports_whether_the_passphrase_opens_the_identity",
		scenario: "fresh-install",
		run: func(ctx context.Context, h apiv1.Handler) error {
			archive, err := buildMockSealedArchive()
			if err != nil {
				return err
			}
			for pass, want := range map[string]apiv1.ConfigImportSecretsStatus{
				contractArchivePassphrase: apiv1.ConfigImportSecretsStatusOpened,
				"":                        apiv1.ConfigImportSecretsStatusNoPassphrase,
			} {
				req := &apiv1.PreviewConfigImportReq{Archive: ht.MultipartFile{File: bytes.NewReader(archive)}}
				if pass != "" {
					req.Passphrase = apiv1.NewOptString(pass)
				}
				p, err := h.PreviewConfigImport(ctx, req)
				if err != nil {
					return err
				}
				if p.Secrets.Status != want || p.Secrets.Identity.Or("") != want {
					return fmt.Errorf("with passphrase %q the preview reports secrets %s and identity %s, want both %s", pass, p.Secrets.Status, p.Secrets.Identity.Or(""), want)
				}
			}
			return nil
		},
	},
	{
		op:       "ImportConfig",
		name:     "disk_mapping_on_an_installation_with_an_array",
		scenario: "healthy",
		run: func(ctx context.Context, h apiv1.Handler) error {
			return contractImportBareMetal(ctx, h, `{"disks":[]}`)
		},
	},
	{
		op:       "PreviewConfigImport",
		name:     "valid_bare_metal_preview_shows_the_disk_mapping",
		scenario: "fresh-install",
		run: func(ctx context.Context, h apiv1.Handler) error {
			p, err := contractPreviewBareMetal(ctx, h)
			if err != nil {
				return err
			}
			bm, ok := p.BareMetal.Get()
			if !ok {
				return errors.New("the preview has no bareMetal block")
			}
			states := map[apiv1.ConfigImportDiskState]string{}
			for _, d := range bm.Disks {
				states[d.State] = d.Device.Or("")
			}
			if len(bm.Disks) != 2 || len(p.Blockers) != 0 ||
				states[apiv1.ConfigImportDiskStateMatched] != "/dev/sdb" ||
				states[apiv1.ConfigImportDiskStateAbsent] != "" {
				return fmt.Errorf("bareMetal disks = %+v, blockers = %+v, want data 1 matched on /dev/sdb, data 2 absent and no blocker", bm.Disks, p.Blockers)
			}
			if len(bm.DiskMapping.Disks) != 1 || bm.DiskMapping.Disks[0].Device != "/dev/sdb" || bm.DiskMapping.Disks[0].RoleIndex != 1 {
				return fmt.Errorf("the mapping to confirm = %+v, want only data 1 on /dev/sdb", bm.DiskMapping)
			}
			return nil
		},
	},
	{
		op:       "PreviewConfigImport",
		name:     "bare_metal_preview_of_a_newer_schema_is_a_blocker",
		scenario: "fresh-install",
		run: func(ctx context.Context, h apiv1.Handler) error {
			p, err := contractPreviewBareMetal(ctx, h, contractNewerSchema)
			if err != nil {
				return err
			}
			if p.BareMetal.Set || len(p.Blockers) != 1 || p.Blockers[0].Code != apiv1.ConfigImportBlockerCodeArchiveNewerVersion {
				return fmt.Errorf("bareMetal set = %v, blockers = %+v, want no bareMetal block and one archive_newer_version blocker", p.BareMetal.Set, p.Blockers)
			}
			return nil
		},
	},
	{
		op:   "PreviewConfigImport",
		name: "invalid_archive",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.PreviewConfigImport(ctx, &apiv1.PreviewConfigImportReq{
				Archive: ht.MultipartFile{File: bytes.NewReader([]byte("not a tar.zst archive"))},
			})
			return err
		},
	},

	// --- Backup destinations (#60): both sides run backup.PrepareDestination,
	// so these pin the status and error code each returns for the same
	// request, not the shared validation itself ---
	{
		op:   "ListBackupDestinations",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ListBackupDestinations(ctx)
			return err
		},
	},
	{
		op:   "CreateBackupDestination",
		name: "valid_local",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.CreateBackupDestination(ctx, &apiv1.CreateBackupDestinationRequest{
				Name: "External disk", Type: apiv1.BackupDestinationTypeLocal, Path: "/mnt/disks/backup",
			})
			return err
		},
	},
	{
		op:   "CreateBackupDestination",
		name: "relative_path",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.CreateBackupDestination(ctx, &apiv1.CreateBackupDestinationRequest{
				Name: "Relative", Type: apiv1.BackupDestinationTypeLocal, Path: "backups",
			})
			return err
		},
	},
	{
		op:   "CreateBackupDestination",
		name: "system_path",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.CreateBackupDestination(ctx, &apiv1.CreateBackupDestinationRequest{
				Name: "System", Type: apiv1.BackupDestinationTypeLocal, Path: "/etc",
			})
			return err
		},
	},
	{
		op:   "CreateBackupDestination",
		name: "state_dir_path",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.CreateBackupDestination(ctx, &apiv1.CreateBackupDestinationRequest{
				Name: "State", Type: apiv1.BackupDestinationTypeLocal, Path: "/var/lib/hoserva/stacks",
			})
			return err
		},
	},
	{
		op:   "CreateBackupDestination",
		name: "duplicate_name",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.CreateBackupDestination(ctx, &apiv1.CreateBackupDestinationRequest{
				Name: "Boot device", Type: apiv1.BackupDestinationTypeLocal, Path: "/mnt/disks/other",
			})
			return err
		},
	},
	{
		op:   "CreateBackupDestination",
		name: "remote_without_passphrase",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.CreateBackupDestination(ctx, &apiv1.CreateBackupDestinationRequest{
				Name: "Bucket", Type: apiv1.BackupDestinationTypeS3, Path: "bucket/hoserva",
				Options: apiv1.OptCreateBackupDestinationRequestOptions{Set: true, Value: apiv1.CreateBackupDestinationRequestOptions{"access_key_id": "AKIA"}},
				Secrets: apiv1.OptCreateBackupDestinationRequestSecrets{Set: true, Value: apiv1.CreateBackupDestinationRequestSecrets{"secret_access_key": "s3cr3t"}},
			})
			return err
		},
	},
	{
		op:   "CreateBackupDestination",
		name: "remote_unencrypted",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.CreateBackupDestination(ctx, &apiv1.CreateBackupDestinationRequest{
				Name: "Bucket", Type: apiv1.BackupDestinationTypeS3, Path: "bucket/hoserva",
				Encrypt: apiv1.NewOptBool(false),
			})
			return err
		},
	},
	{
		op:   "DeleteBackupDestination",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			return h.DeleteBackupDestination(ctx, apiv1.DeleteBackupDestinationParams{DestinationId: "boot"})
		},
	},
	{
		op:   "DeleteBackupDestination",
		name: "unknown_id",
		run: func(ctx context.Context, h apiv1.Handler) error {
			return h.DeleteBackupDestination(ctx, apiv1.DeleteBackupDestinationParams{DestinationId: "nope"})
		},
	},
	{
		op:   "UpdateBackupDestination",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateBackupDestination(ctx, &apiv1.UpdateBackupDestinationRequest{
				Enabled:   apiv1.NewOptBool(false),
				Retention: apiv1.NewOptBackupRetention(apiv1.BackupRetention{Daily: 3, Weekly: 0, Monthly: 1}),
			}, apiv1.UpdateBackupDestinationParams{DestinationId: "boot"})
			return err
		},
	},
	{
		op:   "UpdateBackupDestination",
		name: "unknown_id",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateBackupDestination(ctx, &apiv1.UpdateBackupDestinationRequest{Enabled: apiv1.NewOptBool(false)}, apiv1.UpdateBackupDestinationParams{DestinationId: "nope"})
			return err
		},
	},
	{
		op:   "UpdateBackupDestination",
		name: "retention_keeps_nothing",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateBackupDestination(ctx, &apiv1.UpdateBackupDestinationRequest{
				Retention: apiv1.NewOptBackupRetention(apiv1.BackupRetention{}),
			}, apiv1.UpdateBackupDestinationParams{DestinationId: "boot"})
			return err
		},
	},
	{
		op:   "UpdateBackupDestination",
		name: "retention_over_the_bound",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateBackupDestination(ctx, &apiv1.UpdateBackupDestinationRequest{
				Retention: apiv1.NewOptBackupRetention(apiv1.BackupRetention{Daily: 1001}),
			}, apiv1.UpdateBackupDestinationParams{DestinationId: "boot"})
			return err
		},
	},
	{
		op:   "UpdateBackupDestination",
		name: "unknown_id_with_bad_retention",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.UpdateBackupDestination(ctx, &apiv1.UpdateBackupDestinationRequest{
				Retention: apiv1.NewOptBackupRetention(apiv1.BackupRetention{}),
			}, apiv1.UpdateBackupDestinationParams{DestinationId: "nope"})
			return err
		},
	},
	{
		op:   "TestBackupDestination",
		name: "valid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.TestBackupDestination(ctx, apiv1.TestBackupDestinationParams{DestinationId: "boot"})
			return err
		},
	},
	{
		op:   "TestBackupDestination",
		name: "unknown_id",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.TestBackupDestination(ctx, apiv1.TestBackupDestinationParams{DestinationId: "nope"})
			return err
		},
	},

	// --- Restore drill (#63): the read of the last result and the
	// submission; what a drill then does is production's own tests. ---
	{
		op:   "GetRestoreDrill",
		name: "valid_reports_the_last_result",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetRestoreDrill(ctx)
			return err
		},
	},
	{
		op:   "StartRestoreDrill",
		name: "valid_queues_the_drill",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.StartRestoreDrill(ctx)
			return err
		},
	},
	{
		op:   "StartRestoreDrill",
		name: "refused_in_maintenance_mode",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
				return err
			}
			_, err := h.StartRestoreDrill(ctx)
			return err
		},
	},

	// --- Config backup on demand (#450): the refusals and the job
	// submission; what the job then writes is production's own tests. ---
	{
		op:   "RunConfigBackup",
		name: "valid_queues_the_backup",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.RunConfigBackup(ctx)
			return err
		},
	},
	{
		op:   "RunConfigBackup",
		name: "refused_with_no_enabled_destination",
		run: func(ctx context.Context, h apiv1.Handler) error {
			for _, id := range []string{"boot", "pool"} {
				if _, err := h.UpdateBackupDestination(ctx, &apiv1.UpdateBackupDestinationRequest{Enabled: apiv1.NewOptBool(false)}, apiv1.UpdateBackupDestinationParams{DestinationId: id}); err != nil {
					return err
				}
			}
			_, err := h.RunConfigBackup(ctx)
			return err
		},
	},
	{
		op:   "RunConfigBackup",
		name: "refused_in_maintenance_mode",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
				return err
			}
			_, err := h.RunConfigBackup(ctx)
			return err
		},
	},

	// --- Appdata backup (#61): the scope, the refusals and the job
	// submission; what a job then does is production's own tests. ---
	{
		op:   "GetAppdataBackup",
		name: "valid_lists_the_containers_in_scope",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetAppdataBackup(ctx)
			return err
		},
	},
	{
		op:   "SetAppdataBackupContainer",
		name: "valid_opting_a_database_out_of_being_stopped",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.SetAppdataBackupContainer(ctx, &apiv1.SetAppdataBackupContainerRequest{Stop: false, Included: true},
				apiv1.SetAppdataBackupContainerParams{Name: "postgres"})
			return err
		},
	},
	{
		op:   "SetAppdataBackupContainer",
		name: "container_with_no_appdata_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.SetAppdataBackupContainer(ctx, &apiv1.SetAppdataBackupContainerRequest{Stop: true, Included: true},
				apiv1.SetAppdataBackupContainerParams{Name: "portainer"})
			return err
		},
	},
	{
		op:   "SetAppdataBackupContainer",
		name: "unknown_container_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.SetAppdataBackupContainer(ctx, &apiv1.SetAppdataBackupContainerRequest{Stop: true, Included: true},
				apiv1.SetAppdataBackupContainerParams{Name: "no-such-container"})
			return err
		},
	},
	{
		op:   "StartAppdataBackup",
		name: "valid_every_included_container",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.StartAppdataBackup(ctx, apiv1.OptStartAppdataBackupRequest{})
			return err
		},
	},
	{
		op:   "StartAppdataBackup",
		name: "valid_named_containers",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.StartAppdataBackup(ctx, apiv1.NewOptStartAppdataBackupRequest(apiv1.StartAppdataBackupRequest{Containers: []string{"jellyfin"}}))
			return err
		},
	},
	{
		op:   "StartAppdataBackup",
		name: "container_with_no_appdata_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.StartAppdataBackup(ctx, apiv1.NewOptStartAppdataBackupRequest(apiv1.StartAppdataBackupRequest{Containers: []string{"portainer"}}))
			return err
		},
	},
	{
		op:   "StartAppdataBackup",
		name: "refused_while_the_array_is_stopped",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
				return err
			}
			_, err := h.StartAppdataBackup(ctx, apiv1.OptStartAppdataBackupRequest{})
			return err
		},
	},
	{
		op:       "StartAppdataBackup",
		name:     "refused_while_storage_is_not_ready",
		scenario: "degraded",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.StartAppdataBackup(ctx, apiv1.OptStartAppdataBackupRequest{})
			return err
		},
	},
	{
		op:   "ListAppdataArchives",
		name: "valid_lists_the_archives_of_a_container",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.ListAppdataArchives(ctx, apiv1.ListAppdataArchivesParams{Container: apiv1.NewOptString("jellyfin")})
			return err
		},
	},
	{
		// The production side writes its archive by running a backup
		// first; the mock reports fixed ones.
		op:   "RestoreAppdata",
		name: "valid_restore_of_an_archive_on_a_destination",
		run: func(ctx context.Context, h apiv1.Handler) error {
			a, err := contractOrdinaryArchive(ctx, h, true)
			if err != nil {
				return err
			}
			_, err = h.RestoreAppdata(ctx, &apiv1.RestoreAppdataRequest{Container: "jellyfin", Archive: a.Name, DestinationId: a.DestinationId, Confirm: true})
			return err
		},
	},
	{
		op:   "RestoreAppdata",
		name: "confirm_is_required",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.RestoreAppdata(ctx, &apiv1.RestoreAppdataRequest{Container: "jellyfin", Archive: "x.tar.zst", DestinationId: "pool"})
			return err
		},
	},
	{
		op:   "RestoreAppdata",
		name: "refused_while_the_array_is_stopped",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
				return err
			}
			_, err := h.RestoreAppdata(ctx, &apiv1.RestoreAppdataRequest{Container: "jellyfin", Archive: "x.tar.zst", DestinationId: "pool", Confirm: true})
			return err
		},
	},
	{
		op:   "RestoreAppdata",
		name: "an_archive_name_that_is_not_one_is_invalid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.RestoreAppdata(ctx, &apiv1.RestoreAppdataRequest{Container: "jellyfin", Archive: "../../etc/passwd", DestinationId: "pool", Confirm: true})
			return err
		},
	},
	{
		op:   "RestoreAppdata",
		name: "another_installations_archive_is_invalid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.RestoreAppdata(ctx, &apiv1.RestoreAppdataRequest{
				Container: "jellyfin", Archive: "hoserva-appdata-ffffffffffff-jellyfin-2026-09-01T04-00-00.tar.zst", DestinationId: "pool", Confirm: true,
			})
			return err
		},
	},
	{
		op:   "RestoreAppdata",
		name: "an_archive_of_another_container_is_invalid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			a, err := contractOrdinaryArchive(ctx, h, true)
			if err != nil {
				return err
			}
			_, err = h.RestoreAppdata(ctx, &apiv1.RestoreAppdataRequest{Container: "postgres", Archive: a.Name, DestinationId: a.DestinationId, Confirm: true})
			return err
		},
	},
	{
		op:   "RestoreAppdata",
		name: "unknown_destination_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			a, err := contractOrdinaryArchive(ctx, h, true)
			if err != nil {
				return err
			}
			_, err = h.RestoreAppdata(ctx, &apiv1.RestoreAppdataRequest{Container: "jellyfin", Archive: a.Name, DestinationId: "nope", Confirm: true})
			return err
		},
	},
	{
		op:   "RestoreAppdata",
		name: "an_archive_the_destination_does_not_hold_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			a, err := contractOrdinaryArchive(ctx, h, true)
			if err != nil {
				return err
			}
			missing := strings.Replace(a.Name, ".tar.zst", "-9.tar.zst", 1)
			_, err = h.RestoreAppdata(ctx, &apiv1.RestoreAppdataRequest{Container: "jellyfin", Archive: missing, DestinationId: a.DestinationId, Confirm: true})
			return err
		},
	},
	{
		op:   "PreviewAppdataRestore",
		name: "valid_preview_of_an_archive_on_a_destination",
		run: func(ctx context.Context, h apiv1.Handler) error {
			a, err := contractOrdinaryArchive(ctx, h, true)
			if err != nil {
				return err
			}
			_, err = h.PreviewAppdataRestore(ctx, &apiv1.PreviewAppdataRestoreRequest{Container: "jellyfin", Archive: a.Name, DestinationId: a.DestinationId})
			return err
		},
	},
	{
		op:   "PreviewAppdataRestore",
		name: "refused_while_the_array_is_stopped",
		run: func(ctx context.Context, h apiv1.Handler) error {
			if _, err := h.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
				return err
			}
			_, err := h.PreviewAppdataRestore(ctx, &apiv1.PreviewAppdataRestoreRequest{Container: "jellyfin", Archive: "x.tar.zst", DestinationId: "pool"})
			return err
		},
	},
	{
		op:   "PreviewAppdataRestore",
		name: "an_archive_name_that_is_not_one_is_invalid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.PreviewAppdataRestore(ctx, &apiv1.PreviewAppdataRestoreRequest{Container: "jellyfin", Archive: "../../etc/passwd", DestinationId: "pool"})
			return err
		},
	},
	{
		op:   "PreviewAppdataRestore",
		name: "another_installations_archive_is_invalid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.PreviewAppdataRestore(ctx, &apiv1.PreviewAppdataRestoreRequest{
				Container: "jellyfin", Archive: "hoserva-appdata-ffffffffffff-jellyfin-2026-09-01T04-00-00.tar.zst", DestinationId: "pool",
			})
			return err
		},
	},
	{
		op:   "PreviewAppdataRestore",
		name: "an_archive_of_another_container_is_invalid",
		run: func(ctx context.Context, h apiv1.Handler) error {
			a, err := contractOrdinaryArchive(ctx, h, true)
			if err != nil {
				return err
			}
			_, err = h.PreviewAppdataRestore(ctx, &apiv1.PreviewAppdataRestoreRequest{Container: "postgres", Archive: a.Name, DestinationId: a.DestinationId})
			return err
		},
	},
	{
		op:   "PreviewAppdataRestore",
		name: "unknown_destination_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			a, err := contractOrdinaryArchive(ctx, h, true)
			if err != nil {
				return err
			}
			_, err = h.PreviewAppdataRestore(ctx, &apiv1.PreviewAppdataRestoreRequest{Container: "jellyfin", Archive: a.Name, DestinationId: "nope"})
			return err
		},
	},
	{
		op:   "PreviewAppdataRestore",
		name: "an_archive_the_destination_does_not_hold_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			a, err := contractOrdinaryArchive(ctx, h, true)
			if err != nil {
				return err
			}
			missing := strings.Replace(a.Name, ".tar.zst", "-9.tar.zst", 1)
			_, err = h.PreviewAppdataRestore(ctx, &apiv1.PreviewAppdataRestoreRequest{Container: "jellyfin", Archive: missing, DestinationId: a.DestinationId})
			return err
		},
	},
	{
		op:   "GetAppdataRestorePreview",
		name: "valid_result_of_a_finished_preview",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := contractFinishedPreview(ctx, h)
			return err
		},
	},
	{
		op:   "GetAppdataRestorePreview",
		name: "an_unknown_job_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			_, err := h.GetAppdataRestorePreview(ctx, apiv1.GetAppdataRestorePreviewParams{JobId: uuid.New()})
			return err
		},
	},
	{
		op:   "GetAppdataRestorePreview",
		name: "a_job_that_is_not_a_preview_is_not_found",
		run: func(ctx context.Context, h apiv1.Handler) error {
			j, err := h.StartAppdataBackup(ctx, apiv1.NewOptStartAppdataBackupRequest(apiv1.StartAppdataBackupRequest{Containers: []string{"jellyfin"}}))
			if err != nil {
				return err
			}
			_, err = h.GetAppdataRestorePreview(ctx, apiv1.GetAppdataRestorePreviewParams{JobId: j.ID})
			return err
		},
	},
}

// contractFinishedPreview previews restoring an archive of jellyfin and
// reads the result once the job has finished: production runs the job, the
// mock records it as already succeeded.
func contractFinishedPreview(ctx context.Context, h apiv1.Handler) (*apiv1.AppdataRestorePreview, error) {
	a, err := contractOrdinaryArchive(ctx, h, true)
	if err != nil {
		return nil, err
	}
	j, err := h.PreviewAppdataRestore(ctx, &apiv1.PreviewAppdataRestoreRequest{Container: "jellyfin", Archive: a.Name, DestinationId: a.DestinationId})
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		p, err := h.GetAppdataRestorePreview(ctx, apiv1.GetAppdataRestorePreviewParams{JobId: j.ID})
		if err == nil {
			return p, nil
		}
		if _, code := contractOutcome(h, err); code != "appdata_preview_not_ready" || time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// contractOrdinaryArchive returns an ordinary (not pre-restore) archive of
// jellyfin. With backup true it first starts a backup and waits for its
// archive to appear: production's destination starts empty, the mock's
// already lists fixed ones.
func contractOrdinaryArchive(ctx context.Context, h apiv1.Handler, backup bool) (apiv1.AppdataArchive, error) {
	if backup {
		if _, err := h.StartAppdataBackup(ctx, apiv1.NewOptStartAppdataBackupRequest(apiv1.StartAppdataBackupRequest{Containers: []string{"jellyfin"}})); err != nil {
			return apiv1.AppdataArchive{}, err
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		list, err := h.ListAppdataArchives(ctx, apiv1.ListAppdataArchivesParams{Container: apiv1.NewOptString("jellyfin")})
		if err != nil {
			return apiv1.AppdataArchive{}, err
		}
		for _, a := range list.Archives {
			if !a.Reason.IsSet() || a.Reason.IsNull() {
				return a, nil
			}
		}
		if time.Now().After(deadline) {
			return apiv1.AppdataArchive{}, fmt.Errorf("no archive of jellyfin appeared")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// contractMatchedMapping is the mapping a user confirms against the
// fresh-install scenario: the archive's first data disk on the scenario's
// first disk.
const contractMatchedMapping = `{"disks":[{"role":"data","roleIndex":1,"device":"/dev/sdb"}]}`

// contractNewerSchema marks an archive's database as written by a Hoserva
// newer than any this build knows.
const contractNewerSchema = `INSERT INTO schema_migrations (version, slug, checksum, applied_at) VALUES ('99999999999999', 'from_the_future', 'x', '2099-01-01T00:00:00Z')`

func contractImportBareMetal(ctx context.Context, h apiv1.Handler, mapping string, extra ...string) error {
	archive, err := buildMockBareMetalArchive(extra...)
	if err != nil {
		return fmt.Errorf("building the archive: %w", err)
	}
	req := &apiv1.ImportConfigReq{Confirm: true, Archive: ht.MultipartFile{File: bytes.NewReader(archive)}}
	if mapping != "" {
		req.DiskMapping = apiv1.NewOptString(mapping)
	}
	_, err = h.ImportConfig(ctx, req)
	return err
}

func contractImportSealed(ctx context.Context, h apiv1.Handler, passphrase string) (*apiv1.ConfigImportReport, error) {
	archive, err := buildMockSealedArchive()
	if err != nil {
		return nil, fmt.Errorf("building the archive: %w", err)
	}
	req := &apiv1.ImportConfigReq{Confirm: true, Archive: ht.MultipartFile{File: bytes.NewReader(archive)}, DiskMapping: apiv1.NewOptString(contractMatchedMapping)}
	if passphrase != "" {
		req.Passphrase = apiv1.NewOptString(passphrase)
	}
	return h.ImportConfig(ctx, req)
}

func contractPreviewBareMetal(ctx context.Context, h apiv1.Handler, extra ...string) (*apiv1.ConfigImportPreview, error) {
	archive, err := buildMockBareMetalArchive(extra...)
	if err != nil {
		return nil, fmt.Errorf("building the archive: %w", err)
	}
	return h.PreviewConfigImport(ctx, &apiv1.PreviewConfigImportReq{Archive: ht.MultipartFile{File: bytes.NewReader(archive)}})
}
