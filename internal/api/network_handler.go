package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/acme"
	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/job"
)

// TLSCertView is the daemon's current TLS certificate expiry (Q9).
type TLSCertView struct {
	Kind     string
	Domain   string
	NotAfter time.Time
}

// HTTPSControl is access-scope, listen-port and certificate operations
// the network settings page exposes. cmd/hoservad implements this against
// the live TCP listener; tests inject a fake.
type HTTPSControl interface {
	Certificate() (TLSCertView, error)
	Regenerate(ctx context.Context) (TLSCertView, error)
	AllowAllSources() bool
	SetAllowAllSources(bool) error
	ListenPort() int
	SetListenPort(int) error
	ListenPortRestartRequired() bool
}

func errNetworkNotConfigured() error {
	return &apiError{code: "not_configured", statusCode: 501, message: "network settings are not configured on this daemon"}
}

func mapNetworkError(err error) error {
	switch {
	case errors.Is(err, config.ErrNetworkInvalid):
		return &apiError{code: "network_invalid_input", statusCode: 400, message: err.Error()}
	case errors.Is(err, config.ErrNetworkReadOnly):
		return &apiError{code: "network_read_only", statusCode: 409, message: err.Error()}
	case errors.Is(err, config.ErrNetworkPending):
		return &apiError{code: "network_pending", statusCode: 409, message: err.Error()}
	case errors.Is(err, config.ErrNetworkConfirmExpired):
		return &apiError{code: "network_confirm_expired", statusCode: 409, message: err.Error()}
	case errors.Is(err, config.ErrUnmanaged), errors.Is(err, config.ErrExistingHostFile):
		return &apiError{code: "unmanaged_config", statusCode: 409, message: err.Error()}
	default:
		return err
	}
}

func (h *Handler) GetNetworkSettings(ctx context.Context) (*apiv1.NetworkSettings, error) {
	if h.Network == nil {
		return nil, errNetworkNotConfigured()
	}
	st, err := h.Network.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting network settings: %w", err)
	}
	return h.networkSettingsToAPI(ctx, st)
}

func (h *Handler) ApplyNetworkSettings(ctx context.Context, req *apiv1.ApplyNetworkSettingsRequest) (*apiv1.NetworkSettings, error) {
	if h.Network == nil {
		return nil, errNetworkNotConfigured()
	}
	change, addressing, err := networkChangeFromAPI(req)
	if err != nil {
		return nil, mapNetworkError(err)
	}
	if err := validateHTTPSRequest(h.HTTPS, req); err != nil {
		return nil, mapNetworkError(err)
	}
	snap, haveSnap := snapshotHTTPS(h.HTTPS)
	if err := h.applyHTTPS(req); err != nil {
		restoreHTTPS(h.HTTPS, snap, haveSnap)
		return nil, mapNetworkError(err)
	}
	if addressing {
		st, err := h.Network.Apply(ctx, change)
		if err != nil {
			restoreHTTPS(h.HTTPS, snap, haveSnap)
			return nil, mapNetworkError(err)
		}
		return h.networkSettingsToAPI(ctx, st)
	}
	st, err := h.Network.Status(ctx)
	if err != nil {
		restoreHTTPS(h.HTTPS, snap, haveSnap)
		return nil, fmt.Errorf("getting network settings: %w", err)
	}
	return h.networkSettingsToAPI(ctx, st)
}

func (h *Handler) ConfirmNetworkSettings(ctx context.Context) (*apiv1.NetworkSettings, error) {
	if h.Network == nil {
		return nil, errNetworkNotConfigured()
	}
	st, err := h.Network.Confirm(ctx)
	if err != nil {
		return nil, mapNetworkError(err)
	}
	return h.networkSettingsToAPI(ctx, st)
}

func (h *Handler) RegenerateTLSCertificate(ctx context.Context) (*apiv1.NetworkSettings, error) {
	if h.Network == nil {
		return nil, errNetworkNotConfigured()
	}
	if h.HTTPS == nil {
		return nil, errNetworkNotConfigured()
	}
	if h.ACME != nil {
		if err := h.ACME.Clear(ctx); err != nil {
			return nil, fmt.Errorf("disarming Let's Encrypt renewal: %w", err)
		}
	}
	if _, err := h.HTTPS.Regenerate(ctx); err != nil {
		return nil, fmt.Errorf("regenerating TLS certificate: %w", err)
	}
	st, err := h.Network.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting network settings: %w", err)
	}
	return h.networkSettingsToAPI(ctx, st)
}

func (h *Handler) ConfigureLetsEncrypt(ctx context.Context, req *apiv1.ConfigureLetsEncryptRequest) (*apiv1.Job, error) {
	if h.ACME == nil {
		return nil, errNetworkNotConfigured()
	}
	if h.Scheduler == nil {
		return nil, fmt.Errorf("job scheduler not configured")
	}
	setup := acme.Setup{
		Domain:   req.GetDomain(),
		Provider: string(req.GetProvider()),
	}
	if v, ok := req.GetCloudflareAPIToken().Get(); ok {
		setup.CloudflareAPIToken = v
	}
	if v, ok := req.GetRfc2136Nameserver().Get(); ok {
		setup.RFC2136Nameserver = v
	}
	if v, ok := req.GetRfc2136TsigKeyName().Get(); ok {
		setup.RFC2136TSIGKeyName = v
	}
	if v, ok := req.GetRfc2136TsigSecret().Get(); ok {
		setup.RFC2136TSIGSecret = v
	}
	if v, ok := req.GetRfc2136TsigAlgorithm().Get(); ok {
		setup.RFC2136TSIGAlgorithm = v
	}
	if err := h.ACME.Configure(ctx, setup); err != nil {
		return nil, mapACMEError(err)
	}
	body, err := json.Marshal(job.ACMEIssueParams{Renew: false})
	if err != nil {
		return nil, fmt.Errorf("encoding acme_issue params: %w", err)
	}
	j, err := h.Scheduler.Submit(ctx, job.TypeACMEIssue, []string{"tls"}, body)
	if err != nil {
		return nil, mapSchedulerError(uuid.Nil, err)
	}
	return jobToAPI(j)
}

func (h *Handler) DisableLetsEncrypt(ctx context.Context) (*apiv1.NetworkSettings, error) {
	if h.Network == nil {
		return nil, errNetworkNotConfigured()
	}
	if h.ACME == nil {
		return nil, errNetworkNotConfigured()
	}
	if err := h.ACME.Disable(ctx); err != nil {
		return nil, fmt.Errorf("disabling Let's Encrypt: %w", err)
	}
	st, err := h.Network.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting network settings: %w", err)
	}
	return h.networkSettingsToAPI(ctx, st)
}

func validateHTTPSRequest(https HTTPSControl, req *apiv1.ApplyNetworkSettingsRequest) error {
	port, portSet := req.ListenPort.Get()
	if https == nil {
		if req.AllowAllSources.IsSet() || portSet {
			return fmt.Errorf("%w: HTTPS settings are not configured", config.ErrNetworkInvalid)
		}
		return nil
	}
	if portSet && (port < 1 || port > 65535) {
		return fmt.Errorf("%w: listen port must be between 1 and 65535", config.ErrNetworkInvalid)
	}
	return nil
}

func snapshotHTTPS(https HTTPSControl) (httpsSnapshot, bool) {
	if https == nil {
		return httpsSnapshot{}, false
	}
	return httpsSnapshot{allowAll: https.AllowAllSources(), port: https.ListenPort()}, true
}

func restoreHTTPS(https HTTPSControl, snap httpsSnapshot, have bool) {
	if https == nil || !have {
		return
	}
	_ = https.SetAllowAllSources(snap.allowAll)
	_ = https.SetListenPort(snap.port)
}

type httpsSnapshot struct {
	allowAll bool
	port     int
}

func (h *Handler) applyHTTPS(req *apiv1.ApplyNetworkSettingsRequest) error {
	if h.HTTPS == nil {
		return nil
	}
	if v, ok := req.AllowAllSources.Get(); ok {
		if err := h.HTTPS.SetAllowAllSources(v); err != nil {
			return err
		}
	}
	if v, ok := req.ListenPort.Get(); ok {
		if err := h.HTTPS.SetListenPort(v); err != nil {
			return err
		}
	}
	return nil
}

func networkChangeFromAPI(req *apiv1.ApplyNetworkSettingsRequest) (config.NetworkChange, bool, error) {
	iface, ifaceSet := req.Interface.Get()
	method, methodSet := req.Method.Get()
	address, addressSet := req.Address.Get()
	prefix, prefixSet := req.Prefix.Get()
	gateway, gatewaySet := req.Gateway.Get()
	dnsSet := req.DNS != nil
	addressing := ifaceSet || methodSet || addressSet || prefixSet || gatewaySet || dnsSet
	if !addressing {
		return config.NetworkChange{}, false, nil
	}
	if !ifaceSet || iface == "" {
		return config.NetworkChange{}, false, fmt.Errorf("%w: interface is required to change addressing", config.ErrNetworkInvalid)
	}
	change := config.NetworkChange{Interface: iface, DNS: req.DNS, DNSSet: dnsSet}
	if methodSet {
		change.Method = config.AddressMethod(method)
	}
	if change.Method == "" {
		return config.NetworkChange{}, false, fmt.Errorf("%w: method is required to change addressing", config.ErrNetworkInvalid)
	}
	if addressSet {
		change.Address = address
	}
	if prefixSet {
		change.Prefix = prefix
	}
	if gatewaySet {
		change.Gateway = gateway
	}
	if change.Method == config.MethodStatic && (!addressSet || !prefixSet) {
		return config.NetworkChange{}, false, fmt.Errorf("%w: static addressing requires address and prefix", config.ErrNetworkInvalid)
	}
	return change, true, nil
}

func (h *Handler) networkSettingsToAPI(ctx context.Context, st config.NetworkStatus) (*apiv1.NetworkSettings, error) {
	out := &apiv1.NetworkSettings{
		Backend:         apiv1.NetworkBackend(st.Backend),
		Editable:        st.Editable,
		Interfaces:      make([]apiv1.NetworkInterface, 0, len(st.Interfaces)),
		AllowAllSources: false,
		ListenPort:      8008,
		Certificate: apiv1.TLSCertificateInfo{
			Kind:          apiv1.TLSCertificateKindSelfSigned,
			NotAfter:      time.Now().UTC().Add(10 * 365 * 24 * time.Hour),
			DaysRemaining: 3650,
		},
		LetsEncrypt: apiv1.LetsEncryptStatus{},
	}
	if st.ReadOnlyReason != "" {
		out.ReadOnlyReason = apiv1.NewOptString(st.ReadOnlyReason)
	}
	for _, iface := range st.Interfaces {
		out.Interfaces = append(out.Interfaces, ifaceToAPI(iface))
	}
	if st.Pending != nil {
		out.Pending = apiv1.NewOptNetworkPending(apiv1.NetworkPending{
			Interface:        st.Pending.Interface,
			ExpiresAt:        st.Pending.ExpiresAt,
			RemainingSeconds: st.Pending.RemainingSeconds,
		})
	}
	if h.HTTPS != nil {
		cert, err := h.HTTPS.Certificate()
		if err != nil {
			return nil, fmt.Errorf("reading TLS certificate: %w", err)
		}
		out.Certificate = certViewToAPI(cert)
		out.AllowAllSources = h.HTTPS.AllowAllSources()
		out.ListenPort = h.HTTPS.ListenPort()
		if h.HTTPS.ListenPortRestartRequired() {
			out.ListenPortRestartRequired = apiv1.NewOptBool(true)
		}
	}
	if h.ACME != nil {
		st, err := h.ACME.Status(ctx)
		if err != nil {
			return nil, fmt.Errorf("reading Let's Encrypt status: %w", err)
		}
		out.LetsEncrypt = letsEncryptStatusToAPI(st)
	}
	return out, nil
}

func ifaceToAPI(iface config.Iface) apiv1.NetworkInterface {
	out := apiv1.NetworkInterface{
		Name:   iface.Name,
		Method: apiv1.NetworkAddressMethod(iface.Method),
		State:  apiv1.NetworkInterfaceState(iface.State),
		DNS:    iface.DNS,
	}
	if iface.MAC != "" {
		out.MAC = apiv1.NewOptString(iface.MAC)
	}
	if iface.Address != "" {
		out.Address = apiv1.NewOptString(iface.Address)
	}
	if iface.Prefix > 0 {
		out.Prefix = apiv1.NewOptInt(iface.Prefix)
	}
	if iface.Gateway != "" {
		out.Gateway = apiv1.NewOptString(iface.Gateway)
	}
	return out
}

func certViewToAPI(v TLSCertView) apiv1.TLSCertificateInfo {
	days := int(time.Until(v.NotAfter).Hours() / 24)
	kind := apiv1.TLSCertificateKindSelfSigned
	if v.Kind == acme.KindLetsEncrypt {
		kind = apiv1.TLSCertificateKindLetsEncrypt
	}
	out := apiv1.TLSCertificateInfo{
		Kind:          kind,
		NotAfter:      v.NotAfter.UTC(),
		DaysRemaining: days,
	}
	if v.Domain != "" {
		out.Domain = apiv1.NewOptString(v.Domain)
	}
	return out
}

func letsEncryptStatusToAPI(st acme.Status) apiv1.LetsEncryptStatus {
	out := apiv1.LetsEncryptStatus{
		Configured: st.Configured,
		Enabled:    st.Enabled,
	}
	if st.Domain != "" {
		out.Domain = apiv1.NewOptString(st.Domain)
	}
	if st.Provider != "" {
		out.Provider = apiv1.NewOptDNS01Provider(apiv1.DNS01Provider(st.Provider))
	}
	if st.HasSecret {
		out.HasSecret = apiv1.NewOptBool(true)
	}
	if st.LastError != "" {
		out.LastError = apiv1.NewOptString(st.LastError)
	}
	return out
}

func mapACMEError(err error) error {
	if errors.Is(err, acme.ErrInvalidSetup) {
		return &apiError{code: "network_invalid_input", statusCode: 400, message: err.Error()}
	}
	return fmt.Errorf("configuring Let's Encrypt: %w", err)
}
