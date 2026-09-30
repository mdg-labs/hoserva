package main

import (
	"context"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

func TestMockGetPool_MountedFollowsTheArrayStopAndStart(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	poolMounted := func() bool {
		t.Helper()
		pool, err := h.GetPool(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return pool.Mounted
	}

	if !poolMounted() {
		t.Fatal("GET /pool with the array running: mounted = false, want true")
	}
	if _, err := h.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
		t.Fatal(err)
	}
	if poolMounted() {
		t.Fatal("GET /pool with the array stopped: mounted = true, want false")
	}
	if res := testDestination(t, h, "pool"); res.Success {
		t.Fatal("backup connection test of the pool with the array stopped succeeded, want the not-mounted refusal")
	}
	if _, err := h.StartArray(ctx); err != nil {
		t.Fatal(err)
	}
	if !poolMounted() {
		t.Fatal("GET /pool after the array restarted: mounted = false, want true")
	}
}

func TestMockGetPool_FreshInstallReportsTheUnmountedPool(t *testing.T) {
	h, err := newHandler("fresh-install")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := h.GetPool(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if pool.Mounted {
		t.Fatal("GET /pool in the fresh-install scenario: mounted = true, want false")
	}
}
