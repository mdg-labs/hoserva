package main

import (
	"slices"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

func TestMockUpdateAndRevertApp(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if _, err := h.RevertApp(ctx, apiv1.RevertAppParams{ID: "jellyfin"}); err == nil || mockErrorCode(t, err) != "nothing_to_revert" {
		t.Fatalf("RevertApp before any update = %v, want nothing_to_revert", err)
	}

	j, err := h.UpdateApp(ctx, apiv1.UpdateAppParams{ID: "jellyfin"})
	if err != nil || j.Type != apiv1.JobTypeContainerUpdate || j.Class != apiv1.JobClassService {
		t.Fatalf("UpdateApp = %+v, %v, want a queued container_update job", j, err)
	}
	hist, err := h.ListAppUpdateHistory(ctx)
	if err != nil || len(hist.Records) != 1 || !hist.Records[0].Revertible || !hist.Records[0].SnapshotArchive.Set {
		t.Fatalf("history = %+v, %v, want one revertible record with a snapshot, as jellyfin's appdata is on the cache", hist, err)
	}

	if _, err := h.RevertApp(ctx, apiv1.RevertAppParams{ID: "jellyfin"}); err != nil {
		t.Fatalf("RevertApp: %v", err)
	}
	hist, _ = h.ListAppUpdateHistory(ctx)
	if hist.Records[0].Revertible || !hist.Records[0].RevertedAt.Set {
		t.Fatalf("record after the revert = %+v", hist.Records[0])
	}
	if _, err := h.RevertApp(ctx, apiv1.RevertAppParams{ID: "jellyfin"}); err == nil || mockErrorCode(t, err) != "nothing_to_revert" {
		t.Fatalf("a second RevertApp = %v, want nothing_to_revert", err)
	}
}

// Production's job records an update only when the pull brought a newer image,
// so the mock records one only for the container the update check shows an
// update for, and a container pinned to a digest cannot be updated at all.
func TestMockUpdateRecordsOnlyARealUpdateAndRefusesAPinnedContainer(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if _, err := h.UpdateApp(ctx, apiv1.UpdateAppParams{ID: "postgres"}); err != nil {
		t.Fatalf("UpdateApp of a container already on the newest image: %v", err)
	}
	if hist, _ := h.ListAppUpdateHistory(ctx); len(hist.Records) != 0 {
		t.Fatalf("history = %+v, want nothing recorded for an update that finds no newer image", hist.Records)
	}
	if _, err := h.RevertApp(ctx, apiv1.RevertAppParams{ID: "postgres"}); err == nil || mockErrorCode(t, err) != "nothing_to_revert" {
		t.Fatalf("RevertApp = %v, want nothing_to_revert", err)
	}

	if _, err := h.UpdateApp(ctx, apiv1.UpdateAppParams{ID: "transcoder"}); err == nil || mockErrorCode(t, err) != "app_pinned" {
		t.Fatalf("UpdateApp of a pinned container = %v, want app_pinned", err)
	}
	if _, err := h.StartAppUpdates(ctx, apiv1.NewOptStartAppUpdatesRequest(apiv1.StartAppUpdatesRequest{Containers: []string{"transcoder"}})); err == nil || mockErrorCode(t, err) != "app_pinned" {
		t.Fatalf("StartAppUpdates of a pinned container = %v, want app_pinned", err)
	}
	if hist, _ := h.ListAppUpdateHistory(ctx); len(hist.Records) != 0 {
		t.Fatalf("history = %+v after refused updates", hist.Records)
	}
}

func TestMockRevertIsUnavailableOnceTheKeepPeriodIsOver(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if _, err := h.UpdateApp(ctx, apiv1.UpdateAppParams{ID: "jellyfin"}); err != nil {
		t.Fatal(err)
	}
	h.appsMu.Lock()
	h.updateRecords[0].KeepUntil = time.Now().Add(-time.Minute)
	h.appsMu.Unlock()
	if _, err := h.RevertApp(ctx, apiv1.RevertAppParams{ID: "jellyfin"}); err == nil || mockErrorCode(t, err) != "revert_unavailable" {
		t.Fatalf("RevertApp after the keep period = %v, want revert_unavailable", err)
	}
	if hist, _ := h.ListAppUpdateHistory(ctx); hist.Records[0].RevertedAt.Set {
		t.Fatal("a refused revert was recorded as done")
	}
}

func TestMockBulkUpdateSkipsContainersThatOptedOut(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	got, err := h.StartAppUpdates(ctx, apiv1.OptStartAppUpdatesRequest{})
	if err != nil || !slices.Equal(got.Containers, []string{"jellyfin"}) || !got.Job.Set {
		t.Fatalf("StartAppUpdates = %+v, %v, want jellyfin updated", got, err)
	}

	if _, err := h.SetAppUpdatePolicy(ctx, &apiv1.SetAppUpdatePolicyRequest{BulkExcluded: true}, apiv1.SetAppUpdatePolicyParams{ID: "jellyfin"}); err != nil {
		t.Fatal(err)
	}
	updates, err := h.ListAppUpdates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range updates.Updates {
		if !u.BulkExcluded.Set || u.BulkExcluded.Value != (u.Container == "jellyfin") {
			t.Fatalf("%s bulkExcluded = %+v", u.Container, u.BulkExcluded)
		}
	}
	got, err = h.StartAppUpdates(ctx, apiv1.OptStartAppUpdatesRequest{})
	if err != nil || len(got.Containers) != 0 || got.Job.Set || len(got.Skipped) != 1 || got.Skipped[0].Container != "jellyfin" {
		t.Fatalf("StartAppUpdates after the opt-out = %+v, %v, want no job and jellyfin skipped", got, err)
	}
	got, err = h.StartAppUpdates(ctx, apiv1.NewOptStartAppUpdatesRequest(apiv1.StartAppUpdatesRequest{Containers: []string{"jellyfin"}}))
	if err != nil || !slices.Equal(got.Containers, []string{"jellyfin"}) {
		t.Fatalf("StartAppUpdates by name = %+v, %v, want the opt-out to apply to bulk updates only", got, err)
	}
}

func TestMockAppSettings(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if got, err := h.GetAppSettings(ctx); err != nil || got.ImageKeepDays != 7 {
		t.Fatalf("GetAppSettings = %+v, %v, want the 7 day default", got, err)
	}
	if got, err := h.UpdateAppSettings(ctx, &apiv1.AppSettings{ImageKeepDays: 14}); err != nil || got.ImageKeepDays != 14 {
		t.Fatalf("UpdateAppSettings = %+v, %v", got, err)
	}
	if _, err := h.UpdateAppSettings(ctx, &apiv1.AppSettings{ImageKeepDays: 0}); err == nil || mockErrorCode(t, err) != "invalid_image_keep_days" {
		t.Fatalf("UpdateAppSettings(0) = %v, want invalid_image_keep_days", err)
	}
	if got, _ := h.GetAppSettings(ctx); got.ImageKeepDays != 14 {
		t.Fatalf("a refused period changed the setting to %d", got.ImageKeepDays)
	}
}
