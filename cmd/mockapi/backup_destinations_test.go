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

func addLocalDestination(t *testing.T, h *handler, name, path string) string {
	t.Helper()
	dest, err := h.CreateBackupDestination(context.Background(), &apiv1.CreateBackupDestinationRequest{
		Name: name,
		Type: apiv1.BackupDestinationTypeLocal,
		Path: path,
	})
	if err != nil {
		t.Fatal(err)
	}
	return dest.ID
}

func testDestination(t *testing.T, h *handler, id string) *apiv1.BackupDestinationTestResult {
	t.Helper()
	res, err := h.TestBackupDestination(context.Background(), apiv1.TestBackupDestinationParams{DestinationId: id})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestMockTestBackupDestination_HandAddedExternalPathFollowsItsMountState(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if _, err := h.RegisterExternalDisk(ctx, &apiv1.RegisterExternalDiskRequest{Device: "/dev/sdf", Label: "backup2"}); err != nil {
		t.Fatal(err)
	}
	id := addLocalDestination(t, h, "Hand added", "/mnt/disks/backup2/hoserva")

	res := testDestination(t, h, id)
	if res.Success {
		t.Fatal("test of a hand-added destination on an unmounted external disk succeeded, want the not-mounted refusal")
	}
	if want := `the external disk is not mounted at "/mnt/disks/backup2"`; res.Error.Or("") != want {
		t.Fatalf("error = %q, want %q", res.Error.Or(""), want)
	}

	if _, err := h.MountExternalDisk(ctx, apiv1.MountExternalDiskParams{Label: "backup2"}); err != nil {
		t.Fatal(err)
	}
	if res := testDestination(t, h, id); !res.Success {
		t.Fatalf("test of a hand-added destination on a mounted external disk = %+v, want success", res)
	}
}

func TestMockTestBackupDestination_HandAddedPathOnAnUnknownExternalDiskIsRefused(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	id := addLocalDestination(t, h, "Unknown disk", "/mnt/disks/nowhere")
	res := testDestination(t, h, id)
	if res.Success || !strings.Contains(res.Error.Or(""), "not mounted") {
		t.Fatalf("result = %+v, want the not-mounted refusal", res)
	}
}

func TestMockTestBackupDestination_PoolDestinationFollowsThePoolMount(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	handAdded := addLocalDestination(t, h, "Hand added", "/mnt/user/other")

	for _, id := range []string{"pool", handAdded} {
		if res := testDestination(t, h, id); !res.Success {
			t.Fatalf("test of %q with the pool mounted = %+v, want success", id, res)
		}
	}

	if _, err := h.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"pool", handAdded} {
		res := testDestination(t, h, id)
		if res.Success {
			t.Fatalf("test of %q with the pool stopped succeeded, want the not-mounted refusal", id)
		}
		if want := `the pool is not mounted at "/mnt/user"`; res.Error.Or("") != want {
			t.Fatalf("error = %q, want %q", res.Error.Or(""), want)
		}
	}

	if _, err := h.StartArray(ctx); err != nil {
		t.Fatal(err)
	}
	if res := testDestination(t, h, "pool"); !res.Success {
		t.Fatalf("test of the pool destination after the array restarted = %+v, want success", res)
	}
}

func TestMockTestBackupDestination_DestinationsOffTheMountedPathsAreUnaffected(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.StopArray(context.Background(), &apiv1.StopArrayRequest{Confirm: true}); err != nil {
		t.Fatal(err)
	}
	lookalike := addLocalDestination(t, h, "Lookalike", "/mnt/username/backups")
	for _, id := range []string{"boot", lookalike} {
		if res := testDestination(t, h, id); !res.Success {
			t.Fatalf("test of %q = %+v, want success: it is on neither mount", id, res)
		}
	}
}
