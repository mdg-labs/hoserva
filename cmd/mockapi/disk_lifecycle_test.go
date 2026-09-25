package main

import (
	"context"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

// TestFinishDiskRemoval_MirrorsProductionRefusals proves the mock refuses
// finishDiskRemoval in production's own order (internal/api's
// FinishDiskRemoval): not_configured with no array, disk_slot_not_found,
// then disk_not_evacuated ahead of any confirmation check.
func TestFinishDiskRemoval_MirrorsProductionRefusals(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		scenario   string
		mountpoint string
		confirm    string
		code       string
	}{
		{scenario: "fresh-install", mountpoint: "/mnt/disk1", confirm: "REMOVE /mnt/disk1", code: "not_configured"},
		{scenario: "healthy", mountpoint: "/mnt/disk9", confirm: "REMOVE /mnt/disk9", code: "disk_slot_not_found"},
		{scenario: "healthy", mountpoint: "/mnt/parity", confirm: "REMOVE /mnt/parity", code: "disk_slot_not_found"},
		{scenario: "healthy", mountpoint: "/mnt/disk1", confirm: "wrong", code: "disk_not_evacuated"},
		{scenario: "degraded", mountpoint: "/mnt/disk3", confirm: "REMOVE /mnt/disk3", code: "disk_not_evacuated"},
	} {
		client := newTestClient(t, tc.scenario)
		_, err := client.FinishDiskRemoval(ctx, &apiv1.FinishDiskRemovalRequest{Mountpoint: tc.mountpoint, Confirmation: tc.confirm})
		if got := errorCode(t, err); got != tc.code {
			t.Fatalf("%s: FinishDiskRemoval(%s) = %s, want %s", tc.scenario, tc.mountpoint, got, tc.code)
		}
	}
}

// TestReplaceAndUpgrade_RefuseADiskInRemoval proves the mock mirrors
// production's disk_leaving_array refusal (#366) for the degraded
// scenario's evacuating disk3, ahead of every later check, while the
// same requests against disk1 get past it.
func TestReplaceAndUpgrade_RefuseADiskInRemoval(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t, "degraded")

	_, err := client.PlanDiskReplace(ctx, &apiv1.ReplaceDiskPlanRequest{Mountpoint: "/mnt/disk3", Device: "/dev/sdf"})
	if got := errorCode(t, err); got != "disk_leaving_array" {
		t.Fatalf("PlanDiskReplace(/mnt/disk3) = %s, want disk_leaving_array", got)
	}
	_, err = client.ReplaceDisk(ctx, &apiv1.ReplaceDiskRequest{Mountpoint: "/mnt/disk3", Device: "/dev/sdf", Confirmation: "ERASE /dev/sdf"})
	if got := errorCode(t, err); got != "disk_leaving_array" {
		t.Fatalf("ReplaceDisk(/mnt/disk3) = %s, want disk_leaving_array", got)
	}
	_, err = client.PlanDiskUpgrade(ctx, &apiv1.DiskUpgradePlanRequest{Mountpoint: "/mnt/disk3", Device: "/dev/sdf"})
	if got := errorCode(t, err); got != "disk_leaving_array" {
		t.Fatalf("PlanDiskUpgrade(/mnt/disk3) = %s, want disk_leaving_array", got)
	}
	_, err = client.UpgradeDisk(ctx, &apiv1.UpgradeDiskRequest{Mountpoint: "/mnt/disk3", Device: "/dev/sdf", Confirmation: "ERASE /dev/sdf"})
	if got := errorCode(t, err); got != "disk_leaving_array" {
		t.Fatalf("UpgradeDisk(/mnt/disk3) = %s, want disk_leaving_array", got)
	}

	_, err = client.PlanDiskReplace(ctx, &apiv1.ReplaceDiskPlanRequest{Mountpoint: "/mnt/disk1", Device: "/dev/sdf"})
	if got := errorCode(t, err); got == "disk_leaving_array" {
		t.Fatal("PlanDiskReplace(/mnt/disk1) = disk_leaving_array, want disk1 (not in removal) past that check")
	}
}
