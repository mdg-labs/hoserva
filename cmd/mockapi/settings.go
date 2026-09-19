package main

import (
	"context"
	"strings"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

func (h *handler) GetGeneralSettings(ctx context.Context) (*apiv1.GeneralSettings, error) {
	h.notifyMu.Lock()
	defer h.notifyMu.Unlock()
	return cloneGeneralSettings(h.generalSettings), nil
}

func (h *handler) UpdateGeneralSettings(ctx context.Context, req *apiv1.UpdateGeneralSettingsRequest) (*apiv1.GeneralSettings, error) {
	h.notifyMu.Lock()
	defer h.notifyMu.Unlock()

	if hostname, ok := req.Hostname.Get(); ok {
		trimmed := strings.TrimSpace(hostname)
		if hostname != "" && trimmed == "" {
			return nil, &mockError{code: "settings_invalid_input", statusCode: 400, message: "hostname must not be whitespace only"}
		}
		if trimmed == "" {
			h.generalSettings.Hostname.Reset()
		} else {
			h.generalSettings.Hostname = apiv1.NewOptString(trimmed)
		}
	}
	if timezone, ok := req.Timezone.Get(); ok {
		trimmed := strings.TrimSpace(timezone)
		if trimmed == "" {
			return nil, &mockError{code: "settings_invalid_input", statusCode: 400, message: "timezone must not be empty"}
		}
		if _, err := time.LoadLocation(trimmed); err != nil {
			return nil, &mockError{code: "settings_invalid_input", statusCode: 400, message: "unknown timezone " + trimmed}
		}
		h.generalSettings.Timezone = apiv1.NewOptString(trimmed)
	}
	if passphrase, ok := req.BackupPassphrase.Get(); ok {
		h.generalSettings.BackupPassphraseSet = passphrase != ""
	}
	return cloneGeneralSettings(h.generalSettings), nil
}

func cloneGeneralSettings(in apiv1.GeneralSettings) *apiv1.GeneralSettings {
	out := apiv1.GeneralSettings{BackupPassphraseSet: in.BackupPassphraseSet}
	if v, ok := in.Hostname.Get(); ok {
		out.Hostname = apiv1.NewOptString(v)
	}
	if v, ok := in.Timezone.Get(); ok {
		out.Timezone = apiv1.NewOptString(v)
	}
	return &out
}
