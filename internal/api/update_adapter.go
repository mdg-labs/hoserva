package api

import (
	"context"

	"github.com/mdg-labs/hoserva/internal/update"
)

// NewUpdateSettings adapts SettingsService to update.Settings.
func NewUpdateSettings(svc *SettingsService) update.Settings {
	if svc == nil {
		return nil
	}
	return engineSettings{svc: svc}
}

// engineSettings adapts SettingsService to update.Settings.
type engineSettings struct {
	svc *SettingsService
}

func (a engineSettings) Get(ctx context.Context) (update.SettingsRow, error) {
	row, err := a.svc.GetUpdateSettings(ctx)
	if err != nil {
		return update.SettingsRow{}, err
	}
	return update.SettingsRow{
		Channel:         update.Channel(row.Channel),
		CheckEnabled:    row.CheckEnabled,
		PreviousVersion: row.PreviousVersion,
	}, nil
}

func (a engineSettings) SetChannelAndCheck(ctx context.Context, channel update.Channel, checkEnabled bool) error {
	return a.svc.SetUpdateSettings(ctx, string(channel), checkEnabled)
}

func (a engineSettings) SetPreviousVersion(ctx context.Context, version string) error {
	return a.svc.SetPreviousVersion(ctx, version)
}
