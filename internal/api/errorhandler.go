package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-faster/jx"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

// WriteDecodeError is the generated server's own ErrorHandler
// (apiv1.WithErrorHandler) for both listeners (cmd/hoservad) — it runs for
// every error that happens *before* a Handler method is ever reached: a
// body that fails to decode, a bad content type, or an oversized body
// (http.MaxBytesError, from the MaxBytesHandler cmd/hoservad wraps both
// muxes in). A failed security check is not one of these: the generated
// server always routes that through Handler.NewError instead (every
// handleXRequest method calls s.h.NewError for a security failure, never
// s.cfg.ErrorHandler), so mapAuthError's own
// ogenerrors.ErrSecurityRequirementIsNotSatisfied case is what actually
// classifies it. Without this func, decode/size errors fell through to
// ogenerrors.DefaultErrorHandler's own {"error_message":"..."} shape —
// never the spec's {code,message} Error schema every Handler-returned
// error already renders through Handler.NewError — so a client saw one
// error shape for a body that never reached a handler and a different
// one for every other failure. Rendering both through writeAPIError
// closes that gap.
func WriteDecodeError(ctx context.Context, w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := classifyDecodeError(err)
	writeAPIError(w, status, apiv1.Error{Code: code, Message: message})
}

// classifyDecodeError maps an error the generated server produces before
// ever calling a Handler method to the spec's Error shape. A body over
// the MaxBytesHandler limit is checked first and explicitly — it is also
// a decode failure by the time the generated decoder's io.ReadAll call
// sees it, but the whole point of a size limit is to give the client a
// specific, actionable 413, not a generic 400.
func classifyDecodeError(err error) (status int, code, message string) {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		return http.StatusRequestEntityTooLarge, "request_too_large", "the request body is too large"
	}
	return http.StatusBadRequest, "bad_request", "the request could not be decoded"
}

// writeAPIError writes e as the spec's Error schema, using the generated
// type's own jx encoder rather than encoding/json, since Error has no
// exported struct tags of its own to marshal with.
func writeAPIError(w http.ResponseWriter, status int, e apiv1.Error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := new(jx.Encoder)
	e.Encode(enc)
	_, _ = enc.WriteTo(w)
}
