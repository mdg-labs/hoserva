package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
)

func newTestHandlerClient(t *testing.T, scenario string) (*handler, apiv1.Invoker) {
	t.Helper()
	h, err := newHandler(scenario)
	if err != nil {
		t.Fatalf("newHandler(%q): %v", scenario, err)
	}
	srv, err := apiv1.NewServer(h, securityHandler{}, apiv1.WithPathPrefix("/api/v1"))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	client, err := apiv1.NewClient(ts.URL+"/api/v1", staticSecurity{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return h, client
}

// Production's Scheduler.admitMigrationLocked refuses every array-write and
// topology job with 409 migration_in_progress while the migration is pending
// (an import waiting for its point of no return, or a parity initialisation
// that stopped part-way) and admits them once it is finished. Each handler
// that queues one of those jobs does the same, after its own validation.
func TestMockJobsRefusedWhileMigrationUnfinished(t *testing.T) {
	ctx := context.Background()
	evacuationPlan := func(t *testing.T, client apiv1.Invoker, mountpoint string) string {
		t.Helper()
		res, err := client.PlanDiskEvacuation(ctx, &apiv1.EvacuateDiskPlanRequest{Mountpoint: mountpoint})
		if err != nil {
			t.Fatalf("PlanDiskEvacuation: %v", err)
		}
		plan, ok := res.(*apiv1.EvacuationPlan)
		if !ok {
			t.Fatalf("PlanDiskEvacuation = %T, want *apiv1.EvacuationPlan", res)
		}
		return plan.Confirmation
	}

	cases := []struct {
		name     string
		scenario string
		prepare  func(t *testing.T, client apiv1.Invoker) func() error
	}{
		{"StartMover", "healthy", func(t *testing.T, client apiv1.Invoker) func() error {
			return func() error { _, err := client.StartMover(ctx); return err }
		}},
		{"StartRebalance", "healthy", func(t *testing.T, client apiv1.Invoker) func() error {
			plan, err := client.PlanRebalance(ctx)
			if err != nil {
				t.Fatalf("PlanRebalance: %v", err)
			}
			return func() error {
				_, err := client.StartRebalance(ctx, &apiv1.StartRebalanceRequest{Confirmation: plan.Confirmation})
				return err
			}
		}},
		{"EvacuateDisk", "healthy", func(t *testing.T, client apiv1.Invoker) func() error {
			confirmation := evacuationPlan(t, client, "/mnt/disk1")
			return func() error {
				_, err := client.EvacuateDisk(ctx, &apiv1.EvacuateDiskRequest{Mountpoint: "/mnt/disk1", Confirmation: confirmation})
				return err
			}
		}},
		{"AddDisk", "healthy", func(t *testing.T, client apiv1.Invoker) func() error {
			plan, err := client.PlanDiskAdd(ctx, &apiv1.AddDiskPlanRequest{Device: "/dev/sdf"})
			if err != nil {
				t.Fatalf("PlanDiskAdd: %v", err)
			}
			return func() error {
				_, err := client.AddDisk(ctx, &apiv1.AddDiskRequest{Device: "/dev/sdf", Confirmation: plan.Confirmation})
				return err
			}
		}},
		{"ReplaceDisk", "degraded", func(t *testing.T, client apiv1.Invoker) func() error {
			plan, err := client.PlanDiskReplace(ctx, &apiv1.ReplaceDiskPlanRequest{Mountpoint: "/mnt/disk4", Device: "/dev/sdf"})
			if err != nil {
				t.Fatalf("PlanDiskReplace: %v", err)
			}
			return func() error {
				_, err := client.ReplaceDisk(ctx, &apiv1.ReplaceDiskRequest{Mountpoint: "/mnt/disk4", Device: "/dev/sdf", Confirmation: plan.Confirmation})
				return err
			}
		}},
		{"UpgradeDiskData", "healthy", func(t *testing.T, client apiv1.Invoker) func() error {
			plan, err := client.PlanDiskUpgrade(ctx, &apiv1.DiskUpgradePlanRequest{Mountpoint: "/mnt/disk1", Device: "/dev/sdf"})
			if err != nil {
				t.Fatalf("PlanDiskUpgrade(data): %v", err)
			}
			if _, err := client.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
				t.Fatalf("StopArray: %v", err)
			}
			return func() error {
				_, err := client.UpgradeDisk(ctx, &apiv1.UpgradeDiskRequest{Mountpoint: "/mnt/disk1", Device: "/dev/sdf", Confirmation: plan.Confirmation})
				return err
			}
		}},
		{"UpgradeDiskParity", "healthy", func(t *testing.T, client apiv1.Invoker) func() error {
			plan, err := client.PlanDiskUpgrade(ctx, &apiv1.DiskUpgradePlanRequest{Mountpoint: "/mnt/parity", Device: "/dev/sdf"})
			if err != nil {
				t.Fatalf("PlanDiskUpgrade(parity): %v", err)
			}
			return func() error {
				_, err := client.UpgradeDisk(ctx, &apiv1.UpgradeDiskRequest{Mountpoint: "/mnt/parity", Device: "/dev/sdf", Confirmation: plan.Confirmation, NewMountpoint: plan.NewMountpoint})
				return err
			}
		}},
		{"FinishDiskRemoval", "sync-blocked", func(t *testing.T, client apiv1.Invoker) func() error {
			return func() error {
				_, err := client.FinishDiskRemoval(ctx, &apiv1.FinishDiskRemovalRequest{Mountpoint: "/mnt/disk5", Confirmation: job.EvacuationConfirmation("/mnt/disk5")})
				return err
			}
		}},
	}

	for _, tc := range cases {
		for _, state := range []struct {
			name string
			set  func(h *handler)
		}{
			{"pending import", func(h *handler) { h.migration.imported.Store(true) }},
			{"initializing", func(h *handler) { h.migration.finishing.Store(true) }},
		} {
			t.Run(tc.name+"/"+state.name, func(t *testing.T) {
				h, client := newTestHandlerClient(t, tc.scenario)
				call := tc.prepare(t, client)
				state.set(h)

				err := call()
				var apiErr *apiv1.ErrorStatusCode
				if !errors.As(err, &apiErr) {
					t.Fatalf("%s while the migration is %s = %v, want 409 migration_in_progress", tc.name, state.name, err)
				}
				if apiErr.StatusCode != 409 || apiErr.Response.Code != "migration_in_progress" {
					t.Fatalf("%s while the migration is %s = %d %s, want 409 migration_in_progress", tc.name, state.name, apiErr.StatusCode, apiErr.Response.Code)
				}
				if listed, err := client.ListJobs(ctx, apiv1.ListJobsParams{}); err != nil {
					t.Fatal(err)
				} else {
					before := len(listed.Jobs)
					h.migration.imported.Store(false)
					h.migration.finishing.Store(false)
					if err := call(); err != nil {
						t.Fatalf("%s after the migration finished: %v", tc.name, err)
					}
					after, _ := client.ListJobs(ctx, apiv1.ListJobsParams{})
					if len(after.Jobs) != before+1 {
						t.Fatalf("jobs after the accepted %s = %d, want %d: the refusal queued one or the acceptance did not", tc.name, len(after.Jobs), before+1)
					}
				}
			})
		}
	}
}

// The same refusal through the migration's own flow: an import waiting for its
// point of no return, then a parity initialisation stopped part-way
// (--migration-stop-parity-init), then the finished migration.
func TestMockMover_FollowsTheMigrationFlow(t *testing.T) {
	ctx := context.Background()
	h, _ := newHandler("migration-pending")
	h.migration.stopParityInit = true
	mockVerifiedMigration(t, h)

	mover := func(what string, want string) {
		t.Helper()
		_, err := h.StartMover(ctx)
		if want == "" {
			if err != nil {
				t.Fatalf("StartMover %s: %v", what, err)
			}
			return
		}
		if err == nil {
			t.Fatalf("StartMover %s was accepted", what)
		}
		if st, code := mockErrCode(t, err); st != 409 || code != want {
			t.Fatalf("StartMover %s = %d %s, want 409 %s", what, st, code, want)
		}
	}
	mover("with the import pending", "migration_in_progress")

	m, _ := h.GetMigration(ctx)
	pi, _ := m.ParityInit.Get()
	if _, err := h.InitializeMigrationParity(ctx, &apiv1.MigrationInitializeParityRequest{Confirmation: pi.Confirmation.Value}); err != nil {
		t.Fatalf("InitializeMigrationParity: %v", err)
	}
	mover("with the parity initialisation stopped", "migration_in_progress")

	if _, err := h.InitializeMigrationParity(ctx, &apiv1.MigrationInitializeParityRequest{Confirmation: disk.ParityInitFinishConfirmation}); err != nil {
		t.Fatalf("finishing: %v", err)
	}
	mover("after the migration finished", "")
}
