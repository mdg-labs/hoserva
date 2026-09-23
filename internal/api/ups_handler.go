package api

import (
	"context"
	"errors"
	"fmt"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/config"
)

func mapUPSError(err error) error {
	switch {
	case errors.Is(err, ErrInvalidUPSInput):
		return &apiError{code: "ups_invalid_input", statusCode: 400, message: plainUPSMessage(err)}
	case errors.Is(err, ErrMissingNUTGroup):
		return &apiError{code: "ups_nut_group_missing", statusCode: 409, message: err.Error()}
	case errors.Is(err, config.ErrUnmanaged), errors.Is(err, config.ErrExistingHostFile):
		return &apiError{code: "unmanaged_config", statusCode: 409, message: err.Error()}
	default:
		return err
	}
}

func plainUPSMessage(err error) string {
	msg := err.Error()
	const prefix = "settings: invalid ups input: "
	if len(msg) > len(prefix) && msg[:len(prefix)] == prefix {
		return msg[len(prefix):]
	}
	if msg == ErrInvalidUPSInput.Error() {
		return "the UPS settings are not valid"
	}
	return msg
}

func (h *Handler) GetUPSSettings(ctx context.Context) (*apiv1.UPSSettings, error) {
	if h.UPS == nil {
		return nil, fmt.Errorf("ups settings service is not configured")
	}
	view, err := h.UPS.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting ups settings: %w", err)
	}
	return upsSettingsToAPI(view), nil
}

func (h *Handler) UpdateUPSSettings(ctx context.Context, req *apiv1.UpdateUPSSettingsRequest) (*apiv1.UPSSettings, error) {
	if h.UPS == nil {
		return nil, fmt.Errorf("ups settings service is not configured")
	}
	view, err := h.UPS.Update(ctx, updateUPSFromAPI(req))
	if err != nil {
		return nil, mapUPSError(err)
	}
	return upsSettingsToAPI(view), nil
}

func updateUPSFromAPI(req *apiv1.UpdateUPSSettingsRequest) UpdateUPSInput {
	input := UpdateUPSInput{Connection: config.UPSConnection(req.Connection)}
	if v, ok := req.Driver.Get(); ok {
		input.Driver = v
	}
	if v, ok := req.Port.Get(); ok {
		input.Port = v
	}
	if v, ok := req.MonitorPassword.Get(); ok {
		input.MonitorPassword = &v
	}
	if v, ok := req.NetworkHost.Get(); ok {
		input.NetworkHost = v
	}
	if v, ok := req.NetworkPort.Get(); ok {
		input.NetworkPort = int(v)
	}
	if v, ok := req.NetworkUpsName.Get(); ok {
		input.NetworkUPSName = v
	}
	if v, ok := req.NetworkUsername.Get(); ok {
		input.NetworkUsername = v
	}
	if v, ok := req.NetworkPassword.Get(); ok {
		input.NetworkPassword = &v
	}
	if v, ok := req.LowBatteryPercent.Get(); ok {
		input.LowBatteryPercent = int(v)
	}
	if v, ok := req.RuntimeSeconds.Get(); ok {
		input.RuntimeSeconds = int(v)
	}
	return input
}

func upsSettingsToAPI(view UPSView) *apiv1.UPSSettings {
	out := &apiv1.UPSSettings{Configured: view.Configured}
	if !view.Configured {
		return out
	}
	out.Connection = apiv1.NewOptUPSConnection(apiv1.UPSConnection(view.Connection))
	switch view.Connection {
	case config.UPSConnectionUSB:
		out.Driver = apiv1.NewOptString(view.Driver)
		out.Port = apiv1.NewOptString(view.Port)
		out.MonitorPasswordSet = apiv1.NewOptBool(view.MonitorPasswordSet)
		if view.LowBatteryPercent > 0 {
			out.LowBatteryPercent = apiv1.NewOptInt32(int32(view.LowBatteryPercent))
		}
		if view.RuntimeSeconds > 0 {
			out.RuntimeSeconds = apiv1.NewOptInt32(int32(view.RuntimeSeconds))
		}
	case config.UPSConnectionNetwork:
		out.NetworkHost = apiv1.NewOptString(view.NetworkHost)
		if view.NetworkPort > 0 {
			out.NetworkPort = apiv1.NewOptInt32(int32(view.NetworkPort))
		}
		out.NetworkUpsName = apiv1.NewOptString(view.NetworkUPSName)
		out.NetworkUsername = apiv1.NewOptString(view.NetworkUsername)
		out.NetworkPasswordSet = apiv1.NewOptBool(view.NetworkPasswordSet)
	}
	return out
}
