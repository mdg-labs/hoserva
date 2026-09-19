package api

import (
	"context"
	"errors"
	"fmt"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

func mapSettingsError(err error) error {
	if errors.Is(err, ErrInvalidSettingsInput) {
		return &apiError{code: "settings_invalid_input", statusCode: 400, message: err.Error()}
	}
	return err
}

func (h *Handler) GetGeneralSettings(ctx context.Context) (*apiv1.GeneralSettings, error) {
	if h.Settings == nil {
		return nil, fmt.Errorf("settings service is not configured")
	}
	settings, err := h.Settings.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting general settings: %w", err)
	}
	return generalSettingsToAPI(settings), nil
}

func (h *Handler) UpdateGeneralSettings(ctx context.Context, req *apiv1.UpdateGeneralSettingsRequest) (*apiv1.GeneralSettings, error) {
	if h.Settings == nil {
		return nil, fmt.Errorf("settings service is not configured")
	}
	settings, err := h.Settings.Update(ctx, updateGeneralSettingsFromAPI(req))
	if err != nil {
		return nil, mapSettingsError(err)
	}
	return generalSettingsToAPI(settings), nil
}

func generalSettingsToAPI(settings GeneralSettings) *apiv1.GeneralSettings {
	out := &apiv1.GeneralSettings{BackupPassphraseSet: settings.BackupPassphraseSet}
	if settings.Hostname != nil {
		out.Hostname = apiv1.NewOptString(*settings.Hostname)
	}
	if settings.Timezone != nil {
		out.Timezone = apiv1.NewOptString(*settings.Timezone)
	}
	return out
}

func updateGeneralSettingsFromAPI(req *apiv1.UpdateGeneralSettingsRequest) UpdateGeneralSettingsInput {
	input := UpdateGeneralSettingsInput{}
	if req.Hostname.IsSet() {
		v := req.Hostname.Value
		input.Hostname = &v
	}
	if req.Timezone.IsSet() {
		v := req.Timezone.Value
		input.Timezone = &v
	}
	if req.BackupPassphrase.IsSet() {
		v := req.BackupPassphrase.Value
		input.BackupPassphrase = &v
	}
	return input
}
