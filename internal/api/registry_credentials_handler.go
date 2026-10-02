package api

import (
	"context"
	"errors"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/container"
)

// RegistryCredentialService is the credentials the daily update check logs in
// to registries with (Q81): *container.RegistryCredentials is the production
// implementation. It never returns a credential.
type RegistryCredentialService interface {
	PutCredential(ctx context.Context, registry string, c container.Credential) error
	DeleteCredential(ctx context.Context, registry string) error
	ListCredentialRegistries(ctx context.Context) ([]string, error)
}

func errRegistryCredentialsNotConfigured() error {
	return &apiError{code: "not_configured", statusCode: 501, message: "Registry credentials are not configured on this daemon"}
}

func mapRegistryCredentialError(err error) error {
	switch {
	case errors.Is(err, container.ErrInvalidCredential):
		return &apiError{code: "invalid_registry_credential", statusCode: 400, message: err.Error()}
	case errors.Is(err, container.ErrCredentialNotFound):
		return &apiError{code: "registry_credential_not_found", statusCode: 404, message: err.Error()}
	}
	return err
}

func (h *Handler) ListRegistryCredentials(ctx context.Context) (*apiv1.RegistryCredentialList, error) {
	if h.RegistryCredentials == nil {
		return nil, errRegistryCredentialsNotConfigured()
	}
	registries, err := h.RegistryCredentials.ListCredentialRegistries(ctx)
	if err != nil {
		return nil, mapRegistryCredentialError(err)
	}
	return &apiv1.RegistryCredentialList{Registries: registries}, nil
}

func (h *Handler) PutRegistryCredential(ctx context.Context, req *apiv1.PutRegistryCredentialRequest, params apiv1.PutRegistryCredentialParams) error {
	if h.RegistryCredentials == nil {
		return errRegistryCredentialsNotConfigured()
	}
	err := h.RegistryCredentials.PutCredential(ctx, params.Registry, container.Credential{Username: req.Username, Password: req.Password})
	return mapRegistryCredentialError(err)
}

func (h *Handler) DeleteRegistryCredential(ctx context.Context, params apiv1.DeleteRegistryCredentialParams) error {
	if h.RegistryCredentials == nil {
		return errRegistryCredentialsNotConfigured()
	}
	return mapRegistryCredentialError(h.RegistryCredentials.DeleteCredential(ctx, params.Registry))
}
