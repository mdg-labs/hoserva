package main

import (
	"context"
	"errors"
	"sort"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/container"
)

func mockRegistryCredentialError(err error) error {
	switch {
	case errors.Is(err, container.ErrInvalidCredential):
		return &mockError{code: "invalid_registry_credential", statusCode: 400, message: err.Error()}
	case errors.Is(err, container.ErrCredentialNotFound):
		return &mockError{code: "registry_credential_not_found", statusCode: 404, message: err.Error()}
	}
	return err
}

func (h *handler) ListRegistryCredentials(context.Context) (*apiv1.RegistryCredentialList, error) {
	h.registryMu.Lock()
	defer h.registryMu.Unlock()
	registries := make([]string, 0, len(h.registryCredentials))
	for r := range h.registryCredentials {
		registries = append(registries, r)
	}
	sort.Strings(registries)
	return &apiv1.RegistryCredentialList{Registries: registries}, nil
}

func (h *handler) PutRegistryCredential(_ context.Context, req *apiv1.PutRegistryCredentialRequest, params apiv1.PutRegistryCredentialParams) error {
	key, err := container.NormalizeRegistryHost(params.Registry)
	if err != nil {
		return mockRegistryCredentialError(err)
	}
	if err := container.ValidateCredential(container.Credential{Username: req.Username, Password: req.Password}); err != nil {
		return mockRegistryCredentialError(err)
	}
	h.registryMu.Lock()
	defer h.registryMu.Unlock()
	h.registryCredentials[key] = true
	return nil
}

func (h *handler) DeleteRegistryCredential(_ context.Context, params apiv1.DeleteRegistryCredentialParams) error {
	key, err := container.NormalizeRegistryHost(params.Registry)
	if err != nil {
		return mockRegistryCredentialError(err)
	}
	h.registryMu.Lock()
	defer h.registryMu.Unlock()
	if !h.registryCredentials[key] {
		return mockRegistryCredentialError(container.ErrCredentialNotFound)
	}
	delete(h.registryCredentials, key)
	return nil
}
