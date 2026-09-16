// Package api implements the server interfaces ogen generates from
// api/openapi.yaml (D18). Handler below is a placeholder — #19 and #22 wire
// it to the job system, store and auth middleware — but it already
// implements every method apiv1.Handler declares, deliberately not by
// embedding apiv1.UnimplementedHandler: an explicit method set is what
// makes the compile-time assertion below mean something. Deleting a method,
// or changing its signature so it no longer matches the spec, is a build
// failure, not a test failure — confirmed during development by removing
// ListJobs and seeing `go build ./...` fail on the assertion, then
// restoring it; that experiment isn't kept as a standing broken build.
package api

import (
	"context"
	"errors"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

// Handler implements apiv1.Handler.
type Handler struct{}

var _ apiv1.Handler = (*Handler)(nil)

var errNotImplemented = errors.New("not implemented")

func (Handler) ListJobs(ctx context.Context, params apiv1.ListJobsParams) (*apiv1.ListJobsOK, error) {
	return nil, errNotImplemented
}

func (Handler) GetJob(ctx context.Context, params apiv1.GetJobParams) (*apiv1.Job, error) {
	return nil, errNotImplemented
}

func (Handler) CancelJob(ctx context.Context, params apiv1.CancelJobParams) (*apiv1.Job, error) {
	return nil, errNotImplemented
}

func (Handler) ResumeJob(ctx context.Context, params apiv1.ResumeJobParams) (*apiv1.Job, error) {
	return nil, errNotImplemented
}

func (Handler) GetJobLog(ctx context.Context, params apiv1.GetJobLogParams) (apiv1.GetJobLogOK, error) {
	return apiv1.GetJobLogOK{}, errNotImplemented
}

// NewError maps an internal error to the spec's shared Error schema
// (doc 01 §5). #19 replaces this with real error classification; every
// error is reported as an opaque 500 for now, which is honest given there
// is no real logic yet to classify.
func (Handler) NewError(ctx context.Context, err error) *apiv1.ErrorStatusCode {
	return &apiv1.ErrorStatusCode{
		StatusCode: 500,
		Response: apiv1.Error{
			Code:    "internal",
			Message: err.Error(),
		},
	}
}
