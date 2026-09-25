package main

import (
	"context"
	"fmt"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

func defaultMockUpdateStatus() apiv1.UpdateStatus {
	return apiv1.UpdateStatus{
		CurrentVersion:   "0.1.0-dev",
		AvailableVersion: apiv1.NewOptString("0.2.0"),
		AvailableTag:     apiv1.NewOptString("v0.2.0"),
		Changelog:        apiv1.NewOptString("Mock changelog for the web UI."),
		Channel:          apiv1.UpdateChannelStable,
		CheckEnabled:     true,
		PreviousVersion:  apiv1.NewOptString("0.0.1"),
		RebootRequired:   false,
		PendingDebianUpdates: []apiv1.DebianPackageUpdate{
			{Name: "openssl", InstalledVersion: "3.0.13", CandidateVersion: "3.0.14"},
		},
		Dependencies: []apiv1.PackageDependencyStatus{
			{Name: "mergerfs", InstalledVersion: "2.40.2", TestedFloor: "2.40.2", InRange: true},
			{Name: "snapraid", InstalledVersion: "12.4", TestedFloor: "12.4", InRange: true},
		},
	}
}

func cloneUpdateStatus(in apiv1.UpdateStatus) *apiv1.UpdateStatus {
	out := in
	if in.PendingDebianUpdates != nil {
		out.PendingDebianUpdates = append([]apiv1.DebianPackageUpdate(nil), in.PendingDebianUpdates...)
	}
	if in.Dependencies != nil {
		out.Dependencies = append([]apiv1.PackageDependencyStatus(nil), in.Dependencies...)
	}
	return &out
}

func (h *handler) GetUpdateStatus(ctx context.Context) (*apiv1.UpdateStatus, error) {
	h.notifyMu.Lock()
	defer h.notifyMu.Unlock()
	return cloneUpdateStatus(h.updateStatus), nil
}

func (h *handler) UpdateUpdateSettings(ctx context.Context, req *apiv1.UpdateUpdateSettingsRequest) (*apiv1.UpdateStatus, error) {
	h.notifyMu.Lock()
	defer h.notifyMu.Unlock()
	if req != nil {
		if v, ok := req.Channel.Get(); ok {
			switch v {
			case apiv1.UpdateChannelStable, apiv1.UpdateChannelBeta:
				h.updateStatus.Channel = v
			default:
				return nil, &mockError{code: "update_invalid_channel", statusCode: 400, message: fmt.Sprintf("update: unrecognized channel: %q", v)}
			}
		}
		if v, ok := req.CheckEnabled.Get(); ok {
			h.updateStatus.CheckEnabled = v
			if !v {
				h.updateStatus.AvailableVersion.Reset()
				h.updateStatus.AvailableTag.Reset()
				h.updateStatus.Changelog.Reset()
			} else if !h.updateStatus.AvailableVersion.IsSet() {
				h.updateStatus.AvailableVersion = apiv1.NewOptString("0.2.0")
				h.updateStatus.AvailableTag = apiv1.NewOptString("v0.2.0")
			}
		}
	}
	return cloneUpdateStatus(h.updateStatus), nil
}

func (h *handler) CheckForUpdate(ctx context.Context) (*apiv1.UpdateStatus, error) {
	h.notifyMu.Lock()
	defer h.notifyMu.Unlock()
	h.updateStatus.AvailableVersion = apiv1.NewOptString("0.2.0")
	h.updateStatus.AvailableTag = apiv1.NewOptString("v0.2.0")
	return cloneUpdateStatus(h.updateStatus), nil
}

func (h *handler) ApplyUpdate(ctx context.Context, req *apiv1.ConfirmUpdateRequest) (*apiv1.UpdateStatus, error) {
	if req == nil || !req.Confirm {
		return nil, errConfirmRequired()
	}
	h.notifyMu.Lock()
	defer h.notifyMu.Unlock()
	if !h.updateStatus.AvailableVersion.IsSet() {
		return nil, &mockError{code: "update_not_available", statusCode: 409, message: "no newer release on the configured channel"}
	}
	prev := h.updateStatus.CurrentVersion
	h.updateStatus.PreviousVersion = apiv1.NewOptString(prev)
	h.updateStatus.CurrentVersion = h.updateStatus.AvailableVersion.Value
	h.updateStatus.AvailableVersion.Reset()
	h.updateStatus.AvailableTag.Reset()
	h.updateStatus.Changelog.Reset()
	return cloneUpdateStatus(h.updateStatus), nil
}

func (h *handler) RollbackUpdate(ctx context.Context, req *apiv1.ConfirmUpdateRequest) (*apiv1.UpdateStatus, error) {
	if req == nil || !req.Confirm {
		return nil, errConfirmRequired()
	}
	h.notifyMu.Lock()
	defer h.notifyMu.Unlock()
	prev, ok := h.updateStatus.PreviousVersion.Get()
	if !ok || prev == "" {
		return nil, &mockError{code: "rollback_not_available", statusCode: 409, message: "no previous version to roll back to"}
	}
	h.updateStatus.AvailableVersion = apiv1.NewOptString(h.updateStatus.CurrentVersion)
	h.updateStatus.CurrentVersion = prev
	h.updateStatus.PreviousVersion.Reset()
	return cloneUpdateStatus(h.updateStatus), nil
}

func (h *handler) RebootHost(ctx context.Context, req *apiv1.ConfirmUpdateRequest) (*apiv1.UpdateStatus, error) {
	if req == nil || !req.Confirm {
		return nil, errConfirmRequired()
	}
	h.notifyMu.Lock()
	defer h.notifyMu.Unlock()
	return cloneUpdateStatus(h.updateStatus), nil
}
