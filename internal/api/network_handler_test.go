package api_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/acme"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/notify"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

type fakeHTTPS struct {
	allowAll bool
	port     int
	restart  bool
	notAfter time.Time
	kind     string
	regenErr error
}

func (f *fakeHTTPS) Certificate() (api.TLSCertView, error) {
	kind := f.kind
	if kind == "" {
		kind = "self_signed"
	}
	return api.TLSCertView{Kind: kind, NotAfter: f.notAfter}, nil
}

func (f *fakeHTTPS) Regenerate(context.Context) (api.TLSCertView, error) {
	f.notAfter = time.Date(2036, 1, 1, 0, 0, 0, 0, time.UTC)
	view, err := f.Certificate()
	if err != nil {
		return view, err
	}
	return view, f.regenErr
}

func (f *fakeHTTPS) AllowAllSources() bool { return f.allowAll }
func (f *fakeHTTPS) SetAllowAllSources(v bool) error {
	f.allowAll = v
	return nil
}
func (f *fakeHTTPS) ListenPort() int { return f.port }
func (f *fakeHTTPS) SetListenPort(port int) error {
	f.port = port
	f.restart = port != 8008
	return nil
}
func (f *fakeHTTPS) ListenPortRestartRequired() bool { return f.restart }

func newNetworkHandler(t *testing.T) (*api.Handler, *config.NetworkService, *fakeHTTPS) {
	t.Helper()
	svc, _ := newNetworkServiceForAPI(t)
	https := &fakeHTTPS{port: 8008, notAfter: time.Date(2036, 9, 20, 0, 0, 0, 0, time.UTC)}
	return &api.Handler{Network: svc, HTTPS: https}, svc, https
}

func newNetworkServiceForAPI(t *testing.T) (*config.NetworkService, *config.MemoryRunner) {
	t.Helper()
	runner := &config.MemoryRunner{}
	svc := &config.NetworkService{
		Generator: config.NewGenerator(t.TempDir()),
		Detector:  config.MemoryDetector{Backend: config.BackendIfupdown},
		Runner:    runner,
		Links: config.MemoryLinks{Ifaces: []config.Iface{{
			Name: "eth0", Method: config.MethodDHCP, State: config.IfaceUp,
			Address: "10.0.2.15", Prefix: 24,
		}}},
		StateDir: t.TempDir(),
		Window:   time.Hour,
		Now:      func() time.Time { return time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC) },
	}
	t.Cleanup(svc.Close)
	return svc, runner
}

func TestGetNetworkSettingsIfupdown(t *testing.T) {
	h, _, _ := newNetworkHandler(t)
	got, err := h.GetNetworkSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Backend != apiv1.NetworkBackendIfupdown || !got.Editable {
		t.Fatalf("got %+v", got)
	}
	if len(got.Interfaces) != 1 || got.Interfaces[0].Name != "eth0" {
		t.Fatalf("interfaces = %+v", got.Interfaces)
	}
}

func TestApplyNetworkSettingsStartsPending(t *testing.T) {
	h, _, _ := newNetworkHandler(t)
	req := &apiv1.ApplyNetworkSettingsRequest{}
	req.SetInterface(apiv1.NewOptString("eth0"))
	req.SetMethod(apiv1.NewOptNetworkAddressMethod(apiv1.NetworkAddressMethodStatic))
	req.SetAddress(apiv1.NewOptString("192.0.2.1"))
	req.SetPrefix(apiv1.NewOptInt(24))
	got, err := h.ApplyNetworkSettings(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Pending.IsSet() {
		t.Fatal("expected pending confirm window")
	}
}

func TestApplyNetworkSettingsReadOnly(t *testing.T) {
	svc, _ := newNetworkServiceForAPI(t)
	svc.Detector = config.MemoryDetector{Backend: config.BackendNetworkManager, Reason: "NetworkManager"}
	h := &api.Handler{Network: svc, HTTPS: &fakeHTTPS{port: 8008, notAfter: time.Now().Add(time.Hour)}}
	req := &apiv1.ApplyNetworkSettingsRequest{}
	req.SetInterface(apiv1.NewOptString("eth0"))
	req.SetMethod(apiv1.NewOptNetworkAddressMethod(apiv1.NetworkAddressMethodDhcp))
	_, err := h.ApplyNetworkSettings(context.Background(), req)
	var ae interface{ Error() string }
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v", err)
	}
}

func TestApplyAllowAllSources(t *testing.T) {
	h, _, https := newNetworkHandler(t)
	req := &apiv1.ApplyNetworkSettingsRequest{}
	req.SetAllowAllSources(apiv1.NewOptBool(true))
	got, err := h.ApplyNetworkSettings(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !got.AllowAllSources || !https.allowAll {
		t.Fatal("allowAllSources should be true")
	}
}

func TestApplyNetworkSettings_RejectsAddressingWithoutMutatingHTTPS(t *testing.T) {
	h, _, https := newNetworkHandler(t)
	req := &apiv1.ApplyNetworkSettingsRequest{}
	req.SetAllowAllSources(apiv1.NewOptBool(true))
	req.SetInterface(apiv1.NewOptString("eth0"))
	req.SetMethod(apiv1.NewOptNetworkAddressMethod(apiv1.NetworkAddressMethodStatic))
	req.SetAddress(apiv1.NewOptString("not-an-ip"))
	req.SetPrefix(apiv1.NewOptInt(24))
	if _, err := h.ApplyNetworkSettings(context.Background(), req); err == nil {
		t.Fatal("expected invalid addressing to fail")
	}
	if https.allowAll {
		t.Fatal("rejected addressing must not leave allowAllSources enabled")
	}
}

func TestApplyNetworkSettings_ReadOnlyDoesNotKeepHTTPSChange(t *testing.T) {
	svc, _ := newNetworkServiceForAPI(t)
	svc.Detector = config.MemoryDetector{Backend: config.BackendNetworkManager, Reason: "NetworkManager"}
	https := &fakeHTTPS{port: 8008, notAfter: time.Now().Add(time.Hour)}
	h := &api.Handler{Network: svc, HTTPS: https}
	req := &apiv1.ApplyNetworkSettingsRequest{}
	req.SetAllowAllSources(apiv1.NewOptBool(true))
	req.SetInterface(apiv1.NewOptString("eth0"))
	req.SetMethod(apiv1.NewOptNetworkAddressMethod(apiv1.NetworkAddressMethodDhcp))
	if _, err := h.ApplyNetworkSettings(context.Background(), req); err == nil {
		t.Fatal("expected read-only error")
	}
	if https.allowAll {
		t.Fatal("read-only addressing must not leave allowAllSources enabled")
	}
}

func TestConfigureLetsEncrypt_InvalidDomainIs400(t *testing.T) {
	h, _, _ := newTestHandler(t)
	db := newACMEDB(t)
	h.ACME = &acme.Service{Store: acme.NewStore(db), Cipher: acme.FakeCipher{}}
	req := &apiv1.ConfigureLetsEncryptRequest{
		Domain:   "not a host",
		Provider: apiv1.DNS01ProviderCloudflare,
	}
	req.SetCloudflareAPIToken(apiv1.NewOptString("token"))
	_, err := h.ConfigureLetsEncrypt(context.Background(), req)
	status := apiError(t, h, err)
	if status.StatusCode != 400 || status.Response.Code != "network_invalid_input" {
		t.Fatalf("status = %d %s, want 400 network_invalid_input", status.StatusCode, status.Response.Code)
	}
}

func TestConfigureLetsEncrypt_StoreFailureIsOpaque500(t *testing.T) {
	h, _, _ := newTestHandler(t)
	db := newACMEDB(t)
	h.ACME = &acme.Service{Store: acme.NewStore(db), Cipher: acme.FakeCipher{}}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	req := &apiv1.ConfigureLetsEncryptRequest{
		Domain:   "nas.example.com",
		Provider: apiv1.DNS01ProviderCloudflare,
	}
	req.SetCloudflareAPIToken(apiv1.NewOptString("token"))
	_, err := h.ConfigureLetsEncrypt(context.Background(), req)
	status := apiError(t, h, err)
	if status.StatusCode != 500 || status.Response.Code != "internal" {
		t.Fatalf("status = %d %s, want 500 internal", status.StatusCode, status.Response.Code)
	}
	if status.Response.Message != "an internal error occurred" {
		t.Fatalf("message = %q, want opaque internal error", status.Response.Message)
	}
}

func newACMEDB(t *testing.T) *sql.DB {
	t.Helper()
	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading migrations: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "acme.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return db
}

func TestConfirmNetworkSettings(t *testing.T) {
	h, _, _ := newNetworkHandler(t)
	req := &apiv1.ApplyNetworkSettingsRequest{}
	req.SetInterface(apiv1.NewOptString("eth0"))
	req.SetMethod(apiv1.NewOptNetworkAddressMethod(apiv1.NetworkAddressMethodDhcp))
	if _, err := h.ApplyNetworkSettings(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	got, err := h.ConfirmNetworkSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Pending.IsSet() {
		t.Fatal("pending should be cleared")
	}
}

func TestRegenerateTLSCertificate_BackupLeftSucceedsAndWarns(t *testing.T) {
	nh, svc, _ := newNotifyTestHandler(t)
	h, _, https := newNetworkHandler(t)
	h.Notify = nh.Notify
	https.regenErr = fmt.Errorf("%w: remove hoserva.crt.bak: permission denied", acme.ErrBackupLeft)

	got, err := h.RegenerateTLSCertificate(context.Background())
	if err != nil {
		t.Fatalf("RegenerateTLSCertificate: %v", err)
	}
	if got.Certificate.Kind != apiv1.TLSCertificateKindSelfSigned {
		t.Fatalf("certificate kind = %q, want self_signed", got.Certificate.Kind)
	}

	groups, unread, err := svc.ListInbox(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if unread != 1 || len(groups) != 1 || groups[0].EventType != notify.EventCertificateRenewalFailed || len(groups[0].Alerts) != 1 {
		t.Fatalf("inbox = %+v (unread %d), want one certificate_renewal_failed alert", groups, unread)
	}
	alert := groups[0].Alerts[0]
	if alert.Title != "Certificate regenerated, backup file left behind" {
		t.Errorf("title = %q", alert.Title)
	}
	for _, want := range []string{"new self-signed certificate is in use", "could not be removed", "Remove it from the TLS directory", "hoserva.crt.bak: permission denied"} {
		if !strings.Contains(alert.Message, want) {
			t.Errorf("message %q lacks %q", alert.Message, want)
		}
	}
}

func TestRegenerateTLSCertificate_BackupLeftWithoutNotifierStillSucceeds(t *testing.T) {
	h, _, https := newNetworkHandler(t)
	https.regenErr = fmt.Errorf("%w: remove hoserva.key.bak: permission denied", acme.ErrBackupLeft)
	if _, err := h.RegenerateTLSCertificate(context.Background()); err != nil {
		t.Fatalf("RegenerateTLSCertificate: %v", err)
	}
}

func TestRegenerateTLSCertificate_CleanRegeneratePublishesNothing(t *testing.T) {
	nh, svc, _ := newNotifyTestHandler(t)
	h, _, _ := newNetworkHandler(t)
	h.Notify = nh.Notify
	if _, err := h.RegenerateTLSCertificate(context.Background()); err != nil {
		t.Fatalf("RegenerateTLSCertificate: %v", err)
	}
	if _, unread, err := svc.ListInbox(context.Background()); err != nil || unread != 0 {
		t.Fatalf("inbox unread = %d, err = %v, want none", unread, err)
	}
}

func TestRegenerateTLSCertificate_OtherErrorFailsAndPublishesNothing(t *testing.T) {
	nh, svc, _ := newNotifyTestHandler(t)
	h, _, https := newNetworkHandler(t)
	h.Notify = nh.Notify
	https.regenErr = errors.New("writing hoserva.crt: disk full")
	if _, err := h.RegenerateTLSCertificate(context.Background()); err == nil {
		t.Fatal("RegenerateTLSCertificate succeeded, want the regenerate error")
	}
	if _, unread, err := svc.ListInbox(context.Background()); err != nil || unread != 0 {
		t.Fatalf("inbox unread = %d, err = %v, want none", unread, err)
	}
}
