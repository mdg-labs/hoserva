package main

import (
	"context"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

// securityHandler accepts any credential (doc 06 §8): the mock has no
// sessions or tokens to check against, and frontend work needs every
// screen reachable without first standing up real auth.
type securityHandler struct{}

var _ apiv1.SecurityHandler = securityHandler{}

func (securityHandler) HandleApiToken(ctx context.Context, _ apiv1.OperationName, _ apiv1.ApiToken) (context.Context, error) {
	return ctx, nil
}

func (securityHandler) HandleSessionCookie(ctx context.Context, _ apiv1.OperationName, _ apiv1.SessionCookie) (context.Context, error) {
	return ctx, nil
}
