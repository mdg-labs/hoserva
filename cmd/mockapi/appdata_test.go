package main

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

func TestMockAppdata_OptingADatabaseOutOfBeingStoppedCarriesTheWarning(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	before, err := h.GetAppdataBackup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]apiv1.AppdataBackupContainer{}
	for _, c := range before.Containers {
		names[c.Name] = c
	}
	if _, ok := names["portainer"]; ok {
		t.Fatal("portainer mounts no appdata and must not be in scope")
	}
	pg, ok := names["postgres"]
	if !ok || !pg.DatabaseImage || !pg.Stop {
		t.Fatalf("postgres = %+v, %v; want a database image that is stopped by default", pg, ok)
	}

	got, err := h.SetAppdataBackupContainer(ctx, &apiv1.SetAppdataBackupContainerRequest{Stop: false, Included: true},
		apiv1.SetAppdataBackupContainerParams{Name: "postgres"})
	if err != nil {
		t.Fatal(err)
	}
	if w, ok := got.Warning.Get(); !ok || !strings.Contains(w, "database") {
		t.Fatalf("warning = %q, %v", w, ok)
	}
	after, _ := h.GetAppdataBackup(ctx)
	for _, c := range after.Containers {
		if c.Name == "postgres" && c.Stop {
			t.Fatal("the stored policy did not show up in the next listing")
		}
	}
}
