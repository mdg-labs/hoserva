package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRootCmdHasRemoteFlags(t *testing.T) {
	root := rootCmd()
	for _, name := range []string{"host", "port", "token", "insecure-skip-tls-verify", "tls-fingerprint"} {
		if root.PersistentFlags().Lookup(name) == nil {
			t.Errorf("root command has no --%s persistent flag", name)
		}
	}
}

func TestRootCmdHasTokenCommand(t *testing.T) {
	root := rootCmd()
	token, _, err := root.Find([]string{"token"})
	if err != nil {
		t.Fatalf("find token: %v", err)
	}
	for _, name := range []string{"create", "list", "revoke"} {
		if _, _, err := token.Find([]string{name}); err != nil {
			t.Fatalf("find token %s: %v", name, err)
		}
	}
}

// TestNewRemoteAPIClientRequiresToken is #50's own acceptance criterion:
// the remote CLI authenticates with a personal API token (Q43), so --host
// with no token at all must fail clearly rather than silently connecting
// unauthenticated.
func TestNewRemoteAPIClientRequiresToken(t *testing.T) {
	if _, err := newRemoteAPIClient("hoserva.example", defaultRemotePort, "", false, ""); err == nil {
		t.Error("newRemoteAPIClient with no token should fail")
	} else if !strings.Contains(err.Error(), "--token") {
		t.Errorf("newRemoteAPIClient error = %v, want it to mention --token", err)
	}
}

func TestNewRemoteAPIClientBuildsHTTPSURL(t *testing.T) {
	c, err := newRemoteAPIClient("hoserva.example", 8008, "hspat_sometoken", false, "")
	if err != nil {
		t.Fatalf("newRemoteAPIClient: %v", err)
	}
	if c == nil {
		t.Fatal("expected a non-nil client")
	}
}

// TestRootCmdHostRequiresToken exercises newAPIClient's own dispatch
// through the CLI itself: --host with no --token must surface the same
// clear error at the command level, not just from newRemoteAPIClient in
// isolation.
func TestRootCmdHostRequiresToken(t *testing.T) {
	restoreRemoteFlags(t)
	t.Setenv("HOSERVA_TOKEN", "")
	root := rootCmd()
	root.SetArgs([]string{"--host", "hoserva.example", "status"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "--token") {
		t.Errorf("--host with no token = %v, want an error mentioning --token", err)
	}
}

func restoreRemoteFlags(t *testing.T) {
	t.Helper()
	oldSocketPath, oldHost := socketPath, remoteHost
	oldPort, oldToken := remotePort, remoteToken
	oldInsecure, oldFingerprint := insecureSkipTLSVerify, tlsFingerprint
	t.Cleanup(func() {
		socketPath, remoteHost = oldSocketPath, oldHost
		remotePort, remoteToken = oldPort, oldToken
		insecureSkipTLSVerify, tlsFingerprint = oldInsecure, oldFingerprint
	})
}

// pinnedServer is a TLS server with its own self-signed certificate, which
// no CA vouches for and which names neither 127.0.0.1 nor any host, like
// hoservad's default certificate seen from a LAN address.
type pinnedServer struct {
	host        string
	port        int
	fingerprint string
	mu          sync.Mutex
	requests    []string
}

func (s *pinnedServer) authorizations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func newPinnedServer(t *testing.T) *pinnedServer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "hoserva"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, &x509.Certificate{}, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	sum := sha256.Sum256(der)
	ps := &pinnedServer{fingerprint: "sha256:" + hex.EncodeToString(sum[:])}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ps.mu.Lock()
		ps.requests = append(ps.requests, r.Header.Get("Authorization"))
		ps.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"disks":[]}`))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	host, portText, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split address: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	ps.host, ps.port = host, port
	return ps
}

func TestRemoteClientPinnedToServerCertificateSucceeds(t *testing.T) {
	a := newPinnedServer(t)
	c, err := newRemoteAPIClient(a.host, a.port, "hspat_sometoken", false, a.fingerprint)
	if err != nil {
		t.Fatalf("newRemoteAPIClient: %v", err)
	}
	if _, err := c.ListDisks(context.Background()); err != nil {
		t.Fatalf("ListDisks with the matching pin: %v", err)
	}
	if got := a.authorizations(); len(got) != 1 || got[0] != "Bearer hspat_sometoken" {
		t.Errorf("server saw Authorization headers %q, want one bearer token", got)
	}
}

func TestRemoteClientPinnedToOtherCertificateSendsNothing(t *testing.T) {
	a, b := newPinnedServer(t), newPinnedServer(t)
	c, err := newRemoteAPIClient(a.host, a.port, "hspat_sometoken", false, b.fingerprint)
	if err != nil {
		t.Fatalf("newRemoteAPIClient: %v", err)
	}
	_, err = c.ListDisks(context.Background())
	if err == nil {
		t.Fatal("ListDisks against a certificate other than the pinned one succeeded")
	}
	if !strings.Contains(err.Error(), "fingerprint") {
		t.Errorf("error = %v, want it to mention the fingerprint", err)
	}
	if got := a.authorizations(); len(got) != 0 {
		t.Errorf("server received requests %q despite the pin mismatch", got)
	}
}

func TestRemoteClientWithoutPinStillVerifiesCertificate(t *testing.T) {
	a := newPinnedServer(t)
	c, err := newRemoteAPIClient(a.host, a.port, "hspat_sometoken", false, "")
	if err != nil {
		t.Fatalf("newRemoteAPIClient: %v", err)
	}
	if _, err := c.ListDisks(context.Background()); err == nil {
		t.Fatal("ListDisks against an untrusted certificate succeeded without a pin")
	}
	if got := a.authorizations(); len(got) != 0 {
		t.Errorf("server received requests %q from an unverified connection", got)
	}
}

func TestParseTLSFingerprintAcceptsCommonForms(t *testing.T) {
	const hexForm = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	colons := make([]string, 0, 32)
	for i := 0; i < len(hexForm); i += 2 {
		colons = append(colons, strings.ToUpper(hexForm[i:i+2]))
	}
	want, err := hex.DecodeString(hexForm)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{
		"sha256:" + hexForm,
		hexForm,
		"SHA256:" + strings.Join(colons, ":"),
		strings.Join(colons, ":"),
		"  sha256:" + hexForm + "\n",
	} {
		got, err := parseTLSFingerprint(in)
		if err != nil {
			t.Errorf("parseTLSFingerprint(%q): %v", in, err)
			continue
		}
		if string(got[:]) != string(want) {
			t.Errorf("parseTLSFingerprint(%q) = %x, want %x", in, got, want)
		}
	}
}

func TestMalformedFingerprintIsRefusedBeforeAnyConnection(t *testing.T) {
	a := newPinnedServer(t)
	for _, bad := range []string{
		"sha256:",
		"sha256:zz",
		"sha256:00112233",
		"sha1:" + strings.Repeat("00", 32),
		strings.Repeat("00", 33),
		"sha256:" + strings.Repeat("0g", 32),
	} {
		c, err := newRemoteAPIClient(a.host, a.port, "hspat_sometoken", false, bad)
		if err == nil {
			t.Errorf("fingerprint %q accepted", bad)
			continue
		}
		if c != nil || !strings.Contains(err.Error(), "--tls-fingerprint") {
			t.Errorf("fingerprint %q: client %v, error %v, want no client and an error naming --tls-fingerprint", bad, c, err)
		}
	}
	if got := a.authorizations(); len(got) != 0 {
		t.Errorf("server received requests %q", got)
	}
}

func TestPinAndInsecureSkipVerifyAreMutuallyExclusive(t *testing.T) {
	a := newPinnedServer(t)
	if _, err := newRemoteAPIClient(a.host, a.port, "hspat_sometoken", true, a.fingerprint); err == nil {
		t.Error("--tls-fingerprint together with --insecure-skip-tls-verify was accepted")
	}
}

// TestRootCmdHostGoesThroughThePin runs a real command with --host, so the
// check proves the flag reaches newAPIClient -> newRemoteAPIClientWith.
func TestRootCmdHostGoesThroughThePin(t *testing.T) {
	restoreRemoteFlags(t)
	a, b := newPinnedServer(t), newPinnedServer(t)
	t.Setenv("HOSERVA_TOKEN", "hspat_sometoken")
	t.Setenv("HOSERVA_TLS_FINGERPRINT", "")
	run := func(args ...string) error {
		root := rootCmd()
		root.SetArgs(append([]string{"--host", a.host, "--port", strconv.Itoa(a.port)}, args...))
		root.SetOut(&strings.Builder{})
		return root.Execute()
	}

	if err := run("--tls-fingerprint", b.fingerprint, "disk", "list"); err == nil {
		t.Error("disk list with a fingerprint of another certificate succeeded")
	}
	if got := a.authorizations(); len(got) != 0 {
		t.Fatalf("server received requests %q despite the pin mismatch", got)
	}
	if err := run("--tls-fingerprint", a.fingerprint, "disk", "list"); err != nil {
		t.Errorf("disk list with the matching fingerprint: %v", err)
	}
	if err := run("--tls-fingerprint", a.fingerprint, "--insecure-skip-tls-verify", "disk", "list"); err == nil {
		t.Error("--tls-fingerprint with --insecure-skip-tls-verify was accepted")
	}
	if err := run("--tls-fingerprint", "nonsense", "disk", "list"); err == nil {
		t.Error("a malformed --tls-fingerprint was accepted")
	}
}

func TestFingerprintDefaultsFromEnvironment(t *testing.T) {
	restoreRemoteFlags(t)
	a, b := newPinnedServer(t), newPinnedServer(t)
	t.Setenv("HOSERVA_TOKEN", "hspat_sometoken")
	t.Setenv("HOSERVA_TLS_FINGERPRINT", b.fingerprint)
	root := rootCmd()
	root.SetArgs([]string{"--host", a.host, "--port", strconv.Itoa(a.port), "disk", "list"})
	root.SetOut(&strings.Builder{})
	if err := root.Execute(); err == nil {
		t.Error("disk list succeeded although HOSERVA_TLS_FINGERPRINT pins another certificate")
	}
	if got := a.authorizations(); len(got) != 0 {
		t.Errorf("server received requests %q despite the pin mismatch", got)
	}
}
