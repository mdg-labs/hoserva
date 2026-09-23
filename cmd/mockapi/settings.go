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

func (h *handler) GetUPSSettings(ctx context.Context) (*apiv1.UPSSettings, error) {
	h.notifyMu.Lock()
	defer h.notifyMu.Unlock()
	return cloneUPSSettings(h.upsSettings), nil
}

func (h *handler) UpdateUPSSettings(ctx context.Context, req *apiv1.UpdateUPSSettingsRequest) (*apiv1.UPSSettings, error) {
	h.notifyMu.Lock()
	defer h.notifyMu.Unlock()

	out := apiv1.UPSSettings{Configured: true, Connection: apiv1.NewOptUPSConnection(req.Connection)}
	switch req.Connection {
	case apiv1.UPSConnectionUsb:
		driver, ok := req.Driver.Get()
		if !ok || strings.TrimSpace(driver) == "" {
			return nil, &mockError{code: "ups_invalid_input", statusCode: 400, message: "a USB UPS needs a NUT driver name (for example usbhid-ups)"}
		}
		port, ok := req.Port.Get()
		if !ok || strings.TrimSpace(port) == "" {
			return nil, &mockError{code: "ups_invalid_input", statusCode: 400, message: "a USB UPS needs a driver port (commonly auto)"}
		}
		monitorSet := false
		if prev, ok := h.upsSettings.Connection.Get(); ok && prev == apiv1.UPSConnectionUsb {
			monitorSet = h.upsSettings.MonitorPasswordSet.Or(false)
		}
		if pwd, ok := req.MonitorPassword.Get(); ok {
			if strings.TrimSpace(pwd) == "" {
				return nil, &mockError{code: "ups_invalid_input", statusCode: 400, message: "a USB UPS needs a monitor password"}
			}
			monitorSet = true
		}
		if !monitorSet {
			return nil, &mockError{code: "ups_invalid_input", statusCode: 400, message: "a USB UPS needs a monitor password"}
		}
		if pct, ok := req.LowBatteryPercent.Get(); ok && (pct < 0 || pct > 100) {
			return nil, &mockError{code: "ups_invalid_input", statusCode: 400, message: "low battery percent must be between 0 and 100"}
		}
		if runtime, ok := req.RuntimeSeconds.Get(); ok && runtime < 0 {
			return nil, &mockError{code: "ups_invalid_input", statusCode: 400, message: "runtime on battery must not be negative"}
		}
		out.Driver = apiv1.NewOptString(strings.TrimSpace(driver))
		out.Port = apiv1.NewOptString(strings.TrimSpace(port))
		out.MonitorPasswordSet = apiv1.NewOptBool(true)
		if pct, ok := req.LowBatteryPercent.Get(); ok && pct > 0 {
			out.LowBatteryPercent = apiv1.NewOptInt32(pct)
		}
		if runtime, ok := req.RuntimeSeconds.Get(); ok && runtime > 0 {
			out.RuntimeSeconds = apiv1.NewOptInt32(runtime)
		}
	case apiv1.UPSConnectionNetwork:
		host, ok := req.NetworkHost.Get()
		if !ok || strings.TrimSpace(host) == "" {
			return nil, &mockError{code: "ups_invalid_input", statusCode: 400, message: "a network UPS needs the NUT server hostname"}
		}
		upsName, ok := req.NetworkUpsName.Get()
		if !ok || strings.TrimSpace(upsName) == "" {
			return nil, &mockError{code: "ups_invalid_input", statusCode: 400, message: "a network UPS needs the UPS name on that NUT server"}
		}
		username, ok := req.NetworkUsername.Get()
		if !ok || strings.TrimSpace(username) == "" {
			return nil, &mockError{code: "ups_invalid_input", statusCode: 400, message: "a network UPS needs the monitoring username"}
		}
		networkSet := false
		if prev, ok := h.upsSettings.Connection.Get(); ok && prev == apiv1.UPSConnectionNetwork {
			networkSet = h.upsSettings.NetworkPasswordSet.Or(false)
		}
		if pwd, ok := req.NetworkPassword.Get(); ok {
			if strings.TrimSpace(pwd) == "" {
				return nil, &mockError{code: "ups_invalid_input", statusCode: 400, message: "a network UPS needs the monitoring password"}
			}
			networkSet = true
		}
		if !networkSet {
			return nil, &mockError{code: "ups_invalid_input", statusCode: 400, message: "a network UPS needs the monitoring password"}
		}
		port := int32(3493)
		if p, ok := req.NetworkPort.Get(); ok {
			if p < 1 || p > 65535 {
				return nil, &mockError{code: "ups_invalid_input", statusCode: 400, message: "network port must be between 1 and 65535"}
			}
			port = p
		}
		out.NetworkHost = apiv1.NewOptString(strings.TrimSpace(host))
		out.NetworkPort = apiv1.NewOptInt32(port)
		out.NetworkUpsName = apiv1.NewOptString(strings.TrimSpace(upsName))
		out.NetworkUsername = apiv1.NewOptString(strings.TrimSpace(username))
		out.NetworkPasswordSet = apiv1.NewOptBool(true)
	default:
		return nil, &mockError{code: "ups_invalid_input", statusCode: 400, message: "connection must be usb or network"}
	}
	h.upsSettings = out
	return cloneUPSSettings(out), nil
}

func cloneUPSSettings(in apiv1.UPSSettings) *apiv1.UPSSettings {
	out := apiv1.UPSSettings{Configured: in.Configured}
	if !in.Configured {
		return &out
	}
	if v, ok := in.Connection.Get(); ok {
		out.Connection = apiv1.NewOptUPSConnection(v)
	}
	if v, ok := in.Driver.Get(); ok {
		out.Driver = apiv1.NewOptString(v)
	}
	if v, ok := in.Port.Get(); ok {
		out.Port = apiv1.NewOptString(v)
	}
	if v, ok := in.MonitorPasswordSet.Get(); ok {
		out.MonitorPasswordSet = apiv1.NewOptBool(v)
	}
	if v, ok := in.NetworkHost.Get(); ok {
		out.NetworkHost = apiv1.NewOptString(v)
	}
	if v, ok := in.NetworkPort.Get(); ok {
		out.NetworkPort = apiv1.NewOptInt32(v)
	}
	if v, ok := in.NetworkUpsName.Get(); ok {
		out.NetworkUpsName = apiv1.NewOptString(v)
	}
	if v, ok := in.NetworkUsername.Get(); ok {
		out.NetworkUsername = apiv1.NewOptString(v)
	}
	if v, ok := in.NetworkPasswordSet.Get(); ok {
		out.NetworkPasswordSet = apiv1.NewOptBool(v)
	}
	if v, ok := in.LowBatteryPercent.Get(); ok {
		out.LowBatteryPercent = apiv1.NewOptInt32(v)
	}
	if v, ok := in.RuntimeSeconds.Get(); ok {
		out.RuntimeSeconds = apiv1.NewOptInt32(v)
	}
	return &out
}
