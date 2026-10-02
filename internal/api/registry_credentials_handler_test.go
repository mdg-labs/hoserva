package api_test

import (
	"context"
	"fmt"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/container"
)

type fakeRegistryCredentials struct {
	put      map[string]container.Credential
	registry []string
	err      error
}

func (f *fakeRegistryCredentials) PutCredential(_ context.Context, registry string, c container.Credential) error {
	if f.err != nil {
		return f.err
	}
	if f.put == nil {
		f.put = map[string]container.Credential{}
	}
	f.put[registry] = c
	return nil
}

func (f *fakeRegistryCredentials) DeleteCredential(context.Context, string) error { return f.err }

func (f *fakeRegistryCredentials) ListCredentialRegistries(context.Context) ([]string, error) {
	return f.registry, f.err
}

func TestRegistryCredentials_NotConfiguredIs501(t *testing.T) {
	h := &api.Handler{}
	ctx := context.Background()
	calls := map[string]func() error{
		"ListRegistryCredentials": func() error { _, err := h.ListRegistryCredentials(ctx); return err },
		"PutRegistryCredential": func() error {
			return h.PutRegistryCredential(ctx, &apiv1.PutRegistryCredentialRequest{Username: "u", Password: "p"}, apiv1.PutRegistryCredentialParams{Registry: "ghcr.io"})
		},
		"DeleteRegistryCredential": func() error {
			return h.DeleteRegistryCredential(ctx, apiv1.DeleteRegistryCredentialParams{Registry: "ghcr.io"})
		},
	}
	for name, call := range calls {
		if status, code := statusOf(h, call()); status != 501 || code != "not_configured" {
			t.Errorf("%s = %d %q, want 501 not_configured", name, status, code)
		}
	}
}

func TestRegistryCredentials_PutPassesTheCredentialAndListReturnsHostsOnly(t *testing.T) {
	ctx := context.Background()
	svc := &fakeRegistryCredentials{registry: []string{"ghcr.io"}}
	h := &api.Handler{RegistryCredentials: svc}
	err := h.PutRegistryCredential(ctx, &apiv1.PutRegistryCredentialRequest{Username: "me", Password: "pw"}, apiv1.PutRegistryCredentialParams{Registry: "ghcr.io"})
	if err != nil || svc.put["ghcr.io"] != (container.Credential{Username: "me", Password: "pw"}) {
		t.Fatalf("PutRegistryCredential = %v, saved %+v", err, svc.put)
	}
	list, err := h.ListRegistryCredentials(ctx)
	if err != nil || fmt.Sprint(list.Registries) != "[ghcr.io]" {
		t.Fatalf("ListRegistryCredentials = %+v, %v", list, err)
	}
}

func TestRegistryCredentials_ErrorsMapToTheirStatus(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		err    error
		status int
		code   string
	}{
		"invalid":   {fmt.Errorf("%w: a username and a password are both required", container.ErrInvalidCredential), 400, "invalid_registry_credential"},
		"not found": {container.ErrCredentialNotFound, 404, "registry_credential_not_found"},
		"other":     {fmt.Errorf("the database is locked"), 500, ""},
	} {
		h := &api.Handler{RegistryCredentials: &fakeRegistryCredentials{err: tc.err}}
		put := h.PutRegistryCredential(ctx, &apiv1.PutRegistryCredentialRequest{Username: "u", Password: "p"}, apiv1.PutRegistryCredentialParams{Registry: "ghcr.io"})
		del := h.DeleteRegistryCredential(ctx, apiv1.DeleteRegistryCredentialParams{Registry: "ghcr.io"})
		_, list := h.ListRegistryCredentials(ctx)
		for op, err := range map[string]error{"put": put, "delete": del, "list": list} {
			if status, code := statusOf(h, err); status != tc.status || (tc.code != "" && code != tc.code) {
				t.Errorf("%s/%s = %d %q, want %d %q", name, op, status, code, tc.status, tc.code)
			}
		}
	}
}
