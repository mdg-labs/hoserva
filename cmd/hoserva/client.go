package main

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/ogen-go/ogen/ogenerrors"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
)

const defaultSocket = "/run/hoserva/hoserva.sock"

type unixSecurity struct{}

func (unixSecurity) ApiToken(_ context.Context, _ apiv1.OperationName) (apiv1.ApiToken, error) {
	return apiv1.ApiToken{Token: strings.TrimPrefix(api.UnixSocketCredentialValue, "Bearer ")}, nil
}

func (unixSecurity) SessionCookie(_ context.Context, _ apiv1.OperationName) (apiv1.SessionCookie, error) {
	return apiv1.SessionCookie{}, ogenerrors.ErrSkipClientSecurity
}

func newAPIClient(socketPath string) (*apiv1.Client, error) {
	if socketPath == "" {
		socketPath = defaultSocket
	}
	httpClient := &http.Client{
		Timeout: 5 * time.Minute,
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
		apiv1.WithClient(httpClient),
	)
}
