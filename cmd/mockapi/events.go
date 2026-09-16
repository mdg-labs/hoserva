package main

import (
	"bufio"
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/api/gen/go/events"
	"github.com/mdg-labs/hoserva/web/fixtures"
)

// defaultKeepAlive paces the SSE comment lines ServeHTTP sends once a
// scenario's fixture events are exhausted, so a client (and any proxy or
// load balancer between it and mockapi, once #21's Vite dev proxy sits in
// front of this) sees the connection is still alive rather than timing it
// out. Tests set eventsHandler.keepAlive directly to something far shorter.
const defaultKeepAlive = 15 * time.Second

// eventsHandler serves /api/v1/events (doc 01 §5) for a scenario. ogen
// generates no server side for this operation (Q63, api/openapi.yaml's own
// comment on streamEvents), so this is hand-written — but it still frames
// only events.Event values, decoded from the same fixture the backend
// tests and web/fixtures' own fixtures_test.go validate, so a client
// reading this stream still reads it only through generated types (D18).
//
// The generated router gates every other operation on a credential
// (Authorization: Bearer … or a hoserva_session cookie); this handler is
// hand-written specifically because ogen has no server side for it, so it
// has to reproduce that same gate itself rather than getting it for free
// (handler.go's NewError documents the shared 401 both paths return). Until
// #21's Vite dev proxy exists, a bare browser request needs some credential
// attached — any hoserva_session cookie value is accepted, e.g. from the
// browser console: document.cookie = "hoserva_session=dev".
type eventsHandler struct {
	scenario  string
	frames    [][]byte
	keepAlive time.Duration
}

// newEventsHandler pre-decodes and re-encodes scenario's events fixture
// through events.Event once at startup, so a malformed fixture fails
// mockapi's startup rather than a client's first request.
func newEventsHandler(scenario string) (*eventsHandler, error) {
	raw, err := fixtures.EventsJSONL(scenario)
	if err != nil {
		return nil, fmt.Errorf("load events fixture: %w", err)
	}

	h := &eventsHandler{scenario: scenario, keepAlive: defaultKeepAlive}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev events.Event
		if err := ev.UnmarshalJSON(line); err != nil {
			return nil, fmt.Errorf("decode event: %w", err)
		}
		frame, err := ev.MarshalJSON()
		if err != nil {
			return nil, fmt.Errorf("re-encode event: %w", err)
		}
		h.frames = append(h.frames, frame)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan events fixture: %w", err)
	}

	return h, nil
}

// ServeHTTP replays the scenario's canned events once, then sends a
// periodic SSE comment as a keep-alive until the client disconnects — an
// unbounded stream (per the spec) with a finite, fixture-backed body, which
// is all a mock needs to give the UI something to render (doc 06 §8).
func (h *eventsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !hasCredential(r) {
		writeUnauthorized(w)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	// Only "data:" lines carry meaning here: the frame's own JSON already
	// names its variant via Event.event (api/openapi.yaml's discriminator),
	// and the generated reader (api/gen/go/events/reader.go) reads nothing
	// from SSE's "event:" field, so there is no second place for it to go.
	for _, frame := range h.frames {
		if _, err := fmt.Fprintf(w, "data: %s\n\n", frame); err != nil {
			return
		}
		flusher.Flush()
	}

	keepAlive := h.keepAlive
	if keepAlive <= 0 {
		keepAlive = defaultKeepAlive
	}
	ticker := time.NewTicker(keepAlive)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			// A ":"-prefixed line is an SSE comment: it reaches no
			// "message"/EventSource listener, only keeps the connection
			// from looking idle to a client or an intermediate proxy.
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// hasCredential reports whether r carries the same credential the generated
// router requires elsewhere (api/gen/go/oas_security_gen.go): an
// Authorization: Bearer … header, or a hoserva_session cookie, of any
// value. events.go's own handler has no generated security gate to lean on
// (Q63), so it reproduces the router's presence check by hand to stay
// consistent with every other operation rather than checking nothing.
func hasCredential(r *http.Request) bool {
	for _, v := range r.Header.Values("Authorization") {
		if scheme, _, ok := strings.Cut(v, " "); ok && strings.EqualFold(scheme, "Bearer") {
			return true
		}
	}
	if _, err := r.Cookie("hoserva_session"); err == nil {
		return true
	}
	return false
}

// writeUnauthorized reports the same 401 shape handler.go's NewError does,
// so a client sees one error contract whether it hit a generated operation
// or this hand-written one.
func writeUnauthorized(w http.ResponseWriter) {
	body, err := (&apiv1.Error{Code: unauthorizedCode, Message: unauthorizedMessage}).MarshalJSON()
	if err != nil {
		http.Error(w, unauthorizedMessage, http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write(body)
}
