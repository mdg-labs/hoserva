package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
)

// maxTestRequestBodyBytes mirrors cmd/hoservad's own maxRequestBodyBytes
// (64 KiB) — this test builds the same apiv1.NewServer + http.MaxBytesHandler
// + WithErrorHandler wiring cmd/hoservad uses, at the internal/api level,
// so it doesn't need the daemon's own TLS/Unix-socket listeners at all.
const maxTestRequestBodyBytes = 64 * 1024

// newTestAPIServer wires the generated server with the exact same options
// cmd/hoservad's buildTCPServer/buildUnixServer use — WithErrorHandler set
// to api.WriteDecodeError, and its own mux wrapped in http.MaxBytesHandler
// — so this test exercises the real request path a client's oversized
// body takes, not a hand-rolled stand-in for it.
func newTestAPIServer(t *testing.T, h *api.Handler, authSvc *api.AuthService) *httptest.Server {
	t.Helper()
	security := &api.SessionSecurityHandler{Auth: authSvc}
	apiServer, err := apiv1.NewServer(h, security,
		apiv1.WithPathPrefix("/api/v1"),
		apiv1.WithErrorHandler(api.WriteDecodeError),
	)
	if err != nil {
		t.Fatalf("building generated API server: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/api/v1/", http.MaxBytesHandler(apiServer, maxTestRequestBodyBytes))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// repeatByteReader streams n bytes of b without ever allocating them all
// at once — the whole point of the test below is to confirm the server
// never buffers an oversized body either, so the client side sending it
// must not do so.
type repeatByteReader struct {
	b         byte
	remaining int64
}

func (r *repeatByteReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	n := len(p)
	if int64(n) > r.remaining {
		n = int(r.remaining)
	}
	for i := 0; i < n; i++ {
		p[i] = r.b
	}
	r.remaining -= int64(n)
	return n, nil
}

// vmHWMKiB reads this process's own peak resident set size from
// /proc/self/status (Linux-only, matching this repo's own dev/CI
// platform) — the same metric the review finding's own reproduction used
// live against a running hoservad.
func vmHWMKiB(t *testing.T) int64 {
	t.Helper()
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Skipf("reading /proc/self/status: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "VmHWM:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			t.Fatalf("unexpected VmHWM line: %q", line)
		}
		kib, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			t.Fatalf("parsing VmHWM %q: %v", fields[1], err)
		}
		return kib
	}
	t.Fatal("no VmHWM line in /proc/self/status")
	return 0
}

// TestOversizedLoginBodyGets413WithoutBeingBuffered is the review finding
// this issue closes: an unauthenticated request body of arbitrary size
// used to be read to completion (io.ReadAll, inside the generated
// decoder) before the rate limiter, or any handler, ever saw it —
// reproduced live at the time with a 200 MiB body raising the daemon's
// own VmHWM by over 700 MB. http.MaxBytesHandler now aborts the read as
// soon as the body exceeds maxTestRequestBodyBytes, so a body far larger
// than that limit costs this process no more memory than one just under
// it would.
func TestOversizedLoginBodyGets413WithoutBeingBuffered(t *testing.T) {
	ctx := context.Background()
	h, authSvc := newAuthTestHandler(t)
	if _, _, err := authSvc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}
	srv := newTestAPIServer(t, h, authSvc)

	before := vmHWMKiB(t)

	// 50 MiB: comfortably over maxTestRequestBodyBytes, and bounded well
	// under the 2 GB this test suite's own dispatch limits any one test
	// to. Streamed via a plain io.Reader (unknown Content-Length, so the
	// client sends it chunked) rather than a pre-built []byte, so the
	// client side of this test doesn't itself buffer 50 MiB either.
	const bodySize = 50 * 1024 * 1024
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/auth/login", &repeatByteReader{b: 'a', remaining: bodySize})
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = -1

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusRequestEntityTooLarge)
	}
	var got struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}
	if got.Code != "request_too_large" {
		t.Errorf("Error.Code = %q, want %q (response: %+v)", got.Code, "request_too_large", got)
	}

	after := vmHWMKiB(t)
	// A generous slack (20 MiB), not a tight bound: the point is that
	// VmHWM must not grow anywhere near the 50 MiB body's own size, which
	// it would if the body were buffered in full before being refused.
	const slackKiB = 20 * 1024
	if grown := after - before; grown > slackKiB {
		t.Errorf("VmHWM grew by %d KiB handling one oversized request, want at most %d KiB (before=%d after=%d)", grown, slackKiB, before, after)
	}
}

// TestNormalSizedLoginBodyIsUnaffectedByTheSizeLimit confirms the limit
// added above doesn't collaterally refuse an ordinary, well-formed
// request — only one that actually exceeds it.
func TestNormalSizedLoginBodyIsUnaffectedByTheSizeLimit(t *testing.T) {
	ctx := context.Background()
	h, authSvc := newAuthTestHandler(t)
	if _, _, err := authSvc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}
	srv := newTestAPIServer(t, h, authSvc)

	resp, err := srv.Client().Post(srv.URL+"/api/v1/auth/login", "application/json",
		strings.NewReader(`{"username":"admin","password":"correct horse battery staple"}`))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 200 (body: %s)", resp.StatusCode, body)
	}
}
