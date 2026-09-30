package api_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
)

// A followed log must reach the client line by line while the request is
// still open. Without FlushLogStream the first line sits in net/http's
// response buffer until the handler returns.
func TestFlushLogStream_DeliversALineBeforeTheStreamEnds(t *testing.T) {
	release := make(chan struct{})
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("first line\n"))
		<-release
		_, _ = w.Write([]byte("second line\n"))
	})
	srv := httptest.NewServer(api.FlushLogStream("/api/v1", inner))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/apps/jellyfin/logs?follow=true", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "first line\n" {
		t.Fatalf("first read = %q, %v; want the first line while the handler is still running", line, err)
	}
}

// The wrapper only touches container and job log requests: any other route
// passes through with the original ResponseWriter.
func TestFlushLogStream_LeavesOtherRoutesAlone(t *testing.T) {
	for path, wrapped := range map[string]bool{
		"/api/v1/apps/jellyfin/logs": true,
		"/api/v1/apps/jellyfin":      false,
		"/api/v1/apps/a/b/logs":      false,
		"/api/v1/apps//logs":         false,
		"/api/v1/jobs/x/log":         true,
		"/api/v1/jobs/x/y/log":       false,
		"/api/v1/jobs//log":          false,
		"/api/v1/jobs/x":             false,
		"/api/v1/events":             false,
	} {
		var got http.ResponseWriter
		inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = w })
		rec := httptest.NewRecorder()
		api.FlushLogStream("/api/v1", inner).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if isRecorder := got == http.ResponseWriter(rec); isRecorder == wrapped {
			t.Errorf("%s: wrapped = %v, want %v", path, !isRecorder, wrapped)
		}
	}
}

// The whole route through the generated server: GET /apps/{id}/logs is
// text/plain, an unknown container is the shared JSON error.
func TestGeneratedServer_AppLogsRoute(t *testing.T) {
	f := newAppsFixture(t)
	f.fake.SetLogs("c1", "hello\n")
	server, err := apiv1.NewServer(f.h, api.TrustedSecurityHandler{}, apiv1.WithPathPrefix("/api/v1"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.FlushLogStream("/api/v1", server))
	defer srv.Close()

	get := func(path string) (*http.Response, error) {
		req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set(api.UnixSocketCredentialHeader, api.UnixSocketCredentialValue)
		return http.DefaultClient.Do(req)
	}
	resp, err := get("/api/v1/apps/jellyfin/logs?tail=10")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body := make([]byte, 64)
	n, _ := resp.Body.Read(body)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") || string(body[:n]) != "hello\n" {
		t.Fatalf("status %d, type %q, body %q; want 200 text/plain hello", resp.StatusCode, resp.Header.Get("Content-Type"), body[:n])
	}

	missing, err := get("/api/v1/apps/nope/logs")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = missing.Body.Close() }()
	if missing.StatusCode != 404 {
		t.Fatalf("unknown container: status %d, want 404", missing.StatusCode)
	}
}
