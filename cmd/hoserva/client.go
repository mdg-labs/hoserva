package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/ogen-go/ogen/ogenerrors"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
)

const (
	defaultSocket     = "/run/hoserva/hoserva.sock"
	defaultRemotePort = 8008 // Q9: the one TCP port hoservad listens on.
	requestTimeout    = 5 * time.Minute
)

// unixSecurity is the Unix-socket transport's own client security (Q44):
// the kernel already vouches for the caller's identity via SO_PEERCRED, so
// this attaches only the fixed credential api.TrustedSecurityHandler
// ignores the value of — present purely so ogen's generated per-scheme
// check has something to call HandleApiToken with at all.
type unixSecurity struct{}

func (unixSecurity) ApiToken(_ context.Context, _ apiv1.OperationName) (apiv1.ApiToken, error) {
	return apiv1.ApiToken{Token: strings.TrimPrefix(api.UnixSocketCredentialValue, "Bearer ")}, nil
}

func (unixSecurity) SessionCookie(_ context.Context, _ apiv1.OperationName) (apiv1.SessionCookie, error) {
	return apiv1.SessionCookie{}, ogenerrors.ErrSkipClientSecurity
}

// remoteSecurity is the TCP transport's own client security (Q43, #50): a
// personal API token, carried as a bearer credential — the remote CLI
// never has a session cookie of its own.
type remoteSecurity struct {
	token string
}

func (s remoteSecurity) ApiToken(_ context.Context, _ apiv1.OperationName) (apiv1.ApiToken, error) {
	return apiv1.ApiToken{Token: s.token}, nil
}

func (remoteSecurity) SessionCookie(_ context.Context, _ apiv1.OperationName) (apiv1.SessionCookie, error) {
	return apiv1.SessionCookie{}, ogenerrors.ErrSkipClientSecurity
}

// newAPIClient builds the generated client for whichever transport the
// caller selected: a remote host over TLS (Q43, doc 01 §5) once --host is
// set, the local Unix socket otherwise (doc 01 §3's default, no network
// and no token needed on a local root shell).
func newAPIClient() (*apiv1.Client, error) {
	return newAPIClientWith(requestTimeout, nil)
}

// newStreamingAPIClient is newAPIClient for one long-lived response body:
// no overall timeout (http.Client.Timeout also bounds reading the body, so
// any finite value would cut a `--follow` stream off), and a successful
// response's body copied to sink as it arrives rather than buffered. The
// generated decoder of a text/plain body reads it all into memory first,
// which for an open-ended log would neither print anything nor ever end.
func newStreamingAPIClient(sink io.Writer) (*apiv1.Client, error) {
	return newAPIClientWith(0, func(inner *http.Client) apiHTTPClient {
		return streamingClient{inner: inner, sink: sink}
	})
}

// apiHTTPClient is what apiv1.WithClient accepts.
type apiHTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

func newAPIClientWith(timeout time.Duration, wrap func(*http.Client) apiHTTPClient) (*apiv1.Client, error) {
	if remoteHost != "" {
		return newRemoteAPIClientWith(remoteHost, remotePort, remoteToken, insecureSkipTLSVerify, timeout, wrap)
	}
	return newLocalAPIClientWith(socketPath, timeout, wrap)
}

func newLocalAPIClientWith(socketPath string, timeout time.Duration, wrap func(*http.Client) apiHTTPClient) (*apiv1.Client, error) {
	if socketPath == "" {
		socketPath = defaultSocket
	}
	httpClient := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
	}
	return apiv1.NewClient(
		"http://unix/api/v1",
		unixSecurity{},
		withHTTPClient(httpClient, wrap),
	)
}

func withHTTPClient(httpClient *http.Client, wrap func(*http.Client) apiHTTPClient) apiv1.ClientOption {
	if wrap == nil {
		return apiv1.WithClient(httpClient)
	}
	return apiv1.WithClient(wrap(httpClient))
}

// streamingClient hands a 200 response's body to the generated decoder as
// an already-drained one: every byte goes to sink while the decoder reads,
// and the decoder itself sees an empty body. Error responses are left
// untouched so the generated client still decodes the server's error.
type streamingClient struct {
	inner *http.Client
	sink  io.Writer
}

func (c streamingClient) Do(req *http.Request) (*http.Response, error) {
	resp, err := c.inner.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		return resp, err
	}
	resp.Body = &drainedBody{src: resp.Body, sink: c.sink}
	return resp, nil
}

type drainedBody struct {
	src  io.ReadCloser
	sink io.Writer
	done bool
}

func (b *drainedBody) Read([]byte) (int, error) {
	if b.done {
		return 0, io.EOF
	}
	b.done = true
	if _, err := io.Copy(b.sink, b.src); err != nil {
		return 0, err
	}
	return 0, io.EOF
}

func (b *drainedBody) Close() error { return b.src.Close() }

// newRemoteAPIClient builds a TLS-only client against host:port (Q9's
// :8008 by default), authenticating with a personal API token (Q43).
// token is required — the remote CLI has no other credential to offer.
// insecureSkipVerify exists only for hoservad's own default, self-signed
// certificate (Q9): it is never the default, and every use is the
// caller's own explicit --insecure-skip-tls-verify.
func newRemoteAPIClient(host string, port int, token string, insecureSkipVerify bool) (*apiv1.Client, error) {
	return newRemoteAPIClientWith(host, port, token, insecureSkipVerify, requestTimeout, nil)
}

func newRemoteAPIClientWith(host string, port int, token string, insecureSkipVerify bool, timeout time.Duration, wrap func(*http.Client) apiHTTPClient) (*apiv1.Client, error) {
	if token == "" {
		return nil, fmt.Errorf("--host requires --token (or the HOSERVA_TOKEN environment variable) — the remote CLI authenticates with a personal API token (Q43)")
	}
	httpClient := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: insecureSkipVerify}, //nolint:gosec // explicit, opt-in --insecure-skip-tls-verify only
		},
	}
	baseURL := fmt.Sprintf("https://%s/api/v1", net.JoinHostPort(host, fmt.Sprint(port)))
	return apiv1.NewClient(
		baseURL,
		remoteSecurity{token: token},
		withHTTPClient(httpClient, wrap),
	)
}
