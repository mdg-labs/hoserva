package api_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/store"
)

type stackNopCipher struct{}

func (stackNopCipher) Encrypt(p []byte) ([]byte, error) { return append([]byte{}, p...), nil }
func (stackNopCipher) Decrypt(c []byte) ([]byte, error) { return append([]byte{}, c...), nil }

// stackWrongKey cannot open what was sealed, as under another machine key.
type stackWrongKey struct{ stackNopCipher }

func (stackWrongKey) Decrypt([]byte) ([]byte, error) { return nil, errors.New("wrong key") }

type stackMemStore struct{ rows map[string]store.Stack }

func (m *stackMemStore) Insert(_ context.Context, st store.Stack) error {
	if _, ok := m.rows[st.Name]; ok {
		return store.ErrStackExists
	}
	m.rows[st.Name] = st
	return nil
}

func (m *stackMemStore) Get(_ context.Context, name string) (store.Stack, error) {
	st, ok := m.rows[name]
	if !ok {
		return store.Stack{}, store.ErrStackNotFound
	}
	return st, nil
}

func (m *stackMemStore) List(context.Context) ([]store.Stack, error) { return nil, nil }

func (m *stackMemStore) Delete(_ context.Context, name string) error {
	delete(m.rows, name)
	return nil
}

func newStacksHandler(t *testing.T) (*api.Handler, string) {
	t.Helper()
	h, _, _ := newTestHandler(t)
	root := filepath.Join(t.TempDir(), "stacks")
	h.Stacks = &container.StackService{
		Store:  &stackMemStore{rows: map[string]store.Stack{}},
		Cipher: stackNopCipher{},
		Runner: container.NewFakeRunner(),
		Root:   root,
	}
	return h, root
}

func TestStacks_NotConfiguredIs501(t *testing.T) {
	h, _, _ := newTestHandler(t)
	_, err := h.RemoveStack(context.Background(), apiv1.RemoveStackParams{Name: "nginx"})
	if status, code := statusOf(h, err); status != 501 || code != "not_configured" {
		t.Fatalf("RemoveStack = %d %q, want 501 not_configured", status, code)
	}
}

type stackFailingInsert struct{ *stackMemStore }

func (stackFailingInsert) Insert(context.Context, store.Stack) error {
	return errors.New("disk I/O error")
}

type stackFailingRunner struct{}

func (stackFailingRunner) Run(context.Context, []string, string, ...string) ([]byte, error) {
	return nil, errors.New("signal: killed")
}

// A failure of `docker compose` itself is a 502; a failure inside the daemon
// (here, the database) is a 500, not blamed on an upstream.
func TestStacks_ComposeFailuresAre502AndLocalFailures500(t *testing.T) {
	ctx := context.Background()
	req := &apiv1.CreateStackRequest{Name: "nginx", Compose: "services: {}\n"}

	h, _ := newStacksHandler(t)
	h.Stacks.Runner = stackFailingRunner{}
	_, err := h.CreateStack(ctx, req)
	if status, code := statusOf(h, err); status != 502 || code != "stack_action_failed" {
		t.Fatalf("CreateStack with compose failing = %d %q, want 502 stack_action_failed", status, code)
	}

	h, _ = newStacksHandler(t)
	h.Stacks.Store = stackFailingInsert{&stackMemStore{rows: map[string]store.Stack{}}}
	_, err = h.CreateStack(ctx, req)
	if status, code := statusOf(h, err); status != 500 || code != "stack_action_failed" {
		t.Fatalf("CreateStack with the database failing = %d %q, want 500 stack_action_failed", status, code)
	}
}

func TestStacks_ErrorsAreMappedAndAUnsafeNameDeletesNothing(t *testing.T) {
	h, root := newStacksHandler(t)
	ctx := context.Background()
	outside := filepath.Join(filepath.Dir(root), "keep")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := h.RemoveStack(ctx, apiv1.RemoveStackParams{Name: "../keep", DeleteAppdata: apiv1.NewOptBool(true)})
	if status, code := statusOf(h, err); status != 400 || code != "invalid_stack_name" {
		t.Fatalf("RemoveStack(../keep) = %d %q, want 400 invalid_stack_name", status, code)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("a directory outside the stacks directory was deleted: %v", err)
	}

	_, err = h.RemoveStack(ctx, apiv1.RemoveStackParams{Name: "missing"})
	if status, code := statusOf(h, err); status != 404 || code != "stack_not_found" {
		t.Fatalf("RemoveStack(missing) = %d %q, want 404 stack_not_found", status, code)
	}

	if err := os.MkdirAll(filepath.Join(root, "taken"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "taken", "docker-compose.yml"), []byte("hand written"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = h.CreateStack(ctx, &apiv1.CreateStackRequest{Name: "taken", Compose: "services: {}\n"})
	if status, code := statusOf(h, err); status != 409 || code != "stack_dir_exists" {
		t.Fatalf("CreateStack over an existing directory = %d %q, want 409 stack_dir_exists", status, code)
	}

	created, err := h.CreateStack(ctx, &apiv1.CreateStackRequest{
		Name:     "nginx",
		Compose:  "services: {}\n",
		Template: apiv1.NewOptStackTemplate(apiv1.StackTemplate{Source: "catalog", ID: "nginx", Revision: "2"}),
	})
	if err != nil {
		t.Fatalf("CreateStack: %v", err)
	}
	if created.Template.Revision != "2" || created.Name != "nginx" {
		t.Fatalf("CreateStack = %+v", created)
	}
}

// Removing and installing a stack again under the same name works through the
// API, and an appdata deletion that is not allowed is reported, not turned
// into a 502.
func TestStacks_RemoveThenCreateAgainAndAppdataRefusalsAreMapped(t *testing.T) {
	h, _ := newStacksHandler(t)
	h.Stacks.Provider = container.NewFakeProvider()
	ctx := context.Background()
	req := &apiv1.CreateStackRequest{Name: "plex", Compose: "services: {}\n"}
	if _, err := h.CreateStack(ctx, req); err != nil {
		t.Fatalf("CreateStack: %v", err)
	}
	if _, err := h.RemoveStack(ctx, apiv1.RemoveStackParams{Name: "plex"}); err != nil {
		t.Fatalf("RemoveStack: %v", err)
	}
	if _, err := h.CreateStack(ctx, req); err != nil {
		t.Fatalf("CreateStack after a remove: %v", err)
	}

	// The service has no array state, so it refuses, fail closed.
	_, err := h.RemoveStack(ctx, apiv1.RemoveStackParams{Name: "plex", DeleteAppdata: apiv1.NewOptBool(true)})
	if status, code := statusOf(h, err); status != 503 || code != "array_state_unknown" {
		t.Fatalf("RemoveStack with appdata and no array state = %d %q, want 503 array_state_unknown", status, code)
	}
}

// A stack with no .env is taken down by its project name alone, so another
// project of that name is reported as 409 stack_project_shared, not removed.
func TestStacks_RemoveWithoutAnEnvIsRefusedForAForeignProject(t *testing.T) {
	h, root := newStacksHandler(t)
	ctx := context.Background()
	fake := container.NewFakeProvider()
	h.Stacks.Provider = fake
	if _, err := h.CreateStack(ctx, &apiv1.CreateStackRequest{Name: "immich", Compose: "services: {}\n"}); err != nil {
		t.Fatalf("CreateStack: %v", err)
	}
	h.Stacks.Cipher = stackWrongKey{}
	if err := os.Remove(filepath.Join(root, "immich", ".env")); err != nil {
		t.Fatal(err)
	}
	fake.AddContainer(container.Container{ID: "a", Name: "immich_postgres", State: "running", Labels: map[string]string{
		"com.docker.compose.project":             "immich",
		"com.docker.compose.project.working_dir": "/home/user/immich",
	}})

	_, err := h.RemoveStack(ctx, apiv1.RemoveStackParams{Name: "immich"})
	if status, code := statusOf(h, err); status != 409 || code != "stack_project_shared" {
		t.Fatalf("RemoveStack = %d %q, want 409 stack_project_shared", status, code)
	}
}
