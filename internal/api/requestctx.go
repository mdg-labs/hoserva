package api

import "context"

type sourceAddrContextKey struct{}

// WithSourceAddr attaches the request's source address (host only, no
// port) to ctx — cmd/hoservad's HTTP middleware sets this from
// http.Request.RemoteAddr before calling into the generated server, so
// Login can rate-limit per source address (doc 01 §7) without needing the
// raw *http.Request itself (the generated Handler interface never hands
// one to a method — only ctx and typed parameters, D18).
func WithSourceAddr(ctx context.Context, addr string) context.Context {
	return context.WithValue(ctx, sourceAddrContextKey{}, addr)
}

// sourceAddrFromContext returns "" when no source address was attached —
// the Unix socket never calls Login as itself (TrustedSecurityHandler
// grants access before any password is ever checked), so this is not
// expected to matter there, but Login treats "" as "skip the per-address
// limiter", never as a crash.
func sourceAddrFromContext(ctx context.Context) string {
	addr, _ := ctx.Value(sourceAddrContextKey{}).(string)
	return addr
}
