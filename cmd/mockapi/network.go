package main

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

func defaultMockNetwork() apiv1.NetworkSettings {
	return apiv1.NetworkSettings{
		Backend:  apiv1.NetworkBackendIfupdown,
		Editable: true,
		Interfaces: []apiv1.NetworkInterface{{
			Name:    "enp1s0",
			MAC:     apiv1.NewOptString("02:00:00:00:00:01"),
			Method:  apiv1.NetworkAddressMethodDhcp,
			Address: apiv1.NewOptString("10.0.2.15"),
			Prefix:  apiv1.NewOptInt(24),
			Gateway: apiv1.NewOptString("10.0.2.2"),
			DNS:     []string{"1.1.1.1"},
			State:   apiv1.NetworkInterfaceStateUp,
		}},
		Certificate: apiv1.TLSCertificateInfo{
			Kind:          apiv1.TLSCertificateKindSelfSigned,
			NotAfter:      time.Date(2036, 9, 20, 0, 0, 0, 0, time.UTC),
			DaysRemaining: 3650,
		},
		LetsEncrypt:     apiv1.LetsEncryptStatus{Configured: false, Enabled: false},
		AllowAllSources: false,
		ListenPort:      8008,
	}
}

func (h *handler) cloneNetwork() *apiv1.NetworkSettings {
	h.notifyMu.Lock()
	defer h.notifyMu.Unlock()
	out := h.network
	out.Interfaces = append([]apiv1.NetworkInterface(nil), h.network.Interfaces...)
	return &out
}

func (h *handler) GetNetworkSettings(context.Context) (*apiv1.NetworkSettings, error) {
	return h.cloneNetwork(), nil
}

func (h *handler) ApplyNetworkSettings(_ context.Context, req *apiv1.ApplyNetworkSettingsRequest) (*apiv1.NetworkSettings, error) {
	h.notifyMu.Lock()
	defer h.notifyMu.Unlock()
	if v, ok := req.AllowAllSources.Get(); ok {
		h.network.AllowAllSources = v
	}
	if v, ok := req.ListenPort.Get(); ok {
		h.network.ListenPort = v
		if v != 8008 {
			h.network.ListenPortRestartRequired = apiv1.NewOptBool(true)
		}
	}
	if iface, ok := req.Interface.Get(); ok && iface != "" {
		method, methodSet := req.Method.Get()
		if !methodSet {
			return nil, &mockError{code: "network_invalid_input", statusCode: 400, message: "method is required to change addressing"}
		}
		for i := range h.network.Interfaces {
			if h.network.Interfaces[i].Name != iface {
				continue
			}
			h.network.Interfaces[i].Method = method
			if addr, ok := req.Address.Get(); ok {
				h.network.Interfaces[i].Address = apiv1.NewOptString(addr)
			}
			if prefix, ok := req.Prefix.Get(); ok {
				h.network.Interfaces[i].Prefix = apiv1.NewOptInt(prefix)
			}
			if gw, ok := req.Gateway.Get(); ok {
				if gw == "" {
					h.network.Interfaces[i].Gateway.Reset()
				} else {
					h.network.Interfaces[i].Gateway = apiv1.NewOptString(gw)
				}
			}
			if req.DNS != nil {
				h.network.Interfaces[i].DNS = req.DNS
			}
			expires := time.Now().UTC().Add(60 * time.Second)
			h.network.Pending = apiv1.NewOptNetworkPending(apiv1.NetworkPending{
				Interface:        iface,
				ExpiresAt:        expires,
				RemainingSeconds: 60,
			})
			break
		}
	}
	out := h.network
	out.Interfaces = append([]apiv1.NetworkInterface(nil), h.network.Interfaces...)
	return &out, nil
}

func (h *handler) ConfirmNetworkSettings(context.Context) (*apiv1.NetworkSettings, error) {
	h.notifyMu.Lock()
	defer h.notifyMu.Unlock()
	if !h.network.Pending.IsSet() {
		return nil, &mockError{code: "network_confirm_expired", statusCode: 409, message: "no pending network change to confirm"}
	}
	h.network.Pending.Reset()
	out := h.network
	out.Interfaces = append([]apiv1.NetworkInterface(nil), h.network.Interfaces...)
	return &out, nil
}

func (h *handler) RegenerateTLSCertificate(context.Context) (*apiv1.NetworkSettings, error) {
	h.notifyMu.Lock()
	defer h.notifyMu.Unlock()
	n := atomic.AddInt64(&h.certSerial, 1)
	h.network.Certificate.Kind = apiv1.TLSCertificateKindSelfSigned
	h.network.Certificate.Domain.Reset()
	h.network.Certificate.NotAfter = time.Now().UTC().Add(10 * 365 * 24 * time.Hour)
	h.network.Certificate.DaysRemaining = 3650
	h.network.LetsEncrypt = apiv1.LetsEncryptStatus{Configured: false, Enabled: false}
	_ = n
	out := h.network
	out.Interfaces = append([]apiv1.NetworkInterface(nil), h.network.Interfaces...)
	return &out, nil
}

func (h *handler) ConfigureLetsEncrypt(_ context.Context, req *apiv1.ConfigureLetsEncryptRequest) (*apiv1.Job, error) {
	h.notifyMu.Lock()
	h.network.LetsEncrypt = apiv1.LetsEncryptStatus{
		Configured: true,
		Enabled:    true,
		Domain:     apiv1.NewOptString(req.GetDomain()),
		Provider:   apiv1.NewOptDNS01Provider(req.GetProvider()),
		HasSecret:  apiv1.NewOptBool(true),
	}
	h.notifyMu.Unlock()

	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now().UTC()
	job := apiv1.Job{
		ID:          uuid.New(),
		Type:        apiv1.JobTypeAcmeIssue,
		Class:       apiv1.JobClassService,
		Status:      apiv1.JobStatusQueued,
		Resumable:   false,
		Cancellable: true,
		CreatedAt:   now,
	}
	h.jobs[job.ID] = job
	return &job, nil
}

func (h *handler) DisableLetsEncrypt(context.Context) (*apiv1.NetworkSettings, error) {
	h.notifyMu.Lock()
	defer h.notifyMu.Unlock()
	le := h.network.LetsEncrypt
	le.Enabled = false
	h.network.LetsEncrypt = le
	out := h.network
	out.Interfaces = append([]apiv1.NetworkInterface(nil), h.network.Interfaces...)
	return &out, nil
}
