package api_test

import (
	"context"
	"path/filepath"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/store"
)

func TestHandler_MigrationContainerOperations_Return501WithoutAService(t *testing.T) {
	h, _, _ := newTestHandler(t)
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"ListMigrationContainers": func() error { _, err := h.ListMigrationContainers(ctx); return err },
		"CreateMigrationStacks": func() error {
			_, err := h.CreateMigrationStacks(ctx, &apiv1.MigrationStacksRequest{Items: []apiv1.MigrationStackSelection{{Name: "a.xml"}}})
			return err
		},
		"StartMigrationContainer": func() error {
			_, err := h.StartMigrationContainer(ctx, apiv1.StartMigrationContainerParams{Name: "a"})
			return err
		},
		"CheckMigrationContainer": func() error {
			_, err := h.CheckMigrationContainer(ctx, apiv1.CheckMigrationContainerParams{Name: "a"})
			return err
		},
		"ConfirmMigrationContainer": func() error {
			_, err := h.ConfirmMigrationContainer(ctx, apiv1.OptMigrationContainerConfirmRequest{}, apiv1.ConfirmMigrationContainerParams{Name: "a"})
			return err
		},
	} {
		err := call()
		if err == nil {
			t.Fatalf("%s = nil without a migration service", name)
		}
		if st, code := statusOf(h, err); st != 501 || code != "not_configured" {
			t.Errorf("%s = %d %s, want 501 not_configured", name, st, code)
		}
	}
}

// Each refusal of Phase D has its own code and status, and a migration that
// cannot say whether its parity initialisation finished answers as an
// unconfigured one, never as a ready one.
func TestHandler_MigrationContainerOperations_RefusalCodes(t *testing.T) {
	h, svc := migrationHandler(t)
	ctx := context.Background()
	h.Stacks = &container.StackService{
		Store:  &stackMemStore{rows: map[string]store.Stack{}},
		Cipher: stackNopCipher{},
		Runner: container.NewFakeRunner(),
		Root:   filepath.Join(t.TempDir(), "stacks"),
	}
	svc.Stacks = h.Stacks
	create := func(items ...apiv1.MigrationStackSelection) error {
		_, err := h.CreateMigrationStacks(ctx, &apiv1.MigrationStacksRequest{Items: items})
		return err
	}
	check := func(what string, err error, status int, code string) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s = nil, want %d %s", what, status, code)
		}
		if st, c := statusOf(h, err); st != status || c != code {
			t.Errorf("%s = %d %s (%v), want %d %s", what, st, c, err, status, code)
		}
	}

	check("create with no way to tell", create(apiv1.MigrationStackSelection{Name: "a.xml"}), 501, "not_configured")
	ready := false
	svc.Initialized = func(context.Context) (bool, error) { return ready, nil }
	check("create before the parity initialisation", create(apiv1.MigrationStackSelection{Name: "a.xml"}), 409, "parity_not_initialized")
	_, err := h.StartMigrationContainer(ctx, apiv1.StartMigrationContainerParams{Name: "a"})
	check("start before the parity initialisation", err, 409, "parity_not_initialized")
	_, err = h.CheckMigrationContainer(ctx, apiv1.CheckMigrationContainerParams{Name: "a"})
	check("check before the parity initialisation", err, 409, "parity_not_initialized")
	_, err = h.ConfirmMigrationContainer(ctx, apiv1.OptMigrationContainerConfirmRequest{}, apiv1.ConfirmMigrationContainerParams{Name: "a"})
	check("confirm before the parity initialisation", err, 409, "parity_not_initialized")

	ready = true
	check("create with no scan", create(apiv1.MigrationStackSelection{Name: "a.xml"}), 404, "no_migration_report")
	j, err := h.StartMigrationScan(ctx, scanRequest(flashZip(t, "7.3.2", nil), false))
	if err != nil {
		t.Fatal(err)
	}
	if done, err := h.Scheduler.Await(ctx, j.ID.String()); err != nil || done.Status != job.StatusSucceeded {
		t.Fatalf("scan job = %+v, %v", done, err)
	}
	check("create with nothing selected", create(), 400, "invalid_selection")
	check("create with a name twice", create(apiv1.MigrationStackSelection{Name: "a.xml"}, apiv1.MigrationStackSelection{Name: "a.xml"}), 400, "invalid_selection")
	check("create of a template the scan lacks", create(apiv1.MigrationStackSelection{Name: "a.xml"}), 404, "template_not_found")
	_, err = h.StartMigrationContainer(ctx, apiv1.StartMigrationContainerParams{Name: "a"})
	check("start of a stack the migration did not create", err, 404, "migrated_stack_not_found")
	_, err = h.CheckMigrationContainer(ctx, apiv1.CheckMigrationContainerParams{Name: "a"})
	check("check of a stack the migration did not create", err, 404, "migrated_stack_not_found")
	_, err = h.ConfirmMigrationContainer(ctx, apiv1.OptMigrationContainerConfirmRequest{}, apiv1.ConfirmMigrationContainerParams{Name: "a"})
	check("confirm of a stack the migration did not create", err, 404, "migrated_stack_not_found")

	svc.Initialized = func(context.Context) (bool, error) { return false, context.DeadlineExceeded }
	_, err = h.ListMigrationContainers(ctx)
	if err == nil {
		t.Error("ListMigrationContainers with the parity record unreadable = nil")
	}
}
