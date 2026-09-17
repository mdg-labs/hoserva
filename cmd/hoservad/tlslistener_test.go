package main

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// startTestTLSServer wires an http.Server behind a tlsSniffingListener on
// 127.0.0.1:0 (in-process, never a fixed or non-loopback port — CLAUDE.md),
// returning its address and a cleanup func.
func startTestTLSServer(t *testing.T) (addr string, cleanup func()) {
	t.Helper()

	certPEM, keyPEM, err := generateSelfSignedCertificate()
	if err != nil {
		t.Fatalf("generateSelfSignedCertificate: %v", err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}

	rawLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	sniffing := newTLSSniffingListener(rawLn, &tls.Config{Certificates: []tls.Certificate{cert}})

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	srv := &http.Server{Handler: mux}

	go func() { _ = srv.Serve(sniffing) }()

	return rawLn.Addr().String(), func() {
		_ = srv.Close()
	}
}

func TestTLSSniffingListenerServesTLSRequests(t *testing.T) {
	addr, cleanup := startTestTLSServer(t)
	defer cleanup()

	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // test-only, self-signed cert with no CA to verify against
		Timeout:   5 * time.Second,
	}
	resp, err := client.Get("https://" + addr + "/")
	if err != nil {
		t.Fatalf("GET over TLS: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if resp.TLS == nil {
		t.Error("expected a TLS connection state on the response")
	}
	if resp.TLS != nil && resp.TLS.Version < tls.VersionTLS12 {
		t.Errorf("negotiated TLS version = %x, want at least TLS 1.2", resp.TLS.Version)
	}
}

func TestTLSSniffingListenerPointsPlainHTTPAtHTTPS(t *testing.T) {
	original := drainDeadline.Load()
	drainDeadline.Store(200 * time.Millisecond)
	t.Cleanup(func() { drainDeadline.Store(original) })

	addr, cleanup := startTestTLSServer(t)
	defer cleanup()

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: localhost\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	raw, err := io.ReadAll(conn)
	if err != nil && !strings.Contains(err.Error(), "EOF") {
		// A plain net.Conn read returns io.EOF as the loop-terminating
		// condition once the server closes the connection; io.ReadAll
		// itself already treats that as success (err == nil) unless the
		// connection reset instead — either way there's no test failure
		// path that needs distinguishing here beyond what's checked below.
		t.Fatalf("read: %v", err)
	}
	response := string(raw)

	if !strings.HasPrefix(response, "HTTP/1.1 400") {
		t.Errorf("response = %q, want a 400 status line, not a TLS handshake error or empty response", response)
	}
	if !strings.Contains(response, "https://") {
		t.Errorf("response = %q, want a pointer to https://", response)
	}
	if strings.Contains(response, "Location:") {
		t.Errorf("response = %q, want a plain pointer, not a redirect", response)
	}
}

// TestIdleConnectionDoesNotBlockAnotherClient is the review finding this
// issue closes: one connection that sends nothing must never delay a
// second, well-behaved client — reproduced live before this fix at a
// 9.70s delay (peekDeadline's own 10s), since Accept used to run the
// sniff synchronously.
func TestIdleConnectionDoesNotBlockAnotherClient(t *testing.T) {
	addr, cleanup := startTestTLSServer(t)
	defer cleanup()

	idle, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial idle connection: %v", err)
	}
	defer func() { _ = idle.Close() }()
	// idle sends nothing, ever — deliberately left open.

	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // test-only, self-signed cert with no CA to verify against
		Timeout:   2 * time.Second,
	}
	start := time.Now()
	resp, err := client.Get("https://" + addr + "/")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("GET over TLS while an idle connection is pending: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if elapsed > 2*time.Second {
		t.Errorf("a second client took %v while one idle connection was pending — Accept must not serialize on it", elapsed)
	}
}

// fakeTemporaryError stands in for a real transient accept4 failure
// (EMFILE, ENFILE, ECONNABORTED, ...) without needing to actually exhaust
// file descriptors to produce one.
type fakeTemporaryError struct{}

func (fakeTemporaryError) Error() string { return "fake temporary accept error" }
func (fakeTemporaryError) Timeout() bool { return false }

//nolint:staticcheck // mirrors tlslistener.go's own nolint: Temporary() is deprecated but is exactly what acceptLoop (and net/http.Server.Serve) checks.
func (fakeTemporaryError) Temporary() bool { return true }

// fakeFlakyListener is a net.Listener whose first Accept call fails with
// fakeTemporaryError and whose second returns conn — standing in for a
// real listener recovering from transient fd exhaustion one call later.
type fakeFlakyListener struct {
	mu       sync.Mutex
	attempts int
	conn     net.Conn
	closed   chan struct{}
}

func newFakeFlakyListener(conn net.Conn) *fakeFlakyListener {
	return &fakeFlakyListener{conn: conn, closed: make(chan struct{})}
}

func (f *fakeFlakyListener) Accept() (net.Conn, error) {
	f.mu.Lock()
	f.attempts++
	attempt := f.attempts
	f.mu.Unlock()

	if attempt == 1 {
		return nil, fakeTemporaryError{}
	}
	if attempt == 2 {
		return f.conn, nil
	}
	<-f.closed
	return nil, net.ErrClosed
}

func (f *fakeFlakyListener) Close() error {
	select {
	case <-f.closed:
	default:
		close(f.closed)
	}
	return nil
}

func (f *fakeFlakyListener) Addr() net.Addr { return fakeAddr{} }

// TestAcceptLoopRetriesATemporaryErrorAndKeepsServing is the review
// finding this issue closes: a single temporary Accept error used to end
// acceptLoop for good; net/http's own Serve loop, seeing the same
// temporary error forwarded to it, retried by calling this listener's own
// Accept again, but with acceptLoop already gone nothing was left to ever
// produce another value, so Accept blocked forever and Close/Shutdown —
// which wait for Serve to return — hung too, needing a SIGKILL.
func TestAcceptLoopRetriesATemporaryErrorAndKeepsServing(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()

	inner := newFakeFlakyListener(serverConn)
	certPEM, keyPEM, err := generateSelfSignedCertificate()
	if err != nil {
		t.Fatalf("generateSelfSignedCertificate: %v", err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	sniffing := newTLSSniffingListener(inner, &tls.Config{Certificates: []tls.Certificate{cert}})

	// A TLS ClientHello's own first byte (0x16), so handleConn classifies
	// and forwards the connection through connCh right away rather than
	// waiting out peekDeadline.
	go func() { _, _ = clientConn.Write([]byte{0x16}) }()

	connResult := make(chan net.Conn, 1)
	errResult := make(chan error, 1)
	go func() {
		conn, err := sniffing.Accept()
		if err != nil {
			errResult <- err
			return
		}
		connResult <- conn
	}()

	select {
	case conn := <-connResult:
		_ = conn.Close()
	case err := <-errResult:
		t.Fatalf("Accept after one temporary error = %v, want the connection behind it, not an error", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Accept never returned the connection behind the temporary error — acceptLoop must retry, not give up")
	}

	// Close/Shutdown must return promptly — not hang the way they used to
	// once acceptLoop had already exited on what it wrongly treated as a
	// permanent error.
	closeDone := make(chan error, 1)
	go func() { closeDone <- sniffing.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Errorf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return promptly")
	}

	// A late Accept call, after Close, must also return promptly —
	// net.ErrClosed, not block forever waiting for a value neither
	// channel will ever receive again.
	lateResult := make(chan error, 1)
	go func() {
		_, err := sniffing.Accept()
		lateResult <- err
	}()
	select {
	case err := <-lateResult:
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("Accept after Close = %v, want net.ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Accept after Close did not return promptly")
	}
}

func TestPeekFirstByteTimesOutOnSilentConnection(t *testing.T) {
	original := peekDeadline.Load()
	peekDeadline.Store(50 * time.Millisecond)
	t.Cleanup(func() { peekDeadline.Store(original) })

	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	defer func() { _ = client.Close() }()

	_, _, ok := peekFirstByte(server)
	if ok {
		t.Error("peekFirstByte should fail once its deadline passes with nothing sent, not block or panic")
	}
}

func TestPrefixConnReplaysPeekedByte(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	defer func() { _ = client.Close() }()

	go func() { _, _ = client.Write([]byte("Xrest-of-data")) }()

	var first [1]byte
	if _, err := server.Read(first[:]); err != nil {
		t.Fatalf("read first byte: %v", err)
	}
	wrapped := &prefixConn{Conn: server, prefix: first[:]}

	buf := make([]byte, 13)
	n, err := io.ReadFull(wrapped, buf)
	if err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if got := string(buf[:n]); got != "Xrest-of-data" {
		t.Errorf("prefixConn.Read = %q, want the peeked byte replayed first", got)
	}
}

func TestHostForPointer(t *testing.T) {
	if got := hostForPointer(net.IPv4zero); got != "localhost" {
		t.Errorf("hostForPointer(0.0.0.0) = %q, want localhost", got)
	}
	if got := hostForPointer(net.ParseIP("192.0.2.1")); got != "192.0.2.1" {
		t.Errorf("hostForPointer(192.0.2.1) = %q, want 192.0.2.1", got)
	}
}

// TestPlainHTTPPointerHostBracketsIPv6 is the review finding this issue
// closes: a bare fmt.Sprintf("%s:%d", ...) glued an IPv6 address straight
// to its port with no brackets, producing a "host:port" string with more
// than one colon in the host part — not parseable back as one address at
// all, let alone the right one. net.JoinHostPort fixes it.
func TestPlainHTTPPointerHostBracketsIPv6(t *testing.T) {
	got := plainHTTPPointerHost(&net.TCPAddr{IP: net.ParseIP("::1"), Port: 8008})
	want := "[::1]:8008"
	if got != want {
		t.Errorf("plainHTTPPointerHost(::1, 8008) = %q, want %q", got, want)
	}

	if _, _, err := net.SplitHostPort(got); err != nil {
		t.Errorf("plainHTTPPointerHost(::1, 8008) = %q, does not parse back as one host:port pair: %v", got, err)
	}
}

func TestSourceFilteringListenerAllowsLoopback(t *testing.T) {
	rawLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = rawLn.Close() }()
	filtered := newSourceFilteringListener(rawLn, false)

	go func() {
		conn, err := net.DialTimeout("tcp", rawLn.Addr().String(), 5*time.Second)
		if err == nil {
			_ = conn.Close()
		}
	}()

	accepted, err := filtered.Accept()
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	_ = accepted.Close()
}

func TestConnIP(t *testing.T) {
	tcpConn := &net.TCPAddr{IP: net.ParseIP("203.0.113.1"), Port: 1234}
	if got := connIP(fakeConn{addr: tcpConn}); got == nil || !got.Equal(tcpConn.IP) {
		t.Errorf("connIP = %v, want %v", got, tcpConn.IP)
	}
	if got := connIP(fakeConn{addr: fakeAddr{}}); got != nil {
		t.Errorf("connIP for a non-TCP address = %v, want nil", got)
	}
}

type fakeAddr struct{}

func (fakeAddr) Network() string { return "fake" }
func (fakeAddr) String() string  { return "fake" }

type fakeConn struct {
	net.Conn
	addr net.Addr
}

func (c fakeConn) RemoteAddr() net.Addr { return c.addr }
