//go:build l3

package api_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ogen-go/ogen/ogenerrors"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
)

func TestL3UnreachableAddressReverts(t *testing.T) {
	if os.Getenv("HOSERVA_LAB_ID") == "" {
		t.Skip("HOSERVA_LAB_ID not set — this test only runs inside its own L3 VM")
	}
	socket := os.Getenv("HOSERVA_L3_SOCKET")
	if socket == "" {
		t.Fatal("HOSERVA_L3_SOCKET is required")
	}
	tcpBase := os.Getenv("HOSERVA_L3_TCP")
	if tcpBase == "" {
		tcpBase = "https://127.0.0.1:18008"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	client, err := l3APIClient(socket)
	if err != nil {
		t.Fatalf("api client: %v", err)
	}

	st, err := client.GetNetworkSettings(ctx)
	if err != nil {
		t.Fatalf("GetNetworkSettings: %v", err)
	}
	reason := ""
	if v, ok := st.ReadOnlyReason.Get(); ok {
		reason = v
	}
	fmt.Printf("l3-network: backend=%s editable=%v reason=%q\n", st.Backend, st.Editable, reason)
	if st.Backend != apiv1.NetworkBackendIfupdown || !st.Editable {
		t.Fatalf("L3 guest backend is %s (editable=%v): Q75 expected ifupdown — update docs/internal/13-open-questions.md Q75", st.Backend, st.Editable)
	}
	if len(st.Interfaces) == 0 {
		t.Fatal("no interfaces reported")
	}
	iface := st.Interfaces[0]
	for _, candidate := range st.Interfaces {
		if candidate.State == apiv1.NetworkInterfaceStateUp && candidate.Address.IsSet() {
			iface = candidate
			break
		}
	}
	original, ok := iface.Address.Get()
	if !ok || original == "" {
		t.Fatalf("interface %s has no address", iface.Name)
	}
	prefix := 0
	if v, ok := iface.Prefix.Get(); ok {
		prefix = v
	}
	fmt.Printf("l3-network: using %s original=%s/%d\n", iface.Name, original, prefix)

	if err := l3Reach(ctx, original, listenPortFromBase(tcpBase)); err != nil {
		t.Fatalf("UI was not reachable on the original address before apply: %v", err)
	}

	// Confirm a DHCP apply first so revert restores a managed file rather
	// than deleting the only stanza (cloud-init's file is moved aside for
	// this test so Hoserva's one interfaces.d file is the live definition).
	dhcp := &apiv1.ApplyNetworkSettingsRequest{}
	dhcp.SetInterface(apiv1.NewOptString(iface.Name))
	dhcp.SetMethod(apiv1.NewOptNetworkAddressMethod(apiv1.NetworkAddressMethodDhcp))
	if _, err := client.ApplyNetworkSettings(ctx, dhcp); err != nil {
		t.Fatalf("seed DHCP apply: %v", err)
	}
	if _, err := client.ConfirmNetworkSettings(ctx); err != nil {
		t.Fatalf("seed DHCP confirm: %v", err)
	}
	if err := l3Reach(ctx, original, listenPortFromBase(tcpBase)); err != nil {
		t.Fatalf("UI was not reachable after seeding DHCP: %v", err)
	}

	req := &apiv1.ApplyNetworkSettingsRequest{}
	req.SetInterface(apiv1.NewOptString(iface.Name))
	req.SetMethod(apiv1.NewOptNetworkAddressMethod(apiv1.NetworkAddressMethodStatic))
	req.SetAddress(apiv1.NewOptString("192.0.2.1"))
	req.SetPrefix(apiv1.NewOptInt(24))
	applied, err := client.ApplyNetworkSettings(ctx, req)
	if err != nil {
		t.Fatalf("ApplyNetworkSettings: %v", err)
	}
	if !applied.Pending.IsSet() {
		t.Fatal("apply did not start a confirm-or-revert window")
	}

	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		got, err := client.GetNetworkSettings(ctx)
		if err != nil {
			t.Fatalf("polling GetNetworkSettings: %v", err)
		}
		if !got.Pending.IsSet() {
			if err := l3Reach(ctx, original, listenPortFromBase(tcpBase)); err != nil {
				t.Fatalf("UI was not reachable on the original address after revert: %v", err)
			}
			fmt.Println("l3-network: unreachable static address reverted; UI reachable again")
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatal("pending confirm-or-revert did not expire and restore within 90s")
}

type unixBearer struct{}

func (unixBearer) ApiToken(context.Context, apiv1.OperationName) (apiv1.ApiToken, error) {
	return apiv1.ApiToken{Token: strings.TrimPrefix(api.UnixSocketCredentialValue, "Bearer ")}, nil
}

func (unixBearer) SessionCookie(context.Context, apiv1.OperationName) (apiv1.SessionCookie, error) {
	return apiv1.SessionCookie{}, ogenerrors.ErrSkipClientSecurity
}

func l3APIClient(socket string) (*apiv1.Client, error) {
	httpClient := &http.Client{
		Timeout: 90 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
	}
	return apiv1.NewClient("http://unix/api/v1", unixBearer{}, apiv1.WithClient(httpClient))
}

func listenPortFromBase(tcpBase string) string {
	_, port, err := net.SplitHostPort(strings.TrimPrefix(strings.TrimPrefix(tcpBase, "https://"), "http://"))
	if err != nil {
		return "18008"
	}
	return port
}

func l3Reach(ctx context.Context, ip, port string) error {
	url := fmt.Sprintf("https://%s/api/v1/setup/status", net.JoinHostPort(ip, port))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // L3 guest, self-signed
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 500 {
		return fmt.Errorf("status %s", resp.Status)
	}
	return nil
}
