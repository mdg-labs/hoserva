package api_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/store"
)

const stackOldCompose = "services:\n  web:\n    image: nginx:1.27\n"
const stackNewCompose = "services:\n  web:\n    image: nginx:1.28\n"

// composeConfigRejects makes `docker compose config` fail with an exit error
// and records every docker call.
type composeConfigRejects struct {
	container.Runner
	reject bool
	calls  [][]string
}

func (c *composeConfigRejects) Run(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	c.calls = append(c.calls, args)
	for _, a := range args {
		if a == "config" && c.reject {
			return nil, &exec.ExitError{}
		}
	}
	return c.Runner.Run(ctx, env, name, args...)
}

// stackListingStore lists its rows, which the install's port check would
// also read; the edit tests have no ports to find.
type stackListingStore struct{ *stackMemStore }

func (m stackListingStore) List(context.Context) ([]store.Stack, error) {
	var out []store.Stack
	for _, st := range m.rows {
		out = append(out, st)
	}
	return out, nil
}

func newStacksEditFixture(t *testing.T) (*api.Handler, *composeConfigRejects, string) {
	h, runner, root, _, _ := newStacksEditFixtureWithJobs(t)
	return h, runner, root
}

func newStacksEditFixtureWithJobs(t *testing.T) (*api.Handler, *composeConfigRejects, string, *job.Scheduler, *job.Registry) {
	t.Helper()
	h, sched, reg := newTestHandler(t)
	root := filepath.Join(t.TempDir(), "stacks")
	runner := &composeConfigRejects{Runner: container.NewFakeRunner()}
	h.Stacks = &container.StackService{
		Store:               stackListingStore{&stackMemStore{rows: map[string]store.Stack{}}},
		Cipher:              stackNopCipher{},
		Runner:              runner,
		Root:                root,
		RequireArrayRunning: func() error { return nil },
	}
	if _, err := h.CreateStack(context.Background(), &apiv1.CreateStackRequest{
		Name: "nginx", Compose: stackOldCompose, Env: apiv1.NewOptString("TOKEN=s3cret\n"),
	}); err != nil {
		t.Fatalf("CreateStack: %v", err)
	}
	return h, runner, root, sched, reg
}

func TestStacks_GetReturnsTheComposeAndTheFlagAndListOnlyTheFlag(t *testing.T) {
	h, _, _ := newStacksEditFixture(t)
	ctx := context.Background()

	got, err := h.GetStack(ctx, apiv1.GetStackParams{Name: "nginx"})
	if err != nil {
		t.Fatalf("GetStack: %v", err)
	}
	if got.Compose.Or("") != stackOldCompose || got.ManuallyEdited {
		t.Fatalf("GetStack = compose %q, manuallyEdited %v; want the stored text, not edited", got.Compose.Or(""), got.ManuallyEdited)
	}
	if _, err := h.UpdateStack(ctx, &apiv1.UpdateStackRequest{Compose: stackNewCompose}, apiv1.UpdateStackParams{Name: "nginx"}); err != nil {
		t.Fatalf("UpdateStack: %v", err)
	}
	if got, _ = h.GetStack(ctx, apiv1.GetStackParams{Name: "nginx"}); !got.ManuallyEdited || got.Compose.Or("") != stackNewCompose {
		t.Fatalf("GetStack after an edit = %+v", got)
	}
	list, err := h.ListStacks(ctx)
	if err != nil || len(list.Stacks) != 1 {
		t.Fatalf("ListStacks = %+v, %v", list, err)
	}
	if !list.Stacks[0].ManuallyEdited || list.Stacks[0].Compose.IsSet() {
		t.Fatalf("listed stack = %+v; want the flag and no compose text", list.Stacks[0])
	}
	if strings.Contains(got.Compose.Or(""), "s3cret") {
		t.Fatal("getStack returned the .env")
	}
}

func TestStacks_UpdateRefusesAnInvalidFileWith400AndChangesNothing(t *testing.T) {
	h, runner, root := newStacksEditFixture(t)
	ctx := context.Background()
	runner.reject = true

	_, err := h.UpdateStack(ctx, &apiv1.UpdateStackRequest{Compose: "services: [\n"}, apiv1.UpdateStackParams{Name: "nginx"})
	if status, code := statusOf(h, err); status != 400 || code != "invalid_stack" {
		t.Fatalf("UpdateStack of an invalid file = %d %q, want 400 invalid_stack", status, code)
	}
	if got := h.NewError(ctx, err).Response.Message; !strings.Contains(got, "invalid compose stack") {
		t.Errorf("the 400 carries %q, want the compiler's message", got)
	}
	got, _ := h.GetStack(ctx, apiv1.GetStackParams{Name: "nginx"})
	if got.Compose.Or("") != stackOldCompose || got.ManuallyEdited {
		t.Fatalf("a refused edit changed the stack: %+v", got)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "nginx", "docker-compose.yml")); string(b) != stackOldCompose {
		t.Fatalf("a refused edit changed the file: %q", b)
	}
}

func TestStacks_UpdateDryRunAndErrorsAreMapped(t *testing.T) {
	h, _, _ := newStacksEditFixture(t)
	ctx := context.Background()

	res, err := h.UpdateStack(ctx, &apiv1.UpdateStackRequest{Compose: stackNewCompose}, apiv1.UpdateStackParams{Name: "nginx", DryRun: apiv1.NewOptBool(true)})
	if err != nil {
		t.Fatalf("UpdateStack dry run: %v", err)
	}
	if res.Applied || res.Stack.ManuallyEdited || res.Stack.Compose.Or("") != stackOldCompose {
		t.Fatalf("dry run = %+v; want applied false and the stored stack", res)
	}
	res, err = h.UpdateStack(ctx, &apiv1.UpdateStackRequest{Compose: stackNewCompose}, apiv1.UpdateStackParams{Name: "nginx"})
	if err != nil || !res.Applied || !res.Stack.ManuallyEdited || res.Stack.Compose.Or("") != stackNewCompose {
		t.Fatalf("UpdateStack = %+v, %v", res, err)
	}

	_, err = h.UpdateStack(ctx, &apiv1.UpdateStackRequest{Compose: stackNewCompose}, apiv1.UpdateStackParams{Name: "missing"})
	if status, code := statusOf(h, err); status != 404 || code != "stack_not_found" {
		t.Errorf("unknown stack = %d %q, want 404 stack_not_found", status, code)
	}
	_, err = h.UpdateStack(ctx, &apiv1.UpdateStackRequest{Compose: stackNewCompose}, apiv1.UpdateStackParams{Name: "../x"})
	if status, code := statusOf(h, err); status != 400 || code != "invalid_stack_name" {
		t.Errorf("invalid name = %d %q, want 400 invalid_stack_name", status, code)
	}
	h.Stacks = nil
	_, err = h.UpdateStack(ctx, &apiv1.UpdateStackRequest{Compose: stackNewCompose}, apiv1.UpdateStackParams{Name: "nginx"})
	if status, code := statusOf(h, err); status != 501 || code != "not_configured" {
		t.Errorf("no stack service = %d %q, want 501 not_configured", status, code)
	}
}

func TestStacks_UpdateWithAnUnopenableEnvIsNot400(t *testing.T) {
	h, _, _ := newStacksEditFixture(t)
	h.Stacks.Cipher = stackWrongKey{}
	_, err := h.UpdateStack(context.Background(), &apiv1.UpdateStackRequest{Compose: stackNewCompose}, apiv1.UpdateStackParams{Name: "nginx"})
	if status, code := statusOf(h, err); status != 500 || code != "stack_action_failed" {
		t.Fatalf("UpdateStack = %d %q, want 500 stack_action_failed: a local failure is not an invalid file", status, code)
	}
}

func TestStacks_StartQueuesAServiceJobThatRunsComposeUp(t *testing.T) {
	h, runner, root, sched, registry := newStacksEditFixtureWithJobs(t)
	ctx := context.Background()
	registry.Register(job.TypeStackStart, false, job.RunStackStart(h.Stacks.Up))
	if _, err := h.UpdateStack(ctx, &apiv1.UpdateStackRequest{Compose: stackNewCompose}, apiv1.UpdateStackParams{Name: "nginx"}); err != nil {
		t.Fatalf("UpdateStack: %v", err)
	}

	j, err := h.StartStack(ctx, apiv1.StartStackParams{Name: "nginx"})
	if err != nil {
		t.Fatalf("StartStack: %v", err)
	}
	if j.Type != apiv1.JobTypeStackStart || j.Class != apiv1.JobClassService {
		t.Fatalf("job = %s/%s, want stack_start in the service class", j.Type, j.Class)
	}
	done := awaitJob(t, sched, j.ID.String())
	if done.Status != job.StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", done.Status, done.ErrorMessage)
	}
	var up []string
	for _, c := range runner.calls {
		for _, a := range c {
			if a == "up" {
				up = c
			}
		}
	}
	if len(up) == 0 || up[len(up)-1] != "--detach" {
		t.Fatalf("the job did not run `compose up --detach`: %v", runner.calls)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "nginx", "docker-compose.yml")); string(b) != stackNewCompose {
		t.Fatalf("the started file = %q, want the edited text", b)
	}
}

func TestStacks_StartIsRefusedBeforeQueueingWhileTheArrayIsNotRunning(t *testing.T) {
	h, runner, _ := newStacksEditFixture(t)
	ctx := context.Background()

	h.Stacks.RequireArrayRunning = func() error { return container.ErrArrayStopped }
	_, err := h.StartStack(ctx, apiv1.StartStackParams{Name: "nginx"})
	if status, code := statusOf(h, err); status != 409 || code != "array_stopped" {
		t.Fatalf("StartStack with the array stopped = %d %q, want 409 array_stopped", status, code)
	}
	h.Stacks.RequireArrayRunning = func() error { return container.ErrArrayStateUnknown }
	_, err = h.StartStack(ctx, apiv1.StartStackParams{Name: "nginx"})
	if status, code := statusOf(h, err); status != 503 || code != "array_state_unknown" {
		t.Fatalf("StartStack with the array state unknown = %d %q, want 503 array_state_unknown", status, code)
	}
	jobs, err := h.Store.List(ctx, job.ListFilter{})
	if err != nil || len(jobs) != 0 {
		t.Fatalf("a refused start queued jobs: %+v, %v", jobs, err)
	}
	for _, c := range runner.calls {
		for _, a := range c {
			if a == "up" {
				t.Fatal("a refused start ran compose up")
			}
		}
	}

	h.Stacks.RequireArrayRunning = func() error { return nil }
	_, err = h.StartStack(ctx, apiv1.StartStackParams{Name: "missing"})
	if status, code := statusOf(h, err); status != 404 || code != "stack_not_found" {
		t.Errorf("unknown stack = %d %q, want 404 stack_not_found", status, code)
	}
	_, err = h.StartStack(ctx, apiv1.StartStackParams{Name: "../x"})
	if status, code := statusOf(h, err); status != 400 || code != "invalid_stack_name" {
		t.Errorf("invalid name = %d %q, want 400 invalid_stack_name", status, code)
	}
}

func TestStacks_UpdateAndStartAreAdminOperations(t *testing.T) {
	for _, op := range []apiv1.OperationName{apiv1.UpdateStackOperation, apiv1.StartStackOperation} {
		if got, ok := api.RoleFor(op); !ok || got != api.RoleAdmin {
			t.Errorf("%s needs %v (known %v), want admin", op, got, ok)
		}
	}
}
