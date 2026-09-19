package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/auth"
)

func withPeerCred(req *http.Request, cred auth.PeerCredential) *http.Request {
	return req.WithContext(auth.WithPeerCredential(req.Context(), cred))
}

func TestUnixSocketAuthMiddlewareRoot(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Header.Get(api.UnixSocketCredentialHeader) != api.UnixSocketCredentialValue {
			t.Error("an authorized request must carry the synthetic credential header")
		}
	})
	lookup := &auth.FakeGroupLookup{Group: "hoserva", Exists: true}
	mw := unixSocketAuthMiddleware(next, lookup, 1000)

	req := withPeerCred(httptest.NewRequest("GET", "/api/v1/jobs", nil), auth.PeerCredential{UID: 0, GID: 0})
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if !called {
		t.Error("root must be authorized")
	}
	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestUnixSocketAuthMiddlewareDaemonOwnUID(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })
	lookup := &auth.FakeGroupLookup{Group: "hoserva", Exists: true}
	mw := unixSocketAuthMiddleware(next, lookup, 1000)

	req := withPeerCred(httptest.NewRequest("GET", "/api/v1/jobs", nil), auth.PeerCredential{UID: 1000, GID: 999})
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if !called {
		t.Error("the daemon's own uid must be authorized, even with an unrelated gid")
	}
	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestUnixSocketAuthMiddlewareGroupMember(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })
	lookup := &auth.FakeGroupLookup{Group: "hoserva", Exists: true, Members: map[uint32]bool{2000: true}}
	mw := unixSocketAuthMiddleware(next, lookup, 1000)

	req := withPeerCred(httptest.NewRequest("GET", "/api/v1/jobs", nil), auth.PeerCredential{UID: 1234, GID: 2000})
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if !called {
		t.Error("a hoserva-group member must be authorized")
	}
}

func TestUnixSocketAuthMiddlewareStranger(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })
	lookup := &auth.FakeGroupLookup{Group: "hoserva", Exists: true, Members: map[uint32]bool{2000: true}}
	mw := unixSocketAuthMiddleware(next, lookup, 1000)

	req := withPeerCred(httptest.NewRequest("GET", "/api/v1/jobs", nil), auth.PeerCredential{UID: 1234, GID: 3000})
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if called {
		t.Error("a stranger (not root, not the daemon's uid, not in the group) must be refused")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestUnixSocketAuthMiddlewareMissingGroupDegradesToRootOnly(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })
	lookup := &auth.FakeGroupLookup{Group: "hoserva", Exists: false}
	mw := unixSocketAuthMiddleware(next, lookup, 1000)

	// Root and the daemon's own uid still work.
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, withPeerCred(httptest.NewRequest("GET", "/api/v1/jobs", nil), auth.PeerCredential{UID: 0}))
	if !called {
		t.Error("root must still be authorized when the group doesn't exist")
	}

	// A stranger is refused, not granted access just because the group is
	// unresolvable.
	called = false
	rec = httptest.NewRecorder()
	mw.ServeHTTP(rec, withPeerCred(httptest.NewRequest("GET", "/api/v1/jobs", nil), auth.PeerCredential{UID: 4242, GID: 4242}))
	if called {
		t.Error("a missing group must never fail open")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestUnixSocketAuthMiddlewareNoPeerCredential(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })
	lookup := &auth.FakeGroupLookup{Group: "hoserva", Exists: true}
	mw := unixSocketAuthMiddleware(next, lookup, 1000)

	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/jobs", nil))
	if called {
		t.Error("a request with no peer credential at all must be refused")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestSetupUnixListenerRecreatesStaleSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hoserva.sock")

	// A clean net.UnixListener.Close() already unlinks its own socket
	// file, so a stale one (the case this test exercises: an unclean
	// shutdown, e.g. SIGKILL, that never got to Close()) is simulated
	// directly, without ever creating a real listener at path.
	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatalf("writing a stale placeholder file: %v", err)
	}

	ln, err := setupUnixListener(path)
	if err != nil {
		t.Fatalf("setupUnixListener over a stale non-socket file: %v", err)
	}
	defer func() { _ = ln.Close() }()
}

// TestSetupUnixListenerRefusesToTakeOverALiveSocket is the review finding
// this issue closes: setupUnixListener used to remove an existing socket
// file unconditionally, so a second hoservad instance (or anything else
// bound to the same path) starting up would silently unlink and replace
// the first one's live socket out from under it. Dialing the path first
// distinguishes that from the original, legitimate case — a stale file
// left by an unclean shutdown.
func TestSetupUnixListenerRefusesToTakeOverALiveSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hoserva.sock")

	original, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = original.Close() }()

	if _, err := setupUnixListener(path); err == nil {
		t.Fatal("expected setupUnixListener to refuse taking over a socket something is already answering on")
	}

	// The original listener must be completely undisturbed: it can still
	// accept a connection at the same path.
	accepted := make(chan error, 1)
	go func() {
		conn, acceptErr := original.Accept()
		if acceptErr == nil {
			_ = conn.Close()
		}
		accepted <- acceptErr
	}()

	client, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		t.Fatalf("dial the original listener after a refused takeover attempt: %v", err)
	}
	_ = client.Close()
	if err := <-accepted; err != nil {
		t.Errorf("original listener's Accept: %v", err)
	}
}

func TestApplySocketGroupPermissionsDegradesCleanlyWithoutTheGroup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hoserva.sock")
	ln, err := setupUnixListener(path)
	if err != nil {
		t.Fatalf("setupUnixListener: %v", err)
	}
	defer func() { _ = ln.Close() }()

	// This dev/test host has no "hoserva" group — applySocketGroupPermissions
	// must log and return, never panic or error out the caller.
	applySocketGroupPermissions(path)
}

// TestSetupUnixListenerSetsExplicitOwnerOnlyMode is the review finding
// this issue closes: the socket's mode must be set explicitly (0600),
// not left at whatever the process umask happens to produce.
func TestSetupUnixListenerSetsExplicitOwnerOnlyMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hoserva.sock")
	ln, err := setupUnixListener(path)
	if err != nil {
		t.Fatalf("setupUnixListener: %v", err)
	}
	defer func() { _ = ln.Close() }()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode = %o, want 0600", perm)
	}
}

func TestUnixConnContextNonUnixConn(t *testing.T) {
	ctx := context.Background()
	got := unixConnContext(ctx, nil)
	if got != ctx {
		t.Error("unixConnContext must return ctx unchanged for a non-*net.UnixConn (nil included)")
	}
}

// TestUnixConnContextAttachesAuthPeerCredential is the #173 regression:
// unixConnContext must store SO_PEERCRED with auth.WithPeerCredential so
// downstream handlers (Q78 recovery) read the same key the middleware does.
func TestUnixConnContextAttachesAuthPeerCredential(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	acceptedCh := make(chan *net.UnixConn, 1)
	errCh := make(chan error, 1)
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			errCh <- acceptErr
			return
		}
		acceptedCh <- conn.(*net.UnixConn)
	}()

	client, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = client.Close() }()

	var server *net.UnixConn
	select {
	case server = <-acceptedCh:
	case acceptErr := <-errCh:
		t.Fatalf("accept: %v", acceptErr)
	}
	defer func() { _ = server.Close() }()

	ctx := unixConnContext(context.Background(), server)
	cred, ok := auth.PeerCredentialFromContext(ctx)
	if !ok {
		t.Fatal("unixConnContext must attach the peer via auth.WithPeerCredential")
	}
	if int(cred.UID) != os.Getuid() {
		t.Errorf("UID = %d, want %d", cred.UID, os.Getuid())
	}
}

// recoveryThroughMiddleware exercises Q78 over the same middleware chain
// hoservad serves: socket admission (Q44) is separate from recovery's
// uid-0 requirement (Q78). UnlockUser is called with Auth unset so the
// test stops at requireRootPeer — a non-root caller must get
// ErrRootOnlyRecovery; uid 0 must not.
func recoveryThroughMiddleware(t *testing.T, cred auth.PeerCredential, daemonUID uint32) int {
	t.Helper()
	h := &api.Handler{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := h.UnlockUser(r.Context(), apiv1.UnlockUserParams{Username: "admin"})
		if errors.Is(err, api.ErrRootOnlyRecovery) {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	lookup := &auth.FakeGroupLookup{Group: "hoserva", Exists: true}
	mw := unixSocketAuthMiddleware(next, lookup, daemonUID)

	req := withPeerCred(httptest.NewRequest("POST", "/api/v1/users/admin/unlock", nil), cred)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)
	return rec.Code
}

func TestUnixSocketRecoveryRootAllowed(t *testing.T) {
	if recoveryThroughMiddleware(t, auth.PeerCredential{UID: 0, GID: 0}, 1000) != http.StatusOK {
		t.Error("uid 0 must reach the recovery handler over the Unix socket")
	}
}

func TestUnixSocketRecoveryDaemonUIDRefused(t *testing.T) {
	const daemonUID = 1000
	if recoveryThroughMiddleware(t, auth.PeerCredential{UID: daemonUID, GID: 999}, daemonUID) != http.StatusForbidden {
		t.Error("the daemon's own uid must be admitted to the socket (Q44) but refused for recovery (Q78)")
	}
}
