package main

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

func TestMockTestBackupDestination_ExternalDiskFollowsItsMountState(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if _, err := h.RegisterExternalDisk(ctx, &apiv1.RegisterExternalDiskRequest{Device: "/dev/sdf", Label: "backup2", BackupDestination: apiv1.NewOptBool(true)}); err != nil {
		t.Fatal(err)
	}
	params := apiv1.TestBackupDestinationParams{DestinationId: "external:backup2"}

	res, err := h.TestBackupDestination(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if res.Success {
		t.Fatal("test of an unmounted external disk succeeded, want the not-mounted refusal")
	}
	if msg := res.Error.Or(""); !strings.Contains(msg, "not mounted") {
		t.Fatalf("error = %q, want it to say the disk is not mounted", msg)
	}

	if _, err := h.MountExternalDisk(ctx, apiv1.MountExternalDiskParams{Label: "backup2"}); err != nil {
		t.Fatal(err)
	}
	res, err = h.TestBackupDestination(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Success {
		t.Fatalf("test of a mounted external disk = %+v, want success", res)
	}

	if _, err := h.EjectExternalDisk(ctx, apiv1.EjectExternalDiskParams{Label: "backup2"}); err != nil {
		t.Fatal(err)
	}
	res, err = h.TestBackupDestination(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if res.Success {
		t.Fatal("test after eject succeeded, want the not-mounted refusal")
	}
}
