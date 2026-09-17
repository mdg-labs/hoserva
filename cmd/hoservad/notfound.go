package main

import (
	"net/http"
)

// apiNotFoundBody is the spec's own Error schema (doc 01 §5), written by
// hand rather than through apiv1.Error's generated marshaller: this
// handler runs for requests the generated server never routes at all —
// including the whole /api tree outside /api/v1 — so it has no generated
// response encoder of its own to call.
const apiNotFoundBody = `{"code":"not_found","message":"no such API route"}`

// jsonAPINotFoundHandler answers any unrecognized path under the API
// namespace with the spec's own Error shape (doc 01 §5), never plain
// text and never the SPA: a client asking for /api/v2/jobs or
// /api/v1/nope is asking the API for something that doesn't exist, not
// asking for a web UI page.
func jsonAPINotFoundHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(apiNotFoundBody))
}

// mountAPINotFoundRoutes registers jsonAPINotFoundHandler for every path
// under "/api" that isn't already claimed by a more specific pattern
// mux.Handle registered — net/http.ServeMux always prefers the longest
// matching pattern, so "/api/v1/" (registered separately, ahead of or
// behind this call — the order doesn't matter to ServeMux) still wins for
// anything actually under it. The exact "/api", "/api/" and apiPathPrefix
// itself (e.g. "/api/v1", with no trailing slash) are all registered
// explicitly, rather than relying on ServeMux's own subtree-redirect
// behaviour for an exact match one level short of a registered subtree:
// without apiPathPrefix's own exact registration, a bare "/api/v1"
// request fell through to that redirect — an HTML 301/307 to "/api/v1/",
// not this handler's JSON 404 — since nothing more specific matched it
// either.
func mountAPINotFoundRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api", jsonAPINotFoundHandler)
	mux.HandleFunc("/api/", jsonAPINotFoundHandler)
	mux.HandleFunc(apiPathPrefix, jsonAPINotFoundHandler)
}
