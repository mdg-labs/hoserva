package api

import (
	"context"
	"errors"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

// SecurityHandler implements apiv1.SecurityHandler. Real session and token
// validation (Q43, Q44) is #22's job; every request is refused for now,
// which is the correct default for auth middleware that doesn't exist yet.
// #19 does not touch this: the job operations and the hand-written
// /api/v1/events writer (events.go) go through this same, still-refusing
// gate — events.go documents the analogous seam #22 wires for the one
// route ogen generates no security hook for at all.
type SecurityHandler struct{}

var _ apiv1.SecurityHandler = (*SecurityHandler)(nil)

var errUnauthenticated = errors.New("authentication not implemented")

func (SecurityHandler) HandleApiToken(ctx context.Context, operationName apiv1.OperationName, t apiv1.ApiToken) (context.Context, error) {
	return ctx, errUnauthenticated
}

func (SecurityHandler) HandleSessionCookie(ctx context.Context, operationName apiv1.OperationName, t apiv1.SessionCookie) (context.Context, error) {
	return ctx, errUnauthenticated
}
