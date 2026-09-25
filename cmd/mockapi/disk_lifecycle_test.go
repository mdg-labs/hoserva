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
